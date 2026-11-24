package vertexai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

const mdParent = "projects/p/locations/us-central1"

func newMetadataServer(t *testing.T) (*Server, *metadataService, string) {
	t.Helper()
	s := NewServer(nil)
	file := filepath.Join(t.TempDir(), "nb.json")
	s.SetNotebookRuntimes(newFakeHost(), file)
	return s, &metadataService{s: s}, file
}

func wantCode(t *testing.T, err error, c codes.Code, what string) {
	t.Helper()
	if status.Code(err) != c {
		t.Fatalf("%s: got %v, want %v", what, err, c)
	}
}

func mdStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMetadataDefaultStoreAndStores(t *testing.T) {
	s, m, _ := newMetadataServer(t)
	ctx := context.Background()
	def := s.defaultMetadataStore(mdParent)
	if def != mdParent+"/metadataStores/default" {
		t.Fatalf("default store %q", def)
	}
	got, err := m.GetMetadataStore(ctx, &aiplatformpb.GetMetadataStoreRequest{Name: "projects/q/locations/l/metadataStores/default"})
	if err != nil || got.GetCreateTime() == nil {
		t.Fatalf("default store on first Get: %v %v", got, err)
	}
	_, err = m.GetMetadataStore(ctx, &aiplatformpb.GetMetadataStoreRequest{Name: mdParent + "/metadataStores/nope"})
	wantCode(t, err, codes.NotFound, "missing store")
	_, err = m.CreateMetadataStore(ctx, &aiplatformpb.CreateMetadataStoreRequest{Parent: mdParent, MetadataStoreId: "ab", MetadataStore: &aiplatformpb.MetadataStore{}})
	wantCode(t, err, codes.InvalidArgument, "short id")
	op, err := m.CreateMetadataStore(ctx, &aiplatformpb.CreateMetadataStoreRequest{Parent: mdParent, MetadataStoreId: "store-1",
		MetadataStore: &aiplatformpb.MetadataStore{Description: "d"}})
	if err != nil || !op.GetDone() || op.GetResponse() == nil {
		t.Fatalf("create store: %v %v", op, err)
	}
	_, err = m.CreateMetadataStore(ctx, &aiplatformpb.CreateMetadataStoreRequest{Parent: mdParent, MetadataStoreId: "store-1", MetadataStore: &aiplatformpb.MetadataStore{}})
	wantCode(t, err, codes.AlreadyExists, "duplicate store")
	l, err := m.ListMetadataStores(ctx, &aiplatformpb.ListMetadataStoresRequest{Parent: mdParent, PageSize: 1})
	if err != nil || len(l.GetMetadataStores()) != 1 || l.GetNextPageToken() == "" {
		t.Fatalf("list page 1: %v %v", l, err)
	}
	l2, err := m.ListMetadataStores(ctx, &aiplatformpb.ListMetadataStoresRequest{Parent: mdParent, PageSize: 1, PageToken: l.GetNextPageToken()})
	if err != nil || len(l2.GetMetadataStores()) != 1 || l2.GetNextPageToken() != "" {
		t.Fatalf("list page 2: %v %v", l2, err)
	}
	_, err = m.ListMetadataStores(ctx, &aiplatformpb.ListMetadataStoresRequest{Parent: mdParent, PageSize: 1001})
	wantCode(t, err, codes.InvalidArgument, "page_size 1001")
	if _, err := m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: mdParent + "/metadataStores/store-1", Artifact: &aiplatformpb.Artifact{}}); err != nil {
		t.Fatal(err)
	}
	op, err = m.DeleteMetadataStore(ctx, &aiplatformpb.DeleteMetadataStoreRequest{Name: mdParent + "/metadataStores/store-1"})
	if err != nil || !op.GetDone() {
		t.Fatalf("delete store: %v %v", op, err)
	}
	_, err = m.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: mdParent + "/metadataStores/store-1"})
	wantCode(t, err, codes.NotFound, "list in deleted store")
}

