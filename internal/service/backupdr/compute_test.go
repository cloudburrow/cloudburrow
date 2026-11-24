package backupdr

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// memVMs is a compute.VMBackend whose disks are strings, so a test reads
// back what a restore made.
type memVMs struct {
	mu   sync.Mutex
	data map[string]string
}

func (m *memVMs) CreateDisk(_ context.Context, d compute.DiskSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[d.Key] = m.data[d.SourceSnapshot]
	return nil
}
func (m *memVMs) DeleteDisk(_ context.Context, d compute.DiskSpec) error { return nil }
func (m *memVMs) ResizeDisk(context.Context, compute.DiskSpec) error     { return nil }
func (m *memVMs) Snapshot(_ context.Context, d compute.DiskSpec, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = m.data[d.Key]
	return nil
}
func (m *memVMs) DeleteSnapshot(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
func (m *memVMs) Start(context.Context, compute.VMSpec) error                  { return nil }
func (m *memVMs) Stop(context.Context, compute.VMSpec) error                   { return nil }
func (m *memVMs) Suspend(context.Context, compute.VMSpec) error                { return nil }
func (m *memVMs) Resume(context.Context, compute.VMSpec) error                 { return nil }
func (m *memVMs) SerialOutput(context.Context, compute.VMSpec) (string, error) { return "", nil }
func (m *memVMs) SSHAddress(compute.VMSpec) string                             { return "" }
func (m *memVMs) FreeBytes() (uint64, error)                                   { return 1 << 50, nil }

// diskKey is the backend key of a served disk ("disk-" + its id).
func diskKey(t *testing.T, api *compute.Server, project, zone, disk string) string {
	t.Helper()
	d, err := api.Call("GET", project, "zones/"+zone+"/disks/"+disk, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return "disk-" + d["id"].(string)
}

func waitOp(t *testing.T, s *Server, op map[string]any) map[string]any {
	t.Helper()
	for i := 0; i < 200; i++ {
		_, out := call(t, s, "GET", "/v1/"+op["name"].(string), "")
		if out["done"] == true {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operation %v never finished", op["name"])
	return nil
}

func computeWait(t *testing.T, api *compute.Server, project, zone string, op map[string]any, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Call("POST", project, "zones/"+zone+"/operations/"+op["name"].(string)+"/wait", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// TestComputeBackupAndRestore (#2075): a plan and association for a VM
// and a disk, an on-demand backup of each (a snapshot per disk, kept out
// of snapshots.list), the backup's properties, a VM restored into another
// project and zone with a new machine type, a disk restored by name, one
// disk restored from the VM's backup, and the data read back.
func TestComputeBackupAndRestore(t *testing.T) {
	mem := &memVMs{data: map[string]string{}}
	api, err := compute.New(compute.Options{VMs: mem})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("", &fakeSQL{runs: map[string]Run{}}, "")
	s.SetCompute(func() Compute { return api })
	const zone = "us-central1-a"
	op, err := api.Call("POST", "p", "zones/"+zone+"/disks", nil, map[string]any{"name": "data", "sizeGb": "10"})
	computeWait(t, api, "p", zone, op, err)
	op, err = api.Call("POST", "p", "zones/"+zone+"/instances", nil, map[string]any{"name": "vm", "machineType": "zones/" + zone + "/machineTypes/e2-small",
		"disks": []any{map[string]any{"boot": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}},
			map[string]any{"source": "zones/" + zone + "/disks/data", "deviceName": "data"}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/default"}},
		"metadata":          map[string]any{"items": []any{map[string]any{"key": "k", "value": "v"}}}})
	computeWait(t, api, "p", zone, op, err)
	mem.data[diskKey(t, api, "p", zone, "data")] = "original"
	mem.data[diskKey(t, api, "p", zone, "vm")] = "boot"

	if code, out := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`); code != 200 {
		t.Fatalf("vault: %d %v", code, out)
	}
	for _, typ := range []string{"Instance", "Disk"} {
		// No logRetentionDays: it applies to Cloud SQL and AlloyDB only.
		body := strings.Replace(strings.Replace(planBody, "sqladmin.googleapis.com/Instance", "compute.googleapis.com/"+typ, 1), `,"logRetentionDays":"7"`, "", 1)
		if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId="+strings.ToLower(typ), body); code != 200 {
			t.Fatalf("%s plan: %d %v", typ, code, out)
		}
	}
	hourly := strings.Replace(strings.Replace(strings.Replace(planBody, "sqladmin.googleapis.com/Instance", "compute.googleapis.com/Disk", 1),
		`"recurrenceType":"DAILY"`, `"recurrenceType":"HOURLY","hourlyFrequency":1`, 1), `,"logRetentionDays":"7"`, "", 1)
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=hourly", hourly); code != 200 {
		t.Errorf("an hourly frequency of 1 for a disk: %d %v", code, out)
	}
	if code, _ := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=bad",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/instance","resourceType":"compute.googleapis.com/Instance","resource":"projects/p/zones/`+zone+`/instances/nope"}`); code != 404 {
		t.Errorf("a missing VM: %d", code)
	}
	if code, _ := call(t, s, "POST", "/v1/projects/p/locations/europe-west1/backupPlanAssociations?backupPlanAssociationId=bad",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/instance","resourceType":"compute.googleapis.com/Instance","resource":"projects/p/zones/`+zone+`/instances/vm"}`); code != 400 {
		t.Errorf("an association outside the VM's region: %d", code)
	}
	assoc := func(id, typ, res string) {
		t.Helper()
		code, out := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId="+id,
			`{"backupPlan":"projects/p/locations/us-central1/backupPlans/`+strings.ToLower(typ)+`","resourceType":"compute.googleapis.com/`+typ+`","resource":"`+res+`"}`)
		if code != 200 {
			t.Fatalf("%s association: %d %v", typ, code, out)
		}
	}
	assoc("vm", "Instance", "//compute.googleapis.com/projects/p/zones/"+zone+"/instances/vm")
	assoc("disk", "Disk", "projects/p/zones/"+zone+"/disks/data")
	if code, _ := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=again",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/disk","resourceType":"compute.googleapis.com/Disk","resource":"projects/p/zones/`+zone+`/disks/data"}`); code != 409 {
		t.Errorf("a second association of the disk: %d", code)
	}
	backupOf := func(id string) map[string]any {
		t.Helper()
		code, op := call(t, s, "POST", loc+"/backupPlanAssociations/"+id+":triggerBackup", `{"ruleId":"daily"}`)
		if code != 200 {
			t.Fatalf("triggerBackup %s: %d %v", id, code, op)
		}
		if out := waitOp(t, s, op); out["error"] != nil {
			t.Fatalf("backup %s: %v", id, out["error"])
		}
		_, a := call(t, s, "GET", loc+"/backupPlanAssociations/"+id, "")
		_, list := call(t, s, "GET", "/v1/"+a["dataSource"].(string)+"/backups", "")
		bs := list["backups"].([]any)
		if len(bs) != 1 {
			t.Fatalf("%s backups: %v", id, list)
		}
		return bs[0].(map[string]any)
	}
	vb, db := backupOf("vm"), backupOf("disk")
	if vb["state"] != "ACTIVE" || vb["gcpResource"].(map[string]any)["type"] != ComputeInstanceResourceType {
		t.Fatalf("VM backup: %v", vb)
	}
	props := vb["computeInstanceBackupProperties"].(map[string]any)
	if !strings.HasSuffix(props["machineType"].(string), "/e2-small") || len(props["disk"].([]any)) != 2 {
		t.Errorf("computeInstanceBackupProperties: %v", props)
	}
	if dp := db["diskBackupProperties"].(map[string]any); dp["sizeGb"] != "10" || !strings.HasSuffix(dp["type"].(string), "/pd-balanced") {
		t.Errorf("diskBackupProperties: %v", dp)
	}
	if l, _ := api.Call("GET", "p", "global/snapshots", nil, nil); len(l["items"].([]any)) != 0 {
		t.Errorf("snapshots.list shows the vault's snapshots: %v", l["items"])
	}
	_, ds := call(t, s, "GET", "/v1/"+strings.Split(vb["name"].(string), "/backups/")[0], "")
	if g := ds["dataSourceGcpResource"].(map[string]any); g["computeInstanceDatasourceProperties"].(map[string]any)["totalDiskCount"] != "2" {
		t.Errorf("dataSourceGcpResource: %v", g)
	}
	_, f := call(t, s, "GET", "/v1/projects/p/locations/us-central1/backupVaults/v/dataSources/-/backups:fetchForResourceType?resourceType=compute.googleapis.com/Disk", "")
	if l := f["backups"].([]any); len(l) != 1 {
		t.Errorf("fetchForResourceType Disk: %v", f)
	}

	// The data changes after the backups.
	mem.mu.Lock()
	mem.data[diskKey(t, api, "p", zone, "data")] = "changed"
	mem.mu.Unlock()

	restore := func(b map[string]any, body string) map[string]any {
		t.Helper()
		code, op := call(t, s, "POST", "/v1/"+b["name"].(string)+":restore", body)
		if code != 200 {
			t.Fatalf("restore: %d %v", code, op)
		}
		out := waitOp(t, s, op)
		if out["error"] != nil {
			t.Fatalf("restore: %v", out["error"])
		}
		return out["response"].(map[string]any)
	}
	resp := restore(vb, `{"computeInstanceTargetEnvironment":{"project":"q","zone":"us-east1-b"},
		"computeInstanceRestoreProperties":{"name":"vm2","machineType":"e2-medium"}}`)
	if g := resp["targetResource"].(map[string]any)["gcpResource"].(map[string]any); g["gcpResourcename"] != "//compute.googleapis.com/projects/q/zones/us-east1-b/instances/vm2" {
		t.Errorf("restore response: %v", resp)
	}
	vm2, err := api.Call("GET", "q", "zones/us-east1-b/instances/vm2", nil, nil)
	if err != nil || !strings.HasSuffix(vm2["machineType"].(string), "/e2-medium") || len(vm2["disks"].([]any)) != 2 {
		t.Fatalf("restored VM: %v %v", vm2, err)
	}
	if md := vm2["metadata"].(map[string]any)["items"].([]any); len(md) != 1 {
		t.Errorf("restored metadata: %v", md)
	}
	if got := mem.data[diskKey(t, api, "q", "us-east1-b", "vm2-1")]; got != "original" {
		t.Errorf("the restored VM's data disk holds %q", got)
	}
	if got := mem.data[diskKey(t, api, "q", "us-east1-b", "vm2")]; got != "boot" {
		t.Errorf("the restored VM's boot disk holds %q", got)
	}
	restore(db, `{"diskTargetEnvironment":{"project":"p","zone":"us-central1-b"},"diskRestoreProperties":{"name":"data2"}}`)
	if got := mem.data[diskKey(t, api, "p", "us-central1-b", "data2")]; got != "original" {
		t.Errorf("the restored disk holds %q", got)
	}
	restore(vb, `{"diskTargetEnvironment":{"project":"p","zone":"us-central1-a"},"diskRestoreProperties":{"name":"data3","sizeGb":"20",
		"instanceBackupSource":{"sourceDeviceName":"data"}}}`)
	if d, _ := api.Call("GET", "p", "zones/us-central1-a/disks/data3", nil, nil); d["sizeGb"] != "20" || mem.data["disk-"+d["id"].(string)] != "original" {
		t.Errorf("the disk restored from the VM's backup: %v", d)
	}
	for body, want := range map[string]int{
		`{"diskTargetEnvironment":{"project":"p","zone":"us-central1-a"},"diskRestoreProperties":{}}`:                                  http.StatusBadRequest,
		`{"computeInstanceTargetEnvironment":{"project":"p","zone":"us-central1-a"},"computeInstanceRestoreProperties":{"name":"x"}}`:  http.StatusBadRequest,
		`{"regionDiskTargetEnvironment":{"project":"p","region":"us-central1"}}`:                                                       http.StatusNotImplemented,
		`{"diskTargetEnvironment":{"project":"p","zone":"us-central1-a"},"diskRestoreProperties":{"name":"data2x","storagePool":"x"}}`: http.StatusNotImplemented,
		`{"diskTargetEnvironment":{"project":"p","zone":"us-central1-b"},"diskRestoreProperties":{"name":"data2"}}`:                    http.StatusConflict,
	} {
		if code, out := call(t, s, "POST", "/v1/"+db["name"].(string)+":restore", body); code != want {
			t.Errorf("restore %s: %d %v, want %d", body, code, out, want)
		}
	}
	// Deleting the backup (past its enforced retention) deletes its snapshots.
	s.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if code, out := call(t, s, "DELETE", "/v1/"+db["name"].(string), ""); code != 200 {
		t.Fatalf("delete: %d %v", code, out)
	}
	time.Sleep(100 * time.Millisecond)
	mem.mu.Lock()
	left := 0
	for k := range mem.data {
		if strings.HasPrefix(k, "snap-") {
			left++
		}
	}
	mem.mu.Unlock()
	if left != 2 {
		t.Errorf("snapshots left after deleting the disk's backup: %d, want the VM backup's 2", left)
	}
}
