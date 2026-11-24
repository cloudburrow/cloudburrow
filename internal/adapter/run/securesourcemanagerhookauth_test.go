package run

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc"
)

// Hook.serviceAccountAuth over the JSON API (#1578): accepted on create and
// in an update's mask, as google_secure_source_manager_hook sends it,
// returned by a read, and refused on a repository with no service account.
func TestSourceManagerHookServiceAccountAuthJSON(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := NewServer(&Knative{}, "t", time.Minute)
	var srv *httptest.Server
	m := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(SourceManagerHandlerOr(m, nil, func(g grpc.ServiceRegistrar) { m.Register(g) }, nil))
	defer srv.Close()
	parent := "projects/ssm-hookauth/locations/us-central1"
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i"}); err != nil {
		t.Fatal(err)
	}
	for id, sa := range map[string]string{"plain": "", "withsa": "ssm@ssm-hookauth.iam.gserviceaccount.com"} {
		op, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
			Repository: &ssmpb.Repository{Instance: parent + "/instances/i", ServiceAccount: sa}})
		opResult(t, op, err, &ssmpb.Repository{})
		// ssmState is the package's: deleting the repository removes its
		// hooks, so the next test starts with none.
		name := parent + "/repositories/" + id
		t.Cleanup(func() { _, _ = m.DeleteRepository(context.Background(), &ssmpb.DeleteRepositoryRequest{Name: name}) })
	}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+"/securesourcemanager/v1/"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		return resp.StatusCode, v
	}
	const hook = `{"targetUri":"https://hooks.example.com/x","events":["PUSH"],"serviceAccountAuth":true}`
	if code, v := call("POST", parent+"/repositories/plain/hooks?hookId=h", hook); code != http.StatusBadRequest {
		t.Errorf("a hook with serviceAccountAuth on a repository with no service account: %d %v; want 400", code, v)
	}
	if code, v := call("POST", parent+"/repositories/withsa/hooks?hookId=h", hook); code != http.StatusOK {
		t.Fatalf("create: %d %v", code, v)
	}
	name := parent + "/repositories/withsa/hooks/h"
	get := func() (bool, map[string]any) {
		t.Helper()
		code, v := call("GET", name, "")
		if code != http.StatusOK || v["targetUri"] != "https://hooks.example.com/x" {
			t.Fatalf("get: %d %v", code, v)
		}
		on, _ := v["serviceAccountAuth"].(bool)
		return on, v
	}
	if on, v := get(); !on || !SourceManagerHookServiceAccountAuth(name) {
		t.Errorf("after create the hook is %v; want serviceAccountAuth", v)
	}
	// Terraform's update: every field in the body, the changed ones in the mask.
	if code, v := call("PATCH", name+"?updateMask=events,serviceAccountAuth",
		`{"targetUri":"https://hooks.example.com/x","disabled":false,"events":["PULL_REQUEST"],"serviceAccountAuth":false,"pushOption":{}}`); code != http.StatusOK {
		t.Fatalf("update: %d %v", code, v)
	}
	if on, v := get(); on || SourceManagerHookServiceAccountAuth(name) || len(v["events"].([]any)) != 1 || v["events"].([]any)[0] != "PULL_REQUEST" {
		t.Errorf("after the update the hook is %v; want PULL_REQUEST without serviceAccountAuth", v)
	}
	if code, v := call("PATCH", name+"?updateMask=disabled", `{"disabled":true,"serviceAccountAuth":true}`); code != http.StatusOK {
		t.Fatalf("update of disabled: %d %v", code, v)
	}
	if on, _ := get(); on {
		t.Error("a field outside the mask changed serviceAccountAuth")
	}
}
