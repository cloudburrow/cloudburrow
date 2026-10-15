//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/firestore"
	"cloud.google.com/go/spanner"
	"google.golang.org/api/iterator"
)

// waitEvents polls /admin/events until every method in want is recorded for
// service, and returns them by short method name.
func waitEvents(t *testing.T, h *Harness, service string, since time.Time, want ...string) map[string]adminEvent {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ev := methodsOf(events(t, h, h.Endpoint(EnvControl), service, since))
		missing := false
		for _, m := range want {
			if _, ok := ev[m]; !ok {
				missing = true
			}
		}
		if !missing {
			return ev
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s calls in /admin/events: %v; want %v", service, ev, want)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// measuredInConsole fails unless /metrics counts service and the console's
// request metrics no longer list it unmeasured.
func measuredInConsole(t *testing.T, h *Harness, service string, series ...string) {
	t.Helper()
	code, body := adminDo(t, http.MethodGet, "http://"+h.Endpoint(EnvControl)+"/metrics", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: %d", code)
	}
	for _, s := range series {
		if !strings.Contains(body, s) {
			t.Errorf("/metrics has no %s", s)
		}
	}
	if strings.Contains(body, fmt.Sprintf(`cloudburrow_service_measured{service="%s"} 0`, service)) {
		t.Errorf("/metrics still reports %s unmeasured", service)
	}
	code, body = consoleDo(t, consoleAddr(t, h), http.MethodGet, "/api/metrics/requests", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/metrics/requests: %d", code)
	}
	var rm struct{ Unmeasured []string }
	if err := json.Unmarshal([]byte(body), &rm); err != nil {
		t.Fatalf("decode /api/metrics/requests: %v", err)
	}
	for _, s := range rm.Unmeasured {
		if s == service {
			t.Errorf("the console's request metrics still name %s unmeasured", service)
		}
	}
}

// TestFirestoreCallsReachMetricsAndTheRequestLog: Firestore's calls from
// the host are recorded (#1267), now by the call front in the emulator's
// pod (#1288): a write and a read of a missing document with the official
// client are counted in /metrics by method and code and recorded in
// /admin/events with their database and duration.
func TestFirestoreCallsReachMetricsAndTheRequestLog(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	since := time.Now().Add(-time.Second)
	c, err := firestore.NewClient(h.Context(), h.Project())
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()
	doc := c.Collection("metrics1267").Doc(fmt.Sprintf("d%d", time.Now().UnixNano()))
	if _, err := doc.Set(h.Context(), map[string]any{"n": 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Cleanup(func() { _, _ = doc.Delete(h.Context()) })
	if _, err := c.Collection("metrics1267").Doc("never-written").Get(h.Context()); err == nil {
		t.Fatal("Get of a missing document succeeded")
	}
	ev := waitEvents(t, h, "firestore", since, "Commit", "BatchGetDocuments")
	db := "projects/" + h.Project() + "/databases/(default)"
	for _, m := range []string{"Commit", "BatchGetDocuments"} {
		if e := ev[m]; e.Detail["code"] != "OK" || e.Detail["resource"] != db || e.Detail["duration_ms"] == "" || e.Detail["transport"] != "grpc" {
			t.Errorf("%s recorded %v; want OK on %s with a duration, over grpc", m, e.Detail, db)
		}
	}
	measuredInConsole(t, h, "firestore", `cloudburrow_requests_total{service="firestore",method="Commit",code="OK"}`)
}

// TestBigtableCallsReachMetricsAndTheRequestLog: the Bigtable front keeps a
// call log the CLI reads (#1267), so the official clients' calls, and the
// ones the front refuses itself, are counted and recorded with their table.
func TestBigtableCallsReachMetricsAndTheRequestLog(t *testing.T) {
	h := New(t)
	since := time.Now().Add(-time.Second)
	table := fmt.Sprintf("metrics%d", time.Now().UnixNano())
	admin, c := bigtableTable(t, h, table)
	tbl := c.Open(table)
	mut := bigtable.NewMutation()
	mut.Set("info", "q", bigtable.Now(), []byte("v"))
	if err := tbl.Apply(h.Context(), "r1", mut); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tbl.ReadRows(h.Context(), bigtable.InfiniteRange(""), func(bigtable.Row) bool { return true }); err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	if _, err := admin.Backups(h.Context(), "c").Next(); err == nil {
		t.Fatal("ListBackups succeeded")
	}
	ev := waitEvents(t, h, "bigtable", since, "CreateTable", "MutateRow", "ReadRows")
	name := fmt.Sprintf("projects/%s/instances/%s/tables/%s", h.Project(), bigtableInstance, table)
	if e := ev["MutateRow"]; e.Detail["code"] != "OK" || e.Detail["resource"] != name || e.Detail["duration_ms"] == "" {
		t.Errorf("MutateRow recorded %v; want OK on %s with a duration", e.Detail, name)
	}
	if e := ev["ReadRows"]; e.Detail["stream"] != "true" {
		t.Errorf("ReadRows recorded %v; want a stream", e.Detail)
	}
	// A refused call is answered before its request is read, so it names
	// no resource, and no project: it is looked for among every project's.
	url := fmt.Sprintf("http://%s/admin/events?service=bigtable&kind=request&limit=1000&since=%s",
		h.Endpoint(EnvControl), since.UTC().Format(time.RFC3339Nano))
	if _, raw := adminDo(t, http.MethodGet, url, "", nil); !strings.Contains(raw, `"target":"/google.bigtable.admin.v2.BigtableTableAdmin/ListBackups"`) ||
		!strings.Contains(raw, `"code":"UNIMPLEMENTED"`) {
		t.Errorf("no UNIMPLEMENTED ListBackups in /admin/events: %.500s", raw)
	}
	measuredInConsole(t, h, "bigtable", `cloudburrow_requests_total{service="bigtable",method="MutateRow",code="OK"}`)
}

// TestSpannerCallsReachMetricsAndTheRequestLog: Spanner's tunnel is raw and
// it had no front, so none of its calls were seen; the call front in its pod
// (#1288) records each one, so the official client's are counted in /metrics
// by method and code and recorded in /admin/events with their database and
// duration, the failed query among them.
func TestSpannerCallsReachMetricsAndTheRequestLog(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	since := time.Now().Add(-time.Second)
	db := spannerDatabase(t, ctx, addr, h.Project(), "cb-metrics", fmt.Sprintf("m%d", time.Now().UnixNano()%1e9),
		`CREATE TABLE T (Id INT64 NOT NULL) PRIMARY KEY (Id)`)
	c, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	defer c.Close()
	if _, err := c.Apply(ctx, []*spanner.Mutation{spanner.Insert("T", []string{"Id"}, []any{int64(1)})}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	it := c.Single().Query(ctx, spanner.Statement{SQL: "SELECT Id FROM Absent"})
	_, err = it.Next()
	it.Stop()
	if err == nil || err == iterator.Done {
		t.Fatalf("a query of a missing table = %v, want an error", err)
	}
	ev := waitEvents(t, h, "spanner", since, "CreateDatabase", "Commit", "ExecuteStreamingSql")
	if e := ev["CreateDatabase"]; e.Detail["code"] != "OK" || e.Detail["transport"] != "grpc" || e.Detail["duration_ms"] == "" {
		t.Errorf("CreateDatabase recorded %v; want OK over grpc with a duration", e.Detail)
	}
	if e := ev["Commit"]; e.Detail["code"] != "OK" || !strings.HasPrefix(e.Detail["resource"], db) {
		t.Errorf("Commit recorded %v; want OK on %s", e.Detail, db)
	}
	if e := ev["ExecuteStreamingSql"]; e.Detail["code"] != "INVALID_ARGUMENT" && e.Detail["code"] != "NOT_FOUND" {
		t.Errorf("the failed query recorded %v; want its error code", e.Detail)
	}
	measuredInConsole(t, h, "spanner", `cloudburrow_requests_total{service="spanner",method="Commit",code="OK"}`)
}

// TestAPodsFirestoreCallsReachMetricsAndTheRequestLog: a pod's Firestore
// calls reach the emulator's Service directly, never the host tunnel, so
// they were not counted (#1288). A one-off pod writes and reads a document
// with the official client at firestore.<namespace>.svc.cluster.local:8080,
// in a project of its own, and its Commit and BatchGetDocuments are in
// /admin/events with that project's database and in /metrics: the call
// front serves that port in the emulator's pod.
func TestAPodsFirestoreCallsReachMetricsAndTheRequestLog(t *testing.T) {
	h := New(t)
	_ = h.Endpoint(EnvFirestore)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)
	since := time.Now().Add(-time.Second)
	project := fmt.Sprintf("podfs-%d", time.Now().UnixNano()%1e9)
	endpoint := "firestore." + testNamespace() + ".svc.cluster.local:8080"
	q := url.Values{"endpoint": {endpoint}, "project": {project}, "collection": {"metrics1288"}, "doc": {"d"},
		"traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}
	body := runPod(t, ctx, kubeconfig, "compat-fspod-"+project, envProbeImage, nil, "firestore", q.Encode())
	if !strings.Contains(body, "FIRESTORE PROBE: OK endpoint="+endpoint+" from=pod") {
		t.Fatalf("the pod's probe: %s", body)
	}
	db := "projects/" + project + "/databases/(default)"
	deadline := time.Now().Add(20 * time.Second)
	for {
		u := fmt.Sprintf("http://%s/admin/events?service=firestore&kind=request&limit=1000&since=%s",
			h.Endpoint(EnvControl), since.UTC().Format(time.RFC3339Nano))
		_, raw := adminDo(t, http.MethodGet, u, "", nil)
		if strings.Contains(raw, `"target":"/google.firestore.v1.Firestore/Commit"`) &&
			strings.Contains(raw, `"target":"/google.firestore.v1.Firestore/BatchGetDocuments"`) &&
			strings.Contains(raw, db) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pod's Commit and BatchGetDocuments on %s are not in /admin/events: %.1500s", db, raw)
		}
		time.Sleep(250 * time.Millisecond)
	}
	measuredInConsole(t, h, "firestore", `cloudburrow_requests_total{service="firestore",method="BatchGetDocuments",code="OK"}`)
}
