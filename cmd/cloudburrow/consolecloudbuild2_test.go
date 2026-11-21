package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// cloudBuildConsole serves Cloud Build over gRPC for a console test.
func cloudBuildConsole(t *testing.T, proj string) runBuildsProvider {
	t.Helper()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.Builds(stubSource{}, noopSteps{}).AllowHostSteps(true).CloudBuild().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return runBuildsProvider{defaultProject: proj, runAddr: func() string { return ln.Addr().String() }}
}

// TestConsoleBuildBulkApproveRowRunAndDashboardCards (#1854): History's
// Approve is offered while builds await approval, only those rows are
// selectable, and it approves every checked build; a trigger row's Run is a
// button and Edit leads its menu; the Dashboard is a card per trigger,
// titled by the latest build's outcome, with Google's labels.
func TestConsoleBuildBulkApproveRowRunAndDashboardCards(t *testing.T) {
	ctx := context.Background()
	proj := freshProject("console-bulk")
	b := cloudBuildConsole(t, proj)
	p := buildTriggersProvider{b: b}
	if acts := b.ListSelectActions(ctx, ""); acts != nil {
		t.Errorf("Approve with nothing awaiting approval = %+v", acts)
	}
	if _, err := p.Create(ctx, "", map[string]string{"name": "gated", "region": "global", "image": "alpine:3.20",
		"command": "echo hi", "approval": "true"}); err != nil {
		t.Fatalf("Create trigger: %v", err)
	}
	l, _ := p.List(ctx, "")
	acts := p.Actions(l.Items[0])
	if len(acts) == 0 || acts[0].ID != "run" || !acts[0].Button {
		t.Errorf("the row's actions = %+v; want Run as a button", acts)
	}
	if !p.RowEditFirst() {
		t.Error("the row's menu does not begin with Edit")
	}
	for i := 0; i < 2; i++ {
		if err := p.Act(ctx, "", "gated", "run"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	h, _ := b.List(ctx, "")
	var ids []string
	for _, it := range h.Items {
		if !it.Selectable {
			t.Errorf("build %s awaiting approval is not selectable", it.Name)
		}
		ids = append(ids, it.Name)
	}
	sel := b.ListSelectActions(ctx, "")
	if len(ids) != 2 || len(sel) != 2 || sel[0].ID != "approve-selected" || sel[0].SelectionField != "builds" ||
		sel[1].ID != "reject-selected" || sel[1].Label != "Reject" {
		t.Fatalf("History = %v, its selection actions %+v", ids, sel)
	}
	if err := b.ActAt(ctx, "", nil, "approve-selected", map[string]string{"builds": "nope"}); err == nil {
		t.Error("Approve of an unknown build was accepted")
	}
	if err := b.ActAt(ctx, "", nil, "approve-selected", map[string]string{"builds": strings.Join(ids, "\n"), "comment": "both"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	for _, id := range ids {
		var st cbpb.Build_Status
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if st = b.build("", id).GetStatus(); st == cbpb.Build_SUCCESS {
				break
			}
		}
		if st != cbpb.Build_SUCCESS || b.build("", id).GetApproval().GetResult().GetComment() != "both" {
			t.Errorf("build %s after Approve = %v, %v", id, st, b.build("", id).GetApproval())
		}
	}
	if err := b.ActAt(ctx, "", nil, "approve-selected", map[string]string{"builds": ids[0]}); err == nil {
		t.Error("Approve of an approved build was accepted")
	}

	d, err := buildDashboardProvider{b: b}.List(ctx, "")
	if err != nil || !d.SummaryOnly || len(d.Summary) != 1 || len(d.Items) != 0 {
		t.Fatalf("Dashboard = %+v, %v", d, err)
	}
	card := d.Summary[0]
	labels := map[string]string{}
	for _, pr := range card.Properties {
		labels[pr.Label] = pr.Value
	}
	if card.Heading != "Successful · gated" || labels["Duration"] == "" || labels["Latest build"] == "" ||
		labels["Build history"] == "" || labels["Average duration"] == "" || labels["Pass - Fail %"] != "100% - 0%" {
		t.Errorf("the card = %q %v", card.Heading, labels)
	}
}

// TestCreatePrivatePoolForm (#1854): Create private pool is Google's page,
// and its values are the WorkerPool CreateWorkerPool takes.
func TestCreatePrivatePoolForm(t *testing.T) {
	label, fields := buildWorkerPoolsProvider{}.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Label)
	}
	if label != "Create private pool" || !strings.Contains(strings.Join(names, "|"),
		"Name|Region|Series|Machine type|Machine type|Machine type|Disk size|Nested virtualization|Worker release|Release channel|Release|Network type|Project|Network|IP range|Assign external IPs") {
		t.Errorf("the form = %q %q", label, names)
	}
	w, err := poolFromValues(map[string]string{"series": "c3", "machineType-c3": "c3-standard-8", "diskSizeGb": "200",
		"nestedVirtualization": "true", "release": "channel", "channel": "stable", "network": "private",
		"networkProject": "netproj", "networkName": "vpc", "ipRange": "/26", "externalIPs": ""})
	if err != nil {
		t.Fatal(err)
	}
	wc, nc := w.GetPrivatePoolV1Config().GetWorkerConfig(), w.GetPrivatePoolV1Config().GetNetworkConfig()
	if wc.GetMachineType() != "c3-standard-8" || wc.GetDiskSizeGb() != 200 || !wc.GetEnableNestedVirtualization() ||
		wc.GetWorkerRelease() != "stable" || nc.GetPeeredNetwork() != "projects/netproj/global/networks/vpc" ||
		nc.GetPeeredNetworkIpRange() != "/26" || nc.GetEgressOption() != cbpb.PrivatePoolV1Config_NetworkConfig_NO_PUBLIC_EGRESS {
		t.Errorf("the pool = %v", w)
	}
	for _, bad := range []map[string]string{{"diskSizeGb": "50"}, {"network": "private"}, {"network": "default", "ipRange": "/24"}} {
		if _, err := poolFromValues(bad); err == nil {
			t.Errorf("poolFromValues(%v) was accepted", bad)
		}
	}
}

// TestCreateTriggerFormFollowsTheProject (#1854): Create trigger's
// choices are the asked project's, read per request, not once per console
// load: a pool made in one project is offered there and not in another.
func TestCreateTriggerFormFollowsTheProject(t *testing.T) {
	ctx := context.Background()
	a, other := freshProject("console-a"), freshProject("console-b")
	b := cloudBuildConsole(t, a)
	p := buildTriggersProvider{b: b}
	var _ console.ProjectCreateFormer = p
	choices := func(fields []console.Field, name string) string {
		for _, f := range fields {
			if f.Name == name {
				return fmt.Sprint(f.Options)
			}
		}
		return ""
	}
	before := choices(p.CreateFormFor(ctx, a), "pool")
	if _, err := (buildWorkerPoolsProvider{b: b}).Create(ctx, a, map[string]string{"name": "pp", "region": "us-central1",
		"series": "e2", "machineType-e2": "e2-medium"}); err != nil {
		t.Fatalf("Create private pool: %v", err)
	}
	if got := choices(p.CreateFormFor(ctx, a), "pool"); got == before || !strings.Contains(got, "projects/"+a+"/locations/us-central1/workerPools/pp") {
		t.Errorf("project %s's pools = %s (before %s)", a, got, before)
	}
	if got := choices(p.CreateFormFor(ctx, other), "pool"); strings.Contains(got, "/workerPools/pp") {
		t.Errorf("project %s offers %s's pool: %s", other, a, got)
	}
	if !strings.Contains(choices(p.CreateFormFor(ctx, other), "repository"), "[") {
		t.Error("Create trigger has no Repository")
	}
}
