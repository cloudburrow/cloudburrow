package main

// The Cloud SQL SQL editors' read-write mode (#995), opt-in and confirmed as
// Spanner Studio's (#798) and the BigQuery editor's (#994) are.
//
// Both editors read by default, exactly as before: the PostgreSQL editor runs
// the statement inside a READ ONLY transaction, and the MySQL editor as
// cloudburrow_console, an account holding SELECT and SHOW VIEW on the one
// database, inside START TRANSACTION READ ONLY (#868). Switching the editor
// to Read-write sends the statement with Mode "read-write", which reaches
// WriteReport below and nothing else; the console's page asks first, naming
// the database and showing the statement.
//
// A write runs as the application's own user, cloudburrow, the account the
// instance's PGUSER and MySQL credentials name, with that user's privileges
// and no others:
//
//   - PostgreSQL: one transaction, committed when the statement succeeds and
//     rolled back when it fails, so a failed statement changes nothing. The
//     text is sent as one simple-protocol query, so several statements
//     separated by semicolons run in that one transaction, and a statement
//     PostgreSQL will not run in a transaction block (CREATE DATABASE,
//     VACUUM) is refused with PostgreSQL's own error.
//   - MySQL: one statement, committed (autocommit, MySQL's default). MySQL
//     commits implicitly before and after DDL, so a CREATE, ALTER or DROP has
//     no transaction to roll back in MySQL itself. The read-only account is
//     not widened: the write path is a different login, the application's,
//     which holds every privilege on its own database and on the databases
//     this screen creates, and none on the others, so a write elsewhere is
//     MySQL's own "access denied".
//
// Nothing inspects the SQL to decide what it is: the database is the
// authority, as for the read-only editors, and its answer is shown in its own
// words — PostgreSQL's command tag, MySQL's rows-affected count, or the error.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// maxCloudSQLWriteBytes bounds a read-write statement. Chosen, not measured:
// room for a schema script, and under the query route's 64 KiB request body,
// so the refusal names this limit rather than the body's.
const maxCloudSQLWriteBytes = 32 << 10

// writableSQL is statement trimmed, or why it is not sent at all: nothing
// but a length check, because the database decides what it runs.
func writableSQL(statement string) (string, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", errors.New("a statement is required")
	}
	if len(statement) > maxCloudSQLWriteBytes {
		return "", fmt.Errorf("the statement is %d bytes; the read-write editor accepts at most %d",
			len(statement), maxCloudSQLWriteBytes)
	}
	return statement, nil
}

// cloudSQLWriteRun and cloudSQLWritePlaceholder are both editors' words for
// the mode's button and empty editor.
const (
	cloudSQLWriteRun         = "Run statement"
	cloudSQLWritePlaceholder = "CREATE TABLE widgets (id integer PRIMARY KEY, name varchar(40))"
)

// --- PostgreSQL ---------------------------------------------------------------

// WriteSpec implements console.StatementWriter: a database's page and its
// tables' pages, whose editor queries the same database.
func (cloudSQLProvider) WriteSpec(path []string) *console.WriteSpec {
	if len(path) < 1 || len(path) > 2 {
		return nil
	}
	return &console.WriteSpec{
		Label: "Read-write",
		Hint: "Writes data. The statement runs as " + components.CloudSQLUser + ", the application's user, in one " +
			"transaction that is committed when it succeeds and rolled back when it fails: INSERT, UPDATE and " +
			"DELETE, and DDL such as CREATE TABLE and DROP TABLE. Several statements separated by semicolons run " +
			"in that one transaction, and the answer is PostgreSQL's command tag for the last. A statement " +
			"PostgreSQL does not run inside a transaction block, such as CREATE DATABASE or VACUUM, is refused " +
			"with its own error.",
		Target:      fmt.Sprintf("database %s on the local PostgreSQL server", path[0]),
		Run:         cloudSQLWriteRun,
		Confirm:     "The statement runs as " + components.CloudSQLUser + " in a transaction, and is committed when it succeeds.",
		Placeholder: cloudSQLWritePlaceholder,
	}
}

// Write implements console.StatementWriter. The query route calls
// WriteReport, because a CREATE TABLE changes no rows; Write is the same
// call answered with PostgreSQL's row count alone.
func (p cloudSQLProvider) Write(ctx context.Context, _ string, path []string, statement string) (int64, error) {
	tag, err := p.write(ctx, path, statement)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// WriteReport implements console.WriteReporter: the statement committed,
// answered with PostgreSQL's own command tag.
func (p cloudSQLProvider) WriteReport(ctx context.Context, _ string, path []string, statement string) (string, error) {
	tag, err := p.write(ctx, path, statement)
	if err != nil {
		return "", err
	}
	return postgresWriteReport(tag), nil
}

// write runs statement in one read-write transaction on the database at
// path, as the application's user, and commits it.
func (p cloudSQLProvider) write(ctx context.Context, path []string, statement string) (pgconn.CommandTag, error) {
	if len(path) == 0 {
		return pgconn.CommandTag{}, errors.New("a database is required")
	}
	statement, err := writableSQL(statement)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	database := path[0]

	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	conn, err := p.connect(ctx, database)
	if err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("cannot open %s: %w", database, err)
	}
	defer conn.Close(context.Background())

	tx, err := conn.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("cannot begin a transaction: %w", err)
	}
	// A no-op once committed; on any failure, what the statement did is undone.
	defer func() { _ = tx.Rollback(context.Background()) }()

	// No arguments, so pgx sends the text as one simple-protocol query: several
	// statements run in order in this transaction, and the tag is the last's.
	tag, err := tx.Exec(ctx, statement)
	if err != nil {
		// PostgreSQL's own message: a constraint violation names the
		// constraint, a syntax error the position.
		return pgconn.CommandTag{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("committing the transaction: %w", err)
	}
	return tag, nil
}