func TestMetadataArtifactsCRUD(t *testing.T) {
	_, m, _ := newMetadataServer(t)
	ctx := context.Background()
	store := mdParent + "/metadataStores/default"
	_, err := m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: "Bad_ID", Artifact: &aiplatformpb.Artifact{}})
	wantCode(t, err, codes.InvalidArgument, "bad id")
	_, err = m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: "a-one"})
	wantCode(t, err, codes.InvalidArgument, "missing artifact")
	a, err := m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: "a-one", Artifact: &aiplatformpb.Artifact{
		DisplayName: "model", Uri: "gs://b/model", SchemaTitle: "system.Model", State: aiplatformpb.Artifact_LIVE,
		Metadata: mdStruct(t, map[string]any{" accuracy ": 0.9})}})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetName() != store+"/artifacts/a-one" || a.GetEtag() == "" || a.GetCreateTime() == nil {
		t.Fatalf("created %v", a)
	}
	if _, ok := a.GetMetadata().GetFields()["accuracy"]; !ok {
		t.Fatalf("metadata key not trimmed: %v", a.GetMetadata())
	}
	_, err = m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: "a-one", Artifact: &aiplatformpb.Artifact{}})
	wantCode(t, err, codes.AlreadyExists, "duplicate")
	gen, err := m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, Artifact: &aiplatformpb.Artifact{}})
	if err != nil || len(gen.GetName()) <= len(store+"/artifacts/") {
		t.Fatalf("generated id: %v %v", gen, err)
	}
	_, err = m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, Artifact: &aiplatformpb.Artifact{DisplayName: strings.Repeat("x", 129)}})
	wantCode(t, err, codes.InvalidArgument, "long display name")
	_, err = m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, Artifact: &aiplatformpb.Artifact{
		Metadata: mdStruct(t, map[string]any{"big": strings.Repeat("x", mdMaxBytes)})}})
	wantCode(t, err, codes.InvalidArgument, "metadata over 200 KB")
	_, err = m.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: store + "/artifacts/nope"})
	wantCode(t, err, codes.NotFound, "get missing")

	// Update: etag, mask, allow_missing.
	_, err = m.UpdateArtifact(ctx, &aiplatformpb.UpdateArtifactRequest{Artifact: &aiplatformpb.Artifact{Name: a.GetName(), Etag: "stale"}})
	wantCode(t, err, codes.FailedPrecondition, "stale etag")
	u, err := m.UpdateArtifact(ctx, &aiplatformpb.UpdateArtifactRequest{Artifact: &aiplatformpb.Artifact{Name: a.GetName(), Etag: a.GetEtag(), DisplayName: "renamed", Uri: "gs://other"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"displayName"}}})
	if err != nil || u.GetDisplayName() != "renamed" || u.GetUri() != "gs://b/model" || u.GetEtag() == a.GetEtag() {
		t.Fatalf("masked update: %v %v", u, err)
	}
	_, err = m.UpdateArtifact(ctx, &aiplatformpb.UpdateArtifactRequest{Artifact: &aiplatformpb.Artifact{Name: a.GetName()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}})
	wantCode(t, err, codes.InvalidArgument, "mask on name")
	_, err = m.UpdateArtifact(ctx, &aiplatformpb.UpdateArtifactRequest{Artifact: &aiplatformpb.Artifact{Name: store + "/artifacts/new-one"}})
	wantCode(t, err, codes.NotFound, "update missing")
	n, err := m.UpdateArtifact(ctx, &aiplatformpb.UpdateArtifactRequest{Artifact: &aiplatformpb.Artifact{Name: store + "/artifacts/new-one", DisplayName: "n"}, AllowMissing: true})
	if err != nil || n.GetDisplayName() != "n" {
		t.Fatalf("allow_missing: %v %v", n, err)
	}

	// Delete: etag, then the Operation.
	_, err = m.DeleteArtifact(ctx, &aiplatformpb.DeleteArtifactRequest{Name: n.GetName(), Etag: "stale"})
	wantCode(t, err, codes.FailedPrecondition, "delete stale etag")
	op, err := m.DeleteArtifact(ctx, &aiplatformpb.DeleteArtifactRequest{Name: n.GetName(), Etag: n.GetEtag()})
	if err != nil || !op.GetDone() || !strings.HasPrefix(op.GetName(), n.GetName()+"/operations/") {
		t.Fatalf("delete: %v %v", op, err)
	}
	_, err = m.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: n.GetName()})
	wantCode(t, err, codes.NotFound, "get deleted")
}

