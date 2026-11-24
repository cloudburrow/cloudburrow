package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestConsolePSCEndpoints is #2141: Private Service Connect's Connected
// endpoints tab (#2140's) connects an endpoint to another service's
// published service, a Cloud SQL instance's service attachment: Connect
// endpoint ends in Add endpoint, an unknown attachment is refused, the
// endpoint's status follows the producer's accept list, global access is
// kept, and Delete removes it.
func TestConsolePSCEndpoints(t *testing.T) {
	ctx := context.Background()
	const attach = "projects/42/regions/us-central1/serviceAttachments/a-sql"
	api, err := compute.New(compute.Options{ServiceAttachment: func(link, consumer string) (compute.PrivateService, bool, bool) {
		return compute.PrivateService{Port: 5432, TargetIP: "10.96.1.1", TargetPort: 5432}, consumer == "psc-ok", link == attach
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &computeService{api: api}
	p := computePSCEndpointsProvider{computeScreen{svc: svc}}
	title, fields := p.CreateForm()
	if title != "Connect endpoint" || p.CreateSubmit() != "Add endpoint" || p.CreateTitle() != "Connect endpoint" || fields[0].OptionLabels["published"] != "Published service" ||
		fields[1].Label != "Target service" || fields[2].Label != "Endpoint name" {
		t.Fatalf("Connect endpoint: %q %+v", title, fields)
	}
	if _, err := p.Create(ctx, "psc-ok", map[string]string{"target": "published", "name": "bad", "targetService": "projects/1/regions/us-central1/serviceAttachments/none",
		"network": "default", "region": "us-central1", "subnetwork": "default"}); err == nil {
		t.Error("an unknown service attachment was accepted")
	}
	for _, proj := range []string{"psc-ok", "psc-other"} {
		if _, err := p.Create(ctx, proj, map[string]string{"target": "published", "name": "sql", "targetService": attach, "network": "default", "region": "us-central1",
			"subnetwork": "default", "ipAddress": "10.128.0.40", "allowPscGlobalAccess": "true"}); err != nil {
			t.Fatalf("Add endpoint in %s: %v", proj, err)
		}
	}
	l, err := p.List(ctx, "psc-ok")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List: %+v %v", l, err)
	}
	f := l.Items[0].Fields
	if f["Status"] != "Accepted" || f["IP address"] != "10.128.0.40" || f["Target"] != "a-sql" || f["Network"] != "default" || f["Scope"] != "us-central1" {
		t.Errorf("row: %v", f)
	}
	if r, err := p.call("GET", "psc-ok", "regions/us-central1/forwardingRules/sql", nil, nil); err != nil || r["allowPscGlobalAccess"] != true {
		t.Errorf("global access: %v %v", r, err)
	}
	if l, _ := p.List(ctx, "psc-other"); len(l.Items) != 1 || l.Items[0].Fields["Status"] != "Pending" {
		t.Errorf("an endpoint off the accept list: %+v", l.Items)
	}
	if err := p.ActAt(ctx, "psc-ok", []string{"us-central1", "sql"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, "psc-ok"); len(l.Items) != 0 {
		t.Errorf("after Delete: %+v", l)
	}
}
