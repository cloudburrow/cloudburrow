package sqladmin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// The server's own lines, as kubectl logs --timestamps gives them, for
// the versions CloudBurrow runs (#2014).
func TestParseSignInLine(t *testing.T) {
	for _, tt := range []struct {
		line   string
		role   string
		failed bool
		ok     bool
	}{
		{`2026-11-20T10:00:01.123456789Z 2026-11-20 10:00:01.123 UTC [77] FATAL:  password authentication failed for user "locky"`, "locky", true, true},
		{`2026-11-20T10:00:01.2Z 2026-11-20 10:00:01.200 UTC [78] FATAL:  28P01: password authentication failed for user "a""b"`, `a"b`, true, true},
		{`2026-11-20T10:00:02Z 2026-11-20 10:00:02.000 UTC [79] LOG:  connection authorized: user=locky database=postgres application_name=x`, "locky", false, true},
		{`2026-11-20T10:00:02Z 2026-11-20 10:00:02.000 UTC [80] LOG:  connection authorized: user=locky database=postgres`, "locky", false, true},
		{`2026-11-20T10:00:03Z 2026-11-20 10:00:03.000 UTC [81] LOG:  replication connection authorized: user=replicator`, "", false, false},
		{`2026-11-20T10:00:04Z 2026-11-20 10:00:04.000 UTC [82] FATAL:  role "locky" is not permitted to log in`, "", false, false},
		{`2026-11-20T10:00:04Z 2026-11-20 10:00:04.000 UTC [82] DETAIL:  Connection matched pg_hba.conf line 1`, "", false, false},
		{`not a timestamp FATAL:  password authentication failed for user "x"`, "", false, false},
	} {
		ev, ok := parseSignInLine(tt.line)
		if ok != tt.ok || (ok && (ev.role != tt.role || ev.failed != tt.failed || ev.at.IsZero())) {
			t.Errorf("parseSignInLine(%q) = %+v, %v; want role %q failed %v ok %v", tt.line, ev, ok, tt.role, tt.failed, tt.ok)
		}
	}
}

func TestLockoutOf(t *testing.T) {
	for _, tt := range []struct {
		cfg  []string
		want int64
	}{
		{[]string{pgAllowedFailedSetting + "=3", pgFailedCheckSetting + "=on", "search_path=x"}, 3},
		{[]string{pgAllowedFailedSetting + "=3"}, 0},
		{[]string{pgFailedCheckSetting + "=on"}, 0},
		{nil, 0},
	} {
		if got := lockoutOf(settingsMap(tt.cfg)); got != tt.want {
			t.Errorf("lockoutOf(%v) = %d, want %d", tt.cfg, got, tt.want)
		}
	}
}

// A PostgreSQL server whose log CloudBurrow reads takes the lock; one
// whose log it cannot read answers 501; a negative count is 400.
func TestCheckUserPolicyPostgresLockout(t *testing.T) {
	n, yes := int64(3), true
	in := &userPolicyIn{AllowedFailedAttempts: &n, EnableFailedAttemptsCheck: &yes}
	var e *apiError
	if _, err := checkUserPolicy(&Postgres{Name: "pg", Version: "POSTGRES_17"}, in); !errors.As(err, &e) || e.code != http.StatusNotImplemented {
		t.Errorf("without a log: %v, want 501", err)
	}
	logged := &Postgres{Name: "pg", Version: "POSTGRES_17",
		SignInLog: func(context.Context, time.Time) (io.ReadCloser, error) { return nil, io.EOF }}
	if _, err := checkUserPolicy(logged, in); err != nil {
		t.Errorf("with a log: %v", err)
	}
	neg := int64(-1)
	if _, err := checkUserPolicy(logged, &userPolicyIn{AllowedFailedAttempts: &neg}); !errors.As(err, &e) || e.code != http.StatusBadRequest {
		t.Errorf("allowedFailedAttempts -1: %v, want 400", err)
	}
}
