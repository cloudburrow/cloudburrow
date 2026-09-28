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
// Plain DML (INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE) runs as a
// statement of its own (#1024). Measured first through the official Go
// client (#994): the emulator changed the rows but reported no affected row
// count (statementType SELECT, numDmlAffectedRows 0) and failed a MERGE from
// a subquery, so the editor refused DML. Since #1008 the front reports a lone
// DML statement's rows in jobs.query's answer (numDmlAffectedRows and
// dmlStats) and runs a MERGE from a subquery, so the editor sends one and
// answers with what it changed, from that answer (dmlSentence). What the
// front does not report is still refused before anything is sent, with the
// reason: DML inside a script of several statements (or a BEGIN block),
// whose rows the front does not count (#1028), and UPDATE … FROM, which the
// emulator refuses ("Update with joins not supported", #1027).

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

// The issues the read-write editor's DML refusals name: DML inside a
// script, whose rows the front does not count, and UPDATE … FROM, which the
// emulator refuses.
const (
	bigqueryScriptDMLIssue  = "#1028"
	bigqueryUpdateFromIssue = "#1027"
)

// bigqueryDML are the statements that change a table's rows. TRUNCATE TABLE
// is DML in BigQuery's reference, and the front counts it as a write.
var bigqueryDML = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true}

// bigqueryStatements splits tokens into statements at each semicolon, and
// steps over what opens each one without being it: BEGIN, which opens a
// block, a block's EXCEPTION WHEN ERROR THEN, and leading parentheses. Empty
// statements are dropped.
func bigqueryStatements(tokens []string) [][]string {
	var stmts [][]string
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
			stmts = append(stmts, cur[i:])
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
	return stmts
}

// bigqueryStatementHeads are each statement's first keyword, in order.
func bigqueryStatementHeads(tokens []string) []string {
	var heads []string
	for _, stmt := range bigqueryStatements(tokens) {
		heads = append(heads, strings.ToUpper(stmt[0]))
	}
	return heads
}

// updateHasFrom reports whether an UPDATE statement's tokens hold a FROM
// clause of its own: a FROM outside every parenthesis, so one in a subquery
// (WHERE id IN (SELECT id FROM s)) or a function (EXTRACT(YEAR FROM d)) is
// not it.
func updateHasFrom(stmt []string) bool {
	depth := 0
	for _, tok := range stmt[1:] {
		switch {
		case tok == "(":
			depth++
		case tok == ")":
			if depth > 0 {
				depth--
			}
		case depth == 0 && strings.EqualFold(tok, "FROM"):
			return true
		}
	}
	return false
}

// writableBigQuery refuses, before anything is sent, what the read-write
// editor does not run: DML inside a script, UPDATE … FROM, a lone query, and
// text past the limit. Everything else goes to the API, which decides. For a
// DML statement of its own it returns the statement's keyword (INSERT,
// UPDATE, DELETE, MERGE or TRUNCATE), which the answer is worded for.
func writableBigQuery(statement string) (string, error) {
	if len(statement) > maxBigQueryScriptBytes {
		return "", fmt.Errorf("the statement is %d bytes; the read-write editor accepts at most %d",
			len(statement), maxBigQueryScriptBytes)
	}
	tokens, err := bigQueryTokens(statement)
	if err != nil {
		// Not the editor's to refuse: an unterminated literal is a syntax
		// error, and BigQuery's own message says where it is.
		return "", nil
	}
	stmts := bigqueryStatements(tokens)
	if len(stmts) == 0 {
		return "", errors.New("a statement is required")
	}
	reads := true
	for _, stmt := range stmts {
		h := strings.ToUpper(stmt[0])
		if bigqueryDML[h] && len(stmts) > 1 {
			return "", fmt.Errorf("%s is DML inside a script of several statements, which the read-write editor "+
				"does not run: the rows it changes there are not reported (%s). Run the DML "+
				"statement on its own, and the rest of the script before or after it", h, bigqueryScriptDMLIssue)
		}
		if h == "UPDATE" && updateHasFrom(stmt) {
			return "", fmt.Errorf("UPDATE … FROM is refused by the emulator, \"Update with joins not supported\" "+
				"(%s). Use a MERGE whose WHEN MATCHED clause updates the rows, or an UPDATE whose WHERE "+
				"reads the other table in a subquery", bigqueryUpdateFromIssue)
		}
		if h != "SELECT" && h != "WITH" {
			reads = false
		}
	}
	if reads {
		return "", fmt.Errorf("%s reads data: switch the editor to Read-only, where it runs as a query", strings.ToUpper(stmts[0][0]))
	}
	if h := strings.ToUpper(stmts[0][0]); bigqueryDML[h] {
		return h, nil
	}
	return "", nil
}

