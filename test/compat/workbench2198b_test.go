//go:build compat

package compat

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	"github.com/google/uuid"
)

// verifyIDToken checks an RS256 identity token against the instance's
// signing keys (the metadata server's /certs, a JWKS) and returns its
// claims.
func verifyIDToken(t *testing.T, metadataAddr, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		t.Fatalf("the identity token is not a JWT: %.80q", tok)
	}
	var header struct{ Alg, Kid string }
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if json.Unmarshal(hb, &header) != nil || header.Alg != "RS256" {
		t.Fatalf("the token's header = %s", hb)
	}
	resp, err := http.Get("http://" + metadataAddr + "/certs")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var jwks struct {
		Keys []struct{ Kid, N, E string }
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		t.Fatalf("the instance's /certs: %v %s", err, body)
	}
	var key *rsa.PublicKey
	for _, k := range jwks.Keys {
		if k.Kid == header.Kid {
			n, _ := base64.RawURLEncoding.DecodeString(k.N)
			e, _ := base64.RawURLEncoding.DecodeString(k.E)
			key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		}
	}
	if key == nil {
		t.Fatalf("no key %q in the instance's /certs", header.Kid)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("the identity token's signature does not verify against the instance's keys: %v", err)
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// covers: google.cloud.notebooks.v2.NotebookService/CreateInstance, google.cloud.notebooks.v2.NotebookService/DeleteInstance
//
// TestWorkbenchJupyterLabBearerAndIdentity (#2198): an instance's
// proxy_uri, which refuses a request with neither the console's session
// nor a token, takes a bearer token (Authorization: Bearer, as Google's
// proxy does) in place of the session; and the instance's metadata
// server mints identity tokens for its service account, as a Compute
// Engine VM's does: Google's google.oauth2.id_token.fetch_id_token in
// the kernel gets one for an audience, standard format (aud, azp, sub,
// iss); format=full adds email and google.compute_engine (the instance's
// name, ID, zone and project); every one verifies against the instance's
// IAM signing keys; a request without an audience is refused.
func TestWorkbenchJupyterLabBearerAndIdentity(t *testing.T) {
	h := New(t)
	_, clients := workbenchClients(t, h)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	meta := h.Endpoint(EnvMetadata)
	c := clients["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	id := "wb-id-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	op, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1-a", InstanceId: id,
		Instance: &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{}}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	in, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the create operation: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &nbpb.DeleteInstanceRequest{Name: in.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	m := proxyURIRE.FindStringSubmatch(in.GetProxyUri())
	if m == nil {
		t.Fatalf("proxy_uri %q", in.GetProxyUri())
	}
	get := func(auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+m[2]+"/api/status", nil)
		req.Host = in.GetProxyUri()
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Errorf("no session and no token = %d, want 401", code)
	}
	// The instance's own access token, from its metadata server, as
	// `gcloud auth print-access-token` gives one.
	req, _ := http.NewRequest(http.MethodGet, "http://"+meta+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if code, body := get("Bearer " + tok.AccessToken); code != http.StatusOK || !strings.Contains(body, "started") {
		t.Errorf("a bearer token = %d %.200s, want the server's status", code, body)
	}

	lab := openJupyterLab(t, in.GetProxyUri(), in.GetName())
	out, status := lab.execute(`
import google.auth.transport.requests, google.oauth2.id_token, os, urllib.request
print("fetched", google.oauth2.id_token.fetch_id_token(google.auth.transport.requests.Request(), "https://service.example.test"))
host = os.environ["GCE_METADATA_HOST"]
def md(path):
    r = urllib.request.Request("http://" + host + "/computeMetadata/v1/instance/service-accounts/default/" + path, headers={"Metadata-Flavor": "Google"})
    try:
        return urllib.request.urlopen(r).read().decode()
    except urllib.error.HTTPError as e:
        return "HTTP %d" % e.code
print("standard", md("identity?audience=https://standard.example.test"))
print("full", md("identity?audience=https://full.example.test&format=full"))
print("noaudience", md("identity"))
print("email", md("email"))
`)
	if status != "ok" {
		t.Fatalf("the cell = %s: %s", status, out)
	}
	field := func(name string) string {
		r := regexp.MustCompile(`(?m)^` + name + ` (\S+)`).FindStringSubmatch(out)
		if r == nil {
			t.Fatalf("no %s in %q", name, out)
		}
		return r[1]
	}
	email := field("email")
	// Google's fetch_id_token asks the metadata server for format=full.
	if fetched := verifyIDToken(t, meta, field("fetched")); fetched["aud"] != "https://service.example.test" || fetched["email"] != email {
		t.Errorf("fetch_id_token's claims = %v", fetched)
	}
	std := verifyIDToken(t, meta, field("standard"))
	if std["aud"] != "https://standard.example.test" || std["iss"] != "https://accounts.google.com" || std["sub"] == nil || std["azp"] != std["sub"] || std["email"] != nil {
		t.Errorf("the standard token's claims = %v", std)
	}
	full := verifyIDToken(t, meta, field("full"))
	ce, _ := full["google"].(map[string]any)["compute_engine"].(map[string]any)
	if full["aud"] != "https://full.example.test" || full["email"] != email || full["email_verified"] != true || ce == nil ||
		ce["instance_name"] != id || ce["instance_id"] != in.GetId() || ce["zone"] != "us-central1-a" || ce["project_id"] != project {
		t.Errorf("the full token's claims = %v", full)
	}
	if !strings.Contains(out, "noaudience HTTP 400") {
		t.Errorf("identity without an audience: %q, want HTTP 400", out)
	}
}

// covers: google.cloud.aiplatform.v1.NotebookService/AssignNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/GetNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/StopNotebookRuntime
//
// TestColabEnterpriseRuntimeJupyterServer (#2198): a Colab Enterprise
// runtime the official aiplatform client assigns runs the real Jupyter
// Server, as Google's runtimes do: RUNNING, its proxy_uri is
// HASH-dot-REGION.notebooks.localhost:PORT, where the console serves the
// runtime's JupyterLab; the server's kernels include the one the
// runtime's cells run on; a new Python 3 kernel queries the instance's
// BigQuery with Google's client and %%bigquery, its home /home/colab, and
// cannot reach the internet. A stopped runtime has no proxy_uri.
func TestColabEnterpriseRuntimeJupyterServer(t *testing.T) {
	h := New(t)
	_, clients := notebookClients(t, h)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	// The runtime's project is the one its BigQuery client queries.
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	top, err := c.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "lab-tpl-" + suffix,
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "JupyterLab template"}})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := top.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteNotebookRuntimeTemplate(context.Background(), &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: tpl.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := c.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: tpl.GetName(), NotebookRuntimeId: "lab-" + suffix,
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "JupyterLab runtime", RuntimeUser: "dev@example.com"}})
	if err != nil {
		t.Fatalf("AssignNotebookRuntime: %v", err)
	}
	rt, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the assign operation: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteNotebookRuntime(context.Background(), &aiplatformpb.DeleteNotebookRuntimeRequest{Name: rt.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if m := proxyURIRE.FindStringSubmatch(rt.GetProxyUri()); rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_RUNNING || m == nil || m[1] != "us-central1" {
		t.Fatalf("assigned runtime: %s, proxy_uri %q", rt.GetRuntimeState(), rt.GetProxyUri())
	}
	got, err := c.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: rt.GetName()})
	if err != nil || got.GetProxyUri() != rt.GetProxyUri() {
		t.Fatalf("GetNotebookRuntime's proxy_uri = %q, %v", got.GetProxyUri(), err)
	}
	lab := openJupyterLab(t, rt.GetProxyUri(), rt.GetName())
	var kernels []struct{ Name string }
	if st, b := lab.do(http.MethodGet, "/api/kernels", nil); st != http.StatusOK || json.Unmarshal(b, &kernels) != nil || len(kernels) != 1 || kernels[0].Name != "python3" {
		t.Errorf("the runtime's kernels = %d %s, want its one python3 kernel", st, b)
	}
	out, status := lab.execute(`
import os
from google.cloud import bigquery
print("total", list(bigquery.Client().query("SELECT 40 + 2 AS t").result())[0].t, os.environ.get("HOME"))
ip = get_ipython()
ip.run_cell_magic("bigquery", "df", "SELECT 7 AS seven")
print("magic", int(ip.user_ns["df"]["seven"][0]))
import urllib.request
try:
    urllib.request.urlopen("https://www.google.com", timeout=5)
    print("egress reached the internet")
except Exception as e:
    print("egress blocked", type(e).__name__)
`)
	if status != "ok" {
		t.Fatalf("the cell = %s: %s", status, out)
	}
	for _, want := range []string{"total 42 /home/colab", "magic 7", "egress blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("the cell's output %q lacks %q", out, want)
		}
	}
	sop, err := c.StopNotebookRuntime(ctx, &aiplatformpb.StopNotebookRuntimeRequest{Name: rt.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: rt.GetName()}); err != nil || got.GetProxyUri() != "" {
		t.Errorf("a stopped runtime's proxy_uri = %q, %v", got.GetProxyUri(), err)
	}
}
