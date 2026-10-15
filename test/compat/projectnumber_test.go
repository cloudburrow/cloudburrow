//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/compute/metadata"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
)

// TestOneProjectNumberEverywhere (#1211).
//
// A project's number is the same wherever it appears. The metadata server's
// project, read with the official metadata client (project-id and
// numeric-project-id), is looked up with the official Resource Manager v1
// client, whose projectNumber must be the metadata server's number, and the
// v3 client, whose name must be projects/{that number} and which must answer a
// GetProject naming the project by that number. The Cloud Storage service
// agent the official storage client reads (projects.serviceAccount.get) is
// service-{that number}@gs-project-accounts.iam.gserviceaccount.com, and the
// console's project page shows that number. BigQuery's
// projects.getServiceAccount is not served (#1205), so it is not compared.
//
// covers: google.cloud.resourcemanager.v3.Projects/GetProject
func TestOneProjectNumberEverywhere(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	// GCE_METADATA_HOST is how the official client is pointed at a metadata
	// server other than 169.254.169.254.
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(h.Endpoint(EnvMetadata), "http://"))
	meta := metadata.NewClient(&http.Client{})

	project, err := meta.ProjectIDWithContext(ctx)
	if err != nil {
		t.Fatalf("metadata ProjectID: %v", err)
	}
	number, err := meta.NumericProjectIDWithContext(ctx)
	if err != nil {
		t.Fatalf("metadata NumericProjectID: %v", err)
	}
	if strings.Trim(number, "0") == "" {
		t.Fatalf("the metadata server's numeric-project-id is %q, a placeholder", number)
	}

	// Resource Manager v1: the project the metadata server serves, registered
	// here when it is not already.
	v1 := rmV1(t, h)
	p, err := v1.Projects.Get(project).Context(ctx).Do()
	if e, ok := err.(*googleapi.Error); ok && e.Code == http.StatusNotFound {
		if _, err := v1.Projects.Create(&crmv1.Project{ProjectId: project}).Context(ctx).Do(); err != nil {
			t.Fatalf("v1 projects.create %s: %v", project, err)
		}
		t.Cleanup(func() { _, _ = v1.Projects.Delete(project).Do() })
		p, err = v1.Projects.Get(project).Context(ctx).Do()
	}
	if err != nil {
		t.Fatalf("v1 projects.get %s: %v", project, err)
	}
	if got := fmt.Sprint(p.ProjectNumber); got != number {
		t.Errorf("v1 projectNumber = %s, the metadata server's numeric-project-id %s", got, number)
	}

	// Resource Manager v3: the name carries the number, and the number names it.
	v3, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer v3.Close()
	byID, err := v3.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
	if err != nil {
		t.Fatalf("v3 GetProject(projects/%s): %v", project, err)
	}
	if byID.GetName() != "projects/"+number {
		t.Errorf("v3 name = %q, want projects/%s", byID.GetName(), number)
	}
	if byNumber, err := v3.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + number}); err != nil || byNumber.GetProjectId() != project {
		t.Errorf("v3 GetProject(projects/%s) = %v, %v; want project %s", number, byNumber, err, project)
	}

	// The Cloud Storage service agent.
	email, err := storageClient(t, h).ServiceAccount(ctx, project)
	if err != nil {
		t.Fatalf("storage ServiceAccount: %v", err)
	}
	if want := "service-" + number + "@gs-project-accounts.iam.gserviceaccount.com"; email != want {
		t.Errorf("the Cloud Storage service agent is %q, want %q", email, want)
	}

	// The console's project page.
	var page struct {
		Summary []struct{ Label, Value string }
	}
	consoleJSON(t, consoleAddr(t, h), http.MethodGet,
		"/api/detail/projects?"+url.Values{"project": {project}, "name": {project}}.Encode(), "", &page)
	shown := ""
	for _, s := range page.Summary {
		if s.Label == "Project number" {
			shown = s.Value
		}
	}
	if shown != number {
		out, _ := json.Marshal(page)
		t.Errorf("the console's project page shows the number %q, want %s: %s", shown, number, out)
	}
}
