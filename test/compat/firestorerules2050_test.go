//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	rules "google.golang.org/api/firebaserules/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// serverFirestore is the official server client library on project, which
// rules never bind.
func serverFirestore(t *testing.T, h *Harness, project string) *firestore.Client {
	t.Helper()
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	fc, err := firestore.NewClient(h.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	return fc
}

// covers: firebaserules.projects.rulesets.create, firebaserules.projects.releases.create, firebaserules.projects.releases.delete
//
// TestFirebaseRulesNoReleaseDeniesClients (#2050): a project with no
// cloud.firestore release denies every client SDK call, as on Google,
// both before its first release and after its last one is deleted, while
// the server client library's calls still pass.
func TestFirebaseRulesNoReleaseDeniesClients(t *testing.T) {
	h := New(t)
	c := firebaseRulesClient(t, h) // skips without CLOUDBURROW_TEST_FIREBASERULES
	fsAddr := h.Endpoint(EnvFirestore)
	ctx := h.Context()
	project := fmt.Sprintf("norel-%d", time.Now().UnixNano()%1e8)
	parent := "projects/" + project
	admin := serverFirestore(t, h, project)
	if _, err := admin.Doc("users/alice").Set(ctx, map[string]any{"name": "Alice"}); err != nil {
		t.Fatalf("the server library's write: %v", err)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", "alice"); code != http.StatusForbidden {
		t.Errorf("before any release, a signed-in client read: %d %s; want 403", code, body)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", ""); code != http.StatusForbidden {
		t.Errorf("before any release, an unauthenticated client read: %d %s; want 403", code, body)
	}

	open := "rules_version = '2';\nservice cloud.firestore {\n  match /databases/{database}/documents {\n    match /{document=**} {\n      allow read: if true;\n    }\n  }\n}\n"
	rs, err := c.Projects.Rulesets.Create(parent, &rules.Ruleset{Source: &rules.Source{Files: []*rules.File{{Name: "firestore.rules", Content: open}}}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("rulesets.create: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Rulesets.Delete(rs.Name).Do() })
	relName := parent + "/releases/cloud.firestore"
	if _, err := c.Projects.Releases.Create(parent, &rules.Release{Name: relName, RulesetName: rs.Name}).Context(ctx).Do(); err != nil {
		t.Fatalf("releases.create: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Releases.Delete(relName).Do() })
	if code, body := clientGet(t, fsAddr, project, "users/alice", ""); code != http.StatusOK {
		t.Errorf("with the open release, a client read: %d %s; want 200", code, body)
	}

	if _, err := c.Projects.Releases.Delete(relName).Context(ctx).Do(); err != nil {
		t.Fatalf("releases.delete: %v", err)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", ""); code != http.StatusForbidden || !strings.Contains(body, "PERMISSION_DENIED") {
		t.Errorf("after the last release is deleted, a client read: %d %s; want 403 PERMISSION_DENIED", code, body)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", "alice"); code != http.StatusForbidden {
		t.Errorf("after the last release is deleted, a signed-in client read: %d %s; want 403", code, body)
	}
	if snap, err := admin.Doc("users/alice").Get(ctx); err != nil || snap.Data()["name"] != "Alice" {
		t.Errorf("after the release is deleted, the server library's read: %v", err)
	}
	if _, err := admin.Doc("users/bob").Set(ctx, map[string]any{"name": "Bob"}); err != nil {
		t.Errorf("after the release is deleted, the server library's write: %v", err)
	}
}

// mockRules read a post only as a user whose users/{uid} document says
// admin, and write one only as a user in allow/.
const mockRules = `rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /posts/{id} {
      // get() and exists() are cloud.firestore's service functions.
      allow read: if get(/databases/$(database)/documents/users/$(request.auth.uid)).data.role == 'admin';
      allow create: if exists(/databases/$(database)/documents/allow/$(request.auth.uid));
    }
  }
}
`

// covers: firebaserules.projects.test
//
// TestFirebaseRulesTestFunctionMocks (#2050): projects.test answers
// service functions by testCases[].functionMocks, as the Rules API
// documents: an exactValue argument matches its path, anyValue any, a
// result's value is returned and undefined fails; an unmocked call, or one
// no mock matches, is an error. A mock of a function the service does not
// declare is INVALID_ARGUMENT.
func TestFirebaseRulesTestFunctionMocks(t *testing.T) {
	h := New(t)
	c := firebaseRulesClient(t, h) // skips without CLOUDBURROW_TEST_FIREBASERULES
	ctx := h.Context()
	parent := fmt.Sprintf("projects/mocks-%d", time.Now().UnixNano()%1e8)
	src := &rules.Source{Files: []*rules.File{{Name: "firestore.rules", Content: mockRules}}}
	post := "/databases/(default)/documents/posts/p1"
	alice := map[string]any{"uid": "alice"}
	bob := map[string]any{"uid": "bob"}
	getUser := func(arg *rules.Arg, role string) *rules.FunctionMock {
		return &rules.FunctionMock{Function: "get", Args: []*rules.Arg{arg},
			Result: &rules.Result{Value: map[string]any{"data": map[string]any{"role": role}}}}
	}
	aliceDoc := &rules.Arg{ExactValue: "/databases/(default)/documents/users/alice"}
	anyArg := &rules.Arg{AnyValue: &rules.Empty{}}
	cases := []*rules.TestCase{
		// 0: alice's users document, mocked, says admin.
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": post, "auth": alice},
			FunctionMocks: []*rules.FunctionMock{getUser(aliceDoc, "admin")}},
		// 1: bob's path matches no mock: an error, so denied.
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": post, "auth": bob},
			FunctionMocks: []*rules.FunctionMock{getUser(aliceDoc, "admin")}},
		// 2: anyValue answers bob's, after the exact one: reader.
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": post, "auth": bob},
			FunctionMocks: []*rules.FunctionMock{getUser(aliceDoc, "admin"), getUser(anyArg, "reader")}},
		// 3: and admin for anyone.
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": post, "auth": bob},
			FunctionMocks: []*rules.FunctionMock{getUser(anyArg, "admin")}},
		// 4: no mocks: get() is an error, so denied.
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": post, "auth": alice}},
		// 5: exists() mocked true.
		{Expectation: "ALLOW", Request: map[string]any{"method": "create", "path": post, "auth": alice,
			"resource": map[string]any{"data": map[string]any{"title": "t"}}},
			FunctionMocks: []*rules.FunctionMock{{Function: "exists", Args: []*rules.Arg{anyArg}, Result: &rules.Result{Value: true}}}},
		// 6: exists() undefined: denied.
		{Expectation: "DENY", Request: map[string]any{"method": "create", "path": post, "auth": alice,
			"resource": map[string]any{"data": map[string]any{"title": "t"}}},
			FunctionMocks: []*rules.FunctionMock{{Function: "exists", Args: []*rules.Arg{anyArg}, Result: &rules.Result{Undefined: &rules.Empty{}}}}},
		// 7: a wrong expectation still fails: the mock allows it.
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": post, "auth": alice},
			FunctionMocks: []*rules.FunctionMock{getUser(aliceDoc, "admin")}},
	}
	res, err := c.Projects.Test(parent, &rules.TestRulesetRequest{Source: src, TestSuite: &rules.TestSuite{TestCases: cases}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("projects.test: %v", err)
	}
	var states []string
	for _, r := range res.TestResults {
		states = append(states, r.State)
	}
	if want := "SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,SUCCESS,FAILURE"; strings.Join(states, ",") != want {
		for i, r := range res.TestResults {
			t.Logf("case %d: %s %v", i, r.State, r.DebugMessages)
		}
		t.Errorf("projects.test results: %v; want %s", states, want)
	}

	var ge *googleapi.Error
	_, err = c.Projects.Test(parent, &rules.TestRulesetRequest{Source: src, TestSuite: &rules.TestSuite{TestCases: []*rules.TestCase{
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": post},
			FunctionMocks: []*rules.FunctionMock{{Function: "isAdmin", Args: []*rules.Arg{anyArg}, Result: &rules.Result{Value: true}}}}}}}).Context(ctx).Do()
	if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Errorf("a mock of a function cloud.firestore does not declare: %v; want 400 INVALID_ARGUMENT", err)
	}
}

