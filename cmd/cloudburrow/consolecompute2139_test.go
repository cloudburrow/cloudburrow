package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func vpcField(fields []console.Field, name string) *console.Field {
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i]
		}
	}
	return nil
}

// The Compute Engine parity round (#2139): Create an instance's panes,
// Series and Spot, Allow HTTP traffic and its rule; the list's Provisioning
// model and In use by; Stop over the selection with Skip graceful
// shutdown; Instance schedules; Snapshot schedules and a disk given one;
// Create a snapshot's location and type; Clone disk and Create image;
// custom images' Deprecate and Delete; Instance templates with Create VM
// and Create similar; managed and unmanaged instance groups; Machine
// images; the SSH keys tab.
func TestConsoleComputeParity2139(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "c2139"
	vms := computeInstancesProvider{s}

	fields := vms.CreateFormFor(ctx, project)
	var panes []string
	for _, f := range fields {
		if len(panes) == 0 || panes[len(panes)-1] != f.Section {
			panes = append(panes, f.Section)
		}
	}
	if got := strings.Join(panes, ","); got != "Machine configuration,OS and storage,Data protection,Networking,Observability,Security,Advanced" {
		t.Fatalf("Create an instance's panes = %s", got)
	}
	if mt := vpcField(fields, "machineType"); mt.OptionsBy != "series" || len(mt.OptionsFor["N1"]) == 0 || mt.OptionsFor["E2"][0] != "e2-micro" {
		t.Fatalf("machine type by series = %+v", mt)
	}
	if f := vpcField(fields, "opsAgent"); f == nil || f.Unavailable == "" {
		t.Error("the Ops Agent is offered as if it were served")
	}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "web", "zone": "us-central1-a", "machineType": "e2-small",
		"provisioningModel": "Spot", "terminationAction": "Delete", "allowHttp": "true", "networkTags": "app"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	fw, err := s.call("GET", project, "global/firewalls/default-allow-http", nil, nil)
	if err != nil || vpcStrs(fw, "targetTags")[0] != "http-server" {
		t.Fatalf("default-allow-http = %v, %v", fw, err)
	}
	o, _ := vms.instance(project, []string{"us-central1-a", "web"})
	if tags := vpcStrs(vmMap(o, "tags"), "items"); strings.Join(tags, ",") != "app,http-server" {
		t.Errorf("web's tags = %v", tags)
	}
	if sc := vmMap(o, "scheduling"); vpcStr(sc, "provisioningModel") != "SPOT" || vpcStr(sc, "instanceTerminationAction") != "DELETE" {
		t.Errorf("web's scheduling = %v", sc)
	}
	l, _ := vms.List(ctx, project)
	if r := vpcRow(l, "web"); r == nil || r.Fields["Provisioning model"] != "Spot" || !r.Selectable || r.Fields["In use by"] != "—" {
		t.Fatalf("web's row = %+v", r)
	}
	// The row's Stop is Google's one dialog: "Stop web?", the checkbox,
	// Stop.
	for _, a := range vpcRow(l, "web").Actions {
		if a.ID == "stop" && (a.Title != "Stop web?" || a.Submit != "Stop" || vpcField(a.Fields, "noGracefulShutdown") == nil || a.Confirm != "") {
			t.Errorf("web's Stop = %+v", a)
		}
	}
	sel := vms.ListSelectActions(ctx, project)
	if got := vpcActionIDs(sel); got != "start,stop,suspend,reset,delete" || vpcField(sel[1].Fields, "noGracefulShutdown") == nil {
		t.Fatalf("selection actions = %s %+v", got, sel[1].Fields)
	}
	if err := vms.ActAt(ctx, project, nil, "stop", map[string]string{instanceSelectField: "web", "noGracefulShutdown": "true"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if o, _ := vms.instance(project, []string{"us-central1-a", "web"}); vpcStr(o, "status") != "TERMINATED" {
		t.Fatalf("after Stop over the selection web is %s", vpcStr(o, "status"))
	}

	// Instance schedules.
	sch := computeInstanceSchedulesProvider{s}
	if _, err := sch.Create(ctx, project, map[string]string{"name": "office", "region": "us-central1", "startTime": "09:00", "stopTime": "18:30",
		"timeZone": "America/New_York", "frequency": "Weekdays"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sch.Create(ctx, project, map[string]string{"name": "bad", "region": "us-central1", "startTime": "25:00", "timeZone": "UTC"}); err == nil {
		t.Error("a start time of 25:00 was accepted")
	}
	sl, _ := sch.List(ctx, project)
	r := vpcRow(sl, "office")
	if r == nil || r.Fields["Start"] != "0 9 * * 1-5" || r.Fields["Stop"] != "30 18 * * 1-5" || r.Fields["Time zone"] != "America/New_York" {
		t.Fatalf("office = %+v", r)
	}
	if err := sch.ActAt(ctx, project, r.Target, "addinstances", map[string]string{"instances": "us-central1-a/web"}); err != nil {
		t.Fatal(err)
	}
	sl, _ = sch.List(ctx, project)
	if r := vpcRow(sl, "office"); r.Fields["In use by"] != "web" || r.Actions[len(r.Actions)-1].Unavailable == "" {
		t.Fatalf("office in use = %+v", r)
	}
	if err := sch.ActAt(ctx, project, r.Target, "removeinstances", map[string]string{"instances": "web"}); err != nil {
		t.Fatal(err)
	}
	if err := sch.ActAt(ctx, project, r.Target, "delete", nil); err != nil {
		t.Fatal(err)
	}

	// Snapshot schedules, a disk given one, Create a snapshot.
	ss := computeSnapshotSchedulesProvider{s}
	if _, err := ss.Create(ctx, project, map[string]string{"name": "nightly", "region": "us-central1", "frequency": "Weekly", "day": "Friday",
		"startTime": "03:00", "retention": "7", "location": "Regional"}); err != nil {
		t.Fatal(err)
	}
	ssl, _ := ss.List(ctx, project)
	if r := vpcRow(ssl, "nightly"); r == nil || r.Fields["Frequency"] != "Weekly on Friday" || r.Fields["Start time (UTC)"] != "03:00" || r.Fields["Autodelete after"] != "7 days" {
		t.Fatalf("nightly = %+v", ssl.Items)
	}
	disks := computeDisksProvider{s}
	df := disks.CreateFormFor(ctx, project)
	if f := vpcField(df, "snapshotSchedule"); strings.Join(f.Options, ",") != noBackups+",us-central1/nightly" {
		t.Fatalf("the disk form's schedules = %v", f.Options)
	}
	if f := vpcField(df, "location"); f.UnavailableOptions["Regional"] == "" {
		t.Error("regional disks are offered")
	}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "data", "zone": "us-central1-a", "sizeGb": "20", "snapshotSchedule": "us-central1/nightly"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if d, _ := disks.Detail(ctx, project, []string{"us-central1-a", "data"}); !strings.Contains(c2139Props(d), "Snapshot schedule=nightly") {
		t.Errorf("data's page = %s", c2139Props(d))
	}
	ssl, _ = ss.List(ctx, project)
	if r := vpcRow(ssl, "nightly"); r.Fields["In use by"] != "data" || r.Actions[0].Unavailable == "" {
		t.Fatalf("nightly in use = %+v", r)
	}
	if err := disks.ActAt(ctx, project, []string{"us-central1-a", "data"}, "schedule", map[string]string{"schedule": ""}); err != nil {
		t.Fatal(err)
	}
	if err := disks.ActAt(ctx, project, []string{"us-central1-a", "data"}, "clone", map[string]string{"name": "data-copy"}); err != nil {
		t.Fatal(err)
	}
	if err := disks.ActAt(ctx, project, []string{"us-central1-a", "data"}, "image", map[string]string{"name": "data-img", "family": "data"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	settleGlobal(t, s, project)
	if d, _ := disks.Detail(ctx, project, []string{"us-central1-a", "data-copy"}); !strings.Contains(c2139Props(d), "Source=Disk data") {
		t.Errorf("the clone's page = %s", c2139Props(d))
	}
	snaps := computeSnapshotsProvider{s}
	if f := vpcField(snaps.CreateFormFor(ctx, project), "disk"); f.Type != "select" || !strings.Contains(strings.Join(f.Options, ","), "us-central1-a/data") {
		t.Fatalf("Create a snapshot's source disks = %+v", f)
	}
	if _, err := snaps.Create(ctx, project, map[string]string{"name": "arch", "disk": "us-central1-a/data", "location": "Regional", "snapshotType": "Archive snapshot"}); err != nil {
		t.Fatal(err)
	}
	settleGlobal(t, s, project)
	snl, _ := snaps.List(ctx, project)
	if r := vpcRow(snl, "arch"); r == nil || r.Fields["Location"] != "us-central1" || r.Fields["Type"] != "Archive snapshot" {
		t.Fatalf("arch = %+v", r)
	}

	// Custom images.
	images := computeImagesProvider{s}
	il, _ := images.List(ctx, project)
	img := vpcRow(il, "data-img")
	if img == nil || img.Fields["Family"] != "data" || img.Fields["Created by"] != project || vpcActionIDs(img.Actions) != "deprecate,delete" {
		t.Fatalf("data-img = %+v", img)
	}
	if _, err := images.Create(ctx, project, map[string]string{"name": "from-snap", "source": "Snapshot", "snapshot": "arch", "locationName": "us-central1"}); err != nil {
		t.Fatal(err)
	}
	if err := images.ActAt(ctx, project, []string{"data-img"}, "deprecate", map[string]string{"state": "Obsolete"}); err != nil {
		t.Fatal(err)
	}
	il, _ = images.List(ctx, project)
	if r := vpcRow(il, "data-img"); r.Status != "OBSOLETE" {
		t.Errorf("deprecated image status %q", r.Status)
	}
	settle(t, s, project)
	settleGlobal(t, s, project)
	if err := images.ActAt(ctx, project, []string{"from-snap"}, "delete", nil); err != nil {
		t.Fatal(err)
	}

	// Instance templates.
	tpls := computeTemplatesProvider{s}
	tf := tpls.CreateFormFor(ctx, project)
	if vpcField(tf, "location") == nil || vpcField(tf, "zone") != nil || vpcField(tf, "machineType") == nil || vpcField(tf, "snapshotSchedule") != nil {
		t.Fatalf("Create an instance template's fields = %s", vpcFieldNames(tf))
	}
	if _, err := tpls.Create(ctx, project, map[string]string{"name": "tpl", "location": "Global", "machineType": "e2-small", "networkTags": "web",
		"startupScript": "echo hi"}); err != nil {
		t.Fatal(err)
	}
	tl, _ := tpls.List(ctx, project)
	tr := vpcRow(tl, "tpl")
	if tr == nil || tr.Fields["Machine type"] != "e2-small" || tr.Fields["Location"] != "Global" || vpcActionIDs(tr.Actions) != "createvm,similar,delete" {
		t.Fatalf("tpl = %+v", tr)
	}
	if err := tpls.ActAt(ctx, project, tr.Target, "createvm", map[string]string{"name": "from-tpl", "zone": "us-central1-b"}); err != nil {
		t.Fatal(err)
	}
	if err := tpls.ActAt(ctx, project, tr.Target, "similar", map[string]string{"name": "tpl-2"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if o, err := vms.instance(project, []string{"us-central1-b", "from-tpl"}); err != nil || !strings.HasSuffix(vpcStr(o, "machineType"), "/e2-small") {
		t.Fatalf("from-tpl = %v, %v", o, err)
	}
	if d, _ := tpls.Detail(ctx, project, tr.Target); !strings.Contains(c2139Props(d), "Network tags=web") || !strings.Contains(c2139Props(d), "Metadata=startup-script") {
		t.Errorf("tpl's page = %s", c2139Props(d))
	}

	// Instance groups.
	groups := computeGroupsProvider{s}
	gf := groups.CreateFormFor(ctx, project)
	if f := vpcField(gf, "template"); f.Type != "select" || strings.Join(f.Options, ",") != "global/tpl,global/tpl-2" {
		t.Fatalf("the group form's templates = %+v", f)
	}
	if _, err := groups.Create(ctx, project, map[string]string{"kind": groupManaged, "name": "mig", "template": "global/tpl", "zone": "us-central1-a",
		"autoscaling": autoscaleOff, "size": "2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.Create(ctx, project, map[string]string{"kind": groupUnmanaged, "name": "ug", "zone": "us-central1-a", "network": "default", "instances": "web"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	gl, _ := groups.List(ctx, project)
	if r := vpcRow(gl, "mig"); r == nil || r.Fields["Instances"] != "2" || r.Fields["Group type"] != "Managed (stateless)" || r.Fields["Template"] != "tpl" ||
		vpcActionIDs(r.Actions) != "resize,restart,delete" {
		t.Fatalf("mig = %+v", r)
	}
	if r := vpcRow(gl, "ug"); r == nil || r.Fields["Instances"] != "1" || vpcActionIDs(r.Actions) != "addinstances,removeinstances,delete" {
		t.Fatalf("ug = %+v", r)
	}
	l, _ = vms.List(ctx, project)
	inUse := map[string]int{}
	for _, it := range l.Items {
		inUse[it.Fields["In use by"]]++
	}
	if inUse["mig"] != 2 || inUse["ug"] != 1 {
		t.Fatalf("In use by = %v", inUse)
	}
	if d, _ := groups.Detail(ctx, project, []string{"us-central1-a", "mig"}); d.Sections[0].Listing.Total != 2 {
		t.Fatalf("mig's VMs = %+v", d.Sections[0].Listing)
	}
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "mig"}, "resize", map[string]string{"size": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "mig"}, "restart", map[string]string{"operation": "Replace"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "mig"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "ug"}, "removeinstances", map[string]string{"instances": "web"}); err != nil {
		t.Fatal(err)
	}
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "ug"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if gl, _ := groups.List(ctx, project); gl.Total != 0 {
		t.Fatalf("groups after Delete: %+v", gl.Items)
	}

	// Machine images.
	mis := computeMachineImagesProvider{s}
	if f := vpcField(mis.CreateFormFor(ctx, project), "source"); !strings.Contains(strings.Join(f.Options, ","), "us-central1-a/web") {
		t.Fatalf("source VMs = %v", f.Options)
	}
	if _, err := mis.Create(ctx, project, map[string]string{"name": "web-mi", "source": "us-central1-a/web", "location": "Regional"}); err != nil {
		t.Fatal(err)
	}
	// A machine image's operation is global: it is CREATING until its
	// disks are copied.
	settleGlobal(t, s, project)
	ml, _ := mis.List(ctx, project)
	if r := vpcRow(ml, "web-mi"); r == nil || r.Fields["Source instance"] != "web" || r.Fields["Location"] != "us-central1" {
		t.Fatalf("web-mi = %+v", r)
	}
	if err := mis.ActAt(ctx, project, []string{"web-mi"}, "createvm", map[string]string{"name": "web-copy", "zone": "us-central1-c"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if _, err := vms.instance(project, []string{"us-central1-c", "web-copy"}); err != nil {
		t.Fatal(err)
	}

	// SSH keys.
	keys := computeSSHKeysProvider{computeMetadataProvider{s}}
	edit := keys.ListActions(ctx, project)
	if len(edit) != 1 || edit[0].ID != "edit" {
		t.Fatalf("SSH keys actions = %+v", edit)
	}
	group := console.FormatGroup([]map[string]string{{"key": "alice:ssh-ed25519 AAAAC3 alice@laptop"}, {"key": "ssh-ed25519 AAAAD4 bob@desk"}})
	if err := keys.ActAt(ctx, project, nil, "edit", map[string]string{"keys": group, "fingerprint": edit[0].Fields[1].Default}); err != nil {
		t.Fatal(err)
	}
	kl, _ := keys.List(ctx, project)
	if vpcRow(kl, "alice") == nil || vpcRow(kl, "bob") == nil || kl.Total != 2 {
		t.Fatalf("SSH keys = %+v", kl.Items)
	}
	if err := keys.ActAt(ctx, project, nil, "edit", map[string]string{"keys": console.FormatGroup([]map[string]string{{"key": "nokey"}})}); err == nil {
		t.Error("a key without a username was accepted")
	}
}

// settleGlobal waits for the project's global operations.
func settleGlobal(t *testing.T, s computeScreen, project string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := s.list(project, "global/operations")
		if err != nil {
			t.Fatal(err)
		}
		busy := false
		for _, op := range l {
			busy = busy || vpcStr(op, "status") != "DONE"
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("global operations never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// c2139Props is a page's properties as LABEL=VALUE lines.
func c2139Props(d console.Detail) string {
	var out []string
	for _, sec := range d.Sections {
		for _, g := range sec.Groups {
			for _, p := range g.Properties {
				out = append(out, p.Label+"="+p.Value)
			}
		}
	}
	return strings.Join(out, "\n")
}
