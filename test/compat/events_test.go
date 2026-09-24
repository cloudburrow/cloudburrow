//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// EnvControl points at the instance's loopback-only control port, which serves
// the admin API.
const EnvControl = "CLOUDBURROW_TEST_CONTROL"

type adminEvent struct {
	Time    time.Time         `json:"time"`
	Service string            `json:"service"`
	Kind    string            `json:"kind"`
	Target  string            `json:"target"`
	Detail  map[string]string `json:"detail"`
}

// events reads /admin/events for a service, keeping only this test's project.
func events(t *testing.T, h *Harness, control, service string, since time.Time) []adminEvent {
	t.Helper()
	url := fmt.Sprintf("http://%s/admin/events?service=%s&kind=request&limit=1000&since=%s",
		control, service, since.UTC().Format(time.RFC3339Nano))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body struct{ Events []adminEvent }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var mine []adminEvent
	for _, e := range body.Events {
		if e.Detail["project"] == h.Project() {
			mine = append(mine, e)
		}
	}
	return mine
}

func methodsOf(es []adminEvent) map[string]adminEvent {
	out := map[string]adminEvent{}
	for _, e := range es {
		// Target is the full gRPC method; the short name is what a reader looks for.
		out[e.Target[strings.LastIndex(e.Target, "/")+1:]] = e
	}
	return out
}

// TestAdminEventsRecordOfficialSDKCalls.
//
// /admin/events was served and always empty, because nothing recorded into it
// (#273). Calls made with the official Cloud Tasks and Secret Manager clients
// must now appear there, with the method, the code the caller received and a
// duration, and a missing resource must be recorded as NOT_FOUND.
func TestAdminEventsRecordOfficialSDKCalls(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	since := time.Now().Add(-time.Second)

	tc := tasksClient(t, h)
	queue(t, h, tc, "events-q")

	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "events-secret",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}) })
	const marker = "COMPAT-PAYLOAD-MARKER"
	if _, err := sc.AddSecretVersion(h.Context(), &secretmanagerpb.AddSecretVersionRequest{
		Parent: sec.Name, Payload: &secretmanagerpb.SecretPayload{Data: []byte(marker)}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	if _, err := sc.AccessSecretVersion(h.Context(), &secretmanagerpb.AccessSecretVersionRequest{
		Name: sec.Name + "/versions/latest"}); err != nil {
		t.Fatalf("AccessSecretVersion: %v", err)
	}
	if _, err := sc.GetSecret(h.Context(), &secretmanagerpb.GetSecretRequest{
		Name: secretsParent(h) + "/secrets/never-created"}); err == nil {
		t.Fatal("GetSecret on a missing secret succeeded")
	}

	// Recording happens as each call completes, so the events are there by now;
	// a short bounded retry absorbs nothing but scheduling.
	var tasksEv, secretEv map[string]adminEvent
	for i := 0; i < 20; i++ {
		tasksEv = methodsOf(events(t, h, control, "tasks", since))
		secretEv = methodsOf(events(t, h, control, "secretmanager", since))
		if _, ok := tasksEv["CreateQueue"]; ok {
			if _, ok := secretEv["GetSecret"]; ok {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	check := func(es map[string]adminEvent, method, code string) {
		t.Helper()
		e, ok := es[method]
		if !ok {
			t.Errorf("%s was not recorded", method)
			return
		}
		if e.Detail["code"] != code {
			t.Errorf("%s recorded code %q, want %q", method, e.Detail["code"], code)
		}
		if e.Detail["duration_ms"] == "" {
			t.Errorf("%s recorded no duration", method)
		}
	}
	check(tasksEv, "CreateQueue", "OK")
	check(secretEv, "CreateSecret", "OK")
	check(secretEv, "AccessSecretVersion", "OK")
	check(secretEv, "GetSecret", "NOT_FOUND")

	raw, _ := json.Marshal(secretEv)
	if strings.Contains(string(raw), marker) {
		t.Fatalf("a secret payload reached the event log: %s", raw)
	}
}
