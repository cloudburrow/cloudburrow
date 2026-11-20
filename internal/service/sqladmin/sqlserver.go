package sqladmin

// Cloud SQL for SQL Server (#1916): an instance of a SQLSERVER_* version
// runs Microsoft's SQL Server container image
// (mcr.microsoft.com/mssql/server), which starts only with ACCEPT_EULA set,
// "to confirm your acceptance of the End-User Licensing Agreement"
// (learn.microsoft.com/sql/linux/configure/environment-variables).
//
// The user's decision (2026-11-19): the user who provisions SQL Server
// accepts Microsoft's licence; nobody accepts it on their behalf. So
// CloudBurrow never sets ACCEPT_EULA by default. An instance is created only
// when the user has accepted, either for every instance (`up
// --accept-mssql-eula`, or CLOUDBURROW_ACCEPT_MSSQL_EULA=Y) or for this one
// (the console's Create instance checkbox, which sends EULAHeader). Who
// accepted, when and how is recorded in the instance's record (Spec.MSSQLEULA)
// and shown by `cloudburrow status`; the server is given ACCEPT_EULA=Y only
// for an instance that has that record. Without it, instances.insert is
// refused with FAILED_PRECONDITION saying how to accept.
//
// The image is linux/amd64 only, and Microsoft supports it only on x86-64
// hosts ("Emulation or translation environments (for example, Rosetta 2,
// Prism, or QEMU) aren't tested or supported", quickstart-install-docker):
// as for MySQL 5.7 and 5.6, an instance is served where the cluster's node
// runs linux/amd64 binaries, natively or through binfmt_misc emulation
// (LegacyRunner), and is 501 naming why elsewhere.
//
// What a SQL Server instance serves: instances.insert, get, list, patch of
// its database flags (Google's SQL Server flags list), restart, stop, start
// and delete; the sqlserver user with its rootPassword; databases; users;
// flags.list; and connecting through the Cloud SQL connector. Read
// replicas, high availability, backups and restores, point-in-time
// recovery, clones, imports and exports, major version upgrades and the
// external syncs are 501: CloudBurrow's steps for them are pg_dump,
// mysqldump and the PostgreSQL and MySQL replication statements.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// The links the opt-in names: Microsoft's End-User Licensing Agreement for
// the SQL Server container image, as ACCEPT_EULA's description links it
// (learn.microsoft.com/sql/linux/configure/environment-variables), and the
// licensing terms of each edition.
const (
	MSSQLEULAURL     = "https://go.microsoft.com/fwlink/?LinkId=746388"
	MSSQLLicensesURL = "https://www.microsoft.com/licensing/terms/productoffering/SQLServer"
)

// EULAHeader is the request header that accepts Microsoft's licence for
// the one instance an instances.insert creates: the console's Create
// instance checkbox sends it (EULAHeader: Y). It is CloudBurrow's own; Cloud
// SQL has no such field, as Google holds the licence there.
const EULAHeader = "X-CloudBurrow-Accept-MSSQL-EULA"

// EULAAcceptance is who accepted Microsoft's SQL Server licence, when, and
// how: recorded on each SQL Server instance.
type EULAAcceptance struct {
	AcceptedBy string `json:"acceptedBy"`
	AcceptedAt string `json:"acceptedAt"`
	Via        string `json:"via"`
}

// The ways the licence is accepted, as recorded.
const (
	EULAViaFlag    = "cloudburrow up --accept-mssql-eula"
	EULAViaEnv     = "CLOUDBURROW_ACCEPT_MSSQL_EULA=Y"
	EULAViaFile    = "acceptMssqlEula in the configuration file"
	EULAViaConsole = "the console's Create instance checkbox (request header " + EULAHeader + ": Y)"
)

// LocalUser is who runs this process, the person an acceptance is recorded
// as made by.
func LocalUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// AcceptMSSQLEULA records the acceptance of Microsoft's licence for every
// SQL Server instance this server creates: `up --accept-mssql-eula` or its
// environment variable. nil (the default) accepts nothing.
func (s *Server) AcceptMSSQLEULA(a *EULAAcceptance) {
	s.emu.Lock()
	defer s.emu.Unlock()
	s.mssqlEULA = a
}

