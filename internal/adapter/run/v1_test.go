package run

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const v1SvcJSON = `{"metadata":{"name":"hello","uid":"u1","resourceVersion":"7","generation":2,
  "creationTimestamp":"2026-09-24T10:00:00Z",
  "labels":{"cloudburrow.dev/owned":"true"},
  "annotations":{"cloudburrow.dev/cloud-run-name":"projects/demo-project/locations/us-central1/services/hello",
    "cloudburrow.dev/cloud-run-labels":"{\"team\":\"a\"}"}},
 "spec":{"template":{"metadata":{"annotations":{"autoscaling.knative.dev/max-scale":"3"}},
   "spec":{"containerConcurrency":80,"timeoutSeconds":30,"containers":[{"image":"ghcr.io/x/y@sha256:abc",
     "env":[{"name":"A","value":"b"}],"ports":[{"containerPort":8080}]}]}}},
 "status":{"observedGeneration":2,"url":"http://hello.default.example.com",
   "latestReadyRevisionName":"hello-00002","latestCreatedRevisionName":"hello-00002",
   "conditions":[{"type":"Ready","status":"True"},{"type":"RoutesReady","status":"True"},{"type":"ConfigurationsReady","status":"True"}],
   "traffic":[{"revisionName":"hello-00002","percent":100,"latestRevision":true}]}}`

func v1Server(t *testing.T) *httptest.Server {
	t.Helper()
	r := &scriptedRunner{answers: map[string]string{
		"get ksvc hello":           v1SvcJSON,
		"get ksvc -l":              `{"items":[` + v1SvcJSON + `]}`,
		"get revisions -l":         `{"items":[` + revJSON("hello-00001", "1", "True") + `,` + revJSON("hello-00002", "2", "True") + `]}`,
		"get revision hello-00002": revJSON("hello-00002", "2", "True"),
	}}
	s := NewServer(&Knative{Kube: k8s.NewWith(r, "k", "", "default")}, "inst", time.Second)
	srv := httptest.NewServer(s.V1Handler(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	return srv
}

func TestV1ServesKnativeShapedReads(t *testing.T) {
	srv := v1Server(t)
	for _, path := range []string{
		"/apis/serving.knative.dev/v1/namespaces/demo-project/services",
		"/v1/projects/demo-project/locations/us-central1/services",
		"/v1/projects/demo-project/locations/-/services",
	} {
		code, body := call(t, srv, http.MethodGet, path, "")
		items, _ := body["items"].([]any)
		if code != 200 || len(items) != 1 || body["kind"] != "ServiceList" {
			t.Fatalf("GET %s = %d %v", path, code, body)
		}
		svc := items[0].(map[string]any)
		md := svc["metadata"].(map[string]any)
		labels := md["labels"].(map[string]any)
		if md["name"] != "hello" || md["namespace"] != "demo-project" || labels[v1LocationLabel] != "us-central1" || labels["team"] != "a" {
			t.Errorf("GET %s metadata = %v", path, md)
		}
		st := svc["status"].(map[string]any)
		if st["url"] != "http://hello.default.example.com" || st["latestReadyRevisionName"] != "hello-00002" {
			t.Errorf("GET %s status = %v", path, st)
		}
	}
	// Another region or project lists nothing.
	if code, body := call(t, srv, http.MethodGet, "/v1/projects/demo-project/locations/europe-west1/services", ""); code != 200 || len(itemsOf(body)) != 0 {
		t.Errorf("another region = %d %v", code, body)
	}
	if code, _ := call(t, srv, http.MethodGet, "/apis/serving.knative.dev/v1/namespaces/other/services/hello", ""); code != 404 {
		t.Errorf("another project's get = %d; want 404", code)
	}
	code, body := call(t, srv, http.MethodGet, "/apis/serving.knative.dev/v1/namespaces/demo-project/services/hello", "")
	tmpl := body["spec"].(map[string]any)["template"].(map[string]any)
	if code != 200 || tmpl["metadata"].(map[string]any)["annotations"].(map[string]any)[v1MaxScale] != "3" {
		t.Errorf("get = %d %v", code, body)
	}
	code, body = call(t, srv, http.MethodGet,
		"/apis/serving.knative.dev/v1/namespaces/demo-project/revisions?labelSelector=serving.knative.dev/service+%3D+hello", "")
	if items, _ := body["items"].([]any); code != 200 || len(items) != 2 {
		t.Errorf("revisions list = %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/apis/serving.knative.dev/v1/namespaces/demo-project/revisions?labelSelector=serving.knative.dev/service%3Dother", ""); code != 200 || len(itemsOf(body)) != 0 {
		t.Errorf("revisions of another service = %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/apis/serving.knative.dev/v1/namespaces/demo-project/revisions/hello-00002", ""); code != 200 || body["kind"] != "Revision" {
		t.Errorf("revision get = %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/apis/serving.knative.dev/v1/namespaces/demo-project/routes/hello", ""); code != 200 || body["kind"] != "Route" {
		t.Errorf("route get = %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/v1/projects/demo-project/locations/us-central1/configurations", ""); code != 200 || body["kind"] != "ConfigurationList" {
		t.Errorf("configurations list = %d %v", code, body)
	}
	for _, path := range []string{
		"/apis/domains.cloudrun.com/v1/namespaces/demo-project/domainmappings",
		"/v1/projects/demo-project/locations",
		"/v1/projects/demo-project/locations/us-central1/services/hello:getIamPolicy",
	} {
		if code, _ := call(t, srv, http.MethodGet, path, ""); code != 501 {
			t.Errorf("GET %s = %d; want 501", path, code)
		}
	}
}

// A regional host, as gcloud derives it for --region, scopes the
// namespaced path to its region.
func TestV1RegionalHostScopesTheNamespace(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://europe-west1-run.localhost:1/apis/serving.knative.dev/v1/namespaces/p/services", nil)
	r.SetPathValue("ns", "p")
	if sc := scopeOf(r); sc.location != "europe-west1" || sc.project != "p" {
		t.Errorf("scope = %+v", sc)
	}
}

// What a read returns is what a replace accepts: the round trip changes
// nothing the v2 mapping sees.
func TestV1RoundTripAndRefusals(t *testing.T) {
	k, _ := (&Knative{Kube: k8s.NewWith(&scriptedRunner{answers: map[string]string{"get ksvc hello": v1SvcJSON}}, "k", "", "default")}).Get(t.Context(), "hello")
	v2 := FromKnative(k, "projects/demo-project/locations/us-central1")
	back, err := fromV1Service(toV1Service(v2, k), v2.GetName())
	if err != nil {
		t.Fatalf("fromV1Service(toV1Service) = %v", err)
	}
	if back.GetTemplate().GetScaling().GetMaxInstanceCount() != 3 || back.GetLabels()["team"] != "a" ||
		back.GetTemplate().GetContainers()[0].GetImage() != "ghcr.io/x/y@sha256:abc" || back.GetTemplate().GetTimeout().GetSeconds() != 30 {
		t.Errorf("round trip = %v", back)
	}
	in := toV1Service(v2, k)
	in.Spec.Template.Spec.Volumes = []*runv1.Volume{{Name: "v"}}
	in.Spec.Template.Spec.Containers[0].StartupProbe = &runv1.Probe{}
	_, err = fromV1Service(in, v2.GetName())
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "volumes") || !strings.Contains(err.Error(), "startupProbe") {
		t.Errorf("unmapped fields = %v; want Unimplemented naming them", err)
	}
}

func itemsOf(body map[string]any) []any {
	items, _ := body["items"].([]any)
	return items
}
