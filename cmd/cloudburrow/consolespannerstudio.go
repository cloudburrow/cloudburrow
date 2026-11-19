package main

// Spanner's console pages as Google's console has them (#1859): the
// database's catalogue (tables, indexes, views, change streams, sequences)
// read from INFORMATION_SCHEMA in either dialect, Spanner Studio's calls
// (the Explorer, Run and Explain) through POST /api/studio/spanner, Create
// database on the instance's Overview (GoogleSQL or PostgreSQL), and a
// table's Data tab with Insert row, Edit row and Delete row.
//
// Measured on the emulator (cloud-spanner-emulator 1.5.58, 2026-11-19): a
// PostgreSQL-dialect database is created and queried, the catalogue's views
// list views, change streams and sequences, labels are kept on an instance,
// and AnalyzeQuery (query mode PLAN) answers INTERNAL "query plan
// unavailable", so Explain says the emulator's own words.
//
// Query plans stay unavailable (#1859, round 2). The emulator's README says
// "The SQL query modes PLAN and PROFILE do not return any query plans" and
// no release or flag up to 1.5.59 changes that: its handler
// (frontend/handlers/queries.cc) answers PLAN and PROFILE with one node,
// "No query plan", or none. A plan drawn here from the query's text (a scan
// of each table, a filter per WHERE) would not be Spanner's: the operators,
// the index chosen, the distributed unions and the join strategy are its
// optimizer's decisions, which nothing local makes, so a tuned query could
// look right here and run differently on Spanner. What the emulator does
// return is PROFILE's query statistics, rows_returned and elapsed_time
// (it reports no CPU time), so Run runs in PROFILE mode, as Google's Spanner
// Studio does, and the Explanation tab shows them as the Query summary.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// spannerCatalog is what a database holds, from its INFORMATION_SCHEMA.
type spannerCatalog struct {
	Dialect string `json:"dialect"`
	Tables  []struct {
		Name    string      `json:"name"`
		Parent  string      `json:"parent,omitempty"`
		Columns [][2]string `json:"columns"`
	} `json:"tables"`
	Indexes       [][4]string `json:"indexes"`       // table, index, unique, state
	Views         [][2]string `json:"views"`         // name, definition
	ChangeStreams [][2]string `json:"changeStreams"` // name, all
	Sequences     [][2]string `json:"sequences"`     // name, type
	// Errors names the part that could not be read, by its key.
	Errors map[string]string `json:"errors,omitempty"`
}

// spannerRows runs sql and renders every cell as text.
func spannerRows(ctx context.Context, c *spanner.Client, sql string) ([][]string, error) {
	it := c.Single().Query(ctx, spanner.Statement{SQL: sql})
	defer it.Stop()
	var out [][]string
	for {
		row, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		cells := make([]string, row.Size())
		for i := range cells {
			var v spanner.GenericColumnValue
			if err := row.Column(i, &v); err != nil {
				return out, err
			}
			cells[i] = renderSpannerValue(v)
			if cells[i] == "—" {
				cells[i] = ""
			}
		}
		out = append(out, cells)
	}
}

// spannerCatalogSQL is each part's query, per dialect: a PostgreSQL
// database's catalogue is information_schema with lowercase names and the
// public schema.
var spannerCatalogSQL = map[string]map[string]string{
	"GOOGLE_STANDARD_SQL": {
		"tables": "SELECT TABLE_NAME, IFNULL(PARENT_TABLE_NAME, '') FROM INFORMATION_SCHEMA.TABLES " +
			"WHERE TABLE_SCHEMA = '' AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME",
		"columns": "SELECT TABLE_NAME, COLUMN_NAME, SPANNER_TYPE FROM INFORMATION_SCHEMA.COLUMNS " +
			"WHERE TABLE_SCHEMA = '' ORDER BY TABLE_NAME, ORDINAL_POSITION",
		"indexes": "SELECT TABLE_NAME, INDEX_NAME, IS_UNIQUE, IFNULL(INDEX_STATE, '') FROM INFORMATION_SCHEMA.INDEXES " +
			"WHERE TABLE_SCHEMA = '' AND INDEX_TYPE = 'INDEX' ORDER BY TABLE_NAME, INDEX_NAME",
		"views": "SELECT TABLE_NAME, IFNULL(VIEW_DEFINITION, '') FROM INFORMATION_SCHEMA.VIEWS " +
			"WHERE TABLE_SCHEMA = '' ORDER BY TABLE_NAME",
		"changeStreams": "SELECT CHANGE_STREAM_NAME, `ALL` FROM INFORMATION_SCHEMA.CHANGE_STREAMS ORDER BY CHANGE_STREAM_NAME",
		"sequences":     "SELECT NAME, DATA_TYPE FROM INFORMATION_SCHEMA.SEQUENCES ORDER BY NAME",
	},
	"POSTGRESQL": {
		"tables": "SELECT table_name, COALESCE(parent_table_name, '') FROM information_schema.tables " +
			"WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name",
		"columns": "SELECT table_name, column_name, spanner_type FROM information_schema.columns " +
			"WHERE table_schema = 'public' ORDER BY table_name, ordinal_position",
		"indexes": "SELECT table_name, index_name, is_unique, COALESCE(index_state, '') FROM information_schema.indexes " +
			"WHERE table_schema = 'public' AND index_type = 'INDEX' ORDER BY table_name, index_name",
		"views": "SELECT table_name, COALESCE(view_definition, '') FROM information_schema.views " +
			"WHERE table_schema = 'public' ORDER BY table_name",
		"changeStreams": `SELECT change_stream_name, "all" FROM information_schema.change_streams ORDER BY change_stream_name`,
		"sequences":     "SELECT name, data_type FROM information_schema.sequences ORDER BY name",
	},
}

