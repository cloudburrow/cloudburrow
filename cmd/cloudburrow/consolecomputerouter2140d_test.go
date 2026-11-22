package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleRouterNamedSets2140d: a Cloud Router's Named sets tab (#2140):
// Create named set of type Prefix with its elements, the tab's row, Edit
// elements under the set's fingerprint, a malformed element refused with
// the API's message, a duplicate name refused, and Delete.
func TestConsoleRouterNamedSets2140d(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "nsets-2140"
	routers := computeRoutersProvider{screen}
	if _, err := screen.call("POST", project, "regions/us-central1/routers", nil, map[string]any{"name": "r1",
		"network": "global/networks/default", "bgp": map[string]any{"asn": float64(64512)}}); err != nil {
		t.Fatal(err)
	}
	rpath := []string{"us-central1", "r1"}
	var create *console.Action
	for _, a := range routers.DetailActions(ctx, project, rpath) {
		if a.ID == "createNamedSet" {
			create = &a
		}
	}
	if create == nil || vpcFieldNames(create.Fields) != "setName,setType,setDescription,elements" || create.Fields[1].Default != "NAMED_SET_TYPE_PREFIX" {
		t.Fatalf("Create named set = %+v", create)
	}
	if err := routers.ActAt(ctx, project, rpath, "createNamedSet", map[string]string{"setName": "allowed-prefixes", "setType": "NAMED_SET_TYPE_PREFIX",
		"setDescription": "ours", "elements": "prefix('10.0.0.0/20').orLonger()\n\nprefix('10.1.0.0/16')\n"}); err != nil {
		t.Fatal(err)
	}
	if err := routers.ActAt(ctx, project, rpath, "createNamedSet", map[string]string{"setName": "allowed-prefixes", "setType": "NAMED_SET_TYPE_PREFIX"}); err == nil {
		t.Error("Create named set replaced an existing set")
	}
	if err := routers.ActAt(ctx, project, rpath, "createNamedSet", map[string]string{"setName": "bad", "setType": "NAMED_SET_TYPE_COMMUNITY",
		"elements": "'64500:100"}); err == nil || !strings.Contains(err.Error(), "elements[0]") {
		t.Errorf("an unclosed string element: %v", err)
	}
	section := func() console.Section {
		d, err := routers.Detail(ctx, project, rpath)
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range d.Sections {
			if s.ID == namedSetsTab {
				if i == 0 || d.Sections[i-1].ID != "routepolicies" {
					t.Errorf("Named sets is not beside Route policies: %d", i)
				}
				return s
			}
		}
		t.Fatalf("no Named sets tab: %+v", d.Sections)
		return console.Section{}
	}
	row := vpcRow(section().Listing, "allowed-prefixes")
	if row == nil || row.Fields["Type"] != "Prefix" || row.Fields["Elements"] != "prefix('10.0.0.0/20').orLonger(), prefix('10.1.0.0/16')" ||
		row.Fields["Description"] != "ours" || strings.Join(row.Target, "/") != "us-central1/r1/namedsets/allowed-prefixes" || len(row.Actions) != 2 {
		t.Fatalf("named set row = %+v", row)
	}
	if row.Actions[0].Fields[1].Default != "prefix('10.0.0.0/20').orLonger()\nprefix('10.1.0.0/16')" {
		t.Errorf("Edit elements' default = %q", row.Actions[0].Fields[1].Default)
	}
	// The console offers a row's actions only when DetailActions at its
	// path lists them.
	if acts := routers.DetailActions(ctx, project, row.Target); len(acts) != 2 || acts[0].ID != "editNamedSet" || acts[1].ID != "deleteNamedSet" {
		t.Errorf("DetailActions at the row = %+v", acts)
	}
	if err := routers.ActAt(ctx, project, row.Target, "editNamedSet", map[string]string{"setDescription": "", "elements": "prefix('192.168.0.0/16')"}); err != nil {
		t.Fatal(err)
	}
	row = vpcRow(section().Listing, "allowed-prefixes")
	if row == nil || row.Fields["Elements"] != "prefix('192.168.0.0/16')" || row.Fields["Description"] != "—" {
		t.Fatalf("after Edit elements = %+v", row)
	}
	if err := routers.ActAt(ctx, project, row.Target, "editNamedSet", map[string]string{"elements": "prefix(("}); err == nil {
		t.Error("Edit elements took an unbalanced expression")
	}
	if err := routers.ActAt(ctx, project, row.Target, "deleteNamedSet", nil); err != nil {
		t.Fatal(err)
	}
	if l := section().Listing; len(l.Items) != 0 {
		t.Fatalf("after Delete = %+v", l.Items)
	}
	if err := routers.ActAt(ctx, project, row.Target, "deleteNamedSet", nil); err == nil {
		t.Error("Delete of a set that is gone")
	}
}
