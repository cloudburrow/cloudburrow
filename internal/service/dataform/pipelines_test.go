package dataform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/grpc/codes"
)

// cannedRunner compiles every project to the same graph.
type cannedRunner struct {
	graph string
	dirs  []string
}

func (r *cannedRunner) Compile(_ context.Context, dir string, _ []string) ([]byte, error) {
	r.dirs = append(r.dirs, dir)
	return []byte(r.graph), nil
}
func (r *cannedRunner) InstallPackages(context.Context, string, []string) error { return nil }

// rowsWarehouse answers queries with rows the test sets, and keeps the SQL.
type rowsWarehouse struct {
	mu    sync.Mutex
	rows  map[string][][]*string
	cols  map[string][]string
	exec  []string
	table map[string][]string
}

func (w *rowsWarehouse) EnsureDataset(context.Context, string, string, string) error { return nil }
func (w *rowsWarehouse) Table(_ context.Context, _, d, t string) (bool, string, []string, error) {
	c, ok := w.table[d+"."+t]
	return ok, "TABLE", c, nil
}
func (w *rowsWarehouse) Exec(_ context.Context, _, _, sql string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.exec = append(w.exec, sql)
	return "job_exec", nil
}
func (w *rowsWarehouse) Count(context.Context, string, string, string) (string, int64, error) {
	return "job_count", 0, nil
}
func (w *rowsWarehouse) Rows(_ context.Context, _, _, sql string) (string, []string, [][]*string, int64, error) {
	k := strings.TrimSpace(sql)
	return "job_" + k[:1], w.cols[k], w.rows[k], 10, nil
}

type fakeNotebooks struct {
	ran  []string
	fail bool
}

func (f *fakeNotebooks) RunNotebook(_ context.Context, key, _ string, nb []byte) ([]byte, error) {
	f.ran = append(f.ran, key)
	out := []byte(strings.Replace(string(nb), `"outputs":[]`, `"outputs":[{"output_type":"stream","name":"stdout","text":"1\n"}]`, 1))
	if f.fail {
		return out, errors.New("cell 1 raised ValueError: bad")
	}
	return out, nil
}

type fakeObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (f *fakeObjects) PutObject(_ context.Context, bucket, object, _ string, data []byte) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objs == nil {
		f.objs = map[string][]byte{}
	}
	f.objs[bucket+"/"+object] = data
	return 42, "AAAAAA==", nil
}

const pipelineGraph = `{
 "tables":[{"type":"table","enumType":"TABLE","target":{"database":"p","schema":"ds","name":"t1"},"fileName":"definitions/t1.sqlx","query":"select 1 as a"}],
 "tests":[{"name":"test1","testQuery":"T select 1 as a","expectedOutputQuery":"E select 1 as a","fileName":"definitions/test1.sqlx","target":{"database":"p","schema":"ds","name":"test1"}}],
 "notebooks":[{"target":{"database":"p","schema":"ds","name":"nb"},"fileName":"definitions/nb.ipynb","dependencyTargets":[{"database":"p","schema":"ds","name":"t1"}],"notebookContents":"{\"cells\":[{\"cell_type\":\"code\",\"source\":[\"print(1)\"],\"outputs\":[],\"execution_count\":null}],\"metadata\":{},\"nbformat\":4,\"nbformat_minor\":5}"}],
 "dataPreparations":[{"target":{"database":"p","schema":"ds","name":"prep"},"fileName":"definitions/prep.sqlx","errorTable":{"database":"p","schema":"ds","name":"prep_errors"},"errorTableRetentionDays":3,"query":"FROM ` + "`p.ds.t1`" + ` |> WHERE a > 0","load":{"append":{}},"dependencyTargets":[{"database":"p","schema":"ds","name":"t1"}]}],
 "dataformCoreVersion":"3.1.6","graphErrors":{}}`

