package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// cloudSQLMySQLProvider shows the MySQL running in the cluster (#868).
//
// The MySQL counterpart of cloudSQLProvider, and careful about the same thing:
// it is not the Cloud SQL Admin API. What runs is the database Cloud SQL for
// MySQL runs underneath — one real MySQL server at a stable local address — so
// the screen lists that server's databases rather than inventing instances,
// connection names or flags it has no API for.
//
// Two MySQL accounts are used, for two different jobs:
//
//   - root, from the instance's generated credentials, reads the catalogue and
//     creates and drops databases. The application's own user is granted only
//     its database, so it can neither see the others nor create one.
//   - cloudburrow_console, an account this screen creates holding SELECT and
//     SHOW VIEW on the one database being queried, runs the SQL editor's
//     statements. See Query for why a read-only transaction is not enough on
//     its own in MySQL.
type cloudSQLMySQLProvider struct {
	endpoint string
	creds    components.MySQLCredentials
	// readerPassword is the query account's password, generated once per
	// provider and set on the account before each query, so it is never
	// stored and a restarted console simply sets a new one.
	readerPassword string
}

// newCloudSQLMySQLProvider builds the screen for the server at addr.
func newCloudSQLMySQLProvider(addr string, creds components.MySQLCredentials) cloudSQLMySQLProvider {
	return cloudSQLMySQLProvider{endpoint: addr, creds: creds, readerPassword: randomPassword()}
}

func (cloudSQLMySQLProvider) ID() string    { return "cloudsql-mysql" }
func (cloudSQLMySQLProvider) Title() string { return "Cloud SQL for MySQL" }

// mysqlConsoleReader is the account the SQL editor's statements run as.
const mysqlConsoleReader = "cloudburrow_console"

const cloudSQLMySQLNote = "A real MySQL, not the Cloud SQL Admin API. " +
	"Google publishes no Cloud SQL emulator, so instances, connection names, " +
	"IAM database authentication, backups and replicas do not exist here."