// readSpannerCatalog reads every part; a part that fails is named in Errors
// and the others are still read.
func readSpannerCatalog(ctx context.Context, c *spanner.Client, dialect string) spannerCatalog {
	if dialect != "POSTGRESQL" {
		dialect = "GOOGLE_STANDARD_SQL"
	}
	cat := spannerCatalog{Dialect: dialect, Errors: map[string]string{}}
	q := spannerCatalogSQL[dialect]
	part := func(key string) [][]string {
		rows, err := spannerRows(ctx, c, q[key])
		if err != nil {
			cat.Errors[key] = err.Error()
		}
		return rows
	}
	index := map[string]int{}
	for _, r := range part("tables") {
		cat.Tables = append(cat.Tables, struct {
			Name    string      `json:"name"`
			Parent  string      `json:"parent,omitempty"`
			Columns [][2]string `json:"columns"`
		}{Name: r[0], Parent: r[1], Columns: [][2]string{}})
		index[r[0]] = len(cat.Tables) - 1
	}
	for _, r := range part("columns") {
		if i, ok := index[r[0]]; ok {
			cat.Tables[i].Columns = append(cat.Tables[i].Columns, [2]string{r[1], r[2]})
		}
	}
	cat.Indexes = [][4]string{}
	for _, r := range part("indexes") {
		cat.Indexes = append(cat.Indexes, [4]string{r[0], r[1], spannerYes(r[2]), r[3]})
	}
	two := func(key string) [][2]string {
		out := [][2]string{}
		for _, r := range part(key) {
			out = append(out, [2]string{r[0], r[1]})
		}
		return out
	}
	cat.Views, cat.ChangeStreams, cat.Sequences = two("views"), two("changeStreams"), two("sequences")
	for i := range cat.ChangeStreams {
		cat.ChangeStreams[i][1] = spannerYes(cat.ChangeStreams[i][1])
	}
	if len(cat.Errors) == 0 {
		cat.Errors = nil
	}
	return cat
}

// spannerYes renders a catalogue boolean, BOOL in GoogleSQL and YES/NO in
// PostgreSQL's information_schema, as the console's Yes or No.
func spannerYes(v string) string {
	switch strings.ToLower(v) {
	case "true", "yes":
		return "Yes"
	case "false", "no":
		return "No"
	}
	return orDash(v)
}

// spannerCatalogSections are a database page's Indexes, Views, Change
// streams and Sequences.
func spannerCatalogSections(cat spannerCatalog, unavailable string) []console.Section {
	sec := func(id, label, key, noun, nameCol string, cols []string, rows [][]string, note string) console.Section {
		l := console.Listing{Columns: cols, NameColumn: nameCol, Noun: noun}
		if unavailable != "" {
			l.Unavailable = unavailable
		} else if e := cat.Errors[key]; e != "" {
			l.Unavailable = "reading " + noun + ": " + e
		}
		for _, r := range rows {
			f := map[string]string{}
			for i, c := range cols {
				f[c] = orDash(r[i+1])
			}
			l.Items = append(l.Items, console.Resource{Name: r[0], Fields: f})
		}
		l.Total = len(l.Items)
		l.Note = note
		return console.Section{ID: id, Label: label, Listing: l}
	}
	var idx, views, streams, seqs [][]string
	for _, r := range cat.Indexes {
		idx = append(idx, []string{r[1], r[0], r[2], r[3]})
	}
	for _, r := range cat.Views {
		views = append(views, []string{r[0], r[1]})
	}
	for _, r := range cat.ChangeStreams {
		streams = append(streams, []string{r[0], r[1]})
	}
	for _, r := range cat.Sequences {
		seqs = append(seqs, []string{r[0], r[1]})
	}
	ddl := "Created with DDL: run CREATE %s in Spanner Studio."
	return []console.Section{
		sec("indexes", "Indexes", "indexes", "indexes", "Index name", []string{"Table", "Unique", "State"}, idx, fmt.Sprintf(ddl, "INDEX")),
		sec("views", "Views", "views", "views", "View name", []string{"Definition"}, views, fmt.Sprintf(ddl, "VIEW")),
		sec("change-streams", "Change streams", "changeStreams", "change streams", "Change stream name", []string{"Watches all tables"}, streams,
			fmt.Sprintf(ddl, "CHANGE STREAM")+" The emulator keeps a change stream's definition and serves its change records."),
		sec("sequences", "Sequences", "sequences", "sequences", "Sequence name", []string{"Data type"}, seqs, fmt.Sprintf(ddl, "SEQUENCE")),
	}
}

