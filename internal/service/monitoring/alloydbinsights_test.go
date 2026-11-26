package monitoring

import (
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
)

// TestAlloyDBInsights2253: the latency distributions are the executions
// between two samples, per query and per tag too; lock time comes from the
// collector's samples, per query and in aggregate; wait events are
// DELTAs; per-query pg_stat_statements totals are DELTAs with no
// client_addr; and an instance whose node is its server has node/* on
// InstanceNode and NodeDatabase.
func TestAlloyDBInsights2253(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	now := t0
	hist := func(counts map[int]int64, sum float64) AlloyDBHistogram {
		h := AlloyDBHistogram{Buckets: make([]int64, AlloyDBLatencyBuckets.Count+2), Sum: sum}
		for i, c := range counts {
			h.Buckets[i] = c
			h.Count += c
		}
		return h
	}
	tags := AlloyDBTags{Application: "shop", Route: "/items"}
	base := AlloyDBInstance{Project: "p1", Location: "us-central1", Cluster: "c1", Instance: "primary", VCPUs: 2, Nodes: 1, Up: true,
		HasPod: true, CPUSeconds: 10, MemoryUsed: 1 << 30, MemoryMachine: 16 << 30, NodeID: "alloydb-x-1-2", HasNet: true, RxBytes: 100, TxBytes: 200,
		HasServer: true, MaxConnections: 100, ByState: map[string]int64{"active": 1}, HasLoad: true,
		Databases:    []AlloyDBDatabase{{Name: "shop", Backends: 1, Commits: 10}},
		Load:         []AlloyDBLoad{{User: "app", Database: "shop", ExecUs: 1000}},
		Queries:      []AlloyDBQuery{{QueryHash: "42", QueryString: "SELECT $1", User: "app", Database: "shop", ExecUs: 100, Rows: 1}},
		HasTelemetry: true,
		Latencies: []AlloyDBLatency{
			{Database: "shop", User: "app", Client: "10.0.0.7", Hist: hist(map[int]int64{20: 2}, 2000)},
			{Database: "shop", User: "app", Client: "10.0.0.7", QueryHash: "42", QueryString: "SELECT $1", Hist: hist(map[int]int64{20: 2}, 2000)},
			{Database: "shop", User: "app", Client: "10.0.0.7", TagHash: "t1", Tags: tags, Hist: hist(map[int]int64{20: 1}, 1000)}},
		Locks:      []AlloyDBLockWait{{Database: "shop", User: "app", Client: "10.0.0.7", LockType: "hw", QueryHash: "42", QueryString: "SELECT $1", TagHash: "t1", Tags: tags, Us: 1e6}},
		WaitEvents: []AlloyDBWaitEvent{{Type: "Lock", Name: "transactionid", Count: 1, Us: 1e6}}}
	src := &fakeAlloyDB{instances: []AlloyDBInstance{base}}
	b := NewAlloyDBBuiltin(src, func() time.Time { return now })
	b.Sample()
	next := base
	next.CPUSeconds, next.RxBytes, next.TxBytes = 40, 1100, 2200
	next.Databases = []AlloyDBDatabase{{Name: "shop", Backends: 1, Commits: 15}}
	next.Queries = []AlloyDBQuery{{QueryHash: "42", QueryString: "SELECT $1", User: "app", Database: "shop", ExecUs: 700, Rows: 4}}
	next.Latencies = []AlloyDBLatency{
		{Database: "shop", User: "app", Client: "10.0.0.7", Hist: hist(map[int]int64{20: 5, 30: 1}, 8000)},
		{Database: "shop", User: "app", Client: "10.0.0.7", QueryHash: "42", QueryString: "SELECT $1", Hist: hist(map[int]int64{20: 5, 30: 1}, 8000)},
		{Database: "shop", User: "app", Client: "10.0.0.7", TagHash: "t1", Tags: tags, Hist: hist(map[int]int64{20: 3}, 3000)}}
	next.Locks = []AlloyDBLockWait{{Database: "shop", User: "app", Client: "10.0.0.7", LockType: "hw", QueryHash: "42", QueryString: "SELECT $1", TagHash: "t1", Tags: tags, Us: 3e6}}
	next.WaitEvents = []AlloyDBWaitEvent{{Type: "Lock", Name: "transactionid", Count: 3, Us: 3e6}}
	src.instances = []AlloyDBInstance{next}
	now = t0.Add(time.Minute)
	b.Sample()

	series := func(metric string) []*monitoringpb.TimeSeries {
		t.Helper()
		ts, err := b.Series("p1", metric, t0.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	last := func(metric string) *monitoringpb.TimeSeries {
		t.Helper()
		ts := series(metric)
		if len(ts) != 1 {
			t.Fatalf("%s: %d series, want 1", metric, len(ts))
		}
		return ts[0]
	}
	d := last(AlloyDBLatencies).GetPoints()[0].GetValue().GetDistributionValue()
	if d.GetCount() != 4 || d.GetBucketCounts()[20] != 3 || d.GetBucketCounts()[30] != 1 || d.GetMean() != 1500 {
		t.Fatalf("aggregate latencies = %v", d)
	}
	if e := d.GetBucketOptions().GetExponentialBuckets(); e.GetNumFiniteBuckets() != 64 || e.GetGrowthFactor() != 1.4 {
		t.Fatalf("bucket options = %v", e)
	}
	q := last(AlloyDBQueryLatencies)
	if q.GetMetric().GetLabels()["query_hash"] != "42" || q.GetMetric().GetLabels()["client_addr"] != "10.0.0.7" ||
		q.GetMetric().GetLabels()["querystring"] != "SELECT $1" {
		t.Fatalf("perquery latencies labels = %v", q.GetMetric().GetLabels())
	}
	tg := last(AlloyDBTagLatencies)
	if tg.GetMetric().GetLabels()["application"] != "shop" || tg.GetMetric().GetLabels()["route"] != "/items" ||
		tg.GetPoints()[0].GetValue().GetDistributionValue().GetCount() != 2 {
		t.Fatalf("pertag latencies = %v", tg)
	}
	if v := last(AlloyDBTagExecTime).GetPoints()[0].GetValue().GetInt64Value(); v != 2000 {
		t.Fatalf("pertag execution_time = %d, want 2000", v)
	}
	for _, m := range []string{AlloyDBLockTime, AlloyDBQueryLockTime, AlloyDBTagLockTime} {
		if v := last(m).GetPoints()[0].GetValue().GetInt64Value(); v != 2e6 {
			t.Fatalf("%s = %d, want 2e6", m, v)
		}
	}
	if v := last(AlloyDBWaitCount).GetPoints()[0].GetValue().GetInt64Value(); v != 2 {
		t.Fatalf("wait_count = %d", v)
	}
	if v := last(AlloyDBWaitTime).GetPoints()[0].GetValue().GetDoubleValue(); v != 2e6 {
		t.Fatalf("wait_time = %v", v)
	}
	qe := last(AlloyDBQueryExecTime)
	if qe.GetPoints()[0].GetValue().GetInt64Value() != 600 || qe.GetMetric().GetLabels()["client_addr"] != "" {
		t.Fatalf("perquery execution_time = %v", qe)
	}
	if v := last(AlloyDBQueryRowCount).GetPoints()[0].GetValue().GetInt64Value(); v != 3 {
		t.Fatalf("perquery row_count = %d", v)
	}
	// node/*: the primary's one node.
	cpu := last(AlloyDBNodeCPU)
	if cpu.GetResource().GetType() != AlloyDBNodeResource || cpu.GetResource().GetLabels()["node_id"] != "alloydb-x-1-2" ||
		cpu.GetPoints()[0].GetValue().GetDoubleValue() != 25 {
		t.Fatalf("node cpu = %v", cpu)
	}
	if up := series(AlloyDBNodeUptime); len(up) != 1 || up[0].GetPoints()[len(up[0].GetPoints())-1].GetValue().GetDoubleValue() != 1 {
		t.Fatalf("node uptime = %v", up)
	}
	if v := last(AlloyDBNodeReceived).GetPoints()[0].GetValue().GetInt64Value(); v != 1000 {
		t.Fatalf("node received = %d", v)
	}
	nl := last(AlloyDBPrefix + "node/database/postgresql/insights/aggregate/latencies")
	if nl.GetResource().GetType() != AlloyDBNodeDBResource || nl.GetResource().GetLabels()["database"] != "shop" {
		t.Fatalf("node latencies = %v", nl)
	}
	if v := last(AlloyDBPrefix + "node/database/postgresql/committed_transactions_for_top_databases").GetPoints()[0].GetValue().GetInt64Value(); v != 5 {
		t.Fatalf("node committed = %d", v)
	}
	if ts := series(AlloyDBPrefix + "node/postgres/wait_time"); len(ts) != 1 {
		t.Fatalf("node wait_time = %v", ts)
	}
	// The descriptors are listed, with InstanceNode's resources.
	found := 0
	for _, md := range b.Descriptors("p1") {
		if md.GetType() == AlloyDBNodeCPU && md.GetMonitoredResourceTypes()[0] == AlloyDBNodeResource {
			found++
		}
		if md.GetType() == AlloyDBQueryLatencies && md.GetValueType().String() == "DISTRIBUTION" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("descriptors: found %d of 2", found)
	}
}

// TestAlloyDBNoNodeForAReadPoolOfMany2253: a read pool of several nodes is
// one server, so no node/* is published for it.
func TestAlloyDBNoNodeForAReadPoolOfMany2253(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	pool := AlloyDBInstance{Project: "p1", Location: "us-central1", Cluster: "c1", Instance: "pool", ReadPool: true, VCPUs: 2, Nodes: 3, Up: true,
		HasServer: true, Replica: true, MaxConnections: 100}
	b := NewAlloyDBBuiltin(&fakeAlloyDB{instances: []AlloyDBInstance{pool}}, func() time.Time { return now })
	b.Sample()
	for _, m := range []string{AlloyDBNodeUptime, AlloyDBNodeBackends} {
		if ts, _ := b.Series("p1", m, now.Add(-time.Hour), now); len(ts) != 0 {
			t.Fatalf("%s published for a read pool of 3 nodes: %v", m, ts)
		}
	}
}
