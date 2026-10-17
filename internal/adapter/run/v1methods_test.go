package run

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// v1Client is the official v1 client at a test server.
func v1Client(t *testing.T, srv *httptest.Server) *runv1.APIService {
	t.Helper()
	c, err := runv1.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestV1EveryUnservedMethodIsNotImplemented holds V1MethodStatuses' false
// entries to what V1Handler does (#1533): each, called through the official
// v1 client, answers 501. A served method's evidence is its compat test.
func TestV1EveryUnservedMethodIsNotImplemented(t *testing.T) {
	c := v1Client(t, v1Server(t))
	ns, loc := "namespaces/demo-project", "projects/demo-project/locations/us-central1"
	calls := map[string]func() error{
		"run.namespaces.instances.create": func() error { _, err := c.Namespaces.Instances.Create(ns, &runv1.Instance{}).Do(); return err },
		"run.namespaces.instances.delete": func() error { _, err := c.Namespaces.Instances.Delete(ns + "/instances/i").Do(); return err },
		"run.namespaces.instances.get":    func() error { _, err := c.Namespaces.Instances.Get(ns + "/instances/i").Do(); return err },
		"run.namespaces.instances.list":   func() error { _, err := c.Namespaces.Instances.List(ns).Do(); return err },
		"run.namespaces.instances.replaceInstance": func() error {
			_, err := c.Namespaces.Instances.ReplaceInstance(ns+"/instances/i", &runv1.Instance{}).Do()
			return err
		},
		"run.namespaces.instances.start": func() error {
			_, err := c.Namespaces.Instances.Start(ns+"/instances/i", &runv1.StartInstanceRequest{}).Do()
			return err
		},
		"run.namespaces.instances.stop": func() error {
			_, err := c.Namespaces.Instances.Stop(ns+"/instances/i", &runv1.StopInstanceRequest{}).Do()
			return err
		},
		"run.namespaces.workerpools.create": func() error { _, err := c.Namespaces.Workerpools.Create(ns, &runv1.WorkerPool{}).Do(); return err },
		"run.namespaces.workerpools.delete": func() error { _, err := c.Namespaces.Workerpools.Delete(ns + "/workerpools/p").Do(); return err },
		"run.namespaces.workerpools.get":    func() error { _, err := c.Namespaces.Workerpools.Get(ns + "/workerpools/p").Do(); return err },
		"run.namespaces.workerpools.list":   func() error { _, err := c.Namespaces.Workerpools.List(ns).Do(); return err },
		"run.namespaces.workerpools.replaceWorkerPool": func() error {
			_, err := c.Namespaces.Workerpools.ReplaceWorkerPool(ns+"/workerpools/p", &runv1.WorkerPool{}).Do()
			return err
		},
		"run.projects.locations.instances.getIamPolicy": func() error {
			_, err := c.Projects.Locations.Instances.GetIamPolicy(loc + "/instances/i").Do()
			return err
		},
		"run.projects.locations.instances.setIamPolicy": func() error {
			_, err := c.Projects.Locations.Instances.SetIamPolicy(loc+"/instances/i", &runv1.SetIamPolicyRequest{Policy: &runv1.Policy{}}).Do()
			return err
		},
		"run.projects.locations.instances.testIamPermissions": func() error {
			_, err := c.Projects.Locations.Instances.TestIamPermissions(loc+"/instances/i", &runv1.TestIamPermissionsRequest{Permissions: []string{"run.instances.get"}}).Do()
			return err
		},
	}
	for m, served := range V1MethodStatuses() {
		call, ok := calls[m]
		switch {
		case served && ok:
			t.Errorf("%s is served, but this test calls it as unserved", m)
		case served:
		case !ok:
			t.Errorf("%s is unserved, and this test does not call it", m)
		default:
			var ge *googleapi.Error
			if err := call(); !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
				t.Errorf("%s = %v; want 501", m, err)
			}
		}
	}
}

