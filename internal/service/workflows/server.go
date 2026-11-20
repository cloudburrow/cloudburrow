// Package workflows serves Workflows v1 (workflows.googleapis.com) and
// Workflow Executions v1 (workflowexecutions.googleapis.com) in the CLI
// process, with an interpreter of Google's workflow syntax: steps, assign,
// switch, for, parallel, try/retry/except, subworkflows, expressions and
// the standard library, http.* calls and connectors, callbacks, and the
// documented limits and error types (#1986).
//
// Every http.* call and connector is sent to this instance's own endpoints:
// a googleapis.com URL goes to the local endpoint of the API it names, and
// anything outside the instance is refused locally with a
// ConnectionFailedError, before a byte is sent. A connector to an API this
// instance does not serve fails as Google's does for an API a project has
// not enabled: an HttpError 403 SERVICE_DISABLED.
//
// Workflows, their revisions and executions with their step entries are
// kept in a file in the instance's directory. docs/workflows.md is the
// user's guide.
package workflows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

var (
	locationRE  = regexp.MustCompile(`^projects/([a-z0-9][-a-z0-9.:]{0,62})/locations/([a-z0-9-]+)$`)
	workflowRE  = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/workflows/([^/@]+)$`)
	executionRE = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/workflows/[^/]+)/executions/([^/]+)$`)
	// workflowIDRE is CreateWorkflowRequest.workflow_id: "Must contain only
	// letters, numbers, underscores and hyphens. Must start with a letter.
	// Must be between 1-64 characters. Must end with a number or a letter."
	workflowIDRE = regexp.MustCompile(`^[a-zA-Z]([a-zA-Z0-9_-]{0,62}[a-zA-Z0-9])?$`)
	envKeyRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// MaxWorkflows is "Workflows: 10,000 per project" (quotas).
const MaxWorkflows = 10000

// Options are what the server needs from the instance.
type Options struct {
	// Local is the "http://host:port" of an API this instance serves, by
	// the first label of its googleapis.com host, or "".
	Local func(api string) string
	// Tokens mint the workflow service account's tokens; nil sends none.
	Tokens TokenMinter
	// Log writes an execution's log entries (sys.log's, its start and end,
	// call logs) to logID, in the instance's Cloud Logging when it serves
	// it.
	Log func(project, location, workflow, execution, logID, severity string, payload any)
	// KMS is the instance's Cloud KMS, which a workflow's
	// customer-managed key is used through (cmek.go); nil without it.
	KMS func() KMS
	// Addr is this server's own address, for callback URLs.
	Addr func() string
	// Ingress is the cluster ingress's host address, which Cloud Run
	// URLs are sent to (HTTPCaller.Ingress); "" without Cloud Run.
	Ingress string
	// ProjectNumber resolves a project's number for sys.get_env; nil uses
	// the ID.
	ProjectNumber func(project string) string
}

type workflowRecord struct {
	WF        *workflowspb.Workflow
	Revisions []*workflowspb.Workflow // newest first
	revCount  int
	// cipher is the source encrypted under WF's crypto_key_name.
	cipher []byte
}

type executionRecord struct {
	Ex      *executionspb.Execution
	History []StepEntry
	m       *machine
	cancel  context.CancelCauseFunc
	done    chan struct{}
	// journal is a stopped execution's journal, to resume it from.
	journal []journalEntry
	// key and cipher are its revision's customer-managed key and the
	// source encrypted under it.
	key    string
	cipher []byte
	// level is the execution's own executionHistoryLevel, which the Go
	// client's Execution has no field for (the JSON API carries it); ""
	// takes its workflow's.
	level string
}

// Server serves both APIs over one set of records.
type Server struct {
	opts  Options
	state string

	mu    sync.Mutex
	wfs   map[string]*workflowRecord
	execs map[string]*executionRecord
	ops   map[string]*longrunningpb.Operation
	now   func() time.Time
	wg    sync.WaitGroup
	ctx   context.Context
	stop  context.CancelFunc
	// keyCheck is KeyCheckInterval when the server was made.
	keyCheck time.Duration
}

// NewServer loads the records in state ("" keeps them in memory).
func NewServer(state string, opts Options) (*Server, error) {
	ctx, stop := context.WithCancel(context.Background())
	s := &Server{opts: opts, state: state, wfs: map[string]*workflowRecord{}, execs: map[string]*executionRecord{},
		ops: map[string]*longrunningpb.Operation{}, now: time.Now, ctx: ctx, stop: stop, keyCheck: KeyCheckInterval}
	if state != "" {
		if err := s.load(); err != nil {
			stop()
			return nil, err
		}
	}
	return s, nil
}