func TestMetadataListFilters(t *testing.T) {
	_, m, _ := newMetadataServer(t)
	ctx := context.Background()
	store := mdParent + "/metadataStores/default"
	mk := func(id, title, uri string, md map[string]any) {
		t.Helper()
		if _, err := m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: id, Artifact: &aiplatformpb.Artifact{
			DisplayName: id, SchemaTitle: title, Uri: uri, State: aiplatformpb.Artifact_LIVE, Metadata: mdStruct(t, md)}}); err != nil {
			t.Fatal(err)
		}
	}
	mk("art-1", "system.Dataset", "gs://b/1", map[string]any{"rows": 10, "field:1": "x"})
	mk("art-2", "system.Model", "gs://b/2", map[string]any{"rows": 20, "ok": true})
	mk("art-3", "system.Model", "gs://b/3", map[string]any{"rows": 30})
	c, err := m.CreateContext(ctx, &aiplatformpb.CreateContextRequest{Parent: store, ContextId: "run-1", Context: &aiplatformpb.Context{SchemaTitle: "system.PipelineRun"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddContextArtifactsAndExecutions(ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: c.GetName(), Artifacts: []string{store + "/artifacts/art-3"}}); err != nil {
		t.Fatal(err)
	}
	ids := func(filter, order string) []string {
		t.Helper()
		l, err := m.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: store, Filter: filter, OrderBy: order})
		if err != nil {
			t.Fatalf("filter %q: %v", filter, err)
		}
		var out []string
		for _, a := range l.GetArtifacts() {
			out = append(out, lastSegment(a.GetName()))
		}
		return out
	}
	for _, tc := range []struct{ filter, order, want string }{
		{"", "", "[art-3 art-2 art-1]"},
		{"", "create_time", "[art-1 art-2 art-3]"},
		{`schema_title="system.Model"`, "display_name desc", "[art-3 art-2]"},
		{`uri="gs://b/2" AND schema_title="system.Model"`, "create_time desc", "[art-2]"},
		{`metadata.rows.number_value >= 20`, "display_name", "[art-2 art-3]"},
		{`metadata."field:1".string_value = "x"`, "", "[art-1]"},
		{`metadata.ok.bool_value = true OR display_name = "art-1"`, "display_name", "[art-1 art-2]"},
		{`in_context("` + c.GetName() + `")`, "", "[art-3]"},
		{`in_context("` + store + `/contexts/other") OR state = LIVE`, "display_name", "[art-1 art-2 art-3]"},
		{`create_time > "2000-01-01T00:00:00Z"`, "display_name", "[art-1 art-2 art-3]"},
	} {
		if got := fmt.Sprint(ids(tc.filter, tc.order)); got != tc.want {
			t.Errorf("filter %q order %q: got %s, want %s", tc.filter, tc.order, got, tc.want)
		}
	}
	for _, bad := range []string{`labels.x = "y"`, `metadata.rows = 1`, `((((((display_name="a"))))))`, `nope(1)`, `display_name = "unterminated`} {
		_, err := m.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: store, Filter: bad})
		if c := status.Code(err); c != codes.InvalidArgument && c != codes.Unimplemented {
			t.Errorf("filter %q: got %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	l, err := m.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: store, PageSize: 2})
	if err != nil || len(l.GetArtifacts()) != 2 || l.GetNextPageToken() == "" {
		t.Fatalf("page: %v %v", l, err)
	}
	_, err = m.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: store, PageSize: 2, PageToken: l.GetNextPageToken(), Filter: `state = LIVE`})
	wantCode(t, err, codes.InvalidArgument, "token with another filter")

	// Purge: force=false counts and samples, force=true deletes.
	op, err := m.PurgeArtifacts(ctx, &aiplatformpb.PurgeArtifactsRequest{Parent: store, Filter: `schema_title="system.Model"`})
	if err != nil {
		t.Fatal(err)
	}
	r := &aiplatformpb.PurgeArtifactsResponse{}
	if err := op.GetResponse().UnmarshalTo(r); err != nil || r.GetPurgeCount() != 2 || len(r.GetPurgeSample()) != 2 {
		t.Fatalf("dry purge: %v %v", r, err)
	}
	_, err = m.PurgeArtifacts(ctx, &aiplatformpb.PurgeArtifactsRequest{Parent: store})
	wantCode(t, err, codes.InvalidArgument, "purge without filter")
	op, _ = m.PurgeArtifacts(ctx, &aiplatformpb.PurgeArtifactsRequest{Parent: store, Filter: `schema_title="system.Model"`, Force: true})
	r = &aiplatformpb.PurgeArtifactsResponse{}
	if err := op.GetResponse().UnmarshalTo(r); err != nil || r.GetPurgeCount() != 2 || len(r.GetPurgeSample()) != 0 {
		t.Fatalf("purge: %v %v", r, err)
	}
	if got := fmt.Sprint(ids("", "")); got != "[art-1]" {
		t.Fatalf("after purge %s", got)
	}
}

