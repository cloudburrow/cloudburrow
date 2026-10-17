package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TestPurgeRunsEveryProjectResetter (#1537): POST /admin/purge calls the
// purger, maps its errors to HTTP statuses, and PurgeProject resets the
// project in every component that can scope by project, the skipped one
// and the unscoped one apart.
func TestPurgeRunsEveryProjectResetter(t *testing.T) {
	var calls []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(projectResetter{fakeResetter{name: "pubsub", calls: &calls, ordered: &calls}},
		projectResetter{fakeResetter{name: "resourcemanager", calls: &calls, ordered: &calls}},
		fakeResetter{name: "unscoped", calls: &calls, ordered: &calls})
	srv := serve(a)
	defer srv.Close()

	if code, _ := post(t, srv.URL+"/admin/purge?project=p1", ""); code != 501 {
		t.Errorf("purge with no purger = %d, want 501", code)
	}
	a.SetPurger(func(ctx context.Context, project string) error {
		switch project {
		case "missing":
			return apierror.NotFound("project %q not found", project)
		case "active":
			return apierror.FailedPrecondition("project %q is not pending deletion", project)
		}
		if failed := a.PurgeProject(ctx, project, "resourcemanager"); len(failed) > 0 {
			return errors.New("failed")
		}
		return nil
	})
	if code, _ := post(t, srv.URL+"/admin/purge", ""); code != 400 {
		t.Errorf("purge without project= = %d, want 400", code)
	}
	if code, _ := post(t, srv.URL+"/admin/purge?project=missing", ""); code != 404 {
		t.Errorf("purge of a missing project = %d, want 404", code)
	}
	if code, _ := post(t, srv.URL+"/admin/purge?project=active", ""); code != 400 {
		t.Errorf("purge of an active project = %d, want 400", code)
	}
	if code, body := post(t, srv.URL+"/admin/purge?project=p1", ""); code != 200 {
		t.Fatalf("purge = %d %s", code, body)
	}
	if strings.Join(calls, ",") != "pubsub@p1" {
		t.Errorf("calls = %v, want pubsub@p1 alone", calls)
	}
}
