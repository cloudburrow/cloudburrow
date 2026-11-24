//go:build compat

package compat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLMySQLManagedConnectionPooling (#2141), through the official
// Admin API client (sqladmin/v1) and the official Cloud SQL Go connector
// for MySQL (cloudsqlconn): Cloud SQL for MySQL's Managed Connection
// Pooling is "a server-side thread pool", in which "each open client
// connection still maintains a dedicated server session", and
// max_pool_size "controls the amount of concurrency"
// (https://docs.cloud.google.com/sql/docs/mysql/managed-connection-pooling).
// An Enterprise Plus MySQL 8.4 instance with max_pool_size 2: eight
// clients at once each have a session of their own (eight connection
// IDs), and the server runs at most two of their statements at a time,
// measured by the server's own clock (SYSDATE at each statement's start
// and end); thread_cache_size is 0. The Enterprise edition, a flag MySQL's
// pooling does not have and max_pool_size 0 are refused 400. Turned off,
// the eight statements run together.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.users.insert
func TestCloudSQLMySQLManagedConnectionPooling(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvSQLAdmin) == "" {
		t.Skip("needs the Cloud SQL Admin API")
	}
	svc := sqlAdminService(t, h)
	p := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	name := fmt.Sprintf("ct-mypool-%d", time.Now().UnixNano()%1e6)
	flag := func(n, v string) []*sqladmin.ConnectionPoolFlags {
		return []*sqladmin.ConnectionPoolFlags{{Name: n, Value: v}}
	}
	want400 := func(what string, err error) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
			t.Errorf("%s: %v; want 400", what, err)
		}
	}
	inst := func(edition, tier string, pc *sqladmin.ConnectionPoolConfig) *sqladmin.DatabaseInstance {
		return &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_8_4", Region: "us-central1", RootPassword: "Root-pw-2141",
			Settings: &sqladmin.Settings{Tier: tier, Edition: edition, ConnectionPoolConfig: pc}}
	}
	_, err := svc.Instances.Insert(p, inst("ENTERPRISE", "db-custom-2-7680", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true})).Do()
	want400("pooling on an Enterprise edition instance", err)
	_, err = svc.Instances.Insert(p, inst("ENTERPRISE_PLUS", "db-perf-optimized-N-2", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flag("pool_mode", "transaction")})).Do()
	want400("pool_mode, PostgreSQL's flag, on MySQL", err)
	_, err = svc.Instances.Insert(p, inst("ENTERPRISE_PLUS", "db-perf-optimized-N-2", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flag("max_pool_size", "0")})).Do()
	want400("max_pool_size 0", err)

	op, err := svc.Instances.Insert(p, inst("ENTERPRISE_PLUS", "db-perf-optimized-N-2", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flag("max_pool_size", "2")})).Do()
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
	if pc := got.Settings.ConnectionPoolConfig; pc == nil || !pc.ConnectionPoolingEnabled || len(pc.Flags) != 1 || pc.Flags[0].Value != "2" {
		t.Fatalf("connectionPoolConfig %+v; want enabled, max_pool_size 2", pc)
	}
	const user, password = "pooluser", "Pool-pw-2141"
	uop, err := svc.Users.Insert(p, name, &sqladmin.User{Name: user, Password: password}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, p, uop)

	cleanup, err := cloudsqlmysql.RegisterDriver("cloudsql-mysql-pool", connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	dsn := fmt.Sprintf("%s:%s@cloudsql-mysql-pool(%s:us-central1:%s)/?parseTime=true", user, password, p, name)
	// clients opens n sessions at once through the connector, runs one
	// half-second statement on each at once, and answers the sessions'
	// connection IDs and the most statements the server ran at a time.
	clients := func(t *testing.T, n int) (ids map[int64]bool, most int) {
		t.Helper()
		db, err := sql.Open("cloudsql-mysql-pool", dsn)
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
		type span struct{ from, to time.Time }
		var mu sync.Mutex
		var spans []span
		ids = map[int64]bool{}
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for _, c := range conns {
			wg.Add(1)
			go func(c *sql.Conn) {
				defer wg.Done()
				var id int64
				var from, to time.Time
				var slept int
				if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID(), SYSDATE(6), SLEEP(0.5), SYSDATE(6)").Scan(&id, &from, &slept, &to); err != nil {
					errs <- err
					return
				}
				mu.Lock()
				ids[id] = true
				spans = append(spans, span{from, to})
				mu.Unlock()
			}(c)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a client's statement: %v", err)
		}
		// The most spans open at once, by the server's clock.
		type edge struct {
			at  time.Time
			add int
		}
		var edges []edge
		for _, s := range spans {
			edges = append(edges, edge{s.from, 1}, edge{s.to, -1})
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].at.Equal(edges[j].at) {
				return edges[i].add < edges[j].add
			}
			return edges[i].at.Before(edges[j].at)
		})
		open := 0
		for _, e := range edges {
			open += e.add
			most = max(most, open)
		}
		return ids, most
	}
	t.Run("pooled", func(t *testing.T) {
		ids, most := clients(t, 8)
		if len(ids) != 8 {
			t.Errorf("8 clients had %d server sessions; want 8, a dedicated session each", len(ids))
		}
		if most < 1 || most > 2 {
			t.Errorf("the server ran %d statements at once; want at most 2 (max_pool_size)", most)
		}
		db, err := sql.Open("cloudsql-mysql-pool", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var cache int
		if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.thread_cache_size").Scan(&cache); err != nil || cache != 0 {
			t.Errorf("thread_cache_size %d, %v; want 0", cache, err)
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
	if pc := got.Settings.ConnectionPoolConfig; pc != nil && pc.ConnectionPoolingEnabled {
		t.Errorf("connectionPoolConfig turned off: %+v", pc)
	}
	t.Run("unpooled", func(t *testing.T) {
		_, most := clients(t, 8)
		if most <= 2 {
			t.Errorf("without the thread pool the server ran at most %d of 8 statements at once; want more than 2", most)
		}
	})
}
