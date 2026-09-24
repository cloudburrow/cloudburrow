package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const payloadMarker = "PAYLOAD-MARKER-291"

func requestLogConsole(t *testing.T, rec *admin.Recorder) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceTasks, config.ServiceSecrets}
	c := console.New("127.0.0.1:0", nil)
	c.SetRequests(newConsoleRequests(rec, cfg))
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return srv
}

type requestsPage struct {
	Requests []struct {
		Service, Method, Code string
		DurationMS            *int64 `json:"duration_ms"`
	}
	Unobserved []struct{ Service, Label string }
}

func getRequests(t *testing.T, srv *httptest.Server, query string) (requestsPage, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/requests" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var p requestsPage
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return p, string(raw)
}

// TestTheRequestLogShowsServedCalls: calls made through the Secret Manager
// gRPC API appear in the console's Request Log with method, code and
// duration; the service and code filters narrow it; unobserved services
// carry their label; and no payload reaches any response.
func TestTheRequestLogShowsServedCalls(t *testing.T) {
	client, _, rec := startObservedSecrets(t)
	srv := requestLogConsole(t, rec)
	ctx := context.Background()
	sec, err := client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/log-proj", SecretId: "k",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: sec.Name, Payload: &secretmanagerpb.SecretPayload{Data: []byte(payloadMarker)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: sec.Name + "/versions/latest"}); err != nil {
		t.Fatal(err)
	}
	_, _ = client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/log-proj/secrets/absent"})

	page, raw := getRequests(t, srv, "")
	byMethod := map[string]string{}
	for _, r := range page.Requests {
		if r.DurationMS == nil {
			t.Errorf("%s has no duration", r.Method)
		}
		byMethod[r.Method[strings.LastIndex(r.Method, "/")+1:]] = r.Code
	}
	for method, code := range map[string]string{"CreateSecret": "OK", "AddSecretVersion": "OK", "AccessSecretVersion": "OK", "GetSecret": "NOT_FOUND"} {
		if byMethod[method] != code {
			t.Errorf("%s = %q in the Request Log, want %s", method, byMethod[method], code)
		}
	}
	if strings.Contains(raw, payloadMarker) {
		t.Fatalf("a secret payload reached the console: %s", raw)
	}
	unobserved := map[string]string{}
	for _, u := range page.Unobserved {
		unobserved[u.Service] = u.Label
	}
	for _, s := range []string{"storage", "pubsub"} {
		if unobserved[s] != console.NotObservable {
			t.Errorf("%s is labelled %q, want %q", s, unobserved[s], console.NotObservable)
		}
	}
	if _, ok := unobserved["secretmanager"]; ok {
		t.Error("Secret Manager, which is observed, is labelled unobservable")
	}

	if p, _ := getRequests(t, srv, "?code=NOT_FOUND"); len(p.Requests) != 1 || !strings.HasSuffix(p.Requests[0].Method, "GetSecret") {
		t.Errorf("code filter returned %+v", p.Requests)
	}
	if p, _ := getRequests(t, srv, "?service=tasks"); len(p.Requests) != 0 {
		t.Errorf("service filter returned %+v", p.Requests)
	}
}

// A call made while the page is open arrives over the stream, filtered.
func TestTheRequestLogIsLive(t *testing.T) {
	client, _, rec := startObservedSecrets(t)
	srv := requestLogConsole(t, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream?stream=requests&code=NOT_FOUND", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(200 * time.Millisecond)
		// OK: filtered out. NOT_FOUND: delivered.
		_, _ = client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/live-proj"})
		_, _ = client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/live-proj/secrets/absent"})
	}()
	sc := bufio.NewScanner(resp.Body)
	var event string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") && event == "request" {
			var e console.RequestEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(e.Method, "GetSecret") || e.Code != "NOT_FOUND" {
				t.Errorf("the stream delivered %+v past a code filter of NOT_FOUND", e)
			}
			return
		}
	}
	t.Fatal("no request arrived on the stream")
}

// Whatever a recorded event carries beyond the named fields stays out of the
// console: the adapter copies fields, it does not pass events through.
func TestTheRequestLogCopiesOnlyNamedFields(t *testing.T) {
	rec := admin.NewRecorder(10, nil)
	rec.Record("secretmanager", requestKind, "/google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion",
		map[string]string{"code": "OK", "duration_ms": "3", "payload": payloadMarker, "authorization": "Bearer " + payloadMarker})
	srv := requestLogConsole(t, rec)
	if _, raw := getRequests(t, srv, ""); strings.Contains(raw, payloadMarker) || !strings.Contains(raw, "AccessSecretVersion") {
		t.Errorf("console response: %s", raw)
	}
}