// spannerNotLocal is a page Google's console has that reads a service this
// instance does not run.
func spannerNotLocal(id, label, why string) console.Section {
	return console.Section{ID: id, Label: label, Unavailable: "Not available locally: " + why}
}

// --- Spanner Studio --------------------------------------------------------

type spannerStudioRequest struct {
	Instance string `json:"instance"`
	Database string `json:"database"`
	SQL      string `json:"sql"`
	// Write is set once the user confirmed a statement that changes data or
	// schema.
	Write bool `json:"write"`
}

type spannerStudioResult struct {
	Confirm  string            `json:"confirm,omitempty"`
	Kind     string            `json:"kind"`
	Columns  []string          `json:"columns,omitempty"`
	Rows     [][]string        `json:"rows,omitempty"`
	Message  string            `json:"message,omitempty"`
	Elapsed  string            `json:"elapsed,omitempty"`
	Plan     []spannerPlanNode `json:"plan,omitempty"`
	PlanNote string            `json:"planNote,omitempty"`
	// Stats is PROFILE mode's query statistics, as the emulator returns
	// them, for the Explanation tab's Query summary.
	Stats *spannerQueryStats `json:"stats,omitempty"`
}

// spannerQueryStats is the query statistics PROFILE mode returns
// (ResultSetStats.query_stats): the emulator sets rows_returned and
// elapsed_time; Spanner's cpu_time and rows_scanned are shown when present.
type spannerQueryStats struct {
	RowsReturned string `json:"rowsReturned,omitempty"`
	ElapsedTime  string `json:"elapsedTime,omitempty"`
	CPUTime      string `json:"cpuTime,omitempty"`
	RowsScanned  string `json:"rowsScanned,omitempty"`
}

// spannerNoPlanNote is the Explanation tab's word when the emulator has no
// plan to give; why is in this file's comment.
const spannerNoPlanNote = "Not available locally: the Spanner emulator returns no query plan (its documented limitation: " +
	"\"The SQL query modes PLAN and PROFILE do not return any query plans\")."

// spannerPlan is a returned plan's nodes, none when the plan is the
// emulator's placeholder ("No query plan").
func spannerPlan(plan *sppb.QueryPlan) []spannerPlanNode {
	var out []spannerPlanNode
	for _, n := range plan.GetPlanNodes() {
		if n.GetDisplayName() == "No query plan" && n.GetKind() == sppb.PlanNode_KIND_UNSPECIFIED {
			continue
		}
		node := spannerPlanNode{Index: n.GetIndex(), Name: n.GetDisplayName(), Kind: n.GetKind().String()}
		for _, l := range n.GetChildLinks() {
			node.Children = append(node.Children, l.GetChildIndex())
		}
		out = append(out, node)
	}
	return out
}