// v1's projects.locations.operations, authorized domains under a location,
// and a worker pool's IAM methods (#1533), through the official client.
func TestV1OperationsAndLocationScopedReads(t *testing.T) {
	s := NewServer(&Knative{Kube: k8s.NewWith(&scriptedRunner{answers: map[string]string{}}, "k", "", "default")}, "inst", time.Second)
	srv := httptest.NewServer(s.V1Handler(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	c := v1Client(t, srv)
	loc := "projects/demo-project/locations/us-central1"
	op := s.ops.Create(loc, loc+"/services/hello")
	if err := s.ops.Succeed(op.Name, &runpb.Service{Name: loc + "/services/hello"}); err != nil {
		t.Fatal(err)
	}
	l, err := c.Projects.Locations.Operations.List(loc).Do()
	if err != nil || len(l.Operations) != 1 || l.Operations[0].Name != op.Name || !l.Operations[0].Done {
		t.Fatalf("operations.list = %+v, %v", l, err)
	}
	if got, err := c.Projects.Locations.Operations.Get(op.Name).Do(); err != nil || got.Name != op.Name || got.Response == nil {
		t.Errorf("operations.get = %+v, %v", got, err)
	}
	if got, err := c.Projects.Locations.Operations.Wait(op.Name, &runv1.GoogleLongrunningWaitOperationRequest{Timeout: "1s"}).Do(); err != nil || !got.Done {
		t.Errorf("operations.wait = %+v, %v", got, err)
	}
	if _, err := c.Projects.Locations.Operations.Wait(op.Name, &runv1.GoogleLongrunningWaitOperationRequest{Timeout: "soon"}).Do(); !isCode(err, 400) {
		t.Errorf("operations.wait with a bad timeout = %v; want 400", err)
	}
	if _, err := c.Projects.Locations.Operations.Delete(op.Name).Do(); err != nil {
		t.Errorf("operations.delete = %v", err)
	}
	if _, err := c.Projects.Locations.Operations.Get(loc + "/operations/nope").Do(); !isCode(err, 404) {
		t.Errorf("operations.get of a missing operation = %v; want 404", err)
	}
	if _, err := c.Projects.Locations.Operations.Delete(loc + "/operations/nope").Do(); !isCode(err, 404) {
		t.Errorf("operations.delete of a missing operation = %v; want 404", err)
	}
	if d, err := c.Projects.Locations.Authorizeddomains.List(loc).Do(); err != nil || len(d.Domains) != 0 {
		t.Errorf("projects.locations.authorizeddomains.list = %+v, %v", d, err)
	}
	if d, err := c.Namespaces.Authorizeddomains.List("namespaces/demo-project").Do(); err != nil || len(d.Domains) != 0 {
		t.Errorf("namespaces.authorizeddomains.list = %+v, %v", d, err)
	}
	if _, err := c.Projects.Locations.Workerpools.GetIamPolicy(loc + "/workerpools/nope").Do(); isCode(err, 501) || isCode(err, 400) {
		t.Errorf("workerpools.getIamPolicy of a missing pool = %v; want it read as the v2 pool's, not refused", err)
	}
}

func isCode(err error, code int) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == code
}

// On the Run port, Cloud Build's handler is first and binds
// projects.locations.operations get and cancel; v1's list, wait and delete
// at the same paths pass it by (#1533).
func TestRunV1OperationsCallsPassCloudBuildBy(t *testing.T) {
	ops := "/v1/projects/p/locations/us-central1/operations"
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, ops, true},
		{http.MethodPost, ops + "/op-1:wait", true},
		{http.MethodDelete, ops + "/op-1", true},
		{http.MethodGet, ops + "/op-1", false},
		{http.MethodPost, ops + "/op-1:cancel", false},
		{http.MethodGet, "/v1/projects/p/locations/us-central1/builds", false},
		{http.MethodGet, "/v1/operations/x", false},
	} {
		if got := isRunV1OperationsCall(httptest.NewRequest(tc.method, tc.path, nil)); got != tc.want {
			t.Errorf("%s %s: %v; want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
