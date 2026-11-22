//go:build compat

package compat

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
)

// dataformStorage is the Cloud Storage client notebook outputs and
// repository snapshots are read back with.
func dataformStorage(t *testing.T, h *Harness) *storage.Client {
	t.Helper()
	c, err := storage.NewClient(h.Context(), option.WithoutAuthentication(),
		option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func dataformBucket(ctx context.Context, t *testing.T, c *storage.Client, project, name string) {
	t.Helper()
	if err := c.Bucket(name).Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() {
		b := c.Bucket(name)
		for it := b.Objects(context.Background(), nil); ; {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = b.Object(o.Name).Delete(context.Background())
		}
		_ = b.Delete(context.Background())
	})
}

func dfReadObject(ctx context.Context, t *testing.T, c *storage.Client, bucket, object string) []byte {
	t.Helper()
	r, err := c.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		t.Fatalf("read gs://%s/%s: %v", bucket, object, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const dataformTestNotebook = `{"cells":[{"cell_type":"markdown","metadata":{},"source":["# Rows"]},` +
	`{"cell_type":"code","metadata":{},"execution_count":null,"outputs":[],"source":["from google.cloud import bigquery\n","n = list(bigquery.Client().query(\"SELECT COUNT(*) AS n FROM %s\").result())[0].n\n","print('rows', n)"]}],` +
	`"metadata":{"kernelspec":{"name":"python3","display_name":"Python 3"}},"nbformat":4,"nbformat_minor":5}`

// covers: google.cloud.dataform.v1.Dataform/CreateCompilationResult, google.cloud.dataform.v1.Dataform/QueryCompilationResultActions, google.cloud.dataform.v1.Dataform/CreateWorkflowInvocation, google.cloud.dataform.v1.Dataform/QueryWorkflowInvocationActions, google.cloud.dataform.v1.Dataform/CreateWorkflowConfig, google.cloud.dataform.v1.Dataform/CreateReleaseConfig
//
// TestDataformNotebooksTestsAndDataPreparations (#2105): a project with a
// table, a notebook (definitions/actions.yaml), a unit test and a SQL data
// preparation compiles with Dataform core into a relation, a notebook, a
// bigquery_unit_test and a data preparation action, and the compilation
// writes the repository snapshot its notebook runtime options ask for.
// The default invocation runs every action but the unit test: the
// notebook on the instance's notebook runtime, its executed copy in the
// output bucket printing the table's row count, and the data preparation
// appending into its target. UNIT_TESTS_ONLY runs the unit test alone, a
// passing one and then a failing one. A workflow config's
// end_user_auth_config names the caller.
func TestDataformNotebooksTestsAndDataPreparations(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	bq, project := dataformBigQuery(t, h)
	gcs := dataformStorage(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: Dataform core and the notebook runtime run in containers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	c, rest := clients["grpc"], clients["rest"]
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dataset := "dfp_" + suffix
	out, snaps := "dfnb-out-"+suffix, "dfnb-snap-"+suffix
	dataformBucket(ctx, t, gcs, project, out)
	dataformBucket(ctx, t, gcs, project, snaps)
	t.Cleanup(func() {
		_ = bq.Dataset(dataset).DeleteWithContents(context.Background())
		_ = bq.Dataset(dataset + "_assertions").DeleteWithContents(context.Background())
	})
	repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "nb" + suffix})
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
	w := ws.GetName()
	dataformWrite(ctx, t, c, w, "workflow_settings.yaml", fmt.Sprintf(
		"defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndefaultAssertionDataset: %s_assertions\ndataformCoreVersion: 3.1.6\n", project, dataset, dataset))
	dataformWrite(ctx, t, c, w, "definitions/src.sqlx", "config { type: \"table\" }\nselect 1 as a union all select 2 as a union all select 3 as a\n")
	dataformWrite(ctx, t, c, w, "definitions/doubled.sqlx", "config { type: \"view\" }\nselect a * 2 as b from ${ref(\"src\")}\n")
	dataformWrite(ctx, t, c, w, "definitions/doubled_test.sqlx", "config { type: \"test\", dataset: \"doubled\" }\ninput \"src\" { select 5 as a }\nselect 10 as b\n")
	dataformWrite(ctx, t, c, w, "definitions/rows.ipynb", fmt.Sprintf(dataformTestNotebook, "`"+project+"."+dataset+".src`"))
	dataformWrite(ctx, t, c, w, "definitions/actions.yaml", "actions:\n- notebook:\n    filename: rows.ipynb\n    dependencyTargets:\n    - name: src\n")
	dataformWrite(ctx, t, c, w, "definitions/prep.sqlx", "config { type: \"dataPreparation\", errorTable: { name: \"prep_errors\", retentionDays: 3 }, loadMode: { mode: \"APPEND\" } }\n"+
		"FROM ${ref(\"src\")}\n|> WHERE a > 1\n|> SELECT a\n")

	opts := &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: "gs://" + out},
		RepositorySnapshotStorage: &dfpb.NotebookRuntimeOptions_GcsRepositorySnapshotDestination{GcsRepositorySnapshotDestination: &dfpb.GcsRepositorySnapshotDestination{RepositorySnapshotUri: "gs://" + snaps + "/snapshots/"}}}
	if _, err := rest.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(), CompilationResult: &dfpb.CompilationResult{
		Source: &dfpb.CompilationResult_Workspace{Workspace: w}, CodeCompilationConfig: &dfpb.CodeCompilationConfig{
			DefaultNotebookRuntimeOptions: &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: out}}}}}); quotaCode(err) != codes.InvalidArgument {
		t.Errorf("gcs_output_bucket without gs://: %v, want INVALID_ARGUMENT", err)
	}
	comp, err := rest.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(), CompilationResult: &dfpb.CompilationResult{
		Source: &dfpb.CompilationResult_Workspace{Workspace: w}, CodeCompilationConfig: &dfpb.CodeCompilationConfig{DefaultNotebookRuntimeOptions: opts}}})
	if err != nil {
		t.Fatalf("CreateCompilationResult: %v", err)
	}
	if len(comp.GetCompilationErrors()) > 0 {
		t.Fatalf("compilation errors: %v", comp.GetCompilationErrors())
	}
	md := comp.GetGcsRepositorySnapshotMetadata()
	if !strings.HasPrefix(md.GetRepositorySnapshotUri(), "gs://"+snaps+"/snapshots/") || md.GetGeneration() == 0 || md.GetCrc32CChecksum() == "" {
		t.Fatalf("gcs_repository_snapshot_metadata = %v", md)
	}
	snap := dfReadObject(ctx, t, gcs, snaps, strings.TrimPrefix(md.GetRepositorySnapshotUri(), "gs://"+snaps+"/"))
	zr, err := gzip.NewReader(strings.NewReader(string(snap)))
	if err != nil {
		t.Fatalf("the snapshot is not gzip: %v", err)
	}
	files := map[string]bool{}
	for tr := tar.NewReader(zr); ; {
		hd, err := tr.Next()
		if err != nil {
			break
		}
		files[hd.Name] = true
	}
	if !files["definitions/rows.ipynb"] || !files["workflow_settings.yaml"] {
		t.Errorf("the snapshot holds %v", files)
	}
	kinds := map[string]string{}
	for it := c.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: comp.GetName()}); ; {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("QueryCompilationResultActions: %v", err)
		}
		switch {
		case a.GetNotebook() != nil:
			kinds[a.GetTarget().GetName()] = "NOTEBOOK"
			if !strings.Contains(a.GetNotebook().GetContents(), "bigquery.Client()") {
				t.Errorf("notebook contents = %q", a.GetNotebook().GetContents())
			}
		case a.GetBigqueryUnitTest() != nil:
			kinds[a.GetTarget().GetName()] = "UNIT_TEST"
			if a.GetBigqueryUnitTest().GetTestQuery() == "" || a.GetBigqueryUnitTest().GetExpectedOutputQuery() == "" {
				t.Errorf("unit test = %v", a.GetBigqueryUnitTest())
			}
		case a.GetDataPreparation() != nil:
			kinds[a.GetTarget().GetName()] = "DATA_PREPARATION"
			sq := a.GetDataPreparation().GetContentsSql()
			if !strings.Contains(sq.GetQuery(), "|> WHERE a > 1") || sq.GetLoad().GetAppend() == nil || sq.GetErrorTable().GetRetentionDays() != 3 {
				t.Errorf("data preparation = %v", sq)
			}
		case a.GetRelation() != nil:
			kinds[a.GetTarget().GetName()] = a.GetRelation().GetRelationType().String()
		}
	}
	for k, v := range map[string]string{"src": "TABLE", "doubled": "VIEW", "rows": "NOTEBOOK", "doubled_test": "UNIT_TEST", "prep": "DATA_PREPARATION"} {
		if kinds[k] != v {
			t.Errorf("action %s = %q, want %q (all %v)", k, kinds[k], v, kinds)
		}
	}

	inv, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(), WorkflowInvocation: &dfpb.WorkflowInvocation{
		CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
	if err != nil {
		t.Fatalf("CreateWorkflowInvocation: %v", err)
	}
	done := waitInvocation(ctx, t, c, inv.GetName())
	acts := invocationActions(ctx, t, rest, inv.GetName())
	if acts["doubled_test"] != nil {
		t.Error("the default invocation ran the unit test")
	}
	nb := acts["rows"]
	if nb.GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED {
		t.Fatalf("notebook action = %v (%s); invocation %v", nb.GetState(), nb.GetFailureReason(), done.GetState())
	}
	na := nb.GetNotebookAction()
	if na.GetJobId() == "" || na.GetFilePath() != "definitions/rows.ipynb" || !strings.Contains(na.GetContents(), "print('rows', n)") {
		t.Errorf("notebook_action = %v", na)
	}
	var executed map[string]any
	if err := json.Unmarshal(dfReadObject(ctx, t, gcs, out, na.GetJobId()+"/rows.ipynb"), &executed); err != nil {
		t.Fatalf("the executed notebook: %v", err)
	}
	b, _ := json.Marshal(executed["cells"])
	if !strings.Contains(string(b), `rows 3`) {
		t.Errorf("the executed notebook's outputs = %s", b)
	}
	prep := acts["prep"]
	if prep.GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED {
		t.Fatalf("data preparation = %v (%s)", prep.GetState(), prep.GetFailureReason())
	}
	if dp := prep.GetDataPreparationAction(); dp.GetJobId() == "" || dp.GetGeneratedSql() == "" || dp.GetContentsSql().GetLoadConfig().GetAppend() == nil {
		t.Errorf("data_preparation_action = %v", dp)
	}
	it, err := bq.Query("SELECT COUNT(*) FROM `" + project + "." + dataset + ".prep`").Read(ctx)
	if err != nil {
		t.Fatalf("read prep: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || row[0] != int64(2) {
		t.Errorf("prep rows = %v, %v", row, err)
	}
	if _, err := bq.Dataset(dataset).Table("prep_errors").Metadata(ctx); err != nil {
		t.Errorf("the error table: %v", err)
	}
	if done.GetState() != dfpb.WorkflowInvocation_SUCCEEDED {
		t.Errorf("invocation = %v", done.GetState())
	}

	unitTests := func() *dfpb.WorkflowInvocationAction {
		inv, err := rest.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(), WorkflowInvocation: &dfpb.WorkflowInvocation{
			CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()},
			InvocationConfig:  &dfpb.InvocationConfig{ExecutionMode: dfpb.InvocationConfig_UNIT_TESTS_ONLY}}})
		if err != nil {
			t.Fatalf("CreateWorkflowInvocation UNIT_TESTS_ONLY: %v", err)
		}
		waitInvocation(ctx, t, c, inv.GetName())
		acts := invocationActions(ctx, t, c, inv.GetName())
		if len(acts) != 1 {
			t.Errorf("UNIT_TESTS_ONLY ran %d actions", len(acts))
		}
		return acts["doubled_test"]
	}
	ut := unitTests()
	if ut.GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED || ut.GetBigqueryUnitTestAction().GetActualResultsJobId() == "" || ut.GetBigqueryUnitTestAction().GetExpectedResultsJobId() == "" {
		t.Errorf("passing unit test = %v (%s)", ut, ut.GetFailureReason())
	}
	dataformWrite(ctx, t, c, w, "definitions/doubled_test.sqlx", "config { type: \"test\", dataset: \"doubled\" }\ninput \"src\" { select 5 as a }\nselect 11 as b\n")
	comp2, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(), CompilationResult: &dfpb.CompilationResult{
		Source: &dfpb.CompilationResult_Workspace{Workspace: w}}})
	if err != nil {
		t.Fatal(err)
	}
	comp = comp2
	if ut := unitTests(); ut.GetState() != dfpb.WorkflowInvocationAction_FAILED || !strings.Contains(ut.GetFailureReason(), `expected "11", but saw "10"`) {
		t.Errorf("failing unit test = %v (%s)", ut.GetState(), ut.GetFailureReason())
	}

	// end_user_auth_config's user_email is the caller.
	author := &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}
	if _, err := c.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: w, Author: author, CommitMessage: "all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: w}); err != nil {
		t.Fatal(err)
	}
	rc, err := c.CreateReleaseConfig(ctx, &dfpb.CreateReleaseConfigRequest{Parent: repo.GetName(), ReleaseConfigId: "rel", ReleaseConfig: &dfpb.ReleaseConfig{GitCommitish: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	wc, err := rest.CreateWorkflowConfig(ctx, &dfpb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "eu", WorkflowConfig: &dfpb.WorkflowConfig{
		ReleaseConfig: rc.GetName(), InvocationConfig: &dfpb.InvocationConfig{EndUserAuthConfig: &dfpb.InvocationConfig_EndUserAuthenticationConfig{
			OauthConfig: &dfpb.OAuthConfig{AdditionalOauthScopes: []string{"https://www.googleapis.com/auth/drive.readonly"}}}}}})
	if err != nil {
		t.Fatalf("CreateWorkflowConfig with end_user_auth_config: %v", err)
	}
	if e := wc.GetInvocationConfig().GetEndUserAuthConfig(); !strings.Contains(e.GetUserEmail(), "@") || len(e.GetOauthConfig().GetAdditionalOauthScopes()) != 1 {
		t.Errorf("end_user_auth_config = %v", e)
	}
}

// covers: google.cloud.dataform.v1.Dataform/CreateCompilationResult, google.cloud.dataform.v1.Dataform/InstallNpmPackages, google.cloud.dataform.v1.Dataform/GetWorkflowInvocation
//
// TestDataformPipelineConfig (#2105): code_compilation_config.pipeline_config
// compiles a NOTEBOOK single file asset (BigQuery Studio's notebook, the
// one action a scheduled notebook runs), a SQL one, and a DATAFORM
// pipeline in a folder of the repository; the invocation reports its
// pipeline; a bad pipeline is refused.
func TestDataformPipelineConfig(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	bq, project := dataformBigQuery(t, h)
	gcs := dataformStorage(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dataset := "dfpc_" + suffix
	out := "dfpc-out-" + suffix
	dataformBucket(ctx, t, gcs, project, out)
	t.Cleanup(func() { _ = bq.Dataset(dataset).DeleteWithContents(context.Background()) })
	if err := bq.Dataset(dataset).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "pc" + suffix,
		Repository: &dfpb.Repository{Labels: map[string]string{"single-file-asset-type": "notebook"}}})
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
	w := ws.GetName()
	dataformWrite(ctx, t, c, w, "content.ipynb", `{"cells":[{"cell_type":"code","metadata":{},"execution_count":4,"outputs":[{"output_type":"stream","name":"stdout","text":"old"}],"source":"print(6 * 7)"}],"metadata":{},"nbformat":4,"nbformat_minor":5}`)
	dataformWrite(ctx, t, c, w, "content.sql", "CREATE TABLE `"+project+"."+dataset+".from_sql` AS SELECT 'sql' AS s")
	dataformWrite(ctx, t, c, w, "pipelines/one/workflow_settings.yaml", fmt.Sprintf("defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndataformCoreVersion: 3.1.6\n", project, dataset))
	dataformWrite(ctx, t, c, w, "pipelines/one/definitions/t.sqlx", "config { type: \"table\" }\nselect 'df' as s\n")
	compile := func(p *dfpb.PipelineConfig, o *dfpb.NotebookRuntimeOptions) (*dfpb.CompilationResult, error) {
		return c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(), CompilationResult: &dfpb.CompilationResult{
			Source: &dfpb.CompilationResult_Workspace{Workspace: w}, CodeCompilationConfig: &dfpb.CodeCompilationConfig{DefaultSchema: dataset, PipelineConfig: p, DefaultNotebookRuntimeOptions: o}}})
	}
	if _, err := compile(&dfpb.PipelineConfig{Path: "content.sql"}, nil); quotaCode(err) != codes.InvalidArgument {
		t.Errorf("a pipeline without a type: %v", err)
	}
	if _, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_SQL, Path: "missing.sql"}, nil); quotaCode(err) != codes.InvalidArgument {
		t.Errorf("a missing file: %v", err)
	}
	invoke := func(comp *dfpb.CompilationResult) (*dfpb.WorkflowInvocation, map[string]*dfpb.WorkflowInvocationAction) {
		inv, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(), WorkflowInvocation: &dfpb.WorkflowInvocation{
			CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
		if err != nil {
			t.Fatalf("CreateWorkflowInvocation: %v", err)
		}
		if inv.GetPipelineConfig().GetPipelineType() != comp.GetCodeCompilationConfig().GetPipelineConfig().GetPipelineType() {
			t.Errorf("invocation pipeline_config = %v", inv.GetPipelineConfig())
		}
		return waitInvocation(ctx, t, c, inv.GetName()), invocationActions(ctx, t, c, inv.GetName())
	}
	nbComp, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_NOTEBOOK, Path: "content.ipynb"},
		&dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: "gs://" + out}})
	if err != nil {
		t.Fatalf("compile a NOTEBOOK pipeline: %v", err)
	}
	got, acts := invoke(nbComp)
	a := acts["content"]
	if got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || a.GetNotebookAction().GetJobId() == "" {
		t.Fatalf("NOTEBOOK invocation = %v, action %v (%s)", got.GetState(), a, a.GetFailureReason())
	}
	if !strings.Contains(string(dfReadObject(ctx, t, gcs, out, a.GetNotebookAction().GetJobId()+"/content.ipynb")), "42") {
		t.Error("the executed notebook does not print 42")
	}
	sqlComp, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_SQL, Path: "content.sql"}, nil)
	if err != nil {
		t.Fatalf("compile a SQL pipeline: %v", err)
	}
	if got, acts := invoke(sqlComp); got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED {
		t.Errorf("SQL invocation = %v (%v)", got.GetState(), acts)
	}
	if _, err := bq.Dataset(dataset).Table("from_sql").Metadata(ctx); err != nil {
		t.Errorf("the SQL asset's table: %v", err)
	}
	dfComp, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_DATAFORM, Path: "pipelines/one"}, nil)
	if err != nil {
		t.Fatalf("compile a DATAFORM pipeline: %v", err)
	}
	if got, acts := invoke(dfComp); got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || acts["t"] == nil {
		t.Errorf("DATAFORM invocation = %v (%v)", got.GetState(), acts)
	}
	if _, err := c.InstallNpmPackages(ctx, &dfpb.InstallNpmPackagesRequest{Workspace: w, PipelineConfig: &dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_NOTEBOOK, Path: "content.ipynb"}}); quotaCode(err) != codes.FailedPrecondition {
		t.Errorf("InstallNpmPackages of a notebook asset: %v", err)
	}
}

