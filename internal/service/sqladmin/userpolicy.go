package sqladmin

// A user's own password policy, User.passwordPolicy (#2009), and MySQL's
// dual passwords, User.dualPasswordType, as Google documents them
// (built-in-authentication, create-manage-users; gcloud sql users
// set-password-policy and set-password --retain-password /
// --discard-dual-password).
//
// Each is the engine's own: on MySQL 8.0 and later the account's
// FAILED_LOGIN_ATTEMPTS with PASSWORD_LOCK_TIME UNBOUNDED (locked until
// an administrator unlocks it), PASSWORD EXPIRE INTERVAL n DAY, PASSWORD
// REQUIRE CURRENT and RETAIN CURRENT PASSWORD / DISCARD OLD PASSWORD; on
// MySQL 5.7 only the expiration. On PostgreSQL the expiration is the
// role's VALID UNTIL, set again at each password change; a role with a
// policy is a member of pgPolicyGroup, which pg_hba.conf holds to its
// password, so the server checks it (and its VALID UNTIL) rather than
// trusting the connection. PostgreSQL itself does not count failed
// sign-ins, so a lock after failed attempts is not served there (#2014).

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// userPolicyIn is User.passwordPolicy as users.insert and update take it:
// a field that is sent changes, the others stay (gcloud's
// set-password-policy sends only the flags it is given).
type userPolicyIn struct {
	AllowedFailedAttempts      *int64  `json:"allowedFailedAttempts"`
	PasswordExpirationDuration *string `json:"passwordExpirationDuration"`
	EnableFailedAttemptsCheck  *bool   `json:"enableFailedAttemptsCheck"`
	EnablePasswordVerification *bool   `json:"enablePasswordVerification"`
}

// UserPolicy is a user's password policy and its status, as the engine
// holds them.
type UserPolicy struct {
	AllowedFailedAttempts int64
	// ExpirationDays is the password's lifetime, 0 for none.
	ExpirationSeconds    int64
	FailedAttemptsCheck  bool
	PasswordVerification bool
	// Locked is the account locked by its failed sign-ins.
	Locked bool
	// ExpirationTime is when the current password expires, RFC 3339 in
	// UTC, "" for never.
	ExpirationTime string
	// DualPassword is a retained (secondary) password the account has.
	DualPassword bool
}

func (p UserPolicy) set() bool {
	return p.AllowedFailedAttempts > 0 || p.ExpirationSeconds > 0 || p.FailedAttemptsCheck || p.PasswordVerification
}

// json is passwordPolicy as users.get and list answer it.
func (p UserPolicy) json() map[string]any {
	m := map[string]any{}
	if p.AllowedFailedAttempts > 0 {
		m["allowedFailedAttempts"] = p.AllowedFailedAttempts
	}
	if p.ExpirationSeconds > 0 {
		m["passwordExpirationDuration"] = fmt.Sprintf("%ds", p.ExpirationSeconds)
	}
	if p.FailedAttemptsCheck {
		m["enableFailedAttemptsCheck"] = true
	}
	if p.PasswordVerification {
		m["enablePasswordVerification"] = true
	}
	st := map[string]any{}
	if p.Locked {
		st["locked"] = true
	}
	if p.ExpirationTime != "" {
		st["passwordExpirationTime"] = p.ExpirationTime
	}
	if len(st) > 0 {
		m["status"] = st
	}
	return m
}

// apply is in over p.
func (p UserPolicy) apply(in *userPolicyIn, expiration int64) UserPolicy {
	if in.AllowedFailedAttempts != nil {
		p.AllowedFailedAttempts = *in.AllowedFailedAttempts
	}
	if in.PasswordExpirationDuration != nil {
		p.ExpirationSeconds = expiration
	}
	if in.EnableFailedAttemptsCheck != nil {
		p.FailedAttemptsCheck = *in.EnableFailedAttemptsCheck
	}
	if in.EnablePasswordVerification != nil {
		p.PasswordVerification = *in.EnablePasswordVerification
	}
	return p
}

