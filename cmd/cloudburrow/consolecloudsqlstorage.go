package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	sqladminapi "google.golang.org/api/sqladmin/v1"
)

// The Cloud SQL page's storage shrink actions (#1535): on a created
// primary, Storage shrink config (getDiskShrinkConfig), answered with the
// smallest size its data allows, and Shrink storage (performDiskShrink);
// on a created read replica, Reset replica size (resetReplicaSize), which
// matches its storage to its primary's.

// storageActions are the storage shrink actions an instance's page offers.
func storageActions(in *sqladminapi.DatabaseInstance) []console.Action {
	if upSQLInstance(in.Name) || in.State != "RUNNABLE" || in.InstanceType == "ON_PREMISES_INSTANCE" {
		return nil
	}
	if in.MasterInstanceName != "" {
		return []console.Action{{ID: "reset-replica-size", Label: "Reset replica size",
			Confirm: "The replica's storage size is set to its primary's."}}
	}
	size := ""
	if in.Settings != nil && in.Settings.DataDiskSizeGb > 0 {
		size = strconv.FormatInt(in.Settings.DataDiskSizeGb, 10)
	}
	return []console.Action{
		{ID: "storage-shrink-config", Label: "Storage shrink config"},
		{ID: "shrink-storage", Label: "Shrink storage", Destructive: true,
			Confirm: "The instance's storage size is decreased; its replicas keep theirs until Reset replica size.",
			Fields: []console.Field{{Name: "targetSizeGb", Label: "New size (GB)", Type: "number", Required: true,
				Help: "Smaller than the current " + size + " GB, and no smaller than Storage shrink config's minimal size."}}},
	}
}

// storageResult is Storage shrink config's answer; ok is false for any
// other action.
func storageResult(ctx context.Context, svc *sqladminapi.Service, project, name, action string) (*console.Listing, bool, error) {
	if action != "storage-shrink-config" {
		return nil, false, nil
	}
	res, err := svc.Projects.Instances.GetDiskShrinkConfig(project, name).Context(ctx).Do()
	if err != nil {
		return nil, true, sqlAPIError(err)
	}
	out := &console.Listing{Columns: []string{"Value"}, NameColumn: "Field", Noun: "fields", Total: 2, Items: []console.Resource{
		{Name: "Minimal target size (GB)", Fields: map[string]string{"Value": strconv.FormatInt(res.MinimalTargetSizeGb, 10)}},
		{Name: "Message", Fields: map[string]string{"Value": res.Message}}}}
	return out, true, nil
}

// storageAct performs Shrink storage and Reset replica size; ok is false
// for any other action.
func storageAct(ctx context.Context, svc *sqladminapi.Service, project, name, action string, v map[string]string) (bool, error) {
	var err error
	switch action {
	case "shrink-storage":
		s := strings.TrimSpace(v["targetSizeGb"])
		n, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil || n <= 0 {
			return true, fmt.Errorf("size %q is not a number of GB", s)
		}
		_, err = svc.Projects.Instances.PerformDiskShrink(project, name, &sqladminapi.PerformDiskShrinkContext{TargetSizeGb: n}).Context(ctx).Do()
	case "reset-replica-size":
		_, err = svc.Projects.Instances.ResetReplicaSize(project, name, &sqladminapi.SqlInstancesResetReplicaSizeRequest{}).Context(ctx).Do()
	default:
		return false, nil
	}
	if err != nil {
		return true, sqlAPIError(err)
	}
	return true, nil
}
