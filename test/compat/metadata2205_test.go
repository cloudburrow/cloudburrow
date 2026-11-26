//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

func wantStatus2205(t *testing.T, err error, c codes.Code, what string) {
	t.Helper()
	if status.Code(err) != c {
		t.Fatalf("%s: got %v, want %v", what, err, c)
	}
}

// covers: google.cloud.aiplatform.v1.MetadataService/CreateMetadataStore, google.cloud.aiplatform.v1.MetadataService/GetMetadataStore, google.cloud.aiplatform.v1.MetadataService/ListMetadataStores, google.cloud.aiplatform.v1.MetadataService/DeleteMetadataStore, google.cloud.aiplatform.v1.MetadataService/CreateArtifact, google.cloud.aiplatform.v1.MetadataService/GetArtifact, google.cloud.aiplatform.v1.MetadataService/ListArtifacts, google.cloud.aiplatform.v1.MetadataService/UpdateArtifact, google.cloud.aiplatform.v1.MetadataService/DeleteArtifact, google.cloud.aiplatform.v1.MetadataService/PurgeArtifacts, google.cloud.aiplatform.v1.MetadataService/CreateContext, google.cloud.aiplatform.v1.MetadataService/GetContext, google.cloud.aiplatform.v1.MetadataService/ListContexts, google.cloud.aiplatform.v1.MetadataService/UpdateContext, google.cloud.aiplatform.v1.MetadataService/DeleteContext, google.cloud.aiplatform.v1.MetadataService/PurgeContexts, google.cloud.aiplatform.v1.MetadataService/AddContextArtifactsAndExecutions, google.cloud.aiplatform.v1.MetadataService/AddContextChildren, google.cloud.aiplatform.v1.MetadataService/RemoveContextChildren, google.cloud.aiplatform.v1.MetadataService/QueryContextLineageSubgraph, google.cloud.aiplatform.v1.MetadataService/CreateExecution, google.cloud.aiplatform.v1.MetadataService/GetExecution, google.cloud.aiplatform.v1.MetadataService/ListExecutions, google.cloud.aiplatform.v1.MetadataService/UpdateExecution, google.cloud.aiplatform.v1.MetadataService/DeleteExecution, google.cloud.aiplatform.v1.MetadataService/PurgeExecutions, google.cloud.aiplatform.v1.MetadataService/AddExecutionEvents, google.cloud.aiplatform.v1.MetadataService/QueryExecutionInputsAndOutputs, google.cloud.aiplatform.v1.MetadataService/CreateMetadataSchema, google.cloud.aiplatform.v1.MetadataService/GetMetadataSchema, google.cloud.aiplatform.v1.MetadataService/ListMetadataSchemas, google.cloud.aiplatform.v1.MetadataService/QueryArtifactLineageSubgraph
//
// TestVertexAIMLMetadata (#2205): the official aiplatform MetadataClient
// (gRPC) records a pipeline run as Vertex AI Pipelines does: a store, a
// run context under an experiment context, executions, artifacts and the
// events joining them; the lists' documented filters (schema_title, uri,
// metadata.<key>.<type>_value, in_context, parent_contexts), order_by and
// pages; etags on Update and Delete; Purge's dry run and force; the
// lineage queries; metadata schemas; and the documented errors
// (ALREADY_EXISTS, NOT_FOUND, an ID outside 4-128 [a-z0-9-], a cycle of
// contexts, a negative max_hops, a purge without a filter).
func TestVertexAIMLMetadata(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	if endpoint == "" {
		t.Skip("no Vertex AI endpoint")
	}
	c, err := aiplatform.NewMetadataClient(h.Context(), option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("aiplatform.NewMetadataClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	// The default store answers on first use.
	def, err := c.GetMetadataStore(ctx, &aiplatformpb.GetMetadataStoreRequest{Name: parent + "/metadataStores/default"})
	if err != nil || def.GetName() != parent+"/metadataStores/default" {
		t.Fatalf("default store: %v %v", def, err)
	}

	// A store of this test's own.
	_, err = c.CreateMetadataStore(ctx, &aiplatformpb.CreateMetadataStoreRequest{Parent: parent, MetadataStoreId: "md", MetadataStore: &aiplatformpb.MetadataStore{}})
	wantStatus2205(t, err, codes.InvalidArgument, "a 2-character store ID")
	sop, err := c.CreateMetadataStore(ctx, &aiplatformpb.CreateMetadataStoreRequest{Parent: parent, MetadataStoreId: "md-" + suffix,
		MetadataStore: &aiplatformpb.MetadataStore{Description: "compat #2205"}})
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	store, err := sop.Wait(ctx)
	if err != nil || store.GetDescription() != "compat #2205" {
		t.Fatalf("CreateMetadataStore result: %v %v", store, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteMetadataStore(context.Background(), &aiplatformpb.DeleteMetadataStoreRequest{Name: store.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	_, err = c.CreateMetadataStore(ctx, &aiplatformpb.CreateMetadataStoreRequest{Parent: parent, MetadataStoreId: "md-" + suffix, MetadataStore: &aiplatformpb.MetadataStore{}})
	wantStatus2205(t, err, codes.AlreadyExists, "a taken store ID")
	found := false
	for it := c.ListMetadataStores(ctx, &aiplatformpb.ListMetadataStoresRequest{Parent: parent, PageSize: 1}); ; {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMetadataStores: %v", err)
		}
		found = found || s.GetName() == store.GetName()
	}
	if !found {
		t.Fatalf("ListMetadataStores lacks %s", store.GetName())
	}
	sn := store.GetName()

	// Contexts: an experiment and its run.
	mkCtx := func(id, title string) *aiplatformpb.Context {
		t.Helper()
		x, err := c.CreateContext(ctx, &aiplatformpb.CreateContextRequest{Parent: sn, ContextId: id, Context: &aiplatformpb.Context{DisplayName: id, SchemaTitle: title}})
		if err != nil {
			t.Fatalf("CreateContext %s: %v", id, err)
		}
		return x
	}
	exp, run := mkCtx("experiment", "system.Experiment"), mkCtx("run-1", "system.PipelineRun")
	_, err = c.CreateContext(ctx, &aiplatformpb.CreateContextRequest{Parent: sn, ContextId: "run-1", Context: &aiplatformpb.Context{}})
	wantStatus2205(t, err, codes.AlreadyExists, "a taken context ID")
	if _, err := c.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: exp.GetName(), ChildContexts: []string{run.GetName()}}); err != nil {
		t.Fatalf("AddContextChildren: %v", err)
	}
	_, err = c.AddContextChildren(ctx, &aiplatformpb.AddContextChildrenRequest{Context: run.GetName(), ChildContexts: []string{exp.GetName()}})
	wantStatus2205(t, err, codes.InvalidArgument, "a cycle of contexts")
	if g, err := c.GetContext(ctx, &aiplatformpb.GetContextRequest{Name: run.GetName()}); err != nil || len(g.GetParentContexts()) != 1 {
		t.Fatalf("GetContext parents: %v %v", g, err)
	}
	var children []string
	for it := c.ListContexts(ctx, &aiplatformpb.ListContextsRequest{Parent: sn, Filter: `parent_contexts:"` + exp.GetName() + `"`}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListContexts: %v", err)
		}
		children = append(children, x.GetName())
	}
	if len(children) != 1 || children[0] != run.GetName() {
		t.Fatalf("parent_contexts filter: %v", children)
	}
	uc, err := c.UpdateContext(ctx, &aiplatformpb.UpdateContextRequest{Context: &aiplatformpb.Context{Name: run.GetName(), Etag: run.GetEtag(), Description: "first run"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || uc.GetDescription() != "first run" || uc.GetDisplayName() != "run-1" {
		t.Fatalf("UpdateContext: %v %v", uc, err)
	}

	// The run's executions and artifacts.
	md, _ := structpb.NewStruct(map[string]any{"rows": 100})
	data, err := c.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: sn, ArtifactId: "dataset", Artifact: &aiplatformpb.Artifact{
		DisplayName: "dataset", SchemaTitle: "system.Dataset", Uri: "gs://bucket/data", State: aiplatformpb.Artifact_LIVE, Metadata: md}})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	model, err := c.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: sn, ArtifactId: "model", Artifact: &aiplatformpb.Artifact{
		DisplayName: "model", SchemaTitle: "system.Model", Uri: "gs://bucket/model", State: aiplatformpb.Artifact_PENDING}})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, err = c.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: sn, ArtifactId: "UPPER", Artifact: &aiplatformpb.Artifact{}})
	wantStatus2205(t, err, codes.InvalidArgument, "an uppercase artifact ID")
	_, err = c.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: sn + "/artifacts/none"})
	wantStatus2205(t, err, codes.NotFound, "a missing artifact")
	train, err := c.CreateExecution(ctx, &aiplatformpb.CreateExecutionRequest{Parent: sn, ExecutionId: "train", Execution: &aiplatformpb.Execution{
		DisplayName: "train", SchemaTitle: "system.ContainerExecution", State: aiplatformpb.Execution_RUNNING}})
	if err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}
	if _, err := c.AddExecutionEvents(ctx, &aiplatformpb.AddExecutionEventsRequest{Execution: train.GetName(), Events: []*aiplatformpb.Event{
		{Artifact: data.GetName(), Type: aiplatformpb.Event_INPUT}, {Artifact: model.GetName(), Type: aiplatformpb.Event_OUTPUT}}}); err != nil {
		t.Fatalf("AddExecutionEvents: %v", err)
	}
	if _, err := c.AddContextArtifactsAndExecutions(ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: run.GetName(),
		Artifacts: []string{data.GetName(), model.GetName()}, Executions: []string{train.GetName()}}); err != nil {
		t.Fatalf("AddContextArtifactsAndExecutions: %v", err)
	}
	_, err = c.UpdateExecution(ctx, &aiplatformpb.UpdateExecutionRequest{Execution: &aiplatformpb.Execution{Name: train.GetName(), Etag: "stale", State: aiplatformpb.Execution_COMPLETE}})
	wantStatus2205(t, err, codes.FailedPrecondition, "a stale execution etag")
	ue, err := c.UpdateExecution(ctx, &aiplatformpb.UpdateExecutionRequest{Execution: &aiplatformpb.Execution{Name: train.GetName(), Etag: train.GetEtag(), State: aiplatformpb.Execution_COMPLETE},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	if err != nil || ue.GetState() != aiplatformpb.Execution_COMPLETE {
		t.Fatalf("UpdateExecution: %v %v", ue, err)
	}
	ua, err := c.UpdateArtifact(ctx, &aiplatformpb.UpdateArtifactRequest{Artifact: &aiplatformpb.Artifact{Name: model.GetName(), State: aiplatformpb.Artifact_LIVE},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	if err != nil || ua.GetState() != aiplatformpb.Artifact_LIVE || ua.GetUri() != "gs://bucket/model" {
		t.Fatalf("UpdateArtifact: %v %v", ua, err)
	}
	if g, err := c.GetExecution(ctx, &aiplatformpb.GetExecutionRequest{Name: train.GetName()}); err != nil || g.GetState() != aiplatformpb.Execution_COMPLETE {
		t.Fatalf("GetExecution: %v %v", g, err)
	}

	// The documented list filters.
	arts := func(filter, order string) []string {
		t.Helper()
		var out []string
		for it := c.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: sn, Filter: filter, OrderBy: order, PageSize: 1}); ; {
			a, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListArtifacts %q: %v", filter, err)
			}
			out = append(out, a.GetDisplayName())
		}
	}
	for _, tc := range []struct{ filter, order, want string }{
		{`uri="gs://bucket/model" AND schema_title="system.Model"`, "create_time desc", "[model]"},
		{`metadata.rows.number_value = 100`, "", "[dataset]"},
		{`in_context("` + run.GetName() + `")`, "display_name desc", "[model dataset]"},
		{`state = LIVE`, "display_name", "[dataset model]"},
	} {
		if got := fmt.Sprint(arts(tc.filter, tc.order)); got != tc.want {
			t.Errorf("ListArtifacts filter %q: got %s, want %s", tc.filter, got, tc.want)
		}
	}
	var execs []string
	for it := c.ListExecutions(ctx, &aiplatformpb.ListExecutionsRequest{Parent: sn, Filter: `in_context("` + run.GetName() + `") AND state = COMPLETE`}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListExecutions: %v", err)
		}
		execs = append(execs, e.GetDisplayName())
	}
	if fmt.Sprint(execs) != "[train]" {
		t.Fatalf("ListExecutions: %v", execs)
	}

	// Lineage.
	io, err := c.QueryExecutionInputsAndOutputs(ctx, &aiplatformpb.QueryExecutionInputsAndOutputsRequest{Execution: train.GetName()})
	if err != nil || len(io.GetArtifacts()) != 2 || len(io.GetEvents()) != 2 {
		t.Fatalf("QueryExecutionInputsAndOutputs: %v %v", io, err)
	}
	g, err := c.QueryContextLineageSubgraph(ctx, &aiplatformpb.QueryContextLineageSubgraphRequest{Context: run.GetName()})
	if err != nil || len(g.GetArtifacts()) != 2 || len(g.GetExecutions()) != 1 || len(g.GetEvents()) != 2 {
		t.Fatalf("QueryContextLineageSubgraph: %v %v", g, err)
	}
	g, err = c.QueryArtifactLineageSubgraph(ctx, &aiplatformpb.QueryArtifactLineageSubgraphRequest{Artifact: data.GetName()})
	if err != nil || len(g.GetArtifacts()) != 2 || len(g.GetExecutions()) != 1 {
		t.Fatalf("QueryArtifactLineageSubgraph: %v %v", g, err)
	}
	_, err = c.QueryArtifactLineageSubgraph(ctx, &aiplatformpb.QueryArtifactLineageSubgraphRequest{Artifact: data.GetName(), MaxHops: -1})
	wantStatus2205(t, err, codes.InvalidArgument, "a negative max_hops")

	// Schemas.
	schema := "title: compat.Thing\ntype: object\n"
	sc, err := c.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: sn, MetadataSchemaId: "thing-v1",
		MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "1.0.0", Schema: schema, SchemaType: aiplatformpb.MetadataSchema_ARTIFACT_TYPE}})
	if err != nil {
		t.Fatalf("CreateMetadataSchema: %v", err)
	}
	_, err = c.CreateMetadataSchema(ctx, &aiplatformpb.CreateMetadataSchemaRequest{Parent: sn, MetadataSchema: &aiplatformpb.MetadataSchema{SchemaVersion: "1.0.0", Schema: schema}})
	wantStatus2205(t, err, codes.AlreadyExists, "a schema of the same title and version")
	if got, err := c.GetMetadataSchema(ctx, &aiplatformpb.GetMetadataSchemaRequest{Name: sc.GetName()}); err != nil || got.GetSchema() != schema {
		t.Fatalf("GetMetadataSchema: %v %v", got, err)
	}
	n := 0
	for it := c.ListMetadataSchemas(ctx, &aiplatformpb.ListMetadataSchemasRequest{Parent: sn}); ; {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMetadataSchemas: %v", err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("ListMetadataSchemas: %d schemas", n)
	}

	// Purges and deletes.
	_, err = c.PurgeArtifacts(ctx, &aiplatformpb.PurgeArtifactsRequest{Parent: sn})
	wantStatus2205(t, err, codes.InvalidArgument, "a purge without a filter")
	pop, err := c.PurgeArtifacts(ctx, &aiplatformpb.PurgeArtifactsRequest{Parent: sn, Filter: `schema_title="system.Model"`})
	if err != nil {
		t.Fatalf("PurgeArtifacts: %v", err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil || pr.GetPurgeCount() != 1 || len(pr.GetPurgeSample()) != 1 || pr.GetPurgeSample()[0] != model.GetName() {
		t.Fatalf("PurgeArtifacts dry run: %v %v", pr, err)
	}
	pop, err = c.PurgeArtifacts(ctx, &aiplatformpb.PurgeArtifactsRequest{Parent: sn, Filter: `schema_title="system.Model"`, Force: true})
	if err != nil {
		t.Fatalf("PurgeArtifacts force: %v", err)
	}
	if pr, err := pop.Wait(ctx); err != nil || pr.GetPurgeCount() != 1 {
		t.Fatalf("PurgeArtifacts force: %v %v", pr, err)
	}
	_, err = c.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: model.GetName()})
	wantStatus2205(t, err, codes.NotFound, "a purged artifact")
	_, err = c.DeleteArtifact(ctx, &aiplatformpb.DeleteArtifactRequest{Name: data.GetName(), Etag: "stale"})
	wantStatus2205(t, err, codes.FailedPrecondition, "a delete with a stale etag")
	dop, err := c.DeleteArtifact(ctx, &aiplatformpb.DeleteArtifactRequest{Name: data.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("DeleteArtifact: %v", err)
	}
	eop, err := c.PurgeExecutions(ctx, &aiplatformpb.PurgeExecutionsRequest{Parent: sn, Filter: `display_name = "train"`})
	if err != nil {
		t.Fatalf("PurgeExecutions: %v", err)
	}
	if er, err := eop.Wait(ctx); err != nil || er.GetPurgeCount() != 1 {
		t.Fatalf("PurgeExecutions: %v %v", er, err)
	}
	xop, err := c.DeleteExecution(ctx, &aiplatformpb.DeleteExecutionRequest{Name: train.GetName()})
	if err != nil || xop.Wait(ctx) != nil {
		t.Fatalf("DeleteExecution: %v", err)
	}
	if _, err := c.RemoveContextChildren(ctx, &aiplatformpb.RemoveContextChildrenRequest{Context: exp.GetName(), ChildContexts: []string{run.GetName()}}); err != nil {
		t.Fatalf("RemoveContextChildren: %v", err)
	}
	cop, err := c.PurgeContexts(ctx, &aiplatformpb.PurgeContextsRequest{Parent: sn, Filter: `schema_title="system.Experiment"`, Force: true})
	if err != nil {
		t.Fatalf("PurgeContexts: %v", err)
	}
	if cr, err := cop.Wait(ctx); err != nil || cr.GetPurgeCount() != 1 {
		t.Fatalf("PurgeContexts: %v %v", cr, err)
	}
	_, err = c.DeleteContext(ctx, &aiplatformpb.DeleteContextRequest{Name: run.GetName(), Etag: "stale"})
	wantStatus2205(t, err, codes.FailedPrecondition, "a context delete with a stale etag")
	kop, err := c.DeleteContext(ctx, &aiplatformpb.DeleteContextRequest{Name: run.GetName()})
	if err != nil || kop.Wait(ctx) != nil {
		t.Fatalf("DeleteContext: %v", err)
	}
	mop, err := c.DeleteMetadataStore(ctx, &aiplatformpb.DeleteMetadataStoreRequest{Name: sn})
	if err != nil || mop.Wait(ctx) != nil {
		t.Fatalf("DeleteMetadataStore: %v", err)
	}
	_, err = c.GetMetadataStore(ctx, &aiplatformpb.GetMetadataStoreRequest{Name: sn})
	wantStatus2205(t, err, codes.NotFound, "a deleted store")
}