// Dual password types, User.dualPasswordType.
const (
	dualUnspecified = "DUAL_PASSWORD_TYPE_UNSPECIFIED"
	dualNoModify    = "NO_MODIFY_DUAL_PASSWORD"
	dualNone        = "NO_DUAL_PASSWORD"
	dualRetain      = "DUAL_PASSWORD"
)

// policyEngine is an engine that serves users' password policies.
type policyEngine interface {
	// UserPolicies are the users' policies and statuses, by
	// passwordKey(name, host).
	UserPolicies(ctx context.Context) (map[string]UserPolicy, error)
	// SetUserPolicy sets u's policy to p, and unlocks it.
	SetUserPolicy(ctx context.Context, u User, p UserPolicy) error
	// PasswordChanged is called once u's password has changed.
	PasswordChanged(ctx context.Context, u User) error
	// ChangeDualPassword changes u's password to password, keeping the
	// current one as its secondary (dualRetain), or discards the
	// secondary (dualNone), having changed the password first unless
	// password is "".
	ChangeDualPassword(ctx context.Context, u User, password, dual string) error
}

// mysqlOld is MySQL 5.6 or 5.7, before account locking, password
// verification and dual passwords (8.0).
func mysqlOld(version string) bool {
	return strings.HasPrefix(version, "MYSQL_5_")
}

// checkUserPolicy refuses a policy e does not take, and returns its
// expiration in seconds.
func checkUserPolicy(e Engine, in *userPolicyIn) (int64, error) {
	bad := func(f string, a ...any) error {
		return Errorf(http.StatusBadRequest, "Invalid request: "+f, a...)
	}
	if _, ok := e.(policyEngine); !ok {
		return 0, unimplemented("User.passwordPolicy on "+e.DatabaseVersion(),
			"user password policies are served on MySQL and PostgreSQL")
	}
	v := e.DatabaseVersion()
	mysql := strings.HasPrefix(v, "MYSQL_")
	var secs int64
	if in.PasswordExpirationDuration != nil && *in.PasswordExpirationDuration != "" {
		d, err := parseProtoDuration(*in.PasswordExpirationDuration)
		if err != nil || d < 0 {
			return 0, bad("passwordPolicy.passwordExpirationDuration %q is not a duration.", *in.PasswordExpirationDuration)
		}
		secs = int64(math.Ceil(d.Seconds()))
		if mysql {
			// MySQL's PASSWORD EXPIRE INTERVAL is in days, 1 to 65535.
			days := (secs + 86399) / 86400
			if days > 65535 {
				return 0, bad("passwordPolicy.passwordExpirationDuration is at most 65535 days on MySQL.")
			}
			secs = days * 86400
		}
		if v == "MYSQL_5_6" {
			return 0, bad("user password policies are supported on Cloud SQL for MySQL 5.7 (password expiration) and 8.0 and later.")
		}
	}
	if in.AllowedFailedAttempts != nil && (*in.AllowedFailedAttempts < 0 || *in.AllowedFailedAttempts > 32767) {
		return 0, bad("passwordPolicy.allowedFailedAttempts must be from 0 to 32767.")
	}
	locking := (in.AllowedFailedAttempts != nil && *in.AllowedFailedAttempts > 0) ||
		(in.EnableFailedAttemptsCheck != nil && *in.EnableFailedAttemptsCheck)
	verify := in.EnablePasswordVerification != nil && *in.EnablePasswordVerification
	switch {
	case !mysql && verify:
		return 0, bad("passwordPolicy.enablePasswordVerification is supported only for MySQL.")
	case !mysql && locking:
		return 0, unimplemented("passwordPolicy.allowedFailedAttempts and enableFailedAttemptsCheck on PostgreSQL",
			"PostgreSQL does not count a role's failed sign-ins, and the lock Cloud SQL adds to it is not served (#2014)")
	case mysqlOld(v) && (locking || verify):
		return 0, bad("passwordPolicy.allowedFailedAttempts, enableFailedAttemptsCheck and enablePasswordVerification are supported on Cloud SQL for MySQL 8.0 and later.")
	}
	return secs, nil
}

