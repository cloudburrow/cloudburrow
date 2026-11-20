package sqladmin

// PostgreSQL's lock after failed sign-ins (#2014): User.passwordPolicy's
// allowedFailedAttempts with enableFailedAttemptsCheck, as Google
// documents it (create-manage-users, built-in-authentication). Cloud SQL
// locks such a user once it has tried its password incorrectly that many
// times: the user loses its login privilege (User.PasswordStatus.locked,
// "If true, user does not have login privileges"), and is unlocked by
// ALTER USER ... WITH LOGIN, a new password, or the Users page.
//
// PostgreSQL does not count a role's failed sign-ins itself, but its log
// says each one: "password authentication failed for user", which the
// server always logs, and, with log_connections on, "connection
// authorized: user=" for each sign-in that passed. The server logs to its
// container's stderr, which CloudBurrow reads through the cluster
// (Postgres.SignInLog, kubectl logs --follow); a watcher per server counts
// each policy role's failures in a row in the role's own setting
// cloudsql.failed_sign_ins, forgets them at a sign-in that passed, and at
// allowedFailedAttempts sets the role NOLOGIN. The role is a member of
// pgPolicyGroup, which pg_hba.conf holds to its password, so the server
// checks the password rather than trusting the connection.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The role settings that keep a role's lock after failed attempts, beside
// pgExpirationSetting.
const (
	pgAllowedFailedSetting = "cloudsql.allowed_failed_attempts"
	pgFailedCheckSetting   = "cloudsql.enable_failed_attempts_check"
	pgFailedCountSetting   = "cloudsql.failed_sign_ins"
)

// SignInLog streams a PostgreSQL server's log, each line prefixed with the
// RFC 3339 time it was written (kubectl logs --timestamps), from since
// until ctx ends or the server's pod goes.
type SignInLog func(ctx context.Context, since time.Time) (io.ReadCloser, error)

var (
	// pgAuthFailed is the server's FATAL for a wrong password (auth.c,
	// every version CloudBurrow runs, 9.6 to 18).
	pgAuthFailed = regexp.MustCompile(`FATAL:  (?:28P01: )?password authentication failed for user "((?:[^"]|"")*)"`)
	// pgAuthorized is log_connections' line for a sign-in that passed
	// (postinit.c); a replication connection's is "replication
	// connection authorized: user=", which the policy does not count.
	pgAuthorized = regexp.MustCompile(`LOG:  (?:00000: )?connection authorized: user=(\S+)`)
)

// signInEvent is one line of the log a policy may count.
type signInEvent struct {
	at     time.Time
	role   string
	failed bool
}

// parseSignInLine is line's sign-in, if it is one.
func parseSignInLine(line string) (signInEvent, bool) {
	var ev signInEvent
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return ev, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ev, false
	}
	ev.at = t
	if m := pgAuthFailed.FindStringSubmatch(rest); m != nil {
		ev.role, ev.failed = strings.ReplaceAll(m[1], `""`, `"`), true
		return ev, true
	}
	if m := pgAuthorized.FindStringSubmatch(rest); m != nil && !strings.Contains(rest, "replication connection") {
		ev.role = m[1]
		return ev, true
	}
	return ev, false
}

// signInWatches are the running watchers, by server.
var signInWatches = struct {
	sync.Mutex
	m map[string]context.CancelFunc
}{m: map[string]context.CancelFunc{}}

func (p *Postgres) logKey() string {
	if p.LogKey != "" {
		return p.LogKey
	}
	return p.Name
}

// countsSignIns is whether p's failed sign-ins can be counted.
func (p *Postgres) countsSignIns() bool { return p.SignInLog != nil }

// WatchSignIns starts counting p's failed sign-ins, once, if one of its
// roles has a lock after failed attempts: SetUserPolicy starts it, and the
// provisioner each time the server starts.
func (p *Postgres) WatchSignIns(ctx context.Context) error {
	if !p.countsSignIns() {
		return nil
	}
	has, err := p.hasLockout(ctx)
	if err != nil || !has {
		return err
	}
	p.watch()
	return nil
}

// StopSignInWatch stops the watcher of server key, if one runs.
func StopSignInWatch(key string) {
	signInWatches.Lock()
	defer signInWatches.Unlock()
	if cancel := signInWatches.m[key]; cancel != nil {
		cancel()
		delete(signInWatches.m, key)
	}
}