func pipelineServer(t *testing.T) (*Server, *cannedRunner, *rowsWarehouse, *fakeNotebooks, *fakeObjects, string) {
	t.Helper()
	r := &cannedRunner{graph: pipelineGraph}
	s1, s2 := "1", "2"
	w := &rowsWarehouse{
		cols:  map[string][]string{"T select 1 as a": {"a"}, "E select 1 as a": {"a"}},
		rows:  map[string][][]*string{"T select 1 as a": {{&s1}}, "E select 1 as a": {{&s1}}},
		table: map[string][]string{"ds.prep": {"a"}},
	}
	_ = s2
	nb, objs := &fakeNotebooks{}, &fakeObjects{}
	s, err := NewServer(Options{DataDir: t.TempDir(), Runner: r, Warehouse: w, Notebooks: nb, Storage: objs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	const parent = "projects/p/locations/us-central1"
	repo, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r", Repository: &dfpb.Repository{}})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "w", Workspace: &dfpb.Workspace{}})
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{
		"workflow_settings.yaml":     "defaultProject: p\ndefaultDataset: ds\n",
		"sub/workflow_settings.yaml": "defaultProject: p\ndefaultDataset: ds\n",
		"content.ipynb":              `{"cells":[{"cell_type":"code","source":"print(2)","outputs":[{"output_type":"stream"}],"execution_count":3}],"metadata":{},"nbformat":4,"nbformat_minor":5}`,
		"q.sql":                      "select 7",
	} {
		if _, err := s.WriteFile(ctx, &dfpb.WriteFileRequest{Workspace: ws.GetName(), Path: p, Contents: []byte(c)}); err != nil {
			t.Fatal(err)
		}
	}
	return s, r, w, nb, objs, ws.GetName()
}

func waitInvocation(t *testing.T, s *Server, name string) (*dfpb.WorkflowInvocation, []*dfpb.WorkflowInvocationAction) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		inv, err := s.GetWorkflowInvocation(ctx, &dfpb.GetWorkflowInvocationRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if inv.GetState() != dfpb.WorkflowInvocation_RUNNING {
			acts, err := s.QueryWorkflowInvocationActions(ctx, &dfpb.QueryWorkflowInvocationActionsRequest{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			return inv, acts.GetWorkflowInvocationActions()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s still RUNNING", name)
	return nil, nil
}

func byName(acts []*dfpb.WorkflowInvocationAction) map[string]*dfpb.WorkflowInvocationAction {
	m := map[string]*dfpb.WorkflowInvocationAction{}
	for _, a := range acts {
		m[a.GetTarget().GetName()] = a
	}
	return m
}

// TestUnitTestsAndExecutionMode: a test compiles to a bigquery_unit_test
// action, which only UNIT_TESTS_ONLY runs; its rows are compared.
func TestUnitTestsAndExecutionMode(t *testing.T) {
	s, _, w, _, _, ws := pipelineServer(t)
	ctx := context.Background()
	repo := strings.SplitN(ws, "/workspaces/", 2)[0]
	comp, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo, CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws}}})
	if err != nil {
		t.Fatal(err)
	}
	acts, err := s.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: comp.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	var test *dfpb.CompilationResultAction
	for _, a := range acts.GetCompilationResultActions() {
		if a.GetBigqueryUnitTest() != nil {
			test = a
		}
	}
	if test == nil || test.GetBigqueryUnitTest().GetDisplayName() != "test1" || test.GetBigqueryUnitTest().GetExpectedOutputQuery() != "E select 1 as a" {
		t.Fatalf("unit test action = %v", test)
	}
	run := func(mode dfpb.InvocationConfig_ExecutionMode) map[string]*dfpb.WorkflowInvocationAction {
		inv, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo, WorkflowInvocation: &dfpb.WorkflowInvocation{
			CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()},
			InvocationConfig:  &dfpb.InvocationConfig{ExecutionMode: mode}}})
		if err != nil {
			t.Fatal(err)
		}
		_, a := waitInvocation(t, s, inv.GetName())
		return byName(a)
	}
	def := run(dfpb.InvocationConfig_DEFAULT)
	if def["test1"] != nil || def["t1"] == nil {
		t.Fatalf("DEFAULT ran %v", def)
	}
	only := run(dfpb.InvocationConfig_UNIT_TESTS_ONLY)
	if len(only) != 1 || only["test1"].GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED {
		t.Fatalf("UNIT_TESTS_ONLY = %v", only)
	}
	ut := only["test1"].GetBigqueryUnitTestAction()
	if ut.GetActualResultsJobId() == "" || ut.GetExpectedResultsJobId() == "" || ut.GetTotalProcessedBytes() != 20 {
		t.Errorf("unit test action = %v", ut)
	}
	two := "2"
	w.rows["E select 1 as a"] = [][]*string{{&two}}
	only = run(dfpb.InvocationConfig_UNIT_TESTS_ONLY)
	if a := only["test1"]; a.GetState() != dfpb.WorkflowInvocationAction_FAILED || !strings.Contains(a.GetFailureReason(), `For row 0 and column "a": expected "2", but saw "1".`) {
		t.Fatalf("a failing unit test = %v", a)
	}
}

