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

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// covers: sql.users.insert, sql.users.list, sql.users.get, sql.users.delete, sql.instances.executeSql, cloudidentity.groups.memberships.delete
//
// TestCloudSQLIAMGroupAuthentication (#1524), with the official
// google.golang.org/api/sqladmin/v1, cloudidentity/v1, iam/credentials/apiv1
// and resourcemanager clients and cloud.google.com/go/cloudsqlconn (pgx v5
// and MySQL drivers), as Google's IAM group authentication guides
// (docs.cloud.google.com/sql/docs/{postgres,mysql}/add-manage-iam-users and
// .../iam-authentication, read 2026-11-25) document it, on PostgreSQL and
// MySQL 8.4 with the IAM flag on:
//   - a Cloud Identity group added as CLOUD_IAM_GROUP is listed ACTIVE; a
//     duplicate is 409, one with a password and a
//     CLOUD_IAM_GROUP_SERVICE_ACCOUNT are 400;
//   - a service account in the group through a nested group, the group
//     holding roles/cloudsql.instanceUser, signs in with its own token
//     (first as the password, then WithIAMAuthN), its account then listed
//     as CLOUD_IAM_GROUP_SERVICE_ACCOUNT, and reads a table granted to the
//     group only; executeSql with autoIamAuthn runs as it too;
//   - an account outside the group is refused; the member cannot be added
//     as an individual CLOUD_IAM_SERVICE_ACCOUNT or deleted through the API;
//   - when the nesting is removed in Cloud Identity the member is refused
//     and its account is gone from users.list; back in the group it signs
//     in again; when the group is removed from the instance its account
//     goes too;
//   - a group deleted from Cloud Identity is listed INACTIVE.
func TestCloudSQLIAMGroupAuthentication(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	ci := cloudIdentity(t, h, shardEndpoint(h, envSQLResourceManager, EnvResourceManager))
	project := iamProject(t, h, "sqlgrp-")
	member := newIAMPrincipalIn(t, h, envSQLResourceManager, project, "sqlgm")
	outsider := newIAMPrincipalIn(t, h, envSQLResourceManager, project, "sqlgx", sqlInstanceUserRole)
	sfx := time.Now().UnixNano() % 1e6

	type engine struct {
		name, version, flag, admin string
		mysql                      bool
	}
	var engines []engine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, engine{"postgres", "POSTGRES_17", "cloudsql.iam_authentication", "postgres", false})
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, engine{"mysql", "MYSQL_8_4", "cloudsql_iam_authentication", "root", true})
	}
	dbName := func(email string, mysql bool) string {
		if mysql {
			n, _, _ := strings.Cut(email, "@")
			return n
		}
		return strings.TrimSuffix(email, ".gserviceaccount.com")
	}
	for _, eng := range engines {
		t.Run(eng.name, func(t *testing.T) {
			h := h.For(t)
			// The groups: the member is in inner, inner in outer, and
			// outer, which holds roles/cloudsql.instanceUser, is the
			// instance's.
			outer, inner := groupFixture(t, ci, fmt.Sprintf("sqlg-o%s-%d", eng.name[:2], sfx), fmt.Sprintf("sqlg-i%s-%d", eng.name[:2], sfx))
			addMember(t, ci, inner.Name, member.email)
			nested := addMember(t, ci, outer.Name, inner.GroupKey.Id)
			change := projectRoles(t, h, project, "group:"+outer.GroupKey.Id)
			change(sqlInstanceUserRole, true)
			t.Cleanup(func() { change(sqlInstanceUserRole, false) })

			inst := fmt.Sprintf("grp-%s-%d", eng.name[:2], sfx)
			in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: eng.version,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataApiAccess: "ALLOW_DATA_API",
					DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: eng.flag, Value: "on"}}}})
			insert := func(u *sqladmin.User) error {
				op, err := svc.Users.Insert(project, inst, u).Do()
				if err == nil {
					waitSQLOp(t, svc, project, op)
				}
				return err
			}
			group := outer.GroupKey.Id
			if err := insert(&sqladmin.User{Name: group, Type: "CLOUD_IAM_GROUP"}); err != nil {
				t.Fatalf("users.insert of the group %s: %v", group, err)
			}
			wantHTTP(t, "the same group again", insert(&sqladmin.User{Name: group, Type: "CLOUD_IAM_GROUP"}), http.StatusConflict)
			wantHTTP(t, "a group with a password", insert(&sqladmin.User{Name: "x" + group, Type: "CLOUD_IAM_GROUP", Password: "p"}), http.StatusBadRequest)
			wantHTTP(t, "a CLOUD_IAM_GROUP_SERVICE_ACCOUNT", insert(&sqladmin.User{Name: member.email, Type: "CLOUD_IAM_GROUP_SERVICE_ACCOUNT"}), http.StatusBadRequest)

			groupUser := group
			if eng.mysql {
				groupUser, _, _ = strings.Cut(group, "@")
			}
			users := func() map[string]*sqladmin.User {
				t.Helper()
				list, err := svc.Users.List(project, inst).Do()
				if err != nil {
					t.Fatalf("users.list: %v", err)
				}
				out := map[string]*sqladmin.User{}
				for _, u := range list.Items {
					out[u.Name] = u
				}
				return out
			}
			if g := users()[groupUser]; g == nil || g.Type != "CLOUD_IAM_GROUP" || g.IamStatus != "ACTIVE" {
				t.Fatalf("users.list: the group %s is %+v; want CLOUD_IAM_GROUP, ACTIVE", groupUser, g)
			}
			if g, err := svc.Users.Get(project, inst, group).Do(); err != nil || g.Type != "CLOUD_IAM_GROUP" || g.IamStatus != "ACTIVE" {
				t.Errorf("users.get %s: %+v, %v", group, g, err)
			}
			mine := dbName(member.email, eng.mysql)
			if users()[mine] != nil {
				t.Errorf("users.list has %s before its first sign-in", mine)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			n := 0
			// open signs in through the connector as user and runs q:
			// with tok as the IAM login token (iam), or as the password.
			open := func(user, tok string, iam bool, q string) (string, error) {
				n++
				drv := fmt.Sprintf("cloudsql-grp-%s-%d-%d", eng.name, sfx, n)
				opts := connectorOptions(h)
				if iam {
					api := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "cloudburrow", Expiry: time.Now().Add(time.Hour)})
					login := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, Expiry: time.Now().Add(50 * time.Minute)})
					opts = connectorDialOptions(h, cloudsqlconn.WithIAMAuthN(), cloudsqlconn.WithIAMAuthNTokenSources(api, login))
				}
				var db *sql.DB
				var err error
				if eng.mysql {
					cleanup, rerr := cloudsqlmysql.RegisterDriver(drv, opts...)
					if rerr != nil {
						return "", rerr
					}
					defer cleanup()
					pw, extra := "", ""
					if !iam {
						pw, extra = ":"+tok, "?allowCleartextPasswords=true"
					}
					db, err = sql.Open(drv, fmt.Sprintf("%s%s@%s(%s)/app%s", user, pw, drv, in.ConnectionName, extra))
				} else {
					cleanup, rerr := pgxv5.RegisterDriver(drv, opts...)
					if rerr != nil {
						return "", rerr
					}
					defer cleanup()
					dsn := fmt.Sprintf("host=%s user=%s dbname=postgres sslmode=disable", in.ConnectionName, user)
					if !iam {
						dsn += " password=" + tok
					}
					db, err = sql.Open(drv, dsn)
				}
				if err != nil {
					return "", err
				}
				defer db.Close()
				var out string
				for _, stmt := range strings.Split(q, ";") {
					if stmt = strings.TrimSpace(stmt); stmt == "" {
						continue
					}
					if strings.HasPrefix(strings.ToUpper(stmt), "SELECT") {
						if err := db.QueryRowContext(ctx, stmt).Scan(&out); err != nil {
							return "", err
						}
					} else if _, err := db.ExecContext(ctx, stmt); err != nil {
						return "", err
					}
				}
				return out, nil
			}
			// A table only the group is granted.
			setup := `CREATE TABLE notes (msg TEXT); INSERT INTO notes VALUES ('for the group'); GRANT SELECT ON notes TO "` + group + `"`
			whoQ, wantWho := "SELECT current_user", mine
			if eng.mysql {
				op, err := svc.Databases.Insert(project, inst, &sqladmin.Database{Name: "app"}).Do()
				if err != nil {
					t.Fatalf("databases.insert app: %v", err)
				}
				waitSQLOp(t, svc, project, op)
				domain := strings.SplitN(group, "@", 2)[1]
				setup = "CREATE TABLE notes (msg TEXT); INSERT INTO notes VALUES ('for the group'); GRANT SELECT ON app.notes TO '" + groupUser + "'@'" + domain + "'"
				whoQ, wantWho = "SELECT CURRENT_USER()", mine+"@%"
			}
			if _, err := open(eng.admin, "Root-pass1", false, setup); err != nil {
				t.Fatalf("the table as %s: %v", eng.admin, err)
			}
			// The first sign-in, with the token as the password, makes
			// the member's account; the next is the connector's own.
			if who, err := open(mine, member.token, false, whoQ); err != nil || who != wantWho {
				t.Fatalf("first sign-in of the group member %s (token as password): %q, %v; want %s", mine, who, err, wantWho)
			}
			if got, err := open(mine, member.token, true, "SELECT msg FROM notes"); err != nil || got != "for the group" {
				t.Errorf("the member reads the group's table WithIAMAuthN: %q, %v", got, err)
			}
			if u := users()[mine]; u == nil || u.Type != "CLOUD_IAM_GROUP_SERVICE_ACCOUNT" {
				t.Errorf("users.list after the first sign-in: %s is %+v; want CLOUD_IAM_GROUP_SERVICE_ACCOUNT", mine, u)
			}
			// executeSql with autoIamAuthn as the member.
			callerSvc, err := sqladmin.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvSQLAdmin)+"/"),
				option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: member.token})}}))
			if err != nil {
				t.Fatal(err)
			}
			db := "postgres"
			if eng.mysql {
				db = "app"
			}
			res, err := callerSvc.Instances.ExecuteSql(project, inst, &sqladmin.ExecuteSqlPayload{AutoIamAuthn: true, Database: db,
				SqlStatement: "SELECT msg FROM notes"}).Do()
			if err != nil || res.Status != nil || len(res.Results) != 1 || len(res.Results[0].Rows) != 1 ||
				res.Results[0].Rows[0].Values[0].Value != "for the group" {
				t.Errorf("executeSql with autoIamAuthn as the group member: %+v, %v", res, err)
			}

			refused := func(what, user, tok string) {
				t.Helper()
				who, err := open(user, tok, true, whoQ)
				if err == nil {
					t.Errorf("%s: signed in as %q; want a refusal", what, who)
					return
				}
				if !strings.Contains(err.Error(), "authentication failed") && !strings.Contains(err.Error(), "Access denied") {
					t.Errorf("%s: %v; want the database's refusal", what, err)
				}
			}
			refused("an account outside the group", dbName(outsider.email, eng.mysql), outsider.token)
			wantHTTP(t, "the member as an individual CLOUD_IAM_SERVICE_ACCOUNT",
				insert(&sqladmin.User{Name: member.email, Type: "CLOUD_IAM_SERVICE_ACCOUNT"}), http.StatusBadRequest)
			del := func(name, host string) error {
				call := svc.Users.Delete(project, inst).Name(name)
				if host != "" {
					call = call.Host(host)
				}
				op, err := call.Do()
				if err == nil {
					waitSQLOp(t, svc, project, op)
				}
				return err
			}
			memberHost := ""
			if eng.mysql {
				memberHost = "%"
			}
			wantHTTP(t, "users.delete of the group member", del(mine, memberHost), http.StatusBadRequest)

			// Out of the group (the nesting removed): refused, and gone.
			if _, err := ci.Groups.Memberships.Delete(nested.Name).Do(); err != nil {
				t.Fatal(err)
			}
			refused("the member once its group left the instance's group", mine, member.token)
			if u := users()[mine]; u != nil {
				t.Errorf("users.list after the member left the group: %+v; want it removed", u)
			}
			// Back in the group, it signs in again.
			addMember(t, ci, outer.Name, inner.GroupKey.Id)
			if got, err := open(mine, member.token, true, "SELECT msg FROM notes"); err != nil || got != "for the group" {
				t.Errorf("the member back in the group: %q, %v", got, err)
			}
			// The group removed from the instance takes the member's account.
			if err := del(group, ""); err != nil {
				t.Fatalf("users.delete of the group: %v", err)
			}
			us := users()
			if us[groupUser] != nil || us[mine] != nil {
				t.Errorf("users.list after the group was removed: group %+v, member %+v; want neither", us[groupUser], us[mine])
			}
			refused("the member once its group was removed from the instance", mine, member.token)

			// A group deleted from Cloud Identity is INACTIVE.
			gone, _ := groupFixture(t, ci, fmt.Sprintf("sqlg-g%s-%d", eng.name[:2], sfx), fmt.Sprintf("sqlg-u%s-%d", eng.name[:2], sfx))
			if err := insert(&sqladmin.User{Name: gone.GroupKey.Id, Type: "CLOUD_IAM_GROUP"}); err != nil {
				t.Fatalf("users.insert of %s: %v", gone.GroupKey.Id, err)
			}
			if _, err := ci.Groups.Delete(gone.Name).Do(); err != nil {
				t.Fatal(err)
			}
			if g, err := svc.Users.Get(project, inst, gone.GroupKey.Id).Do(); err != nil || g.IamStatus != "INACTIVE" {
				t.Errorf("users.get of a group deleted from Cloud Identity: %+v, %v; want INACTIVE", g, err)
			}
		})
	}
}