// sqlServerVersions are Cloud SQL's SQL Server versions, as the Admin API's
// databaseVersion enum lists them (sqladmin v1, re-read 2026-11-19): 2017,
// 2019 and 2022 in EXPRESS, WEB, STANDARD and ENTERPRISE, 2025 in EXPRESS,
// STANDARD and ENTERPRISE (Google lists no SQLSERVER_2025_WEB, and SQL
// Server 2025 has no Web edition).
var sqlServerVersions = []string{
	"SQLSERVER_2017_EXPRESS", "SQLSERVER_2017_WEB", "SQLSERVER_2017_STANDARD", "SQLSERVER_2017_ENTERPRISE",
	"SQLSERVER_2019_EXPRESS", "SQLSERVER_2019_WEB", "SQLSERVER_2019_STANDARD", "SQLSERVER_2019_ENTERPRISE",
	"SQLSERVER_2022_EXPRESS", "SQLSERVER_2022_WEB", "SQLSERVER_2022_STANDARD", "SQLSERVER_2022_ENTERPRISE",
	"SQLSERVER_2025_EXPRESS", "SQLSERVER_2025_STANDARD", "SQLSERVER_2025_ENTERPRISE",
}

// SQLServerVersions are the SQL Server versions an instance can have.
func SQLServerVersions() []string { return append([]string(nil), sqlServerVersions...) }

// IsSQLServer is whether version is one of Cloud SQL's SQL Server versions.
func IsSQLServer(version string) bool {
	for _, v := range sqlServerVersions {
		if v == version {
			return true
		}
	}
	return false
}

// SQLServerYear is version's release, "2022" for SQLSERVER_2022_STANDARD.
func SQLServerYear(version string) string {
	if p := strings.Split(version, "_"); len(p) == 3 {
		return p[1]
	}
	return ""
}

// SQLServerEdition is version's Cloud SQL edition, "STANDARD" for
// SQLSERVER_2022_STANDARD.
func SQLServerEdition(version string) string {
	if p := strings.Split(version, "_"); len(p) == 3 {
		return p[2]
	}
	return ""
}

// MSSQLPID is the SQL Server edition (MSSQL_PID) the local server of
// version runs, and why. Cloud SQL's Standard, Enterprise and Web editions
// are licensed through Google there; locally nobody holds those licences,
// so they run Microsoft's free Developer edition, which has every feature
// of Enterprise and is "licensed for non-production use"
// (environment-variables: "MSSQL_PID specifies the freely licensed
// Developer Edition of SQL Server for non-production use"), which a local
// emulator is. SQL Server 2025 splits Developer into Standard Developer
// and Enterprise Developer (its MSSQL_PID table), so its STANDARD runs the
// first and its ENTERPRISE the second. Express is free for any use and
// runs as itself.
func MSSQLPID(version string) (pid, why string) {
	ed, year := SQLServerEdition(version), SQLServerYear(version)
	switch {
	case ed == "EXPRESS":
		return "Express", "Express edition is free to use, so it runs as itself"
	case year == "2025" && ed == "STANDARD":
		return "StandardDeveloper", "Standard Developer edition: Standard's features, licensed for development and test only"
	case year == "2025":
		return "EnterpriseDeveloper", "Enterprise Developer edition: Enterprise's features, licensed for development and test only"
	case ed == "ENTERPRISE":
		return "Developer", "Developer edition: Enterprise's features, licensed for development and test only"
	}
	return "Developer", "Developer edition, licensed for development and test only: it has Enterprise's features, " +
		"so a " + strings.ToLower(ed) + " instance here allows features its edition lacks on Cloud SQL"
}

// eulaRefusal is the FAILED_PRECONDITION an instances.insert of a SQL
// Server version gets when no one has accepted Microsoft's licence.
// UNVERIFIED (re-read 2026-11-19, docs.cloud.google.com/sql/docs/sqlserver/create-instance: Google documents no such refusal, since Google holds the
// licence on Cloud SQL): the code is the one this API answers for a
// missing precondition of the local server (engineServed), and the wording
// is CloudBurrow's.
func eulaRefusal(version string) error {
	return Errorf(http.StatusPreconditionFailed, "Invalid request: a %s instance runs Microsoft's SQL Server container image, "+
		"which starts only once Microsoft's End-User Licensing Agreement (%s, and the %s edition's terms, %s) is accepted, "+
		"and CloudBurrow never accepts it on your behalf. To accept it yourself, start CloudBurrow with "+
		"`cloudburrow up --accept-mssql-eula` (or CLOUDBURROW_ACCEPT_MSSQL_EULA=Y), or check \"I accept Microsoft's licence terms\" "+
		"on the console's Create instance page; who accepted and when is recorded on the instance.",
		version, MSSQLEULAURL, SQLServerEdition(version), MSSQLLicensesURL)
}

