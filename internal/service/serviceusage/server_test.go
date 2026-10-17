package serviceusage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type anyProject struct{}

func (anyProject) Resolve(s string) (string, error) {
	if _, err := strconv.Atoi(s); err == nil {
		return "", PermissionDenied("Project %s not found", s)
	}
	return s, nil
}

func newTestServer(t *testing.T, st store.Store) (*Server, *su.Service) {
	t.Helper()
	srv := NewServer("", []API{{Name: "run.googleapis.com", Title: "Cloud Run Admin API"},
		{Name: "storage.googleapis.com", Title: "Cloud Storage API"}},
		[]Unserved{{Name: "spanner.googleapis.com", Reason: "start the instance with --services spanner"}}, st, anyProject{})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	c, err := su.NewService(context.Background(), option.WithEndpoint(hs.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func TestServicesThroughTheOfficialClient(t *testing.T) {
	st := store.NewMemory()
	_, c := newTestServer(t, st)
	ctx := context.Background()
	const p = "projects/su-test"
	num := "projects/" + strconv.FormatInt(resource.ProjectNumber("su-test"), 10)

	list, err := c.Services.List(p).Filter("state:ENABLED").Context(ctx).Do()
	if err != nil || len(list.Services) != 2 {
		t.Fatalf("list = %+v, %v; want both served APIs", list, err)
	}
	if s := list.Services[0]; s.Name != num+"/services/run.googleapis.com" || s.State != "ENABLED" || s.Config.Title != "Cloud Run Admin API" || s.Parent != num {
		t.Errorf("first service = %+v", s)
	}

	op, err := c.Services.Disable(p+"/services/run.googleapis.com", &su.DisableServiceRequest{CheckIfServiceHasUsage: "SKIP"}).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("disable = %+v, %v", op, err)
	}
	got, err := c.Operations.Get(op.Name).Context(ctx).Do()
	if err != nil || !got.Done || !strings.Contains(string(got.Response), "DisableServiceResponse") {
		t.Fatalf("operations.get = %+v, %v", got, err)
	}
	if s, err := c.Services.Get(p + "/services/run.googleapis.com").Context(ctx).Do(); err != nil || s.State != "DISABLED" {
		t.Errorf("after disable, get = %+v, %v", s, err)
	}
	if l, _ := c.Services.List(p).Filter("state:ENABLED").Context(ctx).Do(); len(l.Services) != 1 {
		t.Errorf("after disable, %d enabled; want 1", len(l.Services))
	}
	// Another project is unchanged.
	if s, _ := c.Services.Get("projects/other-project/services/run.googleapis.com").Context(ctx).Do(); s == nil || s.State != "ENABLED" {
		t.Errorf("another project's run = %+v", s)
	}

	if op, err := c.Services.BatchEnable(p, &su.BatchEnableServicesRequest{ServiceIds: []string{"run.googleapis.com", "storage.googleapis.com"}}).Context(ctx).Do(); err != nil || !op.Done {
		t.Fatalf("batchEnable = %+v, %v", op, err)
	}
	bg, err := c.Services.BatchGet(p).Names(p+"/services/run.googleapis.com", p+"/services/storage.googleapis.com").Context(ctx).Do()
	if err != nil || len(bg.Services) != 2 || bg.Services[0].State != "ENABLED" || bg.Services[1].State != "ENABLED" {
		t.Fatalf("batchGet = %+v, %v", bg, err)
	}

	// A batch with one API nothing serves changes nothing.
	if _, err := c.Services.Disable(p+"/services/storage.googleapis.com", &su.DisableServiceRequest{}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	_, err = c.Services.BatchEnable(p, &su.BatchEnableServicesRequest{ServiceIds: []string{"storage.googleapis.com", "spanner.googleapis.com"}}).Context(ctx).Do()
	if code(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "--services spanner") {
		t.Errorf("batchEnable with spanner = %v; want 400 naming --services spanner", err)
	}
	if s, _ := c.Services.Get(p + "/services/storage.googleapis.com").Context(ctx).Do(); s.State != "DISABLED" {
		t.Errorf("a refused batch enabled storage: %+v", s)
	}
	if _, err := c.Services.Enable(p+"/services/compute.googleapis.com", &su.EnableServiceRequest{}).Context(ctx).Do(); code(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "not served by CloudBurrow") {
		t.Errorf("enable compute = %v", err)
	}
	if s, err := c.Services.Get(p + "/services/spanner.googleapis.com").Context(ctx).Do(); err != nil || s.State != "DISABLED" {
		t.Errorf("get spanner = %+v, %v; want DISABLED", s, err)
	}
	if _, err := c.Services.Get("projects/123/services/run.googleapis.com").Context(ctx).Do(); code(err) != http.StatusForbidden {
		t.Errorf("an unknown project number = %v; want 403", err)
	}

	// Paging, and the state survives a new server over the same store.
	_, c2 := newTestServer(t, st)
	first, err := c2.Services.List(p).PageSize(1).Context(ctx).Do()
	if err != nil || len(first.Services) != 1 || first.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", first, err)
	}
	second, err := c2.Services.List(p).PageSize(1).PageToken(first.NextPageToken).Context(ctx).Do()
	if err != nil || len(second.Services) != 1 || second.NextPageToken != "" || second.Services[0].State != "DISABLED" {
		t.Fatalf("page 2 = %+v, %v; want storage, still DISABLED", second, err)
	}
	if _, err := c2.Services.List(p).Filter("name:run").Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("an unsupported filter = %v; want 400", err)
	}
}

// Every method in Methods is the one its path routes to, and answers 501
// exactly when it is not implemented.
func TestMethodsTableMatchesTheRouter(t *testing.T) {
	srv := NewServer("", nil, nil, store.NewMemory(), anyProject{})
	for _, m := range Methods {
		var got string
		srv.Observe = func(method string, _ int) { got = method }
		req := httptest.NewRequest(m.HTTPMethod, "/"+m.Path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if got != m.ID {
			t.Errorf("%s %s routes to %q, want %s", m.HTTPMethod, m.Path, got, m.ID)
		}
		if unimpl := rec.Code == http.StatusNotImplemented; unimpl == m.Implemented {
			t.Errorf("%s answered %d; implemented = %v", m.ID, rec.Code, m.Implemented)
		}
	}
}

// operations.list lists every operation oldest first and pages; cancel of
// a done one answers Empty and leaves it done; delete forgets it; a filter
// is refused saying why (#1530).
func TestOperationsListCancelDelete(t *testing.T) {
	_, c := newTestServer(t, store.NewMemory())
	ctx := context.Background()
	const p = "projects/su-ops"
	var names []string
	for _, svc := range []string{"run.googleapis.com", "storage.googleapis.com", "run.googleapis.com"} {
		op, err := c.Services.Disable(p+"/services/"+svc, &su.DisableServiceRequest{}).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, op.Name)
	}
	page, err := c.Operations.List().PageSize(2).Context(ctx).Do()
	if err != nil || len(page.Operations) != 2 || page.Operations[0].Name != names[0] || page.NextPageToken == "" {
		t.Fatalf("list page 1 = %+v, %v", page, err)
	}
	rest, err := c.Operations.List().PageSize(2).PageToken(page.NextPageToken).Context(ctx).Do()
	if err != nil || len(rest.Operations) != 1 || rest.Operations[0].Name != names[2] || rest.NextPageToken != "" {
		t.Fatalf("list page 2 = %+v, %v", rest, err)
	}
	if _, err := c.Operations.Cancel(names[0], &su.CancelOperationRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if op, err := c.Operations.Get(names[0]).Context(ctx).Do(); err != nil || !op.Done || op.Error != nil {
		t.Errorf("after cancel get = %+v, %v; want it still done", op, err)
	}
	if _, err := c.Operations.Delete(names[1]).Context(ctx).Do(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Operations.Get(names[1]).Context(ctx).Do(); code(err) != http.StatusNotFound {
		t.Errorf("get of a deleted operation = %v; want 404", err)
	}
	if _, err := c.Operations.Delete(names[1]).Context(ctx).Do(); code(err) != http.StatusNotFound {
		t.Errorf("second delete = %v; want 404", err)
	}
	if _, err := c.Operations.Cancel("operations/nope", &su.CancelOperationRequest{}).Context(ctx).Do(); code(err) != http.StatusNotFound {
		t.Errorf("cancel of an unknown operation = %v; want 404", err)
	}
	all, err := c.Operations.List().Context(ctx).Do()
	if err != nil || len(all.Operations) != 2 {
		t.Errorf("list after delete = %+v, %v", all, err)
	}
	if _, err := c.Operations.List().Filter("done=true").Context(ctx).Do(); code(err) != http.StatusNotImplemented || !strings.Contains(err.Error(), "filter") {
		t.Errorf("list with a filter = %v; want 501 naming the filter", err)
	}
}
