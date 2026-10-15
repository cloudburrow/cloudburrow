package main

import (
	"context"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
)

// Edit queue's HTTP target section (#1282) saves the queue's http_target
// through UpdateQueue, which GetQueue reads back; the form and the
// Configuration tab are then filled from it, and an empty section clears
// it. A URI override that is no http(s) URL is refused.
func TestTasksQueueHTTPTargetThroughTheEditForm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)
	d, err := p.Detail(ctx, "edit-proj", []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("queue detail offers no edit form: %v", err)
	}
	values := submittedValues(d.Edit)
	if values["targetURI"] != "" {
		t.Errorf("a queue with no http_target prefills URI override %q", values["targetURI"])
	}
	values["targetURI"] = "http://worker.internal:8081/hook?v=2"
	values["targetMode"] = "IF_NOT_EXISTS"
	values["targetMethod"] = "PUT"
	values["targetHeaders"] = `{"X-Who":"queue"}`
	values["targetOidcAccount"] = "invoker@edit-proj.iam.gserviceaccount.com"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	q, err := p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ht := q.GetHttpTarget()
	o := ht.GetUriOverride()
	if o.GetHost() != "worker.internal" || o.GetPort() != 8081 || o.GetScheme() != cloudtaskspb.UriOverride_HTTP ||
		o.GetPathOverride().GetPath() != "/hook" || o.GetQueryOverride().GetQueryParams() != "v=2" ||
		o.GetUriOverrideEnforceMode() != cloudtaskspb.UriOverride_IF_NOT_EXISTS || ht.GetHttpMethod() != cloudtaskspb.HttpMethod_PUT ||
		len(ht.GetHeaderOverrides()) != 1 || ht.GetOidcToken().GetServiceAccountEmail() != values["targetOidcAccount"] {
		t.Errorf("http_target read back as %v", ht)
	}
	d, _ = p.Detail(ctx, "edit-proj", []string{name})
	if got := submittedValues(d.Edit); got["targetURI"] != values["targetURI"] || got["targetMode"] != "IF_NOT_EXISTS" ||
		got["targetHeaders"] != `{"X-Who":"queue"}` || got["targetMethod"] != "PUT" {
		t.Errorf("the form after saving is prefilled with %v", got)
	}

	for _, k := range []string{"targetURI", "targetMode", "targetMethod", "targetHeaders", "targetOidcAccount"} {
		values[k] = ""
	}
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatal(err)
	}
	if q, _ := p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name}); q.GetHttpTarget() != nil {
		t.Errorf("an empty HTTP target section left %v", q.GetHttpTarget())
	}
	values["targetURI"] = "ftp://x"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err == nil {
		t.Error("an ftp URI override was accepted")
	}
}
