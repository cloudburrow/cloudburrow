package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

const computeTestToken = "compute-test-token"

func computeServer(t *testing.T) (*httptest.Server, *byoctest.Cloud, *keychain.Memory) {
	t.Helper()
	aws := byoctest.New(byoc.AWS)
	keys := keychain.NewMemory()
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: keys, Drivers: []byoc.Driver{aws, byoctest.New(byoc.Azure)},
		IPLookup:    func(context.Context) (string, error) { return "203.0.113.1", nil },
		OpenBrowser: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	s := New("", func(context.Context) Status { return Status{} })
	s.SetCloudCompute(m, "dev", func(_ context.Context, authz string) error {
		if authz != "Bearer "+computeTestToken {
			return &InstanceRefusal{Status: http.StatusUnauthorized, Message: "missing or wrong admin token"}
		}
		return nil
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, aws, keys
}

func computeCall(t *testing.T, srv *httptest.Server, method, path, body string, token bool, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token {
		req.Header.Set("Authorization", "Bearer "+computeTestToken)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestComputeNeedsTheAdminToken(t *testing.T) {
	srv, _, _ := computeServer(t)
	var out map[string]any
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/compute", ""},
		{"POST", "/api/compute/accesskey", `{"access_key_id":"A","secret_access_key":"B"}`},
		{"POST", "/api/compute/machines", `{"instance":"dev"}`},
		{"DELETE", "/api/compute/machines/dev", ""},
	} {
		if code := computeCall(t, srv, c.method, c.path, c.body, false, &out); code != http.StatusUnauthorized || out["token_required"] != true {
			t.Errorf("%s %s without the token = %d %v", c.method, c.path, code, out)
		}
	}
}

func TestComputeUnattached(t *testing.T) {
	s := New("", func(context.Context) Status { return Status{} })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if code := computeCall(t, srv, "GET", "/api/compute", "", true, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("an unattached console answered %d", code)
	}
}

func TestComputeConnectCreateAndDelete(t *testing.T) {
	srv, aws, keys := computeServer(t)
	var st byoc.SignInStatus
	if code := computeCall(t, srv, "POST", "/api/compute/signin", `{"cloud":"aws","start_url":"http://not-https","region":"us-east-1"}`, true, nil); code != http.StatusBadRequest {
		t.Fatalf("a non-https start URL = %d", code)
	}
	if code := computeCall(t, srv, "POST", "/api/compute/signin", `{"cloud":"aws","start_url":"https://d-1.awsapps.com/start","region":"us-east-1"}`, true, &st); code != 200 || st.Prompt.UserCode == "" {
		t.Fatalf("sign-in = %d %+v", code, st)
	}
	aws.Approve()
	deadline := time.Now().Add(5 * time.Second)
	for st.State == "waiting" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		computeCall(t, srv, "GET", "/api/compute/signin/"+st.ID, "", true, &st)
	}
	if st.State != "choose" {
		t.Fatalf("sign-in state %+v", st)
	}
	var conn byoc.Connection
	if code := computeCall(t, srv, "POST", "/api/compute/signin/"+st.ID+"/choose", `{"choice":"111122223333"}`, true, &conn); code != 200 || conn.ID != "aws-111122223333" {
		t.Fatalf("choose = %d %+v", code, conn)
	}
	var overview computeOverview
	computeCall(t, srv, "GET", "/api/compute", "", true, &overview)
	if len(overview.Connections) != 1 || overview.Instance != "dev" || len(overview.Clouds) != 2 {
		t.Fatalf("overview %+v", overview)
	}
	raw := map[string]any{}
	computeCall(t, srv, "GET", "/api/compute", "", true, &raw)
	if b, _ := json.Marshal(raw); strings.Contains(string(b), "t-111122223333") {
		t.Fatal("the overview carries the credentials")
	}
	var sizes struct{ Sizes []byoc.Size }
	if code := computeCall(t, srv, "GET", "/api/compute/connections/"+conn.ID+"/sizes?region=us-east-1", "", true, &sizes); code != 200 || len(sizes.Sizes) != 4 {
		t.Fatalf("sizes = %d %+v", code, sizes)
	}
	aws.Missing = []byoc.Permission{{Action: "ec2:RunInstances", Why: "to start the VM"}}
	var perms struct {
		OK      bool
		Summary string
	}
	computeCall(t, srv, "GET", "/api/compute/connections/"+conn.ID+"/permissions?region=us-east-1", "", true, &perms)
	if perms.OK || !strings.Contains(perms.Summary, "ec2:RunInstances") {
		t.Fatalf("permissions %+v", perms)
	}
	aws.Missing = nil
	var op byoc.Operation
	if code := computeCall(t, srv, "POST", "/api/compute/machines", `{"instance":"dev","connection":"`+conn.ID+`","region":"us-east-1","preset":"small","auto_stop_minutes":60}`, true, &op); code != 200 {
		t.Fatalf("create = %d", code)
	}
	op = waitComputeOp(t, srv, op.ID)
	if op.Error != "" || op.Machine == nil || op.Machine.State != byoc.Running {
		t.Fatalf("create op %+v", op)
	}
	if code := computeCall(t, srv, "POST", "/api/compute/machines/dev/scale", `{"preset":"huge"}`, true, nil); code != http.StatusBadRequest {
		t.Fatalf("scale to an unknown preset = %d", code)
	}
	computeCall(t, srv, "POST", "/api/compute/machines/dev/stop", `{}`, true, &op)
	if op = waitComputeOp(t, srv, op.ID); op.Machine.State != byoc.Stopped {
		t.Fatalf("stop op %+v", op)
	}
	var res struct{ Resources []byoc.Resource }
	computeCall(t, srv, "GET", "/api/compute/machines/dev/resources", "", true, &res)
	if len(res.Resources) != 3 {
		t.Fatalf("resources %+v", res)
	}
	if code := computeCall(t, srv, "DELETE", "/api/compute/connections/"+conn.ID, "", true, nil); code != http.StatusConflict {
		t.Fatalf("disconnect with a VM = %d", code)
	}
	computeCall(t, srv, "DELETE", "/api/compute/machines/dev", "", true, &op)
	if op = waitComputeOp(t, srv, op.ID); op.Error != "" {
		t.Fatalf("delete op %+v", op)
	}
	if code := computeCall(t, srv, "GET", "/api/compute/machines/dev", "", true, nil); code != http.StatusNotFound {
		t.Fatalf("a deleted machine = %d", code)
	}
	if code := computeCall(t, srv, "DELETE", "/api/compute/connections/"+conn.ID, "", true, nil); code != 200 {
		t.Fatalf("disconnect = %d", code)
	}
	if n := keys.Names(); len(n) != 0 {
		t.Fatalf("keychain items left %v", n)
	}
}

func waitComputeOp(t *testing.T, srv *httptest.Server, id string) byoc.Operation {
	t.Helper()
	var op byoc.Operation
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		computeCall(t, srv, "GET", "/api/compute/operations/"+id, "", true, &op)
		if op.Done {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s did not finish: %+v", id, op)
	return op
}
