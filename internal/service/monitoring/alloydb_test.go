package monitoring

import (
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
)

type fakeAlloyDB struct {
	clusters  []AlloyDBCluster
	instances []AlloyDBInstance
}

func (f *fakeAlloyDB) AlloyDBSample() ([]AlloyDBCluster, []AlloyDBInstance) {
	return f.clusters, f.instances
}

// TestAlloyDBBuiltin2251: gauges are the sample's readings, DELTAs the
// change since the last sample (none across a reset), each on Google's
// resource, and a read pool's lag on its primary's instance.
func TestAlloyDBBuiltin2251(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	now := t0
	db := func(commits, inserted int64) AlloyDBDatabase {
		return AlloyDBDatabase{Name: "app", Backends: 3, Commits: commits, Rollbacks: 1, Inserted: inserted, Updated: 2, Deleted: 1, Live: 10, Dead: 2, HasTuples: true}
	}
	primary := AlloyDBInstance{Project: "p1", Location: "us-central1", Cluster: "c1", Instance: "primary", VCPUs: 2, Nodes: 1, Up: true,
		HasPod: true, CPUSeconds: 10, MemoryUsed: 1 << 30, MemoryMachine: 16 << 30, HasServer: true, MaxConnections: 100,
		ByState: map[string]int64{"active": 1, "idle": 2}, ByApplication: map[string]int64{"psql": 3}, XIDUtilization: 0.5,
		OldestAge: map[string]int64{"running": 4}, Databases: []AlloyDBDatabase{db(100, 50)}, HasLoad: true,
		Load:       []AlloyDBLoad{{User: "postgres", Database: "app", ExecUs: 1000, Rows: 10}},
		Statements: []AlloyDBStatementCount{{Database: "app", Operation: "SELECT", Calls: 7}}}
	pool := AlloyDBInstance{Project: "p1", Location: "us-central1", Cluster: "c1", Instance: "pool", ReadPool: true, VCPUs: 2, Nodes: 2, Up: true,
		HasServer: true, Replica: true, ReplayLagMs: 25, MaxConnections: 100}
	src := &fakeAlloyDB{clusters: []AlloyDBCluster{{Project: "p1", Location: "us-central1", Cluster: "c1", StorageBytes: 4096, HasStorage: true,
		LastBackups: map[string]time.Time{"on-demand": t0.Add(-time.Hour)}}}, instances: []AlloyDBInstance{primary, pool}}
	b := NewAlloyDBBuiltin(src, func() time.Time { return now })
	b.Sample()
	p2 := primary
	p2.CPUSeconds = 40 // 30 s of CPU over 60 s on 2 vCPUs: 25%
	p2.Databases = []AlloyDBDatabase{db(130, 55)}
	p2.Load = []AlloyDBLoad{{User: "postgres", Database: "app", ExecUs: 4000, Rows: 16}}
	p2.Statements = []AlloyDBStatementCount{{Database: "app", Operation: "SELECT", Calls: 9}}
	p2.Waits = []AlloyDBWait{{User: "postgres", Database: "app", LockType: "hw", Sessions: 1}}
	src.instances = []AlloyDBInstance{p2, pool}
	now = t0.Add(time.Minute)
	b.Sample()

	one := func(metric string) []*monitoringpb.TimeSeries {
		t.Helper()
		ts, err := b.Series("p1", metric, t0.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	// of is the primary's series of ts: the read pool's server reports
	// its own (here none of its databases changed).
	of := func(ts []*monitoringpb.TimeSeries) []*monitoringpb.TimeSeries {
		var out []*monitoringpb.TimeSeries
		for _, s := range ts {
			if s.GetResource().GetLabels()["instance_id"] == "primary" {
				out = append(out, s)
			}
		}
		return out
	}
	lastVal := func(ts *monitoringpb.TimeSeries) float64 {
		p := ts.GetPoints()[len(ts.GetPoints())-1].GetValue()
		if d, ok := p.GetValue().(*monitoringpb.TypedValue_DoubleValue); ok {
			return d.DoubleValue
		}
		return float64(p.GetInt64Value())
	}
	if ts := one(AlloyDBCPUMax); len(ts) != 1 || lastVal(ts[0]) != 25 || ts[0].GetResource().GetType() != AlloyDBInstanceResource ||
		ts[0].GetResource().GetLabels()["instance_id"] != "primary" {
		t.Errorf("cpu/maximum_utilization = %v", ts)
	}
	if ts := one(AlloyDBMemAvailable); len(ts) != 1 || lastVal(ts[0]) != float64(15<<30) {
		t.Errorf("min_available_memory = %v", ts)
	}
	if ts := one(AlloyDBConnections); len(ts) != 2 {
		t.Errorf("total_connections = %v", ts)
	} else if lastVal(ts[0])+lastVal(ts[1]) != 3 {
		t.Errorf("total_connections sum = %v", ts)
	}
	if ts := of(one(AlloyDBCommits)); len(ts) != 1 || lastVal(ts[0]) != 30 {
		t.Errorf("commit_count = %v", ts)
	}
	if ts := of(one(AlloyDBTransactions)); len(ts) != 1 || lastVal(ts[0]) != 30 {
		t.Errorf("transaction_count = %v", ts)
	}
	if ts := of(one(AlloyDBTuplesWritten)); len(ts) != 1 || lastVal(ts[0]) != 5 {
		t.Errorf("written_tuples_count = %v", ts)
	}
	if ts := one(AlloyDBExecTime); len(ts) != 1 || lastVal(ts[0]) != 3000 || ts[0].GetResource().GetLabels()["database"] != "app" ||
		ts[0].GetMetric().GetLabels()["user"] != "postgres" {
		t.Errorf("insights/aggregate/execution_time = %v", ts)
	}
	if ts := one(AlloyDBLockTime); len(ts) != 1 || lastVal(ts[0]) != 60e6 {
		t.Errorf("insights/aggregate/lock_time = %v", ts)
	}
	if ts := one(AlloyDBStatements); len(ts) != 1 || lastVal(ts[0]) != 2 || ts[0].GetMetric().GetLabels()["operation_type"] != "SELECT" {
		t.Errorf("statements_executed_count = %v", ts)
	}
	if ts := one(AlloyDBReplicationLag); len(ts) != 1 || lastVal(ts[0]) != 25 || ts[0].GetResource().GetLabels()["instance_id"] != "primary" ||
		ts[0].GetMetric().GetLabels()["replica_instance_id"] != "pool" {
		t.Errorf("replication/maximum_lag = %v", ts)
	}
	if ts := one(AlloyDBNodes); len(ts) != 4 {
		t.Errorf("postgres/instances = %v", ts)
	}
	if ts := one(AlloyDBStorage); len(ts) != 1 || lastVal(ts[0]) != 4096 || ts[0].GetResource().GetType() != AlloyDBClusterResource {
		t.Errorf("cluster/storage/usage = %v", ts)
	}
	if ts := one(AlloyDBLastBackup); len(ts) != 1 || lastVal(ts[0]) != float64(t0.Add(-time.Hour).UnixMicro()) {
		t.Errorf("cluster/last_backup_timestamp = %v", ts)
	}
	if ts := one(AlloyDBDBTuples); len(ts) != 2 {
		t.Errorf("database/postgresql/tuples = %v", ts)
	}
	// Another project's series are not this one's.
	if ts, _ := b.Series("p2", AlloyDBCPUMax, t0.Add(-time.Hour), now); len(ts) != 0 {
		t.Errorf("p2's cpu = %v", ts)
	}
	// A reset (the counters fell) records no DELTA.
	p3 := p2
	p3.Databases = []AlloyDBDatabase{db(5, 1)}
	src.instances = []AlloyDBInstance{p3, pool}
	now = t0.Add(2 * time.Minute)
	b.Sample()
	if ts := of(one(AlloyDBCommits)); len(ts) != 1 || len(ts[0].GetPoints()) != 1 {
		t.Errorf("after a reset, commit_count = %v", ts)
	}
	ds := b.Descriptors("p1")
	if len(ds) != len(alloyDBDescriptors) || ds[0].GetMonitoredResourceTypes()[0] != AlloyDBClusterResource {
		t.Errorf("descriptors = %d", len(ds))
	}
}
