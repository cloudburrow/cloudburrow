package dataproc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

type fakeEngine struct {
	addr  string // the sessions' Spark Connect server
	mu    sync.Mutex
	jobs  []sparkproc.Job
	res   sparkproc.Result
	block chan struct{}
}

func (f *fakeEngine) Run(ctx context.Context, job sparkproc.Job) (sparkproc.Result, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return sparkproc.Result{}, ctx.Err()
		}
	}
	return f.res, nil
}

type fakeConn struct {
	addr string
	done chan struct{}
	once sync.Once
}

func (c *fakeConn) Address() string          { return c.addr }
func (c *fakeConn) Done() <-chan struct{}    { return c.done }
func (c *fakeConn) Result() sparkproc.Result { return sparkproc.Result{} }
func (c *fakeConn) Stop()                    { c.once.Do(func() { close(c.done) }) }
func (f *fakeEngine) StartConnect(context.Context, sparkproc.Job) (Conn, error) {
	addr := f.addr
	if addr == "" {
		addr = "127.0.0.1:1"
	}
	return &fakeConn{addr: addr, done: make(chan struct{})}, nil
}

type fakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeStorage) Read(_ context.Context, uri string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[uri]
	if !ok {
		return nil, status.Error(codes.NotFound, "no object")
	}
	return b, nil
}

func (f *fakeStorage) Write(_ context.Context, uri string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[uri] = data
	return nil
}

func (f *fakeStorage) EnsureBucket(context.Context, string, string, string) error { return nil }

func wait(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

const parent = "projects/p/locations/us-central1"

// TestBatchLifecycle: a PySpark batch runs with its file and arguments,
// ends SUCCEEDED with its state history, output and usage, and its
// operation's response is the batch; a running batch cannot be deleted,
// a cancelled one ends CANCELLED; filters and IDs are checked.
func TestBatchLifecycle(t *testing.T) {
	eng := &fakeEngine{res: sparkproc.Result{DriverOutput: "hello\n"}}
	st := &fakeStorage{objects: map[string][]byte{"gs://b/job.py": []byte("print('hi')")}}
	s, err := NewServer(Options{Engine: eng, Storage: st, ProjectNumber: func(string) string { return "123" }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, err := s.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "batch-1", Batch: &dataprocpb.Batch{
		BatchConfig:   &dataprocpb.Batch_PysparkBatch{PysparkBatch: &dataprocpb.PySparkBatch{MainPythonFileUri: "gs://b/job.py", Args: []string{"x"}}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Properties: map[string]string{"spark.executor.instances": "3"}}, Labels: map[string]string{"env": "dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(op.Name, "projects/p/regions/us-central1/operations/") {
		t.Errorf("operation name %s", op.Name)
	}
	ops := &operations{s: s}
	wait(t, func() bool {
		o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name})
		return o.GetDone()
	})
	b, err := s.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: parent + "/batches/batch-1"})
	if err != nil || b.State != dataprocpb.Batch_SUCCEEDED || len(b.StateHistory) != 2 || b.RuntimeConfig.Version != DefaultRuntime {
		t.Fatalf("batch: %v %v", b, err)
	}
	if eng.jobs[0].Kind != sparkproc.KindPySpark || string(eng.jobs[0].Main.Data) != "print('hi')" || eng.jobs[0].Args[0] != "x" ||
		eng.jobs[0].Log.Type != "cloud_dataproc_batch" || eng.jobs[0].Log.Labels["batch_id"] != "batch-1" {
		t.Errorf("job: %+v", eng.jobs[0])
	}
	if out := string(st.objects[b.RuntimeInfo.OutputUri+".000000000"]); out != "hello\n" || !strings.HasPrefix(b.RuntimeInfo.OutputUri, "gs://dataproc-staging-us-central1-123-") {
		t.Errorf("output %s: %q", b.RuntimeInfo.OutputUri, out)
	}
	if b.RuntimeInfo.ApproximateUsage.GetMilliDcuSeconds() < 16000 {
		t.Errorf("usage %v", b.RuntimeInfo.ApproximateUsage)
	}
	o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: strings.Replace(op.Name, "/regions/", "/locations/", 1)})
	var got dataprocpb.Batch
	if err := o.GetResponse().UnmarshalTo(&got); err != nil || got.Name != b.Name {
		t.Errorf("operation response: %v %v", got.Name, err)
	}

	// A duplicate, a bad ID, a bad workload.
	if _, err := s.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "batch-1", Batch: b}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "B!", Batch: b}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad id: %v", err)
	}
	if _, err := s.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, Batch: &dataprocpb.Batch{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no workload: %v", err)
	}

	// A running batch: delete refused, cancel ends it CANCELLED.
	eng.block = make(chan struct{})
	op2, err := s.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "batch-2", Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_SparkSqlBatch{SparkSqlBatch: &dataprocpb.SparkSqlBatch{QueryFileUri: "file:///q.sql", QueryVariables: map[string]string{"a": "1"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	name2 := parent + "/batches/batch-2"
	wait(t, func() bool {
		b, _ := s.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: name2})
		return b.State == dataprocpb.Batch_RUNNING
	})
	if _, err := s.DeleteBatch(ctx, &dataprocpb.DeleteBatchRequest{Name: name2}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete running: %v", err)
	}
	if _, err := ops.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op2.Name}); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		b, _ := s.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: name2})
		return b.State == dataprocpb.Batch_CANCELLED
	})
	o, _ = ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op2.Name})
	if o.GetError().GetCode() != int32(codes.Canceled) {
		t.Errorf("cancelled operation: %v", o)
	}

	// Filters: state, batch_id, labels.
	for filter, want := range map[string]int{`state = SUCCEEDED`: 1, `batch_id = "batch-2"`: 1, `labels.env = dev`: 1, `state = RUNNING`: 0, ``: 2} {
		resp, err := s.ListBatches(ctx, &dataprocpb.ListBatchesRequest{Parent: parent, Filter: filter})
		if err != nil || len(resp.Batches) != want {
			t.Errorf("filter %q: %d %v", filter, len(resp.GetBatches()), err)
		}
	}
	if _, err := s.ListBatches(ctx, &dataprocpb.ListBatchesRequest{Parent: parent, OrderBy: "name"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("order_by name: %v", err)
	}
	if _, err := s.DeleteBatch(ctx, &dataprocpb.DeleteBatchRequest{Name: name2}); err != nil {
		t.Errorf("delete cancelled: %v", err)
	}
}

