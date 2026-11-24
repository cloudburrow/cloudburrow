//go:build compat

package compat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// pgRefusedPassword is whether err is PostgreSQL's own refusal of a wrong
// password: FATAL 28P01, password authentication failed for user.
func pgRefusedPassword(err error, user string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "28P01" &&
		pe.Message == fmt.Sprintf("password authentication failed for user %q", user)
}

// myRefusedPassword is whether err is MySQL's own refusal of a wrong
// password: ERROR 1045 (28000) Access denied for user ... (using password: YES).
func myRefusedPassword(err error, user string) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1045 && strings.Contains(me.Message, "Access denied for user '"+user+"'") &&
		strings.Contains(me.Message, "(using password: YES)")
}

// pgDirect signs in to the PostgreSQL server at addr, not through any
// connector.
func pgDirect(ctx context.Context, addr, user, password, database string) (*pgx.Conn, error) {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: addr, Path: "/" + database,
		RawQuery: "sslmode=disable&connect_timeout=10"}
	return pgx.Connect(ctx, u.String())
}

// TestCloudSQLPasswordAuthentication (#2208): a built-in user's wrong
// password is refused with the server's own error, and the right one
// accepted, on every path Google refuses one: a direct TCP connection to
// the servers `up` runs, with pgx and go-sql-driver/mysql (including as
// CloudBurrow's management role, which no user can sign in as); and, for
// an instance of each database version Cloud SQL offers, the official Go
// connector (cloud.google.com/go/cloudsqlconn, the connectors'
// server-side proxy) and Google's Auth Proxy binary where
// CLOUDBURROW_TEST_CLOUD_SQL_PROXY names it. An instance created with no
// rootPassword has a postgres user that cannot sign in, as Google
// documents. Each server stores passwords as Cloud SQL's defaults say:
// PostgreSQL's password_encryption md5 before 14 and scram-sha-256 from
// 14; MySQL 8.0's users mysql_native_password, 8.4's
// caching_sha2_password.
// covers: sql.instances.insert, sql.users.insert, sql.users.update, sql.users.list, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLPasswordAuthentication(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}

	if addr := os.Getenv(EnvCloudSQL); addr != "" {
		t.Run("up-postgres-direct", func(t *testing.T) {
			h := h.For(t)
			ctx := h.Context()
			addr := h.Endpoint(EnvCloudSQL)
			password := os.Getenv(EnvCloudSQLPassword)
			if password == "" {
				t.Fatalf("%s is empty: cloudburrow env exported no PGPASSWORD", EnvCloudSQLPassword)
			}
			c, err := pgDirect(ctx, addr, "cloudburrow", password, "cloudburrow")
			if err != nil {
				t.Fatalf("the right password: %v", err)
			}
			var one int
			if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Errorf("SELECT 1: %d %v", one, err)
			}
			_ = c.Close(ctx)
			for _, try := range []struct{ user, password string }{
				{"cloudburrow", "wrong-" + password}, {"cloudburrow", ""}, {"cloudsqladmin", "guess"}, {"postgres", "guess"},
			} {
				if c, err := pgDirect(ctx, addr, try.user, try.password, "cloudburrow"); err == nil {
					_ = c.Close(ctx)
					t.Errorf("%s with %q signed in", try.user, try.password)
				} else if !pgRefusedPassword(err, try.user) {
					t.Errorf("%s with a wrong password: %v, want 28P01 password authentication failed", try.user, err)
				}
			}
		})
	}
	if os.Getenv(EnvMySQL) != "" {
		t.Run("up-mysql-direct", func(t *testing.T) {
			h := h.For(t)
			ctx := h.Context()
			open := func(password string) error {
				cfg := mysql.NewConfig()
				cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = "cloudburrow", password, "tcp", h.Endpoint(EnvMySQL), "cloudburrow"
				cfg.Timeout = 10 * time.Second
				conn, err := mysql.NewConnector(cfg)
				if err != nil {
					return err
				}
				db := sql.OpenDB(conn)
				defer db.Close()
				return db.PingContext(ctx)
			}
			if err := open(os.Getenv(EnvMySQLPassword)); err != nil {
				t.Errorf("the right password: %v", err)
			}
			if err := open("wrong"); !myRefusedPassword(err, "cloudburrow") {
				t.Errorf("a wrong password: %v, want ERROR 1045 Access denied", err)
			}
		})
	}

	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	type version struct {
		name   string
		stored string // the prefix of the stored password, or MySQL's plugin
	}
	var versions []version
	if os.Getenv(EnvCloudSQL) != "" {
		for _, v := range []string{"POSTGRES_9_6", "POSTGRES_10", "POSTGRES_11", "POSTGRES_12", "POSTGRES_13"} {
			versions = append(versions, version{v, "md5"})
		}
		for _, v := range []string{"POSTGRES_14", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17", "POSTGRES_18"} {
			versions = append(versions, version{v, "SCRAM-SHA-256$"})
		}
	}
	if os.Getenv(EnvMySQL) != "" {
		versions = append(versions, version{"MYSQL_5_6", "mysql_native_password"}, version{"MYSQL_5_7", "mysql_native_password"},
			version{"MYSQL_8_0", "mysql_native_password"}, version{"MYSQL_8_4", "caching_sha2_password"})
	}
	proxy := os.Getenv(EnvCloudSQLProxy)
	proxied := map[string]int{}
	type created struct {
		v    version
		in   *sqladmin.DatabaseInstance
		port int
	}
	var made []created
	t.Run("versions", func(t *testing.T) {
		h := h.For(t)
		for _, v := range versions {
			t.Run(v.name, func(t *testing.T) {
				h := h.For(t)
				ctx := h.Context()
				e := sqlEngine{mysql: strings.HasPrefix(v.name, "MYSQL_")}
				name := "pw-" + strings.ToLower(strings.ReplaceAll(v.name, "_", "")) + "-" + suffix
				ins := &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: v.name, RootPassword: "Root-pass1",
					Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}
				op, err := svc.Instances.Insert(project, ins).Do()
				if err != nil && (v.name == "MYSQL_5_6" || v.name == "MYSQL_5_7") && strings.Contains(err.Error(), "linux/amd64 only") {
					t.Skipf("this cluster's node does not run linux/amd64 images: %v", err)
				}
				if err != nil {
					t.Fatalf("insert: %v", err)
				}
				t.Cleanup(func() {
					if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
						waitSQLOpDone(t, svc, project, d)
					}
				})
				if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
					t.Fatalf("create: %+v", op.Error.Errors[0])
				}
				in, err := svc.Instances.Get(project, name).Do()
				if err != nil {
					t.Fatal(err)
				}
				user, db := "postgres", "postgres"
				if e.mysql {
					user, db = "root", "mysql"
				}
				// The right password, through the connector.
				right := e.open(t, h, in.ConnectionName, user, "Root-pass1", db)
				var stored string
				q := "SELECT rolpassword FROM pg_authid WHERE rolname = 'postgres'"
				if e.mysql {
					q = "SELECT plugin FROM mysql.user WHERE user = 'root' AND host = '%'"
				}
				eventually(t, "the right password", func() error { return right.QueryRow(q).Scan(&stored) })
				if !strings.HasPrefix(stored, v.stored) {
					t.Errorf("the root user's password is stored as %.20q, want %s", stored, v.stored)
				}
				// A wrong one, refused by the server.
				wrong := e.open(t, h, in.ConnectionName, user, "Wrong-pass1", db)
				err = wrong.PingContext(ctx)
				if e.mysql && !myRefusedPassword(err, user) || !e.mysql && !pgRefusedPassword(err, user) {
					t.Errorf("a wrong password through the connector: %v, want the server's refusal", err)
				}
				// A user made by users.insert, with its own password.
				uop, err := svc.Users.Insert(project, name, &sqladmin.User{Name: "app", Password: "App-pass1"}).Do()
				if err != nil {
					t.Fatal(err)
				}
				waitSQLOpDone(t, svc, project, uop)
				if err := e.open(t, h, in.ConnectionName, "app", "App-pass1", db).PingContext(ctx); err != nil {
					t.Errorf("users.insert's user with its password: %v", err)
				}
				if err := e.open(t, h, in.ConnectionName, "app", "App-pass2", db).PingContext(ctx); e.mysql && !myRefusedPassword(err, "app") || !e.mysql && !pgRefusedPassword(err, "app") {
					t.Errorf("users.insert's user with a wrong password: %v", err)
				}
				if proxy != "" {
					made = append(made, created{v: v, in: in})
				}
			})
		}
		// An instance with no rootPassword: its postgres user cannot sign in.
		if os.Getenv(EnvCloudSQL) != "" {
			t.Run("no-root-password", func(t *testing.T) {
				h := h.For(t)
				ctx := h.Context()
				name := "pw-none-" + suffix
				in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
					Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
				err := sqlEngine{}.open(t, h, in.ConnectionName, "postgres", "", "postgres").PingContext(ctx)
				if err == nil {
					t.Error("postgres with no password signed in")
				}
				op, err := svc.Users.Update(project, name, &sqladmin.User{Name: "postgres", Password: "Set-later1"}).Name("postgres").Do()
				if err != nil {
					t.Fatal(err)
				}
				waitSQLOpDone(t, svc, project, op)
				if err := (sqlEngine{}).open(t, h, in.ConnectionName, "postgres", "Set-later1", "postgres").PingContext(ctx); err != nil {
					t.Errorf("postgres with the password users.update set: %v", err)
				}
				// CloudBurrow's management role is no user of the instance.
				users, err := svc.Users.List(project, name).Do()
				if err != nil {
					t.Fatal(err)
				}
				for _, u := range users.Items {
					if u.Name == "cloudsqladmin" {
						t.Error("users.list lists cloudsqladmin")
					}
				}
				_, err = svc.Users.Update(project, name, &sqladmin.User{Name: "cloudsqladmin", Password: "Mine-now1"}).Name("cloudsqladmin").Do()
				wantHTTP(t, "users.update cloudsqladmin", err, 400)
			})
		}
	})

	if proxy == "" || len(made) == 0 {
		t.Logf("%s is not set: the Auth Proxy binary is not run", EnvCloudSQLProxy)
		return
	}
	t.Run("auth-proxy", func(t *testing.T) {
		h := h.For(t)
		ctx := h.Context()
		for i := range made {
			made[i].port = freeLocalPort(t)
			proxied[made[i].in.ConnectionName] = made[i].port
		}
		startCloudSQLProxy(t, h, proxy, proxied)
		for _, m := range made {
			addr := fmt.Sprintf("127.0.0.1:%d", m.port)
			if strings.HasPrefix(m.v.name, "MYSQL_") {
				open := func(password string) error {
					cfg := mysql.NewConfig()
					cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = "root", password, "tcp", addr, "mysql"
					cfg.AllowNativePasswords, cfg.Timeout = true, 10*time.Second
					conn, err := mysql.NewConnector(cfg)
					if err != nil {
						return err
					}
					db := sql.OpenDB(conn)
					defer db.Close()
					return db.PingContext(ctx)
				}
				if err := open("Root-pass1"); err != nil {
					t.Errorf("%s through the proxy, the right password: %v", m.v.name, err)
				}
				if err := open("Wrong-pass1"); !myRefusedPassword(err, "root") {
					t.Errorf("%s through the proxy, a wrong password: %v", m.v.name, err)
				}
				continue
			}
			c, err := pgDirect(ctx, addr, "postgres", "Root-pass1", "postgres")
			if err != nil {
				t.Errorf("%s through the proxy, the right password: %v", m.v.name, err)
			} else {
				_ = c.Close(ctx)
			}
			if _, err := pgDirect(ctx, addr, "postgres", "Wrong-pass1", "postgres"); !pgRefusedPassword(err, "postgres") {
				t.Errorf("%s through the proxy, a wrong password: %v", m.v.name, err)
			}
		}
	})
}
