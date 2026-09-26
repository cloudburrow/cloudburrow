//go:build compat

package compat

import (
	"cloud.google.com/go/kms/apiv1/kmspb"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// TestFaultInjectionAgainstTheSDKRetry (#306): a rule of two UNAVAILABLE
// faults on AccessSecretVersion, installed through /admin/faults on the CI
// instance, is absorbed by the official client's own retry, and
// /admin/events records both faults, and DELETE clears the rules. Storage
// rules are TestFaultsStorage's.
func TestFaultInjectionAgainstTheSDKRetry(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+control+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	t.Cleanup(func() { do(http.MethodDelete, "/admin/faults", "") })

	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: "faulted",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}) })
	if _, err := sc.AddSecretVersion(h.Context(), &secretmanagerpb.AddSecretVersionRequest{Parent: sec.Name,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("survives")}}); err != nil {
		t.Fatal(err)
	}

	if code, body := do(http.MethodPost, "/admin/faults",
		`{"service":"secretmanager","method":"AccessSecretVersion","project":"`+h.Project()+`","code":"UNAVAILABLE","count":2}`); code != http.StatusCreated {
		t.Fatalf("add rule = %d: %s", code, body)
	}
	resp, err := sc.AccessSecretVersion(h.Context(), &secretmanagerpb.AccessSecretVersionRequest{Name: sec.Name + "/versions/latest"})
	if err != nil || string(resp.GetPayload().GetData()) != "survives" {
		t.Fatalf("AccessSecretVersion under 2 UNAVAILABLE faults = %v, %v; want the SDK's retry to succeed", resp, err)
	}

	code, body := do(http.MethodGet, "/admin/events?service=secretmanager&kind=fault", "")
	var ev struct{ Events []struct{ Target string } }
	_ = json.Unmarshal([]byte(body), &ev)
	n := 0
	for _, e := range ev.Events {
		if strings.HasSuffix(e.Target, "/AccessSecretVersion") {
			n++
		}
	}
	if code != http.StatusOK || n != 2 {
		t.Errorf("/admin/events shows %d injected AccessSecretVersion faults (%d), want 2: %s", n, code, body)
	}

	do(http.MethodPost, "/admin/faults", `{"service":"secretmanager"}`)
	if code, _ := do(http.MethodDelete, "/admin/faults", ""); code != http.StatusOK {
		t.Errorf("DELETE /admin/faults = %d", code)
	}
	if _, body := do(http.MethodGet, "/admin/faults", ""); !strings.Contains(body, `"faults":[]`) {
		t.Errorf("rules after DELETE: %s", body)
	}
}

// TestKMSFaultInjectionAgainstTheSDKRetry (#392): one UNAVAILABLE fault on
// ListKeyRings, which the KMS client retries (defaultKeyManagementCallOptions,
// key_management_client.go:114-126 @v1.35.0, retries Unavailable and
// DeadlineExceeded), so the listing still succeeds; /admin/events records the
// injected fault for kms.
func TestKMSFaultInjectionAgainstTheSDKRetry(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+control+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	t.Cleanup(func() { do(http.MethodDelete, "/admin/faults", "") })
	c := kmsClients(t, h)["grpc"]
	parent := "projects/" + h.Project() + "/locations/global"
	if _, err := c.CreateKeyRing(h.Context(), &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: "faulted-ring"}); err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	if code, body := do(http.MethodPost, "/admin/faults", `{"service":"kms","method":"ListKeyRings","code":"UNAVAILABLE","count":1}`); code != http.StatusCreated {
		t.Fatalf("add rule = %d: %s", code, body)
	}
	if _, err := c.ListKeyRings(h.Context(), &kmspb.ListKeyRingsRequest{Parent: parent}).Next(); err != nil {
		t.Fatalf("ListKeyRings under one UNAVAILABLE fault = %v; want the SDK's retry to succeed", err)
	}
	code, body := do(http.MethodGet, "/admin/events?service=kms&kind=fault", "")
	if code != http.StatusOK || !strings.Contains(body, "/ListKeyRings") {
		t.Errorf("/admin/events for kms faults = %d %s; want the injected ListKeyRings fault", code, body)
	}
}

// TestFaultsStorage: `up` runs the storage server in the cluster (#514),
// where the instance's rules cannot reach it, so a storage rule is refused
// with that reason rather than accepted and never applied. (A storage server
// run in-process interposes faults; internal/service/storage tests that.)
func TestFaultsStorage(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	post := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+control+"/admin/faults", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, "http://"+control+"/admin/faults", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	code, body := post(`{"service":"storage","method":"storage.buckets.get","httpStatus":503,"count":1}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "runs in the cluster") {
		t.Errorf("a storage rule = %d %s, want 400 saying the server runs in the cluster", code, body)
	}
	if code, body := rawStorage(t, h, http.MethodGet, "/storage/v1/b/"+h.Project()+"-faulted", ""); code != http.StatusNotFound {
		t.Errorf("an unfaulted call = %d %s; want the ordinary 404", code, body)
	}
}