// Close cancels the running executions and waits for them.
func (s *Server) Close() {
	s.stop()
	s.wg.Wait()
}

// ---- persistence ----

type diskRecord struct {
	Workflows  []diskWorkflow    `json:"workflows"`
	Executions []diskExecution   `json:"executions"`
	Operations []json.RawMessage `json:"operations"`
}

type diskWorkflow struct {
	Workflow  json.RawMessage   `json:"workflow"`
	Revisions []json.RawMessage `json:"revisions"`
	Count     int               `json:"revisionCount"`
	Cipher    []byte            `json:"cipher,omitempty"`
}

type diskExecution struct {
	Execution json.RawMessage `json:"execution"`
	History   []StepEntry     `json:"history"`
	Journal   []journalEntry  `json:"journal,omitempty"`
	Key       string          `json:"key,omitempty"`
	Cipher    []byte          `json:"cipher,omitempty"`
	Level     string          `json:"executionHistoryLevel,omitempty"`
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d diskRecord
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("workflows state %s: %w", s.state, err)
	}
	for _, w := range d.Workflows {
		rec := &workflowRecord{WF: &workflowspb.Workflow{}, revCount: w.Count, cipher: w.Cipher}
		if err := protojson.Unmarshal(w.Workflow, rec.WF); err != nil {
			return err
		}
		for _, r := range w.Revisions {
			rev := &workflowspb.Workflow{}
			if err := protojson.Unmarshal(r, rev); err != nil {
				return err
			}
			rec.Revisions = append(rec.Revisions, rev)
		}
		s.wfs[rec.WF.GetName()] = rec
	}
	for _, e := range d.Executions {
		rec := &executionRecord{Ex: &executionspb.Execution{}, History: e.History, journal: e.Journal, key: e.Key, cipher: e.Cipher, level: e.Level}
		if err := protojson.Unmarshal(e.Execution, rec.Ex); err != nil {
			return err
		}
		// An execution the instance stopped during carries on when Resume
		// is called (journal.go).
		s.execs[rec.Ex.GetName()] = rec
	}
	for _, o := range d.Operations {
		op := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(o, op); err != nil {
			return err
		}
		s.ops[op.GetName()] = op
	}
	return nil
}

