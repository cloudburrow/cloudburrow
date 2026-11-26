//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"cloud.google.com/go/alloydbconn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

// alloyDBConnErr opens a pgx connection to instance through the connector,
// returning the server's error.
func alloyDBConnErr(ctx context.Context, d *alloydbconn.Dialer, instance, user, password string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("dbname=postgres sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg.User, cfg.Password = user, password
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, instance) }
	return pgx.ConnectConfig(ctx, cfg)
}

// alloyDBIAMDialerOptions are alloyDBDialer's options without its token:
// the Admin API, telemetry off, and the listener's dial redirected.
func alloyDBIAMDialerOptions(h *Harness) []alloydbconn.Option {
	opts := []alloydbconn.Option{
		alloydbconn.WithAdminAPIEndpoint("http://" + h.Endpoint(EnvAlloyDB)),
		alloydbconn.WithOptOutOfBuiltInTelemetry(),
		alloydbconn.WithHTTPClient(&http.Client{Transport: &http.Transport{}}),
	}
	if listener := os.Getenv(EnvAlloyDBConnector); listener != "" {
		opts = append(opts, alloydbconn.WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			if _, port, _ := net.SplitHostPort(addr); port != "5433" {
				return nil, fmt.Errorf("the connector dialed %s, want port 5433", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, listener)
		}))
	}
	return opts
}

// TestAlloyDBPasswordAuthentication (#2208): a built-in user's wrong
// password is refused with PostgreSQL's own error (FATAL 28P01) and the
// right one accepted, through the official Go connector
// (cloud.google.com/go/alloydbconn, the connectors' server-side proxy),
// ExecuteSql (AlloyDB Studio's call) and a direct connection to the
// instance's address where it has one of its own; CloudBurrow's management
// role is no user anyone signs in as. An IAM user signs in through the
// connector with the token of its principal (WithIAMAuthN), which the
// proxy checks and then signs the role in with a password of its own,
// while the same role is refused without one, and a token of nobody's is
// refused.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/ExecuteSql, google.cloud.alloydb.v1.AlloyDBAdmin/CreateUser
func TestAlloyDBPasswordAuthentication(t *testing.T) {
	h := New(t)
	c := alloyDBClients(t, h)["grpc"]
	ctx := alloyDBWait(t)
	name, primary := alloyDBCluster(ctx, t, h, c, "us-central1", "pwauth", nil)
	d := alloyDBDialer(t, h)
	conn := alloyDBConn(ctx, t, d, primary.GetName(), "postgres", "burrow-pw")
	var stored string
	if err := conn.QueryRow(ctx, "SELECT rolpassword FROM pg_authid WHERE rolname = 'postgres'").Scan(&stored); err != nil ||
		!strings.HasPrefix(stored, "SCRAM-SHA-256$") {
		t.Errorf("postgres's password is stored as %.20q (%v), want SCRAM-SHA-256", stored, err)
	}
	for _, try := range []struct{ user, password string }{{"postgres", "wrong-pw"}, {"postgres", ""}, {"alloydbadmin", "guess"}} {
		if c, err := alloyDBConnErr(ctx, d, primary.GetName(), try.user, try.password); err == nil {
			_ = c.Close(ctx)
			t.Errorf("%s with %q signed in through the connector", try.user, try.password)
		} else if !pgRefusedPassword(err, try.user) {
			t.Errorf("%s with a wrong password through the connector: %v, want 28P01", try.user, err)
		}
	}

	// ExecuteSql signs in with the password given.
	if _, err := c.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: primary.GetName(), Database: "postgres", User: "postgres",
		UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "wrong-pw"}, SqlStatement: "SELECT 1"}); err == nil ||
		!strings.Contains(err.Error(), `password authentication failed for user "postgres"`) {
		t.Errorf("ExecuteSql with a wrong password: %v, want the server's refusal", err)
	}
	if res, err := c.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: primary.GetName(), Database: "postgres", User: "postgres",
		UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "burrow-pw"}, SqlStatement: "SELECT 1"}); err != nil ||
		len(res.GetSqlResults()) != 1 {
		t.Errorf("ExecuteSql with the right password: %v, %v", res, err)
	}

	// A direct connection to the instance's own address.
	if alloyDBOwnAddress(primary) {
		alloyDBDirect(ctx, t, primary.GetIpAddress(), 5432, "postgres", "burrow-pw", "require")
		if _, err := alloyDBDirectErr(ctx, primary.GetIpAddress(), 5432, "postgres", "wrong-pw", "require"); !pgRefusedPassword(err, "postgres") {
			t.Errorf("a wrong password at %s: %v, want 28P01", primary.GetIpAddress(), err)
		}
	} else {
		t.Logf("%s shares the host's address %s: not reached by IP", primary.GetName(), primary.GetIpAddress())
	}

	t.Run("iam", func(t *testing.T) {
		h := h.For(t)
		project := iamProject(t, h, "adbiam-")
		p := newIAMPrincipalIn(t, h, envSQLResourceManager, project, "adbiam")
		role := strings.TrimSuffix(p.email, ".gserviceaccount.com")
		if _, err := c.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: name, UserId: p.email,
			User: &alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_IAM_USER}}); err != nil {
			t.Fatal(err)
		}
		iamDialer := func(token string) *alloydbconn.Dialer {
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, Expiry: time.Now().Add(time.Hour)})
			dd, err := alloydbconn.NewDialer(ctx, append(alloyDBIAMDialerOptions(h), alloydbconn.WithIAMAuthN(),
				alloydbconn.WithTokenSource(ts), alloydbconn.WithIAMAuthNTokenSource(ts))...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = dd.Close() })
			return dd
		}
		iamConn, err := alloyDBConnErr(ctx, iamDialer(p.token), primary.GetName(), role, "")
		if err != nil {
			t.Fatalf("WithIAMAuthN as %s: %v", role, err)
		}
		var who string
		if err := iamConn.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil || who != role {
			t.Errorf("current_user %q, %v; want %s", who, err, role)
		}
		_ = iamConn.Close(ctx)
		// Without the token, the IAM role's sign-in is the server's: refused.
		if c, err := alloyDBConnErr(ctx, d, primary.GetName(), role, "guess"); err == nil {
			_ = c.Close(ctx)
			t.Errorf("%s signed in with a password guess", role)
		} else if !strings.Contains(err.Error(), "authentication failed") {
			t.Errorf("%s with a password guess: %v", role, err)
		}
		// A token this instance never minted.
		if _, err := alloyDBConnErr(ctx, iamDialer("ya29.not-a-token"), primary.GetName(), role, ""); err == nil {
			t.Error("a made-up token signed in")
		}
	})
}
