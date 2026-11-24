package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestExecutionResumesAfterRestart stops the server while an execution
// sleeps between two HTTP calls; a new server on the same state carries it
// on: the first call is not sent again, sys.now answers what it answered,
// and the sleep only lasts what was left of it.
func TestExecutionResumesAfterRestart(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"n": %d}`, n)
	}))
	defer backend.Close()
	state := filepath.Join(t.TempDir(), "workflows.json")
	s, err := NewServer(state, Options{})
	if err != nil {
		t.Fatal(err)
	}
	wc, ec, _ := startServer(t, s)
	src := `main:
  steps:
    - first:
        call: http.get
        args: {url: "` + backend.URL + `/a"}
        result: a
    - t0:
        assign:
          - t: ${sys.now()}
          - id: ${uuid.generate()}
    - wait:
        call: sys.sleep
        args: {seconds: 3}
    - second:
        call: http.get
        args: {url: "` + backend.URL + `/b"}
        result: b
    - done:
        return: {a: "${a.body.n}", b: "${b.body.n}", t: "${t}", id: "${id}"}
`
	w := deploy(t, wc, "resume", src)
	ex, err := ec.CreateExecution(context.Background(), &executionspb.CreateExecutionRequest{Parent: w.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	// Stop the server once the sleep is under way: its end is journaled,
	// and a second of it has passed. Waiting on the journal rather than on
	// a fixed delay after the first call keeps a loaded machine from
	// stopping the execution before it reached the sleep, which would
	// rightly sleep in full after the restart.
	sleeping := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		er := s.execs[ex.GetName()]
		if er == nil || er.m == nil {
			return false
		}
		for _, e := range er.m.Journal() {
			if e.Kind == "sleep" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for !sleeping() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sleeping() {
		t.Fatal("the execution never reached its sleep")
	}
	time.Sleep(time.Second)
	stoppedAt := time.Now()
	s.Close()
	s.mu.Lock()
	j := s.execs[ex.GetName()].journal
	st := s.execs[ex.GetName()].Ex.GetState()
	s.mu.Unlock()
	if st != executionspb.Execution_ACTIVE || len(j) < 4 {
		t.Fatalf("after stop: %v, journal %v", st, j)
	}
	var t0 string
	var sleepEnd time.Time
	for _, e := range j {
		switch e.Kind {
		case "now":
			t0 = string(e.Value)
		case "sleep":
			v, err := decodeTyped(e.Value)
			end, ok := v.(int64)
			if err != nil || !ok {
				t.Fatalf("sleep journal %s: %v", e.Value, err)
			}
			sleepEnd = time.Unix(0, end)
		}
	}

	s2, err := NewServer(state, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, ec2, _ := startServer(t, s2)
	s2.Resume()
	s2.Wait(ex.GetName())
	got, err := ec2.GetExecution(context.Background(), &executionspb.GetExecutionRequest{Name: ex.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("resumed execution %v", got)
	}
	if took := time.Since(stoppedAt); took >= 3*time.Second {
		t.Errorf("the resumed sleep took %v: it slept again in full", took)
	}
	if early := time.Until(sleepEnd); sleepEnd.IsZero() || early > 0 {
		t.Errorf("the resumed execution ended %v before its sleep did", early)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(got.GetResult()), &res)
	if res["a"] != float64(1) || res["b"] != float64(2) || hits.Load() != 2 {
		t.Errorf("result %s, %d requests: the first call was sent again", got.GetResult(), hits.Load())
	}
	if !strings.Contains(t0, "f") {
		t.Errorf("sys.now journal %s", t0)
	}
}

type logEntry struct {
	logID, severity string
	payload         map[string]any
}

type logSink struct {
	mu      sync.Mutex
	entries []logEntry
}

func (l *logSink) log(_, _, _, _, logID, severity string, payload any) {
	m, _ := payload.(map[string]any)
	l.mu.Lock()
	l.entries = append(l.entries, logEntry{logID, severity, m})
	l.mu.Unlock()
}

func (l *logSink) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if e.payload["@type"] == engineCallLogType {
			out = append(out, fmt.Sprint(e.payload["state"], " ", e.payload["step"], " ", e.payload["callee"]))
		}
	}
	return out
}

func TestCallLogLevels(t *testing.T) {
	src := `main:
  steps:
    - s1:
        call: sub
        args: {x: 2}
        result: r
    - s2:
        try:
          call: text.to_upper
          args: {source: 7}
        except:
          as: e
          steps:
            - ok:
                assign:
                  - c: 1
    - s3:
        call: http.get
        args: {url: "http://127.0.0.1:1/x", headers: {Authorization: "Bearer secret"}, timeout: 1}
        result: h
    - s4:
        return: ${r}
