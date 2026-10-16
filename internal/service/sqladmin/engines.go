package sqladmin

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
)

const engineTimeout = 15 * time.Second

// identifier is what this API accepts as a database or user name: the
// statements below cannot take one as a parameter, so a name is checked
// before it is quoted, and quoting is the second guard.
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$-]{0,62}$`)

func checkName(kind, name string) error {
	if !identifier.MatchString(name) {
		return Errorf(http.StatusBadRequest, "Invalid request: %s name %q: CloudBurrow accepts letters, digits, _, $ and -, starting with a letter or _, at most 63 characters.", kind, name)
	}
	return nil
}

// versionName turns "17.11" into "POSTGRES_17_11".
func versionName(prefix, v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' }); i >= 0 {
		v = v[:i]
	}
	return prefix + "_" + strings.ReplaceAll(strings.Trim(v, "."), ".", "_")
}

// Postgres is the PostgreSQL server, reached as its one superuser.
type Postgres struct {
	Name     string
	Version  string // the Cloud SQL enum, POSTGRES_17
	User     string // the server's superuser
	Database string // the database it was initialised with
	HostAddr func() string
}

func (p *Postgres) Instance() string        { return p.Name }
func (p *Postgres) DatabaseVersion() string { return p.Version }
func (p *Postgres) Addr() string            { return p.HostAddr() }

func (p *Postgres) conn(ctx context.Context) (*pgx.Conn, error) {
	host, port, err := net.SplitHostPort(p.HostAddr())
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "the PostgreSQL tunnel is not up")
	}
	c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=disable", p.User, host, port, p.Database))
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "cannot reach PostgreSQL: %v", err)
	}
	return c, nil
}

func (p *Postgres) exec(ctx context.Context, stmt string, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(ctx, stmt, args...); err != nil {
		return pgError(err)
	}
	return nil
}

func pgError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "SQLSTATE 42P04"), strings.Contains(msg, "SQLSTATE 42710"):
		return Errorf(http.StatusConflict, "%s", msg)
	case strings.Contains(msg, "SQLSTATE 3D000"), strings.Contains(msg, "SQLSTATE 42704"):
		return Errorf(http.StatusNotFound, "%s", msg)
	}
	return Errorf(http.StatusBadRequest, "%s", msg)
}

func (p *Postgres) InstalledVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	var v string
	if err := c.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return "", err
	}
	return versionName("POSTGRES", v), nil
}

func (p *Postgres) ListDatabases(ctx context.Context) ([]Database, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(ctx, `SELECT datname, pg_catalog.pg_encoding_to_char(encoding), datcollate
		FROM pg_catalog.pg_database WHERE NOT datistemplate ORDER BY datname`)
	if err != nil {
		return nil, pgError(err)
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		if err := rows.Scan(&d.Name, &d.Charset, &d.Collation); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateDatabase(ctx context.Context, d Database) error {
	if err := checkName("database", d.Name); err != nil {
		return err
	}
	stmt := "CREATE DATABASE " + pgx.Identifier{d.Name}.Sanitize()
	if d.Charset != "" || d.Collation != "" {
		// A collation other than the template's needs template0, as it
		// does for Cloud SQL.
		stmt += " TEMPLATE template0"
	}
	if d.Charset != "" {
		if err := checkName("charset", d.Charset); err != nil {
			return err
		}
		stmt += " ENCODING '" + d.Charset + "'"
	}
	if d.Collation != "" {
		if !regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`).MatchString(d.Collation) {
			return Errorf(http.StatusBadRequest, "Invalid request: collation %q.", d.Collation)
		}
		stmt += " LC_COLLATE '" + d.Collation + "' LC_CTYPE '" + d.Collation + "'"
	}
	return p.exec(ctx, stmt)
}