func TestMetadataContextsAndLineage(t *testing.T) {
	_, m, file := newMetadataServer(t)
	ctx := context.Background()
	store := mdParent + "/metadataStores/default"
	mkCtx := func(id string) string {
		t.Helper()
		c, err := m.CreateContext(ctx, &aiplatformpb.CreateContextRequest{Parent: store, ContextId: id, Context: &aiplatformpb.Context{DisplayName: id}})
		if err != nil {
			t.Fatal(err)
		}
		return c.GetName()
	}
	exp, run, other := mkCtx("experiment"), mkCtx("run-one"), mkCtx("run-two")
	if _, err := m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: exp, ChildContexts: []string{run, other}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: exp, ChildContexts: []string{run}}); err != nil {
		t.Fatalf("re-adding a child is skipped: %v", err)
	}
	_, err := m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: run, ChildContexts: []string{exp}})
	wantCode(t, err, codes.InvalidArgument, "cycle")
	_, err = m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: run, ChildContexts: []string{run}})
	wantCode(t, err, codes.InvalidArgument, "self cycle")
	_, err = m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: exp, ChildContexts: []string{store + "/contexts/none"}})
	wantCode(t, err, codes.NotFound, "missing child")
	var parents []string
	for i := 0; i < mdMaxParents; i++ {
		parents = append(parents, mkCtx(fmt.Sprintf("parent-%d", i)))
	}
	leaf := mkCtx("leaf")
	for _, p := range parents {
		if _, err := m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: p, ChildContexts: []string{leaf}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = m.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: exp, ChildContexts: []string{leaf}})
	wantCode(t, err, codes.InvalidArgument, "11th parent")
	l, err := m.ListContexts(ctx, &aiplatformpb.ListContextsRequest{Parent: store, Filter: `parent_contexts:"` + exp + `"`, OrderBy: "display_name"})
	if err != nil || len(l.GetContexts()) != 2 || l.GetContexts()[0].GetName() != run {
		t.Fatalf("parent_contexts filter: %v %v", l, err)
	}
	l, err = m.ListContexts(ctx, &aiplatformpb.ListContextsRequest{Parent: store, Filter: `child_contexts:"` + run + `"`})
	if err != nil || len(l.GetContexts()) != 1 || l.GetContexts()[0].GetName() != exp {
		t.Fatalf("child_contexts filter: %v %v", l, err)
	}
	if _, err := m.RemoveContextChildren(ctx, &aiplatformpb.RemoveContextChildrenRequest{Context: exp, ChildContexts: []string{other, leaf}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := m.GetContext(ctx, &aiplatformpb.GetContextRequest{Name: other}); len(g.GetParentContexts()) != 0 {
		t.Fatalf("removed child keeps parent: %v", g)
	}

	// A pipeline run: dataset -> train -> model -> eval -> metrics.
	art := func(id string) string {
		t.Helper()
		a, err := m.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: id, Artifact: &aiplatformpb.Artifact{DisplayName: id, SchemaTitle: "system.Artifact"}})
		if err != nil {
			t.Fatal(err)
		}
		return a.GetName()
	}
	exe := func(id string) string {
		t.Helper()
		e, err := m.CreateExecution(ctx, &aiplatformpb.CreateExecutionRequest{Parent: store, ExecutionId: id, Execution: &aiplatformpb.Execution{DisplayName: id, State: aiplatformpb.Execution_RUNNING}})
		if err != nil {
			t.Fatal(err)
		}
		return e.GetName()
	}
	data, model, metrics := art("dataset"), art("model"), art("metrics")
	train, eval := exe("train"), exe("eval")
	ev := func(e string, in, out string) {
		t.Helper()
		if _, err := m.AddExecutionEvents(ctx, &aiplatformpb.AddExecutionEventsRequest{Execution: e, Events: []*aiplatformpb.Event{
			{Artifact: in, Type: aiplatformpb.Event_INPUT}, {Artifact: out, Type: aiplatformpb.Event_OUTPUT}}}); err != nil {
			t.Fatal(err)
		}
	}
	ev(train, data, model)
	ev(eval, model, metrics)
	ev(eval, model, metrics) // skipped: the events exist
	_, err = m.AddExecutionEvents(ctx, &aiplatformpb.AddExecutionEventsRequest{Execution: train, Events: []*aiplatformpb.Event{{Artifact: data}}})
	wantCode(t, err, codes.InvalidArgument, "event without type")
	_, err = m.AddExecutionEvents(ctx, &aiplatformpb.AddExecutionEventsRequest{Execution: train, Events: []*aiplatformpb.Event{{Artifact: store + "/artifacts/none", Type: aiplatformpb.Event_INPUT}}})
	wantCode(t, err, codes.NotFound, "event on a missing artifact")
	io, err := m.QueryExecutionInputsAndOutputs(ctx, &aiplatformpb.QueryExecutionInputsAndOutputsRequest{Execution: eval})
	if err != nil || len(io.GetArtifacts()) != 2 || len(io.GetEvents()) != 2 || len(io.GetExecutions()) != 1 || io.GetEvents()[0].GetExecution() != eval {
		t.Fatalf("inputs and outputs: %v %v", io, err)
	}
	if _, err := m.AddContextArtifactsAndExecutions(ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: run,
		Artifacts: []string{data, model}, Executions: []string{train}}); err != nil {
		t.Fatal(err)
	}
	_, err = m.AddContextArtifactsAndExecutions(ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: run, Artifacts: []string{"projects/p/locations/us-central1/metadataStores/xyzw/artifacts/abcd"}})
	wantCode(t, err, codes.InvalidArgument, "artifact of another store")
	g, err := m.QueryContextLineageSubgraph(ctx, &aiplatformpb.QueryContextLineageSubgraphRequest{Context: run})
	if err != nil || len(g.GetArtifacts()) != 2 || len(g.GetExecutions()) != 1 || len(g.GetEvents()) != 2 {
		t.Fatalf("context lineage: %v %v", g, err)
	}
	el, err := m.ListExecutions(ctx, &aiplatformpb.ListExecutionsRequest{Parent: store, Filter: `in_context("` + run + `") AND state = RUNNING`})
	if err != nil || len(el.GetExecutions()) != 1 || el.GetExecutions()[0].GetName() != train {
		t.Fatalf("executions in_context: %v %v", el, err)
	}
	count := func(maxHops int32, filter string) (int, int, int) {
		t.Helper()
		g, err := m.QueryArtifactLineageSubgraph(ctx, &aiplatformpb.QueryArtifactLineageSubgraphRequest{Artifact: data, MaxHops: maxHops, Filter: filter})
		if err != nil {
			t.Fatal(err)
		}
		return len(g.GetArtifacts()), len(g.GetExecutions()), len(g.GetEvents())
	}
	if a, e, v := count(0, ""); a != 3 || e != 2 || v != 4 {
		t.Errorf("closure: %d %d %d", a, e, v)
	}
	if a, e, v := count(2, ""); a != 2 || e != 1 || v != 2 {
		t.Errorf("2 hops: %d %d %d", a, e, v)
	}
	if a, e, _ := count(0, `display_name != "model"`); a != 1 || e != 1 {
		t.Errorf("filtered: %d %d", a, e)
	}
	_, err = m.QueryArtifactLineageSubgraph(ctx, &aiplatformpb.QueryArtifactLineageSubgraphRequest{Artifact: data, MaxHops: -1})
	wantCode(t, err, codes.InvalidArgument, "negative max_hops")

	// Updates of executions, and the store's file.
	u, err := m.UpdateExecution(ctx, &aiplatformpb.UpdateExecutionRequest{Execution: &aiplatformpb.Execution{Name: train, State: aiplatformpb.Execution_COMPLETE},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	if err != nil || u.GetState() != aiplatformpb.Execution_COMPLETE || u.GetDisplayName() != "train" {
		t.Fatalf("update execution: %v %v", u, err)
	}
	s2 := NewServer(nil)
	s2.SetNotebookRuntimes(newFakeHost(), file)
	m2 := &metadataService{s: s2}
	g2, err := m2.QueryContextLineageSubgraph(ctx, &aiplatformpb.QueryContextLineageSubgraphRequest{Context: run})
	if err != nil || len(g2.GetArtifacts()) != 2 || len(g2.GetEvents()) != 2 {
		t.Fatalf("after reload: %v %v", g2, err)
	}
	if c, _ := m2.GetContext(ctx, &aiplatformpb.GetContextRequest{Name: run}); len(c.GetParentContexts()) != 1 {
		t.Fatalf("parents after reload: %v", c)
	}

	// Deleting an execution drops its events and associations.
	if _, err := m.DeleteExecution(ctx, &aiplatformpb.DeleteExecutionRequest{Name: train}); err != nil {
		t.Fatal(err)
	}
	g, _ = m.QueryContextLineageSubgraph(ctx, &aiplatformpb.QueryContextLineageSubgraphRequest{Context: run})
	if len(g.GetExecutions()) != 0 || len(g.GetEvents()) != 0 {
		t.Fatalf("after delete: %v", g)
	}
	op, err := m.PurgeContexts(ctx, &aiplatformpb.PurgeContextsRequest{Parent: store, Filter: `display_name = "parent-*"`, Force: true})
	pr := &aiplatformpb.PurgeContextsResponse{}
	if err != nil || op.GetResponse().UnmarshalTo(pr) != nil || pr.GetPurgeCount() != mdMaxParents {
		t.Fatalf("purge contexts: %v %v", pr, err)
	}
	op, err = m.PurgeExecutions(ctx, &aiplatformpb.PurgeExecutionsRequest{Parent: store, Filter: `display_name = "eval"`})
	px := &aiplatformpb.PurgeExecutionsResponse{}
	if err != nil || op.GetResponse().UnmarshalTo(px) != nil || px.GetPurgeCount() != 1 || len(px.GetPurgeSample()) != 1 {
		t.Fatalf("purge executions: %v %v", px, err)
	}
	if _, err := m.DeleteContext(ctx, &aiplatformpb.DeleteContextRequest{Name: exp}); err != nil {
		t.Fatal(err)
	}
	if c, _ := m.GetContext(ctx, &aiplatformpb.GetContextRequest{Name: run}); len(c.GetParentContexts()) != 0 {
		t.Fatalf("deleted parent kept: %v", c)
	}
}

func TestMetadataSchemas(t *testing.T) {
	_, m, _ := newMetadataServer(t)
	ctx := context.Background()
	store := mdParent + "/metadataStores/default"
	schema := "title: test.Thing\ntype: object\nproperties:\n  n:\n    type: number\n"
	sc, err := m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store, MetadataSchemaId: "thing-v1",
		MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "1.0.0", Schema: schema, SchemaType: aiplatformpb.MetadataSchema_ARTIFACT_TYPE}})
	if err != nil || sc.GetName() != store+"/metadataSchemas/thing-v1" || sc.GetCreateTime() == nil {
		t.Fatalf("create: %v %v", sc, err)
	}
	_, err = m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store, MetadataSchemaId: "thing-v1b",
		MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "1.0.0", Schema: schema}})
	wantCode(t, err, codes.AlreadyExists, "same title and version")
	_, err = m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store, MetadataSchemaId: "thing-v1",
		MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "2.0.0", Schema: schema}})
	wantCode(t, err, codes.AlreadyExists, "same id")
	_, err = m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store, MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "v1", Schema: schema}})
	wantCode(t, err, codes.InvalidArgument, "bad version")
	_, err = m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store, MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "1.0.0"}})
	wantCode(t, err, codes.InvalidArgument, "no schema")
	_, err = m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store, MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "1.0.0", Schema: "title: x\n" + strings.Repeat("#", mdMaxBytes)}})
	wantCode(t, err, codes.InvalidArgument, "schema over the limit")
	if _, err := m.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: store,
		MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "2.0.0", Schema: schema, SchemaType: aiplatformpb.MetadataSchema_EXECUTION_TYPE}}); err != nil {
		t.Fatal(err)
	}
	g, err := m.GetMetadataSchema(ctx, &aiplatformpb.GetMetadataSchemaRequest{Name: sc.GetName()})
	if err != nil || g.GetSchema() != schema {
		t.Fatalf("get: %v %v", g, err)
	}
	_, err = m.GetMetadataSchema(ctx, &aiplatformpb.GetMetadataSchemaRequest{Name: store + "/metadataSchemas/none"})
	wantCode(t, err, codes.NotFound, "missing schema")
	l, err := m.ListMetadataSchemas(ctx, &aiplatformpb.ListMetadataSchemasRequest{Parent: store, Filter: `schema_type = EXECUTION_TYPE`})
	if err != nil || len(l.GetMetadataSchemas()) != 1 {
		t.Fatalf("list: %v %v", l, err)
	}
}

