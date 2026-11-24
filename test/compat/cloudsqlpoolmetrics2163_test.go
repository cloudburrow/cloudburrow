//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// envSQLMonitoring is Cloud Monitoring in the sql shards (#2163).
const envSQLMonitoring = "CLOUDBURROW_TEST_SQL_MONITORING"

// TestCloudSQLConnectionPoolingMetrics (#2163), through the official
// Admin API client (sqladmin/v1), the official Cloud SQL Go connectors
// and the official Cloud Monitoring client (monitoring/apiv3): managed
// connection pooling's metrics, as Google's metrics list names them
// (https://docs.cloud.google.com/monitoring/api/metrics_gcp_c), read
// while pooled load runs and after it ends.
//
//   - PostgreSQL (PgBouncer, max_pool_size 2, transaction mode): eight
//     clients of database app looping one-second transactions through the
//     connector make per_database/conn_pool/client_connections active plus
//     waiting 8 (at least 5 waiting), server_connections at most 2 (at
//     least 1 active), num_pools 1, client_connections_avg_wait_time above
//     half a second once the pooler's stats period has passed,
//     client_connections_error_count the wrong password's sign-ins (pgx
//     tries each address localhost resolves to) and
//     client_connections_attempts_count eight more; after the load,
//     no client waits and no server connection is active.
//   - MySQL (the thread pool, max_pool_size 2): eight sessions looping
//     one-second statements make mysql/conn_pool/threads active 2 and idle
//     0, pending_connections at least 5 and avg_wait_time above half a
//     second; after it, threads active 0 and idle 2, nothing pending.
//   - Every metric's descriptor is Google's: kind, value type, unit, its
//     labels and its monitored resource.
//
// covers: sql.instances.insert, sql.users.insert, sql.databases.insert
func TestCloudSQLConnectionPoolingMetrics(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvSQLAdmin) == "" {
		t.Skip("needs the Cloud SQL Admin API")
	}
	addr := h.Endpoint(envSQLMonitoring)
	svc := sqlAdminService(t, h)
	mc, err := monitoring.NewMetricClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	p := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	descs := []struct {
		typ, kind, vt, unit, label, resource string
	}{
		{"database/mysql/conn_pool/avg_wait_time", "GAUGE", "INT64", "ms", "", "cloudsql_database"},
		{"database/mysql/conn_pool/pending_connections", "GAUGE", "INT64", "1", "", "cloudsql_database"},
		{"database/mysql/conn_pool/threads", "GAUGE", "INT64", "1", "status", "cloudsql_database"},
		{"per_database/conn_pool/client_connections", "GAUGE", "INT64", "1", "status", "cloudsql_instance_database"},
		{"per_database/conn_pool/client_connections_attempts_count", "CUMULATIVE", "INT64", "1", "", "cloudsql_instance_database"},
		{"per_database/conn_pool/client_connections_avg_wait_time", "GAUGE", "INT64", "us", "", "cloudsql_instance_database"},
		{"per_database/conn_pool/client_connections_error_count", "CUMULATIVE", "INT64", "1", "", "cloudsql_instance_database"},
		{"per_database/conn_pool/num_pools", "GAUGE", "INT64", "1", "", "cloudsql_instance_database"},
		{"per_database/conn_pool/server_connections", "GAUGE", "INT64", "1", "status", "cloudsql_instance_database"},
	}
	for _, d := range descs {
		md, err := mc.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{
			Name: "projects/" + p + "/metricDescriptors/cloudsql.googleapis.com/" + d.typ})
		if err != nil {
			t.Errorf("descriptor %s: %v", d.typ, err)
			continue
		}
		labels := ""
		for _, l := range md.GetLabels() {
			labels += l.GetKey()
		}
		if md.GetMetricKind().String() != d.kind || md.GetValueType().String() != d.vt || md.GetUnit() != d.unit || labels != d.label ||
			len(md.GetMonitoredResourceTypes()) != 1 || md.GetMonitoredResourceTypes()[0] != d.resource {
			t.Errorf("descriptor %s: %s %s %q labels %q on %v; want %s %s %q labels %q on %s", d.typ, md.GetMetricKind(), md.GetValueType(),
				md.GetUnit(), labels, md.GetMonitoredResourceTypes(), d.kind, d.vt, d.unit, d.label, d.resource)
		}
	}

	suffix := time.Now().UnixNano() % 1e6
	pgName, myName := fmt.Sprintf("ct-pgpm-%d", suffix), fmt.Sprintf("ct-mypm-%d", suffix)
	flags := func(kv ...string) []*sqladmin.ConnectionPoolFlags {
		var out []*sqladmin.ConnectionPoolFlags
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, &sqladmin.ConnectionPoolFlags{Name: kv[i], Value: kv[i+1]})
		}
		return out
	}
	insert := func(name, version string, pc *sqladmin.ConnectionPoolConfig) *sqladmin.Operation {
		op, err := svc.Instances.Insert(p, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: version, Region: "us-central1",
			RootPassword: "Root-pw-2163", Settings: &sqladmin.Settings{Tier: "db-perf-optimized-N-2", Edition: "ENTERPRISE_PLUS",
				ConnectionPoolConfig: pc}}).Do()
		if err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		t.Cleanup(func() {
			if d, err := svc.Instances.Delete(p, name).Do(); err == nil {
				waitSQLOp(t, svc, p, d)
			}
		})
		return op
	}
	pgOp := insert(pgName, "POSTGRES_17", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: flags("pool_mode", "transaction", "max_pool_size", "2", "max_prepared_statements", "100")})
	myOp := insert(myName, "MYSQL_8_4", &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true, Flags: flags("max_pool_size", "2")})
	waitSQLOp(t, svc, p, pgOp)
	waitSQLOp(t, svc, p, myOp)
	const user, password = "pooluser", "Pool-pw-2163"
	for _, name := range []string{pgName, myName} {
		uop, err := svc.Users.Insert(p, name, &sqladmin.User{Name: user, Password: password}).Do()
		if err != nil {
			t.Fatal(err)
		}
		waitSQLOp(t, svc, p, uop)
	}
	dop, err := svc.Databases.Insert(p, pgName, &sqladmin.Database{Name: "app"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, p, dop)

	// latest reads metric's newest point of each series matching filter
	// (a sample is taken when the last is ten seconds old), by its status
	// label.
	latest := func(metric, filter string) map[string]int64 {
		t.Helper()
		now := time.Now()
		it := mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + p,
			Filter:   fmt.Sprintf(`metric.type = "cloudsql.googleapis.com/%s" AND %s`, metric, filter),
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-30 * time.Minute)), EndTime: timestamppb.New(now.Add(time.Second))},
			View:     monitoringpb.ListTimeSeriesRequest_FULL})
		out := map[string]int64{}
		for {
			ts, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListTimeSeries %s: %v", metric, err)
			}
			if len(ts.GetPoints()) > 0 {
				// Points are newest first.
				out[ts.GetMetric().GetLabels()["status"]] = ts.GetPoints()[0].GetValue().GetInt64Value()
			}
		}
		return out
	}
	pgFilter := fmt.Sprintf(`resource.type = "cloudsql_instance_database" AND resource.labels.resource_id = "%s:%s" AND resource.labels.database = "app"`, p, pgName)
	myFilter := fmt.Sprintf(`resource.type = "cloudsql_database" AND resource.labels.database_id = "%s:%s"`, p, myName)

	// load runs eight clients looping statement on db's connections until
	// stop is closed.
	load := func(db *sql.DB, statement string, stop chan struct{}, wg *sync.WaitGroup, errs chan<- error) {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(8)
		for i := 0; i < 8; i++ {
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("client %d: %v", i, err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := c.ExecContext(ctx, statement); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
	}

	cleanupPG, err := pgxv5.RegisterDriver("cloudsql-postgres-pm", connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupPG()
	cleanupMy, err := cloudsqlmysql.RegisterDriver("cloudsql-mysql-pm", connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupMy()
	pgDSN := func(pw string) string {
		return fmt.Sprintf("host=%s:us-central1:%s user=%s password=%s dbname=app sslmode=disable", p, pgName, user, pw)
	}
	pgDB, err := sql.Open("cloudsql-postgres-pm", pgDSN(password))
	if err != nil {
		t.Fatal(err)
	}
	defer pgDB.Close()
	myDB, err := sql.Open("cloudsql-mysql-pm", fmt.Sprintf("%s:%s@cloudsql-mysql-pm(%s:us-central1:%s)/", user, password, p, myName))
	if err != nil {
		t.Fatal(err)
	}
	defer myDB.Close()
	// One sign-in with a wrong password: an attempt, and an error.
	bad, err := sql.Open("cloudsql-postgres-pm", pgDSN("not-the-password"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.PingContext(ctx); err == nil {
		t.Error("a wrong password signed in through the pooler")
	}
	bad.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	load(pgDB, "SELECT pg_sleep(1)", stop, &wg, errs)
	load(myDB, "DO SLEEP(1)", stop, &wg, errs)
	stopped := false
	halt := func() {
		if !stopped {
			stopped = true
			close(stop)
			wg.Wait()
		}
	}
	defer halt()
	// PgBouncer's averages are its last stats period's (60 seconds), and
	// the thread pool's the time since the last sample: both have load
	// behind them after 75 seconds.
	time.Sleep(75 * time.Second)
	select {
	case err := <-errs:
		t.Fatalf("a client's statement: %v", err)
	default:
	}

	t.Run("PostgreSQL under load", func(t *testing.T) {
		cl := latest("per_database/conn_pool/client_connections", pgFilter)
		if cl["active"]+cl["waiting"] != 8 || cl["waiting"] < 5 {
			t.Errorf("client_connections %v; want active+waiting 8 (eight clients), at least 5 waiting for 2 server connections", cl)
		}
		sv := latest("per_database/conn_pool/server_connections", pgFilter)
		if sv["active"] < 1 || sv["active"]+sv["idle"] > 2 {
			t.Errorf("server_connections %v; want at most 2 (max_pool_size), at least 1 active", sv)
		}
		if n := latest("per_database/conn_pool/num_pools", pgFilter)[""]; n != 1 {
			t.Errorf("num_pools %d; want 1 (one user of app)", n)
		}
		if w := latest("per_database/conn_pool/client_connections_avg_wait_time", pgFilter)[""]; w < 500000 || w > 10000000 {
			t.Errorf("client_connections_avg_wait_time %d us; want 0.5 s to 10 s (8 one-second transactions on 2 connections)", w)
		}
		// pgx signs in once at each address the connector's localhost
		// resolves to (127.0.0.1, ::1) before it gives up, so the wrong
		// password is one or two attempts, each an error.
		errors := latest("per_database/conn_pool/client_connections_error_count", pgFilter)[""]
		if errors < 1 || errors > 2 {
			t.Errorf("client_connections_error_count %d; want 1 or 2 (the wrong password's sign-ins)", errors)
		}
		if n := latest("per_database/conn_pool/client_connections_attempts_count", pgFilter)[""]; n != 8+errors {
			t.Errorf("client_connections_attempts_count %d; want %d (eight clients and the wrong password's %d)", n, 8+errors, errors)
		}
	})
	t.Run("MySQL under load", func(t *testing.T) {
		th := latest("database/mysql/conn_pool/threads", myFilter)
		if th["active"] != 2 || th["idle"] != 0 {
			t.Errorf("threads %v; want active 2, idle 0 (max_pool_size 2, all busy)", th)
		}
		if n := latest("database/mysql/conn_pool/pending_connections", myFilter)[""]; n < 5 || n > 6 {
			t.Errorf("pending_connections %d; want 5 or 6 (eight sessions, two threads)", n)
		}
		if w := latest("database/mysql/conn_pool/avg_wait_time", myFilter)[""]; w < 500 || w > 10000 {
			t.Errorf("avg_wait_time %d ms; want 0.5 s to 10 s", w)
		}
	})

	halt()
	// A fresh sample after the load.
	time.Sleep(12 * time.Second)
	t.Run("after the load", func(t *testing.T) {
		cl := latest("per_database/conn_pool/client_connections", pgFilter)
		if cl["waiting"] != 0 {
			t.Errorf("client_connections %v after the load; want none waiting", cl)
		}
		if sv := latest("per_database/conn_pool/server_connections", pgFilter); sv["active"] != 0 {
			t.Errorf("server_connections %v after the load; want none active", sv)
		}
		th := latest("database/mysql/conn_pool/threads", myFilter)
		if th["active"] != 0 || th["idle"] != 2 {
			t.Errorf("threads %v after the load; want active 0, idle 2", th)
		}
		if n := latest("database/mysql/conn_pool/pending_connections", myFilter)[""]; n != 0 {
			t.Errorf("pending_connections %d after the load; want 0", n)
		}
	})
}
