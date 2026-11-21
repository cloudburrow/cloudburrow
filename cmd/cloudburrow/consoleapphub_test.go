package main

import (
	"context"
	"strings"
	"testing"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/apphub"
)

// TestConsoleAppHub (#2110): the App Hub pages' providers over a server
// that discovers a Cloud Run service and job: Settings attaches the
// project, edits its boundary and detaches it from its host; Applications
// creates a global and a multi-regional application, registers the service
// and the job, edits each and unregisters them; Services and workloads
// lists both and opens each one's page.
func TestConsoleAppHub(t *testing.T) {
	ctx := context.Background()
	const project = "ah-console"
	svcURI := "//run.googleapis.com/projects/" + project + "/locations/us-central1/services/web"
	jobURI := "//run.googleapis.com/projects/" + project + "/locations/us-central1/jobs/batch"
	api, err := apphub.NewServer("", apphub.Options{Discover: func(_ context.Context, p string) ([]apphub.Resource, error) {
		if p != project {
			return nil, nil
		}
		return []apphub.Resource{{URI: svcURI, Project: p, Location: "us-central1"},
			{URI: jobURI, Project: p, Location: "us-central1", Workload: true}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &appHubService{api: api}
	settings, apps, disc := appHubSettingsProvider{svc: svc}, appHubProvider{svc: svc}, appHubDiscoveredProvider{svc: svc}

	actions := func() map[string]string {
		out := map[string]string{}
		for _, a := range settings.ListActions(ctx, project) {
			out[a.ID] = a.Unavailable
		}
		return out
	}
	if got := actions(); got["boundary"] != "" || !strings.Contains(got["detach-self"], "not attached") {
		t.Fatalf("Settings' actions before attaching: %v", got)
	}
	if _, err := settings.Create(ctx, project, map[string]string{"project": project}); err != nil {
		t.Fatalf("Attach projects: %v", err)
	}
	if got := actions(); got["detach-self"] != "" {
		t.Fatalf("Detach from host project once attached: %v", got)
	}
	if err := settings.ActAt(ctx, project, nil, "boundary", map[string]string{"crmNode": "projects/" + project}); err != nil {
		t.Fatalf("Edit boundary: %v", err)
	}
	if err := settings.ActAt(ctx, project, nil, "boundary", map[string]string{"crmNode": ""}); err != nil {
		t.Fatalf("Edit boundary back to manual: %v", err)
	}

	l, err := disc.List(ctx, project)
	if err != nil || len(l.Items) != 2 {
		t.Fatalf("Services and workloads: %+v, %v", l, err)
	}
	for _, it := range l.Items {
		d, err := disc.Detail(ctx, project, it.Opens)
		if err != nil || d.Unavailable != "" || !strings.Contains(appHubPageText(d), it.Name) {
			t.Fatalf("the page of %s: %+v, %v", it.Name, d, err)
		}
	}

	app, err := apps.Create(ctx, project, map[string]string{"scope": "GLOBAL", "applicationId": "shop", "criticality": "HIGH"})
	if err != nil {
		t.Fatalf("Create application: %v", err)
	}
	mr, err := apps.Create(ctx, project, map[string]string{"scope": "MULTI_REGIONAL", "multiRegion": "us", "applicationId": "shop-us"})
	if err != nil || mr != "projects/"+project+"/locations/us/applications/shop-us" {
		t.Fatalf("Create a multi-regional application: %q, %v", mr, err)
	}
	if d, _ := apps.Detail(ctx, project, []string{mr}); !strings.Contains(appHubPageText(d), "Multi-regional") {
		t.Errorf("the multi-regional application's page: %+v", d)
	}
	var opts []string
	for _, a := range apps.DetailActions(ctx, project, []string{mr}) {
		if a.ID == "register" {
			opts = a.Fields[0].Options
		}
	}
	if len(opts) != 2 {
		t.Errorf("a us application's registrable resources: %v", opts)
	}
	for _, o := range opts {
		id := "web"
		if strings.HasPrefix(o, "workload|") {
			id = "batch"
		}
		if err := apps.ActAt(ctx, project, []string{app}, "register", map[string]string{"resource": o, "id": id}); err != nil {
			t.Fatalf("Register %s: %v", o, err)
		}
	}
	for _, n := range []string{"web", "batch"} {
		if err := apps.ActAt(ctx, project, []string{app}, "edit-component", map[string]string{"name": n, "displayName": "Edited " + n, "environment": "TEST"}); err != nil {
			t.Fatalf("Edit service/workload %s: %v", n, err)
		}
	}
	if s, err := api.GetService(ctx, &apphubpb.GetServiceRequest{Name: app + "/services/web"}); err != nil || s.GetDisplayName() != "Edited web" {
		t.Fatalf("the edited service: %v, %v", s, err)
	}
	for _, n := range []string{"web", "batch"} {
		if err := apps.ActAt(ctx, project, []string{app}, "unregister", map[string]string{"name": n}); err != nil {
			t.Fatalf("Unregister %s: %v", n, err)
		}
	}
	if err := settings.ActAt(ctx, project, nil, "detach-self", nil); err != nil {
		t.Fatalf("Detach from host project: %v", err)
	}
	if got := actions(); !strings.Contains(got["detach-self"], "not attached") {
		t.Fatalf("after detaching: %v", got)
	}
}

// appHubPageText is a detail's summary and property values, one per line.
func appHubPageText(d console.Detail) string {
	var b strings.Builder
	for _, p := range d.Summary {
		b.WriteString(p.Value + "\n")
	}
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				b.WriteString(p.Value + "\n")
			}
		}
	}
	return b.String()
}
