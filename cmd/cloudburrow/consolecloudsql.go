package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/identity-wael/cloudburrow/internal/components"
	"github.com/identity-wael/cloudburrow/internal/console"
)

// cloudSQLProvider shows the PostgreSQL running in the cluster.
//
// This screen is careful about one thing above all: it is not the Cloud SQL
// Admin API. Google publishes no Cloud SQL emulator, so what runs here is the
// database Cloud SQL runs underneath — a real PostgreSQL at a stable local
// address. Instances, connection names, IAM database authentication, backups
// and read replicas are all absent, and the screen says so rather than leaving
// a developer to infer compatibility CloudBurrow does not have.
type cloudSQLProvider struct{ endpoint string }

func (cloudSQLProvider) ID() string    { return "cloudsql" }
func (cloudSQLProvider) Title() string { return "Cloud SQL" }

// connect opens a connection to one database on the local server.
//
// No password: the server runs with trust authentication, consistently with
// the rest of CloudBurrow, which authenticates nothing. The endpoint is
// loopback-bound for that reason.
func (p cloudSQLProvider) connect(ctx context.Context, database string) (*pgx.Conn, error) {
	host, port, err := net.SplitHostPort(p.endpoint)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint %q: %w", p.endpoint, err)
	}
	if database == "" {
		database = components.CloudSQLDatabase
	}
	url := fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=disable",
		components.CloudSQLUser, host, port, database)
	return pgx.Connect(ctx, url)
}

const cloudSQLNote = "A real PostgreSQL, not the Cloud SQL Admin API. " +
	"Google publishes no Cloud SQL emulator, so instances, connection names, " +
	"IAM database authentication, backups and replicas do not exist here."

func (p cloudSQLProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{
		Columns:    []string{"Owner", "Encoding", "Size"},
		NameColumn: "Database",
		Noun:       "databases",
		Note:       cloudSQLNote,
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, "")
	if err != nil {
		base.Unavailable = "cannot reach PostgreSQL: " + err.Error()
		return base, nil
	}
	defer conn.Close(context.Background())

	// The catalogue is the database's own, so this reports what the server
	// holds rather than what CloudBurrow believes it holds. Templates are
	// excluded because they are not databases anyone uses.
	rows, err := conn.Query(ctx, `
		SELECT d.datname,
		       pg_catalog.pg_get_userbyid(d.datdba),
		       pg_catalog.pg_encoding_to_char(d.encoding),
		       pg_catalog.pg_size_pretty(pg_catalog.pg_database_size(d.datname))
		FROM pg_catalog.pg_database d
		WHERE NOT d.datistemplate
		ORDER BY d.datname`)
	if err != nil {
		base.Unavailable = "listing databases: " + err.Error()
		return base, nil
	}
	defer rows.Close()

	var items []console.Resource
	for rows.Next() {
		var name, owner, encoding, size string
		if err := rows.Scan(&name, &owner, &encoding, &size); err != nil {
			base.Unavailable = "reading databases: " + err.Error()
			return base, nil
		}
		items = append(items, console.Resource{
			Name: name,
			Fields: map[string]string{
				"Owner": owner, "Encoding": encoding, "Size": size,
			},
		})
	}
	if err := rows.Err(); err != nil {
		base.Unavailable = "reading databases: " + err.Error()
		return base, nil
	}
	base.Items, base.Total = items, len(items)
	return base, nil
}

// contents lists the tables in one database.
// Detail implements console.Driller for one database.
func (p cloudSQLProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// A database holds tables and a table holds columns, which is two levels
	// below the list screen. The catalogue query already selects from
	// information_schema.columns for the table listing's column COUNT; the
	// same view answers which columns they are.
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	if len(path) == 2 {
		return p.tableDetail(ctx, path[0], path[1])
	}
	name := path[0]
	list, err := p.contents(ctx, project, name)
	if err != nil {
		return console.Detail{}, err
	}
	// Owner, Encoding and Size are read for the listing and were dropped on
	// click-through; they are the answer to "what is this database", which is
	// what its own page is titled after.
	var summary []console.Property
	if parent, err := p.List(ctx, project); err == nil {
		summary = summariseFrom(parent, name)
	}
	// Two sections, so the tab strip has a real consumer rather than being
	// machinery nothing exercises. Schemas are a second aspect of the same
	// database, read from the same catalogue.
	schemas, _ := p.schemas(ctx, name)
	sections := []console.Section{{ID: "tables", Label: "Tables", Listing: list}}
	if schemas != nil {
		sections = append(sections, console.Section{
			ID: "schemas", Label: "Schemas", Listing: *schemas,
		})
	}
	// The rest of what a database holds, and what the server is doing right
	// now. Views and functions are objects a developer creates and then cannot
	// find; activity is the difference between a query that is slow and a query
	// that is waiting on a lock.
	sections = append(sections,
		p.relationsSection(ctx, name),
		p.routinesSection(ctx, name),
		p.activitySection(ctx, name),
		p.settingsSection(ctx, name),
		p.usersSection(ctx, name),
	)
	return console.Detail{
		Summary:     summary,
		Sections:    sections,
		Unavailable: list.Unavailable,
		Prompt:      list.Prompt,
	}, nil
}