// StopSignInWatches stops every watcher.
func StopSignInWatches() {
	signInWatches.Lock()
	defer signInWatches.Unlock()
	for k, cancel := range signInWatches.m {
		cancel()
		delete(signInWatches.m, k)
	}
}

func (p *Postgres) watch() {
	key := p.logKey()
	signInWatches.Lock()
	defer signInWatches.Unlock()
	if signInWatches.m[key] != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	signInWatches.m[key] = cancel
	go p.watchLoop(ctx, key)
}

// watchLoop follows the log, again from where it was when the stream ends
// (the pod was replaced, by a restart or a failover), until it is stopped
// or no role of the server has a lock any more.
func (p *Postgres) watchLoop(ctx context.Context, key string) {
	defer func() {
		signInWatches.Lock()
		if signInWatches.m[key] != nil && ctx.Err() == nil {
			signInWatches.m[key]()
			delete(signInWatches.m, key)
		}
		signInWatches.Unlock()
	}()
	since := time.Now().Add(-time.Second)
	var last time.Time
	for ctx.Err() == nil {
		r, err := p.SignInLog(ctx, since)
		if err == nil {
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				ev, ok := parseSignInLine(sc.Text())
				if !ok || !ev.at.After(last) {
					continue
				}
				last = ev.at
				p.countSignIn(ctx, ev)
			}
			_ = r.Close()
		}
		if !last.IsZero() {
			since = last
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if has, err := p.hasLockout(ctx); err == nil && !has {
			return
		}
	}
}

// pgRoleSettings are role's cloudsql.* settings.
func pgRoleSettings(ctx context.Context, c *pgx.Conn, role string) (map[string]string, bool, error) {
	var cfg []string
	var login bool
	err := c.QueryRow(ctx, `SELECT r.rolcanlogin, COALESCE(s.setconfig, '{}') FROM pg_catalog.pg_roles r
		LEFT JOIN pg_catalog.pg_db_role_setting s ON s.setrole = r.oid AND s.setdatabase = 0 WHERE r.rolname = $1`, role).Scan(&login, &cfg)
	if err != nil {
		return nil, false, err
	}
	return settingsMap(cfg), login, nil
}

func settingsMap(cfg []string) map[string]string {
	m := map[string]string{}
	for _, kv := range cfg {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "cloudsql.") {
			m[k] = v
		}
	}
	return m
}

// lockoutOf is a role's allowedFailedAttempts, 0 when its failed sign-ins
// do not lock it.
func lockoutOf(set map[string]string) int64 {
	n, _ := strconv.ParseInt(set[pgAllowedFailedSetting], 10, 64)
	if set[pgFailedCheckSetting] != "on" || n <= 0 {
		return 0
	}
	return n
}

func (p *Postgres) hasLockout(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close(context.Background())
	var has bool
	err = c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_db_role_setting s, unnest(s.setconfig) x
		WHERE s.setdatabase = 0 AND x = '`+pgFailedCheckSetting+`=on')`).Scan(&has)
	return has, err
}

// countSignIn counts ev against its role's policy: a failure adds one,
// and the one that reaches allowedFailedAttempts locks the role; a sign-in
// that passed forgets them.
func (p *Postgres) countSignIn(ctx context.Context, ev signInEvent) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return
	}
	defer c.Close(context.Background())
	set, login, err := pgRoleSettings(ctx, c, ev.role)
	if err != nil {
		return
	}
	allowed := lockoutOf(set)
	if allowed == 0 {
		return
	}
	role := pgx.Identifier{ev.role}.Sanitize()
	n, _ := strconv.ParseInt(set[pgFailedCountSetting], 10, 64)
	switch {
	case !ev.failed:
		if n > 0 {
			_, _ = c.Exec(ctx, "ALTER ROLE "+role+" RESET "+pgFailedCountSetting)
		}
	case !login:
		// Locked already.
	case n+1 >= allowed:
		_, _ = c.Exec(ctx, "ALTER ROLE "+role+" NOLOGIN")
		_, _ = c.Exec(ctx, "ALTER ROLE "+role+" RESET "+pgFailedCountSetting)
	default:
		_, _ = c.Exec(ctx, fmt.Sprintf("ALTER ROLE %s SET %s = '%d'", role, pgFailedCountSetting, n+1))
	}
}
