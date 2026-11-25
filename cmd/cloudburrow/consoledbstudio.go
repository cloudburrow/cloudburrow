package main

// Cloud SQL Studio and AlloyDB Studio (#2208), as Google's console has
// them (https://docs.cloud.google.com/sql/docs/postgres/manage-data-using-studio,
// https://docs.cloud.google.com/sql/docs/mysql/manage-data-using-studio and
// https://docs.cloud.google.com/alloydb/docs/manage-data-using-studio, read
// 2026-11-24): an instance's (a cluster's) Studio opens on the sign-in
// dialog (the database, Built-in database authentication with a user and
// its password, or IAM database authentication), then the Explorer of the
// database's schemas with their tables (columns and indexes), views,
// sequences and functions, each with View actions that fill the editor;
// the editor's tabs with Run, Run selected, Clear, Save and Save as; the
// Results with the All results list when a script returns several and
// Export; and the saved queries.
//
// The statements run as the signed-in user, through the same calls the
// APIs' Data API serves (sqladmin.Server.StudioSQL for Cloud SQL,
// alloydb.Server.StudioExecuteSql for AlloyDB), so the server checks the
// password as it checks every sign-in, and a wrong one is refused with
// the server's own message. Google's limits are kept: every run is a new
// session; a response over 10 MB is truncated and says so; a run is
// cancelled after five minutes; at most 10 run at once on an instance;
// a script mixing DML, DDL and queries shows only the queries' results.
//
// The sign-in is held here, never in the browser: signin answers a
// session ID, and every other call names it. A session ends after an hour
// unused, or with Sign out.
//
// Saved queries: Google keeps them as Studio assets of the project, which
// CloudBurrow does not serve; here they are CloudBurrow's own record in the
// instance's state directory, as Bigtable Studio's are, reachable by no API.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// dbStudioRequest is a Studio call's body.
type dbStudioRequest struct {
	// Instance is a Cloud SQL instance's name, or an AlloyDB cluster's full
	// name.
	Instance string `json:"instance"`
	// The sign-in.
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	IAM      bool   `json:"iam"`
	Session  string `json:"session"`
	SQL      string `json:"sql"`
	// A saved query's id and name.
	ID   string `json:"id"`
	Name string `json:"name"`
}

// dbStudioResult is one statement's result.
type dbStudioResult struct {
	Columns []string    `json:"columns"`
	Types   []string    `json:"types,omitempty"`
	Rows    [][]*string `json:"rows"`
	Message string      `json:"message,omitempty"`
	Partial bool        `json:"partial,omitempty"`
}

// dbStudioLogin is a signed-in user of one instance's database.
type dbStudioLogin struct {
	product, project, instance string
	database, user, password   string
	// iam is the principal of an IAM sign-in, "" for a built-in one.
	iam string
	// role is the database user it signs in as.
	role    string
	engine  string
	expires time.Time
}

// dbStudioSessionTTL is how long an unused sign-in lasts.
const dbStudioSessionTTL = time.Hour

// dbStudioResponseLimit is Google's: "If the response is larger than 10MB,
// the response is truncated".
const dbStudioResponseLimit = 10 << 20

// dbStudioRunTimeout is Google's: "Requests that run longer than five
// minutes are canceled".
const dbStudioRunTimeout = 5 * time.Minute

var dbStudioSessions = struct {
	sync.Mutex
	m map[string]*dbStudioLogin
}{m: map[string]*dbStudioLogin{}}

func newDBStudioSession(l dbStudioLogin) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	l.expires = time.Now().Add(dbStudioSessionTTL)
	dbStudioSessions.Lock()
	defer dbStudioSessions.Unlock()
	for k, v := range dbStudioSessions.m {
		if time.Now().After(v.expires) {
			delete(dbStudioSessions.m, k)
		}
	}
	dbStudioSessions.m[id] = &l
	return id
}

// dbStudioSession is the live sign-in id names, for product's instance of
// project.
func dbStudioSession(product, project, instance, id string) (dbStudioLogin, error) {
	dbStudioSessions.Lock()
	defer dbStudioSessions.Unlock()
	l, ok := dbStudioSessions.m[id]
	if !ok || time.Now().After(l.expires) || l.product != product || l.project != project || l.instance != instance {
		delete(dbStudioSessions.m, id)
		return dbStudioLogin{}, errDBStudioSignedOut
	}
	l.expires = time.Now().Add(dbStudioSessionTTL)
	return *l, nil
}

func endDBStudioSession(id string) {
	dbStudioSessions.Lock()
	delete(dbStudioSessions.m, id)
	dbStudioSessions.Unlock()
}

var errDBStudioSignedOut = errors.New("signed out: sign in to the database again")

// dbStudioBackend is what a product's Studio runs through.
type dbStudioBackend struct {
	product string
	// savedQueries is the saved queries record's path.
	savedQueries string
	// options are the sign-in dialog's choices for instance.
	options func(ctx context.Context, project, instance string) (map[string]any, error)
	// run runs sql as l, l.role filled in.
	run func(ctx context.Context, l *dbStudioLogin, sql string) ([]dbStudioResult, error)
	// engine is instance's "POSTGRES" or "MYSQL".
	engine func(ctx context.Context, project, instance string) (string, error)
	// iamPrincipal is the principal an IAM sign-in is made as: the
	// console's own.
	iamPrincipal string
}

