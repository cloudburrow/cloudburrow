package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestConsolePSCEndpoints is #2141: Private Service Connect's Connected
// endpoints and Connect endpoint, which reserves the endpoint's address
// (or takes a reserved one) and adds the endpoint; its status follows the
// producer's accept list; Delete removes it.
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
	p := computePSCProvider{computeScreen{svc: svc}}
	title, fields := p.CreateForm()
	if title != "Connect endpoint" || p.CreateSubmit() != "Add endpoint" || p.CreateTitle() != "Connect endpoint" || fields[0].OptionLabels["published"] != "Published service" ||
		fields[1].Label != "Target service" || fields[2].Label != "Endpoint name" {
		t.Fatalf("Connect endpoint: %q %+v", title, fields)
	}
	if _, err := p.Create(ctx, "psc-ok", map[string]string{"name": "bad", "targetService": "projects/1/regions/us-central1/serviceAttachments/none",
		"network": "default", "region": "us-central1", "subnetwork": "default", "ipChoice": "create", "ipAssign": "auto"}); err == nil {
		t.Error("an unknown service attachment was accepted")
	}
	if a, err := p.call("GET", "psc-ok", "regions/us-central1/addresses/bad", nil, nil); err == nil {
		t.Errorf("a refused endpoint's address was kept: %v", a)
	}
	for _, proj := range []string{"psc-ok", "psc-other"} {
		name, err := p.Create(ctx, proj, map[string]string{"name": "sql", "targetService": attach, "network": "default", "region": "us-central1",
			"subnetwork": "default", "ipChoice": "create", "ipAssign": "choose", "customIp": "10.128.0.40", "globalAccess": "true"})
		if err != nil || name != "us-central1/sql" {
			t.Fatalf("Add endpoint in %s: %q %v", proj, name, err)
		}
	}
	l, err := p.List(ctx, "psc-ok")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List: %+v %v", l, err)
	}
	f := l.Items[0].Fields
	if f["Status"] != "Accepted" || f["IP address"] != "10.128.0.40" || f["Global access"] != "Enabled" || f["Target"] != attach || f["Network"] != "default" {
		t.Errorf("row: %v", f)
	}
	if l, _ := p.List(ctx, "psc-other"); len(l.Items) != 1 || l.Items[0].Fields["Status"] != "Pending" {
		t.Errorf("an endpoint off the accept list: %+v", l.Items)
	}
	if err := p.Delete(ctx, "psc-ok", "us-central1/sql"); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, "psc-ok"); len(l.Items) != 0 || !strings.Contains(l.Note, "accepted") {
		t.Errorf("after Delete: %+v", l)
	}
}
