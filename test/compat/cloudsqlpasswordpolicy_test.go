//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLPasswordValidationPolicy (#1876): an instance's
// settings.passwordValidationPolicy, as Google documents it for MySQL and
// PostgreSQL (built-in-authentication), is answered as set, a patch of one
// field keeps the others, and once enablePasswordPolicy is set users.insert
// and users.update refuse, with 400, a password shorter than minLength,
// one without COMPLEXITY_DEFAULT's four kinds of character, one with the
// username in it, and one of the last reuseInterval passwords; on
// PostgreSQL a change within passwordChangeInterval too, which MySQL does
// not take. `gcloud sql instances patch --enable-password-policy
// --password-policy-min-length` sets it, and `gcloud sql users create`
// is refused under it.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.instances.delete, sql.users.insert, sql.users.update, sql.operations.get
func TestCloudSQLPasswordValidationPolicy(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	project := g.project
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var versions []string
	if os.Getenv(EnvCloudSQL) != "" {
		versions = append(versions, "POSTGRES_17")
	}
	if os.Getenv(EnvMySQL) != "" {
		versions = append(versions, "MYSQL_8_4")
	}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			pg := strings.HasPrefix(version, "POSTGRES")
			name := "pwpolicy-" + strings.ToLower(version[:2]) + "-" + suffix
			op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: version,
				Region: "us-central1", RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
					PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{MinLength: 10, Complexity: "COMPLEXITY_DEFAULT",
						ReuseInterval: 2, DisallowUsernameSubstring: true}}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
			}()
			waitSQLOp(t, svc, project, op)
			// Not enabled yet: not checked.
			if _, err := svc.Users.Insert(project, name, &sqladmin.User{Name: "early", Password: "x"}).Do(); err != nil {
				t.Fatalf("a password before the policy is enabled: %v", err)
			}
			enable := &sqladmin.PasswordValidationPolicy{EnablePasswordPolicy: true}
			if pg {
				enable.PasswordChangeInterval = "3600s"
			} else {
				_, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
					PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{PasswordChangeInterval: "60s"}}}).Do()
				wantHTTP(t, "passwordChangeInterval on MySQL", err, 400)
			}
			op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
				PasswordValidationPolicy: enable}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			in, err := svc.Instances.Get(project, name).Do()
			if err != nil {
				t.Fatal(err)
			}
			if p := in.Settings.PasswordValidationPolicy; p == nil || !p.EnablePasswordPolicy || p.MinLength != 10 ||
				p.ReuseInterval != 2 || !p.DisallowUsernameSubstring || p.Complexity != "COMPLEXITY_DEFAULT" {
				t.Fatalf("the policy after a patch of enablePasswordPolicy: %+v", p)
			}
			refused := func(what string, err error) {
				t.Helper()
				wantHTTP(t, what, err, 400)
				if err != nil && !strings.Contains(err.Error(), "password validation policy") {
					t.Errorf("%s: %v, want the policy named", what, err)
				}
			}
			_, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "alice", Password: "Sh0rt!"}).Do()
			refused("a password shorter than minLength", err)
			_, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "alice", Password: "alllowercase1!"}).Do()
			refused("a password without uppercase", err)
			_, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "alice", Password: "XAlice-pass1"}).Do()
			refused("a password with the username", err)
			op, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "alice", Password: "Good-pass-1"}).Do()
			if err != nil {
				t.Fatalf("a password that meets the policy: %v", err)
			}
			waitSQLOp(t, svc, project, op)
			if pg {
				_, err = svc.Users.Update(project, name, &sqladmin.User{Name: "alice", Password: "Other-pass-2"}).Name("alice").Do()
				refused("a change within passwordChangeInterval", err)
				op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
					PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{PasswordChangeInterval: "0s"}}}).Do()
				if err != nil {
					t.Fatal(err)
				}
				waitSQLOp(t, svc, project, op)
			}
			update := func(pw string) error {
				call := svc.Users.Update(project, name, &sqladmin.User{Name: "alice", Password: pw}).Name("alice")
				if !pg {
					call = call.Host("%")
				}
				_, err := call.Do()
				return err
			}
			if err := update("Other-pass-2"); err != nil {
				t.Fatalf("a change that meets the policy: %v", err)
			}
			refused("one of the last two passwords", update("Good-pass-1"))
			for _, pw := range []string{"Third-pass-3", "Fourth-pass-4", "Good-pass-1"} {
				if err := update(pw); err != nil {
					t.Fatalf("update to %s: %v", pw, err)
				}
			}
			if pg {
				// gcloud sets the policy, and its users create is refused.
				g.must("sql", "instances", "patch", name, "--project", g.project, "--enable-password-policy",
					"--password-policy-min-length=14", "--quiet")
				in, err := svc.Instances.Get(project, name).Do()
				if err != nil || in.Settings.PasswordValidationPolicy == nil || in.Settings.PasswordValidationPolicy.MinLength != 14 ||
					in.Settings.PasswordValidationPolicy.ReuseInterval != 2 {
					t.Errorf("after gcloud patch: %+v %v", in.Settings.PasswordValidationPolicy, err)
				}
				if out, err := g.run(nil, "sql", "users", "create", "bob", "--instance", name, "--project", g.project,
					"--password=Short-pw-1"); err == nil || !strings.Contains(out, "password validation policy") {
					t.Errorf("gcloud sql users create under the policy: %v %s", err, out)
				}
			}
		})
	}
}