func (b dbStudioBackend) call(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	var req dbStudioRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("malformed request: %w", err)
	}
	if req.Instance == "" {
		return nil, errors.New("a Studio call names an instance")
	}
	store := bqSavedQueryStore{path: b.savedQueries, region: func(string) bool { return false }, product: b.product}
	switch call {
	case "savedqueries":
		all, err := store.list(project)
		if err != nil {
			return nil, err
		}
		out := []bqSavedQuery{}
		for _, q := range all {
			if q.Region == "" {
				out = append(out, q)
			}
		}
		return map[string]any{"queries": out}, nil
	case "savequery":
		return store.save(project, bqSavedQuery{ID: req.ID, Name: req.Name, Query: req.SQL}, time.Now())
	case "deletequery":
		if err := store.remove(project, req.ID); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": req.ID}, nil
	case "options":
		out, err := b.options(ctx, project, req.Instance)
		if err != nil {
			return nil, err
		}
		out["iamPrincipal"] = b.iamPrincipal
		return out, nil
	case "signin":
		return b.signIn(ctx, project, req)
	case "signout":
		endDBStudioSession(req.Session)
		return map[string]bool{"signedOut": true}, nil
	case "explorer", "run":
		l, err := dbStudioSession(b.product, project, req.Instance, req.Session)
		if err != nil {
			return nil, err
		}
		if call == "explorer" {
			return b.explorer(ctx, &l)
		}
		return b.runScript(ctx, &l, req.SQL)
	}
	return nil, fmt.Errorf("unknown %s call %q", b.product+" Studio", call)
}

// signIn checks the sign-in by running a statement as the user, and holds
// it for the calls after.
func (b dbStudioBackend) signIn(ctx context.Context, project string, req dbStudioRequest) (any, error) {
	if strings.TrimSpace(req.Database) == "" {
		return nil, errors.New("select a database")
	}
	l := dbStudioLogin{product: b.product, project: project, instance: req.Instance, database: req.Database}
	if req.IAM {
		if b.iamPrincipal == "" {
			return nil, errors.New("IAM database authentication signs in as the console's principal, and this console has none")
		}
		l.iam = b.iamPrincipal
	} else {
		if strings.TrimSpace(req.User) == "" {
			return nil, errors.New("select a user")
		}
		l.user, l.password = req.User, req.Password
	}
	engine, err := b.engine(ctx, project, req.Instance)
	if err != nil {
		return nil, err
	}
	l.engine = engine
	probe := "SELECT current_user"
	if engine == "MYSQL" {
		probe = "SELECT CURRENT_USER()"
	}
	if _, err := b.run(ctx, &l, probe); err != nil {
		return nil, err
	}
	return map[string]string{"session": newDBStudioSession(l), "database": l.database, "user": l.role, "engine": engine}, nil
}

// runScript runs a script as Google's Studio shows it: only the results
// with rows when it mixes queries with other statements, "Statement
// executed successfully" when it has none, and at most 10 MB.
func (b dbStudioBackend) runScript(ctx context.Context, l *dbStudioLogin, sql string) (any, error) {
	if strings.TrimSpace(sql) == "" {
		return nil, errors.New("the editor is empty: write a statement to run")
	}
	ctx, cancel := context.WithTimeout(ctx, dbStudioRunTimeout)
	defer cancel()
	start := time.Now()
	results, err := b.run(ctx, l, sql)
	elapsed := time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("the request ran longer than five minutes and was canceled")
		}
		// Every statement of the script is aborted, and the first error
		// is shown (Google's batch rule).
		return map[string]any{"error": err.Error(), "elapsed": elapsed}, nil
	}
	var queries []dbStudioResult
	for _, r := range results {
		if len(r.Columns) > 0 {
			queries = append(queries, r)
		}
	}
	out := map[string]any{"elapsed": elapsed}
	if len(queries) == 0 {
		out["results"] = []dbStudioResult{}
		out["message"] = "Statement executed successfully"
		return out, nil
	}
	size, truncated := 0, false
	for i := range queries {
		for j, row := range queries[i].Rows {
			for _, v := range row {
				if v != nil {
					size += len(*v)
				}
			}
			if size > dbStudioResponseLimit {
				queries[i].Rows, queries[i].Partial, truncated = queries[i].Rows[:j], true, true
				break
			}
		}
		if truncated {
			queries = queries[:i+1]
			break
		}
	}
	out["results"] = queries
	if truncated {
		out["message"] = "The response is larger than 10 MB and was truncated: these are partial results."
	}
	return out, nil
}

// dbStudioExplorer is the Explorer's tree.
type dbStudioExplorer struct {
	Database string                 `json:"database"`
	User     string                 `json:"user"`
	Engine   string                 `json:"engine"`
	Schemas  []*dbStudioSchemaGroup `json:"schemas"`
}