// checkDual refuses a dualPasswordType e or the request does not take.
func checkDual(e Engine, dual, password string) error {
	switch dual {
	case "", dualUnspecified, dualNoModify:
		return nil
	case dualRetain, dualNone:
	default:
		return Errorf(http.StatusBadRequest, "Invalid value at 'body.dual_password_type' (type.googleapis.com/google.cloud.sql.v1.User.DualPasswordType), %q", dual)
	}
	v := e.DatabaseVersion()
	if _, ok := e.(policyEngine); !ok || !strings.HasPrefix(v, "MYSQL_") || mysqlOld(v) {
		return Errorf(http.StatusBadRequest, "Invalid request: dual passwords (dualPasswordType %s) are only available for MySQL 8.0 and later.", dual)
	}
	if dual == dualRetain && password == "" {
		return Errorf(http.StatusBadRequest, "Invalid request: dualPasswordType DUAL_PASSWORD retains the current password while a new one is set: a password is required.")
	}
	return nil
}

// withPolicies fills in us' policies, where e serves them.
func withPolicies(ctx context.Context, e Engine, us []User) ([]User, error) {
	pe, ok := e.(policyEngine)
	if !ok {
		return us, nil
	}
	ps, err := pe.UserPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range us {
		if p, ok := ps[passwordKey(us[i].Name, us[i].Host)]; ok {
			p := p
			us[i].Policy = &p
		}
	}
	return us, nil
}

// MySQL.

func (m *MySQL) acct(u User) string { return myString(u.Name) + "@" + myString(hostOr(u.Host)) }