// save writes the records. The caller holds mu.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	var d diskRecord
	for _, name := range sortedNames(s.wfs) {
		w := s.wfs[name]
		b, err := protojson.Marshal(w.WF)
		if err != nil {
			return err
		}
		dw := diskWorkflow{Workflow: b, Count: w.revCount}
		for _, r := range w.Revisions {
			rb, err := protojson.Marshal(r)
			if err != nil {
				return err
			}
			dw.Revisions = append(dw.Revisions, rb)
		}
		dw.Cipher = w.cipher
		d.Workflows = append(d.Workflows, dw)
	}
	for _, name := range sortedNames(s.execs) {
		e := s.execs[name]
		b, err := protojson.Marshal(e.Ex)
		if err != nil {
			return err
		}
		h, j := e.History, e.journal
		if e.m != nil {
			h, j = e.m.History(), e.m.Journal()
		}
		if e.Ex.GetState() != executionspb.Execution_ACTIVE && e.Ex.GetState() != executionspb.Execution_QUEUED {
			j = nil
		}
		d.Executions = append(d.Executions, diskExecution{Execution: b, History: h, Journal: j, Key: e.key, Cipher: e.cipher, Level: e.level})
	}
	for _, name := range sortedNames(s.ops) {
		b, err := protojson.Marshal(s.ops[name])
		if err != nil {
			return err
		}
		d.Operations = append(d.Operations, b)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

func sortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResetProject forgets one project's workflows and executions, or every
// project's for "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	match := func(name string) bool { return project == "" || strings.HasPrefix(name, "projects/"+project+"/") }
	for name, e := range s.execs {
		if match(name) {
			if e.cancel != nil {
				e.cancel(context.Canceled)
			}
			delete(s.execs, name)
		}
	}
	for name := range s.wfs {
		if match(name) {
			delete(s.wfs, name)
		}
	}
	for name := range s.ops {
		if match(name) {
			delete(s.ops, name)
		}
	}
	return s.save()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---- Workflows ----

// WorkflowsAPI is google.cloud.workflows.v1.Workflows.
type WorkflowsAPI struct {
	workflowspb.UnimplementedWorkflowsServer
	s *Server
}

// Workflows is the Workflows API.
func (s *Server) Workflows() *WorkflowsAPI { return &WorkflowsAPI{s: s} }

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

func (s *Server) op(location, verb, target string, resp proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(s.now())
	md, err := anypb.New(&workflowspb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: location + "/operations/operation-" + randomHex(10), Done: true, Metadata: md}
	if resp != nil {
		r, err := anypb.New(resp)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		op.Result = &longrunningpb.Operation_Response{Response: r}
	} else {
		r, _ := anypb.New(&emptypb.Empty{})
		op.Result = &longrunningpb.Operation_Response{Response: r}
	}
	s.ops[op.Name] = op
	return clone(op), nil
}

// revisionID is Google's form: a six-digit counter and three hex digits.
func revisionID(n int) string { return fmt.Sprintf("%06d-%s", n, randomHex(2)[:3]) }

func validateWorkflow(w *workflowspb.Workflow) error {
	if len(w.GetDescription()) > 1000 {
		return apierror.InvalidArgument("description must be at most 1000 characters")
	}
	if len(w.GetLabels()) > 64 {
		return apierror.InvalidArgument("a workflow has at most 64 labels")
	}
	if len(w.GetUserEnvVars()) > MaxEnvVars {
		return apierror.InvalidArgument("a workflow has at most %d user-defined environment variables", MaxEnvVars)
	}
	for k, v := range w.GetUserEnvVars() {
		if !envKeyRE.MatchString(k) || strings.HasPrefix(k, "GOOGLE_") || strings.HasPrefix(k, "WORKFLOWS_") {
			return apierror.InvalidArgument("user_env_vars key %q is not allowed: keys are letters, digits and underscores, and may not start with GOOGLE or WORKFLOWS", k)
		}
		if len(k)+1+len(v) > MaxEnvVarBytes {
			return apierror.InvalidArgument("user_env_vars %q is longer than %d bytes", k, MaxEnvVarBytes)
		}
	}
	if err := checkKeyName(w); err != nil {
		return err
	}
	src := w.GetSourceContents()
	if src == "" {
		return apierror.InvalidArgument("source_contents is required")
	}
	if _, err := Parse(src); err != nil {
		return apierror.InvalidArgument("Workflow failed to deploy: %v", err)
	}
	return nil
}

func (a *WorkflowsAPI) CreateWorkflow(_ context.Context, req *workflowspb.CreateWorkflowRequest) (*longrunningpb.Operation, error) {
	s := a.s
	if !locationRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	if !workflowIDRE.MatchString(req.GetWorkflowId()) {
		return nil, apierror.InvalidArgument("workflow_id %q must start with a letter, end with a letter or a number, be 1 to 64 characters, and contain only letters, numbers, underscores and hyphens", req.GetWorkflowId())
	}
	w := clone(req.GetWorkflow())
	if w == nil {
		return nil, apierror.InvalidArgument("workflow is required")
	}
	if err := validateWorkflow(w); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/workflows/" + req.GetWorkflowId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wfs[name] != nil {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	n := 0
	for k := range s.wfs {
		if strings.HasPrefix(k, strings.SplitN(req.GetParent(), "/locations/", 2)[0]+"/") {
			n++
		}
	}
	if n >= MaxWorkflows {
		return nil, &apierror.Error{Code: codes.ResourceExhausted, Reason: "rateLimitExceeded",
			Message: fmt.Sprintf("the project has %d workflows; the quota is %d", n, MaxWorkflows)}
	}
	now := timestamppb.New(s.now())
	w.Name = name
	w.State = workflowspb.Workflow_ACTIVE
	w.CreateTime, w.UpdateTime, w.RevisionCreateTime = now, now, now
	w.RevisionId = revisionID(1)
	w.StateError = nil
	if w.ServiceAccount == "" {
		project := strings.Split(name, "/")[1]
		w.ServiceAccount = "projects/" + project + "/serviceAccounts/" + a.s.projectNumber(project) + "-compute@developer.gserviceaccount.com"
	}
	cipher, err := s.seal(w)
	if err != nil {
		return nil, err
	}
	rec := &workflowRecord{WF: w, Revisions: []*workflowspb.Workflow{clone(w)}, revCount: 1, cipher: cipher}
	s.wfs[name] = rec
	op, err := s.op(req.GetParent(), "create", name, w)
	if err != nil {
		return nil, err
	}
	return op, apierror.Wrap(s.save())
}

func (s *Server) projectNumber(project string) string {
	if s.opts.ProjectNumber != nil {
		if n := s.opts.ProjectNumber(project); n != "" {
			return n
		}
	}
	return project
}

func (a *WorkflowsAPI) GetWorkflow(_ context.Context, req *workflowspb.GetWorkflowRequest) (*workflowspb.Workflow, error) {
	s := a.s
	if !workflowRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/workflows/{workflow}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.wfs[req.GetName()]
	if rec == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if rid := req.GetRevisionId(); rid != "" {
		for _, r := range rec.Revisions {
			if r.GetRevisionId() == rid {
				return clone(r), nil
			}
		}
		return nil, apierror.NotFound("Revision '%s' of '%s' was not found", rid, req.GetName())
	}
	return clone(rec.WF), nil
}

func (a *WorkflowsAPI) ListWorkflows(_ context.Context, req *workflowspb.ListWorkflowsRequest) (*workflowspb.ListWorkflowsResponse, error) {
	s := a.s
	parent := req.GetParent()
	project, location, all := "", "", false
	if m := locationRE.FindStringSubmatch(parent); m != nil {
		project, location = m[1], m[2]
		all = location == "-"
	} else {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
	}
	s.mu.Lock()
	var items []*workflowspb.Workflow
	for _, name := range sortedNames(s.wfs) {
		prefix := "projects/" + project + "/locations/" + location + "/"
		if all {
			prefix = "projects/" + project + "/locations/"
		}
		if strings.HasPrefix(name, prefix) {
			items = append(items, clone(s.wfs[name].WF))
		}
	}
	s.mu.Unlock()
	items, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), items)
	if err != nil {
		return nil, err
	}
	page, next, err := pageOf("workflows:"+parent+req.GetFilter()+req.GetOrderBy(), items, func(w *workflowspb.Workflow) string { return w.GetName() }, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &workflowspb.ListWorkflowsResponse{Workflows: page, NextPageToken: next}, nil
}

