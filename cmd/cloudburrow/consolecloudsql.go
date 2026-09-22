package main

import (
	"context"
	"fmt"
	"net"
	"strings"

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
	return console.Detail{
		Summary:     summary,
		Sections:    sections,
		Unavailable: list.Unavailable,
		Prompt:      list.Prompt,
	}, nil
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
		LIMIT $1`, detailLimit)
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
	out.Items, out.Total = items, len(items)
	out.Note = truncatedNote(len(items), "tables")
	return out, nil
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
