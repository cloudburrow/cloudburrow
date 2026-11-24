package sqladmin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
	// Logical is a replica of a PostgreSQL external primary (#1524),
	// which replicates by subscriptions, not as a standby.
	Logical bool
	// SignInLog, when set, streams the server's log, whose failed
	// sign-ins lock a role with a lock after failed attempts (#2014);
	// LogKey names the server it reads, Name when "".
	SignInLog SignInLog
	LogKey    string
	// PoolerHostAddr, when set, is the host address of the instance's
	// managed connection pooler, "" while it has none (#2141).
	PoolerHostAddr func() string
	// AdminUser and AdminPassword are CloudBurrow's management role
	// (#2208, components.PGAdminUser), which the Admin API signs in as
	// and then acts as User; UserPassword is User's own, used when there
	// is no management role (the console's screen of `up`'s server).
	// Every TCP sign-in is checked by password.
	AdminUser, AdminPassword string
	UserPassword             string
}

// PoolerAddr is the address of the instance's managed connection pooler,
// which the connectors reach on 3307 when it pools connections, or "".
func (p *Postgres) PoolerAddr() string {
	if p.PoolerHostAddr == nil {
		return ""
	}
	return p.PoolerHostAddr()
}

func (p *Postgres) Instance() string        { return p.Name }
func (p *Postgres) DatabaseVersion() string { return p.Version }
func (p *Postgres) Addr() string            { return p.HostAddr() }