// open returns a handle on one database as user, or on the server when
// database is empty.
//
// One connection, opened for the request and closed after it, as the
// PostgreSQL screen does: the console is not a connection pool, and a handle
// left open would hold a session on the developer's server between clicks.
func (p cloudSQLMySQLProvider) open(user, password, database string) (*sql.DB, error) {
	if p.endpoint == "" {
		return nil, errors.New("the MySQL tunnel has no address")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = user, password, "tcp", p.endpoint, database
	cfg.Timeout = dbTimeout
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (p cloudSQLMySQLProvider) root(database string) (*sql.DB, error) {
	return p.open("root", p.creds.RootPassword, database)
}

// server describes the one server behind the screen, for the listing's note:
// what the Cloud SQL console would call the instance is exactly this.
func (p cloudSQLMySQLProvider) server(ctx context.Context, db *sql.DB) string {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return cloudSQLMySQLNote
	}
	return fmt.Sprintf("One server: MySQL %s at %s. %s", version, p.endpoint, cloudSQLMySQLNote)
}

// List implements console.Provider: the server's databases.
func (p cloudSQLMySQLProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{
		Columns:    []string{"Character set", "Collation", "Tables", "Size"},
		NameColumn: "Database",
		Noun:       "databases",
		Note:       cloudSQLMySQLNote,
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	db, err := p.root("")
	if err != nil {
		base.Unavailable = "cannot reach MySQL: " + err.Error()
		return base, nil
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		base.Unavailable = "cannot reach MySQL: " + err.Error()
		return base, nil
	}
	base.Note = p.server(ctx, db)

	// The server's own catalogue, so this reports what MySQL holds rather than
	// what CloudBurrow believes it holds. MySQL's own schemas are left out,
	// the set reset leaves alone, because nobody's application lives in them.
	rows, err := db.QueryContext(ctx, `
		SELECT s.SCHEMA_NAME, s.DEFAULT_CHARACTER_SET_NAME, s.DEFAULT_COLLATION_NAME,
		       COUNT(t.TABLE_NAME),
		       COALESCE(SUM(t.DATA_LENGTH + t.INDEX_LENGTH), 0)
		FROM information_schema.SCHEMATA s
		LEFT JOIN information_schema.TABLES t
		  ON t.TABLE_SCHEMA = s.SCHEMA_NAME AND t.TABLE_TYPE = 'BASE TABLE'
		WHERE s.SCHEMA_NAME NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
		GROUP BY s.SCHEMA_NAME, s.DEFAULT_CHARACTER_SET_NAME, s.DEFAULT_COLLATION_NAME
		ORDER BY s.SCHEMA_NAME`)
	if err != nil {
		base.Unavailable = "listing databases: " + err.Error()
		return base, nil
	}
	defer rows.Close()
	for rows.Next() {
		var name, charset, collation string
		var tables, size int64
		if err := rows.Scan(&name, &charset, &collation, &tables, &size); err != nil {
			base.Unavailable = "reading databases: " + err.Error()
			return base, nil
		}
		base.Items = append(base.Items, console.Resource{
			Name: name,
			Fields: map[string]string{
				"Character set": charset, "Collation": collation,
				"Tables": strconv.FormatInt(tables, 10), "Size": humanBytes(size),
			},
		})
	}
	if err := rows.Err(); err != nil {
		base.Unavailable = "reading databases: " + err.Error()
		return base, nil
	}
	base.Total = len(base.Items)
	return base, nil
}

// systemDatabaseRefusal is why a system database is not browsed or queried.
//
// The listing leaves them out; this is the same rule for a path typed into a
// URL or posted to the query route. mysql holds the password hashes, and the
// query account would be granted SELECT on whatever database it is asked for.
func systemDatabaseRefusal(name string) error {
	if mysqlSystemDatabases[strings.ToLower(name)] {
		return fmt.Errorf("%q is one of MySQL's own system databases, which this console does not open", name)
	}
	return nil
}

// Detail implements console.Driller: a database, or one table in it.
func (p cloudSQLMySQLProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	if err := systemDatabaseRefusal(path[0]); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	if len(path) == 2 {
		return p.tableDetail(ctx, path[0], path[1])
	}
	name := path[0]
	tables, err := p.tablesPage(ctx, name, 0)
	if err != nil {
		return console.Detail{}, err
	}
	var summary []console.Property
	if parent, err := p.List(ctx, project); err == nil {
		summary = summariseFrom(parent, name)
	}
	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "tables", Label: "Tables", Listing: tables},
			p.viewsSection(ctx, name),
			p.routinesSection(ctx, name),
			p.activitySection(ctx, name),
			p.settingsSection(ctx, name),
			p.usersSection(ctx, name),
		},
		Unavailable: tables.Unavailable,
	}, nil
}

