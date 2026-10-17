package resourcemanager

import (
	"context"
	"net/http"
	"testing"

	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Folder capabilities through Google's generated v3 client: unset reads
// false, a patch is a done operation and the folder lists it, an unknown
// capability is refused.
func TestFolderCapabilities(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	c, err := crmv3.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	f, err := reg.CreateFolder(OrganizationName, "Apps")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name + "/capabilities/app-management"
	if got, err := c.Folders.Capabilities.Get(name).Context(ctx).Do(); err != nil || got.Name != name || got.Value {
		t.Fatalf("get unset = %+v, %v", got, err)
	}
	op, err := c.Folders.Capabilities.Patch(name, &crmv3.Capability{Name: name, Value: true}).UpdateMask("value").Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("patch = %+v, %v", op, err)
	}
	if got, err := c.Operations.Get(op.Name).Context(ctx).Do(); err != nil || !got.Done || len(got.Response) == 0 {
		t.Errorf("operations.get = %+v, %v", got, err)
	}
	if got, err := c.Folders.Capabilities.Get(name).Context(ctx).Do(); err != nil || !got.Value {
		t.Errorf("get after patch = %+v, %v", got, err)
	}
	if got, err := c.Folders.Get(f.Name).Context(ctx).Do(); err != nil || len(got.ConfiguredCapabilities) != 1 || got.ConfiguredCapabilities[0] != name {
		t.Errorf("folder = %+v, %v", got, err)
	}
	// The management project (#1532): in the folder, named by it, and
	// marked as one.
	mp := "projects/" + ManagementProjectID(f.ID())
	if got, err := c.Folders.Get(f.Name).Context(ctx).Do(); err != nil || got.ManagementProject != mp {
		t.Errorf("folder managementProject = %+v, %v; want %s", got, err, mp)
	}
	if got, err := c.Projects.Get(mp).Context(ctx).Do(); err != nil || !got.IsManagementProject || got.Parent != f.Name ||
		len(got.ConfiguredCapabilities) != 1 || got.ConfiguredCapabilities[0] != name {
		t.Errorf("management project = %+v, %v", got, err)
	}
	if _, err := reg.DeleteFolder(f.Name); err == nil {
		t.Errorf("deleting an app-enabled folder succeeded; its management project is in it")
	}
	if _, err := c.Folders.Capabilities.Patch(f.Name+"/capabilities/other", &crmv3.Capability{Value: true}).Context(ctx).Do(); err == nil || err.(*googleapi.Error).Code != 400 {
		t.Errorf("an unknown capability = %v; want 400", err)
	}
	if _, err := c.Folders.Capabilities.Get("folders/999/capabilities/app-management").Context(ctx).Do(); err == nil || err.(*googleapi.Error).Code != 404 {
		t.Errorf("a missing folder = %v; want 404", err)
	}
	if _, err := c.Folders.Capabilities.Patch(name, &crmv3.Capability{Value: false}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Folders.Get(f.Name).Context(ctx).Do(); len(got.ConfiguredCapabilities) != 0 || got.ManagementProject != "" {
		t.Errorf("folder after unset = %+v", got)
	}
	// Disabling deletes the management project permanently.
	if _, err := reg.Get(ManagementProjectID(f.ID())); err == nil {
		t.Errorf("the management project outlived app management")
	}
	if _, err := reg.DeleteFolder(f.Name); err != nil {
		t.Errorf("DeleteFolder after unset: %v", err)
	}
}
