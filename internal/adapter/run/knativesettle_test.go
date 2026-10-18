package run

import (
	"encoding/json"
	"strings"
	"testing"
)

// A Service whose Route ran before its Configuration was in the Route's
// cache: Knative reports Ready False for a moment, Cloud Run Unknown (#1631).
func TestConfigurationMissingIsStillReconciling(t *testing.T) {
	racing := strings.NewReplacer("\n", "").Replace(`{"metadata":{"name":"tf-v1-svc-e7cd6935","generation":1,
"annotations":{"cloudburrow.dev/cloud-run-name":"projects/p/locations/us-central1/services/tf-v1-svc"}},
"status":{"observedGeneration":1,"conditions":[
{"type":"ConfigurationsReady","status":"Unknown"},
{"type":"Ready","status":"False","reason":"ConfigurationMissing","message":"Configuration \"tf-v1-svc-e7cd6935\" referenced in traffic not found."},
{"type":"RoutesReady","status":"False","reason":"ConfigurationMissing","message":"Configuration \"tf-v1-svc-e7cd6935\" referenced in traffic not found."}]}}`)
	var k ksvc
	if err := json.Unmarshal([]byte(racing), &k); err != nil {
		t.Fatal(err)
	}
	if ok, msg := k.Ready(); ok || msg != "" {
		t.Errorf("Ready() = %v, %q; want not ready and no failure", ok, msg)
	}
	svc := FromKnative(k, "projects/p/locations/us-central1")
	for _, c := range toV1Service(svc, k).Status.Conditions {
		if c.Status != "Unknown" {
			t.Errorf("v1 condition %s = %s (%s); want Unknown", c.Type, c.Status, c.Reason)
		}
	}
	if c := toV1Route(svc, k).Status.Conditions[0]; c.Status != "Unknown" {
		t.Errorf("v1 Route Ready = %s; want Unknown", c.Status)
	}
	// A list decodes the same way.
	var list struct {
		Items []ksvc `json:"items"`
	}
	if err := json.Unmarshal([]byte(`{"items":[`+racing+`]}`), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v %d", err, len(list.Items))
	}
	if ok, msg := list.Items[0].Ready(); ok || msg != "" {
		t.Errorf("listed Ready() = %v, %q; want not ready and no failure", ok, msg)
	}

	// A failed revision is final, and stays False with its reason.
	failed := `{"metadata":{"name":"s"},"status":{"conditions":[
{"type":"ConfigurationsReady","status":"False","reason":"RevisionFailed","message":"Revision \"s-00001\" failed with message: boom."},
{"type":"Ready","status":"False","reason":"RevisionMissing","message":"Configuration \"s\" does not have any ready Revision."},
{"type":"RoutesReady","status":"False","reason":"RevisionMissing","message":"Configuration \"s\" does not have any ready Revision."}]}}`
	k = ksvc{}
	if err := json.Unmarshal([]byte(failed), &k); err != nil {
		t.Fatal(err)
	}
	if ok, msg := k.Ready(); ok || !strings.Contains(msg, "boom") {
		t.Errorf("failed Ready() = %v, %q; want the revision's failure", ok, msg)
	}
	for _, c := range toV1Service(FromKnative(k, "projects/p/locations/us-central1"), k).Status.Conditions {
		if c.Status != "False" {
			t.Errorf("failed v1 condition %s = %s; want False", c.Type, c.Status)
		}
	}
}
