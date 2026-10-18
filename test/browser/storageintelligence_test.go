//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestStorageIntelligenceThroughThePage (#1591), in the storage shard. On
// the Storage Intelligence page, Enable (Standard) from the project's row
// sets the edition the official StorageControl client's
// GetProjectIntelligenceConfig reads; on the configuration's page, Edit
// filter with an excluded bucket regex sets the filter it reads; Inherit
// from parent from the row returns it to INHERIT.
func TestStorageIntelligenceThroughThePage(t *testing.T) {
	needService(t, "storage-intelligence")
	ep := strings.TrimRight(strings.TrimSpace(os.Getenv(envStorage)), "/")
	if ep == "" {
		t.Skipf("%s is not set: every change is read back through the official StorageControl client", envStorage)
	}
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	ctx := context.Background()
	c, err := control.NewStorageControlRESTClient(ctx, option.WithEndpoint(ep), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	scope := "projects/" + project
	name := scope + "/locations/global/intelligenceConfig"
	t.Cleanup(func() {
		_, _ = c.UpdateProjectIntelligenceConfig(context.Background(), &controlpb.UpdateProjectIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: name, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	})
	read := func() *controlpb.IntelligenceConfig {
		t.Helper()
		got, err := c.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: name})
		if err != nil {
			t.Fatalf("GetProjectIntelligenceConfig: %v", err)
		}
		return got
	}
	waitEdition := func(want controlpb.IntelligenceConfig_EditionConfig) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); read().GetEditionConfig() != want; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("GetProjectIntelligenceConfig reads edition %v, want %v", read().GetEditionConfig(), want)
			}
		}
	}

	p := open(t)
	q := "?project=" + project
	p.navigate("/storage/intelligence" + q)
	p.cellMenu(scope, "Enable (Standard)")
	waitEdition(controlpb.IntelligenceConfig_STANDARD)

	p.navigate("/storage/intelligence" + q)
	p.clickText("#view tbody a", scope)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit filter")`)
	p.pageAction("Edit filter", map[string]string{"excludeBuckets": "^tmp-.*"}, "")
	if f := read().GetFilter(); f == nil || !strings.Contains(fmt.Sprint(f), "^tmp-.*") {
		t.Errorf("after Edit filter, GetProjectIntelligenceConfig's filter = %v; want the excluded regex ^tmp-.*", f)
	}

	p.navigate("/storage/intelligence" + q)
	p.cellMenu(scope, "Inherit from parent")
	waitEdition(controlpb.IntelligenceConfig_INHERIT)
}

// TestServiceUsageOperationsThroughThePage (#1591), in the storage shard.
// A project's Pub/Sub API disabled with the official Service Usage client
// leaves an operation; the Service Usage operations page lists it with the
// service it changed, and Delete from its row, confirmed by its name,
// forgets it, as operations.list then reads, while the API stays disabled.
func TestServiceUsageOperationsThroughThePage(t *testing.T) {
	needService(t, "serviceusage-operations")
	c, err := su.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envServiceUsage)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	const api = "pubsub.googleapis.com"
	t.Cleanup(func() {
		_, _ = c.Services.Enable("projects/"+project+"/services/"+api, &su.EnableServiceRequest{}).Do()
	})
	op, err := c.Services.Disable("projects/"+project+"/services/"+api, &su.DisableServiceRequest{}).Do()
	if err != nil || op.Name == "" {
		t.Fatalf("services.disable = %v, %v", op, err)
	}
	ops := func() []string {
		t.Helper()
		l, err := c.Operations.List().Filter("").Do()
		if err != nil {
			t.Fatalf("operations.list: %v", err)
		}
		var out []string
		for _, o := range l.Operations {
			out = append(out, o.Name)
		}
		return out
	}

	// The row is named by the operation's ID, without "operations/".
	id := strings.TrimPrefix(op.Name, "operations/")
	p := open(t)
	p.navigate("/apis/operations?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	var row string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q)).textContent`, id), &row)
	if !strings.Contains(row, api) {
		t.Errorf("the operation's row reads %q; want the service it changed, %s", row, api)
	}
	p.cellMenu(id, "Delete")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		gone := true
		for _, n := range ops() {
			gone = gone && n != op.Name
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Delete, operations.list still lists %s", op.Name)
		}
	}
	if s, err := c.Services.Get("projects/" + project + "/services/" + api).Do(); err != nil || s.State != "DISABLED" {
		t.Errorf("after deleting its operation, services.get = %v, %v; want the API still DISABLED", s, err)
	}
}
