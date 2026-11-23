//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"github.com/google/uuid"
)

// covers: google.cloud.dataform.v1.Dataform/CreateCompilationResult, google.cloud.dataform.v1.Dataform/CreateWorkflowInvocation
//
// TestDataformNotebookOnRuntimeTemplate (#2105): a NOTEBOOK pipeline whose
// default_notebook_runtime_options name a Colab Enterprise runtime
// template runs on a runtime of that template: the template's environment
// variable is what the notebook prints into the executed notebook in
// Cloud Storage. A template that does not exist fails the action.
func TestDataformNotebookOnRuntimeTemplate(t *testing.T) {
	h := New(t)
	_, nbClients := notebookClients(t, h)
	clients := dataformClients(t, h)
	_, project := dataformBigQuery(t, h)
	gcs := dataformStorage(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	c, nb := clients["grpc"], nbClients["grpc"]
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	op, err := nb.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "df-tpl-" + suffix,
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "Dataform template",
			SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{Env: []*aiplatformpb.EnvVar{{Name: "STAGE", Value: "from-template-" + suffix}}}}})
	if err != nil {
		t.Fatalf("CreateNotebookRuntimeTemplate: %v", err)
	}
	tpl, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := nb.DeleteNotebookRuntimeTemplate(context.Background(), &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: tpl.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	out := "dftpl-out-" + suffix
	dataformBucket(ctx, t, gcs, project, out)
	repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "tpl" + suffix})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	dataformWrite(ctx, t, c, ws.GetName(), "content.ipynb", `{"cells":[{"cell_type":"code","metadata":{},"execution_count":null,"outputs":[],"source":"import os\nprint(os.environ.get('STAGE'))"}],"metadata":{},"nbformat":4,"nbformat_minor":5}`)
	run := func(template string) (*dfpb.WorkflowInvocation, *dfpb.WorkflowInvocationAction) {
		t.Helper()
		comp, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(), CompilationResult: &dfpb.CompilationResult{
			Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()}, CodeCompilationConfig: &dfpb.CodeCompilationConfig{
				PipelineConfig: &dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_NOTEBOOK, Path: "content.ipynb"},
				DefaultNotebookRuntimeOptions: &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: "gs://" + out},
					AiPlatformNotebookRuntimeTemplate: template}}}})
		if err != nil {
			t.Fatalf("CreateCompilationResult: %v", err)
		}
		inv, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(), WorkflowInvocation: &dfpb.WorkflowInvocation{
			CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
		if err != nil {
			t.Fatalf("CreateWorkflowInvocation: %v", err)
		}
		return waitInvocation(ctx, t, c, inv.GetName()), invocationActions(ctx, t, c, inv.GetName())["content"]
	}
	inv, a := run(tpl.GetName())
	if inv.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || a.GetNotebookAction().GetJobId() == "" {
		t.Fatalf("the notebook on the template = %v, action %v (%s)", inv.GetState(), a, a.GetFailureReason())
	}
	if got := string(dfReadObject(ctx, t, gcs, out, a.GetNotebookAction().GetJobId()+"/content.ipynb")); !strings.Contains(got, "from-template-"+suffix) {
		t.Errorf("the executed notebook does not print the template's STAGE: %s", got)
	}
	if inv, a := run(parent + "/notebookRuntimeTemplates/missing-" + suffix); inv.GetState() != dfpb.WorkflowInvocation_FAILED || a.GetFailureReason() == "" {
		t.Errorf("a missing template = %v, action %v", inv.GetState(), a)
	}
}
