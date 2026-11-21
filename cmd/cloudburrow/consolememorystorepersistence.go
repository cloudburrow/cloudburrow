package main

import (
	"context"
	"fmt"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// A Redis cluster's and a Valkey instance's persistence on its page
// (#2099). Since #1911 a cluster or instance without persistence keeps no
// data across a restart of its nodes, as Google documents, so turning
// persistence off or on is a choice the page must offer, as Google's does:
// "In the Configurations section, click the pencil icon next to
// Persistence", then "Select the No persistence option", or Memory Snapshot
// with its "Snapshot interval", or Append Only File (AOF) with its "AOF
// Fsync mode", and "Click the Update Instance button"
// (https://docs.cloud.google.com/memorystore/docs/valkey/manage-persistence,
// read 2026-11-21). Saved through UpdateCluster's or UpdateInstance's
// persistence_config.

// persistenceActionID is the page's persistence action.
const persistenceActionID = "persistence"

// persistenceIntervals are the RDB snapshot intervals, as both APIs name
// them, and their labels.
var persistenceIntervals = []string{"ONE_HOUR", "SIX_HOURS", "TWELVE_HOURS", "TWENTY_FOUR_HOURS"}

var persistenceIntervalLabels = map[string]string{"ONE_HOUR": "1 hour", "SIX_HOURS": "6 hours",
	"TWELVE_HOURS": "12 hours", "TWENTY_FOUR_HOURS": "24 hours"}

// persistenceView is a cluster's or an instance's persistence, as the
// action's defaults: the mode (DISABLED, RDB, AOF), the interval and the
// fsync mode, in the API's own enum names.
type persistenceView struct {
	mode, interval, fsync string
	// fsyncs are the API's fsync names, every second first: the cluster
	// API's EVERYSEC, ALWAYS, NO, or Valkey's EVERY_SEC, ALWAYS, NEVER.
	fsyncs []string
	// snapshot is the RDB option's name: Redis Database (RDB) or Memory
	// Snapshot.
	snapshot string
	what     string
}

func clusterPersistenceView(c *clusterpb.Cluster) persistenceView {
	pc := c.GetPersistenceConfig()
	v := persistenceView{mode: persistenceMode(pc.GetMode().String()), interval: pc.GetRdbConfig().GetRdbSnapshotPeriod().String(),
		fsync: pc.GetAofConfig().GetAppendFsync().String(), fsyncs: []string{"EVERYSEC", "ALWAYS", "NO"},
		snapshot: "Redis Database (RDB)", what: "cluster"}
	return v.normal()
}

func valkeyPersistenceView(i *memorystorepb.Instance) persistenceView {
	pc := i.GetPersistenceConfig()
	v := persistenceView{mode: persistenceMode(pc.GetMode().String()), interval: pc.GetRdbConfig().GetRdbSnapshotPeriod().String(),
		fsync: pc.GetAofConfig().GetAppendFsync().String(), fsyncs: []string{"EVERY_SEC", "ALWAYS", "NEVER"},
		snapshot: "Memory Snapshot", what: "instance"}
	return v.normal()
}

// persistenceMode is DISABLED for an unset mode, as the APIs mean it.
func persistenceMode(m string) string {
	if m == "RDB" || m == "AOF" {
		return m
	}
	return "DISABLED"
}

// normal fills an unset interval and fsync with the APIs' defaults: 24
// hours and every second.
func (v persistenceView) normal() persistenceView {
	if _, ok := persistenceIntervalLabels[v.interval]; !ok {
		v.interval = "TWENTY_FOUR_HOURS"
	}
	known := false
	for _, f := range v.fsyncs {
		known = known || f == v.fsync
	}
	if !known {
		v.fsync = v.fsyncs[0]
	}
	return v
}

// action is the page's Edit persistence, prefilled.
func (v persistenceView) action() console.Action {
	fsyncLabels := map[string]string{v.fsyncs[0]: "Every second", v.fsyncs[1]: "Always", v.fsyncs[2]: "No (the operating system decides)"}
	return console.Action{ID: persistenceActionID, Label: "Edit persistence", Title: "Persistence", Fields: []console.Field{
		{Name: "persistence", Label: "Data persistence strategy", Type: "radio", Default: v.mode, Options: []string{"DISABLED", "AOF", "RDB"},
			OptionLabels: map[string]string{"DISABLED": "No persistence", "AOF": "Append Only File (AOF)", "RDB": v.snapshot},
			Help: "Without persistence the " + v.what + " keeps no data across a restart of its nodes; with it, the data comes back from the last snapshot or the append-only file."},
		{Name: "snapshotInterval", Label: "Snapshot interval", Type: "select", Default: v.interval, ShowWhen: "persistence=RDB",
			Options: persistenceIntervals, OptionLabels: persistenceIntervalLabels},
		{Name: "appendFsync", Label: "AOF Fsync mode", Type: "select", Default: v.fsync, ShowWhen: "persistence=AOF",
			Options: v.fsyncs, OptionLabels: fsyncLabels},
	}}
}

// persistenceChoice reads the action's values: the mode, and the interval
// or fsync mode it takes, checked against the API's names.
func persistenceChoice(values map[string]string, fsyncs []string) (mode, interval, fsync string, err error) {
	mode = values["persistence"]
	switch mode {
	case "DISABLED":
		return mode, "", "", nil
	case "RDB":
		interval = values["snapshotInterval"]
		if _, ok := persistenceIntervalLabels[interval]; !ok {
			return "", "", "", fmt.Errorf("snapshot interval %q is not 1, 6, 12 or 24 hours", interval)
		}
		return mode, interval, "", nil
	case "AOF":
		fsync = values["appendFsync"]
		for _, f := range fsyncs {
			if f == fsync {
				return mode, "", fsync, nil
			}
		}
		return "", "", "", fmt.Errorf("AOF fsync mode %q is not one of %v", fsync, fsyncs)
	}
	return "", "", "", fmt.Errorf("persistence %q is not No persistence, RDB or AOF", mode)
}

// clusterPersistenceAct is a Redis cluster's Edit persistence; ok is false
// for another action.
func clusterPersistenceAct(ctx context.Context, api *redisadmin.ClusterServer, srv *redisadmin.Server, name, action string, values map[string]string) (ok bool, err error) {
	if action != persistenceActionID {
		return false, nil
	}
	mode, interval, fsync, err := persistenceChoice(values, []string{"EVERYSEC", "ALWAYS", "NO"})
	if err != nil {
		return true, err
	}
	pc := &clusterpb.ClusterPersistenceConfig{Mode: clusterpb.ClusterPersistenceConfig_PersistenceMode(clusterpb.ClusterPersistenceConfig_PersistenceMode_value[mode])}
	switch mode {
	case "RDB":
		pc.RdbConfig = &clusterpb.ClusterPersistenceConfig_RDBConfig{
			RdbSnapshotPeriod: clusterpb.ClusterPersistenceConfig_RDBConfig_SnapshotPeriod(clusterpb.ClusterPersistenceConfig_RDBConfig_SnapshotPeriod_value[interval])}
	case "AOF":
		pc.AofConfig = &clusterpb.ClusterPersistenceConfig_AOFConfig{
			AppendFsync: clusterpb.ClusterPersistenceConfig_AOFConfig_AppendFsync(clusterpb.ClusterPersistenceConfig_AOFConfig_AppendFsync_value[fsync])}
	}
	op, err := api.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"persistence_config"}},
		Cluster: &clusterpb.Cluster{Name: name, PersistenceConfig: pc}})
	if err != nil {
		return true, err
	}
	return true, waitRedisOp(ctx, srv, op.GetName())
}