// cloudSQLSection runs one catalogue query and renders it as a section.
//
// Every one of these is the same shape: connect, query, scan text columns, build
// rows. Writing it five times would mean five places to get the failure handling
// wrong, and the failure handling is the part that matters — a section that
// cannot be read must say so rather than render as empty, because an empty table
// reads as "this database has no views".
func (p cloudSQLProvider) cloudSQLSection(ctx context.Context, database string, sec catalogueSection) console.Section {
	out := console.Listing{
		Columns: sec.columns, NameColumn: sec.nameColumn, Noun: sec.noun,
	}
	section := console.Section{ID: sec.id, Label: sec.label, Listing: out, Note: sec.note}

	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, database)
	if err != nil {
		out.Unavailable = "cannot open " + database + ": " + err.Error()
		section.Listing = out
		return section
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(ctx, sec.sql, sec.args...)
	if err != nil {
		out.Unavailable = "reading " + sec.noun + ": " + err.Error()
		section.Listing = out
		return section
	}
	defer rows.Close()

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			out.Unavailable = "reading " + sec.noun + ": " + err.Error()
			section.Listing = out
			return section
		}
		if len(values) == 0 {
			continue
		}
		// The first column is the row's name; the rest map onto the declared
		// columns in order. A query whose column count and the section's column
		// count disagree is a bug in this file, so the shorter of the two wins
		// rather than panicking on a live page.
		fields := map[string]string{}
		for i, column := range sec.columns {
			if i+1 < len(values) {
				fields[column] = formatSQLValue(values[i+1])
			}
		}
		out.Items = append(out.Items, console.Resource{
			Name:   formatSQLValue(values[0]),
			Status: sec.status(fields),
			Fields: fields,
		})
	}
	if err := rows.Err(); err != nil {
		out.Unavailable = "reading " + sec.noun + ": " + err.Error()
	}
	out.Total = len(out.Items)
	out.AlwaysStatus = sec.alwaysStatus
	if out.Total == 0 && out.Unavailable == "" && sec.empty != "" {
		out.Note = sec.empty
	}
	section.Listing = out
	return section
}

// catalogueSection describes one read of the PostgreSQL catalogue.
type catalogueSection struct {
	id, label, noun, nameColumn string
	columns                     []string
	sql                         string
	args                        []any
	note, empty                 string
	alwaysStatus                bool
	// status derives a row's state word from its fields, for the sections where
	// there is one. Nil means the rows have no state.
	status func(map[string]string) string
}

func noStatus(map[string]string) string { return "" }

// relationsSection lists views and materialized views.
//
// A developer who creates a view and then looks for it on the tables list does
// not find it: information_schema.tables is filtered to BASE TABLE there,
// correctly, and nothing else listed the rest.
func (p cloudSQLProvider) relationsSection(ctx context.Context, database string) console.Section {
	return p.cloudSQLSection(ctx, database, catalogueSection{
		id: "views", label: "Views", noun: "views", nameColumn: "View",
		columns: []string{"Schema", "Kind", "Owner"},
		sql: `SELECT c.relname, n.nspname,
		             CASE c.relkind WHEN 'v' THEN 'view'
		                            WHEN 'm' THEN 'materialized view' END,
		             pg_catalog.pg_get_userbyid(c.relowner)
		      FROM pg_catalog.pg_class c
		      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		      WHERE c.relkind IN ('v', 'm')
		        AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		      ORDER BY n.nspname, c.relname
		      LIMIT $1`,
		args:   []any{detailLimit},
		status: noStatus,
		empty:  "This database has no views.",
	})
}