// spannerStats reads PROFILE's query statistics, nil when there are none.
func spannerStats(m map[string]any) *spannerQueryStats {
	str := func(k string) string {
		if v, ok := m[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	st := &spannerQueryStats{RowsReturned: str("rows_returned"), ElapsedTime: str("elapsed_time"),
		CPUTime: str("cpu_time"), RowsScanned: str("rows_scanned")}
	if *st == (spannerQueryStats{}) {
		return nil
	}
	return st
}

type spannerPlanNode struct {
	Index    int32   `json:"index"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Children []int32 `json:"children,omitempty"`
}

// StudioCall implements console.Studio for Spanner Studio.
func (p spannerProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	var req spannerStudioRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("malformed request: %w", err)
	}
	if !spannerIDRe.MatchString(req.Instance) || req.Database == "" || strings.ContainsAny(req.Database, "/ ") {
		return nil, errors.New("a Spanner Studio call names an instance and a database")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*dbTimeout)
	defer cancel()
	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, req.Instance, req.Database)
	dbAdmin, err := database2AdminClient(ctx, p.endpoint)
	if err != nil {
		return nil, err
	}
	defer dbAdmin.Close()
	db, err := dbAdmin.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName})
	if err != nil {
		return nil, fmt.Errorf("cannot read the database: %w", err)
	}
	c, err := newConsoleSpannerClient(ctx, p.endpoint, dbName)
	if err != nil {
		return nil, fmt.Errorf("cannot open the database: %w", err)
	}
	defer c.Close()
	switch call {
	case "explorer":
		return readSpannerCatalog(ctx, c, db.GetDatabaseDialect().String()), nil
	case "run":
		return p.spannerStudioRun(ctx, dbAdmin, c, dbName, req)
	case "explain":
		return spannerStudioExplain(ctx, c, req.SQL)
	}
	return nil, fmt.Errorf("unknown Spanner Studio call %q", call)
}

// spannerStudioRun runs the editor's text: a query in a read-only
// transaction, one DML statement in a read-write transaction, or DDL
// statements through UpdateDatabaseDdl. A change asks for confirmation
// first (Confirm), and is run when the request says Write.
func (p spannerProvider) spannerStudioRun(ctx context.Context, dbAdmin *database.DatabaseAdminClient, c *spanner.Client,
	dbName string, req spannerStudioRequest) (*spannerStudioResult, error) {
	sql := strings.TrimSpace(req.SQL)
	if sql == "" {
		return nil, errors.New("the editor is empty: write a statement to run")
	}
	st, err := classifySpannerStatement(sql)
	if err != nil || len(st.Kinds) == 0 {
		st = spannerStatement{Kinds: []spannerStatementKind{spannerQuery}, Keywords: []string{""}}
	}
	ddl, dml := 0, 0
	for _, k := range st.Kinds {
		switch k {
		case spannerDDL:
			ddl++
		case spannerDML:
			dml++
		}
	}
	short := lastSegment(dbName)
	start := time.Now()
	switch {
	case ddl > 0:
		if ddl != len(st.Kinds) {
			return nil, errors.New("DDL runs alone: run the schema statements and the other statements separately")
		}
		statements, err := splitSpannerDDL(sql)
		if err != nil {
			return nil, err
		}
		if !req.Write {
			return &spannerStudioResult{Kind: "ddl", Confirm: fmt.Sprintf(
				"Update the schema of database %s? %d statement(s) run in order with UpdateDatabaseDdl.", short, len(statements))}, nil
		}
		op, err := dbAdmin.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: dbName, Statements: statements})
		if err != nil {
			return nil, err
		}
		if err := op.Wait(ctx); err != nil {
			return nil, err
		}
		return &spannerStudioResult{Kind: "ddl", Message: fmt.Sprintf("Schema updated: %d statement(s) ran.", len(statements)),
			Elapsed: time.Since(start).Round(time.Millisecond).String()}, nil
	case dml > 0:
		one, err := writableSpanner(sql)
		if err != nil {
			return nil, err
		}
		if !req.Write {
			return &spannerStudioResult{Kind: "dml", Confirm: fmt.Sprintf(
				"Run this %s on database %s? It is committed in a read-write transaction.", st.Keywords[0], short)}, nil
		}
		var rows int64
		if _, err := c.ReadWriteTransaction(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
			n, err := txn.Update(ctx, spanner.Statement{SQL: one})
			rows = n
			return err
		}); err != nil {
			return nil, err
		}
		return &spannerStudioResult{Kind: "dml", Message: fmt.Sprintf("%d row(s) affected.", rows),
			Elapsed: time.Since(start).Round(time.Millisecond).String()}, nil
	}
	if len(st.Kinds) > 1 {
		return nil, errors.New("Run runs one query at a time: select one statement, or remove everything after the first semicolon")
	}
	// PROFILE mode, as Google's Spanner Studio runs a query: its results
	// and its statistics. The rows past the first 1,000 are read and
	// dropped, since the statistics come at the end of the stream.
	it := c.Single().QueryWithStats(ctx, spanner.Statement{SQL: strings.TrimRight(sql, "; \t\r\n")})
	defer it.Stop()
	res := &spannerStudioResult{Kind: "query", Rows: [][]string{}}
	more := false
	for {
		row, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(res.Rows) == 1000 {
			more = true
			continue
		}
		if res.Columns == nil {
			res.Columns = row.ColumnNames()
		}
		cells := make([]string, row.Size())
		for i := range cells {
			var v spanner.GenericColumnValue
			if err := row.Column(i, &v); err != nil {
				return nil, err
			}
			cells[i] = renderSpannerValue(v)
		}
		res.Rows = append(res.Rows, cells)
	}
	if res.Columns == nil && it.Metadata != nil {
		for _, f := range it.Metadata.GetRowType().GetFields() {
			res.Columns = append(res.Columns, f.GetName())
		}
	}
	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	if more {
		res.Message = "Showing the first 1,000 rows."
	}
	res.Stats = spannerStats(it.QueryStats)
	if res.Plan = spannerPlan(it.QueryPlan); len(res.Plan) == 0 {
		res.PlanNote = spannerNoPlanNote
	}
	return res, nil
}

// spannerStudioExplain is Explain: AnalyzeQuery's plan, or the emulator's
// own reason it has none.
func spannerStudioExplain(ctx context.Context, c *spanner.Client, sql string) (*spannerStudioResult, error) {
	sql = strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
	if sql == "" {
		return nil, errors.New("the editor is empty: write a query to explain")
	}
	if err := readOnlySpanner(sql); err != nil {
		return nil, fmt.Errorf("Explain shows a query's plan: %w", err)
	}
	plan, err := c.Single().AnalyzeQuery(ctx, spanner.Statement{SQL: sql})
	res := &spannerStudioResult{Kind: "explain"}
	if err != nil {
		res.PlanNote = spannerNoPlanNote[:len(spannerNoPlanNote)-2] + "; AnalyzeQuery answered: " + spanner.ErrDesc(err) + ")."
		return res, nil
	}
	if res.Plan = spannerPlan(plan); len(res.Plan) == 0 {
		res.PlanNote = spannerNoPlanNote
	}
	return res, nil
}

var spannerIDRe = regexp.MustCompile(`^[a-z][a-z0-9\-]{1,62}[a-z0-9]$`)

// --- Create database (the instance's Overview) ------------------------------

const spannerCreateDatabaseAction = "createdatabase"

// spannerDDLTemplates are the Create database page's schema templates:
// Google's page offers DDL templates to start the schema from.
var spannerDDLTemplates = []string{
	"CREATE TABLE Singers (\n  SingerId INT64 NOT NULL,\n  FirstName STRING(1024),\n  LastName STRING(1024),\n) PRIMARY KEY (SingerId);",
	"CREATE TABLE Albums (\n  SingerId INT64 NOT NULL,\n  AlbumId INT64 NOT NULL,\n  AlbumTitle STRING(MAX),\n) PRIMARY KEY (SingerId, AlbumId),\n  INTERLEAVE IN PARENT Singers ON DELETE CASCADE;",
	"CREATE INDEX AlbumsByAlbumTitle ON Albums(AlbumTitle);",
}

// spannerCreateDatabaseForm is Create database, on an instance's page.
func spannerCreateDatabaseForm() console.Action {
	return console.Action{ID: spannerCreateDatabaseAction, Label: "Create database", Fields: []console.Field{
		{Name: "database", Label: "Database name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9_\-]{0,28}[a-z0-9]$`,
			Section: "Name your database",
			Help:    "2 to 30 characters: lowercase letters, digits, underscores and hyphens, starting with a letter. It cannot be changed later."},
		{Name: "dialect", Label: "Select a database dialect", Type: "radio", Section: "Select a database dialect",
			Options: []string{"Google Standard SQL", "PostgreSQL"}, Default: "Google Standard SQL",
			Help: "The dialect cannot be changed once the database is created."},
		{Name: "ddl", Label: "Define your schema (optional)", Type: "textarea", Section: "Define your schema (optional)",
			Options: spannerDDLTemplates,
			Help:    "DDL statements separated by semicolons; leave it empty for an empty database. Show DDL templates inserts a starting point (GoogleSQL)."},
		{Name: "encryption", Label: "Encryption", Type: "radio", Section: "Show encryption options",
			Options: []string{"Google-managed encryption key", "Cloud KMS key"}, Default: "Google-managed encryption key",
			UnavailableOptions: map[string]string{"Cloud KMS key": "Not available locally: the Spanner emulator encrypts nothing with a customer-managed key."}},
	}}
}

