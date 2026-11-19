//go:build compat

package compat

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"cloud.google.com/go/run/apiv2/runpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// idTokenFor is an ID token for account and audience, minted through IAM
// Credentials' generateIdToken as the instance's own account (which
// newIAMPrincipal made a Token Creator on the account, and which may mint
// its own).
func idTokenFor(t *testing.T, h *Harness, account, audience string) string {
	t.Helper()
	meta := h.Endpoint(EnvMetadata)
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	_, source := instanceAccount(t)
	hc := &http.Client{Transport: &oauth2.Transport{Source: source}}
	t.Cleanup(hc.CloseIdleConnections)
	ic, err := credentials.NewIamCredentialsRESTClient(h.Context(), option.WithEndpoint(meta), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	tok, err := ic.GenerateIdToken(h.Context(), &credentialspb.GenerateIdTokenRequest{Name: "projects/-/serviceAccounts/" + account,
		Audience: audience, IncludeEmail: true})
	if err != nil {
		t.Fatalf("GenerateIdToken for %s: %v", account, err)
	}
	return tok.GetToken()
}

// invokeAs GETs path of the service at host through the ingress at base,
// with token as its bearer ("" for none).
func invokeAs(t *testing.T, base, host, path, token string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// A transport of its own: never the owner's token the suite's
	// default transport adds (ownerinvoke1879_test.go).
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitInvoke retries until GET path answers want, the last answer
// otherwise: a revision's first request can arrive while it starts.
func waitInvoke(t *testing.T, base, host, path, token string, want int) (int, string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		code, body := invokeAs(t, base, host, path, token)
		if code == want || time.Now().After(deadline) {
			return code, body
		}
		time.Sleep(time.Second)
	}
}

// covers: google.cloud.run.v2.Services/SetIamPolicy, google.cloud.run.v2.Services/GetIamPolicy
//
// TestIAMEnforcementRunInvoke (#1879): a Cloud Run service made through the
// official client, invoked through the instance's ingress. Without a
// token, and without allUsers holding roles/run.invoker, the request is
// Google's 403 page ("Error: Forbidden", "Your client does not have
// permission to get URL <code>/</code> from this server."); an ID token
// for a service account without the role is the same 403; its access token
// is 401, as Cloud Run takes only ID tokens; roles/run.invoker on the
// service's policy lets the ID token through and the revoke refuses it
// again; the role on the project is inherited; allUsers on the service
// lets a request without a token through.
func TestIAMEnforcementRunInvoke(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvResourceManager)
	requireIAMEnforcement(t)
	if os.Getenv(envKubeconfig) == "" {
		if cli := os.Getenv(EnvCLI); cli != "" {
			t.Setenv(envKubeconfig, filepath.Join(instanceDirFrom(t, cliArgs()), "kubeconfig"))
		}
	}
	ctx := runWait(t)
	// A project of the test's own, so its principal's project grant is
	// the service's ancestry's.
	project := iamProject(t, h, "ierun-")
	parent := "projects/" + project + strings.TrimPrefix(runParent(h), "projects/"+h.Project())
	id := "iam-invoke"
	name := parent + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	base, host := ingress(t), hostOf(t, svc.GetUri())
	forbidden := func(what string, code int, body string) {
		t.Helper()
		if code != http.StatusForbidden || !strings.Contains(body, "<h1>Error: Forbidden</h1>") ||
			!strings.Contains(body, "Your client does not have permission to get URL <code>/</code> from this server.") {
			t.Fatalf("%s = %d %q; want Google's 403 page", what, code, body)
		}
	}
	code, body := waitInvoke(t, base, host, "/", "", http.StatusForbidden)
	forbidden("GET without a token", code, body)

	p := newIAMPrincipal(t, h, project, "ierun")
	idt := idTokenFor(t, h, p.email, svc.GetUri())
	code, body = invokeAs(t, base, host, "/", idt)
	forbidden("GET with an ID token of an account without run.invoker", code, body)
	if code, body := invokeAs(t, base, host, "/", p.token); code != http.StatusUnauthorized {
		t.Fatalf("GET with an access token = %d %q; want 401", code, body)
	}
	if code, body := invokeAs(t, base, host, "/", idTokenFor(t, h, p.email, "http://elsewhere.example")); code != http.StatusUnauthorized {
		t.Fatalf("GET with an ID token for another audience = %d %q; want 401", code, body)
	}

	getPol := func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return c.GetIamPolicy(ctx, r) }
	setPol := func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return c.SetIamPolicy(ctx, r) }
	undo := bindOn(t, func(ctx context.Context, r *iampb.GetIamPolicyRequest, _ ...any) (*iampb.Policy, error) {
		return getPol(r)
	},
		func(ctx context.Context, r *iampb.SetIamPolicyRequest, _ ...any) (*iampb.Policy, error) {
			return setPol(r)
		},
		name, &iampb.Binding{Role: "roles/run.invoker", Members: []string{"serviceAccount:" + p.email}})
	if code, body := waitInvoke(t, base, host, "/", idt, http.StatusOK); code != http.StatusOK || !strings.Contains(body, "Hello") {
		t.Fatalf("GET with run.invoker on the service = %d %q", code, body)
	}
	undo()
	code, body = invokeAs(t, base, host, "/", idt)
	forbidden("GET after the revoke", code, body)

	p.grant("roles/run.invoker")
	if code, body := waitInvoke(t, base, host, "/", idt, http.StatusOK); code != http.StatusOK {
		t.Fatalf("GET with run.invoker on the project = %d %q", code, body)
	}
	p.revoke("roles/run.invoker")
	code, body = invokeAs(t, base, host, "/", idt)
	forbidden("GET after the project's revoke", code, body)

	undo = bindOn(t, func(ctx context.Context, r *iampb.GetIamPolicyRequest, _ ...any) (*iampb.Policy, error) {
		return getPol(r)
	},
		func(ctx context.Context, r *iampb.SetIamPolicyRequest, _ ...any) (*iampb.Policy, error) {
			return setPol(r)
		},
		name, &iampb.Binding{Role: "roles/run.invoker", Members: []string{"allUsers"}})
	if code, body := waitInvoke(t, base, host, "/", "", http.StatusOK); code != http.StatusOK || !strings.Contains(body, "Hello") {
		t.Fatalf("GET without a token, allUsers holding run.invoker = %d %q", code, body)
	}
	undo()
	code, body = invokeAs(t, base, host, "/", "")
	forbidden("GET without a token after allUsers is gone", code, body)
}