// covers: google.cloud.dataform.v1beta1.Dataform/ComputeRepositoryAccessTokenStatus, google.cloud.dataform.v1beta1.Dataform/FetchRemoteBranches
//
// TestDataformV1beta1RemoteBranchesAndTokenStatus (#2105): the official
// v1beta1 client, over gRPC and REST, on a repository whose remote is a
// Secure Source Manager repository of this instance: the token's status,
// VALID, then NOT_FOUND for a secret version that does not exist; a
// workspace pushes two branches, which FetchRemoteBranches lists.
func TestDataformV1beta1RemoteBranchesAndTokenStatus(t *testing.T) {
	h := New(t)
	clients := dataformBetaClients(t, h)
	_ = runClient(t, h)
	sm := secretsClient(t, h)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	ssm, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ssm.Close() })
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "bi-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "br-" + suffix, Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + h.Project(), SecretId: "dfb-token-" + suffix,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret.GetName()})
	})
	version, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte("local-token")}})
	if err != nil {
		t.Fatal(err)
	}
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			dfParent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
			repo, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: dfParent, RepositoryId: "brem" + transport + suffix,
				Repository: &b1pb.Repository{GitRemoteSettings: &b1pb.Repository_GitRemoteSettings{Url: remote.GetUris().GetGitHttps(), DefaultBranch: "main",
					AuthenticationTokenSecretVersion: version.GetName()}}})
			if err != nil {
				t.Fatalf("CreateRepository: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &b1pb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &b1pb.ComputeRepositoryAccessTokenStatusRequest{Name: repo.GetName()})
			if err != nil || st.GetTokenStatus() != b1pb.ComputeRepositoryAccessTokenStatusResponse_VALID {
				t.Errorf("ComputeRepositoryAccessTokenStatus = %v, %v", st, err)
			}
			ws, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if _, err := c.WriteFile(ctx, &b1pb.WriteFileRequest{Workspace: ws.GetName(), Path: "definitions/b_" + transport + ".sqlx", Contents: []byte("select 1")}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.CommitWorkspaceChanges(ctx, &b1pb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: &b1pb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}, CommitMessage: "b"}); err != nil {
				t.Fatal(err)
			}
			for _, b := range []string{"main", "beta-" + transport} {
				if _, err := c.PushGitCommits(ctx, &b1pb.PushGitCommitsRequest{Name: ws.GetName(), RemoteBranch: b}); err != nil {
					t.Fatalf("PushGitCommits %s: %v", b, err)
				}
			}
			br, err := c.FetchRemoteBranches(ctx, &b1pb.FetchRemoteBranchesRequest{Name: repo.GetName()})
			if err != nil || !strings.Contains(","+strings.Join(br.GetBranches(), ",")+",", ",beta-"+transport+",") || !strings.Contains(","+strings.Join(br.GetBranches(), ",")+",", ",main,") {
				t.Errorf("FetchRemoteBranches = %v, %v", br, err)
			}
			upd := &b1pb.Repository{Name: repo.GetName(), GitRemoteSettings: &b1pb.Repository_GitRemoteSettings{Url: remote.GetUris().GetGitHttps(), DefaultBranch: "main",
				AuthenticationTokenSecretVersion: secret.GetName() + "/versions/99"}}
			if _, err := c.UpdateRepository(ctx, &b1pb.UpdateRepositoryRequest{Repository: upd}); err != nil {
				t.Fatalf("UpdateRepository: %v", err)
			}
			if st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &b1pb.ComputeRepositoryAccessTokenStatusRequest{Name: repo.GetName()}); err != nil || st.GetTokenStatus() != b1pb.ComputeRepositoryAccessTokenStatusResponse_NOT_FOUND {
				t.Errorf("ComputeRepositoryAccessTokenStatus of a missing version = %v, %v", st, err)
			}
		})
	}
}

