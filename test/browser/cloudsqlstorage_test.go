//go:build browser

package browser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// clickAction performs one of a resource page's actions that has no form,
// which is sent on click; confirm, when not empty, is the name its
// confirmation asks to be typed back. It returns the text of the rows the
// action answers with, "" when it answers none.
func (p *tab) clickAction(label, confirm string) string {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", label)
	if confirm != "" {
		p.confirmByName(confirm)
	}
	p.waitFor(`document.querySelector("#action-result-table") !== null ||
		(document.querySelector(".modal.is-open") === null && document.querySelector(".snackbar") !== null)`)
	var rows string
	p.eval(`(() => { const t = document.querySelector("#action-result-table"); return t ? t.textContent : ""; })()`, &rows)
	if rows != "" {
		p.clickText(".modal.is-open .modal-actions button", "Close")
	}
	p.waitFor(`document.querySelector(".modal") === null`)
	return rows
}

// createSQL inserts a Cloud SQL instance with the official client, waits
// for it, and deletes it when the test ends.
func createSQL(t *testing.T, svc *sqladmin.Service, project string, in *sqladmin.DatabaseInstance) {
	t.Helper()
	op, err := svc.Instances.Insert(project, in).Do()
	if err != nil {
		t.Fatalf("insert %s: %v", in.Name, err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, in.Name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
}

// waitSQLInstance polls instances.get until ok holds of the instance.
func waitSQLInstance(t *testing.T, svc *sqladmin.Service, project, name, what string, ok func(*sqladmin.DatabaseInstance) bool) {
	t.Helper()
	var in *sqladmin.DatabaseInstance
	var err error
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
		if in, err = svc.Instances.Get(project, name).Do(); err == nil && ok(in) {
			return
		}
	}
	t.Fatalf("instances.get %s %s: %+v, %v", name, what, in, err)
}

// TestCloudSQLStorageShrinkThroughThePage (#1535, #1592), in the sql shard.
// On a created PostgreSQL primary of 30 GB with a read replica: Storage
// shrink config answers the smallest size its data allows, 10 GB, as
// getDiskShrinkConfig does (the page dropped the answer of an action sent
// on click, #1592); Shrink storage, confirmed by the instance's
// name, makes the dataDiskSizeGb instances.get reads 20; and Reset replica
// size on the replica's page, confirmed, sets the replica's to its
// primary's.
func TestCloudSQLStorageShrinkThroughThePage(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	n := time.Now().UnixNano() % 1e6
	primary, replica := fmt.Sprintf("browser-shrink-%d", n), fmt.Sprintf("browser-shrinkr-%d", n)
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: primary, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataDiskSizeGb: 30}})
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: replica, MasterInstanceName: primary, InstanceType: "READ_REPLICA_INSTANCE",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataDiskSizeGb: 30}})
	cfg, err := svc.Projects.Instances.GetDiskShrinkConfig(project, primary).Do()
	if err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/cloudsql-instances/" + primary + "?project=" + project)
	rows := p.clickAction("Storage shrink config", "")
	if !strings.Contains(rows, "Minimal target size (GB)") || !strings.Contains(rows, fmt.Sprint(cfg.MinimalTargetSizeGb)) {
		t.Errorf("Storage shrink config answered %q; want getDiskShrinkConfig's %d GB", rows, cfg.MinimalTargetSizeGb)
	}
	p.navigate("/cloudsql-instances/" + primary + "?project=" + project)
	p.pageAction("Shrink storage", map[string]string{"targetSizeGb": "20"}, primary)
	waitSQLInstance(t, svc, project, primary, "after Shrink storage, at 20 GB", func(in *sqladmin.DatabaseInstance) bool {
		return in.State == "RUNNABLE" && in.Settings.DataDiskSizeGb == 20
	})
	if in, err := svc.Instances.Get(project, replica).Do(); err != nil || in.Settings.DataDiskSizeGb != 30 {
		t.Fatalf("the replica before Reset replica size: %+v, %v; want 30 GB", in, err)
	}

	p.navigate("/cloudsql-instances/" + replica + "?project=" + project)
	p.clickAction("Reset replica size", replica)
	waitSQLInstance(t, svc, project, replica, "after Reset replica size, at its primary's 20 GB", func(in *sqladmin.DatabaseInstance) bool {
		return in.State == "RUNNABLE" && in.Settings.DataDiskSizeGb == 20
	})
}

// TestCloudSQLDemoteThroughThePage (#1524, #1592), in the sql shard. A
// MySQL source, and a second MySQL instance made its replica and promoted,
// so it has GTIDs on and only the source's transactions; a source
// representation instance records the source's server. Demote on the
// promoted instance's page, by demoteMaster with the replication user and
// confirmed by its name, makes it the READ_REPLICA_INSTANCE of the source
// representation instance that instances.get reads, and its page then
// offers Demote no more.
func TestCloudSQLDemoteThroughThePage(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	n := time.Now().UnixNano() % 1e6
	src, rep, inst := fmt.Sprintf("browser-dsrc-%d", n), fmt.Sprintf("browser-dext-%d", n), fmt.Sprintf("browser-dem-%d", n)
	tier := &sqladmin.Settings{Tier: "db-custom-1-3840"}
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: src, DatabaseVersion: "MYSQL_8_4", RootPassword: "Root-pass1",
		Region: "us-central1", Settings: tier})
	// Made a replica, src turns its GTIDs on; promoted, inst has only its.
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, MasterInstanceName: src, InstanceType: "READ_REPLICA_INSTANCE",
		Region: "us-central1", Settings: tier})
	op, err := svc.Instances.PromoteReplica(project, inst).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQL(t, svc, project, op, 5*time.Minute)
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: rep, DatabaseVersion: "MYSQL_8_4", InstanceType: "ON_PREMISES_INSTANCE",
		Region: "us-central1", OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + src + ":3306",
			Username: "root", Password: "Root-pass1"}})
	waitSQLInstance(t, svc, project, inst, "promoted, RUNNABLE and standalone", func(in *sqladmin.DatabaseInstance) bool {
		return in.State == "RUNNABLE" && in.MasterInstanceName == ""
	})

	p := open(t)
	page := "/cloudsql-instances/" + inst + "?project=" + project
	p.navigate(page)
	p.pageAction("Demote", map[string]string{"source": rep, "method": "demoteMaster", "username": "root", "password": "Root-pass1"}, inst)
	waitSQLInstance(t, svc, project, inst, "after Demote, a RUNNABLE replica of "+rep, func(in *sqladmin.DatabaseInstance) bool {
		return in.State == "RUNNABLE" && in.InstanceType == "READ_REPLICA_INSTANCE" && in.MasterInstanceName == project+":"+rep
	})
	p.navigate(page)
	p.waitFor(`document.querySelectorAll("#view .page-actions button").length > 0`)
	var offered bool
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Demote")`, &offered)
	if offered {
		t.Error("the demoted replica's page still offers Demote")
	}
}