// pageOf pages items in their order, by their keys.
func pageOf[T any](scope string, items []T, key func(T) string, token string, size int) ([]T, string, error) {
	keys := make([]string, len(items))
	byKey := map[string]T{}
	for i, it := range items {
		keys[i] = fmt.Sprintf("%08d\x00%s", i, key(it))
		byKey[keys[i]] = it
	}
	page, next, err := paging.Page(scope, keys, token, size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("invalid page_token")
	}
	out := make([]T, 0, len(page))
	for _, k := range page {
		out = append(out, byKey[k])
	}
	return out, next, nil
}

func (a *WorkflowsAPI) DeleteWorkflow(_ context.Context, req *workflowspb.DeleteWorkflowRequest) (*longrunningpb.Operation, error) {
	s := a.s
	m := workflowRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/workflows/{workflow}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wfs[req.GetName()] == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	delete(s.wfs, req.GetName())
	// Its executions go with it; running ones are cancelled.
	for name, e := range s.execs {
		if strings.HasPrefix(name, req.GetName()+"/executions/") {
			if e.cancel != nil {
				e.cancel(context.Canceled)
			}
			delete(s.execs, name)
		}
	}
	op, err := s.op(m[1], "delete", req.GetName(), nil)
	if err != nil {
		return nil, err
	}
	return op, apierror.Wrap(s.save())
}

// revisionFields are the fields whose change makes a new revision
// (Workflow.revision_id: "source code, service account, ... crypto key,
// call log level, environment variables, execution history level").
var revisionFields = map[string]bool{"source_contents": true, "service_account": true, "crypto_key_name": true,
	"call_log_level": true, "user_env_vars": true, "execution_history_level": true}

var updatable = map[string]bool{"description": true, "labels": true, "source_contents": true, "service_account": true,
	"crypto_key_name": true, "call_log_level": true, "user_env_vars": true, "execution_history_level": true, "tags": true}