// sshGitServer is a local sshd test fixture: an SSH server on loopback
// that runs git-upload-pack and git-receive-pack on bare repositories
// under root for the one client key it authorizes.
type sshGitServer struct {
	addr    string
	hostPub string // OpenSSH authorized_keys form
	userKey []byte // OpenSSH PEM private key
	ln      net.Listener
	wg      sync.WaitGroup
}

func startSSHGitServer(t *testing.T, root string) *sshGitServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	userPub, userPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(userPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ssh.NewPublicKey(userPub)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(allowed.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("unknown key")
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sshGitServer{addr: ln.Addr().String(), ln: ln, userKey: pem.EncodeToMemory(block),
		hostPub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(conn, cfg, root)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); s.wg.Wait() })
	return s
}

func (s *sshGitServer) serve(conn net.Conn, cfg *ssh.ServerConfig, root string) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range in {
				if req.Type != "exec" || len(req.Payload) < 4 {
					_ = req.Reply(false, nil)
					continue
				}
				line := string(req.Payload[4:])
				verb, arg, _ := strings.Cut(line, " ")
				if verb != "git-upload-pack" && verb != "git-receive-pack" {
					_ = req.Reply(false, nil)
					continue
				}
				repo := filepath.Join(root, filepath.Base(strings.Trim(arg, "'\"/")))
				_ = req.Reply(true, nil)
				cmd := exec.Command(verb, repo)
				cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				status := uint32(0)
				if err := cmd.Run(); err != nil {
					status = 1
				}
				_ = ch.CloseWrite()
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}

