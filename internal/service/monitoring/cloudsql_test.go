package monitoring

import (
	"testing"
	"time"
)

type fakeSQL struct{ in []SQLInstance }

func (f *fakeSQL) SQLInstances() []SQLInstance { return f.in }

// Two samples a minute apart make CPU utilization from the CPU time used
// over the reserved cores, the deltas from the counters, and the gauges
// from the readings, on cloudsql_database (#2141).
func TestSQLBuiltin(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	src := &fakeSQL{in: []SQLInstance{{Project: "p", Instance: "db", Region: "us-central1", Engine: "POSTGRES", Up: true,
		HasPod: true, CPUSeconds: 10, ReservedCores: 2, MemoryQuota: 4 << 30, MemoryUsage: 1 << 30, MemoryTotal: 2 << 30,
		RxBytes: 100, TxBytes: 50, DiskUsed: 1 << 30, DiskQuota: 10 << 30, HasServer: true,
		Backends: map[string]int64{"postgres": 3}, Commits: map[string]uint64{"postgres": 10}, Rollbacks: map[string]uint64{"postgres": 1}}}}
	b := NewSQLBuiltin(src, func() time.Time { return now })
	b.Sample()
	now = now.Add(time.Minute)
	in := src.in[0]
	in.CPUSeconds, in.RxBytes, in.TxBytes = 70, 400, 90
	in.Commits, in.Rollbacks = map[string]uint64{"postgres": 25}, map[string]uint64{"postgres": 1}
	src.in = []SQLInstance{in}
	b.Sample()
	read := func(metric string) []float64 {
		ss, err := b.Series("p", metric, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		var out []float64
		for _, s := range ss {
			if s.GetResource().GetType() != "cloudsql_database" || s.GetResource().GetLabels()["database_id"] != "p:db" {
				t.Errorf("%s resource %v", metric, s.GetResource())
			}
			for _, p := range s.GetPoints() {
				out = append(out, asFloat(p.GetValue()))
			}
		}
		return out
	}
	if got := read(SQLCPUUtilization); len(got) != 1 || got[0] != 0.5 {
		t.Errorf("cpu/utilization = %v, want [0.5]", got)
	}
	if got := read(SQLMemoryUtil); len(got) != 2 || got[1] != 0.25 {
		t.Errorf("memory/utilization = %v", got)
	}
	if got := read(SQLDiskUtil); len(got) != 2 || got[1] != 0.1 {
		t.Errorf("disk/utilization = %v", got)
	}
	if got := read(SQLReceivedBytes); len(got) != 1 || got[0] != 300 {
		t.Errorf("received_bytes_count = %v", got)
	}
	if got := read(SQLPGBackends); len(got) != 2 || got[1] != 3 {
		t.Errorf("num_backends = %v", got)
	}
	if got := read(SQLPGTransactions); len(got) != 2 || got[0]+got[1] != 15 {
		t.Errorf("transaction_count = %v", got)
	}
	if got := read(SQLUptime); len(got) != 1 || got[0] != 60 {
		t.Errorf("uptime = %v", got)
	}
	if got := read(SQLConnections); len(got) != 0 {
		t.Errorf("network/connections on PostgreSQL = %v", got)
	}
	if n := len(b.Descriptors("p")); n != len(sqlDescriptors) {
		t.Errorf("%d descriptors", n)
	}
}

// Query insights' aggregate metrics (#2141): execution and IO time are
// the server's totals, cumulative from the first sample on, starting
// again when they fall (a statistics reset); lock time accumulates the
// backends seen waiting times the time between samples, on
// cloudsql_instance_database.
func TestSQLBuiltinInsights(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	t0 := now
	src := &fakeSQL{in: []SQLInstance{{Project: "p", Instance: "db", Region: "us-central1", Engine: "POSTGRES", Up: true, HasServer: true,
		Load: []SQLLoad{{User: "u", Database: "d", ExecUs: 1000, IOReadUs: 100, IOWriteUs: 10}}}}}
	b := NewSQLBuiltin(src, func() time.Time { return now })
	b.Sample()
	now = now.Add(time.Minute)
	in := src.in[0]
	in.Load = []SQLLoad{{User: "u", Database: "d", ExecUs: 61_000_000, IOReadUs: 2_000_000, IOWriteUs: 10}}
	in.Waits = []SQLWait{{User: "u", Database: "d", LockType: "hw", Sessions: 2}}
	src.in = []SQLInstance{in}
	b.Sample()
	now = now.Add(time.Minute)
	in.Load = []SQLLoad{{User: "u", Database: "d", ExecUs: 5, IOReadUs: 0, IOWriteUs: 0}}
	src.in = []SQLInstance{in}
	b.Sample()
	read := func(metric string, label, value string) (vals []int64, starts []time.Time) {
		ss, err := b.Series("p", metric, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			r := s.GetResource()
			if r.GetType() != "cloudsql_instance_database" || r.GetLabels()["resource_id"] != "p:db" || r.GetLabels()["database"] != "d" ||
				s.GetMetric().GetLabels()["user"] != "u" || s.GetMetricKind().String() != "CUMULATIVE" {
				t.Errorf("%s: %v %v", metric, r, s.GetMetric())
			}
			if label != "" && s.GetMetric().GetLabels()[label] != value {
				continue
			}
			for _, p := range s.GetPoints() {
				vals = append(vals, p.GetValue().GetInt64Value())
				starts = append(starts, p.GetInterval().GetStartTime().AsTime())
			}
		}
		return vals, starts
	}
	vals, starts := read(SQLPGInsightsExecTime, "", "")
	if len(vals) != 3 || vals[1] != 61_000_000 || !starts[1].Equal(t0) || vals[2] != 5 || !starts[2].Equal(t0.Add(2*time.Minute)) {
		t.Errorf("execution_time %v from %v", vals, starts)
	}
	if vals, _ := read(SQLPGInsightsIOTime, "io_type", "read"); len(vals) != 3 || vals[1] != 2_000_000 {
		t.Errorf("io_time read %v", vals)
	}
	if vals, starts := read(SQLPGInsightsLockTime, "lock_type", "hw"); len(vals) != 2 || vals[0] != 120_000_000 || vals[1] != 240_000_000 ||
		!starts[0].Equal(t0) {
		t.Errorf("lock_time hw %v from %v", vals, starts)
	}
}

// Managed connection pooling's metrics (#2163): MySQL's thread pool on
// cloudsql_database (threads by status, pending connections, the
// sample's average wait in ms), PostgreSQL's pooler on
// cloudsql_instance_database by database (client and server connections
// by status, pools, average wait in us, attempts and errors cumulative).
func TestSQLBuiltinPooling(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	my := SQLInstance{Project: "p", Instance: "my", Region: "us-central1", Engine: "MYSQL", Up: true, HasServer: true,
		MySQLPool: &SQLMySQLPool{Active: 2, Idle: 0, Pending: 6, Waits: 10, WaitedUs: 50000}}
	pg := SQLInstance{Project: "p", Instance: "pg", Region: "us-central1", Engine: "POSTGRES", Up: true, HasServer: true, HasPGPool: true,
		PGPools: []SQLPGPool{{Database: "app", Pools: 1, ClientActive: 2, ClientWaiting: 6, ServerActive: 2, ServerIdle: 0, AvgWaitUs: 1500,
			Attempts: 8, Errors: 1, Counted: true}}}
	src := &fakeSQL{in: []SQLInstance{my, pg}}
	b := NewSQLBuiltin(src, func() time.Time { return now })
	b.Sample()
	now = now.Add(time.Minute)
	my.MySQLPool = &SQLMySQLPool{Active: 0, Idle: 2, Pending: 0, Waits: 30, WaitedUs: 50000 + 20*4000}
	pg.PGPools[0].Attempts, pg.PGPools[0].ClientWaiting = 12, 0
	src.in = []SQLInstance{my, pg}
	b.Sample()
	read := func(metric, status string) []int64 {
		t.Helper()
		ss, err := b.Series("p", metric, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, s := range ss {
			if status != "" && s.GetMetric().GetLabels()["status"] != status {
				continue
			}
			if r := s.GetResource(); r.GetType() == "cloudsql_instance_database" &&
				(r.GetLabels()["database"] != "app" || r.GetLabels()["resource_id"] != "p:pg") {
				t.Errorf("%s resource %v", metric, r)
			}
			for _, p := range s.GetPoints() {
				out = append(out, p.GetValue().GetInt64Value())
			}
		}
		return out
	}
	eq := func(name string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		}
	}
	eq("threads active", read(SQLMySQLPoolThreads, "active"), 2, 0)
	eq("threads idle", read(SQLMySQLPoolThreads, "idle"), 0, 2)
	eq("pending_connections", read(SQLMySQLPoolPending, ""), 6, 0)
	eq("avg_wait_time (ms)", read(SQLMySQLPoolAvgWait, ""), 5, 4)
	eq("client_connections active", read(SQLPoolClientConns, "active"), 2, 2)
	eq("client_connections waiting", read(SQLPoolClientConns, "waiting"), 6, 0)
	eq("server_connections active", read(SQLPoolServerConns, "active"), 2, 2)
	eq("num_pools", read(SQLPoolNumPools, ""), 1, 1)
	eq("client_connections_avg_wait_time", read(SQLPoolClientAvgWait, ""), 1500, 1500)
	eq("client_connections_attempts_count", read(SQLPoolClientAttempts, ""), 8, 12)
	eq("client_connections_error_count", read(SQLPoolClientErrors, ""), 1, 1)
	ss, _ := b.Series("p", SQLPoolClientAttempts, now.Add(-time.Hour), now)
	if len(ss) != 1 || ss[0].GetMetricKind().String() != "CUMULATIVE" ||
		!ss[0].GetPoints()[1].GetInterval().GetStartTime().AsTime().Equal(now.Add(-time.Minute)) {
		t.Errorf("attempts_count is not cumulative from the first sample: %v", ss)
	}
	for _, d := range b.Descriptors("p") {
		want := "cloudsql_database"
		if d.GetType() == SQLPoolNumPools || d.GetType() == SQLPoolServerConns {
			want = "cloudsql_instance_database"
		}
		if d.GetType() == SQLPoolNumPools || d.GetType() == SQLMySQLPoolThreads || d.GetType() == SQLPoolServerConns {
			if got := d.GetMonitoredResourceTypes(); len(got) != 1 || got[0] != want {
				t.Errorf("%s resource types %v, want %s", d.GetType(), got, want)
			}
		}
	}
}