func (m *MySQL) UserPolicies(ctx context.Context) (map[string]UserPolicy, error) {
	if m.Version == "MYSQL_5_6" {
		return map[string]UserPolicy{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, myError(err)
	}
	defer conn.Close()
	// UTC, for password_last_changed and the error log's LOGGED.
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		return nil, myError(err)
	}
	old := mysqlOld(m.Version)
	q := `SELECT User, Host, COALESCE(password_lifetime, 0), COALESCE(DATE_FORMAT(password_last_changed, '%Y-%m-%dT%H:%i:%sZ'), ''),
		0, 0, 'N', 0, ''`
	if !old {
		q = `SELECT User, Host, COALESCE(password_lifetime, 0), COALESCE(DATE_FORMAT(password_last_changed, '%Y-%m-%dT%H:%i:%sZ'), ''),
		COALESCE(CAST(JSON_EXTRACT(User_attributes, '$.Password_locking.failed_login_attempts') AS SIGNED), 0),
		COALESCE(CAST(JSON_EXTRACT(User_attributes, '$.Password_locking.password_lock_time_days') AS SIGNED), 0),
		COALESCE(Password_require_current, 'N'),
		COALESCE(JSON_CONTAINS_PATH(User_attributes, 'one', '$.additional_password'), 0),
		COALESCE(JSON_UNQUOTE(JSON_EXTRACT(User_attributes, '$.metadata.cloudsql_unlocked_at')), '')`
	}
	rows, err := conn.QueryContext(ctx, q+` FROM mysql.user WHERE User NOT LIKE 'mysql.%'`)
	if err != nil {
		return nil, myError(err)
	}
	type row struct {
		name, host     string
		p              UserPolicy
		lockDays       int64
		unlockedAt     string
		lifetime       int64
		changed, reqCu string
	}
	var all []row
	for rows.Next() {
		var r row
		var dual int64
		if err := rows.Scan(&r.name, &r.host, &r.lifetime, &r.changed, &r.p.AllowedFailedAttempts, &r.lockDays, &r.reqCu,
			&dual, &r.unlockedAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.p.ExpirationSeconds = r.lifetime * 86400
		r.p.FailedAttemptsCheck = r.lockDays != 0 && r.p.AllowedFailedAttempts > 0
		r.p.PasswordVerification = r.reqCu == "Y"
		r.p.DualPassword = dual != 0
		if t, err := time.Parse(time.RFC3339, r.changed); err == nil && r.lifetime > 0 {
			r.p.ExpirationTime = t.Add(time.Duration(r.lifetime) * 24 * time.Hour).Format(time.RFC3339)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]UserPolicy{}
	for _, r := range all {
		if r.p.FailedAttemptsCheck {
			// MySQL keeps an account's lock in memory only: its error log
			// says when one was refused for it (ER_ACCESS_DENIED_FOR_USER_
			// ACCOUNT_BLOCKED_BY_PASSWORD_LOCK, logged at
			// log_error_verbosity 3), since the server started and since
			// the account was last unlocked.
			if r.unlockedAt == "" {
				r.unlockedAt = "1970-01-01 00:00:00"
			}
			var n int64
			err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.error_log
				WHERE ERROR_CODE = 'MY-013525' AND LOCATE(CONCAT('Access denied for user ''', ?, '''@'), DATA) = 1
				AND LOGGED > CAST(? AS DATETIME(6))
				AND LOGGED > UTC_TIMESTAMP(6) - INTERVAL (SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Uptime') SECOND`,
				r.name, r.unlockedAt).Scan(&n)
			if err != nil {
				return nil, myError(err)
			}
			r.p.Locked = n > 0
		}
		if r.p.set() || r.p.DualPassword {
			out[passwordKey(r.name, r.host)] = r.p
		}
	}
	return out, nil
}

func (m *MySQL) SetUserPolicy(ctx context.Context, u User, p UserPolicy) error {
	acct := m.acct(u)
	exp := "PASSWORD EXPIRE NEVER"
	if p.ExpirationSeconds > 0 {
		exp = fmt.Sprintf("PASSWORD EXPIRE INTERVAL %d DAY", p.ExpirationSeconds/86400)
	}
	if mysqlOld(m.Version) {
		return m.exec(ctx, "ALTER USER "+acct+" "+exp)
	}
	lock := "0"
	if p.FailedAttemptsCheck && p.AllowedFailedAttempts > 0 {
		lock = "UNBOUNDED"
	}
	verify := "PASSWORD REQUIRE CURRENT OPTIONAL"
	if p.PasswordVerification {
		verify = "PASSWORD REQUIRE CURRENT"
	}
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
	var now string
	if err := conn.QueryRowContext(ctx, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')").Scan(&now); err != nil {
		return myError(err)
	}
	// Setting FAILED_LOGIN_ATTEMPTS and ACCOUNT UNLOCK reset the account's
	// count and lock; cloudsql_unlocked_at marks when, for UserPolicies.
	stmts := []string{fmt.Sprintf("ALTER USER %s FAILED_LOGIN_ATTEMPTS %d PASSWORD_LOCK_TIME %s %s %s ACCOUNT UNLOCK ATTRIBUTE %s",
		acct, p.AllowedFailedAttempts, lock, exp, verify, myString(`{"cloudsql_unlocked_at": "`+now+`"}`))}
	if lock != "0" {
		// The refusals of a locked account are notes of the error log.
		stmts = append(stmts, "SET PERSIST log_error_verbosity = 3")
	}
	for _, s := range stmts {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return myError(err)
		}
	}
	return nil
}

// PasswordChanged: MySQL restarts the expiration itself.
func (m *MySQL) PasswordChanged(context.Context, User) error { return nil }

func (m *MySQL) ChangeDualPassword(ctx context.Context, u User, password, dual string) error {
	acct := m.acct(u)
	var stmts []string
	switch dual {
	case dualRetain:
		stmts = append(stmts, "ALTER USER "+acct+" IDENTIFIED BY "+myString(password)+" RETAIN CURRENT PASSWORD")
	case dualNone:
		if password != "" {
			stmts = append(stmts, "ALTER USER "+acct+" IDENTIFIED BY "+myString(password))
		}
		stmts = append(stmts, "ALTER USER "+acct+" DISCARD OLD PASSWORD")
	}
	return m.exec(ctx, stmts...)
}

// PostgreSQL.

// pgPolicyGroup is the role whose members, the roles with a password
// policy, pg_hba.conf holds to their password.
const pgPolicyGroup = "cloudsqlpasswordpolicy"

// pgExpirationSetting is the role setting that keeps a role's
// passwordExpirationDuration, in seconds, for each password change.
const pgExpirationSetting = "cloudsql.password_expiration_seconds"

func (p *Postgres) UserPolicies(ctx context.Context) (map[string]UserPolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(ctx, `SELECT r.rolname, NOT r.rolcanlogin,
		CASE WHEN r.rolvaliduntil IS NULL OR r.rolvaliduntil = 'infinity' THEN ''
			ELSE to_char(r.rolvaliduntil AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') END,
		split_part(x, '=', 2)
		FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_db_role_setting s ON s.setrole = r.oid AND s.setdatabase = 0,
		unnest(s.setconfig) x WHERE x LIKE '`+pgExpirationSetting+`=%'`)
	if err != nil {
		return nil, pgError(err)
	}
	defer rows.Close()
	out := map[string]UserPolicy{}
	for rows.Next() {
		var name, secs string
		var up UserPolicy
		if err := rows.Scan(&name, &up.Locked, &up.ExpirationTime, &secs); err != nil {
			return nil, err
		}
		_, _ = fmt.Sscan(secs, &up.ExpirationSeconds)
		if up.set() {
			out[passwordKey(name, "")] = up
		}
	}
	return out, rows.Err()
}

func (p *Postgres) SetUserPolicy(ctx context.Context, u User, up UserPolicy) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	role := pgx.Identifier{u.Name}.Sanitize()
	var stmts []string
	if up.ExpirationSeconds > 0 {
		var exists bool
		if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", pgPolicyGroup).Scan(&exists); err != nil {
			return pgError(err)
		}
		if !exists {
			stmts = append(stmts, "CREATE ROLE "+pgPolicyGroup+" NOLOGIN")
		}
		until := time.Now().UTC().Add(time.Duration(up.ExpirationSeconds) * time.Second).Format(time.RFC3339)
		stmts = append(stmts, "GRANT "+pgPolicyGroup+" TO "+role,
			"ALTER ROLE "+role+" LOGIN VALID UNTIL "+quotePGString(until),
			fmt.Sprintf("ALTER ROLE %s SET %s = '%d'", role, pgExpirationSetting, up.ExpirationSeconds))
	} else {
		stmts = append(stmts, "ALTER ROLE "+role+" LOGIN VALID UNTIL 'infinity'", "ALTER ROLE "+role+" RESET "+pgExpirationSetting)
		var member bool
		if err := c.QueryRow(ctx, "SELECT pg_has_role($1, $2, 'MEMBER') AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $2)",
			u.Name, pgPolicyGroup).Scan(&member); err == nil && member {
			stmts = append(stmts, "REVOKE "+pgPolicyGroup+" FROM "+role)
		}
	}
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			return pgError(err)
		}
	}
	if up.ExpirationSeconds > 0 {
		return pgEnsureHBA(ctx, c, "host all +"+pgPolicyGroup+" all md5")
	}
	return nil
}

// PasswordChanged restarts the role's expiration from now.
func (p *Postgres) PasswordChanged(ctx context.Context, u User) error {
	ps, err := p.UserPolicies(ctx)
	if err != nil {
		return err
	}
	up, ok := ps[passwordKey(u.Name, "")]
	if !ok || up.ExpirationSeconds == 0 {
		return nil
	}
	until := time.Now().UTC().Add(time.Duration(up.ExpirationSeconds) * time.Second).Format(time.RFC3339)
	return p.exec(ctx, "ALTER ROLE "+pgx.Identifier{u.Name}.Sanitize()+" VALID UNTIL "+quotePGString(until))
}

func (p *Postgres) ChangeDualPassword(context.Context, User, string, string) error {
	return Errorf(http.StatusBadRequest, "Invalid request: dual passwords are only available for MySQL 8.0 and later.")
}
