package redisadmin

import (
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/timeofday"
)

// TestDueBackups: a cluster's daily automated backup is due once the
// latest start time at or before now is after its creation and its last
// automated backup, and not when the config is disabled.
func TestDueBackups(t *testing.T) {
	cfg := protoJSON(&clusterpb.AutomatedBackupConfig{AutomatedBackupMode: clusterpb.AutomatedBackupConfig_ENABLED,
		Schedule: &clusterpb.AutomatedBackupConfig_FixedFrequencySchedule_{FixedFrequencySchedule: &clusterpb.AutomatedBackupConfig_FixedFrequencySchedule{
			StartTime: &timeofday.TimeOfDay{Hours: 3}}}})
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := &clusterRecord{Name: "projects/p/locations/l/clusters/a", Created: created, AutoBackup: cfg, state: clusterpb.Cluster_ACTIVE}
	off := &clusterRecord{Name: "projects/p/locations/l/clusters/b", Created: created, state: clusterpb.Cluster_ACTIVE}
	c := &clusters{byName: map[string]*clusterRecord{r.Name: r, off.Name: off}}
	if got := c.dueBackups(created.Add(time.Hour)); len(got) != 0 {
		t.Errorf("due an hour after creation, before 03:00 = %d, want 0", len(got))
	}
	next := time.Date(2026, 10, 2, 3, 5, 0, 0, time.UTC)
	if got := c.dueBackups(next); len(got) != 1 || got[0] != r {
		t.Errorf("due at 03:05 the next day = %v, want the enabled cluster", got)
	}
	r.LastAutoBackup = next
	if got := c.dueBackups(next.Add(time.Hour)); len(got) != 0 {
		t.Errorf("due after today's backup = %d, want 0", len(got))
	}
	if _, hour, ret := autoBackupConfig(r); hour != 3 || ret != 35*24*time.Hour {
		t.Errorf("autoBackupConfig = %d, %v; want 3 and the 35-day default", hour, ret)
	}
}
