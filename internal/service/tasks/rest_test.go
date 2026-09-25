package tasks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The JSON calls Terraform makes for google_cloud_tasks_queue and its
// iam_member (#366), in the order it makes them, over the same store.
func TestRESTQueueAndIamPolicy(t *testing.T) {
	r := rest.NewRouter()
	NewRESTServer(NewStore(store.NewMemory())).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const loc = "/v2/projects/demo-proj/locations/us-central1"
	const q = loc + "/queues/tf"
	for _, c := range []struct {
		method, path, body string
		code               int
		has                string
	}{
		{"POST", loc + "/queues", `{"name":"projects/demo-proj/locations/us-central1/queues/tf"}`, 200, `"state":"RUNNING"`},
		{"GET", q, "", 200, `"name":"projects/demo-proj/locations/us-central1/queues/tf"`},
		{"POST", q + ":getIamPolicy", "", 200, `"etag":"ACAB"`},
		{"POST", q + ":setIamPolicy", `{"policy":{"version":3,"etag":"ACAB","bindings":[{"role":"roles/cloudtasks.enqueuer","members":["user:a@example.com"]}]}}`, 200, "user:a@example.com"},
		{"GET", q + ":getIamPolicy?options.requestedPolicyVersion=3", "", 200, "roles/cloudtasks.enqueuer"},
		{"POST", q + ":setIamPolicy", `{"policy":{"etag":"ACAB"}}`, 409, "etag"},
		{"POST", q + ":testIamPermissions", `{"permissions":["cloudtasks.tasks.create"]}`, 200, "cloudtasks.tasks.create"},
		{"POST", loc + "/queues", `{"name":"projects/demo-proj/locations/us-central1/queues/x","noSuchField":1}`, 400, "noSuchField"},
		{"PATCH", q + "?updateMask=rateLimits", `{"rateLimits":{"maxDispatchesPerSecond":1}}`, 501, ""},
		{"GET", q + "/tasks", "", 501, "gRPC"},
		{"GET", q + ":somethingElse", "", 501, ""},
		{"DELETE", q, "", 200, ""},
		{"GET", q, "", 404, ""},
	} {
		code, body := do(c.method, c.path, c.body)
		if code != c.code || !strings.Contains(body, c.has) {
			t.Errorf("%s %s: %d %s; want %d containing %q", c.method, c.path, code, body, c.code, c.has)
		}
	}
}