// routinesSection lists functions and procedures.
func (p cloudSQLProvider) routinesSection(ctx context.Context, database string) console.Section {
	return p.cloudSQLSection(ctx, database, catalogueSection{
		id: "routines", label: "Functions", noun: "functions", nameColumn: "Function",
		columns: []string{"Schema", "Kind", "Language", "Returns", "Arguments"},
		sql: `SELECT p.proname, n.nspname,
		             CASE p.prokind WHEN 'f' THEN 'function'
		                            WHEN 'p' THEN 'procedure'
		                            WHEN 'a' THEN 'aggregate'
		                            WHEN 'w' THEN 'window' END,
		             l.lanname,
		             pg_catalog.pg_get_function_result(p.oid),
		             pg_catalog.pg_get_function_arguments(p.oid)
		      FROM pg_catalog.pg_proc p
		      JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		      JOIN pg_catalog.pg_language l ON l.oid = p.prolang
		      WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		      ORDER BY n.nspname, p.proname
		      LIMIT $1`,
		args:   []any{detailLimit},
		status: noStatus,
		empty:  "This database has no functions outside the system schemas.",
	})
}

// activitySection is what the server is doing right now.
//
// The one section here that is not a schema read. A query that is slow and a
// query that is waiting on a lock look identical from outside the database, and
// pg_stat_activity is the only thing that tells them apart.
func (p cloudSQLProvider) activitySection(ctx context.Context, database string) console.Section {
	return p.cloudSQLSection(ctx, database, catalogueSection{
		id: "activity", label: "Activity", noun: "connections", nameColumn: "PID",
		columns: []string{"State", "User", "Application", "Waiting on", "Running for", "Query"},
		// The query text is included because it is the point, and it is the
		// user's own SQL against their own local database — not a credential
		// this console issued. Nothing here is written to the log.
		sql: `SELECT pid, state, usename, application_name,
		             COALESCE(wait_event_type || ':' || wait_event, ''),
		             COALESCE(to_char(now() - query_start, 'HH24:MI:SS'), ''),
		             LEFT(COALESCE(query, ''), 200)
		      FROM pg_catalog.pg_stat_activity
		      WHERE datname = current_database()
		      ORDER BY query_start DESC NULLS LAST
		      LIMIT $1`,
		args:         []any{detailLimit},
		alwaysStatus: true,
		status: func(fields map[string]string) string {
			// The wait wins over the state: "active" on a query blocked behind a
			// lock is true and useless, and "idle in transaction" is the state
			// that holds locks open and is worth colouring.
			if w := fields["Waiting on"]; w != "" && w != "—" {
				return "waiting"
			}
			return fields["State"]
		},
		note: "Read live from pg_stat_activity each time this tab is opened. " +
			"A row waiting on a lock reads \"waiting\" whatever its own state says, " +
			"because \"active\" on a blocked query is true and useless.",
		empty: "No connections to this database, which cannot include this one — " +
			"so the read itself failed silently if you are seeing this.",
	})
}

// settingsSection is the server's configuration, as the server reports it.
//
// Cloud SQL calls these database flags. This is not the Cloud SQL Admin API, so
// they cannot be set from here, and the section says so rather than leaving the
// absence of an edit control unexplained.
func (p cloudSQLProvider) settingsSection(ctx context.Context, database string) console.Section {
	return p.cloudSQLSection(ctx, database, catalogueSection{
		id: "settings", label: "Server settings", noun: "settings", nameColumn: "Setting",
		columns: []string{"Value", "Unit", "Set by", "Description"},
		// A chosen list rather than all of pg_settings: the whole view is about
		// 350 rows, which is a wall rather than an answer. These are the ones
		// that change how an application behaves.
		sql: `SELECT name, setting, COALESCE(unit, ''), source, short_desc
		      FROM pg_catalog.pg_settings
		      WHERE name IN (
		        'server_version', 'max_connections', 'shared_buffers',
		        'work_mem', 'maintenance_work_mem', 'effective_cache_size',
		        'statement_timeout', 'idle_in_transaction_session_timeout',
		        'lock_timeout', 'default_transaction_isolation',
		        'default_transaction_read_only', 'timezone', 'log_statement',
		        'max_wal_size', 'wal_level', 'fsync', 'synchronous_commit')
		      ORDER BY name`,
		status: noStatus,
		note: "Reported by the server, not set from here: this is a real " +
			"PostgreSQL rather than the Cloud SQL Admin API, so there is no " +
			"database-flags API to change them through.",
	})
}

