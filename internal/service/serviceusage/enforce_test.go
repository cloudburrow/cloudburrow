package serviceusage

import (
	"context"
	"strings"
	"testing"

	su "google.golang.org/api/serviceusage/v1"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// A disabled API is what every gate refuses (#1914), until it is enabled.
func TestDisabledIsTheGatesDecision(t *testing.T) {
	srv, c := newTestServer(t, store.NewMemory())
	ctx := context.Background()
	if _, off := srv.Disabled(ctx, "p1", "storage.googleapis.com"); off {
		t.Fatal("an API starts enabled")
	}
	if _, err := c.Services.Disable("projects/p1/services/storage.googleapis.com", &su.DisableServiceRequest{}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	d, off := srv.Disabled(ctx, "p1", "storage.googleapis.com")
	if !off || d.Number != resource.ProjectNumber("p1") || d.Title != "Cloud Storage API" {
		t.Fatalf("after disable: %+v %v", d, off)
	}
	if _, off := srv.Disabled(ctx, "p2", "storage.googleapis.com"); off {
		t.Fatal("another project's API is disabled")
	}
	snap, err := srv.DisabledSnapshot()
	if err != nil || len(snap.Projects) != 1 || snap.Projects[0].ID != "p1" {
		t.Fatalf("snapshot %+v, %v", snap, err)
	}
	if _, off := snap.Decide("p1", "storage.googleapis.com"); !off {
		t.Fatal("snapshot does not decide it")
	}
	if _, err := c.Services.Enable("projects/p1/services/storage.googleapis.com", &su.EnableServiceRequest{}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, off := srv.Disabled(ctx, "p1", "storage.googleapis.com"); off {
		t.Fatal("enabled again, still refused")
	}
}

// Disabling an API enabled APIs depend on is refused without
// disableDependentServices, and takes them with it.
func TestDisableDependentServices(t *testing.T) {
	srv, c := newTestServer(t, store.NewMemory())
	srv.Dependents = map[string][]string{"storage.googleapis.com": {"run.googleapis.com"}}
	ctx := context.Background()
	_, err := c.Services.Disable("projects/p1/services/storage.googleapis.com", &su.DisableServiceRequest{}).Context(ctx).Do()
	if code(err) != 400 || !strings.Contains(err.Error(), "depended on by the following active service(s): run.googleapis.com") ||
		!strings.Contains(err.Error(), "disable_dependent_services=true") {
		t.Fatalf("without disableDependentServices: %v", err)
	}
	if _, off := srv.Disabled(ctx, "p1", "storage.googleapis.com"); off {
		t.Fatal("a refused disable changed the state")
	}
	if _, err := c.Services.Disable("projects/p1/services/storage.googleapis.com", &su.DisableServiceRequest{DisableDependentServices: true}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"storage.googleapis.com", "run.googleapis.com"} {
		if _, off := srv.Disabled(ctx, "p1", s); !off {
			t.Errorf("%s still enabled", s)
		}
	}
	// A dependent already disabled no longer holds its dependency back.
	if _, err := c.Services.Enable("projects/p1/services/storage.googleapis.com", &su.EnableServiceRequest{}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Services.Disable("projects/p1/services/storage.googleapis.com", &su.DisableServiceRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("with its dependent disabled: %v", err)
	}
}