// covers: google.cloud.dataform.v1.Dataform/CreateRepository, google.cloud.dataform.v1.Dataform/ComputeRepositoryAccessTokenStatus, google.cloud.dataform.v1.Dataform/FetchRemoteBranches, google.cloud.dataform.v1.Dataform/PushGitCommits, google.cloud.dataform.v1.Dataform/PullGitCommits
//
// TestDataformSSHRemote (#2105): git_remote_settings with
// ssh_authentication_config, against a local sshd test fixture (this
// instance's Secure Source Manager serves git over HTTPS only): the
// private key from Secret Manager and the host's public key; a workspace
// pushes over SSH, the remote's branches are listed, a second workspace
// pulls; a wrong host key fails host key checking; a remote that is not
// on this machine is refused.
func TestDataformSSHRemote(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	sm := secretsClient(t, h)
	for _, bin := range []string{"git", "ssh", "git-upload-pack"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required", bin)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", filepath.Join(root, "remote.git")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	srv := startSSHGitServer(t, root)
	host, port, _ := net.SplitHostPort(srv.addr)
	url := "ssh://git@" + host + ":" + port + "/remote.git"
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	secret, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + h.Project(), SecretId: "df-ssh-" + suffix,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret.GetName()})
	})
	version, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: srv.userKey}})
	if err != nil {
		t.Fatal(err)
	}
	sshCfg := &dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig{UserPrivateKeySecretVersion: version.GetName(), HostPublicKey: srv.hostPub}
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
			if _, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "sshfar" + transport + suffix,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: "ssh://git@example.com/r.git", DefaultBranch: "main", SshAuthenticationConfig: sshCfg}}}); quotaCode(err) != codes.InvalidArgument {
				t.Errorf("an SSH remote off this machine: %v, want INVALID_ARGUMENT", err)
			}
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "ssh" + transport + suffix,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: url, DefaultBranch: "main", SshAuthenticationConfig: sshCfg}}})
			if err != nil {
				t.Fatalf("CreateRepository with an SSH remote: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			if got := repo.GetGitRemoteSettings().GetSshAuthenticationConfig(); got.GetHostPublicKey() != srv.hostPub {
				t.Errorf("ssh_authentication_config = %v", got)
			}
			if st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &dfpb.ComputeRepositoryAccessTokenStatusRequest{Name: repo.GetName()}); err != nil || st.GetTokenStatus() != dfpb.ComputeRepositoryAccessTokenStatusResponse_VALID {
				t.Errorf("ComputeRepositoryAccessTokenStatus = %v, %v", st, err)
			}
			ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "a"})
			if err != nil {
				t.Fatal(err)
			}
			dataformWrite(ctx, t, c, ws.GetName(), "definitions/"+transport+".sqlx", "select 1 as x\n")
			author := &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}
			if _, err := c.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: author, CommitMessage: transport}); err != nil {
				t.Fatal(err)
			}
			_, _ = c.PullGitCommits(ctx, &dfpb.PullGitCommitsRequest{Name: ws.GetName(), Author: author})
			if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName(), RemoteBranch: "main"}); err != nil {
				t.Fatalf("PushGitCommits over SSH: %v", err)
			}
			if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName(), RemoteBranch: "ssh-" + transport}); err != nil {
				t.Fatalf("PushGitCommits to a new branch over SSH: %v", err)
			}
			br, err := c.FetchRemoteBranches(ctx, &dfpb.FetchRemoteBranchesRequest{Name: repo.GetName()})
			if err != nil || !strings.Contains(strings.Join(br.GetBranches(), ","), "ssh-"+transport) {
				t.Errorf("FetchRemoteBranches = %v, %v", br, err)
			}
			ws2, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "b"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.PullGitCommits(ctx, &dfpb.PullGitCommitsRequest{Name: ws2.GetName(), Author: author}); err != nil {
				t.Fatalf("PullGitCommits over SSH: %v", err)
			}
			if f, err := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws2.GetName(), Path: "definitions/" + transport + ".sqlx"}); err != nil || string(f.GetFileContents()) != "select 1 as x\n" {
				t.Errorf("the pulled file = %v, %v", f, err)
			}
			// A host key that is not the server's fails host key checking.
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			signer, _ := ssh.NewSignerFromKey(other)
			bad := &dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig{UserPrivateKeySecretVersion: version.GetName(),
				HostPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}
			badRepo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "sshbad" + transport + suffix,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: url, DefaultBranch: "main", SshAuthenticationConfig: bad}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: badRepo.GetName(), Force: true})
			})
			if st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &dfpb.ComputeRepositoryAccessTokenStatusRequest{Name: badRepo.GetName()}); err != nil || st.GetTokenStatus() != dfpb.ComputeRepositoryAccessTokenStatusResponse_INVALID {
				t.Errorf("ComputeRepositoryAccessTokenStatus with a wrong host key = %v, %v", st, err)
			}
		})
	}
}