// spannerCreateDatabase is CreateDatabase in instanceID, waited on.
func (p spannerProvider) spannerCreateDatabase(ctx context.Context, project, instanceID string, v map[string]string) error {
	dbID := strings.TrimSpace(v["database"])
	if dbID == "" {
		return errors.New("a database name is required")
	}
	dbAdmin, err := database2AdminClient(ctx, p.endpoint)
	if err != nil {
		return err
	}
	defer dbAdmin.Close()
	req := &databasepb.CreateDatabaseRequest{
		Parent:          fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
		CreateStatement: fmt.Sprintf("CREATE DATABASE `%s`", dbID),
	}
	pg := strings.EqualFold(strings.TrimSpace(v["dialect"]), "PostgreSQL")
	if pg {
		// A PostgreSQL database is created with its name double-quoted and
		// its schema applied after: CreateDatabase takes no extra
		// statements in that dialect.
		req.CreateStatement = fmt.Sprintf("CREATE DATABASE %q", dbID)
		req.DatabaseDialect = databasepb.DatabaseDialect_POSTGRESQL
	}
	var statements []string
	if ddl := strings.TrimSpace(v["ddl"]); ddl != "" {
		if statements, err = splitSpannerDDL(ddl); err != nil {
			return err
		}
	}
	if !pg {
		req.ExtraStatements = statements
	}
	op, err := dbAdmin.CreateDatabase(ctx, req)
	if err != nil {
		return fmt.Errorf("creating the database: %w", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for the database: %w", err)
	}
	if pg && len(statements) > 0 {
		uop, err := dbAdmin.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: req.Parent + "/databases/" + dbID, Statements: statements})
		if err == nil {
			err = uop.Wait(ctx)
		}
		if err != nil {
			return fmt.Errorf("the database was created, and its schema refused: %w", err)
		}
	}
	return nil
}

