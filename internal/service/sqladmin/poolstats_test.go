package sqladmin

import "testing"

// SHOW POOLS' rows are summed by database, the pooler's own left out,
// with SHOW STATS' avg_wait_time (#2163).
func TestPoolDatabases(t *testing.T) {
	pools := []map[string]string{
		{"database": "pgbouncer", "user": "pgbouncer", "cl_active": "1"},
		{"database": "app", "user": "alice", "cl_active": "2", "cl_waiting": "5", "sv_active": "2", "sv_idle": "0", "sv_used": "1"},
		{"database": "app", "user": "bob", "cl_active": "1", "cl_waiting": "0", "sv_active": "0", "sv_idle": "1", "sv_used": "0"},
		{"database": "postgres", "user": "alice", "cl_active": "0", "cl_waiting": "0", "sv_active": "0", "sv_idle": "0"},
	}
	stats := []map[string]string{{"database": "app", "avg_wait_time": "1234"}, {"database": "pgbouncer", "avg_wait_time": "9"}}
	got := poolDatabases(pools, stats)
	want := []PoolDatabase{{Database: "app", Pools: 2, ClientActive: 3, ClientWaiting: 5, ServerActive: 2, ServerIdle: 2, AvgWaitUs: 1234},
		{Database: "postgres", Pools: 1}}
	if len(got) != len(want) {
		t.Fatalf("got %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %+v; want %+v", got[i], want[i])
		}
	}
}