type dbStudioSchemaGroup struct {
	Name       string           `json:"name"`
	Tables     []*dbStudioTable `json:"tables"`
	Views      []*dbStudioTable `json:"views"`
	Sequences  []string         `json:"sequences,omitempty"`
	Functions  []string         `json:"functions"`
	Procedures []string         `json:"procedures"`
}

type dbStudioTable struct {
	Name    string      `json:"name"`
	Kind    string      `json:"kind,omitempty"`
	Columns [][2]string `json:"columns"`
	Indexes []string    `json:"indexes"`
}

const dbStudioPostgresCatalog = `SELECT n.nspname FROM pg_catalog.pg_namespace n
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%' ORDER BY 1;
SELECT n.nspname, c.relname, c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'
 ORDER BY 1, 2;
SELECT table_schema, table_name, column_name, data_type FROM information_schema.columns
 WHERE table_schema NOT IN ('pg_catalog', 'information_schema') AND table_schema NOT LIKE 'pg\_%' ORDER BY table_schema, table_name, ordinal_position;
SELECT schemaname, tablename, indexname FROM pg_catalog.pg_indexes
 WHERE schemaname NOT IN ('pg_catalog', 'information_schema') AND schemaname NOT LIKE 'pg\_%' ORDER BY 1, 2, 3;
SELECT routine_schema, routine_name, routine_type FROM information_schema.routines
 WHERE routine_schema NOT IN ('pg_catalog', 'information_schema') AND routine_schema NOT LIKE 'pg\_%' ORDER BY 1, 2`

const dbStudioMySQLCatalog = `SELECT DATABASE();
SELECT DATABASE(), TABLE_NAME, IF(TABLE_TYPE = 'VIEW', 'v', 'r') FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME;
SELECT DATABASE(), TABLE_NAME, COLUMN_NAME, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION;
SELECT DISTINCT DATABASE(), TABLE_NAME, INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, INDEX_NAME;
SELECT DATABASE(), ROUTINE_NAME, ROUTINE_TYPE FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = DATABASE() ORDER BY ROUTINE_NAME`

// explorer reads the database's catalogue as the signed-in user, so it
// lists what that user can see.
func (b dbStudioBackend) explorer(ctx context.Context, l *dbStudioLogin) (*dbStudioExplorer, error) {
	catalog := dbStudioPostgresCatalog
	if l.engine == "MYSQL" {
		catalog = dbStudioMySQLCatalog
	}
	res, err := b.run(ctx, l, catalog)
	if err != nil {
		return nil, err
	}
	if len(res) != 5 {
		return nil, fmt.Errorf("the catalogue answered %d results, not 5", len(res))
	}
	out := &dbStudioExplorer{Database: l.database, User: l.role, Engine: l.engine, Schemas: []*dbStudioSchemaGroup{}}
	schemas := map[string]*dbStudioSchemaGroup{}
	schema := func(n string) *dbStudioSchemaGroup {
		if g, ok := schemas[n]; ok {
			return g
		}
		g := &dbStudioSchemaGroup{Name: n, Tables: []*dbStudioTable{}, Views: []*dbStudioTable{}, Functions: []string{}, Procedures: []string{}}
		schemas[n] = g
		out.Schemas = append(out.Schemas, g)
		return g
	}
	str := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	for _, r := range res[0].Rows {
		if len(r) > 0 {
			schema(str(r[0]))
		}
	}
	tables := map[string]*dbStudioTable{}
	for _, r := range res[1].Rows {
		if len(r) < 3 {
			continue
		}
		g, name := schema(str(r[0])), str(r[1])
		t := &dbStudioTable{Name: name, Columns: [][2]string{}, Indexes: []string{}}
		switch str(r[2]) {
		case "v":
			g.Views = append(g.Views, t)
		case "m":
			t.Kind = "materialized view"
			g.Views = append(g.Views, t)
		case "S":
			g.Sequences = append(g.Sequences, name)
			continue
		case "f":
			t.Kind = "foreign table"
			g.Tables = append(g.Tables, t)
		default:
			g.Tables = append(g.Tables, t)
		}
		tables[str(r[0])+"\x00"+name] = t
	}
	for _, r := range res[2].Rows {
		if len(r) >= 4 {
			if t := tables[str(r[0])+"\x00"+str(r[1])]; t != nil {
				t.Columns = append(t.Columns, [2]string{str(r[2]), str(r[3])})
			}
		}
	}
	for _, r := range res[3].Rows {
		if len(r) >= 3 {
			if t := tables[str(r[0])+"\x00"+str(r[1])]; t != nil {
				t.Indexes = append(t.Indexes, str(r[2]))
			}
		}
	}
	for _, r := range res[4].Rows {
		if len(r) < 3 {
			continue
		}
		g := schema(str(r[0]))
		if strings.EqualFold(str(r[2]), "PROCEDURE") {
			g.Procedures = append(g.Procedures, str(r[1]))
		} else {
			g.Functions = append(g.Functions, str(r[1]))
		}
	}
	return out, nil
}
