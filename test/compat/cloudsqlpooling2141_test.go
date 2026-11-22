//go:build compat

package compat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLManagedConnectionPooling (#2141), through the official
// Admin API client (sqladmin/v1) and the official Cloud SQL Go connector
// (cloudsqlconn, port 3307, the pooler's with pooling on): an Enterprise
// Plus PostgreSQL instance with settings.connectionPoolConfig enabled,
// pool_mode transaction, max_pool_size 2 and max_prepared_statements 100
// (pgx prepares its statements, which a transaction pooler without it
// refuses), answers poolerCount (vCPUs / 4: 2 for db-perf-optimized-N-8),
// and eight clients signed in with a user's password through the connector
// at once share at most two server connections, each transaction on one
// of them. An instance of the
// Enterprise edition, a flag Google does not have and a value out of
// range are refused 400. Turned off, the instance answers no poolerCount
// and each client has a server connection of its own.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.users.insert
func TestCloudSQLManagedConnectionPooling(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvSQLAdmin) == "" {
		t.Skip("needs the Cloud SQL Admin API")
	}
	svc := sqlAdminService(t, h)
	p := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	name := fmt.Sprintf("ct-pool-%d", time.Now().UnixNano()%1e6)
	flags := func(kv ...string) []*sqladmin.ConnectionPoolFlags {
		var out []*sqladmin.ConnectionPoolFlags
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, &sqladmin.ConnectionPoolFlags{Name: kv[i], Value: kv[i+1]})
		}
		return out
	}
	want400 := func(what string, err error) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
			t.Errorf("%s: %v; want 400", what, err)
		}
	}
	inst := func(edition string, pc *sqladmin.ConnectionPoolConfig) *sqladmin.DatabaseInstance {
		return &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-central1", RootPassword: "Root-pw-2141",
			Settings: &sqladmin.Settings{Tier: "db-perf-optimized-N-8", Edition: edition, ConnectionPoolConfig: pc}}
	}
	_, err := svc.Instances.Insert(p, inst("ENTERPRISE", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true})).Do()
	want400("pooling on an Enterprise edition instance", err)
	_, err = svc.Instances.Insert(p, inst("ENTERPRISE_PLUS", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flags("max_pool", "2")})).Do()
	want400("a flag Managed Connection Pooling does not have", err)
	_, err = svc.Instances.Insert(p, inst("ENTERPRISE_PLUS", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flags("query_wait_timeout", "2147484")})).Do()
	want400("query_wait_timeout above 2147483", err)

	op, err := svc.Instances.Insert(p, inst("ENTERPRISE_PLUS", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flags("pool_mode", "transaction", "max_pool_size", "2", "max_prepared_statements", "100")})).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(p, name).Do(); err == nil {
			waitSQLOp(t, svc, p, d)
		}
	})
	waitSQLOp(t, svc, p, op)
	got, err := svc.Instances.Get(p, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if pc := got.Settings.ConnectionPoolConfig; pc == nil || !pc.ConnectionPoolingEnabled || pc.PoolerCount != 2 || len(pc.Flags) != 3 {
		t.Fatalf("connectionPoolConfig %+v; want enabled, its three flags, poolerCount 2", pc)
	}
	const user, password = "pooluser", "Pool-pw-2141"
	uop, err := svc.Users.Insert(p, name, &sqladmin.User{Name: user, Password: password}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, p, uop)

	cleanup, err := pgxv5.RegisterDriver("cloudsql-postgres-pool", connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	dsn := fmt.Sprintf("host=%s:us-central1:%s user=%s password=%s dbname=postgres sslmode=disable", p, name, user, password)
	// clients opens n connections at once through the connector, runs
	// rounds transactions on each, and answers the server backends they
	// were served by and the most server connections the user had.
	clients := func(t *testing.T, n, rounds int) (pids map[int]bool, most int) {
		t.Helper()
		db, err := sql.Open("cloudsql-postgres-pool", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(n)
		db.SetMaxIdleConns(n)
		conns := make([]*sql.Conn, n)
		for i := range conns {
			if conns[i], err = db.Conn(ctx); err != nil {
				t.Fatalf("client %d: %v", i, err)
			}
			defer conns[i].Close()
		}
		var mu sync.Mutex
		pids = map[int]bool{}
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for _, c := range conns {
			wg.Add(1)
			go func(c *sql.Conn) {
				defer wg.Done()
				for r := 0; r < rounds; r++ {
					tx, err := c.BeginTx(ctx, nil)
					if err != nil {
						errs <- err
						return
					}
					var pid, count int
					err = tx.QueryRowContext(ctx, "SELECT pg_backend_pid(), (SELECT count(*) FROM pg_stat_activity WHERE usename = $1)", user).Scan(&pid, &count)
					_ = tx.Commit()
					if err != nil {
						errs <- err
						return
					}
					mu.Lock()
					pids[pid] = true
					most = max(most, count)
					mu.Unlock()
				}
			}(c)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a client's transaction: %v", err)
		}
		return pids, most
	}
	t.Run("pooled", func(t *testing.T) {
		pids, most := clients(t, 8, 5)
		if len(pids) < 1 || len(pids) > 2 || most > 2 {
			t.Errorf("8 clients were served by %d server backends, the user having at most %d connections; want at most 2 (max_pool_size)", len(pids), most)
		}
	})

	pop, err := svc.Instances.Patch(p, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		ConnectionPoolConfig: &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: false, ForceSendFields: []string{"ConnectionPoolingEnabled"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, p, pop)
	got, err = svc.Instances.Get(p, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if pc := got.Settings.ConnectionPoolConfig; pc != nil && (pc.ConnectionPoolingEnabled || pc.PoolerCount != 0) {
		t.Errorf("connectionPoolConfig turned off: %+v", pc)
	}
	t.Run("unpooled", func(t *testing.T) {
		pids, _ := clients(t, 4, 2)
		if len(pids) != 4 {
			t.Errorf("4 clients without the pooler were served by %d server backends; want 4", len(pids))
		}
	})
}