// dmlSentence says what a DML statement changed, from jobs.query's answer:
// numDmlAffectedRows and, for a MERGE, dmlStats' inserted, updated and
// deleted rows. The words follow the kind of statement: an INSERT adds
// rows, a DELETE or TRUNCATE TABLE removes them, an UPDATE or a MERGE
// modifies them.
func dmlSentence(kind string, resp *bqv2.QueryResponse) string {
	rows := func(n int64) string {
		if n == 1 {
			return "1 row"
		}
		return fmt.Sprintf("%d rows", n)
	}
	n := resp.NumDmlAffectedRows
	switch kind {
	case "INSERT":
		return "This statement added " + rows(n) + "."
	case "DELETE", "TRUNCATE":
		return "This statement removed " + rows(n) + "."
	case "MERGE":
		st := resp.DmlStats
		if st == nil {
			st = &bqv2.DmlStatistics{}
		}
		return fmt.Sprintf("This statement modified %s: %d inserted, %d updated, %d deleted.", rows(n),
			st.InsertedRowCount, st.UpdatedRowCount, st.DeletedRowCount)
	}
	return "This statement modified " + rows(n) + "."
}

// WriteSpec implements console.StatementWriter: a dataset's page and its
// tables' pages, whose unqualified names resolve in the dataset.
func (p bigqueryProvider) WriteSpec(path []string) *console.WriteSpec {
	if len(path) < 1 || len(path) > 2 {
		return nil
	}
	return &console.WriteSpec{
		Label: "Read-write",
		Hint: fmt.Sprintf("Changes datasets, tables and rows: DDL (CREATE SCHEMA, CREATE TABLE, with columns or AS "+
			"SELECT, CREATE VIEW, CREATE OR REPLACE, IF NOT EXISTS, DROP), scripts with DECLARE and SET, and a DML "+
			"statement of its own (INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE), answered with the rows it "+
			"changed, all sent with jobs.query. Unqualified names resolve in dataset %s. DML inside a script (%s) "+
			"and UPDATE … FROM (%s) are not run here. What the API refuses is shown in its words.",
			path[0], bigqueryScriptDMLIssue, bigqueryUpdateFromIssue),
		Target:      fmt.Sprintf("project %s (default dataset %s)", p.project, path[0]),
		Run:         "Run statement",
		Confirm:     "BigQuery has no transaction to undo it: what the statement creates, replaces, drops, adds, changes or deletes is changed when it runs.",
		Placeholder: "CREATE TABLE totals AS SELECT region, SUM(amount) AS total FROM orders GROUP BY region",
	}
}

// Write implements console.StatementWriter. The query route calls
// WriteReport instead, whose sentence says what the statement did; Write is
// the same call without it.
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
	dml, err := writableBigQuery(statement)
	if err != nil {
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
	if dml != "" {
		// The front reports a lone DML statement's rows (#1008).
		return dmlSentence(dml, resp) + " It ran" + job + ".", nil
	}
	msg := "The statement ran" + job + "."
	if resp.Schema != nil && len(resp.Schema.Fields) > 0 {
		msg += fmt.Sprintf(" Its last statement returned %d rows, which Read-only shows: run that query there.",
			resp.TotalRows)
	}
	return msg, nil
}

var _ console.WriteReporter = bigqueryProvider{}
