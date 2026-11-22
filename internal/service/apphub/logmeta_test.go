package apphub

import (
	"reflect"
	"testing"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
)

func TestLogMetadata(t *testing.T) {
	s := testServer(t, "", nil)
	uris := LogResourceURIs("cloud_run_revision", map[string]string{"project_id": "svc-project-1", "location": "us-central1",
		"service_name": "hello", "revision_name": "hello-00001"}, "123")
	if len(uris) != 1 || uris[0] != runURI {
		t.Fatalf("LogResourceURIs = %v; want [%s]", uris, runURI)
	}
	if _, ok := s.LogMetadata(runURI); ok {
		t.Fatal("an unregistered resource has App Hub metadata")
	}
	s.services["projects/host-1/locations/global/applications/shop/services/web"] = &apphubpb.Service{
		ServiceReference: &apphubpb.ServiceReference{Uri: runURI},
		Attributes: &apphubpb.Attributes{Environment: &apphubpb.Environment{Type: apphubpb.Environment_PRODUCTION},
			Criticality: &apphubpb.Criticality{Type: apphubpb.Criticality_HIGH}}}
	m, ok := s.LogMetadata(runURI)
	want := LogMetadata{"apphub.application.container": "projects/host-1", "apphub.application.location": "global",
		"apphub.application.id": "shop", "apphub.service.id": "web", "apphub.service.environmentType": "PRODUCTION",
		"apphub.service.criticalityType": "HIGH"}
	if !ok || !reflect.DeepEqual(m, want) {
		t.Fatalf("LogMetadata = %v, %v; want %v", m, ok, want)
	}
	nested := m.Nested()
	if app := nested["application"].(map[string]any); app["id"] != "shop" || nested["service"].(map[string]any)["id"] != "web" {
		t.Errorf("Nested = %v", nested)
	}
	// Registered to two applications, an entry has none (the REST
	// reference: "empty if ... multiple associated applications").
	s.workloads["projects/host-1/locations/global/applications/other/workloads/w"] = &apphubpb.Workload{
		WorkloadReference: &apphubpb.WorkloadReference{Uri: runURI}}
	if m, ok := s.LogMetadata(runURI); ok {
		t.Errorf("a resource in two applications: %v", m)
	}
	if got := LogResourceURIs("cloud_tasks_queue", map[string]string{"project_id": "p", "location": "us-central1", "queue_id": "q"}, "42"); len(got) != 1 ||
		got[0] != "//cloudtasks.googleapis.com/projects/42/locations/us-central1/queues/q" {
		t.Errorf("a queue's URI: %v", got)
	}
	if got := LogResourceURIs("global", map[string]string{"project_id": "p"}, ""); got != nil {
		t.Errorf("an unsupported type's URIs: %v", got)
	}
}