func (p *Postgres) UpdateDatabase(ctx context.Context, d Database) error {
	// Cloud SQL for PostgreSQL documents that the character set and
	// collation are not set through the API, the console, gcloud or
	// Terraform, only with CREATE DATABASE in psql
	// (docs.cloud.google.com/sql/docs/postgres/create-manage-databases),
	// and PostgreSQL fixes both when a database is created (#1462).
	return Errorf(http.StatusBadRequest, "Invalid request: the charset and collation of PostgreSQL database %s can't be changed: "+
		"PostgreSQL sets both when a database is created, and Cloud SQL sets them only with CREATE DATABASE ... ENCODING ... "+
		"LC_COLLATE ... TEMPLATE template0 in a PostgreSQL client.", d.Name)
}

func (p *Postgres) DeleteDatabase(ctx context.Context, name string) error {
	if name == p.Database || name == "postgres" {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is a database the server needs and cannot be deleted.", name)
	}
	if err := checkName("database", name); err != nil {
		return err
	}
	return p.exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
}

func (p *Postgres) ListUsers(ctx context.Context) ([]User, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(ctx, `SELECT rolname FROM pg_catalog.pg_roles
		WHERE rolcanlogin AND rolname NOT LIKE 'pg\_%' ORDER BY rolname`)
	if err != nil {
		return nil, pgError(err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CreateUser creates a login role able to create databases and roles, as
// Cloud SQL's built-in users are (members of cloudsqlsuperuser, which this
// server does not have).
func (p *Postgres) CreateUser(ctx context.Context, u User, password string) error {
	if err := checkName("user", u.Name); err != nil {
		return err
	}
	stmt := "CREATE ROLE " + pgx.Identifier{u.Name}.Sanitize() + " LOGIN CREATEDB CREATEROLE"
	if password != "" {
		stmt += " PASSWORD " + quotePGString(password)
	}
	return p.exec(ctx, stmt)
}

func (p *Postgres) UpdateUser(ctx context.Context, u User, password string) error {
	if password == "" {
		return nil
	}
	return p.exec(ctx, "ALTER ROLE "+pgx.Identifier{u.Name}.Sanitize()+" PASSWORD "+quotePGString(password))
}

func (p *Postgres) DeleteUser(ctx context.Context, u User) error {
	if u.Name == p.User {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is the server's own superuser and cannot be deleted.", u.Name)
	}
	return p.exec(ctx, "DROP ROLE "+pgx.Identifier{u.Name}.Sanitize())
}

func quotePGString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// MySQL is the MySQL server, reached as root.
type MySQL struct {
	Name         string
	Version      string // MYSQL_8_4
	AppUser      string // the application's user, granted each new database
	Database     string
	RootPassword string
	HostAddr     func() string
}

func (m *MySQL) Instance() string        { return m.Name }
func (m *MySQL) DatabaseVersion() string { return m.Version }
func (m *MySQL) Addr() string            { return m.HostAddr() }

func (m *MySQL) db() (*sql.DB, error) {
	addr := m.HostAddr()
	if addr == "" {
		return nil, Errorf(http.StatusServiceUnavailable, "the MySQL tunnel is not up")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = "root", m.RootPassword, "tcp", addr
	cfg.Timeout = engineTimeout
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func myError(err error) error {
	var me *mysql.MySQLError
	if ok := asMySQL(err, &me); ok {
		switch me.Number {
		case 1007, 1396: // database exists; CREATE USER of an existing account
			return Errorf(http.StatusConflict, "%s", err.Error())
		case 1008:
			return Errorf(http.StatusNotFound, "%s", err.Error())
		}
		return Errorf(http.StatusBadRequest, "%s", err.Error())
	}
	return Errorf(http.StatusServiceUnavailable, "cannot reach MySQL: %v", err)
}

func asMySQL(err error, out **mysql.MySQLError) bool {
	me, ok := err.(*mysql.MySQLError)
	if ok {
		*out = me
	}
	return ok
}

func (m *MySQL) exec(ctx context.Context, stmts ...string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return myError(err)
		}
	}
	return nil
}

func (m *MySQL) InstalledVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return "", err
	}
	return versionName("MYSQL", v), nil
}

var mysqlSystem = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

func (m *MySQL) ListDatabases(ctx context.Context) ([]Database, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME
		FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME`)
	if err != nil {
		return nil, myError(err)
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		if err := rows.Scan(&d.Name, &d.Charset, &d.Collation); err != nil {
			return nil, err
		}
		if !mysqlSystem[d.Name] {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

func myIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func myString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
}

func (m *MySQL) options(d Database) (string, error) {
	var out string
	if d.Charset != "" {
		if err := checkName("charset", d.Charset); err != nil {
			return "", err
		}
		out += " CHARACTER SET " + d.Charset
	}
	if d.Collation != "" {
		if err := checkName("collation", d.Collation); err != nil {
			return "", err
		}
		out += " COLLATE " + d.Collation
	}
	return out, nil
}

// CreateDatabase creates the database and grants it to the application's
// user, as the console does: on MySQL that user holds privileges on named
// databases only.
func (m *MySQL) CreateDatabase(ctx context.Context, d Database) error {
	if err := checkName("database", d.Name); err != nil {
		return err
	}
	if mysqlSystem[d.Name] {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is a MySQL system database.", d.Name)
	}
	opts, err := m.options(d)
	if err != nil {
		return err
	}
	stmts := []string{"CREATE DATABASE " + myIdent(d.Name) + opts}
	if m.AppUser != "" {
		// A created instance (#1330) has no application user: only root.
		stmts = append(stmts, "GRANT ALL PRIVILEGES ON "+myIdent(d.Name)+".* TO "+myString(m.AppUser)+"@'%'")
	}
	return m.exec(ctx, stmts...)
}

func (m *MySQL) UpdateDatabase(ctx context.Context, d Database) error {
	opts, err := m.options(d)
	if err != nil {
		return err
	}
	return m.exec(ctx, "ALTER DATABASE "+myIdent(d.Name)+opts)
}

func (m *MySQL) DeleteDatabase(ctx context.Context, name string) error {
	if name == m.Database || mysqlSystem[name] {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is a database the server needs and cannot be deleted.", name)
	}
	return m.exec(ctx, "DROP DATABASE "+myIdent(name))
}

func (m *MySQL) ListUsers(ctx context.Context) ([]User, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT User, Host FROM mysql.user
		WHERE User NOT LIKE 'mysql.%' ORDER BY User, Host`)
	if err != nil {
		return nil, myError(err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Name, &u.Host); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func hostOr(h string) string {
	if h == "" {
		return "%"
	}
	return h
}

// CreateUser creates the account at host ('%' when none is given, as Cloud
// SQL does) with every privilege on every database, as close as one server
// comes to Cloud SQL's cloudsqlsuperuser role.
func (m *MySQL) CreateUser(ctx context.Context, u User, password string) error {
	if err := checkName("user", u.Name); err != nil {
		return err
	}
	acct := myString(u.Name) + "@" + myString(hostOr(u.Host))
	return m.exec(ctx, "CREATE USER "+acct+" IDENTIFIED BY "+myString(password),
		"GRANT ALL PRIVILEGES ON *.* TO "+acct)
}

func (m *MySQL) UpdateUser(ctx context.Context, u User, password string) error {
	return m.exec(ctx, "ALTER USER "+myString(u.Name)+"@"+myString(hostOr(u.Host))+" IDENTIFIED BY "+myString(password))
}

func (m *MySQL) DeleteUser(ctx context.Context, u User) error {
	if u.Name == "root" || u.Name == m.AppUser {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is an account CloudBurrow itself uses and cannot be deleted.", u.Name)
	}
	return m.exec(ctx, "DROP USER "+myString(u.Name)+"@"+myString(hostOr(u.Host)))
}
