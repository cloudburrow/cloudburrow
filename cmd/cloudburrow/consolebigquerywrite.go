package main

// The BigQuery editor's read-write mode (#994), opt-in and confirmed as
// Spanner Studio's is (#798).
//
// The editor reads by default, as it always has: Query runs one SELECT and
// refuses anything else before sending it (readOnlyBigQuery). Switching it to
// Read-write sends the statement with Mode "read-write", which reaches
// WriteReport below and nothing else, and WriteReport sends it with
// jobs.query, as Query does, so no job-named dataset is left behind (#698).
// BigQuery has no read-only transaction, so the split is the switch: the
// person chose to write, and the confirmation says where.
//
// What the mode runs is what the validating front (internal/bigqueryfront)
// serves and the compat tests exercise through jobs.query: DDL (CREATE
// SCHEMA, CREATE TABLE with columns or AS SELECT, CREATE VIEW, CREATE OR
// REPLACE, IF NOT EXISTS, DROP) and scripts with DECLARE and SET
// (docs/compatibility.md). A statement the front answers 501, such as ALTER
// TABLE or a control-flow block, is sent, and the answer is shown in the
// API's words: the front is the authority, as for every BigQuery form (#874).
//
// Plain DML (INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE) is refused before
// anything is sent, in a statement of its own or in a script. It is not
// verified: through the official Go client the emulator changes the rows of
// an INSERT, UPDATE or DELETE but reports no affected row count
// (statistics.query.numDmlAffectedRows is 0 and statementType SELECT), and
// fails a MERGE whose source is a subquery with 400 jobInternalError "MERGE:
// source must be a single-table reference" (measured, #994), where BigQuery
// runs both. A write mode whose answer to "how many rows did that change" is
// always 0 would be wrong on every DML statement it ran, so DML waits for the
// front to report it (#1008). Rows are added with Insert rows and loads.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	bqv2 "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// maxBigQueryScriptBytes bounds a read-write statement. Chosen, not measured:
// room for a script of many statements, and under the query route's 64 KiB
// request body, so the refusal names this limit rather than the body's.
const maxBigQueryScriptBytes = 32 << 10

// bigqueryDMLIssue is the issue that DML in the read-write editor waits on.
const bigqueryDMLIssue = "#1008"

// bigqueryDML are the statements that change a table's rows. TRUNCATE TABLE
// is DML in BigQuery's reference, and the front counts it as a write.
var bigqueryDML = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true}

// bigqueryStatementHeads are each statement's first keyword, in order, once
// semicolons outside quotes and comments split the text. BEGIN, which opens
// a block, and a block's EXCEPTION WHEN ERROR THEN are stepped over, so the
// statement they start is the one named.
func bigqueryStatementHeads(tokens []string) []string {
	var heads []string
	var cur []string
	add := func() {
		i := 0
		for i < len(cur) {
			switch {
			case strings.EqualFold(cur[i], "BEGIN") && !(i+1 < len(cur) && strings.EqualFold(cur[i+1], "TRANSACTION")):
				i++
				continue
			case strings.EqualFold(cur[i], "EXCEPTION") && i+3 < len(cur) && strings.EqualFold(cur[i+1], "WHEN") &&
				strings.EqualFold(cur[i+2], "ERROR") && strings.EqualFold(cur[i+3], "THEN"):
				i += 4
				continue
			case cur[i] == "(":
				i++
				continue
			}
			break
		}
		if i < len(cur) {
			heads = append(heads, strings.ToUpper(cur[i]))
		}
		cur = nil
	}
	for _, tok := range tokens {
		if tok == ";" {
			add()
			continue
		}
		cur = append(cur, tok)
	}
	add()
	return heads
}