func (a *WorkflowsAPI) UpdateWorkflow(_ context.Context, req *workflowspb.UpdateWorkflowRequest) (*longrunningpb.Operation, error) {
	s := a.s
	in := req.GetWorkflow()
	m := workflowRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("workflow.name %q must be projects/{project}/locations/{location}/workflows/{workflow}", in.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.wfs[in.GetName()]
	if rec == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"description", "labels", "source_contents", "service_account", "call_log_level", "user_env_vars", "execution_history_level"}
	}
	next := clone(rec.WF)
	newRevision := false
	for _, p := range paths {
		p = snake(p)
		if !updatable[p] {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated", p)
		}
		switch p {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			next.Labels = in.GetLabels()
		case "tags":
			next.Tags = in.GetTags()
		case "source_contents":
			if next.GetSourceContents() != in.GetSourceContents() {
				newRevision = true
			}
			next.SourceCode = &workflowspb.Workflow_SourceContents{SourceContents: in.GetSourceContents()}
		case "service_account":
			if in.GetServiceAccount() != "" && next.GetServiceAccount() != in.GetServiceAccount() {
				newRevision = true
				next.ServiceAccount = in.GetServiceAccount()
			}
		case "crypto_key_name":
			if next.CryptoKeyName != in.GetCryptoKeyName() {
				// "that revision of the workflow is encrypted with the new
				// key" (use-cmek).
				newRevision = true
			}
			next.CryptoKeyName = in.GetCryptoKeyName()
		case "call_log_level":
			if next.CallLogLevel != in.GetCallLogLevel() {
				newRevision = true
			}
			next.CallLogLevel = in.GetCallLogLevel()
		case "user_env_vars":
			if !proto.Equal(&workflowspb.Workflow{UserEnvVars: next.UserEnvVars}, &workflowspb.Workflow{UserEnvVars: in.GetUserEnvVars()}) {
				newRevision = true
			}
			next.UserEnvVars = in.GetUserEnvVars()
		case "execution_history_level":
			if next.ExecutionHistoryLevel != in.GetExecutionHistoryLevel() {
				newRevision = true
			}
			next.ExecutionHistoryLevel = in.GetExecutionHistoryLevel()
		}
	}
	if err := validateWorkflow(next); err != nil {
		return nil, err
	}
	cipher := rec.cipher
	if next.GetCryptoKeyName() != rec.WF.GetCryptoKeyName() || next.GetSourceContents() != rec.WF.GetSourceContents() {
		c, err := s.seal(next)
		if err != nil {
			return nil, err
		}
		cipher = c
	}
	rec.cipher = cipher
	now := timestamppb.New(s.now())
	next.UpdateTime = now
	if newRevision && revisionFields != nil {
		rec.revCount++
		next.RevisionId = revisionID(rec.revCount)
		next.RevisionCreateTime = now
		rec.Revisions = append([]*workflowspb.Workflow{clone(next)}, rec.Revisions...)
	} else if len(rec.Revisions) > 0 {
		rec.Revisions[0] = clone(next)
	}
	rec.WF = next
	op, err := s.op(m[1], "update", next.GetName(), next)
	if err != nil {
		return nil, err
	}
	return op, apierror.Wrap(s.save())
}

func snake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (a *WorkflowsAPI) ListWorkflowRevisions(_ context.Context, req *workflowspb.ListWorkflowRevisionsRequest) (*workflowspb.ListWorkflowRevisionsResponse, error) {
	s := a.s
	if !workflowRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/workflows/{workflow}", req.GetName())
	}
	s.mu.Lock()
	rec := s.wfs[req.GetName()]
	var revs []*workflowspb.Workflow
	if rec != nil {
		for _, r := range rec.Revisions {
			revs = append(revs, clone(r))
		}
	}
	s.mu.Unlock()
	if rec == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	size := int(req.GetPageSize())
	if size == 0 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	page, next, err := pageOf("revisions:"+req.GetName(), revs, func(w *workflowspb.Workflow) string { return w.GetRevisionId() }, req.GetPageToken(), size)
	if err != nil {
		return nil, err
	}
	return &workflowspb.ListWorkflowRevisionsResponse{Workflows: page, NextPageToken: next}, nil
}

// ---- Operations ----

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op := o.s.ops[req.GetName()]
	if op == nil {
		return nil, apierror.NotFound("Operation '%s' was not found", req.GetName())
	}
	return clone(op), nil
}

func (o *operations) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	o.s.mu.Lock()
	var items []*longrunningpb.Operation
	for _, name := range sortedNames(o.s.ops) {
		if strings.HasPrefix(name, req.GetName()+"/operations/") {
			items = append(items, clone(o.s.ops[name]))
		}
	}
	o.s.mu.Unlock()
	page, next, err := pageOf("ops:"+req.GetName(), items, func(op *longrunningpb.Operation) string { return op.GetName() }, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: page, NextPageToken: next}, nil
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if o.s.ops[req.GetName()] == nil {
		return nil, apierror.NotFound("Operation '%s' was not found", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	return &emptypb.Empty{}, apierror.Wrap(o.s.save())
}