// TestNotebookActionsAndSnapshot: a notebook action runs on the notebook
// runner, its output goes to the output bucket, and the compilation
// writes the repository snapshot.
func TestNotebookActionsAndSnapshot(t *testing.T) {
	s, _, w, nb, objs, ws := pipelineServer(t)
	ctx := context.Background()
	repo := strings.SplitN(ws, "/workspaces/", 2)[0]
	if _, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo, CompilationResult: &dfpb.CompilationResult{
		Source:                &dfpb.CompilationResult_Workspace{Workspace: ws},
		CodeCompilationConfig: &dfpb.CodeCompilationConfig{DefaultNotebookRuntimeOptions: &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: "bucket"}}}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("a bucket without gs://: %v", err)
	}
	opts := &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: "gs://out-bucket"},
		RepositorySnapshotStorage: &dfpb.NotebookRuntimeOptions_GcsRepositorySnapshotDestination{GcsRepositorySnapshotDestination: &dfpb.GcsRepositorySnapshotDestination{RepositorySnapshotUri: "gs://snaps/dir/"}}}
	comp, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo, CompilationResult: &dfpb.CompilationResult{
		Source: &dfpb.CompilationResult_Workspace{Workspace: ws}, CodeCompilationConfig: &dfpb.CodeCompilationConfig{DefaultNotebookRuntimeOptions: opts}}})
	if err != nil {
		t.Fatal(err)
	}
	md := comp.GetGcsRepositorySnapshotMetadata()
	id := comp.GetName()[strings.LastIndex(comp.GetName(), "/")+1:]
	if md.GetRepositorySnapshotUri() != "gs://snaps/dir/"+id+".tar.gz" || md.GetGeneration() != 42 || objs.objs["snaps/dir/"+id+".tar.gz"] == nil {
		t.Fatalf("snapshot = %v", md)
	}
	inv, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo, WorkflowInvocation: &dfpb.WorkflowInvocation{
		CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	got, acts := waitInvocation(t, s, inv.GetName())
	a := byName(acts)
	n := a["nb"]
	if n.GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED || n.GetNotebookAction().GetJobId() == "" || n.GetNotebookAction().GetFilePath() != "definitions/nb.ipynb" || len(nb.ran) != 1 {
		t.Fatalf("notebook action = %v (ran %v)", n, nb.ran)
	}
	out := objs.objs["out-bucket/"+n.GetNotebookAction().GetJobId()+"/nb.ipynb"]
	if !strings.Contains(string(out), `"text":"1\n"`) {
		t.Errorf("executed notebook = %s", out)
	}
	if got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED {
		t.Errorf("invocation = %v", got.GetState())
	}
	// The data preparation appended into its existing target and made
	// its error table.
	p := a["prep"]
	if p.GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED || !strings.Contains(p.GetDataPreparationAction().GetGeneratedSql(), "insert into `p.ds.prep`") ||
		p.GetDataPreparationAction().GetContentsSql().GetErrorTable().GetRetentionDays() != 3 || p.GetDataPreparationAction().GetContentsSql().GetLoadConfig().GetAppend() == nil {
		t.Fatalf("data preparation = %v", p)
	}
	joined := strings.Join(w.exec, "\n")
	if !strings.Contains(joined, "create table if not exists `p.ds.prep_errors`") || !strings.Contains(joined, "|> WHERE a > 0") {
		t.Errorf("SQL run = %s", joined)
	}
	// A cell that raises fails the action, its dependents skipped.
	nb.fail = true
	inv, err = s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo, WorkflowInvocation: &dfpb.WorkflowInvocation{
		CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	got, acts = waitInvocation(t, s, inv.GetName())
	if n := byName(acts)["nb"]; n.GetState() != dfpb.WorkflowInvocationAction_FAILED || !strings.Contains(n.GetFailureReason(), "ValueError") || got.GetState() != dfpb.WorkflowInvocation_FAILED {
		t.Fatalf("a failing notebook = %v, invocation %v", n, got.GetState())
	}
}

// TestPipelineConfig: DATAFORM compiles the path's folder; NOTEBOOK and
// SQL compile the one file without Dataform core; the invocation reports
// the compilation's pipeline.
func TestPipelineConfig(t *testing.T) {
	s, r, _, nb, _, ws := pipelineServer(t)
	ctx := context.Background()
	repo := strings.SplitN(ws, "/workspaces/", 2)[0]
	compile := func(p *dfpb.PipelineConfig) (*dfpb.CompilationResult, error) {
		return s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo, CompilationResult: &dfpb.CompilationResult{
			Source: &dfpb.CompilationResult_Workspace{Workspace: ws}, CodeCompilationConfig: &dfpb.CodeCompilationConfig{PipelineConfig: p}}})
	}
	for _, bad := range []*dfpb.PipelineConfig{{Path: "q.sql"}, {PipelineType: dfpb.PipelineConfig_SQL}, {PipelineType: dfpb.PipelineConfig_SQL, Path: "../x.sql"}} {
		if _, err := compile(bad); code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	if _, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_DATAFORM, Path: "nowhere"}); code(err) != codes.InvalidArgument {
		t.Errorf("a DATAFORM path without workflow_settings.yaml: %v", err)
	}
	before := len(r.dirs)
	if _, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_DATAFORM, Path: "sub"}); err != nil {
		t.Fatal(err)
	}
	if len(r.dirs) != before+1 || filepath.Base(r.dirs[len(r.dirs)-1]) != "sub" {
		t.Errorf("DATAFORM compiled %v", r.dirs)
	}
	c, err := compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_NOTEBOOK, Path: "content.ipynb"})
	if err != nil {
		t.Fatal(err)
	}
	acts, _ := s.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: c.GetName()})
	if len(acts.GetCompilationResultActions()) != 1 {
		t.Fatalf("NOTEBOOK actions = %v", acts)
	}
	a := acts.GetCompilationResultActions()[0]
	if a.GetTarget().GetName() != "content" || a.GetTarget().GetDatabase() != "p" || !strings.Contains(a.GetNotebook().GetContents(), `"outputs":[]`) || strings.Contains(a.GetNotebook().GetContents(), "execution_count\":3") {
		t.Errorf("NOTEBOOK action = %v", a)
	}
	inv, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo, WorkflowInvocation: &dfpb.WorkflowInvocation{
		CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: c.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	if inv.GetPipelineConfig().GetPipelineType() != dfpb.PipelineConfig_NOTEBOOK {
		t.Errorf("invocation pipeline_config = %v", inv.GetPipelineConfig())
	}
	if got, _ := waitInvocation(t, s, inv.GetName()); got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || len(nb.ran) != 1 {
		t.Errorf("NOTEBOOK invocation = %v", got.GetState())
	}
	c, err = compile(&dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_SQL, Path: "q.sql"})
	if err != nil {
		t.Fatal(err)
	}
	acts, _ = s.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: c.GetName()})
	if q := acts.GetCompilationResultActions()[0].GetOperations().GetQueries(); len(q) != 1 || q[0] != "select 7" {
		t.Errorf("SQL action = %v", acts)
	}
	if _, err := s.InstallNpmPackages(ctx, &dfpb.InstallNpmPackagesRequest{Workspace: ws, PipelineConfig: &dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_SQL, Path: "q.sql"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("npm install of a SQL asset: %v", err)
	}
}

