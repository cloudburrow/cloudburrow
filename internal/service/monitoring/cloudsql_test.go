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
