package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ExecutionsAPI is google.cloud.workflows.executions.v1.Executions.
type ExecutionsAPI struct {
	executionspb.UnimplementedExecutionsServer
	s *Server
}

// Executions is the Workflow Executions API.
func (s *Server) Executions() *ExecutionsAPI { return &ExecutionsAPI{s: s} }

// runtime is an execution's view of the instance.
type runtime struct {
	h   *HTTPCaller
	log func(severity string, payload any)
}

func (r *runtime) HTTP() *HTTPCaller                { return r.h }
func (r *runtime) Log(severity string, payload any) { r.log(severity, payload) }

func (a *ExecutionsAPI) CreateExecution(_ context.Context, req *executionspb.CreateExecutionRequest) (*executionspb.Execution, error) {
	return a.s.Start(req.GetParent(), req.GetExecution())
}

// Start creates an execution of the workflow's current revision and runs
// it. Eventarc's workflow destination starts its executions here.
func (s *Server) Start(parent string, in *executionspb.Execution) (*executionspb.Execution, error) {
	m := workflowRE.FindStringSubmatch(parent)
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}/workflows/{workflow}", parent)
	}
	if in == nil {
		in = &executionspb.Execution{}
	}
	if len(in.GetArgument()) > MaxMemoryBytes {
		return nil, apierror.InvalidArgument("the argument is %d bytes; the limit is %d", len(in.GetArgument()), MaxMemoryBytes)
	}
	if a := strings.TrimSpace(in.GetArgument()); a != "" {
		var v any
		if err := json.Unmarshal([]byte(a), &v); err != nil {
			return nil, apierror.InvalidArgument("argument must be a JSON string: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.wfs[parent]
	if rec == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", parent)
	}
	wf := clone(rec.WF)
	prog, err := Parse(wf.GetSourceContents())
	if err != nil {
		return nil, apierror.FailedPrecondition("the workflow's source no longer parses: %v", err)
	}
	now := s.now()
	name := parent + "/executions/" + newUUID()
	ex := &executionspb.Execution{Name: name, StartTime: timestamppb.New(now), State: executionspb.Execution_ACTIVE,
		Argument: in.GetArgument(), WorkflowRevisionId: wf.GetRevisionId(), CallLogLevel: in.GetCallLogLevel(),
		Labels: in.GetLabels(), Status: &executionspb.Execution_Status{}}
	if ex.CallLogLevel == executionspb.Execution_CALL_LOG_LEVEL_UNSPECIFIED {
		ex.CallLogLevel = executionspb.Execution_CallLogLevel(wf.GetCallLogLevel())
	}
	parts := strings.Split(name, "/")
	project, location, workflow, id := parts[1], parts[3], parts[5], parts[7]
	env := map[string]any{
		"GOOGLE_CLOUD_PROJECT_ID": project, "GOOGLE_CLOUD_PROJECT_NUMBER": s.projectNumber(project),
		"GOOGLE_CLOUD_LOCATION": location, "GOOGLE_CLOUD_WORKFLOW_ID": workflow,
		"GOOGLE_CLOUD_WORKFLOW_REVISION_ID": wf.GetRevisionId(), "GOOGLE_CLOUD_WORKFLOW_EXECUTION_ID": id,
	}
	for k, v := range wf.GetUserEnvVars() {
		env[k] = v
	}
	sa := wf.GetServiceAccount()
	if i := strings.LastIndex(sa, "/serviceAccounts/"); i >= 0 {
		sa = sa[i+len("/serviceAccounts/"):]
	}
	ctx, cancel := context.WithCancel(s.ctx)
	logf := func(severity string, payload any) {
		if s.opts.Log != nil {
			s.opts.Log(project, location, workflow, id, severity, payload)
		}
	}
	rt := &runtime{h: &HTTPCaller{Client: NewHTTPClient(), Local: s.opts.Local, Tokens: s.opts.Tokens, ServiceAccount: sa, Ingress: s.opts.Ingress}, log: logf}
	cbURL := func(n string) string {
		addr := "localhost"
		if s.opts.Addr != nil && s.opts.Addr() != "" {
			addr = s.opts.Addr()
		}
		return "http://" + addr + "/v1/" + n
	}
	mach := newMachine(ctx, prog, rt, env, name, cbURL)
	mach.now = s.now
	er := &executionRecord{Ex: ex, m: mach, cancel: cancel, done: make(chan struct{})}
	s.execs[name] = er
	if err := s.save(); err != nil {
		cancel()
		delete(s.execs, name)
		return nil, apierror.Wrap(err)
	}
	s.wg.Add(1)
	go s.run(er, in.GetArgument())
	return clone(ex), nil
}

func (s *Server) run(er *executionRecord, argument string) {
	defer s.wg.Done()
	defer close(er.done)
	result, err := er.m.run(argument)
	er.m.callbacks.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := er.Ex
	end := s.now()
	ex.EndTime = timestamppb.New(end)
	ex.Duration = durationpb.New(end.Sub(ex.GetStartTime().AsTime()))
	switch {
	case err == nil:
		ex.State = executionspb.Execution_SUCCEEDED
		ex.Result = result
	case errors.Is(err, context.Canceled):
		if ex.State != executionspb.Execution_CANCELLED {
			// The instance is stopping: it is recorded as interrupted.
			ex.State = executionspb.Execution_FAILED
			ex.Error = &executionspb.Execution_Error{Payload: `"the CloudBurrow instance stopped while the execution ran"`,
				Context: "RuntimeError: the CloudBurrow instance stopped while the execution ran"}
		}
	default:
		ex.State = executionspb.Execution_FAILED
		payload, ctxText := errorPayload(err)
		ex.Error = &executionspb.Execution_Error{Payload: payload, Context: ctxText}
		var we *wfError
		if errors.As(err, &we) && we.step != "" {
			ex.Error.StackTrace = &executionspb.Execution_StackTrace{Elements: []*executionspb.Execution_StackTraceElement{{
				Step: we.step, Routine: we.routine,
				Position: &executionspb.Execution_StackTraceElement_Position{Line: int64(we.line), Column: int64(we.col)}}}}
		}
	}
	ex.Status = &executionspb.Execution_Status{}
	er.History = er.m.History()
	er.m = nil
	er.cancel = nil
	if s.execs[ex.GetName()] == er {
		_ = s.save()
	}
}

// view is an execution as a view shows it: BASIC leaves out the argument,
// the result and the error.
func (s *Server) view(er *executionRecord, v executionspb.ExecutionView) *executionspb.Execution {
	ex := clone(er.Ex)
	if er.m != nil && ex.GetState() == executionspb.Execution_ACTIVE {
		routine, step := er.m.Current()
		if step != "" {
			ex.Status = &executionspb.Execution_Status{CurrentSteps: []*executionspb.Execution_Status_Step{{Routine: routine, Step: step}}}
		}
		ex.Duration = durationpb.New(s.now().Sub(ex.GetStartTime().AsTime()))
	}
	if v == executionspb.ExecutionView_BASIC {
		ex.Argument, ex.Result, ex.Error, ex.Status = "", "", nil, nil
	}
	return ex
}

func (a *ExecutionsAPI) GetExecution(_ context.Context, req *executionspb.GetExecutionRequest) (*executionspb.Execution, error) {
	s := a.s
	if !executionRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/workflows/{workflow}/executions/{execution}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	er := s.execs[req.GetName()]
	if er == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	v := req.GetView()
	if v == executionspb.ExecutionView_EXECUTION_VIEW_UNSPECIFIED {
		v = executionspb.ExecutionView_FULL
	}
	return s.view(er, v), nil
}

func (a *ExecutionsAPI) ListExecutions(_ context.Context, req *executionspb.ListExecutionsRequest) (*executionspb.ListExecutionsResponse, error) {
	s := a.s
	parent := req.GetParent()
	if !workflowRE.MatchString(parent) && !strings.HasSuffix(parent, "/workflows/-") {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}/workflows/{workflow}", parent)
	}
	prefix := parent + "/executions/"
	if strings.HasSuffix(parent, "/workflows/-") {
		prefix = strings.TrimSuffix(parent, "-")
	}
	v := req.GetView()
	if v == executionspb.ExecutionView_EXECUTION_VIEW_UNSPECIFIED {
		v = executionspb.ExecutionView_BASIC
	}
	s.mu.Lock()
	if !strings.HasSuffix(parent, "/workflows/-") && s.wfs[parent] == nil {
		s.mu.Unlock()
		return nil, apierror.NotFound("Resource '%s' was not found", parent)
	}
	var items []*executionspb.Execution
	for _, name := range sortedNames(s.execs) {
		if strings.HasPrefix(name, prefix) {
			items = append(items, s.view(s.execs[name], executionspb.ExecutionView_FULL))
		}
	}
	s.mu.Unlock()
	order := req.GetOrderBy()
	if order == "" {
		// Newest first, as Google lists them.
		order = "startTime desc"
	}
	items, err := aipfilter.List(req.GetFilter(), order, items)
	if err != nil {
		return nil, err
	}
	if v == executionspb.ExecutionView_BASIC {
		for _, ex := range items {
			ex.Argument, ex.Result, ex.Error, ex.Status = "", "", nil, nil
		}
	}
	size := int(req.GetPageSize())
	if size > 10000 {
		size = 10000
	}
	page, next, err := pageOf("executions:"+parent+req.GetFilter()+order, items, func(e *executionspb.Execution) string { return e.GetName() }, req.GetPageToken(), size)
	if err != nil {
		return nil, err
	}
	return &executionspb.ListExecutionsResponse{Executions: page, NextPageToken: next}, nil
}