// writableBigQuery refuses, before anything is sent, what the read-write
// editor does not run: DML anywhere in the text, a lone query, and text past
// the limit. Everything else goes to the API, which decides.
func writableBigQuery(statement string) error {
	if len(statement) > maxBigQueryScriptBytes {
		return fmt.Errorf("the statement is %d bytes; the read-write editor accepts at most %d",
			len(statement), maxBigQueryScriptBytes)
	}
	tokens, err := bigQueryTokens(statement)
	if err != nil {
		// Not the editor's to refuse: an unterminated literal is a syntax
		// error, and BigQuery's own message says where it is.
		return nil
	}
	heads := bigqueryStatementHeads(tokens)
	if len(heads) == 0 {
		return errors.New("a statement is required")
	}
	reads := true
	for _, h := range heads {
		if bigqueryDML[h] {
			return fmt.Errorf("%s is DML, which the read-write editor does not run yet: the emulator changes the "+
				"rows but reports no affected row count, and fails a MERGE from a subquery (%s). Add rows with "+
				"Insert rows or a load; CREATE TABLE … AS SELECT makes a table from a query", h, bigqueryDMLIssue)
		}
		if h != "SELECT" && h != "WITH" {
			reads = false
		}
	}
	if reads {
		return fmt.Errorf("%s reads data: switch the editor to Read-only, where it runs as a query", heads[0])
	}
	return nil
}

// WriteSpec implements console.StatementWriter: a dataset's page and its
// tables' pages, whose unqualified names resolve in the dataset.
func (p bigqueryProvider) WriteSpec(path []string) *console.WriteSpec {
	if len(path) < 1 || len(path) > 2 {
		return nil
	}
	return &console.WriteSpec{
		Label: "Read-write",
		Hint: fmt.Sprintf("Changes datasets and tables: DDL (CREATE SCHEMA, CREATE TABLE, with columns or AS SELECT, "+
			"CREATE VIEW, CREATE OR REPLACE, IF NOT EXISTS, DROP) and scripts with DECLARE and SET, sent with "+
			"jobs.query. Unqualified names resolve in dataset %s. DML (INSERT, UPDATE, DELETE, MERGE) is not run "+
			"here yet (%s). What the API refuses is shown in its words.", path[0], bigqueryDMLIssue),
		Target:      fmt.Sprintf("project %s (default dataset %s)", p.project, path[0]),
		Run:         "Run statement",
		Confirm:     "BigQuery has no transaction to undo it: what the statement creates, replaces or drops is changed when it runs.",
		Placeholder: "CREATE TABLE totals AS SELECT region, SUM(amount) AS total FROM orders GROUP BY region",
	}
}

// Write implements console.StatementWriter. The query route calls
// WriteReport instead, because a DDL statement changes no rows; Write is the
// same call without its sentence.
func (p bigqueryProvider) Write(ctx context.Context, project string, path []string, statement string) (int64, error) {
	_, err := p.WriteReport(ctx, project, path, statement)
	return 0, err
}

// WriteReport implements console.WriteReporter: the statement through
// jobs.query, and what came back.
func (p bigqueryProvider) WriteReport(ctx context.Context, project string, path []string, statement string) (string, error) {
	if err := p.writable(project); err != nil {
		return "", err
	}
	if len(path) < 1 {
		return "", errors.New("a BigQuery statement runs in a dataset: open one first")
	}
	statement = strings.TrimSpace(statement)
	if err := writableBigQuery(statement); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := bqv2.NewService(ctx, option.WithEndpoint("http://"+p.endpoint), option.WithoutAuthentication())
	if err != nil {
		return "", fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	legacy := false
	resp, err := svc.Jobs.Query(p.project, &bqv2.QueryRequest{
		Query: statement, UseLegacySql: &legacy,
		DefaultDataset: &bqv2.DatasetReference{ProjectId: p.project, DatasetId: path[0]},
		TimeoutMs:      (dbTimeout - dbTimeout/4).Milliseconds(),
	}).Context(ctx).Do()
	if err != nil {
		return "", bigqueryRefusal(err)
	}
	job := ""
	if resp.JobReference != nil && resp.JobReference.JobId != "" {
		job = " as job " + resp.JobReference.JobId
	}
	if !resp.JobComplete {
		return fmt.Sprintf("The statement is still running%s; Job history shows how it ends.", job), nil
	}
	msg := "The statement ran" + job + "."
	if resp.Schema != nil && len(resp.Schema.Fields) > 0 {
		msg += fmt.Sprintf(" Its last statement returned %d rows, which Read-only shows: run that query there.",
			resp.TotalRows)
	}
	return msg, nil
}

var _ console.WriteReporter = bigqueryProvider{}