// --- a table's Data tab ------------------------------------------------------

// spannerColumn is one column of a table, as the Data tab edits it.
type spannerColumn struct {
	Name, Type string
	Nullable   bool
	Key        int64
	Generated  bool
}

func spannerTableColumns(ctx context.Context, c *spanner.Client, table string) ([]spannerColumn, error) {
	it := c.Single().Query(ctx, spanner.Statement{
		SQL: `SELECT c.COLUMN_NAME, c.SPANNER_TYPE, c.IS_NULLABLE, IFNULL(k.ORDINAL_POSITION, 0), IFNULL(c.IS_GENERATED, 'NEVER')
		      FROM INFORMATION_SCHEMA.COLUMNS c
		      LEFT JOIN INFORMATION_SCHEMA.INDEX_COLUMNS k
		        ON k.TABLE_NAME = c.TABLE_NAME AND k.COLUMN_NAME = c.COLUMN_NAME AND k.INDEX_NAME = 'PRIMARY_KEY'
		      WHERE c.TABLE_SCHEMA = '' AND c.TABLE_NAME = @table ORDER BY c.ORDINAL_POSITION`,
		Params: map[string]any{"table": table}})
	defer it.Stop()
	var out []spannerColumn
	for {
		row, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var col spannerColumn
		var nullable, generated string
		if err := row.Columns(&col.Name, &col.Type, &nullable, &col.Key, &generated); err != nil {
			return nil, err
		}
		col.Nullable, col.Generated = nullable == "YES", generated != "NEVER"
		out = append(out, col)
	}
}

// spannerEditable says whether the Data tab's forms can write a column:
// the scalar types and ARRAYs of them, in either dialect
// (consolespannerdata.go).
func spannerEditable(t string) bool {
	return spannerColumnType(t) != nil
}

// spannerCastExpr is the SQL that turns the string parameter name into a
// value of the column's type: Google's console takes each value as text.
func spannerCastExpr(t, name string) string {
	base := strings.ToUpper(t)
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "STRING":
		return "@" + name
	case "JSON":
		return "PARSE_JSON(@" + name + ")"
	case "BYTES":
		return "FROM_BASE64(@" + name + ")"
	}
	return fmt.Sprintf("CAST(@%s AS %s)", name, base)
}

// spannerRowFields are Insert row's and Edit row's fields: one per column the
// form can write, prefilled with row when it is given. A key column cannot
// change on Edit row (Spanner has no key update: it is a delete and an
// insert).
func spannerRowFields(cols []spannerColumn, row map[string]string) []console.Field {
	var out []console.Field
	for _, col := range cols {
		if col.Generated || !spannerEditable(col.Type) {
			continue
		}
		f := console.Field{Name: "col_" + col.Name, Label: col.Name, Type: "text", Required: !col.Nullable && col.Key == 0 || col.Key > 0,
			Help: col.Type}
		if spannerColumnType(col.Type).GetCode() == sppb.TypeCode_ARRAY {
			f.Help += ": a JSON array, such as [1, 2] or [\"a\", null]"
		}
		if col.Nullable {
			f.Help += ", nullable: left empty, the value is NULL"
		}
		if row != nil {
			if v := row[col.Name]; v != "—" {
				f.Default = v
			}
			f.Immutable = col.Key > 0
		}
		out = append(out, f)
	}
	return out
}