func (a *ExecutionsAPI) CancelExecution(_ context.Context, req *executionspb.CancelExecutionRequest) (*executionspb.Execution, error) {
	s := a.s
	if !executionRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/workflows/{workflow}/executions/{execution}", req.GetName())
	}
	s.mu.Lock()
	er := s.execs[req.GetName()]
	if er == nil {
		s.mu.Unlock()
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if er.Ex.GetState() != executionspb.Execution_ACTIVE && er.Ex.GetState() != executionspb.Execution_QUEUED {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("Execution '%s' is not active and cannot be cancelled: it is %s", req.GetName(), er.Ex.GetState())
	}
	er.Ex.State = executionspb.Execution_CANCELLED
	cancel, done := er.cancel, er.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(er, executionspb.ExecutionView_FULL), nil
}

// Wait blocks until an execution finishes, for tests.
func (s *Server) Wait(name string) {
	s.mu.Lock()
	er := s.execs[name]
	s.mu.Unlock()
	if er != nil && er.done != nil {
		<-er.done
	}
}

// StepEntries is an execution's history.
func (s *Server) StepEntries(name string) ([]StepEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	er := s.execs[name]
	if er == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	if er.m != nil {
		return er.m.History(), nil
	}
	return append([]StepEntry(nil), er.History...), nil
}

