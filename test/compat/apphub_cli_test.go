//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	apphub "cloud.google.com/go/apphub/apiv1"
	"cloud.google.com/go/apphub/apiv1/apphubpb"
	"google.golang.org/grpc/codes"
)

// TestGcloudAppHub (#2110): gcloud apphub, through gcloud-setup's apphub
// override alone, attaches the project as a service project, looks a
// deployed Cloud Run service up among the discovered services, creates a
// global application with attributes, registers the service, lists and
// describes them, does the same with a MULTI_REGIONAL application in us,
// and deletes and removes everything; the official client reads back what
// gcloud made.
func TestGcloudAppHub(t *testing.T) {
	h := New(t)
	c := appHubClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_APPHUB
	h.Endpoint(EnvRun)
	ctx := h.Context()
	g := newGcloudSession(t, h)
	detachAppHubProject(t, h, c, g.project)
	suffix := time.Now().UnixNano() % 1e6
	svcName, _ := deployAppHubResources(t, h, g.project, fmt.Sprintf("gc-ah-%d", suffix))
	run := func(a ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"apphub"}, a...), "--project", g.project, "--quiet")...)
		if err != nil {
			t.Fatalf("gcloud apphub %s: %v\n%s", strings.Join(a, " "), err, out)
		}
		return out
	}
	run("service-projects", "add", g.project, "--location", "global")
	// lookup takes no --location: it looks the --project up, in global.
	if out := run("service-projects", "lookup", "--format", "value(serviceProjectAttachment.name)"); !strings.Contains(out, "/serviceProjectAttachments/"+g.project) {
		t.Errorf("gcloud apphub service-projects lookup: %s", out)
	}
	disc := strings.TrimSpace(run("discovered-services", "lookup", "--location", "us-central1", "--uri", "//run.googleapis.com/"+svcName, "--format", "value(discoveredService.name)"))
	if !strings.Contains(disc, "/discoveredServices/") {
		t.Fatalf("gcloud apphub discovered-services lookup: %q", disc)
	}
	// A list's default format scopes name to its ID (name.scope(...)), and
	// value(name) inherits that transform.
	if out := run("discovered-services", "list", "--location", "us-central1", "--format", "value(name)"); !strings.Contains(out, disc[strings.LastIndex(disc, "/")+1:]) {
		t.Errorf("gcloud apphub discovered-services list: %s", out)
	}
	app := fmt.Sprintf("gc-app-%d", suffix)
	run("applications", "create", app, "--location", "global", "--scope-type", "GLOBAL", "--display-name", "Burrow",
		"--criticality-type", "HIGH", "--environment-type", "STAGING", "--developer-owners", "display-name=Dev,email=dev@example.com")
	name := "projects/" + g.project + "/locations/global/applications/" + app
	a, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: name})
	if err != nil || a.GetAttributes().GetCriticality().GetType() != apphubpb.Criticality_HIGH || a.GetAttributes().GetDeveloperOwners()[0].GetEmail() != "dev@example.com" {
		t.Fatalf("the application gcloud created: %v, %v", a, err)
	}
	run("applications", "services", "create", "hello", "--application", app, "--location", "global", "--discovered-service", disc)
	if out := run("applications", "services", "list", "--application", app, "--location", "global", "--format", "value(name)"); !strings.Contains(out, "hello") {
		t.Errorf("gcloud apphub applications services list: %s", out)
	}
	if out := run("applications", "describe", app, "--location", "global", "--format", "value(scope.type)"); !strings.Contains(out, "GLOBAL") {
		t.Errorf("gcloud apphub applications describe: %s", out)
	}
	run("applications", "services", "delete", "hello", "--application", app, "--location", "global")
	run("applications", "delete", app, "--location", "global")
	// A multi-regional application in us, which registers the us-central1
	// service (global-regional-applications).
	mr := fmt.Sprintf("gc-mr-%d", suffix)
	run("applications", "create", mr, "--location", "us", "--scope-type", "MULTI_REGIONAL")
	if out := run("applications", "describe", mr, "--location", "us", "--format", "value(scope.type)"); !strings.Contains(out, "MULTI_REGIONAL") {
		t.Errorf("gcloud apphub applications describe of a multi-regional application: %s", out)
	}
	run("applications", "services", "create", "hello", "--application", mr, "--location", "us", "--discovered-service", disc)
	run("applications", "services", "delete", "hello", "--application", mr, "--location", "us")
	run("applications", "delete", mr, "--location", "us")
	if _, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: name}); pmCode(err) != codes.NotFound {
		t.Errorf("after gcloud delete: %v, want NOT_FOUND", err)
	}
	run("service-projects", "remove", g.project, "--location", "global")
}