// spannerDataSection is a table's Data tab: up to 200 rows, each with Edit
// row and Delete row, as Google's table Data page has them.
func spannerDataSection(ctx context.Context, c *spanner.Client, d spannerDialect, instanceID, dbID, table string, cols []spannerColumn) console.Section {
	sec := console.Section{ID: "data", Label: "Data"}
	l := console.Listing{Noun: "rows"}
	var keys []string
	for _, col := range cols {
		if col.Key > 0 {
			keys = append(keys, col.Name)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return spannerKeyPos(cols, keys[i]) < spannerKeyPos(cols, keys[j])
	})
	var names []string
	for _, col := range cols {
		names = append(names, d.quote(col.Name))
	}
	if len(names) == 0 {
		sec.Listing = l
		return sec
	}
	order := ""
	if len(keys) > 0 {
		quoted := make([]string, len(keys))
		for i, k := range keys {
			quoted[i] = d.quote(k)
		}
		order = " ORDER BY " + strings.Join(quoted, ", ")
	}
	it := c.Single().Query(ctx, spanner.Statement{SQL: "SELECT " + strings.Join(names, ", ") + " FROM " + d.quote(table) + order + " LIMIT 200"})
	defer it.Stop()
	if len(keys) > 0 {
		l.NameColumn = keys[0]
	}
	for _, col := range cols {
		if len(keys) == 0 || col.Name != keys[0] {
			l.Columns = append(l.Columns, col.Name)
		}
	}
	for {
		row, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			l.Unavailable = "reading rows: " + err.Error()
			break
		}
		values := map[string]string{}
		for i, col := range cols {
			var v spanner.GenericColumnValue
			if err := row.Column(i, &v); err != nil {
				l.Unavailable = "decoding rows: " + err.Error()
				break
			}
			values[col.Name] = renderSpannerValue(v)
		}
		keyVals := make([]string, len(keys))
		for i, k := range keys {
			keyVals[i] = values[k]
		}
		keyJSON, _ := json.Marshal(keyVals)
		name := string(keyJSON)
		if len(keys) > 0 {
			name = values[keys[0]]
		}
		fields := map[string]string{}
		for k, v := range values {
			fields[k] = summarise(v)
		}
		res := console.Resource{Name: name, Fields: fields}
		if len(keys) > 0 {
			hidden := console.Field{Name: "key", Type: "hidden", Default: string(keyJSON)}
			res.ActsOn = []string{instanceID, dbID, table, "rows"}
			res.Actions = []console.Action{
				{ID: "editrow", Label: "Edit row", Fields: append([]console.Field{hidden}, spannerRowFields(cols, values)...)},
				{ID: "deleterow", Label: "Delete row", Destructive: true, Fields: []console.Field{hidden}},
			}
		}
		l.Items = append(l.Items, res)
	}
	l.Total = len(l.Items)
	if len(l.Items) == detailLimit {
		l.Note = truncatedNote(len(l.Items), "rows")
	}
	sec.Listing = l
	if len(keys) == 0 {
		sec.Note = "This table has no primary key columns the console can address, so its rows are read-only here."
	}
	return sec
}

func spannerKeyPos(cols []spannerColumn, name string) int64 {
	for _, c := range cols {
		if c.Name == name {
			return c.Key
		}
	}
	return 0
}