// Callbacks lists an execution's callback endpoints.
func (s *Server) Callbacks(name string) ([]CallbackInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	er := s.execs[name]
	if er == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	if er.m == nil {
		return nil, nil
	}
	return er.m.callbacks.list(), nil
}

// Deliver hands an HTTP request to an execution's callback endpoint.
func (s *Server) Deliver(execution, id, method string, req map[string]any) error {
	s.mu.Lock()
	er := s.execs[execution]
	var m *machine
	if er != nil {
		m = er.m
	}
	s.mu.Unlock()
	if m == nil {
		return apierror.NotFound("callback %s/callbacks/%s was not found: the execution is not running", execution, id)
	}
	ok, methodOK := m.callbacks.deliver(id, method, req)
	switch {
	case !ok:
		return apierror.NotFound("callback %s/callbacks/%s was not found", execution, id)
	case !methodOK:
		return &apierror.Error{Code: 3, Reason: "invalid", Message: "the callback endpoint does not accept the " + method + " method"}
	}
	return nil
}

// DeleteHistory is executions.deleteExecutionHistory: the step entries of a
// finished execution are deleted.
func (s *Server) DeleteHistory(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	er := s.execs[name]
	if er == nil {
		return apierror.NotFound("Resource '%s' was not found", name)
	}
	if er.m != nil {
		return apierror.FailedPrecondition("Execution '%s' is still running", name)
	}
	er.History = nil
	return apierror.Wrap(s.save())
}

// WorkflowSource is a workflow's current source, for the console.
func (s *Server) WorkflowSource(name string) (*workflowspb.Workflow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.wfs[name]
	if rec == nil {
		return nil, false
	}
	return clone(rec.WF), true
}
