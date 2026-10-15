//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/firestore"
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
// the host pass through the tunnel's guard, which now records each one
// (#1267): a write and a read of a missing document with the official
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