// spannerRowAct is Insert row, Edit row and Delete row, as DML with every
// value a parameter cast to its column's type.
func (p spannerProvider) spannerRowAct(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, path[0], path[1])
	c, err := newConsoleSpannerClient(ctx, p.endpoint, dbName)
	if err != nil {
		return fmt.Errorf("cannot open the database: %w", err)
	}
	defer c.Close()
	table := path[2]
	d := spannerDialectOfClient(ctx, c)
	cols, err := spannerTableColumnsIn(ctx, c, d, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("table %s not found", table)
	}
	b := &spannerBinder{d: d, params: map[string]any{}}
	params := b.params
	// A typed parameter for an ARRAY and every PostgreSQL column; a string
	// cast to the column's type otherwise (spannerCastExpr).
	arg := func(col spannerColumn, pn, raw string) (string, error) {
		if !spannerTypedColumn(d, col.Type) {
			params[pn] = raw
			return spannerCastExpr(col.Type, pn), nil
		}
		v, err := spannerTypedParam(col.Type, raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", col.Name, err)
		}
		return b.bind(pn, v), nil
	}
	var keys []spannerColumn
	for _, col := range cols {
		if col.Key > 0 {
			keys = append(keys, col)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
	where := func() (string, error) {
		var vals []string
		if err := json.Unmarshal([]byte(v["key"]), &vals); err != nil || len(vals) != len(keys) {
			return "", errors.New("the row's key is missing: reload the table")
		}
		var conds []string
		for i, k := range keys {
			e, err := arg(k, fmt.Sprintf("k%d", i), vals[i])
			if err != nil {
				return "", err
			}
			conds = append(conds, fmt.Sprintf("%s = %s", d.quote(k.Name), e))
		}
		return strings.Join(conds, " AND "), nil
	}
	value := func(col spannerColumn, pn string) (string, bool, error) {
		raw, ok := v["col_"+col.Name]
		if !ok {
			return "", false, nil
		}
		if raw == "" && col.Nullable {
			return "NULL", true, nil
		}
		e, err := arg(col, pn, raw)
		return e, err == nil, err
	}
	var sql string
	switch action {
	case "insertrow":
		var names, exprs []string
		for i, col := range cols {
			if col.Generated || !spannerEditable(col.Type) {
				continue
			}
			e, ok, err := value(col, fmt.Sprintf("v%d", i))
			if err != nil {
				return err
			}
			if ok {
				names, exprs = append(names, d.quote(col.Name)), append(exprs, e)
			}
		}
		if len(names) == 0 {
			return errors.New("no values to insert")
		}
		sql = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", d.quote(table), strings.Join(names, ", "), strings.Join(exprs, ", "))
	case "editrow":
		cond, err := where()
		if err != nil {
			return err
		}
		var sets []string
		for i, col := range cols {
			if col.Key > 0 || col.Generated || !spannerEditable(col.Type) {
				continue
			}
			e, ok, err := value(col, fmt.Sprintf("v%d", i))
			if err != nil {
				return err
			}
			if ok {
				sets = append(sets, fmt.Sprintf("%s = %s", d.quote(col.Name), e))
			}
		}
		if len(sets) == 0 {
			return errors.New("nothing to change: a key column cannot change")
		}
		sql = fmt.Sprintf("UPDATE %s SET %s WHERE %s", d.quote(table), strings.Join(sets, ", "), cond)
	case "deleterow":
		cond, err := where()
		if err != nil {
			return err
		}
		sql = fmt.Sprintf("DELETE FROM %s WHERE %s", d.quote(table), cond)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	var n int64
	_, err = c.ReadWriteTransaction(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		var err error
		n, err = txn.Update(ctx, spanner.Statement{SQL: sql, Params: params})
		return err
	})
	if err != nil {
		return err
	}
	if n == 0 && action != "insertrow" {
		return errors.New("no row has that key any more: reload the table")
	}
	return nil
}

// spannerDialectOf is a database's dialect, or GoogleSQL when it cannot be
// read.
func spannerDialectOf(db *databasepb.Database) string {
	if db.GetDatabaseDialect() == databasepb.DatabaseDialect_POSTGRESQL {
		return "PostgreSQL"
	}
	return "Google Standard SQL"
}

// spannerCapacity is an instance's compute capacity as Google's list shows
// it: nodes from 1000 processing units, processing units below.
func spannerCapacity(nodes, pu int32) string {
	if pu == 0 {
		pu = nodes * 1000
	}
	if pu >= 1000 && pu%1000 == 0 {
		n := pu / 1000
		unit := "nodes"
		if n == 1 {
			unit = "node"
		}
		return fmt.Sprintf("%d %s (%s processing units)", n, unit, strconv.FormatInt(int64(pu), 10))
	}
	return fmt.Sprintf("%d processing units", pu)
}

// spannerLabels renders labels key:value, sorted.
func spannerLabels(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+": "+v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// spannerRestoreAction is a backup row's Restore on a database's
// Backup/Restore tab: a new database from the backup, as the backup's page
// has it.
func spannerRestoreAction() console.Action {
	return console.Action{ID: "restorebackup", Label: "Restore", Fields: []console.Field{
		{Name: "database", Label: "Database name", Type: "text", Required: true, Pattern: spannerIDPattern,
			Help: "A database that does not exist yet in this instance: it is created with the backup's schema and rows."},
	}}
}

// spannerCreateDatabaseBackupAction is Create backup on a database's
// Backup/Restore tab: the source database is the page's.
func spannerCreateDatabaseBackupAction() console.Action {
	return console.Action{ID: "createdbbackup", Label: "Create backup", Fields: []console.Field{
		{Name: "backup", Label: "Backup name", Type: "text", Required: true, Pattern: spannerIDPattern,
			Help: "Lowercase letters, digits, underscores and hyphens, starting with a letter; 2 to 30 characters."},
		spannerExpiryField("7"),
	}}
}

var _ console.Studio = spannerProvider{}