sub:
  params: [x]
  steps:
    - in:
        try:
          raise: inner
        except:
          as: e
          steps:
            - back:
                return: ${x * 2}
`
	for _, level := range []executionspb.Execution_CallLogLevel{executionspb.Execution_LOG_ALL_CALLS,
		executionspb.Execution_LOG_ERRORS_ONLY, executionspb.Execution_LOG_NONE} {
		sink := &logSink{}
		s, err := NewServer("", Options{Log: sink.log})
		if err != nil {
			t.Fatal(err)
		}
		wc, ec, _ := startServer(t, s)
		w := deploy(t, wc, "logs-"+strings.ToLower(strings.ReplaceAll(level.String(), "_", "-")), src)
		ex, err := ec.CreateExecution(context.Background(), &executionspb.CreateExecutionRequest{Parent: w.GetName(),
			Execution: &executionspb.Execution{CallLogLevel: level}})
		if err != nil {
			t.Fatal(err)
		}
		s.Wait(ex.GetName())
		time.Sleep(50 * time.Millisecond)
		got := strings.Join(sink.calls(), "\n")
		var want string
		switch level {
		case executionspb.Execution_LOG_ALL_CALLS:
			want = "BEGUN s1 sub\nEXCEPTION_HANDLED s1 sub\nSUCCEEDED s1 sub\nBEGUN s2 text.to_upper\nEXCEPTION_RAISED s2 text.to_upper\nBEGUN s3 http.get\nEXCEPTION_RAISED s3 http.get"
		case executionspb.Execution_LOG_ERRORS_ONLY:
			want = "EXCEPTION_RAISED s2 text.to_upper\nEXCEPTION_RAISED s3 http.get"
		}
		if got != want {
			t.Errorf("%v: call logs\n%s\nwant\n%s", level, got, want)
		}
		sink.mu.Lock()
		for _, e := range sink.entries {
			if e.payload["state"] == "BEGUN" && e.payload["step"] == "s3" {
				b, _ := json.Marshal(e.payload)
				if strings.Contains(string(b), "secret") {
					t.Errorf("Authorization not redacted: %s", b)
				}
			}
			if e.payload["@type"] == engineCallLogType && (e.logID != SystemLogID || e.payload["executionId"] == "") {
				t.Errorf("entry %+v", e)
			}
		}
		sink.mu.Unlock()
		s.Close()
	}
}

func TestExperimentalExecutions(t *testing.T) {
	s, err := NewServer("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wc, ec, _ := startServer(t, s)
	deploy(t, wc, "child", "main:\n  params: [n]\n  steps:\n    - c:\n        switch:\n          - condition: ${n == 0}\n            raise: zero\n    - r:\n        return: ${n * 10}\n")
	w := deploy(t, wc, "parent", `main:
  steps:
    - one:
        call: experimental.executions.run
        args: {workflow_id: child, argument: 4}
        result: a
    - many:
        call: experimental.executions.map
        args: {workflow_id: child, arguments: [1, 2, 3]}
        result: b
    - bad:
        try:
          call: experimental.executions.run
          args: {workflow_id: child, argument: 0}
        except:
          as: e
          steps:
            - keep:
                assign:
                  - tag: ${e.tags[0]}
    - r:
        return: {a: "${a}", b: "${b}", tag: "${tag}"}