// eulaFor is the acceptance a new SQL Server instance of r is created
// under: the request's own (EULAHeader: Y), else the one `up` was given;
// or eulaRefusal.
func (s *Server) eulaFor(r *http.Request, version string) (*EULAAcceptance, error) {
	if v := strings.TrimSpace(r.Header.Get(EULAHeader)); v != "" {
		if v != "Y" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: %s must be Y, which accepts Microsoft's licence for this instance.", EULAHeader)
		}
		return &EULAAcceptance{AcceptedBy: LocalUser(), AcceptedAt: time.Now().UTC().Format(time.RFC3339), Via: EULAViaConsole}, nil
	}
	s.emu.RLock()
	a := s.mssqlEULA
	s.emu.RUnlock()
	if a == nil {
		return nil, eulaRefusal(version)
	}
	cp := *a
	return &cp, nil
}

// sqlServerServed is whether a SQL Server instance can run here, and the
// 501's reason when it cannot: its image is linux/amd64 only.
func (s *Server) sqlServerServed(version string) (bool, string) {
	if s.writes == nil {
		return false, instanceWhy
	}
	lr, ok := s.writes.prov.(LegacyRunner)
	if !ok {
		return false, sqlServerVersionWhy
	}
	if why := lr.LegacyUnrunnable(version); why != "" {
		return false, why
	}
	return true, ""
}

// sqlServerWhy is why the steps limitedRefusal refuses are not served on
// a SQL Server instance.
const sqlServerWhy = "CloudBurrow's backup, restore, recovery, replication, standby, import, export and upgrade steps are " +
	"pg_dump, mysqldump and PostgreSQL's and MySQL's statements, and none is built yet for SQL Server's BACKUP DATABASE " +
	"and its replication; a SQL Server instance here serves its server, the sqlserver user, databases, users, flags and connections"

// sqlServerUnimplemented is the 501 for what, on a SQL Server instance.
func sqlServerUnimplemented(what string) error {
	return unimplemented(what+" on a SQL Server instance", sqlServerWhy)
}

// limitedUnimplemented is the 501 for what on an instance of version that
// serves only its server, databases, users, flags and connections: MySQL
// 5.7 and 5.6, and SQL Server. nil for any other version.
func limitedUnimplemented(version, what string) error {
	switch {
	case legacyMySQL(version):
		return legacyUnimplemented(what)
	case IsSQLServer(version):
		return sqlServerUnimplemented(what)
	}
	return nil
}

// sqlServerEngine is the 501 for what on e when it is a SQL Server
// instance's server, or nil.
func sqlServerEngine(e Engine, what string) error {
	if IsSQLServer(e.DatabaseVersion()) {
		return sqlServerUnimplemented(what)
	}
	return nil
}

// sqlServerRefusal is the 501 for the settings of spec a SQL Server
// instance cannot have here, or nil.
func sqlServerRefusal(spec Spec) error {
	if !IsSQLServer(spec.DatabaseVersion) {
		return nil
	}
	switch {
	case spec.MasterInstanceName != "":
		return sqlServerUnimplemented("a read replica")
	case spec.Regional():
		return sqlServerUnimplemented("availabilityType REGIONAL")
	case spec.PITREnabled():
		return sqlServerUnimplemented("point-in-time recovery")
	case spec.BackupEnabled:
		return sqlServerUnimplemented("automated backups (backupConfiguration.enabled)")
	case spec.External != nil:
		return sqlServerUnimplemented("an external primary's replica")
	case spec.KMSKey != "":
		return sqlServerUnimplemented("a customer-managed encryption key")
	}
	return checkSQLServerFlags(spec.Flags)
}

// NewSAPassword is a password for a SQL Server instance's sa login, which
// only CloudBurrow uses (the instance's sqlserver user is the customer's,
// as on Cloud SQL): long, and with the four character classes SQL Server's
// password policy asks three of.
func NewSAPassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return "Cb9!" + hex.EncodeToString(b) + "xZ"
}