func TestCompareRows(t *testing.T) {
	a, b := "1", "2"
	for _, c := range []struct {
		cols, ecols []string
		rows, erows [][]*string
		want        string
	}{
		{[]string{"a"}, []string{"a"}, [][]*string{{&a}}, [][]*string{{&a}}, ""},
		{[]string{"a"}, []string{"a"}, [][]*string{{&a}}, nil, "Expected 0 rows, but saw 1 rows."},
		{[]string{"a"}, []string{"b"}, [][]*string{{&a}}, [][]*string{{&a}}, `Expected columns "b", but saw "a".`},
		{[]string{"a"}, []string{"a"}, [][]*string{{nil}}, [][]*string{{&b}}, `For row 0 and column "a": expected "2", but saw null.`},
	} {
		got := strings.Join(compareRows(c.cols, c.rows, c.ecols, c.erows), "\n")
		if got != c.want {
			t.Errorf("compareRows = %q, want %q", got, c.want)
		}
	}
}

// TestSSHRemotes: an SSH remote's URL and authentication config are
// checked, and git gets an ssh command with that key and host key alone.
func TestSSHRemotes(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKNqSx03FGDwlcmm6O0orPmVNFZVZjHcJPJbugEDbAtM"
	good := &dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig{UserPrivateKeySecretVersion: "projects/p/secrets/k/versions/1", HostPublicKey: key}
	for _, c := range []struct {
		url  string
		cfg  *dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig
		want codes.Code
	}{
		{"ssh://git@127.0.0.1:2222/r.git", good, codes.OK},
		{"git@localhost:r.git", good, codes.OK},
		{"ssh://git@example.com/r.git", good, codes.InvalidArgument},
		{"https://127.0.0.1/r.git", good, codes.InvalidArgument},
		{"ssh://git@127.0.0.1/r.git", nil, codes.InvalidArgument},
		{"ssh://git@127.0.0.1/r.git", &dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig{UserPrivateKeySecretVersion: "k", HostPublicKey: key}, codes.InvalidArgument},
		{"ssh://git@127.0.0.1/r.git", &dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig{UserPrivateKeySecretVersion: good.UserPrivateKeySecretVersion, HostPublicKey: "nope"}, codes.InvalidArgument},
	} {
		err := checkSSH(&dfpb.Repository_GitRemoteSettings{Url: c.url, SshAuthenticationConfig: c.cfg})
		got := codes.OK
		if err != nil {
			got = code(err)
		}
		if got != c.want {
			t.Errorf("%s: %v, want %v", c.url, err, c.want)
		}
	}
	s, err := NewServer(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conf, err := s.sshConf("projects/p/locations/l/repositories/r", "ssh://git@127.0.0.1:2222/r.git", good, []byte("PRIVATE"))
	if err != nil {
		t.Fatal(err)
	}
	if len(conf) != 1 || !strings.Contains(conf[0], "StrictHostKeyChecking=yes") || !strings.Contains(conf[0], "IdentitiesOnly=yes") {
		t.Fatalf("conf = %v", conf)
	}
	matches, _ := filepath.Glob(filepath.Join(s.opts.DataDir, "ssh", "*", "known_hosts"))
	if len(matches) != 1 {
		t.Fatalf("known_hosts files = %v", matches)
	}
	b, _ := os.ReadFile(matches[0])
	if string(b) != "[127.0.0.1]:2222 "+key+"\n" {
		t.Errorf("known_hosts = %q", b)
	}
	st, _ := os.Stat(filepath.Join(filepath.Dir(matches[0]), "id"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v", st.Mode())
	}
}

func TestStripOutputs(t *testing.T) {
	out, err := stripOutputs([]byte(`{"cells":[{"cell_type":"code","outputs":[1],"execution_count":2},{"cell_type":"markdown","source":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var nb map[string]any
	_ = json.Unmarshal(out, &nb)
	c := nb["cells"].([]any)[0].(map[string]any)
	if len(c["outputs"].([]any)) != 0 || c["execution_count"] != nil {
		t.Errorf("stripped = %s", out)
	}
}

// TestMergeLoadModeIsAnError: Dataform core's MERGE load mode has no API
// LoadConfig, so it is a compilation error rather than a replace.
func TestMergeLoadModeIsAnError(t *testing.T) {
	var g dfGraph
	if err := json.Unmarshal([]byte(`{"dataPreparations":[{"target":{"database":"p","schema":"d","name":"m"},"fileName":"definitions/m.sqlx",
		"query":"FROM t","load":{"merge":{"uniqueKey":["id"]}}}]}`), &g); err != nil {
		t.Fatal(err)
	}
	errs := g.errors("p")
	if len(errs) != 1 || errs[0].GetPath() != "definitions/m.sqlx" || !strings.Contains(errs[0].GetMessage(), "MERGE") {
		t.Errorf("errors = %v", errs)
	}
}