`)
	got := runAndWait(t, s, ec, w.GetName(), "")
	if got.GetState() != executionspb.Execution_SUCCEEDED || got.GetResult() != `{"a":40,"b":[10,20,30],"tag":"ExecutionError"}` {
		t.Fatalf("parent %v", got)
	}
	// Five child executions were made.
	it := ec.ListExecutions(context.Background(), &executionspb.ListExecutionsRequest{Parent: parent + "/workflows/child"})
	n := 0
	for {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 5 {
		t.Errorf("%d child executions", n)
	}
}

// fakeKMS seals with a reversible transform and fails while disabled.
type fakeKMS struct {
	disabled atomic.Bool
}

func (k *fakeKMS) Encrypt(_ context.Context, r *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	if !strings.Contains(r.GetName(), "/cryptoKeys/") || strings.HasSuffix(r.GetName(), "/missing") {
		return nil, status.Error(codes.NotFound, "no key")
	}
	if k.disabled.Load() {
		return nil, status.Error(codes.FailedPrecondition, "the primary version is DISABLED")
	}
	return &kmspb.EncryptResponse{Name: r.GetName() + "/cryptoKeyVersions/1", Ciphertext: append([]byte("x"), r.GetPlaintext()...)}, nil
}

func (k *fakeKMS) Decrypt(_ context.Context, r *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	if k.disabled.Load() {
		return nil, status.Error(codes.FailedPrecondition, "projects/p/.../cryptoKeyVersions/1 is not enabled, current state is: DISABLED")
	}
	return &kmspb.DecryptResponse{Plaintext: r.GetCiphertext()[1:]}, nil
}

func TestCMEK(t *testing.T) {
	old := KeyCheckInterval
	KeyCheckInterval = 50 * time.Millisecond
	defer func() { KeyCheckInterval = old }()
	k := &fakeKMS{}
	s, err := NewServer("", Options{KMS: func() KMS { return k }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wc, ec, _ := startServer(t, s)
	ctx := context.Background()
	key := "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k"
	create := func(id, key string) (*workflowspb.Workflow, error) {
		op, err := wc.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: id,
			Workflow: &workflowspb.Workflow{CryptoKeyName: key, SourceCode: &workflowspb.Workflow_SourceContents{
				SourceContents: "main:\n  params: [a]\n  steps:\n    - s:\n        call: sys.sleep\n        args: {seconds: \"${a}\"}\n    - r:\n        return: ok\n"}}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}
	if _, err := create("wrongregion", "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/k"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key in another region: %v", err)
	}
	if _, err := create("missing", "projects/p/locations/us-central1/keyRings/r/cryptoKeys/missing"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a missing key: %v", err)
	}
	w, err := create("sealed", key)
	if err != nil {
		t.Fatal(err)
	}
	if w.GetCryptoKeyName() != key || w.GetCryptoKeyVersion() != key+"/cryptoKeyVersions/1" || len(w.GetAllKmsKeys()) != 1 {
		t.Errorf("workflow %v", w)
	}
	if got := runAndWait(t, s, ec, w.GetName(), "0"); got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("with the key enabled: %v", got)
	}
	long, err := ec.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: w.GetName(), Execution: &executionspb.Execution{Argument: "30"}})
	if err != nil {
		t.Fatal(err)
	}
	k.disabled.Store(true)
	_, err = ec.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: w.GetName(), Execution: &executionspb.Execution{Argument: "0"}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Key version is not enabled") {
		t.Errorf("a new execution with the key disabled: %v", err)
	}
	done := make(chan struct{})
	go func() { s.Wait(long.GetName()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the active execution did not fail when its key was disabled")
	}
	got, _ := ec.GetExecution(ctx, &executionspb.GetExecutionRequest{Name: long.GetName()})
	if got.GetState() != executionspb.Execution_FAILED || !strings.Contains(got.GetError().GetContext(), "Key version is not enabled") {
		t.Errorf("active execution: %v", got)
	}
	k.disabled.Store(false)
	if got := runAndWait(t, s, ec, w.GetName(), "0"); got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Errorf("re-enabled: %v", got)
	}
	// Without Cloud KMS a key is refused.
	s3, _ := NewServer("", Options{})
	defer s3.Close()
	wc3, _, _ := startServer(t, s3)
	op, err := wc3.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: "nokms",
		Workflow: &workflowspb.Workflow{CryptoKeyName: key, SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: "- r:\n    return: 1\n"}}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no KMS: %v", err)
	}
	_ = errors.New
}
