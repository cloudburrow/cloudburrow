//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// covers: sql.users.insert, sql.users.update, sql.users.get, sql.users.list, sql.instances.insert
//
// TestCloudSQLUserPasswordPolicy (#2009), with the official
// google.golang.org/api/sqladmin/v1 and cloud.google.com/go/cloudsqlconn
// clients: a user's own password policy (User.passwordPolicy), as Google
// documents it in built-in-authentication, enforced by the server itself.
// On MySQL 8.4, allowedFailedAttempts with enableFailedAttemptsCheck locks
// the user after that many failed sign-ins (FAILED_LOGIN_ATTEMPTS), its
// right password is then refused too, users.get reports status.locked,
// and a users.update of the policy unlocks it; passwordExpirationDuration
// is reported with status.passwordExpirationTime; and users.update with
// dualPasswordType DUAL_PASSWORD (Retain current password, RETAIN CURRENT
// PASSWORD) lets the old and the new password both sign in until
// NO_DUAL_PASSWORD discards the old one. On PostgreSQL 17 the
// expiration is the role's VALID UNTIL: its password signs in until it
// expires, then is refused; enablePasswordVerification is MySQL's only,
// and dual passwords are refused; and (#2014) allowedFailedAttempts with
// enableFailedAttemptsCheck locks the user after that many failed
// sign-ins in a row (a sign-in that passes forgets them), its right
// password is then refused, users.get and list answer status.locked, and
// a users.update of the policy, or of the password, unlocks it.
func TestCloudSQLUserPasswordPolicy(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	sfx := time.Now().UnixNano() % 1e6
	var versions []string
	if os.Getenv(EnvCloudSQL) != "" {
		versions = append(versions, "POSTGRES_17")
	}
	if os.Getenv(EnvMySQL) != "" {
		versions = append(versions, "MYSQL_8_4")
	}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			h := h.For(t)
			mysql := strings.HasPrefix(version, "MYSQL")
			inst := fmt.Sprintf("upol-%s-%d", strings.ToLower(version[:2]), sfx)
			in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: version,
				Region: "us-central1", RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			n := 0
			// open signs in as user with password through the connector.
			open := func(user, password string) error {
				n++
				drv := fmt.Sprintf("cloudsql-upol-%s-%d-%d", version[:2], sfx, n)
				var db *sql.DB
				var err error
				if mysql {
					cleanup, rerr := cloudsqlmysql.RegisterDriver(drv, connectorOptions(h)...)
					if rerr != nil {
						return rerr
					}
					defer cleanup()
					db, err = sql.Open(drv, fmt.Sprintf("%s:%s@%s(%s)/", user, password, drv, in.ConnectionName))
				} else {
					cleanup, rerr := pgxv5.RegisterDriver(drv, connectorOptions(h)...)
					if rerr != nil {
						return rerr
					}
					defer cleanup()
					db, err = sql.Open(drv, fmt.Sprintf("host=%s user=%s password=%s dbname=postgres sslmode=disable",
						in.ConnectionName, user, password))
				}
				if err != nil {
					return err
				}
				defer db.Close()
				var one int
				return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
			}
			signsIn := func(what, user, password string) {
				t.Helper()
				if err := open(user, password); err != nil {
					t.Errorf("%s: %v; want a sign-in", what, err)
				}
			}
			refused := func(what, user, password, want string) {
				t.Helper()
				err := open(user, password)
				if err == nil {
					t.Errorf("%s: signed in; want a refusal", what)
				} else if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: %v; want %q", what, err, want)
				}
			}
			host := ""
			if mysql {
				host = "%"
			}
			get := func(name string) *sqladmin.User {
				t.Helper()
				call := svc.Users.Get(project, inst, name)
				if host != "" {
					call = call.Host(host)
				}
				u, err := call.Do()
				if err != nil {
					t.Fatalf("users.get %s: %v", name, err)
				}
				return u
			}
			update := func(u *sqladmin.User) error {
				call := svc.Users.Update(project, inst, u).Name(u.Name)
				if host != "" {
					call = call.Host(host)
				}
				op, err := call.Do()
				if err == nil {
					waitSQLOp(t, svc, project, op)
				}
				return err
			}
			insert := func(u *sqladmin.User) {
				t.Helper()
				op, err := svc.Users.Insert(project, inst, u).Do()
				if err != nil {
					t.Fatalf("users.insert %s: %v", u.Name, err)
				}
				waitSQLOp(t, svc, project, op)
			}

			if !mysql {
				// PostgreSQL: the expiration is the role's VALID UNTIL.
				insert(&sqladmin.User{Name: "expiring", Password: "Exp-pass-1",
					PasswordPolicy: &sqladmin.UserPasswordValidationPolicy{PasswordExpirationDuration: "8s"}})
				u := get("expiring")
				if u.PasswordPolicy == nil || u.PasswordPolicy.PasswordExpirationDuration != "8s" || u.PasswordPolicy.Status == nil {
					t.Fatalf("users.get after an 8s expiration: %+v", u.PasswordPolicy)
				}
				exp, err := time.Parse(time.RFC3339, u.PasswordPolicy.Status.PasswordExpirationTime)
				if err != nil || time.Until(exp) > 10*time.Second || time.Until(exp) < -2*time.Second {
					t.Errorf("status.passwordExpirationTime %q: want about 8s from now", u.PasswordPolicy.Status.PasswordExpirationTime)
				}
				signsIn("the password before it expires", "expiring", "Exp-pass-1")
				refused("a wrong password of a role with a policy", "expiring", "Wrong-pass-1", "authentication failed")
				time.Sleep(time.Until(exp) + 2*time.Second)
				refused("the password once it has expired", "expiring", "Exp-pass-1", "authentication failed")
				// A new password restarts the expiration.
				if err := update(&sqladmin.User{Name: "expiring", Password: "Exp-pass-2"}); err != nil {
					t.Fatal(err)
				}
				signsIn("a new password after the expiration", "expiring", "Exp-pass-2")
				_, err = svc.Users.Insert(project, inst, &sqladmin.User{Name: "verify", Password: "V-pass-1",
					PasswordPolicy: &sqladmin.UserPasswordValidationPolicy{EnablePasswordVerification: true}}).Do()
				wantHTTP(t, "enablePasswordVerification on PostgreSQL", err, http.StatusBadRequest)
				err = update(&sqladmin.User{Name: "expiring", Password: "Exp-pass-3", DualPasswordType: "DUAL_PASSWORD"})
				wantHTTP(t, "DUAL_PASSWORD on PostgreSQL", err, http.StatusBadRequest)

				// PostgreSQL: failed sign-ins lock the user (#2014).
				insert(&sqladmin.User{Name: "locky", Password: "Lock-pass-1",
					PasswordPolicy: &sqladmin.UserPasswordValidationPolicy{AllowedFailedAttempts: 2, EnableFailedAttemptsCheck: true}})
				p := get("locky").PasswordPolicy
				if p == nil || p.AllowedFailedAttempts != 2 || !p.EnableFailedAttemptsCheck || (p.Status != nil && p.Status.Locked) {
					t.Fatalf("users.get of a new user with a lock: %+v", p)
				}
				locked := func() bool {
					u := get("locky")
					return u.PasswordPolicy != nil && u.PasswordPolicy.Status != nil && u.PasswordPolicy.Status.Locked
				}
				// The lock follows the server's log: wait for it.
				waitLocked := func(what string) {
					t.Helper()
					for end := time.Now().Add(20 * time.Second); !locked(); time.Sleep(500 * time.Millisecond) {
						if time.Now().After(end) {
							t.Fatalf("%s: users.get does not answer status.locked", what)
						}
					}
				}
				signsIn("the right password", "locky", "Lock-pass-1")
				refused("a first wrong password", "locky", "Wrong-1", "password authentication failed")
				// A sign-in that passes forgets the failures before it.
				signsIn("the right password after one failure", "locky", "Lock-pass-1")
				refused("a wrong password after a sign-in", "locky", "Wrong-2", "password authentication failed")
				time.Sleep(4 * time.Second)
				if locked() {
					t.Fatal("locked after failures that a sign-in separated")
				}
				refused("a second wrong password in a row", "locky", "Wrong-3", "password authentication failed")
				waitLocked("two failed sign-ins in a row")
				refused("the right password once locked", "locky", "Lock-pass-1", "not permitted to log in")
				list, err := svc.Users.List(project, inst).Do()
				if err != nil {
					t.Fatal(err)
				}
				for _, lu := range list.Items {
					if lu.Name == "locky" && (lu.PasswordPolicy == nil || lu.PasswordPolicy.Status == nil || !lu.PasswordPolicy.Status.Locked) {
						t.Errorf("users.list: locky's policy %+v; want status.locked", lu.PasswordPolicy)
					}
				}
				// Updating the policy unlocks the user, and keeps what is not sent.
				if err := update(&sqladmin.User{Name: "locky", PasswordPolicy: &sqladmin.UserPasswordValidationPolicy{AllowedFailedAttempts: 3}}); err != nil {
					t.Fatal(err)
				}
				if u := get("locky"); u.PasswordPolicy == nil || u.PasswordPolicy.AllowedFailedAttempts != 3 || !u.PasswordPolicy.EnableFailedAttemptsCheck ||
					(u.PasswordPolicy.Status != nil && u.PasswordPolicy.Status.Locked) {
					t.Errorf("users.get after the policy's update: %+v", u.PasswordPolicy)
				}
				signsIn("the right password once unlocked", "locky", "Lock-pass-1")
				// Three in a row lock it again; a new password unlocks it.
				for i := 1; i <= 3; i++ {
					refused(fmt.Sprintf("wrong password %d of 3", i), "locky", fmt.Sprintf("Wrong-%d", 10+i), "password authentication failed")
				}
				waitLocked("three failed sign-ins in a row")
				if err := update(&sqladmin.User{Name: "locky", Password: "Lock-pass-2"}); err != nil {
					t.Fatal(err)
				}
				if locked() {
					t.Error("still locked after a new password")
				}
				signsIn("a new password unlocks", "locky", "Lock-pass-2")
				return
			}

			// MySQL: failed sign-ins lock the user.
			insert(&sqladmin.User{Name: "locky", Host: "%", Password: "Lock-pass-1",
				PasswordPolicy: &sqladmin.UserPasswordValidationPolicy{AllowedFailedAttempts: 2, EnableFailedAttemptsCheck: true,
					PasswordExpirationDuration: "172800s", EnablePasswordVerification: true}})
			u := get("locky")
			p := u.PasswordPolicy
			if p == nil || p.AllowedFailedAttempts != 2 || !p.EnableFailedAttemptsCheck || !p.EnablePasswordVerification ||
				p.PasswordExpirationDuration != "172800s" || p.Status == nil || p.Status.Locked {
				t.Fatalf("users.get of a new user with a policy: %+v (status %+v)", p, p.Status)
			}
			if exp, err := time.Parse(time.RFC3339, p.Status.PasswordExpirationTime); err != nil ||
				time.Until(exp) < 47*time.Hour || time.Until(exp) > 49*time.Hour {
				t.Errorf("status.passwordExpirationTime %q: want two days from now", p.Status.PasswordExpirationTime)
			}
			signsIn("the right password", "locky", "Lock-pass-1")
			refused("a first wrong password", "locky", "Wrong-1", "Access denied")
			refused("a second wrong password", "locky", "Wrong-2", "Account is blocked")
			refused("the right password once locked", "locky", "Lock-pass-1", "Account is blocked")
			if u := get("locky"); u.PasswordPolicy == nil || u.PasswordPolicy.Status == nil || !u.PasswordPolicy.Status.Locked {
				t.Fatalf("users.get after two failed sign-ins: %+v; want status.locked", u.PasswordPolicy)
			}
			list, err := svc.Users.List(project, inst).Do()
			if err != nil {
				t.Fatal(err)
			}
			for _, lu := range list.Items {
				if lu.Name == "locky" && (lu.PasswordPolicy == nil || lu.PasswordPolicy.Status == nil || !lu.PasswordPolicy.Status.Locked) {
					t.Errorf("users.list: locky's policy %+v; want status.locked", lu.PasswordPolicy)
				}
			}
			// Updating the policy unlocks the user, and keeps what is not sent.
			if err := update(&sqladmin.User{Name: "locky", PasswordPolicy: &sqladmin.UserPasswordValidationPolicy{AllowedFailedAttempts: 3}}); err != nil {
				t.Fatal(err)
			}
			if u := get("locky"); u.PasswordPolicy == nil || u.PasswordPolicy.AllowedFailedAttempts != 3 || !u.PasswordPolicy.EnableFailedAttemptsCheck ||
				u.PasswordPolicy.PasswordExpirationDuration != "172800s" || (u.PasswordPolicy.Status != nil && u.PasswordPolicy.Status.Locked) {
				t.Errorf("users.get after the policy's update: %+v", u.PasswordPolicy)
			}
			signsIn("the right password once unlocked", "locky", "Lock-pass-1")

			// Retain current password: the old and the new both sign in,
			// then the old one is discarded.
			insert(&sqladmin.User{Name: "dual", Host: "%", Password: "Old-pass-1"})
			if err := update(&sqladmin.User{Name: "dual", Password: "New-pass-2", DualPasswordType: "DUAL_PASSWORD"}); err != nil {
				t.Fatalf("users.update with DUAL_PASSWORD: %v", err)
			}
			if u := get("dual"); u.DualPasswordType != "DUAL_PASSWORD" {
				t.Errorf("users.get after a retained password: dualPasswordType %q, want DUAL_PASSWORD", u.DualPasswordType)
			}
			signsIn("the new password", "dual", "New-pass-2")
			signsIn("the retained old password", "dual", "Old-pass-1")
			if err := update(&sqladmin.User{Name: "dual", DualPasswordType: "NO_DUAL_PASSWORD"}); err != nil {
				t.Fatalf("users.update with NO_DUAL_PASSWORD: %v", err)
			}
			refused("the old password once discarded", "dual", "Old-pass-1", "Access denied")
			signsIn("the new password once the old is discarded", "dual", "New-pass-2")
			if u := get("dual"); u.DualPasswordType == "DUAL_PASSWORD" {
				t.Errorf("users.get after the discard: dualPasswordType %q", u.DualPasswordType)
			}
			err = update(&sqladmin.User{Name: "dual", DualPasswordType: "DUAL_PASSWORD"})
			wantHTTP(t, "DUAL_PASSWORD without a password", err, http.StatusBadRequest)
		})
	}
}