// tablesPage reads one page of a database's tables, with the same offset
// cursor as the PostgreSQL screen's.
func (p cloudSQLMySQLProvider) tablesPage(ctx context.Context, name string, offset int) (console.Listing, error) {
	out := console.Listing{
		Columns:      []string{"Engine", "Columns", "Size"},
		NameColumn:   "Table",
		Noun:         "tables",
		RowsOpenable: true,
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	db, err := p.root(name)
	if err != nil {
		out.Unavailable = "cannot open " + name + ": " + err.Error()
		return out, nil
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT t.TABLE_NAME, COALESCE(t.ENGINE, ''),
		       (SELECT COUNT(*) FROM information_schema.COLUMNS c
		        WHERE c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME),
		       COALESCE(t.DATA_LENGTH + t.INDEX_LENGTH, 0)
		FROM information_schema.TABLES t
		WHERE t.TABLE_SCHEMA = ? AND t.TABLE_TYPE = 'BASE TABLE'
		ORDER BY t.TABLE_NAME
		LIMIT ? OFFSET ?`, name, detailLimit+1, offset)
	if err != nil {
		out.Unavailable = "cannot open " + name + ": " + err.Error()
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var table, engine string
		var columns, size int64
		if err := rows.Scan(&table, &engine, &columns, &size); err != nil {
			out.Unavailable = "reading tables: " + err.Error()
			return out, nil
		}
		out.Items = append(out.Items, console.Resource{
			Name: table,
			Fields: map[string]string{
				"Engine": orDash(engine), "Columns": strconv.FormatInt(columns, 10), "Size": humanBytes(size),
			},
		})
	}
	if err := rows.Err(); err != nil {
		out.Unavailable = "reading tables: " + err.Error()
		return out, nil
	}
	// One row more than the page is read, and dropped: the only way to know a
	// next page exists without offering a button that fetches nothing.
	if len(out.Items) > detailLimit {
		out.Items = out.Items[:detailLimit]
		out.More = true
		out.Cursor = strconv.Itoa(offset + detailLimit)
	}
	out.Total = len(out.Items)
	return out, nil
}

// Page implements console.Pager for a database's tables.
func (p cloudSQLMySQLProvider) Page(ctx context.Context, project string, path []string, cursor string) (console.Listing, error) {
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("only a database's table list can be paged")
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return console.Listing{}, fmt.Errorf("not a cursor this screen issued: %q", cursor)
	}
	if err := systemDatabaseRefusal(path[0]); err != nil {
		return console.Listing{}, err
	}
	return p.tablesPage(ctx, path[0], offset)
}

// tableDetail is one table's page: its columns and its indexes.
func (p cloudSQLMySQLProvider) tableDetail(ctx context.Context, database, table string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	db, err := p.root(database)
	if err != nil {
		return console.Detail{Unavailable: "cannot open " + database + ": " + err.Error()}, nil
	}
	defer db.Close()

	// Both names are parameters, never interpolated: they reach here from a URL.
	columns := p.catalogue(ctx, db, catalogueSection{
		noun: "columns", nameColumn: "Column",
		columns: []string{"Type", "Nullable", "Default", "Key", "Position"},
		sql: `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT,
		             COLUMN_KEY, ORDINAL_POSITION
		      FROM information_schema.COLUMNS
		      WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		      ORDER BY ORDINAL_POSITION`,
		args: []any{database, table},
	})
	if columns.Unavailable == "" && columns.Total == 0 {
		// A table that is not there is said to be not there, rather than
		// drawn as a table with no columns — which MySQL does not allow.
		return console.Detail{Unavailable: fmt.Sprintf("%s has no table %q", database, table)}, nil
	}
	indexes := p.catalogue(ctx, db, catalogueSection{
		noun: "indexes", nameColumn: "Index",
		columns: []string{"Columns", "Unique", "Type"},
		sql: `SELECT INDEX_NAME,
		             GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX SEPARATOR ', '),
		             IF(MIN(NON_UNIQUE) = 0, 'Yes', 'No'), MIN(INDEX_TYPE)
		      FROM information_schema.STATISTICS
		      WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		      GROUP BY INDEX_NAME
		      ORDER BY INDEX_NAME`,
		args:  []any{database, table},
		empty: "This table has no indexes.",
	})
	return console.Detail{
		Summary: []console.Property{
			{Label: "Database", Value: database},
			{Label: "Columns", Value: strconv.Itoa(columns.Total)},
			{Label: "Indexes", Value: strconv.Itoa(indexes.Total)},
		},
		Sections: []console.Section{
			{ID: "columns", Label: "Columns", Listing: columns},
			{ID: "indexes", Label: "Indexes", Listing: indexes},
		},
	}, nil
}

// catalogue runs one catalogue read and renders it as a listing: the first
// column is the row's name, the rest map onto the declared columns in order.
//
// A read that fails says so in the listing rather than rendering as empty,
// because an empty table reads as "this database has none".
func (p cloudSQLMySQLProvider) catalogue(ctx context.Context, db *sql.DB, sec catalogueSection) console.Listing {
	out := console.Listing{Columns: sec.columns, NameColumn: sec.nameColumn, Noun: sec.noun}
	_, values, err := queryAll(ctx, db, sec.sql, sec.args...)
	if err != nil {
		out.Unavailable = "reading " + sec.noun + ": " + err.Error()
		return out
	}
	for _, row := range values {
		if len(row) == 0 {
			continue
		}
		fields := map[string]string{}
		for i, column := range sec.columns {
			if i+1 < len(row) {
				fields[column] = orDash(collapseWhitespace(formatSQLValue(row[i+1])))
			}
		}
		status := ""
		if sec.status != nil {
			status = sec.status(fields)
		}
		out.Items = append(out.Items, console.Resource{
			Name: formatSQLValue(row[0]), Status: status, Fields: fields,
		})
	}
	out.Total = len(out.Items)
	out.AlwaysStatus = sec.alwaysStatus
	if out.Total == 0 && sec.empty != "" {
		out.Note = sec.empty
	}
	return out
}

// section opens database as root, runs one catalogue read and wraps it.
func (p cloudSQLMySQLProvider) section(ctx context.Context, database string, sec catalogueSection) console.Section {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	section := console.Section{ID: sec.id, Label: sec.label, Note: sec.note}
	db, err := p.root(database)
	if err != nil {
		section.Listing = console.Listing{
			Columns: sec.columns, NameColumn: sec.nameColumn, Noun: sec.noun,
			Unavailable: "cannot open " + database + ": " + err.Error(),
		}
		return section
	}
	defer db.Close()
	section.Listing = p.catalogue(ctx, db, sec)
	return section
}

// viewsSection lists the database's views, which the tables list leaves out.
func (p cloudSQLMySQLProvider) viewsSection(ctx context.Context, database string) console.Section {
	return p.section(ctx, database, catalogueSection{
		id: "views", label: "Views", noun: "views", nameColumn: "View",
		columns: []string{"Updatable", "Definer", "Security"},
		sql: `SELECT TABLE_NAME, IS_UPDATABLE, DEFINER, SECURITY_TYPE
		      FROM information_schema.VIEWS
		      WHERE TABLE_SCHEMA = ?
		      ORDER BY TABLE_NAME
		      LIMIT ?`,
		args:  []any{database, detailLimit},
		empty: "This database has no views.",
	})
}

// routinesSection lists stored functions and procedures.
func (p cloudSQLMySQLProvider) routinesSection(ctx context.Context, database string) console.Section {
	return p.section(ctx, database, catalogueSection{
		id: "routines", label: "Routines", noun: "routines", nameColumn: "Routine",
		columns: []string{"Kind", "Returns", "Definer"},
		sql: `SELECT ROUTINE_NAME, LOWER(ROUTINE_TYPE), COALESCE(DTD_IDENTIFIER, ''), DEFINER
		      FROM information_schema.ROUTINES
		      WHERE ROUTINE_SCHEMA = ?
		      ORDER BY ROUTINE_NAME
		      LIMIT ?`,
		args:  []any{database, detailLimit},
		empty: "This database has no stored functions or procedures.",
	})
}

// activitySection is what the server is doing in this database right now.
func (p cloudSQLMySQLProvider) activitySection(ctx context.Context, database string) console.Section {
	return p.section(ctx, database, catalogueSection{
		id: "activity", label: "Activity", noun: "connections", nameColumn: "ID",
		columns: []string{"Command", "User", "Host", "State", "Running for", "Query"},
		// performance_schema.processlist rather than information_schema's,
		// which MySQL 8 deprecates. The query text is the user's own SQL
		// against their own local database, and nothing here is logged.
		sql: `SELECT ID, COMMAND, USER, HOST, COALESCE(STATE, ''),
		             CONCAT(TIME, 's'), LEFT(COALESCE(INFO, ''), 200)
		      FROM performance_schema.processlist
		      WHERE DB = ?
		      ORDER BY TIME DESC
		      LIMIT ?`,
		args:         []any{database, detailLimit},
		alwaysStatus: true,
		status:       func(fields map[string]string) string { return strings.ToLower(fields["Command"]) },
		note:         "Read live from performance_schema.processlist each time this tab is opened.",
		empty: "No connections to this database, which cannot include this one — " +
			"so the read itself failed silently if you are seeing this.",
	})
}

// settingsSection is the server's configuration, as the server reports it.
func (p cloudSQLMySQLProvider) settingsSection(ctx context.Context, database string) console.Section {
	return p.section(ctx, database, catalogueSection{
		id: "settings", label: "Server settings", noun: "settings", nameColumn: "Setting",
		columns: []string{"Value"},
		// A chosen list rather than all of global_variables, which is several
		// hundred rows: the ones that change how an application behaves.
		sql: `SELECT VARIABLE_NAME, VARIABLE_VALUE
		      FROM performance_schema.global_variables
		      WHERE VARIABLE_NAME IN (
		        'version', 'max_connections', 'innodb_buffer_pool_size',
		        'character_set_server', 'collation_server', 'time_zone',
		        'sql_mode', 'transaction_isolation', 'autocommit',
		        'wait_timeout', 'max_allowed_packet', 'lower_case_table_names',
		        'default_storage_engine', 'max_execution_time')
		      ORDER BY VARIABLE_NAME`,
		note: "Reported by the server, not set from here: this is a real MySQL " +
			"rather than the Cloud SQL Admin API, so there is no database-flags " +
			"API to change them through.",
	})
}

// usersSection lists the accounts that can log in.
func (p cloudSQLMySQLProvider) usersSection(ctx context.Context, database string) console.Section {
	return p.section(ctx, database, catalogueSection{
		id: "users", label: "Users", noun: "users", nameColumn: "User",
		columns: []string{"Host", "Authentication", "Password expired"},
		// Named columns only: mysql.user also carries authentication_string,
		// the password hash, and a console that selected * would be putting a
		// credential one query away from a page. Locked accounts are MySQL's
		// own internal ones, which nobody logs in as.
		sql: `SELECT User, Host, plugin, IF(password_expired = 'Y', 'Yes', 'No')
		      FROM mysql.user
		      WHERE account_locked = 'N'
		      ORDER BY User, Host`,
		note: "Read from mysql.user without its password column. " + mysqlConsoleReader +
			" is the account this console's SQL editor runs as. Users cannot be " +
			"created from here: this is not the Cloud SQL Admin API.",
	})
}

// CreateForm implements console.Creator.
func (cloudSQLMySQLProvider) CreateForm() (string, []console.Field) {
	return "Create database", []console.Field{
		{
			Name: "database", Label: "Database name", Type: "text", Required: true,
			Help:    "Lowercase letters, digits and underscores.",
			Pattern: `^[a-z][a-z0-9_]{0,62}$`,
		},
	}
}

// Create implements console.Creator: CREATE DATABASE, and the application's
// user granted it.
//
// The grant is what makes the database usable. On PostgreSQL the server's one
// user owns everything it creates; on MySQL the application's user holds
// privileges on its own database only, so a database created as root with no
// grant would be one the application cannot open.
func (p cloudSQLMySQLProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["database"])
	if name == "" {
		return "", fmt.Errorf("a database name is required")
	}
	// CREATE DATABASE takes no parameters, so the identifier is validated and
	// quoted rather than interpolated.
	if err := validSQLIdentifier(name); err != nil {
		return "", err
	}
	if err := systemDatabaseRefusal(name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	db, err := p.root("")
	if err != nil {
		return "", fmt.Errorf("cannot reach MySQL: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+quoteMySQLIdent(name)); err != nil {
		return "", fmt.Errorf("creating the database: %w", err)
	}
	if _, err := db.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+quoteMySQLIdent(name)+".* TO "+
		mysqlAccount(components.CloudSQLUser)); err != nil {
		return "", fmt.Errorf("the database was created, but granting it to %s failed: %w",
			components.CloudSQLUser, err)
	}
	return name, nil
}

// Delete implements console.Deleter: DROP DATABASE.
func (p cloudSQLMySQLProvider) Delete(ctx context.Context, project, name string) error {
	if name == components.CloudSQLDatabase {
		return fmt.Errorf("%q is the database the server was initialised with and cannot be dropped here", name)
	}
	if err := systemDatabaseRefusal(name); err != nil {
		return err
	}
	if err := validSQLIdentifier(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	db, err := p.root("")
	if err != nil {
		return fmt.Errorf("cannot reach MySQL: %w", err)
	}
	defer db.Close()
	// Grants on the name are left in place, as MySQL's own DROP DATABASE
	// leaves them: a database created again under that name is the
	// application's again.
	_, err = db.ExecContext(ctx, "DROP DATABASE "+quoteMySQLIdent(name))
	return err
}

// QueryHint implements console.Executor.
func (cloudSQLMySQLProvider) QueryHint() string {
	return "Read-only. Statements run as " + mysqlConsoleReader + ", an account " +
		"holding only SELECT on this database, inside START TRANSACTION READ ONLY, " +
		"so MySQL itself refuses a write or a schema change — this console does " +
		"not inspect your SQL to decide."
}

// Query implements console.Executor for one database.
//
// Read-only, and enforced by the server, as on the PostgreSQL screen. MySQL's
// equivalent of PostgreSQL's BEGIN READ ONLY is START TRANSACTION READ ONLY,
// and it is used — but on its own it is not enough. MySQL commits implicitly
// before any DDL statement, so CREATE TABLE, DROP TABLE or DROP DATABASE end
// the read-only transaction and then run (measured against the pinned MySQL
// 8.4: each succeeded inside one). PostgreSQL has no such escape.
//
// So the statement runs as an account whose only privileges are SELECT and
// SHOW VIEW on the one database queried. MySQL refuses anything else by
// privilege, whatever the statement looks like — INSERT, DDL, SELECT … INTO
// OUTFILE (FILE), SET GLOBAL, a stored routine (EXECUTE) — and its "command
// denied" error is what the user sees. The read-only transaction is kept as
// the second guard, the one PostgreSQL's screen relies on, so a grant widened
// by hand still cannot write through the editor.
func (p cloudSQLMySQLProvider) Query(ctx context.Context, _ string, path []string, statement string) (console.Listing, error) {
	if len(path) == 0 {
		return console.Listing{}, fmt.Errorf("a database is required")
	}
	database := path[0]
	if err := systemDatabaseRefusal(database); err != nil {
		return console.Listing{}, err
	}
	if err := validMySQLPassword(p.readerPassword); err != nil {
		return console.Listing{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	if err := p.grantReader(ctx, database); err != nil {
		return console.Listing{}, err
	}
	db, err := p.open(mysqlConsoleReader, p.readerPassword, database)
	if err != nil {
		return console.Listing{}, fmt.Errorf("cannot open %s: %w", database, err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return console.Listing{}, fmt.Errorf("cannot open %s: %w", database, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "START TRANSACTION READ ONLY"); err != nil {
		return console.Listing{}, fmt.Errorf("cannot begin a read-only transaction: %w", err)
	}
	// Always rolled back: nothing here is allowed to commit.
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()

	columns, values, err := queryAll(ctx, conn, statement)
	if err != nil {
		// MySQL's own message, unchanged: it names the refusal.
		return console.Listing{}, err
	}
	out := console.Listing{Noun: "rows"}
	if len(columns) > 0 {
		out.NameColumn = columns[0]
		out.Columns = columns[1:]
	}
	for _, row := range values {
		item := console.Resource{Fields: map[string]string{}}
		for i, v := range row {
			text := formatSQLValue(v)
			if i == 0 {
				item.Name = text
				continue
			}
			item.Fields[columns[i]] = text
		}
		out.Items = append(out.Items, item)
	}
	out.Total = len(out.Items)
	out.Note = truncatedNote(out.Total, "rows")
	return out, nil
}

// grantReader makes the query account exist, with this provider's password and
// SELECT on database.
//
// Run before every query rather than once: a reset, a snapshot restore or a
// second console process can each leave the account with another password, and
// three statements as root cost less than diagnosing that.
func (p cloudSQLMySQLProvider) grantReader(ctx context.Context, database string) error {
	if err := validSQLIdentifier(database); err != nil {
		return err
	}
	db, err := p.root("")
	if err != nil {
		return fmt.Errorf("cannot reach MySQL: %w", err)
	}
	defer db.Close()
	account := mysqlAccount(mysqlConsoleReader)
	// The password is interpolated because CREATE USER takes no parameters;
	// validMySQLPassword has already held it to letters and digits.
	for _, stmt := range []string{
		"CREATE USER IF NOT EXISTS " + account + " IDENTIFIED BY '" + p.readerPassword + "'",
		"ALTER USER " + account + " IDENTIFIED BY '" + p.readerPassword + "'",
		"GRANT SELECT, SHOW VIEW ON " + quoteMySQLIdent(database) + ".* TO " + account,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("preparing the read-only account %s: %w", mysqlConsoleReader, err)
		}
	}
	return nil
}

// mysqlAccount is 'user'@'%', the host the image's own user is created with:
// connections arrive through the tunnel from whatever address the pod sees.
func mysqlAccount(user string) string {
	return "'" + strings.ReplaceAll(user, "'", "''") + "'@'%'"
}

// validMySQLPassword refuses a password that could not be written inside a
// quoted SQL literal as is.
func validMySQLPassword(pw string) error {
	if pw == "" {
		return errors.New("the query account has no password; the screen was not built by newCloudSQLMySQLProvider")
	}
	for _, r := range pw {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return errors.New("the query account's password is not alphanumeric")
		}
	}
	return nil
}

// sqlQueryer is a *sql.DB or a *sql.Conn.
type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryAll runs one statement and reads at most detailLimit rows of it.
//
// Every cell is scanned into an any, so the driver's own value comes back:
// []byte for text-protocol results, which formatSQLValue renders as MySQL
// printed it, and nil for NULL, which it renders as an em dash.
func queryAll(ctx context.Context, q sqlQueryer, statement string, args ...any) ([]string, [][]any, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]any
	for rows.Next() {
		row := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		out = append(out, row)
		if len(out) >= detailLimit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return columns, out, nil
}

// humanBytes renders a byte count the way pg_size_pretty does on the
// PostgreSQL screen, so the two Size columns read alike.
func humanBytes(n int64) string {
	const unit = 1024
	if n < 10*unit {
		return fmt.Sprintf("%d bytes", n)
	}
	value, suffix := float64(n), ""
	for _, s := range []string{"kB", "MB", "GB", "TB"} {
		value /= unit
		suffix = s
		if value < 10*unit {
			break
		}
	}
	return fmt.Sprintf("%.0f %s", value, suffix)
}

var (
	_ console.Creator  = cloudSQLMySQLProvider{}
	_ console.Deleter  = cloudSQLMySQLProvider{}
	_ console.Driller  = cloudSQLMySQLProvider{}
	_ console.Pager    = cloudSQLMySQLProvider{}
	_ console.Executor = cloudSQLMySQLProvider{}
)