// SQLServer is a SQL Server instance's server, reached as sa.
type SQLServer struct {
	Name       string
	Version    string
	SAPassword string
	HostAddr   func() string
}

func (m *SQLServer) Instance() string        { return m.Name }
func (m *SQLServer) DatabaseVersion() string { return m.Version }
func (m *SQLServer) Addr() string            { return m.HostAddr() }

func (m *SQLServer) db() (*sql.DB, error) {
	host, port, err := net.SplitHostPort(m.HostAddr())
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "the SQL Server tunnel is not up")
	}
	// Each image has its own self-signed certificate; the tunnel is on
	// loopback.
	q := url.Values{"database": {"master"}, "encrypt": {"true"}, "TrustServerCertificate": {"true"}, "dial timeout": {"5"}}
	u := &url.URL{Scheme: "sqlserver", User: url.UserPassword("sa", m.SAPassword), Host: net.JoinHostPort(host, port), RawQuery: q.Encode()}
	c, err := mssql.NewConnector(u.String())
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

func msError(err error) error {
	var me mssql.Error
	if ok := asMSSQL(err, &me); ok {
		switch me.Number {
		case 1801, 15025, 15023: // database, login or user exists
			return Errorf(http.StatusConflict, "%s", me.Message)
		case 3701, 15151, 911: // does not exist
			return Errorf(http.StatusNotFound, "%s", me.Message)
		}
		return Errorf(http.StatusBadRequest, "%s", me.Message)
	}
	return Errorf(http.StatusServiceUnavailable, "cannot reach SQL Server: %v", err)
}

func asMSSQL(err error, out *mssql.Error) bool {
	for e := err; e != nil; {
		if me, ok := e.(mssql.Error); ok {
			*out = me
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func (m *SQLServer) exec(ctx context.Context, stmts ...string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return msError(err)
		}
	}
	return nil
}

// msIdent quotes a SQL Server identifier.
func msIdent(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

// msString quotes a SQL Server string literal.
func msString(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (m *SQLServer) InstalledVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v string
	if err := db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(64))").Scan(&v); err != nil {
		return "", err
	}
	// UNVERIFIED (re-read 2026-11-19, the sqladmin v1 discovery document): Cloud SQL's databaseInstalledVersion for SQL Server
	// names the version and its cumulative update; the server's product
	// version (16.0.4325.4) does not name the CU, so the version is answered.
	return m.Version, nil
}

// msSystemDatabases are the databases every SQL Server has.
var msSystemDatabases = map[string]bool{"master": true, "tempdb": true, "model": true, "msdb": true}

func (m *SQLServer) ListDatabases(ctx context.Context) ([]Database, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT name, collation_name FROM sys.databases ORDER BY name")
	if err != nil {
		return nil, msError(err)
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		var coll sql.NullString
		if err := rows.Scan(&d.Name, &coll); err != nil {
			return nil, err
		}
		if msSystemDatabases[d.Name] {
			continue
		}
		d.Collation = coll.String
		out = append(out, d)
	}
	return out, rows.Err()
}

var msCollation = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func (m *SQLServer) CreateDatabase(ctx context.Context, d Database) error {
	if err := checkName("database", d.Name); err != nil {
		return err
	}
	if msSystemDatabases[d.Name] {
		return Errorf(http.StatusConflict, "Database '%s' already exists.", d.Name)
	}
	if d.Charset != "" {
		// SQL Server has no per-database character set: its collation
		// names the code page.
		return Errorf(http.StatusBadRequest, "Invalid request: SQL Server databases have no charset; set the collation.")
	}
	stmt := "CREATE DATABASE " + msIdent(d.Name)
	if d.Collation != "" {
		if !msCollation.MatchString(d.Collation) {
			return Errorf(http.StatusBadRequest, "Invalid request: collation %q.", d.Collation)
		}
		stmt += " COLLATE " + d.Collation
	}
	return m.exec(ctx, stmt)
}

func (m *SQLServer) UpdateDatabase(ctx context.Context, d Database) error {
	if err := checkName("database", d.Name); err != nil {
		return err
	}
	if d.Charset != "" {
		return Errorf(http.StatusBadRequest, "Invalid request: SQL Server databases have no charset; set the collation.")
	}
	if d.Collation == "" {
		return nil
	}
	if !msCollation.MatchString(d.Collation) {
		return Errorf(http.StatusBadRequest, "Invalid request: collation %q.", d.Collation)
	}
	return m.exec(ctx, "ALTER DATABASE "+msIdent(d.Name)+" COLLATE "+d.Collation)
}

