//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
)

// EnvServiceUsage is the Service Usage API v1 endpoint (#1352).
const EnvServiceUsage = "CLOUDBURROW_TEST_SERVICEUSAGE"

func serviceUsageClient(t *testing.T, h *Harness) *su.Service {
	t.Helper()
	c, err := su.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvServiceUsage)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// serviceState reads one API's state in a project.
func serviceState(t *testing.T, h *Harness, c *su.Service, project, api string) string {
	t.Helper()
	s, err := c.Services.Get("projects/" + project + "/services/" + api).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("services.get %s in %s: %v", api, project, err)
	}
	return s.State
}

// covers: serviceusage.services.list, serviceusage.services.get, serviceusage.services.batchGet, serviceusage.services.enable, serviceusage.services.batchEnable, serviceusage.services.disable, serviceusage.operations.get
//
// TestServiceUsageServices (#1352), with google.golang.org/api/serviceusage/v1:
// every API the instance serves is listed ENABLED under its googleapis.com
// name; disable is stored (DISABLED, per project) and enable restores it,
// each a done operation operations.get answers; batchGet and batchEnable
// work on several; and enabling an API the instance does not serve is
// refused with the reason, changing nothing.
func TestServiceUsageServices(t *testing.T) {
	h := New(t)
	c := serviceUsageClient(t, h)
	ctx := h.Context()
	p := "projects/" + h.Project()

	list, err := c.Services.List(p).Filter("state:ENABLED").Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	enabled := map[string]bool{}
	for _, s := range list.Services {
		enabled[s.Config.Name] = true
		if !strings.HasSuffix(s.Name, "/services/"+s.Config.Name) || s.Config.Title == "" {
			t.Errorf("service %+v: want projects/N/services/%s and a title", s, s.Config.Name)
		}
	}
	for _, want := range []string{"storage.googleapis.com", "cloudresourcemanager.googleapis.com", "serviceusage.googleapis.com"} {
		if !enabled[want] {
			t.Errorf("%s is not listed ENABLED: %v", want, enabled)
		}
	}

	op, err := c.Services.Disable(p+"/services/storage.googleapis.com",
		&su.DisableServiceRequest{CheckIfServiceHasUsage: "SKIP"}).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("disable = %+v, %v; want a done operation", op, err)
	}
	if got, err := c.Operations.Get(op.Name).Context(ctx).Do(); err != nil || !got.Done || got.Error != nil {
		t.Errorf("operations.get %s = %+v, %v", op.Name, got, err)
	}
	if st := serviceState(t, h, c, h.Project(), "storage.googleapis.com"); st != "DISABLED" {
		t.Errorf("after disable storage is %s", st)
	}
	disabled, err := c.Services.List(p).Filter("state:DISABLED").Context(ctx).Do()
	if err != nil || len(disabled.Services) != 1 || disabled.Services[0].Config.Name != "storage.googleapis.com" {
		t.Errorf("list state:DISABLED = %+v, %v", disabled, err)
	}

	// Not enforced, and per project: another project still has it.
	if st := serviceState(t, h, c, h.Project()+"-b", "storage.googleapis.com"); st != "ENABLED" {
		t.Errorf("another project's storage is %s", st)
	}

	// An API the instance does not serve: refused, with the reason, and the
	// batch it is in changes nothing.
	unserved := ""
	for _, api := range []string{"spanner.googleapis.com", "redis.googleapis.com", "run.googleapis.com"} {
		if !enabled[api] {
			unserved = api
			break
		}
	}
	if unserved != "" {
		_, err := c.Services.BatchEnable(p, &su.BatchEnableServicesRequest{
			ServiceIds: []string{"storage.googleapis.com", unserved}}).Context(ctx).Do()
		if err == nil || !strings.Contains(err.Error(), "--services") {
			t.Errorf("batchEnable with %s = %v; want a refusal naming --services", unserved, err)
		}
		if st := serviceState(t, h, c, h.Project(), "storage.googleapis.com"); st != "DISABLED" {
			t.Errorf("a refused batch changed storage to %s", st)
		}
	}
	if _, err := c.Services.Enable(p+"/services/compute.googleapis.com", &su.EnableServiceRequest{}).Context(ctx).Do(); err == nil ||
		!strings.Contains(err.Error(), "not served by CloudBurrow") {
		t.Errorf("enable compute.googleapis.com = %v; want a refusal", err)
	}

	op, err = c.Services.BatchEnable(p, &su.BatchEnableServicesRequest{
		ServiceIds: []string{"storage.googleapis.com", "cloudresourcemanager.googleapis.com"}}).Context(ctx).Do()
	if err != nil || !op.Done || !strings.Contains(string(op.Response), "BatchEnableServicesResponse") {
		t.Fatalf("batchEnable = %+v, %v", op, err)
	}
	bg, err := c.Services.BatchGet(p).Names(p+"/services/storage.googleapis.com", p+"/services/cloudresourcemanager.googleapis.com").Context(ctx).Do()
	if err != nil || len(bg.Services) != 2 {
		t.Fatalf("batchGet = %+v, %v", bg, err)
	}
	for _, s := range bg.Services {
		if s.State != "ENABLED" {
			t.Errorf("after batchEnable %s is %s", s.Config.Name, s.State)
		}
	}
	if op, err := c.Services.Enable(p+"/services/storage.googleapis.com", &su.EnableServiceRequest{}).Context(ctx).Do(); err != nil || !op.Done {
		t.Errorf("enable of an enabled API = %+v, %v; want a done operation", op, err)
	}
}