// postgresWriteReport says what a committed statement did, in PostgreSQL's
// words: its command tag, and the rows it affected where the tag counts them.
func postgresWriteReport(tag pgconn.CommandTag) string {
	text := tag.String()
	switch {
	case text == "":
		return "Committed."
	case tag.Insert(), tag.Update(), tag.Delete(),
		strings.HasPrefix(text, "MERGE "), strings.HasPrefix(text, "COPY "):
		return fmt.Sprintf("Committed. PostgreSQL answered %s: %s affected.", text, rowsNoun(tag.RowsAffected()))
	case tag.Select():
		return fmt.Sprintf("Committed. PostgreSQL answered %s; Read-only shows a query's rows, so run it there.", text)
	}
	return fmt.Sprintf("Committed. PostgreSQL answered %s.", text)
}

func rowsNoun(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
}

// --- MySQL --------------------------------------------------------------------

// WriteSpec implements console.StatementWriter: a database's page and its
// tables' pages. MySQL's own system databases have no editor.
func (cloudSQLMySQLProvider) WriteSpec(path []string) *console.WriteSpec {
	if len(path) < 1 || len(path) > 2 || systemDatabaseRefusal(path[0]) != nil {
		return nil
	}
	return &console.WriteSpec{
		Label: "Read-write",
		Hint: "Writes data. One statement at a time runs as " + components.CloudSQLUser + ", the application's " +
			"user, with its privileges — every one on its own database and on databases created here, none on " +
			"the rest — and is committed: INSERT, UPDATE and DELETE, and DDL such as CREATE TABLE and DROP " +
			"TABLE. The answer is MySQL's rows-affected count, or MySQL's own error. The read-only account, " +
			mysqlConsoleReader + ", is not used and is not widened.",
		Target: fmt.Sprintf("database %s on the local MySQL server", path[0]),
		Run:    cloudSQLWriteRun,
		Confirm: "The statement runs as " + components.CloudSQLUser + " and is committed when it succeeds. " +
			"MySQL commits a schema change such as CREATE, ALTER or DROP as it runs, so there is nothing to roll back.",
		Placeholder: cloudSQLWritePlaceholder,
	}
}

// Write implements console.StatementWriter: the statement committed, and the
// rows MySQL says it affected. The query route calls WriteReport.
func (p cloudSQLMySQLProvider) Write(ctx context.Context, _ string, path []string, statement string) (int64, error) {
	return p.write(ctx, path, statement)
}

// WriteReport implements console.WriteReporter.
func (p cloudSQLMySQLProvider) WriteReport(ctx context.Context, _ string, path []string, statement string) (string, error) {
	n, err := p.write(ctx, path, statement)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Committed as %s. MySQL answered: %s affected.", components.CloudSQLUser, rowsNoun(n)), nil
}

// write runs one statement as the application's user on the database at
// path, with MySQL's default autocommit, which commits it.
func (p cloudSQLMySQLProvider) write(ctx context.Context, path []string, statement string) (int64, error) {
	if len(path) == 0 {
		return 0, errors.New("a database is required")
	}
	database := path[0]
	if err := systemDatabaseRefusal(database); err != nil {
		return 0, err
	}
	statement, err := writableSQL(statement)
	if err != nil {
		return 0, err
	}
	// A statement's own terminator is not part of it, and the driver sends
	// one statement: MySQL refuses a second as a syntax error, in its words.
	statement = strings.TrimRight(statement, "; \t\r\n")
	if p.creds.Password == "" {
		return 0, fmt.Errorf("the console does not have %s's password, so it cannot write as that user", components.CloudSQLUser)
	}

	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	db, err := p.open(components.CloudSQLUser, p.creds.Password, database)
	if err != nil {
		return 0, fmt.Errorf("cannot open %s: %w", database, err)
	}
	defer db.Close()
	res, err := db.ExecContext(ctx, statement)
	if err != nil {
		// MySQL's own message: a duplicate key names the key, a missing
		// privilege the command and the user.
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("the statement ran, but its rows-affected count could not be read: %w", err)
	}
	return n, nil
}

var (
	_ console.WriteReporter = cloudSQLProvider{}
	_ console.WriteReporter = cloudSQLMySQLProvider{}
)