func (m *SQLServer) DeleteDatabase(ctx context.Context, name string) error {
	if msSystemDatabases[name] {
		return Errorf(http.StatusBadRequest, "Invalid request: %s is a system database and cannot be deleted.", name)
	}
	if err := checkName("database", name); err != nil {
		return err
	}
	return m.exec(ctx, "ALTER DATABASE "+msIdent(name)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE", "DROP DATABASE "+msIdent(name))
}

// SQLServerRootRole is the server role Cloud SQL's sqlserver user, and
// every user users.insert makes, is a member of (Cloud SQL for SQL Server's
// users page: "CustomerDbRootRole").
const SQLServerRootRole = "CustomerDbRootRole"

// sqlServerRootGrants are the server permissions given to the role.
// UNVERIFIED (re-read 2026-11-19, docs.cloud.google.com/sql/docs/sqlserver/users): Google documents the role, not each permission it holds;
// these are what its documented abilities (create and manage databases,
// logins and users, read server state) need.
const sqlServerRootGrants = "CREATE ANY DATABASE, ALTER ANY LOGIN, ALTER ANY SERVER ROLE, ALTER ANY CONNECTION, " +
	"VIEW ANY DATABASE, VIEW ANY DEFINITION, VIEW SERVER STATE, ALTER TRACE, ALTER SERVER STATE"

func (m *SQLServer) ListUsers(ctx context.Context) ([]User, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT name FROM sys.server_principals
		WHERE type = 'S' AND name <> 'sa' AND name NOT LIKE '##%' ORDER BY name`)
	if err != nil {
		return nil, msError(err)
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

func (m *SQLServer) CreateUser(ctx context.Context, u User, password string) error {
	if err := checkName("user", u.Name); err != nil {
		return err
	}
	return m.exec(ctx, "CREATE LOGIN "+msIdent(u.Name)+" WITH PASSWORD = "+msString(password)+", CHECK_POLICY = ON",
		"ALTER SERVER ROLE "+msIdent(SQLServerRootRole)+" ADD MEMBER "+msIdent(u.Name))
}

func (m *SQLServer) UpdateUser(ctx context.Context, u User, password string) error {
	if err := checkName("user", u.Name); err != nil {
		return err
	}
	return m.exec(ctx, "ALTER LOGIN "+msIdent(u.Name)+" WITH PASSWORD = "+msString(password))
}

func (m *SQLServer) DeleteUser(ctx context.Context, u User) error {
	if u.Name == "sa" {
		return Errorf(http.StatusBadRequest, "Invalid request: sa is CloudBurrow's own login and cannot be deleted.")
	}
	if err := checkName("user", u.Name); err != nil {
		return err
	}
	return m.exec(ctx, "DROP LOGIN "+msIdent(u.Name))
}

// SetupStatements are what a SQL Server instance's server is given once it
// serves, each time it starts: Cloud SQL's CustomerDbRootRole and its
// sqlserver user with rootPassword, and the instance's database flags.
func SetupStatements(rootPassword string, flags []Flag) ([]string, error) {
	role := msIdent(SQLServerRootRole)
	stmts := []string{
		"IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = " + msString(SQLServerRootRole) + ") CREATE SERVER ROLE " + role,
		"GRANT " + sqlServerRootGrants + " TO " + role,
	}
	if rootPassword != "" {
		stmts = append(stmts,
			"IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'sqlserver') CREATE LOGIN [sqlserver] WITH PASSWORD = "+
				msString(rootPassword)+" ELSE ALTER LOGIN [sqlserver] WITH PASSWORD = "+msString(rootPassword),
			"IF IS_SRVROLEMEMBER("+msString(SQLServerRootRole)+", N'sqlserver') = 0 ALTER SERVER ROLE "+role+" ADD MEMBER [sqlserver]")
	}
	fs, err := flagStatements(flags)
	if err != nil {
		return nil, err
	}
	return append(stmts, fs...), nil
}

// Setup gives the server what SetupStatements say.
func (m *SQLServer) Setup(ctx context.Context, rootPassword string, flags []Flag) error {
	stmts, err := SetupStatements(rootPassword, flags)
	if err != nil {
		return err
	}
	return m.exec(ctx, stmts...)
}

// sqlServerFlag is one of Google's Cloud SQL for SQL Server flags
// (docs.cloud.google.com/sql/docs/sqlserver/flags, re-read 2026-11-19).
type sqlServerFlag struct {
	name    string
	boolean bool
	min     int64
	max     int64
	restart bool
	// option is the sp_configure option it sets, "" for the flag's name;
	// trace is the trace flag it turns on; cloudSQL marks Cloud SQL's own
	// settings (Extended Events upload, linked servers, PolyBase,
	// sp_help_revlogin), recorded and not applied.
	option   string
	trace    int
	cloudSQL bool
	str      bool
}

var sqlServerFlags = func() []sqlServerFlag {
	var out []sqlServerFlag
	for _, t := range []struct {
		n       int
		restart bool
	}{{1204, false}, {1222, false}, {1224, false}, {2528, false}, {3205, false}, {3226, false}, {3625, true},
		{4199, false}, {4616, false}, {7806, true}, {13702, true}} {
		out = append(out, sqlServerFlag{name: strconv.Itoa(t.n), boolean: true, restart: t.restart, trace: t.n})
	}
	b := func(name string, restart bool) sqlServerFlag {
		return sqlServerFlag{name: name, boolean: true, restart: restart}
	}
	n := func(name string, lo, hi int64, restart bool) sqlServerFlag {
		return sqlServerFlag{name: name, min: lo, max: hi, restart: restart}
	}
	return append(out,
		n("access check cache bucket count", 0, 65536, false),
		n("access check cache quota", 0, 2147483647, false),
		// Google prints both affinity ranges as 2147483648 to 2147483647;
		// sp_configure's is -2147483648 to 2147483647.
		n("affinity mask", -2147483648, 2147483647, true),
		n("affinity I/O mask", -2147483648, 2147483647, true),
		b("agent xps", false),
		b("automatic soft-numa disabled", true),
		sqlServerFlag{name: "cloud sql xe bucket name", str: true, cloudSQL: true},
		sqlServerFlag{name: "cloud sql xe output total disk size (mb)", min: 10, max: 512, cloudSQL: true},
		sqlServerFlag{name: "cloud sql xe file retention (mins)", min: 0, max: 10080, cloudSQL: true},
		sqlServerFlag{name: "cloud sql xe upload interval (mins)", min: 1, max: 60, cloudSQL: true},
		sqlServerFlag{name: "cloudsql enable linked servers", boolean: true, cloudSQL: true},
		n("cost threshold for parallelism", 0, 32767, false),
		b("contained database authentication", false),
		b("cross db ownership chaining", false),
		n("cursor threshold", -1, 2147483647, false),
		n("default full-text language", 0, 2147483647, false),
		n("default language", 0, 32, false),
		b("default trace enabled", false),
		b("disallow results from triggers", false),
		b("external scripts enabled", true),
		n("ft crawl bandwidth (max)", 0, 32767, false),
		n("ft crawl bandwidth (min)", 0, 32767, false),
		n("ft notify bandwidth (max)", 0, 32767, false),
		n("ft notify bandwidth (min)", 0, 32767, false),
		n("fill factor (%)", 0, 100, true),
		n("index create memory (kb)", 704, 2147483647, false),
		n("locks", 5000, 2147483647, true),
		sqlServerFlag{name: "max degree of parallelism", min: 0, max: 32767},
		n("max server memory (mb)", 1000, 2147483647, false),
		n("max text repl size (b)", -1, 2147483647, false),
		n("max worker threads", 128, 65535, false),
		b("nested triggers", false),
		b("optimize for ad hoc workloads", false),
		n("ph timeout (s)", 1, 3600, false),
		sqlServerFlag{name: "cloud sql enable polybase", boolean: true, restart: true, cloudSQL: true},
		sqlServerFlag{name: "cloud sql enable sp_help_revlogin", boolean: true, cloudSQL: true},
		n("query governor cost limit", 0, 2147483647, false),
		n("query wait (s)", -1, 2147483647, false),
		n("recovery interval (min)", 0, 32767, false),
		b("remote access", true),
		n("remote login timeout (s)", 0, 2147483647, false),
		n("remote query timeout (s)", 0, 2147483647, false),
		b("transform noise words", false),
		n("two digit year cutoff", 1753, 9999, false),
		n("user connections", 0, 32767, true),
		n("user options", 0, 32767, false),
	)
}()

func sqlServerFlagNamed(name string) *sqlServerFlag {
	for i := range sqlServerFlags {
		if strings.EqualFold(sqlServerFlags[i].name, name) {
			return &sqlServerFlags[i]
		}
	}
	return nil
}

// flagValue is f's value for sp_configure or a trace flag: 1 or 0 for a
// boolean, the number otherwise, checked against Google's range.
func (f *sqlServerFlag) value(v string) (int64, error) {
	if f.boolean {
		switch strings.ToLower(v) {
		case "on", "true", "1":
			return 1, nil
		case "off", "false", "0":
			return 0, nil
		}
		return 0, Errorf(http.StatusBadRequest, "Invalid request: flag %q takes on or off, not %q.", f.name, v)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < f.min || n > f.max || (f.name == "user connections" && n > 0 && n < 10) {
		return 0, Errorf(http.StatusBadRequest, "Invalid request: flag %q takes an integer from %d to %d, not %q.", f.name, f.min, f.max, v)
	}
	return n, nil
}

// checkSQLServerFlags refuses a flag Google's SQL Server list does not
// name, or a value out of its range.
func checkSQLServerFlags(flags []Flag) error {
	for _, fl := range flags {
		f := sqlServerFlagNamed(fl.Name)
		if f == nil {
			return Errorf(http.StatusBadRequest, "Invalid request: flag %q is not a Cloud SQL for SQL Server flag.", fl.Name)
		}
		if f.str {
			if f.name == "cloud sql xe bucket name" && !strings.HasPrefix(fl.Value, "gs://") {
				return Errorf(http.StatusBadRequest, "Invalid request: flag %q must start with gs://.", f.name)
			}
			continue
		}
		n, err := f.value(fl.Value)
		if err != nil {
			return err
		}
		if f.name == "cross db ownership chaining" && n == 1 {
			// Google: deprecated, and can't be set to on.
			return Errorf(http.StatusBadRequest, "Invalid request: flag %q is deprecated and can't be set to on.", f.name)
		}
	}
	return nil
}

// flagStatements are the statements that set flags: sp_configure for an
// option, DBCC TRACEON or TRACEOFF for a trace flag. Cloud SQL's own
// settings are recorded only.
func flagStatements(flags []Flag) ([]string, error) {
	if err := checkSQLServerFlags(flags); err != nil {
		return nil, err
	}
	var out []string
	configured := false
	for _, fl := range flags {
		f := sqlServerFlagNamed(fl.Name)
		if f.cloudSQL {
			continue
		}
		n, _ := f.value(fl.Value)
		if f.trace != 0 {
			if n == 1 {
				out = append(out, fmt.Sprintf("DBCC TRACEON(%d, -1)", f.trace))
			} else {
				out = append(out, fmt.Sprintf("DBCC TRACEOFF(%d, -1)", f.trace))
			}
			continue
		}
		if !configured {
			out = append(out, "EXEC sp_configure 'show advanced options', 1", "RECONFIGURE")
			configured = true
		}
		opt := f.option
		if opt == "" {
			opt = f.name
		}
		out = append(out, fmt.Sprintf("EXEC sp_configure %s, %d", msString(opt), n))
	}
	if configured {
		out = append(out, "RECONFIGURE WITH OVERRIDE")
	}
	return out, nil
}

// sqlServerFlagDefs are the SQL Server flags as flags.list answers them.
func sqlServerFlagDefs() []flagDef {
	var out []flagDef
	for _, f := range sqlServerFlags {
		d := flagDef{Name: f.name, Type: "INTEGER", AppliesTo: SQLServerVersions(), RequiresRestart: f.restart}
		switch {
		case f.boolean:
			d.Type = "BOOLEAN"
		case f.str:
			d.Type = "STRING"
		default:
			d.MinValue, d.MaxValue = i64(f.min), i64(f.max)
		}
		if f.trace == 13702 {
			// Linked to SQL Server 2025's documentation.
			d.AppliesTo = []string{"SQLSERVER_2025_EXPRESS", "SQLSERVER_2025_STANDARD", "SQLSERVER_2025_ENTERPRISE"}
		}
		out = append(out, d)
	}
	return out
}

func init() { documentedFlags = append(documentedFlags, sqlServerFlagDefs()...) }
