package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// startObservedSecrets runs a real Secret Manager server on a loopback port,
// observed the way `up` observes it, and returns a client and the recorder.
func startObservedSecrets(t *testing.T) (secretmanagerpb.SecretManagerServiceClient, string, *admin.Recorder) {
	t.Helper()
	rec := admin.NewRecorder(1000, nil)
	srv := secrets.NewServer("127.0.0.1:0", secrets.NewStore(store.NewMemory()))
	srv.Observe(callEvents(rec, nil, "secretmanager"), requestEvents(rec, nil, "secretmanager"))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return secretmanagerpb.NewSecretManagerServiceClient(conn), srv.Addr(), rec
}

// TestSecretManagerCallsAreRecordedWithoutTheirPayloads.
//
// /admin/events was bounded, served and always empty, because nothing recorded
// into it (#273). Every call on a port CloudBurrow serves is now recorded — and
// a Secret Manager value must never be among what is kept, or the event log
// would be the one place a secret outlived the call that read it.
func TestSecretManagerCallsAreRecordedWithoutTheirPayloads(t *testing.T) {
	client, _, rec := startObservedSecrets(t)
	ctx := context.Background()
	const marker = "PAYLOAD-MARKER-7f3a9c"
	parent := "projects/demo-local"

	sec, err := client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: parent, SecretId: "api-key",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: sec.Name, Payload: &secretmanagerpb.SecretPayload{Data: []byte(marker)},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: sec.Name + "/versions/latest"})
	if err != nil || string(got.Payload.Data) != marker {
		t.Fatalf("access = %v, %v", got, err)
	}
	// A missing secret is recorded with the code the caller received.
	if _, err := client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{
		Name: parent + "/secrets/does-not-exist"}); err == nil {
		t.Fatal("GetSecret on a missing secret succeeded")
	}

	events := rec.EventsWhere(admin.Filter{Service: "secretmanager", Kind: requestKind}, 0)
	byMethod := map[string]admin.Event{}
	for _, e := range events {
		byMethod[e.Target[strings.LastIndex(e.Target, "/")+1:]] = e
	}
	for _, m := range []string{"CreateSecret", "AddSecretVersion", "AccessSecretVersion", "GetSecret"} {
		e, ok := byMethod[m]
		if !ok {
			t.Fatalf("%s was not recorded; got %d events", m, len(events))
		}
		if e.Detail["duration_ms"] == "" || e.Detail["code"] == "" {
			t.Errorf("%s event lacks a code or a duration: %+v", m, e.Detail)
		}
		if e.Detail["project"] != "demo-local" {
			t.Errorf("%s event project = %q, want demo-local", m, e.Detail["project"])
		}
	}
	if byMethod["GetSecret"].Detail["code"] != "NOT_FOUND" {
		t.Errorf("a missing secret was recorded as %q, want NOT_FOUND", byMethod["GetSecret"].Detail["code"])
	}
	if byMethod["AccessSecretVersion"].Detail["code"] != "OK" {
		t.Errorf("a successful access was recorded as %q", byMethod["AccessSecretVersion"].Detail["code"])
	}

	// The payload appears nowhere in what was recorded — not in any field of
	// any event, and not in the admin API's own response.
	all, _ := json.Marshal(rec.EventsWhere(admin.Filter{}, 0))
	if strings.Contains(string(all), marker) {
		t.Fatalf("a secret payload was recorded:\n%s", all)
	}
}

// TestSecretManagerJSONRequestsAreRecordedOnce.
//
// The JSON surface shares the port with gRPC, and only the JSON path is wrapped
// by the HTTP observer: wrapping both would record every gRPC call twice. The
// query string is never recorded, because that is where a careless caller puts
// a token.
func TestSecretManagerJSONRequestsAreRecordedOnce(t *testing.T) {
	_, addr, rec := startObservedSecrets(t)
	resp, err := http.Get(fmt.Sprintf("http://%s/v1/projects/demo-local/secrets?access_token=LEAKED-TOKEN", addr))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	var httpEvents []admin.Event
	for _, e := range rec.EventsWhere(admin.Filter{Service: "secretmanager"}, 0) {
		if e.Detail["transport"] == "http" {
			httpEvents = append(httpEvents, e)
		}
	}
	if len(httpEvents) != 1 {
		t.Fatalf("one JSON request produced %d HTTP events", len(httpEvents))
	}
	e := httpEvents[0]
	if !strings.HasPrefix(e.Target, "GET /v1/projects/demo-local/secrets") || e.Detail["code"] != "200" {
		t.Errorf("event = %+v", e)
	}
	all, _ := json.Marshal(rec.EventsWhere(admin.Filter{}, 0))
	if strings.Contains(string(all), "LEAKED-TOKEN") {
		t.Fatalf("the query string was recorded:\n%s", all)
	}
}

// TestTheEventRingStaysBounded.
func TestTheEventRingStaysBounded(t *testing.T) {
	rec := admin.NewRecorder(1000, nil)
	obs := callEvents(rec, nil, "tasks")
	for i := 0; i < 10000; i++ {
		obs(callFixture(i))
	}
	if rec.Len() != 1000 {
		t.Fatalf("after 10k calls the ring holds %d events, want its bound of 1000", rec.Len())
	}
	// The newest are the ones kept.
	newest := rec.EventsWhere(admin.Filter{}, 1)[0]
	if newest.Detail["resource"] != "projects/p/locations/l/queues/q9999" {
		t.Errorf("newest event = %+v", newest.Detail)
	}
}

// TestEventFiltersNarrowByKindAndTime.
func TestEventFiltersNarrowByKindAndTime(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := now
	rec := admin.NewRecorder(100, func() time.Time { return clock })
	rec.Record("tasks", "request", "A", nil)
	clock = now.Add(time.Second)
	rec.Record("tasks", "dispatch", "B", nil)
	clock = now.Add(2 * time.Second)
	rec.Record("tasks", "request", "C", nil)

	if got := rec.EventsWhere(admin.Filter{Kind: "request"}, 0); len(got) != 2 {
		t.Errorf("kind=request returned %d events, want 2", len(got))
	}
	// since is strictly after, so a poller passing its last timestamp sees each
	// event once.
	got := rec.EventsWhere(admin.Filter{Since: now.Add(time.Second)}, 0)
	if len(got) != 1 || got[0].Target != "C" {
		t.Errorf("since=+1s returned %+v, want only C", got)
	}
}

func callFixture(i int) grpctransport.Call {
	return grpctransport.Call{
		Method:   "/google.cloud.tasks.v2.CloudTasks/CreateQueue",
		Resource: fmt.Sprintf("projects/p/locations/l/queues/q%d", i),
	}
}
