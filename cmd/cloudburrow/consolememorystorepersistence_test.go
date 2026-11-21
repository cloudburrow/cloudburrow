package main

import (
	"testing"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func actionDefaults(a console.Action) map[string]string {
	out := map[string]string{}
	for _, f := range a.Fields {
		out[f.Name] = f.Default
	}
	return out
}

// TestMemorystorePersistenceAction (#2099): a cluster's and a Valkey
// instance's Edit persistence is prefilled from its persistence_config in
// each API's names, offers Google's options, and its values are checked.
func TestMemorystorePersistenceAction(t *testing.T) {
	c := &clusterpb.Cluster{PersistenceConfig: &clusterpb.ClusterPersistenceConfig{Mode: clusterpb.ClusterPersistenceConfig_RDB,
		RdbConfig: &clusterpb.ClusterPersistenceConfig_RDBConfig{RdbSnapshotPeriod: clusterpb.ClusterPersistenceConfig_RDBConfig_SIX_HOURS}}}
	a := clusterPersistenceView(c).action()
	if d := actionDefaults(a); a.Label != "Edit persistence" || d["persistence"] != "RDB" || d["snapshotInterval"] != "SIX_HOURS" || d["appendFsync"] != "EVERYSEC" {
		t.Errorf("a cluster's Edit persistence is %s %v; want RDB every 6 hours, EVERYSEC by default", a.Label, d)
	}
	if l := a.Fields[0].OptionLabels; l["DISABLED"] != "No persistence" || l["RDB"] != "Redis Database (RDB)" || l["AOF"] != "Append Only File (AOF)" {
		t.Errorf("a cluster's persistence options are %v", l)
	}
	if d := actionDefaults(clusterPersistenceView(&clusterpb.Cluster{}).action()); d["persistence"] != "DISABLED" || d["snapshotInterval"] != "TWENTY_FOUR_HOURS" {
		t.Errorf("a cluster without persistence_config is prefilled %v; want No persistence", d)
	}
	i := &memorystorepb.Instance{PersistenceConfig: &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_AOF,
		AofConfig: &memorystorepb.PersistenceConfig_AOFConfig{AppendFsync: memorystorepb.PersistenceConfig_AOFConfig_ALWAYS}}}
	a = valkeyPersistenceView(i).action()
	if d := actionDefaults(a); d["persistence"] != "AOF" || d["appendFsync"] != "ALWAYS" || a.Fields[0].OptionLabels["RDB"] != "Memory Snapshot" ||
		a.Fields[2].Options[0] != "EVERY_SEC" {
		t.Errorf("a Valkey instance's Edit persistence is %v %v", d, a.Fields)
	}
	for _, tc := range []struct {
		values                map[string]string
		mode, interval, fsync string
		bad                   bool
	}{
		{map[string]string{"persistence": "DISABLED", "snapshotInterval": "ONE_HOUR"}, "DISABLED", "", "", false},
		{map[string]string{"persistence": "RDB", "snapshotInterval": "ONE_HOUR"}, "RDB", "ONE_HOUR", "", false},
		{map[string]string{"persistence": "AOF", "appendFsync": "EVERY_SEC"}, "AOF", "", "EVERY_SEC", false},
		{map[string]string{"persistence": "AOF", "appendFsync": "EVERYSEC"}, "", "", "", true},
		{map[string]string{"persistence": "RDB", "snapshotInterval": "TWO_HOURS"}, "", "", "", true},
		{map[string]string{"persistence": "ALWAYS"}, "", "", "", true},
	} {
		m, iv, f, err := persistenceChoice(tc.values, []string{"EVERY_SEC", "ALWAYS", "NEVER"})
		if (err != nil) != tc.bad || m != tc.mode || iv != tc.interval || f != tc.fsync {
			t.Errorf("persistenceChoice(%v) = %q %q %q, %v", tc.values, m, iv, f, err)
		}
	}
}

// TestMemorystoreEditMaintenanceWindow (#2099): Google's Edit maintenance
// window dialog, Default or Custom with its Day and Time, prefilled from the
// window; Default removes it.
func TestMemorystoreEditMaintenanceWindow(t *testing.T) {
	m := redisClusterMaintenance(&clusterpb.Cluster{})
	a := m.actions()[0]
	if d := actionDefaults(a); a.Label != "Edit maintenance window" || d["maintenance"] != "DEFAULT" {
		t.Errorf("without a window the dialog is %s %v; want Default", a.Label, d)
	}
	m = valkeyMaintenance(&memorystorepb.Instance{MaintenancePolicy: &memorystorepb.MaintenancePolicy{WeeklyMaintenanceWindow: []*memorystorepb.WeeklyMaintenanceWindow{
		{Day: dayofweek.DayOfWeek_TUESDAY, StartTime: &timeofday.TimeOfDay{Hours: 5}}}}})
	a = m.actions()[0]
	if d := actionDefaults(a); d["maintenance"] != "CUSTOM" || d["maintenanceDay"] != "TUESDAY" || d["maintenanceHour"] != "5" {
		t.Errorf("with a Tuesday 05:00 window the dialog is prefilled %v", d)
	}
	for _, f := range a.Fields {
		if f.Section != "" || (f.Name == "maintenanceHour" && f.Label != "Time") {
			t.Errorf("dialog field %+v; want no section, the hour labelled Time", f)
		}
	}
	if _, _, has, err := clusterMaintenanceWindow(map[string]string{"maintenance": "DEFAULT", "maintenanceDay": "TUESDAY", "maintenanceHour": "5"}); has || err != nil {
		t.Errorf("Default reads a window (%v, %v)", has, err)
	}
	if d, h, has, err := clusterMaintenanceWindow(map[string]string{"maintenance": "CUSTOM", "maintenanceDay": "TUESDAY", "maintenanceHour": "5"}); !has || err != nil ||
		d != dayofweek.DayOfWeek_TUESDAY || h != 5 {
		t.Errorf("Custom Tuesday 5 reads %v %d %v %v", d, h, has, err)
	}
}
