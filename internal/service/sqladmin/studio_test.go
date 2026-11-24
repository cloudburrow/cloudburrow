package sqladmin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// studioFake is a server that runs statements, accepting one password.
type studioFake struct {
	*fake
	version string
	block   chan struct{}
	mu      sync.Mutex
	signins []string
}

func (f *studioFake) DatabaseVersion() string { return f.version }

func (f *studioFake) RunSQL(_ context.Context, user, password, database, stmt string, _ int64) ([]sqlResult, error) {
	f.mu.Lock()
	f.signins = append(f.signins, user+"/"+database)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if password != "right" {
		return nil, authError{&pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: `password authentication failed for user "` + user + `"`}}
	}
	if strings.Contains(stmt, "nope") {
		return nil, &pgconn.PgError{Severity: "ERROR", Code: "42P01", Message: `relation "nope" does not exist`}
	}
	v := "1"
	return []sqlResult{{Columns: []map[string]any{{"name": "n", "type": "INT4"}}, Rows: [][]*string{{&v}}}}, nil
}

// TestStudioSQL (#2208): Cloud SQL Studio's run signs in as the user with
// the password given, the server's refusal its error in the server's
// words; MySQL's root and empty passwords are refused as Google documents;
// an IAM sign-in on an instance whose IAM flag is off is refused; and at
// most 10 runs go at once on an instance.
func TestStudioSQL(t *testing.T) {
	ctx := context.Background()
	pg := &studioFake{fake: newFake(), version: "POSTGRES_17"}
	srv := NewServer("", pg)
	login := StudioLogin{Project: "p", Instance: "pg", Database: "cloudburrow", User: "cloudburrow", Password: "wrong"}
	_, _, err := srv.StudioSQL(ctx, login, "SELECT 1", 10)
	var signIn ErrStudioSignIn
	if !errors.As(err, &signIn) || err.Error() != `FATAL: password authentication failed for user "cloudburrow"` {
		t.Errorf("a wrong password: %v", err)
	}
	login.Password = "right"
	res, role, err := srv.StudioSQL(ctx, login, "SELECT 1 AS n", 10)
	if err != nil || role != "cloudburrow" || len(res) != 1 || res[0].Columns[0] != "n" || *res[0].Rows[0][0] != "1" {
		t.Errorf("the right password: %+v %q %v", res, role, err)
	}
	if _, _, err := srv.StudioSQL(ctx, login, "SELECT * FROM nope", 10); err == nil || err.Error() != `ERROR: relation "nope" does not exist` {
		t.Errorf("a refused statement: %v", err)
	}
	if _, _, err := srv.StudioSQL(ctx, StudioLogin{Project: "p", Instance: "pg", User: "u", Password: "right"}, "SELECT 1", 10); err == nil {
		t.Error("a run with no database")
	}
	_, _, err = srv.StudioSQL(ctx, StudioLogin{Project: "p", Instance: "pg", Database: "cloudburrow", IAMEmail: "sa@p.iam.gserviceaccount.com"}, "SELECT 1", 10)
	if !errors.As(err, &signIn) || !strings.Contains(err.Error(), "is not an IAM user of instance pg") {
		t.Errorf("an IAM sign-in as no IAM user: %v", err)
	}
	pg.users["sa@p.iam"] = User{Name: "sa@p.iam", Type: TypeIAMServiceAccount}
	_, _, err = srv.StudioSQL(ctx, StudioLogin{Project: "p", Instance: "pg", Database: "cloudburrow", IAMEmail: "sa@p.iam.gserviceaccount.com"}, "SELECT 1", 10)
	if !errors.As(err, &signIn) || !strings.Contains(err.Error(), `Cloud SQL IAM service account authentication failed for user "sa@p.iam": the instance's cloudsql.iam_authentication flag is not on`) {
		t.Errorf("an IAM sign-in with the flag off: %v", err)
	}

	my := &studioFake{fake: newFake(), version: "MYSQL_8_4"}
	msrv := NewServer("", my)
	for _, l := range []StudioLogin{{User: "root", Password: "right"}, {User: "app", Password: ""}} {
		l.Project, l.Instance, l.Database = "p", "pg", "cloudburrow"
		if _, _, err := msrv.StudioSQL(ctx, l, "SELECT 1", 10); !errors.As(err, &signIn) {
			t.Errorf("MySQL as %q with password %q: %v", l.User, l.Password, err)
		}
	}
	if len(my.signins) != 0 {
		t.Errorf("refused sign-ins reached the server: %v", my.signins)
	}

	// The 11th concurrent run is refused.
	pg.block = make(chan struct{})
	var wg sync.WaitGroup
	for range StudioMaxConcurrent {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = srv.StudioSQL(ctx, login, "SELECT 1", 10) }()
	}
	for {
		pg.mu.Lock()
		n := len(pg.signins)
		pg.mu.Unlock()
		if n >= 3+StudioMaxConcurrent {
			break
		}
	}
	_, _, err = srv.StudioSQL(ctx, login, "SELECT 1", 10)
	if err == nil || err.Error() != "At most '10' concurrent queries may be run on this instance. Try again later." {
		t.Errorf("the 11th concurrent run: %v", err)
	}
	close(pg.block)
	wg.Wait()
}