// valkeyPersistenceAct is a Valkey instance's Edit persistence; ok is
// false for another action.
func valkeyPersistenceAct(ctx context.Context, api *redisadmin.MemorystoreServer, srv *redisadmin.Server, name, action string, values map[string]string) (ok bool, err error) {
	if action != persistenceActionID {
		return false, nil
	}
	mode, interval, fsync, err := persistenceChoice(values, []string{"EVERY_SEC", "ALWAYS", "NEVER"})
	if err != nil {
		return true, err
	}
	pc := &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_PersistenceMode(memorystorepb.PersistenceConfig_PersistenceMode_value[mode])}
	switch mode {
	case "RDB":
		pc.RdbConfig = &memorystorepb.PersistenceConfig_RDBConfig{
			RdbSnapshotPeriod: memorystorepb.PersistenceConfig_RDBConfig_SnapshotPeriod(memorystorepb.PersistenceConfig_RDBConfig_SnapshotPeriod_value[interval])}
	case "AOF":
		pc.AofConfig = &memorystorepb.PersistenceConfig_AOFConfig{
			AppendFsync: memorystorepb.PersistenceConfig_AOFConfig_AppendFsync(memorystorepb.PersistenceConfig_AOFConfig_AppendFsync_value[fsync])}
	}
	op, err := api.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"persistence_config"}},
		Instance: &memorystorepb.Instance{Name: name, PersistenceConfig: pc}})
	if err != nil {
		return true, err
	}
	return true, waitRedisOp(ctx, srv, op.GetName())
}

// memorystoreTabActions draws the persistence and maintenance window dialogs at the
// top of the Configuration and Maintenance tabs, where Google's console
// has them: the pencil next to Persistence in Configurations, and Edit
// next to the Maintenance card's Maintenance window (#2099). DetailActions
// offers them too.
func memorystoreTabActions(d *console.Detail, persistence console.Action, m clusterMaintenance) {
	for i := range d.Sections {
		switch d.Sections[i].ID {
		case "configuration":
			d.Sections[i].Actions = append(d.Sections[i].Actions, persistence)
		case "maintenance":
			d.Sections[i].Actions = append(d.Sections[i].Actions, m.actions()[0])
		}
	}
}