func TestMetadataREST(t *testing.T) {
	s, _, _ := newMetadataServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	base := srv.URL + "/v1/" + mdParent + "/metadataStores/default"
	do := func(method, url, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	if c, out := do("GET", base, ""); c != 200 || out["name"] != mdParent+"/metadataStores/default" {
		t.Fatalf("get store: %d %v", c, out)
	}
	if c, out := do("POST", base+"/artifacts?artifactId=rest-art", `{"displayName":"d","uri":"gs://b/x","metadata":{"k":1}}`); c != 200 || out["uri"] != "gs://b/x" {
		t.Fatalf("create artifact: %d %v", c, out)
	}
	if c, out := do("GET", base+`/artifacts?filter=metadata.k.number_value%3D1`, ""); c != 200 || len(out["artifacts"].([]any)) != 1 {
		t.Fatalf("list artifacts: %d %v", c, out)
	}
	if c, out := do("POST", base+"/executions?executionId=rest-exe", `{"state":"RUNNING"}`); c != 200 {
		t.Fatalf("create execution: %d %v", c, out)
	}
	if c, out := do("POST", base+"/executions/rest-exe:addExecutionEvents", `{"events":[{"artifact":"`+mdParent+`/metadataStores/default/artifacts/rest-art","type":"OUTPUT"}]}`); c != 200 {
		t.Fatalf("add events: %d %v", c, out)
	}
	if c, out := do("GET", base+"/executions/rest-exe:queryExecutionInputsAndOutputs", ""); c != 200 || len(out["events"].([]any)) != 1 {
		t.Fatalf("query io: %d %v", c, out)
	}
	c, out := do("DELETE", base+"/artifacts/rest-art", "")
	if c != 200 || out["done"] != true {
		t.Fatalf("delete artifact: %d %v", c, out)
	}
	if c, op := do("GET", srv.URL+"/v1/"+out["name"].(string), ""); c != 200 || op["done"] != true {
		t.Fatalf("get delete operation: %d %v", c, op)
	}
	if c, _ := do("GET", base+"/artifacts/rest-art", ""); c != 404 {
		t.Fatalf("get deleted artifact: %d", c)
	}
}
