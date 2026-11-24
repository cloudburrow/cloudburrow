package sqladmin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// Cloud SQL Studio (#2208): the console's query editor signs in to an
// instance's database as one of its users, with the user's own password
// (Built-in database authentication) or as the console's IAM principal
// (IAM database authentication), and runs statements as that user. The
// server checks the sign-in as it checks every other: a wrong password is
// refused with the server's own message.
//
// Google's limits (https://docs.cloud.google.com/sql/docs/postgres/manage-data-using-studio,
// read 2026-11-24): only a running instance is queried; at most 10
// executeSql requests run at once on an instance, the 11th refused with
// "At most 'x' concurrent queries may be run on this instance. Try again
// later."; on MySQL the root user and users with empty passwords are not
// supported, and a database is always required.

// StudioMaxConcurrent is how many Studio statements run at once on one
// instance, Google's executeSql limit.
const StudioMaxConcurrent = 10

// StudioResult is one statement's result in Cloud SQL Studio.
type StudioResult struct {
	Columns []string
	Types   []string
	Rows    [][]*string // nil is NULL
	// Message is a statement's command tag when it returns no rows.
	Message string
	// More is set when rows beyond the limit were dropped.
	More bool
}

// StudioLogin is a Cloud SQL Studio sign-in.
type StudioLogin struct {
	Project, Instance, Database string
	// User and Password sign in with built-in database authentication.
	User, Password string
	// IAMEmail, when set, signs in with IAM database authentication as
	// the IAM user or service account of this principal; User and
	// Password are then ignored.
	IAMEmail string
}

// ErrStudioSignIn wraps a refused sign-in; its message is the server's.
type ErrStudioSignIn struct{ Err error }

func (e ErrStudioSignIn) Error() string { return e.Err.Error() }
func (e ErrStudioSignIn) Unwrap() error { return e.Err }

var studioSlots = struct {
	sync.Mutex
	running map[string]int
}{running: map[string]int{}}

// StudioSQL runs stmt in l's database as l's user, returning each
// statement's result until one fails, and that error. The database user
// it signs in as is returned too (the IAM user's role for an IAM
// sign-in).
func (s *Server) StudioSQL(ctx context.Context, l StudioLogin, stmt string, rowLimit int64) ([]StudioResult, string, error) {
	e := s.engineIn(l.Project, l.Instance)
	if e == nil {
		return nil, "", Errorf(http.StatusNotFound, "The Cloud SQL instance %s does not exist in project %s.", l.Instance, l.Project)
	}
	if err := s.stoppedRefusal(s.createdNamed(l.Project, l.Instance), http.MethodPost, []string{"", "", "executeSql"}, ""); err != nil {
		return nil, "", err
	}
	if err := s.busy(l.Instance); err != nil {
		return nil, "", notRunningErr()
	}
	runner, ok := e.(sqlRunner)
	if !ok {
		return nil, "", unimplemented("Cloud SQL Studio on "+e.DatabaseVersion(), "its server cannot run statements as a user")
	}
	if strings.TrimSpace(l.Database) == "" {
		return nil, "", Errorf(http.StatusBadRequest, "Invalid request: database is required.")
	}
	user, password := l.User, l.Password
	if l.IAMEmail != "" {
		typ := TypeIAMUser
		if strings.HasSuffix(strings.ToLower(l.IAMEmail), saSuffix) {
			typ = TypeIAMServiceAccount
		}
		name, err := iamDBName(typ, l.IAMEmail, isMySQLEngine(e))
		if err != nil {
			return nil, "", ErrStudioSignIn{err}
		}
		u, ok := iamUserNamed(ctx, e, name)
		if !ok {
			return nil, name, ErrStudioSignIn{fmt.Errorf("%s is not an IAM user of instance %s: add it on the instance's Users page", l.IAMEmail, l.Instance)}
		}
		login, err := s.checkIAMLoginOf(l.Project, e, u, strings.ToLower(l.IAMEmail))
		if err != nil {
			return nil, name, ErrStudioSignIn{err}
		}
		user, password = login.role, login.password
	} else {
		if strings.TrimSpace(user) == "" {
			return nil, "", Errorf(http.StatusBadRequest, "Invalid request: user is required.")
		}
		if isMySQLEngine(e) && user == "root" {
			return nil, user, ErrStudioSignIn{errors.New("Cloud SQL Studio does not support the root user: sign in as another user")}
		}
		if isMySQLEngine(e) && password == "" {
			return nil, user, ErrStudioSignIn{errors.New("Cloud SQL Studio does not support users with empty passwords")}
		}
	}

	studioSlots.Lock()
	key := l.Project + "/" + l.Instance
	if studioSlots.running[key] >= StudioMaxConcurrent {
		studioSlots.Unlock()
		return nil, user, Errorf(http.StatusTooManyRequests, "At most '%d' concurrent queries may be run on this instance. Try again later.", StudioMaxConcurrent)
	}
	studioSlots.running[key]++
	studioSlots.Unlock()
	defer func() {
		studioSlots.Lock()
		studioSlots.running[key]--
		studioSlots.Unlock()
	}()

	results, err := runner.RunSQL(ctx, user, password, l.Database, stmt, rowLimit)
	var ae authError
	if errors.As(err, &ae) {
		err = ErrStudioSignIn{studioServerError(ae.error)}
	} else if err != nil {
		err = studioServerError(err)
	}
	out := make([]StudioResult, 0, len(results))
	for _, r := range results {
		sr := StudioResult{Rows: r.Rows, Message: r.Message, More: r.More}
		for _, c := range r.Columns {
			name, _ := c["name"].(string)
			typ, _ := c["type"].(string)
			sr.Columns = append(sr.Columns, name)
			sr.Types = append(sr.Types, typ)
		}
		out = append(out, sr)
	}
	return out, user, err
}

// studioServerError is the server's own words for err: a PostgreSQL
// error's severity and message, without the client's connection details.
func studioServerError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return errors.New(pe.Severity + ": " + pe.Message)
	}
	var me *mysql.MySQLError
	if asMySQL(err, &me) {
		return errors.New(me.Error())
	}
	return err
}

// StudioEngine is the database engine of project's instance ("POSTGRES",
// "MYSQL" or "SQLSERVER"), "" when it has none.
func (s *Server) StudioEngine(project, instance string) string {
	e := s.engineIn(project, instance)
	if e == nil {
		return ""
	}
	return engineOf(e.DatabaseVersion())
}