// usersSection lists the roles that can connect.
func (p cloudSQLProvider) usersSection(ctx context.Context, database string) console.Section {
	return p.cloudSQLSection(ctx, database, catalogueSection{
		id: "users", label: "Users", noun: "users", nameColumn: "Role",
		columns: []string{"Superuser", "Create DB", "Create role", "Connections", "Valid until"},
		// pg_roles rather than pg_shadow or pg_authid: those carry the password
		// hash, and a console that selected it would be putting credentials one
		// query away from a page.
		sql: `SELECT rolname, rolsuper, rolcreatedb, rolcreaterole,
		             CASE WHEN rolconnlimit < 0 THEN 'unlimited'
		                  ELSE rolconnlimit::text END,
		             COALESCE(rolvaliduntil::text, 'never')
		      FROM pg_catalog.pg_roles
		      WHERE rolcanlogin
		      ORDER BY rolname`,
		status: noStatus,
		note: "Read from pg_roles, which holds no password material. Users cannot " +
			"be created from here: this is not the Cloud SQL Admin API.",
	})
}

// tableDetail is one table's own page: its columns, its indexes and its keys.
//
// The level a developer actually opens a database console to reach. Until now
// the console could express "which tables exist" and stopped, because a path
// of one name has nowhere to put the table.
func (p cloudSQLProvider) tableDetail(ctx context.Context, database, table string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, database)
	if err != nil {
		return console.Detail{Unavailable: "cannot open " + database + ": " + err.Error()}, nil
	}
	defer conn.Close(context.Background())

	// The identifier is a parameter, never interpolated: this is the same
	// guard validSQLIdentifier exists for on the create path, and a table
	// name reaches here from a URL.
	columns := console.Listing{
		Columns:    []string{"Type", "Nullable", "Default", "Position"},
		NameColumn: "Column",
		Noun:       "columns",
	}
	rows, err := conn.Query(ctx, `
		SELECT column_name, data_type, is_nullable,
		       COALESCE(column_default, ''), ordinal_position
		FROM information_schema.columns
		WHERE table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		columns.Unavailable = "listing columns: " + err.Error()
	} else {
		defer rows.Close()
		for rows.Next() {
			var name, dataType, nullable, def string
			var position int
			if err := rows.Scan(&name, &dataType, &nullable, &def, &position); err != nil {
				columns.Unavailable = "reading columns: " + err.Error()
				break
			}
			columns.Items = append(columns.Items, console.Resource{
				Name: name,
				Fields: map[string]string{
					"Type": dataType, "Nullable": nullable,
					"Default": def, "Position": fmt.Sprint(position),
				},
			})
		}
		columns.Total = len(columns.Items)
	}

	indexes := console.Listing{
		Columns:    []string{"Definition"},
		NameColumn: "Index",
		Noun:       "indexes",
	}
	idx, err := conn.Query(ctx, `
		SELECT indexname, indexdef FROM pg_catalog.pg_indexes
		WHERE tablename = $1 ORDER BY indexname`, table)
	if err != nil {
		indexes.Unavailable = "listing indexes: " + err.Error()
	} else {
		defer idx.Close()
		for idx.Next() {
			var name, def string
			if err := idx.Scan(&name, &def); err != nil {
				indexes.Unavailable = "reading indexes: " + err.Error()
				break
			}
			indexes.Items = append(indexes.Items, console.Resource{
				Name: name, Fields: map[string]string{"Definition": def},
			})
		}
		indexes.Total = len(indexes.Items)
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Database", Value: database},
			{Label: "Columns", Value: fmt.Sprint(len(columns.Items))},
			{Label: "Indexes", Value: fmt.Sprint(len(indexes.Items))},
		},
		Sections: []console.Section{
			{ID: "columns", Label: "Columns", Listing: columns},
			{ID: "indexes", Label: "Indexes", Listing: indexes},
		},
	}, nil
}

// schemas lists the database's own schemas.
//
// Returns nil rather than an empty listing when the catalogue cannot be read:
// a tab that opens onto nothing is worse than a tab that is not there.
func (p cloudSQLProvider) schemas(ctx context.Context, name string) (*console.Listing, error) {
	out := &console.Listing{
		Columns:    []string{"Owner", "Tables"},
		NameColumn: "Schema",
		Noun:       "schemas",
	}
	conn, err := p.connect(ctx, name)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(ctx, `
		SELECT s.schema_name, s.schema_owner, COUNT(t.table_name)
		FROM information_schema.schemata s
		LEFT JOIN information_schema.tables t
		  ON t.table_schema = s.schema_name AND t.table_type = 'BASE TABLE'
		WHERE s.schema_name NOT IN ('pg_catalog', 'information_schema')
		  AND s.schema_name NOT LIKE 'pg_%'
		GROUP BY s.schema_name, s.schema_owner
		ORDER BY s.schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []console.Resource
	for rows.Next() {
		var schema, owner string
		var tables int
		if err := rows.Scan(&schema, &owner, &tables); err != nil {
			return nil, err
		}
		items = append(items, console.Resource{
			Name:   schema,
			Fields: map[string]string{"Owner": owner, "Tables": fmt.Sprintf("%d", tables)},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Items, out.Total = items, len(items)
	return out, nil
}

func (p cloudSQLProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
	return p.tablesPage(ctx, name, 0)
}

// tablesPage reads one page of a database's tables.
//
// The cursor is a row offset. An offset into an ordered catalogue read is stable
// enough for this: the ordering is total (schema, then name), so a table created
// between two pages shifts at most one row rather than reshuffling the set.
func (p cloudSQLProvider) tablesPage(ctx context.Context, name string, offset int) (console.Listing, error) {
	out := console.Listing{
		Columns:      []string{"Schema", "Columns", "Size"},
		NameColumn:   "Table",
		Noun:         "tables",
		RowsOpenable: true,
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, name)
	if err != nil {
		out.Unavailable = "cannot open " + name + ": " + err.Error()
		return out, nil
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(ctx, `
		SELECT t.table_schema, t.table_name, COUNT(c.column_name),
		       pg_catalog.pg_size_pretty(
		         pg_catalog.pg_total_relation_size(
		           quote_ident(t.table_schema) || '.' || quote_ident(t.table_name)))
		FROM information_schema.tables t
		LEFT JOIN information_schema.columns c
		  ON c.table_schema = t.table_schema AND c.table_name = t.table_name
		WHERE t.table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND t.table_type = 'BASE TABLE'
		GROUP BY t.table_schema, t.table_name
		ORDER BY t.table_schema, t.table_name
		LIMIT $1 OFFSET $2`, detailLimit+1, offset)
	if err != nil {
		out.Unavailable = "listing tables: " + err.Error()
		return out, nil
	}
	defer rows.Close()

	var items []console.Resource
	for rows.Next() {
		var schema, table, size string
		var columns int
		if err := rows.Scan(&schema, &table, &columns, &size); err != nil {
			out.Unavailable = "reading tables: " + err.Error()
			return out, nil
		}
		items = append(items, console.Resource{
			Name: table,
			Fields: map[string]string{
				"Schema": schema, "Columns": fmt.Sprintf("%d", columns), "Size": size,
			},
		})
	}
	if err := rows.Err(); err != nil {
		out.Unavailable = "reading tables: " + err.Error()
		return out, nil
	}
	// One row more than the page is read, and dropped. It is the only way to
	// know whether a next page exists without offering a button that fetches
	// nothing.
	if len(items) > detailLimit {
		items = items[:detailLimit]
		out.More = true
		out.Cursor = strconv.Itoa(offset + detailLimit)
	}
	out.Items, out.Total = items, len(items)
	return out, nil
}

// Page implements console.Pager for a database's tables.
func (p cloudSQLProvider) Page(ctx context.Context, project string, path []string, cursor string) (console.Listing, error) {
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("only a database's table list can be paged")
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		// A cursor this provider did not issue is an error, not an empty page:
		// silently returning nothing would look identical to reaching the end.
		return console.Listing{}, fmt.Errorf("not a cursor this screen issued: %q", cursor)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return p.tablesPage(ctx, path[0], offset)
}

// CreateForm implements console.Creator.
func (cloudSQLProvider) CreateForm() (string, []console.Field) {
	return "Create database", []console.Field{
		{
			Name: "database", Label: "Database name", Type: "text", Required: true,
			Help:    "Lowercase letters, digits and underscores.",
			Pattern: `^[a-z][a-z0-9_]{0,62}$`,
		},
	}
}

// Create implements console.Creator.
func (p cloudSQLProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["database"])
	if name == "" {
		return "", fmt.Errorf("a database name is required")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, "")
	if err != nil {
		return "", fmt.Errorf("cannot reach PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())

	// CREATE DATABASE takes no parameters, so the identifier is quoted rather
	// than interpolated. The form's pattern already refuses anything that
	// would need escaping; this is the second guard, because one of them will
	// eventually be edited.
	if err := validSQLIdentifier(name); err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", fmt.Errorf("creating the database: %w", err)
	}
	return name, nil
}

// Delete implements console.Deleter.
func (p cloudSQLProvider) Delete(ctx context.Context, project, name string) error {
	if name == components.CloudSQLDatabase {
		return fmt.Errorf("%q is the database the server was initialised with and cannot be dropped here", name)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, "")
	if err != nil {
		return fmt.Errorf("cannot reach PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())

	if err := validSQLIdentifier(name); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `DROP DATABASE `+pgx.Identifier{name}.Sanitize())
	return err
}

// validSQLIdentifier refuses anything that is not a plain identifier.
//
// Sanitize quotes correctly on its own; this rejects the input earlier and
// with a message that says what is wrong, rather than relying on quoting to
// make an odd name harmless.
func validSQLIdentifier(name string) error {
	if name == "" || len(name) > 63 {
		return fmt.Errorf("database name must be between 1 and 63 characters")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("database name %q: only lowercase letters, digits and "+
				"underscores are allowed, and it cannot start with a digit", name)
		}
	}
	return nil
}

var (
	_ console.Creator = cloudSQLProvider{}
	_ console.Deleter = cloudSQLProvider{}
	_ console.Driller = cloudSQLProvider{}
)

// QueryHint implements console.Executor.
func (cloudSQLProvider) QueryHint() string {
	return "Read-only. Statements run inside a READ ONLY transaction, so " +
		"PostgreSQL itself refuses a write — this console does not inspect " +
		"your SQL to decide."
}

// Query implements console.Executor for one database.
//
// Read-only, and enforced by the server rather than by this code. The obvious
// alternative — scanning the statement for INSERT or UPDATE — is a blocklist,
// and a blocklist is wrong the first time somebody writes a CTE, a function
// call with a side effect, or simply different whitespace. BEGIN READ ONLY
// makes PostgreSQL the authority, and its refusal is the message the user
// sees.
//
// Why read-only at all, on a local emulator: a write from a console pane
// would be this project's first write into a developer's own data, as opposed
// to resources the console created. That is a decision to take deliberately
// and record in docs/compatibility.md, not one to arrive at because an editor
// happened to accept anything.
func (p cloudSQLProvider) Query(ctx context.Context, _ string, path []string, statement string) (console.Listing, error) {
	if len(path) == 0 {
		return console.Listing{}, fmt.Errorf("a database is required")
	}
	database := path[0]

	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	conn, err := p.connect(ctx, database)
	if err != nil {
		return console.Listing{}, fmt.Errorf("cannot open %s: %w", database, err)
	}
	defer conn.Close(context.Background())

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return console.Listing{}, fmt.Errorf("cannot begin a read-only transaction: %w", err)
	}
	// Always rolled back: nothing here is allowed to commit, and saying so in
	// the code costs nothing.
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, statement)
	if err != nil {
		// PostgreSQL's own message, unchanged. A syntax error names the
		// character, and a read-only violation names the statement — both are
		// the whole content of the answer.
		return console.Listing{}, err
	}
	defer rows.Close()

	// The columns are the query's own, taken from what the server described,
	// not a fixed set this code decided on.
	var columns []string
	for _, f := range rows.FieldDescriptions() {
		columns = append(columns, f.Name)
	}

	out := console.Listing{Noun: "rows"}
	if len(columns) > 0 {
		out.NameColumn = columns[0]
		out.Columns = columns[1:]
	}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return console.Listing{}, err
		}
		item := console.Resource{Fields: map[string]string{}}
		for i, v := range values {
			text := formatSQLValue(v)
			if i == 0 {
				item.Name = text
				continue
			}
			item.Fields[columns[i]] = text
		}
		out.Items = append(out.Items, item)
		if len(out.Items) >= detailLimit {
			out.Note = truncatedNote(len(out.Items), "rows")
			break
		}
	}
	if err := rows.Err(); err != nil {
		return console.Listing{}, err
	}
	out.Total = len(out.Items)
	return out, nil
}

// formatSQLValue renders one cell.
//
// A NULL is an em dash rather than the empty string, because "no value" and
// "the empty string" are different answers and a table that renders them
// identically is lying about one of them.
func formatSQLValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "—"
	case []byte:
		return string(value)
	case time.Time:
		return value.Format(time.RFC3339)
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}