// TestSessionsAndTemplates: a session made from a template is ACTIVE with
// its Spark Connect endpoint, terminates, and is deleted; idle_ttl is
// checked.
func TestSessionsAndTemplates(t *testing.T) {
	s, err := NewServer(Options{Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := WithAuthority(context.Background(), "127.0.0.1:9066")
	tmpl, err := s.CreateSessionTemplate(ctx, &dataprocpb.CreateSessionTemplateRequest{Parent: parent, SessionTemplate: &dataprocpb.SessionTemplate{
		Name: parent + "/sessionTemplates/connect-30", SessionConfig: &dataprocpb.SessionTemplate_SparkConnectSession{SparkConnectSession: &dataprocpb.SparkConnectConfig{}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "3.0"}, Labels: map[string]string{"t": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "sess-1", Session: &dataprocpb.Session{
		SessionTemplate: tmpl.Name, EnvironmentConfig: &dataprocpb.EnvironmentConfig{ExecutionConfig: &dataprocpb.ExecutionConfig{IdleTtl: durationpb.New(time.Minute)}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("idle_ttl of a minute: %v", err)
	}
	op, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "sess-1", Session: &dataprocpb.Session{SessionTemplate: tmpl.Name}})
	if err != nil {
		t.Fatal(err)
	}
	ops := &operations{s: s}
	wait(t, func() bool {
		o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name})
		return o.GetDone()
	})
	x, err := s.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: parent + "/sessions/sess-1"})
	if err != nil || x.State != dataprocpb.Session_ACTIVE || x.RuntimeInfo.Endpoints[ConnectEndpoint] != "sc://127.0.0.1:9066" ||
		x.RuntimeConfig.Version != "3.0" || x.Labels["t"] != "1" {
		t.Fatalf("session: %v %v", x, err)
	}
	if _, _, err := s.connectFor(x.Uuid); err != nil {
		t.Errorf("connectFor: %v", err)
	}
	top, err := s.TerminateSession(ctx, &dataprocpb.TerminateSessionRequest{Name: x.Name})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: top.Name})
		return o.GetDone()
	})
	x, _ = s.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: x.Name})
	if x.State != dataprocpb.Session_TERMINATED || len(x.RuntimeInfo.GetEndpoints()) != 0 {
		t.Errorf("terminated: %v", x)
	}
	dop, err := s.DeleteSession(ctx, &dataprocpb.DeleteSessionRequest{Name: x.Name})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: dop.Name})
		return o.GetDone()
	})
	if _, err := s.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: x.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted: %v", err)
	}
	if _, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "sess-2", Session: &dataprocpb.Session{
		SessionConfig: &dataprocpb.Session_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{Kernel: dataprocpb.JupyterConfig_SCALA}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("scala: %v", err)
	}
	jop, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "sess-3", Session: &dataprocpb.Session{
		SessionConfig: &dataprocpb.Session_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{DisplayName: "nb"}}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: jop.Name})
		return o.GetDone()
	})
	j, _ := s.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: parent + "/sessions/sess-3"})
	if j.GetState() != dataprocpb.Session_ACTIVE || j.GetJupyterSession().GetKernel() != dataprocpb.JupyterConfig_PYTHON {
		t.Errorf("jupyter: %v", j)
	}
	if u, err := s.KernelConnectURI(j.Name, "host.docker.internal:8081"); err != nil || u != "sc://host.docker.internal:8081/;session_id="+j.Uuid {
		t.Errorf("kernel URI %q %v", u, err)
	}
}

func TestSessionIDOf(t *testing.T) {
	b := protowire.AppendTag(nil, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, 7)
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, "uuid-1")
	if got := sessionIDOf(b); got != "uuid-1" {
		t.Errorf("sessionIDOf = %q", got)
	}
}
