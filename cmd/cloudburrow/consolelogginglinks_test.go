package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
)

// TestConsoleLoggingLinks (#1292): Edit bucket turns Log Analytics on through
// UpdateBucket; the bucket's page then offers Create link, whose link the
// API's ListLinks lists and the page's Linked datasets section shows; Delete
// link removes it through DeleteLink. A bucket without Log Analytics offers
// neither.
func TestConsoleLoggingLinks(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	p := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	name, err := p.Create(ctx, project, map[string]string{"name": "linked", "location": "global", "retention": "30"})
	if err != nil {
		t.Fatal(err)
	}
	if acts := p.DetailActions(ctx, project, []string{name}); len(acts) != 0 {
		t.Errorf("a bucket without Log Analytics offers %+v", acts)
	}
	if err := p.Edit(ctx, project, []string{name}, map[string]string{"retention": "30", "analytics": "true"}); err != nil {
		t.Fatal(err)
	}
	acts := p.DetailActions(ctx, project, []string{name})
	if len(acts) != 1 || acts[0].ID != "create-link" {
		t.Fatalf("actions = %+v; want Create link", acts)
	}
	if err := p.ActAt(ctx, project, []string{name}, "create-link", map[string]string{"linkId": "my-link", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	resp, err := svc.admin.ConfigServer().ListLinks(ctx, &loggingpb.ListLinksRequest{Parent: name})
	if err != nil || len(resp.GetLinks()) != 1 || resp.GetLinks()[0].GetName() != name+"/links/my-link" {
		t.Fatalf("ListLinks = %v, %v", resp, err)
	}
	d, err := p.Detail(ctx, project, []string{name})
	if err != nil || !strings.Contains(fmt.Sprint(d.Sections), "my_link") {
		t.Errorf("the bucket page = %+v, %v; want the link's dataset", d.Sections, err)
	}
	acts = p.DetailActions(ctx, project, []string{name})
	if len(acts) != 2 || acts[1].ID != "delete-link" || acts[1].Fields[0].Options[0] != "my-link" {
		t.Fatalf("actions = %+v; want Create link and Delete link", acts)
	}
	if err := p.ActAt(ctx, project, []string{name}, "delete-link", map[string]string{"linkId": "my-link"}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := svc.admin.ConfigServer().ListLinks(ctx, &loggingpb.ListLinksRequest{Parent: name}); len(resp.GetLinks()) != 0 {
		t.Errorf("after Delete link, ListLinks = %v", resp)
	}
}
