package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// fakeInfoInstanceAdmin answers GetInstance with one instance and records
// UpdateInstance.
type fakeInfoInstanceAdmin struct {
	instancepb.UnimplementedInstanceAdminServer
	inst *instancepb.Instance
	got  *instancepb.UpdateInstanceRequest
}

func (f *fakeInfoInstanceAdmin) GetInstance(context.Context, *instancepb.GetInstanceRequest) (*instancepb.Instance, error) {
	return f.inst, nil
}

func (f *fakeInfoInstanceAdmin) UpdateInstance(_ context.Context, r *instancepb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	f.got = r
	res, _ := anypb.New(r.GetInstance())
	return &longrunningpb.Operation{Name: "op", Done: true, Result: &longrunningpb.Operation_Response{Response: res}}, nil
}

// TestConsoleSpannerInfoPanelAndUpgrade (#1859): Google's Instances list
// opens a checked instance's Info panel with a Labels tab, where its labels
// are saved; the instance's page has Upgrade beside its edition, Update
// edition offering only the higher editions and ending in Save. Neither is a
// button of its own on the instance's page.
func TestConsoleSpannerInfoPanelAndUpgrade(t *testing.T) {
	f := &fakeInfoInstanceAdmin{inst: &instancepb.Instance{Name: "projects/proj/instances/orders",
		Edition: instancepb.Instance_STANDARD, Labels: map[string]string{"team": "ops", "env": "dev"}}}
	g := grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(g, f)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := spannerProvider{endpoint: ln.Addr().String()}
	ctx := context.Background()

	tab := spannerLabelsTab(f.inst.GetLabels())
	if tab.Label != "Labels" || len(tab.Properties) != 2 || tab.Properties[0].Label != "env" || tab.Form == nil ||
		tab.Form.Submit != "Save" || !tab.Form.Linked || tab.Form.Fields[0].Type != "map" ||
		!strings.Contains(tab.Form.Fields[0].Default, `"team":"ops"`) {
		t.Errorf("the Labels tab = %+v", tab)
	}
	if empty := spannerLabelsTab(nil); len(empty.Properties) != 0 || empty.Empty == "" || empty.Form.Fields[0].Default != "" {
		t.Errorf("an instance with no labels has the tab %+v", empty)
	}

	for e, want := range map[instancepb.Instance_Edition]string{
		instancepb.Instance_EDITION_UNSPECIFIED: "Enterprise,Enterprise Plus",
		instancepb.Instance_STANDARD:            "Enterprise,Enterprise Plus",
		instancepb.Instance_ENTERPRISE:          "Enterprise Plus",
		instancepb.Instance_ENTERPRISE_PLUS:     "",
	} {
		if got := strings.Join(spannerHigherEditions(e), ","); got != want {
			t.Errorf("the editions above %v = %q, want %q", e, got, want)
		}
		prop := spannerEditionProperty(e)
		if (want == "") != (prop.Action == "") || (want != "" && prop.ActionText != "Upgrade") {
			t.Errorf("the Edition property of %v = %+v", e, prop)
		}
	}

	linked := map[string]bool{}
	for _, a := range p.DetailActions(ctx, "proj", []string{"orders"}) {
		if a.ID == spannerLabelsActionID || a.ID == spannerUpgradeActionID {
			linked[a.ID] = a.Linked
		}
	}
	if !linked[spannerLabelsActionID] || !linked[spannerUpgradeActionID] {
		t.Errorf("the instance page's linked actions = %v", linked)
	}

	if err := p.ActAt(ctx, "proj", []string{"orders"}, spannerLabelsActionID, map[string]string{"labels": `{"env":"prod"}`}); err != nil {
		t.Fatal(err)
	}
	if r := f.got; r.GetInstance().GetName() != "projects/proj/instances/orders" || strings.Join(r.GetFieldMask().GetPaths(), ",") != "labels" ||
		len(r.GetInstance().GetLabels()) != 1 || r.GetInstance().GetLabels()["env"] != "prod" {
		t.Errorf("Save on the Labels tab sent %v", r)
	}

	if err := p.ActAt(ctx, "proj", []string{"orders"}, spannerUpgradeActionID, map[string]string{"edition": "Enterprise Plus"}); err != nil {
		t.Fatal(err)
	}
	if r := f.got; strings.Join(r.GetFieldMask().GetPaths(), ",") != "edition" || r.GetInstance().GetEdition() != instancepb.Instance_ENTERPRISE_PLUS {
		t.Errorf("Update edition sent %v", r)
	}
	f.got = nil
	f.inst.Edition = instancepb.Instance_ENTERPRISE
	for _, down := range []string{"Standard", "Enterprise", "Premium"} {
		if err := p.ActAt(ctx, "proj", []string{"orders"}, spannerUpgradeActionID, map[string]string{"edition": down}); err == nil || f.got != nil {
			t.Errorf("Update edition to %s from Enterprise = %v, sent %v", down, err, f.got)
		}
	}
}
