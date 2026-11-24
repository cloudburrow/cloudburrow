//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"github.com/google/uuid"
)

// covers: google.cloud.dataform.v1.Dataform/CreateWorkflowInvocation, google.cloud.dataform.v1.Dataform/QueryWorkflowInvocationActions
//
// TestDataformInvocationRunOptions (#2105): BigQuery Studio's Run >
// Select tasks to run and Select tags to run are invocation_config's
// included_targets and included_tags with transitive_dependents_included,
// and Run as interactive job with high priority cleared is query_priority
// BATCH: over gRPC and REST, a tag run runs the tagged table alone, a
// target run with its dependents runs the view after it too, and a BATCH
// run's BigQuery job has batch priority.
func TestDataformInvocationRunOptions(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	bq, project := dataformBigQuery(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: Dataform core runs in a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
			dataset := "dfrun_" + suffix
			t.Cleanup(func() {
				_ = bq.Dataset(dataset).DeleteWithContents(context.Background())
				_ = bq.Dataset(dataset + "_assertions").DeleteWithContents(context.Background())
			})
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "run" + transport + suffix})
			if err != nil {
				t.Fatalf("CreateRepository: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			dataformProject(ctx, t, c, ws.GetName(), project, dataset)
			comp, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
				CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()}}})
			if err != nil || len(comp.GetCompilationErrors()) > 0 {
				t.Fatalf("CreateCompilationResult = %v, %v", comp, err)
			}
			invoke := func(ic *dfpb.InvocationConfig) (*dfpb.WorkflowInvocation, map[string]*dfpb.WorkflowInvocationAction) {
				t.Helper()
				inv, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
					WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}, InvocationConfig: ic}})
				if err != nil {
					t.Fatalf("CreateWorkflowInvocation: %v", err)
				}
				done := waitInvocation(ctx, t, c, inv.GetName())
				return done, invocationActions(ctx, t, c, inv.GetName())
			}
			batch := dfpb.InvocationConfig_BATCH
			// Select tags to run: daily is x's tag alone; BATCH priority.
			inv, acts := invoke(&dfpb.InvocationConfig{IncludedTags: []string{"daily"}, QueryPriority: &batch})
			if inv.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || len(acts) != 1 || acts["x"] == nil {
				t.Fatalf("a tag run = %v, actions %v", inv.GetState(), acts)
			}
			if got := inv.GetInvocationConfig().GetQueryPriority(); got != dfpb.InvocationConfig_BATCH {
				t.Errorf("invocation_config.query_priority = %v, want BATCH", got)
			}
			id := acts["x"].GetBigqueryAction().GetJobId()
			if id == "" {
				t.Fatalf("the tag run's action has no job: %v", acts["x"])
			}
			job, err := bq.JobFromIDLocation(ctx, id, "US")
			if err != nil {
				t.Fatalf("jobs.get %s: %v", id, err)
			}
			cfg, err := job.Config()
			if err != nil {
				t.Fatal(err)
			}
			if q, ok := cfg.(*bigquery.QueryConfig); !ok || q.Priority != bigquery.BatchPriority {
				t.Errorf("the BATCH run's job configuration = %+v, want batch priority", cfg)
			}
			// Select tasks to run: x with Include dependents runs v, inc, op
			// and the assertions on it too.
			inv, acts = invoke(&dfpb.InvocationConfig{IncludedTargets: []*dfpb.Target{{Database: project, Schema: dataset, Name: "x"}}, TransitiveDependentsIncluded: true})
			for _, n := range []string{"x", "v", "inc", "op"} {
				if acts[n] == nil {
					t.Errorf("x with its dependents did not run %s: %v", n, acts)
				}
			}
			// Without dependents, v alone, at interactive priority.
			inv, acts = invoke(&dfpb.InvocationConfig{IncludedTargets: []*dfpb.Target{{Database: project, Schema: dataset, Name: "v"}}})
			if inv.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || len(acts) != 1 || acts["v"] == nil {
				t.Errorf("a target run = %v, actions %v", inv.GetState(), acts)
			}
		})
	}
}