func (p *Postgres) conn(ctx context.Context) (*pgx.Conn, error) {
	host, port, err := net.SplitHostPort(p.HostAddr())
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "the PostgreSQL tunnel is not up")
	}
	user, password := p.User, p.UserPassword
	if p.AdminUser != "" && p.AdminPassword != "" {
		user, password = p.AdminUser, p.AdminPassword
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(host, port),
		Path: "/" + p.Database, RawQuery: "sslmode=disable"}
	c, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "cannot reach PostgreSQL: %v", err)
	}
	if user != p.User {
		// The management role acts as the server's superuser, so what it
		// makes is owned as before (#2208).
		if _, err := c.Exec(ctx, "SET SESSION AUTHORIZATION "+pgx.Identifier{p.User}.Sanitize()); err != nil {
			_ = c.Close(context.Background())
			return nil, Errorf(http.StatusServiceUnavailable, "cannot act as %s on PostgreSQL: %v", p.User, err)
		}
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

// queryText is the one value q answers, as text.
func (p *Postgres) queryText(ctx context.Context, q string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	var v string
	if err := c.QueryRow(ctx, q).Scan(&v); err != nil {
		return "", pgError(err)
	}
	return v, nil
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
	// An IAM user is a member of cloudsqliamuser or
	// cloudsqliamserviceaccount (#1944), as on Cloud SQL. A user its
	// failed sign-ins locked (#2014) has lost its login, and is a user
	// still.
	rows, err := c.Query(ctx, `SELECT r.rolname, COALESCE((SELECT g.rolname FROM pg_catalog.pg_auth_members m
		JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
		WHERE m.member = r.oid AND g.rolname IN ('`+pgIAMUserGroup+`', '`+pgIAMServiceAccountGroup+`') LIMIT 1), '')
		FROM pg_catalog.pg_roles r
		WHERE (r.rolcanlogin OR EXISTS (SELECT 1 FROM pg_catalog.pg_db_role_setting s, unnest(s.setconfig) x
			WHERE s.setrole = r.oid AND s.setdatabase = 0 AND x = '`+pgFailedCheckSetting+`=on'))
		AND r.rolname NOT LIKE 'pg\_%' AND r.rolname NOT IN ('`+pgAdminUser+`', '`+pgAlloyDBAdminUser+`') ORDER BY r.rolname`)
	if err != nil {
		return nil, pgError(err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var group string
		if err := rows.Scan(&u.Name, &group); err != nil {
			return nil, err
		}
		switch group {
		case pgIAMUserGroup:
			u.Type, u.IAMEmail = TypeIAMUser, u.Name
		case pgIAMServiceAccountGroup:
			u.Type, u.IAMEmail = TypeIAMServiceAccount, u.Name+saSuffix
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CloudBurrow's management roles (#2208, components.PGAdminUser and
// AlloyDBAdminUser), which users.list leaves out, as Google's are.
const (
	pgAdminUser        = "cloudsqladmin"
	pgAlloyDBAdminUser = "alloydbadmin"
)

// The roles Cloud SQL for PostgreSQL makes its IAM users members of.
const (
	pgIAMUserGroup           = "cloudsqliamuser"
	pgIAMServiceAccountGroup = "cloudsqliamserviceaccount"
)

// pgIAMHBA are the pg_hba.conf lines that hold the IAM roles to their
// password, put ahead of the server's trust line (#1944).
var pgIAMHBA = []string{
	"host all +" + pgIAMUserGroup + " all md5",
	"host all +" + pgIAMServiceAccountGroup + " all md5",
}

// CreateIAMUser creates an IAM user's login role (#1944): no privileges,
// a member of the IAM group of its type, signing in with password only,
// which pg_hba.conf requires of it.
func (p *Postgres) CreateIAMUser(ctx context.Context, u User, email, password string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	group := pgIAMUserGroup
	if u.Type == TypeIAMServiceAccount {
		group = pgIAMServiceAccountGroup
	}
	for _, g := range []string{pgIAMUserGroup, pgIAMServiceAccountGroup} {
		var exists bool
		if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", g).Scan(&exists); err != nil {
			return pgError(err)
		}
		if !exists {
			if _, err := c.Exec(ctx, "CREATE ROLE "+g+" NOLOGIN"); err != nil {
				return pgError(err)
			}
		}
	}
	if err := pgEnsureIAMHBA(ctx, c); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "CREATE ROLE "+pgx.Identifier{u.Name}.Sanitize()+" LOGIN PASSWORD "+quotePGString(password)+
		" IN ROLE "+group); err != nil {
		return pgError(err)
	}
	return nil
}

// pgEnsureIAMHBA puts pgIAMHBA at the top of pg_hba.conf, once, and
// reloads it.
func pgEnsureIAMHBA(ctx context.Context, c *pgx.Conn) error {
	return pgEnsureHBA(ctx, c, pgIAMHBA...)
}

// pgEnsureHBA puts lines at the top of pg_hba.conf, once, and reloads it.
func pgEnsureHBA(ctx context.Context, c *pgx.Conn, lines ...string) error {
	var hbaFile, hba string
	if err := c.QueryRow(ctx, "SHOW hba_file").Scan(&hbaFile); err != nil {
		return pgError(err)
	}
	if err := c.QueryRow(ctx, "SELECT pg_read_file($1)", hbaFile).Scan(&hba); err != nil {
		return pgError(err)
	}
	if strings.Contains(hba, lines[0]) {
		return nil
	}
	// The first matching line wins, so the lines go first: the file is
	// rewritten with them ahead of what it holds.
	next := strings.Join(lines, "\n") + "\n" + hba
	if _, err := c.Exec(ctx, "COPY (SELECT "+quotePGString(base64Std(next))+") TO PROGRAM "+
		quotePGString("base64 -d > "+hbaFile)); err != nil {
		return pgError(err)
	}
	if _, err := c.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return pgError(err)
	}
	return nil
}

// CreateUser creates a login role able to create databases and roles, as
// Cloud SQL's built-in users are (members of cloudsqlsuperuser, which this
// server does not have).
// pgSystem refuses a write to CloudBurrow's management role (#2208), as
// Cloud SQL refuses one to cloudsqladmin.
func pgSystem(u User) error {
	if u.Name == pgAdminUser || u.Name == pgAlloyDBAdminUser {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is a system user of the instance, which cannot be created, modified or deleted.", u.Name)
	}
	return nil
}

func (p *Postgres) CreateUser(ctx context.Context, u User, password string) error {
	if err := checkName("user", u.Name); err != nil {
		return err
	}
	if err := pgSystem(u); err != nil {
		return err
	}
	stmt := "CREATE ROLE " + pgx.Identifier{u.Name}.Sanitize() + " LOGIN CREATEDB CREATEROLE"
	if password != "" {
		stmt += " PASSWORD " + quotePGString(password)
	}
	return p.exec(ctx, stmt)
}

func (p *Postgres) UpdateUser(ctx context.Context, u User, password string) error {
	if err := pgSystem(u); err != nil {
		return err
	}
	if password == "" {
		return nil
	}
	return p.exec(ctx, "ALTER ROLE "+pgx.Identifier{u.Name}.Sanitize()+" PASSWORD "+quotePGString(password))
}

func (p *Postgres) DeleteUser(ctx context.Context, u User) error {
	if err := pgSystem(u); err != nil {
		return err
	}
	if u.Name == p.User {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is the server's own superuser and cannot be deleted.", u.Name)
	}
	return p.exec(ctx, "DROP ROLE "+pgx.Identifier{u.Name}.Sanitize())
}

func quotePGString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// MySQL is the MySQL server, reached as its administrative account:
// MySQLAdminUser on a created instance (#1993), whose root the user may
// delete, as Cloud SQL's 'root'@'%' can be; root on `up`'s server.
type MySQL struct {
	Name     string
	Version  string // MYSQL_8_4
	AppUser  string // the application's user, granted each new database
	Database string
	// RootPassword is the password the instance was created with: root's
	// then, and its system accounts' for good (MySQLSystemAccountsSQL).
	RootPassword string
	// Admin is the account CloudBurrow signs in as; "" is root.
	Admin    string
	HostAddr func() string
}

// MySQLAdminUser is CloudBurrow's own account on a created MySQL
// instance (#1993), as Cloud SQL manages its instances through system
// accounts of its own (root@localhost, root@127.0.0.1 and root@::1 in
// docs.cloud.google.com/sql/docs/mysql/users), not through 'root'@'%',
// which the user may delete and the hashicorp/google provider does delete.
// It is a name of its own, not root at a loopback host: MySQL picks the
// most specific host first, so a root@127.0.0.1 would shadow 'root'@'%'
// for every client the tunnel (a kubectl port-forward, from the pod's
// loopback) brings, and refuse root's new password after users.update.
// It exists at the loopback hosts only, so no other pod reaches it, and
// users.list hides it, as gcloud cannot display Cloud SQL's system users.
const MySQLAdminUser = "cloudsqladmin"

// mysqlAdminHosts are MySQLAdminUser's hosts: a TCP sign-in from the
// pod's loopback is 127.0.0.1 or ::1 (the image skips name resolution),
// and localhost is the unix socket.
var mysqlAdminHosts = []string{"localhost", "127.0.0.1", "::1"}

// MySQLSystemAccountsSQL creates or refreshes MySQLAdminUser with every
// privilege, signing in with the instance's creation password, with the
// binary log off: each server has its own, as a read replica's or a
// standby's mysql schema is never copied from its primary's binary log,
// and a logged CREATE USER would stop a replica that has one already.
func MySQLSystemAccountsSQL(version, password string) string {
	pw := myString(password)
	var sb strings.Builder
	sb.WriteString("SET SESSION sql_log_bin = 0;\n")
	for _, h := range mysqlAdminHosts {
		acct := myString(MySQLAdminUser) + "@" + myString(h)
		if version == "MYSQL_5_6" {
			// 5.6 has no CREATE USER IF NOT EXISTS; its GRANT creates.
			fmt.Fprintf(&sb, "GRANT ALL PRIVILEGES ON *.* TO %s IDENTIFIED BY %s WITH GRANT OPTION;\n", acct, pw)
			continue
		}
		fmt.Fprintf(&sb, "CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s;\nALTER USER %s IDENTIFIED BY %s;\nGRANT ALL PRIVILEGES ON *.* TO %s WITH GRANT OPTION;\n",
			acct, pw, acct, pw, acct)
	}
	return sb.String()
}

// mysqlHidden is whether users.list leaves the account out: MySQL's own
// (mysql.*), CloudBurrow's, the replication users (cloudsqlreplica, a
// system user in Cloud SQL's list, and the standby's), and root at a
// loopback host, which Cloud SQL lists as system users "used to provide
// the managed database service".
func mysqlHidden(name, host string) bool {
	if strings.HasPrefix(name, "mysql.") || name == MySQLAdminUser || name == "cloudsqlreplica" || name == "cloudsqlstandby" {
		return true
	}
	return name == "root" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func (m *MySQL) admin() string {
	if m.Admin == "" {
		return "root"
	}
	return m.Admin
}

func (m *MySQL) Instance() string        { return m.Name }
func (m *MySQL) DatabaseVersion() string { return m.Version }
func (m *MySQL) Addr() string            { return m.HostAddr() }

func (m *MySQL) db() (*sql.DB, error) {
	addr := m.HostAddr()
	if addr == "" {
		return nil, Errorf(http.StatusServiceUnavailable, "the MySQL tunnel is not up")
	}
	db, err := m.open(addr, m.admin())
	if err != nil || m.admin() == "root" {
		return db, err
	}
	// An instance made before #1993 has no MySQLAdminUser until its next
	// start makes one (sqlinstances.go); until then, root, which it could
	// not delete, still signs in.
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	perr := db.PingContext(ctx)
	var me *mysql.MySQLError
	if perr == nil || !asMySQL(perr, &me) || me.Number != 1045 {
		return db, nil
	}
	if rdb, rerr := m.open(addr, "root"); rerr == nil && rdb.PingContext(ctx) == nil {
		db.Close()
		return rdb, nil
	} else if rerr == nil {
		rdb.Close()
	}
	return db, nil
}

func (m *MySQL) open(addr, user string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = user, m.RootPassword, "tcp", addr
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
		if _, err := db.ExecContext(ctx, MySQLDialect(m.Version, s)); err != nil {
			return myError(err)
		}
	}
	return nil
}

// execUnlogged runs statements, in order, on one connection with
// sql_log_bin off: Cloud SQL's own housekeeping, which neither writes to
// the binary log nor gives the server a GTID, so neither a replica nor a
// GTID consistency check sees it.
func (m *MySQL) execUnlogged(ctx context.Context, stmts ...string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return myError(err)
	}
	defer conn.Close()
	for _, s := range append([]string{"SET SESSION sql_log_bin = 0"}, stmts...) {
		if _, err := conn.ExecContext(ctx, MySQLDialect(m.Version, s)); err != nil {
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
	iam := map[string][2]string{}
	if irows, err := db.QueryContext(ctx, "SELECT user, type, email FROM "+myIAMTable); err == nil {
		for irows.Next() {
			var n, t, e string
			if irows.Scan(&n, &t, &e) == nil {
				iam[n] = [2]string{t, e}
			}
		}
		irows.Close()
	}
	rows, err := db.QueryContext(ctx, `SELECT User, Host FROM mysql.user ORDER BY User, Host`)
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
		if mysqlHidden(u.Name, u.Host) {
			continue
		}
		if t, ok := iam[u.Name]; ok && u.Host == "%" {
			u.Type, u.IAMEmail = t[0], t[1]
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// myIAMTable is where a MySQL server keeps which of its accounts are IAM
// users (#1944), and their emails, which the account name drops. It is in
// the mysql schema, which no database listing shows, and replicates with
// the accounts.
const myIAMTable = "mysql.cloudburrow_iam_users"

// CreateIAMUser creates an IAM user's account (#1944): no privileges,
// signing in with password only, recorded in myIAMTable.
func (m *MySQL) CreateIAMUser(ctx context.Context, u User, email, password string) error {
	acct := myString(u.Name) + "@'%'"
	ident := " IDENTIFIED BY " + myString(password)
	return m.exec(ctx,
		"CREATE TABLE IF NOT EXISTS "+myIAMTable+" (user VARCHAR(32) NOT NULL PRIMARY KEY, type VARCHAR(40) NOT NULL, email VARCHAR(255) NOT NULL) ENGINE=InnoDB",
		"CREATE USER "+acct+ident,
		"REPLACE INTO "+myIAMTable+" (user, type, email) VALUES ("+myString(u.Name)+", "+myString(u.Type)+", "+myString(email)+")")
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
	if err := m.system(u); err != nil {
		return err
	}
	acct := myString(u.Name) + "@" + myString(hostOr(u.Host))
	return m.exec(ctx, "CREATE USER "+acct+" IDENTIFIED BY "+myString(password),
		"GRANT ALL PRIVILEGES ON *.* TO "+acct)
}

// system refuses a write to a system account: Cloud SQL's "You can't
// delete, modify, or assign database roles to these users" (users page).
// root is one only on `up`'s server, which CloudBurrow reaches as root.
func (m *MySQL) system(u User) error {
	if mysqlHidden(u.Name, hostOr(u.Host)) || (u.Name == "root" && m.admin() == "root") {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is a system user of the instance, which cannot be created, modified or deleted.", u.Name)
	}
	return nil
}

func (m *MySQL) UpdateUser(ctx context.Context, u User, password string) error {
	if mysqlHidden(u.Name, hostOr(u.Host)) {
		return m.system(u)
	}
	acct := myString(u.Name) + "@" + myString(hostOr(u.Host))
	if m.Version == "MYSQL_5_6" {
		// MySQL 5.6's ALTER USER only expires a password (#1524).
		return m.exec(ctx, "SET PASSWORD FOR "+acct+" = PASSWORD("+myString(password)+")")
	}
	return m.exec(ctx, "ALTER USER "+acct+" IDENTIFIED BY "+myString(password))
}

func (m *MySQL) DeleteUser(ctx context.Context, u User) error {
	if u.Name == m.AppUser && m.AppUser != "" {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is an account CloudBurrow itself uses and cannot be deleted.", u.Name)
	}
	if err := m.system(u); err != nil {
		return err
	}
	stmts := []string{"DROP USER " + myString(u.Name) + "@" + myString(hostOr(u.Host))}
	if u.Type != "" && u.Type != TypeBuiltIn {
		stmts = append(stmts, "DELETE FROM "+myIAMTable+" WHERE user = "+myString(u.Name))
	}
	return m.exec(ctx, stmts...)
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
