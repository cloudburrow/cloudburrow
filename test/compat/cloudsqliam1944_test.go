//go:build compat

package compat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
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
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlInstanceUserRole is the role Google documents an IAM database
// sign-in needs (#1944): it holds cloudsql.instances.login.
const sqlInstanceUserRole = "roles/cloudsql.instanceUser"

// covers: sql.users.insert, sql.users.list, sql.users.get, sql.users.delete, sql.connect.generateEphemeral, sql.instances.executeSql, sql.instances.insert
//
// TestCloudSQLIAMDatabaseAuthentication (#1944), with the official
// google.golang.org/api/sqladmin/v1, cloud.google.com/go/cloudsqlconn
// (pgx v5 and MySQL drivers), iam/credentials/apiv1 and resourcemanager
// clients: on a PostgreSQL and a MySQL instance with the IAM flag on, a
// CLOUD_IAM_SERVICE_ACCOUNT user is a database user named as Google names
// it; the connector's WithIAMAuthN signs in as it with an access token
// CloudBurrow minted for the account, which holds
// roles/cloudsql.instanceUser; so does the token sent as the password
// (manual IAM sign-in); a principal without the role, a token of another
// account and a made-up token are refused, as is the principal once its
// role is taken back; executeSql with autoIamAuthn runs as the caller's
// IAM user; and generateEphemeralCert refuses a token it did not issue.
func TestCloudSQLIAMDatabaseAuthentication(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	// A project of the test's own, which Resource Manager holds: the
	// service accounts and the instances are in it.
	shardEndpoint(h, envSQLResourceManager, EnvResourceManager)
	project := iamProject(t, h, "sqliam-")
	granted := newIAMPrincipalIn(t, h, envSQLResourceManager, project, "sqliam", sqlInstanceUserRole)
	nobody := newIAMPrincipalIn(t, h, envSQLResourceManager, project, "sqlnone")
	sfx := time.Now().UnixNano() % 1e6

	type engine struct {
		name, version, flag string
		mysql               bool
	}
	var engines []engine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, engine{"postgres", "POSTGRES_17", "cloudsql.iam_authentication", false})
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, engine{"mysql", "MYSQL_8_4", "cloudsql_iam_authentication", true})
	}
	// dbName is the database user Google names for a service account.
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
			inst := fmt.Sprintf("iam-%s-%d", eng.name[:2], sfx)
			in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: eng.version,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataApiAccess: "ALLOW_DATA_API",
					DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: eng.flag, Value: "on"}}}})
			for _, p := range []*iamPrincipal{granted, nobody} {
				op, err := svc.Users.Insert(project, inst, &sqladmin.User{Name: p.email, Type: "CLOUD_IAM_SERVICE_ACCOUNT"}).Do()
				if err != nil {
					t.Fatalf("users.insert of %s: %v", p.email, err)
				}
				waitSQLOp(t, svc, project, op)
			}
			want := dbName(granted.email, eng.mysql)
			list, err := svc.Users.List(project, inst).Do()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, u := range list.Items {
				if u.Name == want {
					found = true
					if u.Type != "CLOUD_IAM_SERVICE_ACCOUNT" || (eng.mysql && u.IamEmail != granted.email) {
						t.Errorf("users.list: %s is type %s, iamEmail %q", u.Name, u.Type, u.IamEmail)
					}
				}
			}
			if !found {
				t.Fatalf("users.list has no %s", want)
			}
			if u, err := svc.Users.Get(project, inst, want).Do(); err != nil || u.Type != "CLOUD_IAM_SERVICE_ACCOUNT" {
				t.Errorf("users.get %s: %+v, %v", want, u, err)
			}
			// A password is refused for an IAM user.
			_, err = svc.Users.Insert(project, inst, &sqladmin.User{Name: "x-" + granted.email, Type: "CLOUD_IAM_SERVICE_ACCOUNT", Password: "p"}).Do()
			wantHTTP(t, "users.insert of an IAM user with a password", err, http.StatusBadRequest)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			n := 0
			// open signs in through the connector as user, with tok as
			// the IAM login token (iam) or as the password.
			open := func(user, tok string, iam bool) (string, error) {
				n++
				drv := fmt.Sprintf("cloudsql-iam-%s-%d-%d", eng.name, sfx, n)
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
					pw := ""
					extra := ""
					if !iam {
						pw, extra = ":"+tok, "?allowCleartextPasswords=true"
					}
					db, err = sql.Open(drv, fmt.Sprintf("%s%s@%s(%s)/%s", user, pw, drv, in.ConnectionName, extra))
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
				q := "SELECT current_user"
				if eng.mysql {
					q = "SELECT CURRENT_USER()"
				}
				var who string
				err = db.QueryRowContext(ctx, q).Scan(&who)
				return who, err
			}
			wantUser := want
			if eng.mysql {
				wantUser = want + "@%"
			}
			if who, err := open(want, granted.token, true); err != nil || who != wantUser {
				t.Errorf("WithIAMAuthN as %s: %q, %v; want %s", want, who, err, wantUser)
			}
			if who, err := open(want, granted.token, false); err != nil || who != wantUser {
				t.Errorf("the token as %s's password: %q, %v; want %s", want, who, err, wantUser)
			}
			refused := func(what, user, tok string, iam bool) {
				t.Helper()
				who, err := open(user, tok, iam)
				if err == nil {
					t.Errorf("%s: signed in as %q; want a refusal", what, who)
					return
				}
				if !strings.Contains(err.Error(), "authentication failed") && !strings.Contains(err.Error(), "Access denied") {
					t.Errorf("%s: %v; want the database's refusal", what, err)
				}
			}
			refused("a principal without "+sqlInstanceUserRole, dbName(nobody.email, eng.mysql), nobody.token, true)
			refused("another account's token", dbName(nobody.email, eng.mysql), granted.token, false)
			refused("a made-up token as the password", want, "ya29.not-a-token", false)

			// executeSql with autoIamAuthn, as the caller's IAM user.
			callerSvc := func(tok string) *sqladmin.Service {
				s, err := sqladmin.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvSQLAdmin)+"/"),
					option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok})}}))
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			q, db := "SELECT current_user", "postgres"
			if eng.mysql {
				// information_schema, which every account may use.
				q, db = "SELECT CURRENT_USER()", "information_schema"
			}
			res, err := callerSvc(granted.token).Instances.ExecuteSql(project, inst, &sqladmin.ExecuteSqlPayload{
				AutoIamAuthn: true, Database: db, SqlStatement: q}).Do()
			if err != nil || res.Status != nil || len(res.Results) != 1 || len(res.Results[0].Rows) != 1 ||
				res.Results[0].Rows[0].Values[0].Value != wantUser {
				t.Errorf("executeSql with autoIamAuthn: %+v, %v; want %s", res, err, wantUser)
			}
			res, err = callerSvc(nobody.token).Instances.ExecuteSql(project, inst, &sqladmin.ExecuteSqlPayload{
				AutoIamAuthn: true, Database: db, SqlStatement: "SELECT 1"}).Do()
			if err != nil || res.Status == nil || res.Status.Code != 16 {
				t.Errorf("executeSql with autoIamAuthn as a principal without the role: %+v, %v; want status 16", res, err)
			}
			_, err = callerSvc("ya29.not-a-token").Instances.ExecuteSql(project, inst, &sqladmin.ExecuteSqlPayload{
				AutoIamAuthn: true, Database: db, SqlStatement: "SELECT 1"}).Do()
			wantHTTP(t, "executeSql with autoIamAuthn and a made-up token", err, http.StatusUnauthorized)

			// generateEphemeralCert embeds only a token CloudBurrow issued.
			_, err = svc.Connect.GenerateEphemeralCert(project, inst, &sqladmin.GenerateEphemeralCertRequest{
				PublicKey: testRSAPublicKeyPEM(t), AccessToken: "ya29.not-a-token"}).Do()
			wantHTTP(t, "generateEphemeralCert with a made-up access token", err, http.StatusBadRequest)
			cert, err := svc.Connect.GenerateEphemeralCert(project, inst, &sqladmin.GenerateEphemeralCertRequest{
				PublicKey: testRSAPublicKeyPEM(t), AccessToken: granted.token}).Do()
			if err != nil || cert.EphemeralCert == nil || cert.EphemeralCert.Cert == "" {
				t.Errorf("generateEphemeralCert with a minted access token: %+v, %v", cert, err)
			}

			// Taking the role back refuses the next sign-in.
			granted.revoke(sqlInstanceUserRole)
			refused("the principal once its role is taken back", want, granted.token, true)
			granted.grant(sqlInstanceUserRole)

			op, err := svc.Users.Delete(project, inst).Name(want).Do()
			if err != nil {
				t.Fatalf("users.delete %s: %v", want, err)
			}
			waitSQLOp(t, svc, project, op)
			_, err = svc.Users.Get(project, inst, want).Do()
			var ge *googleapi.Error
			if !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
				t.Errorf("users.get after delete: %v; want 404", err)
			}
		})
	}
}

// testRSAPublicKeyPEM is a fresh RSA public key, as the connectors send
// generateEphemeralCert one.
func testRSAPublicKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}
