//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLStorageShrink (#1535): a created PostgreSQL primary with
// 30 GB of storage and a read replica of it. Through the official client,
// getDiskShrinkConfig answers the smallest size the primary's data allows
// (Cloud SQL's minimum, 10 GB, for a near-empty server) and refuses a
// replica; performDiskShrink refuses a size not smaller than the current
// one or below that minimum, and then shrinks the primary to 25 GB;
// resetReplicaSize sets the replica's storage to its primary's and refuses
// a primary. `gcloud sql instances get-storage-shrink-config` and
// `perform-storage-shrink` shrink it to 20 GB, and the console's Storage
// shrink config, Shrink storage and Reset replica size to 15 GB.
// covers: sql.projects.instances.getDiskShrinkConfig, sql.projects.instances.performDiskShrink, sql.projects.instances.resetReplicaSize, sql.instances.insert, sql.instances.get, sql.instances.delete
func TestCloudSQLStorageShrink(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	addr := consoleAddr(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	primary, replica := "shrink-p-"+suffix, "shrink-r-"+suffix
	del := func(n string) {
		if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}
	defer del(primary)
	defer del(replica)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: primary, DatabaseVersion: "POSTGRES_17",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataDiskSizeGb: 30}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	op, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: replica, MasterInstanceName: primary,
		InstanceType: "READ_REPLICA_INSTANCE", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataDiskSizeGb: 30}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	size := func(n string) int64 {
		t.Helper()
		in, err := svc.Instances.Get(project, n).Do()
		if err != nil {
			t.Fatal(err)
		}
		return in.Settings.DataDiskSizeGb
	}

	cfg, err := svc.Projects.Instances.GetDiskShrinkConfig(project, primary).Do()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "sql#getDiskShrinkConfig" || cfg.MinimalTargetSizeGb != 10 {
		t.Errorf("getDiskShrinkConfig: %+v, want minimalTargetSizeGb 10", cfg)
	}
	_, err = svc.Projects.Instances.GetDiskShrinkConfig(project, replica).Do()
	wantCode(t, "getDiskShrinkConfig of a replica", err, 400, "resetReplicaSize")
	for _, c := range []struct {
		gb   int64
		want string
	}{{30, "not smaller"}, {40, "not smaller"}, {5, "minimal target size, 10 GB"}} {
		_, err := svc.Projects.Instances.PerformDiskShrink(project, primary, &sqladmin.PerformDiskShrinkContext{TargetSizeGb: c.gb}).Do()
		wantCode(t, fmt.Sprintf("performDiskShrink to %d GB", c.gb), err, 400, c.want)
	}
	_, err = svc.Projects.Instances.PerformDiskShrink(project, replica, &sqladmin.PerformDiskShrinkContext{TargetSizeGb: 20}).Do()
	wantCode(t, "performDiskShrink of a replica", err, 400, "read replica")
	op, err = svc.Projects.Instances.PerformDiskShrink(project, primary, &sqladmin.PerformDiskShrinkContext{TargetSizeGb: 25}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if got := size(primary); got != 25 {
		t.Errorf("after performDiskShrink, dataDiskSizeGb = %d, want 25", got)
	}
	if got := size(replica); got != 30 {
		t.Errorf("the replica's dataDiskSizeGb = %d before resetReplicaSize, want 30", got)
	}
	_, err = svc.Projects.Instances.ResetReplicaSize(project, primary, &sqladmin.SqlInstancesResetReplicaSizeRequest{}).Do()
	wantCode(t, "resetReplicaSize of a primary", err, 400, "not a read replica")
	op, err = svc.Projects.Instances.ResetReplicaSize(project, replica, &sqladmin.SqlInstancesResetReplicaSizeRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if got := size(replica); got != 25 {
		t.Errorf("after resetReplicaSize, the replica's dataDiskSizeGb = %d, want 25", got)
	}

	g := newGcloudSession(t, h)
	p := "--project=" + project
	if out := strings.TrimSpace(g.must("sql", "instances", "get-storage-shrink-config", primary, p, "--format=value(minimalTargetSizeGb)")); out != "10" {
		t.Errorf("gcloud get-storage-shrink-config: %q, want 10", out)
	}
	g.must("sql", "instances", "perform-storage-shrink", primary, "--storage-size=20GB", p, "--quiet")
	if got := size(primary); got != 20 {
		t.Errorf("after gcloud perform-storage-shrink, dataDiskSizeGb = %d, want 20", got)
	}

	act := func(inst, action string, values map[string]string) string {
		t.Helper()
		code, out := consoleAct(t, addr, "cloudsql-instances", project, []string{inst}, action, values)
		if code != http.StatusOK {
			t.Fatalf("console %s on %s = %d: %s", action, inst, code, out)
		}
		return out
	}
	detail := func(inst string) string {
		t.Helper()
		code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?project="+project+"&name="+inst, "")
		if code != http.StatusOK {
			t.Fatalf("detail %s = %d: %.300s", inst, code, out)
		}
		return out
	}
	for inst, ids := range map[string][]string{primary: {"storage-shrink-config", "shrink-storage"}, replica: {"reset-replica-size"}} {
		page := detail(inst)
		for _, id := range ids {
			if !strings.Contains(page, `"id":"`+id+`"`) {
				t.Errorf("%s's page offers no %s", inst, id)
			}
		}
	}
	if out := act(primary, "storage-shrink-config", nil); !strings.Contains(out, "Minimal target size (GB)") || !strings.Contains(out, `"10"`) {
		t.Errorf("console Storage shrink config: %.500s", out)
	}
	act(primary, "shrink-storage", map[string]string{"targetSizeGb": "15"})
	act(replica, "reset-replica-size", nil)
	if got := size(primary); got != 15 {
		t.Errorf("after the console's Shrink storage, dataDiskSizeGb = %d, want 15", got)
	}
	if got := size(replica); got != 15 {
		t.Errorf("after the console's Reset replica size, the replica's dataDiskSizeGb = %d, want 15", got)
	}
}