// TestTerraformAppHub (#2110): google_apphub_service_project_attachment,
// the google_apphub_discovered_service data source, google_apphub_application
// and google_apphub_service apply against this instance through
// apphub_custom_endpoint, plan clean, update the application's attributes
// in place and destroy; the official client reads back what Terraform made.
func TestTerraformAppHub(t *testing.T) {
	h := New(t)
	c := appHubClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_APPHUB
	h.Endpoint(EnvRun)
	ctx := h.Context()
	m := newTFModule(t)
	detachAppHubProject(t, h, c, m.project)
	suffix := time.Now().UnixNano() % 1e6
	svcName, _ := deployAppHubResources(t, h, m.project, fmt.Sprintf("tf-ah-%d", suffix))
	app := fmt.Sprintf("tf-app-%d", suffix)
	conf := func(env string) string {
		return fmt.Sprintf(`
resource "google_apphub_service_project_attachment" "self" {
  project                       = %[1]q
  service_project_attachment_id = %[1]q
}

data "google_apphub_discovered_service" "hello" {
  project     = %[1]q
  location    = "us-central1"
  service_uri = "//run.googleapis.com/%[3]s"
  depends_on  = [google_apphub_service_project_attachment.self]
}

resource "google_apphub_application" "app" {
  project        = %[1]q
  location       = "global"
  application_id = %[2]q
  display_name   = "Burrow"
  scope {
    type = "GLOBAL"
  }
  attributes {
    criticality {
      type = "MEDIUM"
    }
    environment {
      type = %[4]q
    }
    developer_owners {
      display_name = "Dev"
      email        = "dev@example.com"
    }
  }
}

resource "google_apphub_service" "hello" {
  project            = %[1]q
  location           = "global"
  application_id     = google_apphub_application.app.application_id
  service_id         = "hello"
  discovered_service = data.google_apphub_discovered_service.hello.name
}
`, m.project, app, svcName, env)
	}
	m.write(conf("STAGING"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/global/applications/" + app
	a, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: name})
	if err != nil || a.GetAttributes().GetEnvironment().GetType() != apphubpb.Environment_STAGING {
		t.Fatalf("the application Terraform made: %v, %v", a, err)
	}
	if s, err := c.GetService(ctx, &apphubpb.GetServiceRequest{Name: name + "/services/hello"}); err != nil || s.GetServiceReference().GetUri() != "//run.googleapis.com/"+svcName {
		t.Fatalf("the service Terraform registered: %v, %v", s, err)
	}
	m.planClean()
	m.write(conf("PRODUCTION"))
	m.apply()
	if a, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: name}); err != nil || a.GetAttributes().GetEnvironment().GetType() != apphubpb.Environment_PRODUCTION {
		t.Fatalf("after the update: %v, %v", a, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: name}); pmCode(err) != codes.NotFound {
		t.Errorf("after destroy: %v; want NOT_FOUND", err)
	}
}

// detachAppHubProject detaches project now and when the test ends, so a
// failed run leaves no attachment behind.
func detachAppHubProject(t *testing.T, h *Harness, c *apphub.Client, project string) {
	t.Helper()
	detach := func() {
		_, _ = c.DetachServiceProjectAttachment(h.Context(), &apphubpb.DetachServiceProjectAttachmentRequest{Name: "projects/" + project + "/locations/global"})
	}
	detach()
	t.Cleanup(detach)
}