// covers: serviceusage.services.list, serviceusage.services.get, serviceusage.services.enable, serviceusage.services.batchEnable, serviceusage.services.disable
//
// TestTerraformProjectService (#1352): google_project_service, through
// `cloudburrow terraform` and its service_usage_custom_endpoint, enables
// two APIs, reads back ENABLED with the official client, plans clean, and
// on destroy disables the one with disable_on_destroy and leaves the other.
// An API the instance does not serve fails the apply with the reason.
// Measured with TF_LOG=DEBUG (hashicorp/google 8.4.0): the provider reads
// with services.list (filter=state:ENABLED, pageSize=200, fields=
// services/name), enables with :enable or, for several at once,
// :batchEnable, disables with :disable, and reads the project from
// Resource Manager v1 after each apply. A done operation is not polled.
func TestTerraformProjectService(t *testing.T) {
	h := New(t)
	c := serviceUsageClient(t, h)
	m := newTFModule(t)
	// The provider reads the project from Resource Manager after each
	// apply, and drops the resource if the project is not there.
	project := v1ID(h, "tfsu-")
	rm := rmV1(t, h)
	if _, err := rm.Projects.Create(&crmv1.Project{ProjectId: project}).Context(h.Context()).Do(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rm.Projects.Delete(project).Do() })
	module := func(extra string) string {
		return fmt.Sprintf(`
resource "google_project_service" "storage" {
  project            = %q
  service            = "storage.googleapis.com"
  disable_on_destroy = true
}

resource "google_project_service" "rm" {
  project            = %q
  service            = "cloudresourcemanager.googleapis.com"
  disable_on_destroy = false
}
%s`, project, project, extra)
	}
	// Disabled first, so the apply has something to enable.
	if _, err := c.Services.Disable("projects/"+project+"/services/storage.googleapis.com", &su.DisableServiceRequest{}).Context(h.Context()).Do(); err != nil {
		t.Fatal(err)
	}
	m.write(module(""))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	for _, api := range []string{"storage.googleapis.com", "cloudresourcemanager.googleapis.com"} {
		if st := serviceState(t, h, c, project, api); st != "ENABLED" {
			t.Errorf("after apply %s is %s", api, st)
		}
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if st := serviceState(t, h, c, project, "storage.googleapis.com"); st != "DISABLED" {
		t.Errorf("after destroy storage (disable_on_destroy) is %s", st)
	}
	if st := serviceState(t, h, c, project, "cloudresourcemanager.googleapis.com"); st != "ENABLED" {
		t.Errorf("after destroy cloudresourcemanager (kept) is %s", st)
	}

	m.write(module(fmt.Sprintf(`
resource "google_project_service" "compute" {
  project = %q
  service = "compute.googleapis.com"
}
`, project)))
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil || !strings.Contains(out, "not served by CloudBurrow") {
		t.Errorf("apply enabling compute.googleapis.com = %v; want the refusal:\n%s", err, lastLines(out, 30))
	}
}