// TestFirestoreStreamMetrics (#2050): the Listen and Write streams are in
// Firestore's document metrics, as Google bills them: a listener reads
// each document it delivers (LOOKUP for a document, QUERY for a query,
// one read for a query with no results), and each write of a write stream
// is a CREATE, an UPDATE or a delete.
//
// covers: google.monitoring.v3.MetricService/ListTimeSeries
func TestFirestoreStreamMetrics(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvFirestore)
	mc := monitoringClient(t, h)
	ctx := h.Context()
	// A project of its own, so the counts are exactly this test's.
	project := fmt.Sprintf("streams-%d", time.Now().UnixNano()%1e8)
	fc := serverFirestore(t, h, project)
	col := fc.Collection("posts")
	for _, id := range []string{"a", "b"} {
		if _, err := col.Doc(id).Set(ctx, map[string]any{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}

	// A query listener: two documents, then a third.
	qit := col.Snapshots(ctx)
	snap, err := qit.Next()
	if err != nil || snap.Size != 2 {
		t.Fatalf("the query listener's first snapshot: %v %v; want 2 documents", snap, err)
	}
	if _, err := col.Doc("c").Set(ctx, map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if snap, err = qit.Next(); err != nil || snap.Size != 3 {
		t.Fatalf("the query listener's second snapshot: %v %v; want 3 documents", snap, err)
	}
	qit.Stop()
	// A document listener, and a listener on an empty collection.
	dit := col.Doc("a").Snapshots(ctx)
	if ds, err := dit.Next(); err != nil || !ds.Exists() {
		t.Fatalf("the document listener: %v %v", ds, err)
	}
	dit.Stop()
	eit := fc.Collection("empty").Snapshots(ctx)
	if es, err := eit.Next(); err != nil || es.Size != 0 {
		t.Fatalf("the empty listener: %v %v", es, err)
	}
	eit.Stop()

	// A write stream (the official GAPIC client's Write): one create, one
	// update and one delete.
	gc := firestoreGAPIC(t, h)
	db := "projects/" + project + "/databases/(default)"
	sctx := metadata.AppendToOutgoingContext(ctx, "google-cloud-resource-prefix", db)
	ws, err := gc.Write(sctx)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := ws.Send(&firestorepb.WriteRequest{Database: db}); err != nil {
		t.Fatal(err)
	}
	hs, err := ws.Recv()
	if err != nil || len(hs.GetStreamToken()) == 0 {
		t.Fatalf("the write stream's handshake: %v %v", hs, err)
	}
	name := func(id string) string { return db + "/documents/posts/" + id }
	if err := ws.Send(&firestorepb.WriteRequest{StreamToken: hs.GetStreamToken(), Writes: []*firestorepb.Write{
		{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: name("d"), Fields: map[string]*firestorepb.Value{"n": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 1}}}}}},
		{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: name("a"), Fields: map[string]*firestorepb.Value{"n": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 2}}}}}},
		{Operation: &firestorepb.Write_Delete{Delete: name("b")}},
	}}); err != nil {
		t.Fatal(err)
	}
	wr, err := ws.Recv()
	if err != nil || len(wr.GetWriteResults()) != 3 {
		t.Fatalf("the write stream's answer: %v %v; want 3 write results", wr, err)
	}
	_ = ws.CloseSend()
	if snap, err := col.Doc("d").Get(ctx); err != nil || !snap.Exists() {
		t.Fatalf("the streamed write did not apply: %v", err)
	}

	end := time.Now().Add(2 * time.Minute)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-30 * time.Minute)), EndTime: timestamppb.New(end)}
	sum := func(metric, label, value string) int64 {
		f := `metric.type = "firestore.googleapis.com/document/` + metric + `" AND resource.type = "firestore.googleapis.com/Database" AND resource.labels.database_id = "(default)"`
		if label != "" {
			f += ` AND metric.labels.` + label + ` = "` + value + `"`
		}
		it := mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: f, Interval: iv})
		var n int64
		for {
			ts, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListTimeSeries(%s): %v", metric, err)
			}
			for _, p := range ts.GetPoints() {
				n += p.GetValue().GetInt64Value()
			}
		}
		return n
	}
	// Sets of a, b and c (3) and the stream's d create; the get of d is a
	// LOOKUP.
	for _, w := range []struct {
		metric, label, value string
		want                 int64
	}{
		{"write_ops_count", "op", "CREATE", 4},
		{"write_ops_count", "op", "UPDATE", 1},
		{"delete_ops_count", "", "", 1},
		// 2 + 1 documents the query listener delivered, and the empty one.
		{"read_ops_count", "type", "QUERY", 4},
		// The document listener's, and the get of d.
		{"read_ops_count", "type", "LOOKUP", 2},
	} {
		if n := sum(w.metric, w.label, w.value); n != w.want {
			t.Errorf("%s %s = %d, want %d", w.metric, w.value, n, w.want)
		}
	}
}
