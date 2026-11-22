package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleRequiredBucketAndCustomIndexes (#2108): Logs Storage greys out
// _Required's Edit and Delete and _Default's Delete; a bucket is created and
// edited with Custom indexes, which its page lists and the API reads back;
// _Required's page has no edit form and says why; and an index the API
// refuses is refused on the form.
func TestConsoleRequiredBucketAndCustomIndexes(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	res := srv.URL + "/api/resources/logging-buckets?project=" + project
	var l console.Listing
	getJSON200(t, res, &l)
	withheld := map[string]map[string]string{}
	for _, it := range l.Items {
		withheld[it.Name[strings.LastIndex(it.Name, "/")+1:]] = it.Withheld
	}
	if withheld["_Required"]["edit"] == "" || withheld["_Required"]["delete"] == "" || withheld["_Default"]["delete"] == "" || withheld["_Default"]["edit"] != "" {
		t.Errorf("withheld = %v", withheld)
	}
	if code, out := consolePost(t, res, map[string]string{"name": "idx", "location": "global", "retention": "30",
		"indexes": `[{"fieldPath":"jsonPayload.req_status","type":"INDEX_TYPE_INTEGER"}]`}); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, out)
	}
	name := "projects/proj-1/locations/global/buckets/idx"
	b, err := svc.admin.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name})
	if err != nil || len(b.GetIndexConfigs()) != 1 || b.GetIndexConfigs()[0].GetType() != loggingpb.IndexType_INDEX_TYPE_INTEGER {
		t.Fatalf("the API reads %v, %v", b, err)
	}
	if code, out := consolePatch(t, res, []string{name}, map[string]string{"description": "d", "retention": "30", "locked": "false",
		"indexes": `[{"fieldPath":"jsonPayload.req_status","type":"INDEX_TYPE_INTEGER"},{"fieldPath":"labels.team","type":"INDEX_TYPE_STRING"}]`}); code != http.StatusOK {
		t.Fatalf("edit = %d %s", code, out)
	}
	var d console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-buckets?project="+project+"&name="+url.QueryEscape(name), &d)
	var idx *console.Section
	for i := range d.Sections {
		if d.Sections[i].ID == "indexes" {
			idx = &d.Sections[i]
		}
	}
	if idx == nil || idx.Listing.Total != 2 || idx.Listing.Items[1].Name != "labels.team" || idx.Listing.Items[1].Fields["Type"] != "String" {
		t.Errorf("the Custom indexes tab = %+v", idx)
	}
	if d.Edit == nil || !strings.Contains(d.Edit.Fields[len(d.Edit.Fields)-1].Default, "labels.team") {
		t.Errorf("the edit form does not carry the indexes: %+v", d.Edit)
	}
	if code, out := consolePatch(t, res, []string{name}, map[string]string{"retention": "30", "locked": "false",
		"indexes": `[{"fieldPath":"nope.x","type":"INDEX_TYPE_STRING"}]`}); code == http.StatusOK || !strings.Contains(out, "not a LogEntry field") {
		t.Errorf("an undefined field path = %d %s", code, out)
	}
	var req console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-buckets?project="+project+"&name="+url.QueryEscape("projects/proj-1/locations/global/buckets/_Required"), &req)
	if req.Edit != nil || !strings.Contains(loggingDetailText(req), "You can't change the retention period of the _Required log bucket") ||
		!strings.Contains(loggingDetailText(req), "400 days") {
		t.Errorf("the _Required page = %+v", req)
	}
}

// TestConsoleTraceScopes (#2108): the Trace Scopes tab lists _Default as
// the default, greyed out for Edit and Delete; Create trace scope, Edit
// scope, Set as default and Delete scope go through the Observability API.
func TestConsoleTraceScopes(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	p := loggingTraceScopesProvider{loggingAdminScreen: loggingAdminScreen{svc: svc}}
	const project = "proj-1"
	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Type"] != "System (Default)" || l.Items[0].Withheld["edit"] == "" {
		t.Fatalf("list = %+v, %v", l, err)
	}
	name, err := p.Create(ctx, project, map[string]string{"id": "team", "description": "both",
		"projects": `[{"project":"proj-1"},{"project":"projects/proj-2"}]`})
	if err != nil || name != "projects/proj-1/locations/global/traceScopes/team" {
		t.Fatalf("create = %q, %v", name, err)
	}
	if err := p.Edit(ctx, project, strings.Split(name, "/"), map[string]string{"projects": `[{"project":"proj-3"}]`, "description": "one"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := p.Act(ctx, project, name, scopeSetDefault); err != nil {
		t.Fatalf("set as default: %v", err)
	}
	l, _ = p.List(ctx, project)
	if len(l.Items) != 2 || l.Items[1].Fields["Type"] != "Custom (Default)" || l.Items[1].Fields["Resources"] != "projects/proj-3" || l.Items[1].Withheld != nil {
		t.Errorf("after edit and set as default = %+v", l.Items)
	}
	if a := p.Actions(l.Items[1]); a[0].Unavailable == "" {
		t.Errorf("Set as default on the default = %+v", a)
	}
	d, err := p.Detail(ctx, project, strings.Split(name, "/"))
	if err != nil || d.Edit == nil {
		t.Errorf("detail = %+v, %v", d, err)
	}
	if d, _ := p.Detail(ctx, project, strings.Split("projects/proj-1/locations/global/traceScopes/_Default", "/")); d.Edit != nil {
		t.Error("_Default offers Edit scope")
	}
	if err := p.Delete(ctx, project, "projects/proj-1/locations/global/traceScopes/_Default"); err == nil {
		t.Error("_Default was deleted")
	}
	if err := p.Delete(ctx, project, name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if l, _ := p.List(ctx, project); len(l.Items) != 1 || l.Items[0].Fields["Type"] != "System (Default)" {
		t.Errorf("after delete = %+v", l.Items)
	}
}
