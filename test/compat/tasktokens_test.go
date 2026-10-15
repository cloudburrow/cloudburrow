//go:build compat

package compat

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bearerTarget is a local HTTP target that hands over the Authorization
// header of each request it receives.
func bearerTarget(t *testing.T) (*httptest.Server, chan string) {
	t.Helper()
	got := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func receive(t *testing.T, got chan string, what string) string {
	t.Helper()
	select {
	case h := <-got:
		return h
	case <-time.After(15 * time.Second):
		t.Fatalf("%s was not delivered to the target", what)
		return ""
	}
}

// checkIDToken checks an OIDC bearer the way a Cloud Run service behind
// Google's front end would read it: Google's claims, for the audience and
// the service account. When the metadata server is exported, the signature
// is verified against the instance's own /certs; it would not verify
// against Google's, by design.
func checkIDToken(t *testing.T, header, aud, email string) {
	t.Helper()
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %.20q, want a Bearer token", header)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("the OIDC token is not a JWT (%d segments)", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"iss": "https://accounts.google.com", "aud": aud, "email": email, "email_verified": true} {
		if claims[k] != want {
			t.Errorf("claim %s = %v, want %v", k, claims[k], want)
		}
	}
	if exp, _ := claims["exp"].(float64); time.Unix(int64(exp), 0).Before(time.Now()) {
		t.Errorf("exp %v is not in the future", claims["exp"])
	}
	meta := strings.TrimSpace(os.Getenv(EnvMetadata))
	if meta == "" {
		return
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+strings.TrimPrefix(meta, "http://")+"/certs", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch the instance's certs: %v", err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct{ N, E string } `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil || len(jwks.Keys) == 0 {
		t.Fatalf("decode the instance's certs: %v", err)
	}
	n, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].N)
	e, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].E)
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("the OIDC token does not verify against the instance's /certs: %v", err)
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/GetTask
//
// TestTasksOIDCAndOAuthTokens (#1243): through the official client, an HTTP
// task's oidcToken and oauthToken are kept and read back, and a local target
// receives Authorization: Bearer with a locally minted token: an ID token
// for the service account whose audience is the URL when none is given, or
// an opaque access token. A token without its service account is
// INVALID_ARGUMENT.
func TestTasksOIDCAndOAuthTokens(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "compat-tokens")
	srv, got := bearerTarget(t)
	sa := "invoker@" + h.Project() + ".iam.gserviceaccount.com"

	create := func(hr *cloudtaskspb.HttpRequest) *cloudtaskspb.Task {
		t.Helper()
		task, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: q, Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: hr}}})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		return task
	}

	// OIDC with no audience: the URL is the audience. Read back first; the
	// task may be dispatched at once, so a NotFound afterwards is a
	// delivered task.
	url := srv.URL + "/handle?x=1"
	task := create(&cloudtaskspb.HttpRequest{Url: url, AuthorizationHeader: &cloudtaskspb.HttpRequest_OidcToken{
		OidcToken: &cloudtaskspb.OidcToken{ServiceAccountEmail: sa}}})
	if o := task.GetHttpRequest().GetOidcToken(); o.GetServiceAccountEmail() != sa || o.GetAudience() != "" {
		t.Errorf("CreateTask returned oidcToken %v, want the account and no audience", o)
	}
	if back, err := c.GetTask(ctx, &cloudtaskspb.GetTaskRequest{Name: task.GetName()}); err == nil {
		if back.GetHttpRequest().GetOidcToken().GetServiceAccountEmail() != sa {
			t.Errorf("GetTask oidcToken = %v", back.GetHttpRequest().GetOidcToken())
		}
	} else if status.Code(err) != codes.NotFound {
		t.Errorf("GetTask: %v", err)
	}
	checkIDToken(t, receive(t, got, "the OIDC task"), url, sa)

	// OIDC with an audience, which replaces an Authorization the task set.
	create(&cloudtaskspb.HttpRequest{Url: srv.URL, Headers: map[string]string{"Authorization": "Bearer mine"},
		AuthorizationHeader: &cloudtaskspb.HttpRequest_OidcToken{OidcToken: &cloudtaskspb.OidcToken{ServiceAccountEmail: sa, Audience: "https://svc.example"}}})
	checkIDToken(t, receive(t, got, "the OIDC task with an audience"), "https://svc.example", sa)

	// OAuth: an opaque access token, as on Google.
	oauth := create(&cloudtaskspb.HttpRequest{Url: srv.URL, AuthorizationHeader: &cloudtaskspb.HttpRequest_OauthToken{
		OauthToken: &cloudtaskspb.OAuthToken{ServiceAccountEmail: sa, Scope: "https://www.googleapis.com/auth/cloud-platform"}}})
	if o := oauth.GetHttpRequest().GetOauthToken(); o.GetServiceAccountEmail() != sa || o.GetScope() == "" {
		t.Errorf("CreateTask returned oauthToken %v", o)
	}
	if hdr := receive(t, got, "the OAuth task"); !strings.HasPrefix(hdr, "Bearer ") || len(hdr) < len("Bearer ")+8 || strings.Count(hdr, ".") == 2 {
		t.Errorf("OAuth Authorization = %.16q..., want an opaque Bearer token", hdr)
	}

	_, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: q, Task: &cloudtaskspb.Task{
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{Url: srv.URL,
			AuthorizationHeader: &cloudtaskspb.HttpRequest_OidcToken{OidcToken: &cloudtaskspb.OidcToken{}}}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an oidcToken without serviceAccountEmail = %v, want InvalidArgument", err)
	}
}

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/RunJob
//
// TestSchedulerJobTokens (#1243): through the official client, an HTTP job's
// oidcToken and oauthToken are kept and read back, and RunJob delivers to a
// local target with Authorization: Bearer and a locally minted token, the
// ID token's audience being the URI when none is given.
func TestSchedulerJobTokens(t *testing.T) {
	h := New(t)
	c := schedulerClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/us-central1"
	srv, got := bearerTarget(t)
	sa := "scheduler@" + h.Project() + ".iam.gserviceaccount.com"

	job := func(id string, auth any) *schedulerpb.Job {
		t.Helper()
		target := &schedulerpb.HttpTarget{Uri: srv.URL + "/" + id, HttpMethod: schedulerpb.HttpMethod_POST}
		switch a := auth.(type) {
		case *schedulerpb.OidcToken:
			target.AuthorizationHeader = &schedulerpb.HttpTarget_OidcToken{OidcToken: a}
		case *schedulerpb.OAuthToken:
			target.AuthorizationHeader = &schedulerpb.HttpTarget_OauthToken{OauthToken: a}
		}
		// Yearly, so only RunJob fires it during the test.
		j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
			Name: parent + "/jobs/" + id, Schedule: "0 0 1 1 *", Target: &schedulerpb.Job_HttpTarget{HttpTarget: target}}})
		if err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}
		t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: j.GetName()}) })
		back, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: j.GetName()})
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
			t.Fatalf("RunJob %s: %v", id, err)
		}
		return back
	}

	oidc := job("compat-oidc", &schedulerpb.OidcToken{ServiceAccountEmail: sa})
	if o := oidc.GetHttpTarget().GetOidcToken(); o.GetServiceAccountEmail() != sa {
		t.Errorf("GetJob oidcToken = %v", o)
	}
	checkIDToken(t, receive(t, got, "the OIDC job"), srv.URL+"/compat-oidc", sa)

	aud := job("compat-oidc-aud", &schedulerpb.OidcToken{ServiceAccountEmail: sa, Audience: "https://svc.example"})
	if aud.GetHttpTarget().GetOidcToken().GetAudience() != "https://svc.example" {
		t.Errorf("GetJob oidcToken = %v", aud.GetHttpTarget().GetOidcToken())
	}
	checkIDToken(t, receive(t, got, "the OIDC job with an audience"), "https://svc.example", sa)

	oauth := job("compat-oauth", &schedulerpb.OAuthToken{ServiceAccountEmail: sa})
	if oauth.GetHttpTarget().GetOauthToken().GetServiceAccountEmail() != sa {
		t.Errorf("GetJob oauthToken = %v", oauth.GetHttpTarget().GetOauthToken())
	}
	if hdr := receive(t, got, "the OAuth job"); !strings.HasPrefix(hdr, "Bearer ") || strings.Count(hdr, ".") == 2 {
		t.Errorf("OAuth Authorization = %.16q..., want an opaque Bearer token", hdr)
	}

	_, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/compat-no-account", Schedule: "0 0 1 1 *", Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: srv.URL, AuthorizationHeader: &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{}}}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an oidcToken without service_account_email = %v, want InvalidArgument", err)
	}
}