// covers: serviceusage.services.list, serviceusage.services.get, serviceusage.services.enable, serviceusage.services.disable
//
// TestGcloudServices (#1352): gcloud, configured by `cloudburrow
// gcloud-setup` alone, lists the enabled APIs, disables one, lists it as
// available but not enabled, enables it again, and is refused an API the
// instance does not serve. Every proxy variable points at the egress guard.
// Measured with --log-http (gcloud 587.0.0): services list is
// services.list (filter=state:ENABLED for --enabled, none for --available,
// pageSize=200), and enable and disable of one API are :enable and
// :disable; a done operation is not polled.
func TestGcloudServices(t *testing.T) {
	h := New(t)
	c := serviceUsageClient(t, h)
	g := newGcloudSession(t, h)
	project := h.Project()
	enabled := func() string {
		return g.must("services", "list", "--enabled", "--project", project, "--format=value(config.name)")
	}
	if out := enabled(); !strings.Contains(out, "storage.googleapis.com") || !strings.Contains(out, "serviceusage.googleapis.com") {
		t.Fatalf("services list --enabled:\n%s", out)
	}
	g.must("services", "disable", "storage.googleapis.com", "--project", project, "--force")
	if st := serviceState(t, h, c, project, "storage.googleapis.com"); st != "DISABLED" {
		t.Errorf("after gcloud services disable storage is %s", st)
	}
	if out := enabled(); strings.Contains(out, "storage.googleapis.com") {
		t.Errorf("a disabled API is listed enabled:\n%s", out)
	}
	if out := g.must("services", "list", "--available", "--project", project, "--format=value(config.name)"); !strings.Contains(out, "storage.googleapis.com") {
		t.Errorf("services list --available lacks storage:\n%s", out)
	}
	g.must("services", "enable", "storage.googleapis.com", "--project", project)
	if st := serviceState(t, h, c, project, "storage.googleapis.com"); st != "ENABLED" {
		t.Errorf("after gcloud services enable storage is %s", st)
	}
	if out, err := g.run(nil, "services", "enable", "compute.googleapis.com", "--project", project); err == nil ||
		!strings.Contains(out, "not served by CloudBurrow") {
		t.Errorf("gcloud services enable compute = %v; want the refusal:\n%s", err, out)
	}
}

// covers: serviceusage.operations.list, serviceusage.operations.cancel, serviceusage.operations.delete, serviceusage.operations.get
//
// TestServiceUsageOperations (#1530), with google.golang.org/api/serviceusage/v1:
// operations.list answers the operations a disable and an enable returned,
// in order and paged; cancel of a done operation answers Empty and leaves
// it done; delete forgets it, so get and a second delete are 404; and a
// filter, whose grammar Google does not document for this API, is refused
// 501 saying so.
func TestServiceUsageOperations(t *testing.T) {
	h := New(t)
	c := serviceUsageClient(t, h)
	ctx := h.Context()
	p := "projects/" + h.Project()
	dis, err := c.Services.Disable(p+"/services/storage.googleapis.com", &su.DisableServiceRequest{}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	en, err := c.Services.Enable(p+"/services/storage.googleapis.com", &su.EnableServiceRequest{}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	// Other tests share the instance, so look for these two among them.
	var listed []string
	call := c.Operations.List().PageSize(1)
	if err := call.Pages(ctx, func(r *su.ListOperationsResponse) error {
		if len(r.Operations) > 1 {
			return fmt.Errorf("a page of size 1 held %d operations", len(r.Operations))
		}
		for _, op := range r.Operations {
			if op.Name == dis.Name || op.Name == en.Name {
				listed = append(listed, op.Name)
				if !op.Done {
					return fmt.Errorf("%s is listed not done", op.Name)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("operations.list: %v", err)
	}
	if len(listed) != 2 || listed[0] != dis.Name || listed[1] != en.Name {
		t.Fatalf("operations.list held %v; want the disable then the enable", listed)
	}

	if _, err := c.Operations.Cancel(dis.Name, &su.CancelOperationRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if op, err := c.Operations.Get(dis.Name).Context(ctx).Do(); err != nil || !op.Done || op.Error != nil {
		t.Errorf("get after cancel = %+v, %v; want it done", op, err)
	}
	if _, err := c.Operations.Delete(dis.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Operations.Get(dis.Name).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("get of a deleted operation = %v; want 404", err)
	}
	if _, err := c.Operations.Delete(dis.Name).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("second delete = %v; want 404", err)
	}
	// The change the deleted operation made stays.
	if st := serviceState(t, h, c, h.Project(), "storage.googleapis.com"); st != "ENABLED" {
		t.Errorf("after deleting an operation storage is %s", st)
	}
	if _, err := c.Operations.List().Filter("done=true").Context(ctx).Do(); httpCode(err) != http.StatusNotImplemented ||
		!strings.Contains(err.Error(), "filter") {
		t.Errorf("list with a filter = %v; want 501 naming the filter", err)
	}
}
