// Package dataform serves Dataform (dataform.googleapis.com v1, #2105):
// repositories, their workspaces and files, release and workflow configs,
// compilation results built with Dataform core, and workflow invocations
// that run the compiled SQLX actions on the instance's BigQuery.
//
// As Google documents it (docs.cloud.google.com/dataform/docs and the v1
// reference, read 2026-11-21):
//
//   - a repository either keeps its code in Dataform's own git repository,
//     or names a third-party remote with git_remote_settings (a URL, a
//     default branch and a Secret Manager secret version holding the
//     access token). Here a remote must be a Secure Source Manager
//     repository this instance serves: its git_https URI. Any other URL is
//     INVALID_ARGUMENT, so a workspace never pulls from or pushes to a
//     host outside the instance;
//   - CommitRepositoryChanges, ReadRepositoryFile,
//     QueryRepositoryDirectoryContents and FetchRepositoryHistory need a
//     repository without a remote; FetchRemoteBranches and
//     ComputeRepositoryAccessTokenStatus need one with a remote;
//   - a workspace is a git working copy of the repository: files and
//     directories, uncommitted changes, commits authored as the request
//     names, pull, push, reset, diffs and file statuses;
//   - InstallNpmPackages runs npm install for the workspace's package.json;
//   - a compilation result compiles a git commitish, a workspace or a
//     release config with Dataform core (the official @dataform/cli, which
//     installs the core version the project's workflow_settings.yaml or
//     package.json names), its errors reported per file;
//   - a workflow invocation runs a compilation result's actions (or a
//     workflow config's release) on BigQuery in dependency order: tables,
//     views, incremental tables, assertions and operations, each action
//     PENDING, RUNNING, then SUCCEEDED, FAILED, SKIPPED (a dependency
//     failed), DISABLED or CANCELLED; an assertion fails when its query
//     returns rows.
//
// Compiling and installing packages run in a container of the pinned Node
// image (Runner); the SQL runs through the official BigQuery client
// (Warehouse). Both are the caller's, so the package stays pure Go.
package dataform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

const defaultPageSize = 50

// Runner runs Dataform core and npm for a project directory.
type Runner interface {
	// Compile compiles the project in dir with @dataform/cli's compile
	// --json and the given flags, returning its JSON output (a compiled
	// graph, with graphErrors when the project does not compile).
	Compile(ctx context.Context, dir string, flags []string) ([]byte, error)
	// InstallPackages runs npm install in dir, with env set.
	InstallPackages(ctx context.Context, dir string, env []string) error
}

// Warehouse runs an invocation's SQL on BigQuery.
type Warehouse interface {
	// EnsureDataset creates project.dataset in location unless it exists.
	EnsureDataset(ctx context.Context, project, dataset, location string) error
	// Table describes project.dataset.table: whether it exists, its type
	// (TABLE, VIEW, MATERIALIZED_VIEW) and its column names.
	Table(ctx context.Context, project, dataset, table string) (exists bool, kind string, columns []string, err error)
	// Exec runs a statement as a query job and returns the job's ID.
	Exec(ctx context.Context, project, location, sql string) (jobID string, err error)
	// Count runs a query returning one INT64 column in one row, and
	// returns the job's ID and that value (0 when it is NULL).
	Count(ctx context.Context, project, location, sql string) (jobID string, n int64, err error)
}

// Options are the server's collaborators.
type Options struct {
	// DataDir holds the repositories' git repositories and the workspaces.
	DataDir string
	// State is the records' file; "" keeps them in memory.
	State string
	// Runner compiles and installs packages; nil refuses both
	// FAILED_PRECONDITION.
	Runner Runner
	// Warehouse runs invocations; nil fails each invocation's actions.
	Warehouse Warehouse
	// GitBase is the Secure Source Manager git base URL of this instance
	// (http://host:port), or "" when it is not served.
	GitBase func() string
	// AccessSecret reads a Secret Manager secret version's payload; nil
	// when Secret Manager is not served.
	AccessSecret func(ctx context.Context, name string) ([]byte, error)
	// Keys are the customer-managed keys: the instance's Cloud KMS.
	Keys cmek.Keys
	// LinkToken fetches a read-write token for a Developer Connect git
	// repository link (projects/*/locations/*/connections/*/gitRepositoryLinks/*);
	// nil when Developer Connect is not served.
	LinkToken func(ctx context.Context, link string) (string, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Server serves google.cloud.dataform.v1.Dataform.
type Server struct {
	dfpb.UnimplementedDataformServer

	opts Options

	mu           sync.Mutex
	repos        map[string]*dfpb.Repository
	workspaces   map[string]*dfpb.Workspace
	releases     map[string]*dfpb.ReleaseConfig
	workflows    map[string]*dfpb.WorkflowConfig
	compilations map[string]*compilation
	invocations  map[string]*invocation
	folders      map[string]*dfpb.Folder
	teamFolders  map[string]*dfpb.TeamFolder
	configs      map[string]*dfpb.Config
	policies     map[string]*iampolicy.Stored
	ops          map[string]*longrunningpb.Operation
	// sealed are the data keys of repositories with a customer-managed
	// key, by repository name (cmek.go).
	sealed map[string]cmek.Sealed
	// wsExtras and triggers are what only v1beta1 sets on workspaces and
	// workflow configs, by name (v1beta1.go, triggers.go).
	wsExtras map[string]*workspaceExtra
	triggers map[string]*triggerState
	// locks serialize git work on one repository or workspace directory.
	locks map[string]*sync.Mutex
	// cancels stop running invocations.
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

type compilation struct {
	C       *dfpb.CompilationResult
	Actions []*dfpb.CompilationResultAction
}

type invocation struct {
	I       *dfpb.WorkflowInvocation
	Actions []*dfpb.WorkflowInvocationAction
}

// NewServer loads the records in opts.State.
func NewServer(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts}
	s.clear()
	if opts.DataDir != "" {
		if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
			return nil, fmt.Errorf("dataform data directory: %w", err)
		}
	}
	if opts.State != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) clear() {
	s.repos = map[string]*dfpb.Repository{}
	s.workspaces = map[string]*dfpb.Workspace{}
	s.releases = map[string]*dfpb.ReleaseConfig{}
	s.workflows = map[string]*dfpb.WorkflowConfig{}
	s.compilations = map[string]*compilation{}
	s.invocations = map[string]*invocation{}
	s.folders = map[string]*dfpb.Folder{}
	s.teamFolders = map[string]*dfpb.TeamFolder{}
	s.configs = map[string]*dfpb.Config{}
	s.policies = map[string]*iampolicy.Stored{}
	s.ops = map[string]*longrunningpb.Operation{}
	s.sealed = map[string]cmek.Sealed{}
	s.wsExtras = map[string]*workspaceExtra{}
	s.triggers = map[string]*triggerState{}
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	if s.cancels == nil {
		s.cancels = map[string]context.CancelFunc{}
	}
}

// Close cancels running invocations and waits for them.
func (s *Server) Close() {
	s.mu.Lock()
	for _, c := range s.cancels {
		c()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// ResetProject deletes a project's records and files ("" is every project).
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	for name, c := range s.cancels {
		if project == "" || strings.HasPrefix(name, "projects/"+project+"/") {
			c()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.clear()
		if s.opts.DataDir != "" {
			for _, d := range []string{"repos", "workspaces", "snapshots"} {
				_ = os.RemoveAll(filepath.Join(s.opts.DataDir, d))
			}
		}
		return s.save()
	}
	in := func(name string) bool { return strings.HasPrefix(name, "projects/"+project+"/") }
	for k := range s.repos {
		if in(k) {
			_ = os.RemoveAll(s.repoDir(k))
			delete(s.repos, k)
			delete(s.sealed, k)
		}
	}
	for k := range s.workspaces {
		if in(k) {
			_ = os.RemoveAll(s.workspaceDir(k))
			delete(s.workspaces, k)
		}
	}
	dropIn(s.releases, in)
	dropIn(s.workflows, in)
	dropIn(s.compilations, in)
	dropIn(s.invocations, in)
	dropIn(s.folders, in)
	dropIn(s.teamFolders, in)
	dropIn(s.configs, in)
	dropIn(s.policies, in)
	dropIn(s.ops, in)
	return s.save()
}

func dropIn[V any](m map[string]V, in func(string) bool) {
	for k := range m {
		if in(k) {
			delete(m, k)
		}
	}
}

// ---- persistence ----

type stateFile struct {
	Repositories   []json.RawMessage            `json:"repositories,omitempty"`
	Workspaces     []json.RawMessage            `json:"workspaces,omitempty"`
	ReleaseConfigs []json.RawMessage            `json:"releaseConfigs,omitempty"`
	WorkflowConfig []json.RawMessage            `json:"workflowConfigs,omitempty"`
	Compilations   []storedCompilation          `json:"compilationResults,omitempty"`
	Invocations    []storedInvocation           `json:"workflowInvocations,omitempty"`
	Folders        []json.RawMessage            `json:"folders,omitempty"`
	TeamFolders    []json.RawMessage            `json:"teamFolders,omitempty"`
	Configs        []json.RawMessage            `json:"configs,omitempty"`
	Policies       map[string]*iampolicy.Stored `json:"policies,omitempty"`
	Keys           map[string]cmek.Sealed       `json:"keys,omitempty"`
	// WorkspaceExtras and Triggers are v1beta1's (v1beta1.go).
	WorkspaceExtras map[string]*workspaceExtra `json:"workspaceExtras,omitempty"`
	Triggers        map[string]*triggerState   `json:"workflowTriggers,omitempty"`
}

type storedCompilation struct {
	Result  json.RawMessage   `json:"result"`
	Actions []json.RawMessage `json:"actions,omitempty"`
}

type storedInvocation struct {
	Invocation json.RawMessage   `json:"invocation"`
	Actions    []json.RawMessage `json:"actions,omitempty"`
}

func marshalAll[M proto.Message](m map[string]M) []json.RawMessage {
	keys := sortedKeys(m)
	out := make([]json.RawMessage, 0, len(keys))
	for _, k := range keys {
		b, err := protojson.Marshal(m[k])
		if err == nil {
			out = append(out, b)
		}
	}
	return out
}

func marshalList[M proto.Message](xs []M) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(xs))
	for _, x := range xs {
		b, err := protojson.Marshal(x)
		if err == nil {
			out = append(out, b)
		}
	}
	return out
}

func unmarshalAll[M proto.Message](raw []json.RawMessage, mk func() M, name func(M) string, into map[string]M) error {
	for _, r := range raw {
		m := mk()
		if err := protojson.Unmarshal(r, m); err != nil {
			return err
		}
		into[name(m)] = m
	}
	return nil
}

// save writes the records; the caller holds mu.
func (s *Server) save() error {
	// What only v1beta1 sets goes with its workspace or workflow config.
	for k := range s.wsExtras {
		if _, ok := s.workspaces[k]; !ok {
			delete(s.wsExtras, k)
		}
	}
	for k, t := range s.triggers {
		if _, ok := s.workflows[k]; !ok {
			delete(s.triggers, k)
		} else {
			t.marshal()
		}
	}
	if s.opts.State == "" {
		return nil
	}
	st := stateFile{
		WorkspaceExtras: s.wsExtras,
		Triggers:        s.triggers,
		Repositories:    marshalAll(s.repos),
		Workspaces:      marshalAll(s.workspaces),
		ReleaseConfigs:  marshalAll(s.releases),
		WorkflowConfig:  marshalAll(s.workflows),
		Folders:         marshalAll(s.folders),
		TeamFolders:     marshalAll(s.teamFolders),
		Configs:         marshalAll(s.configs),
		Policies:        s.policies,
		Keys:            s.sealed,
	}
	for _, k := range sortedKeys(s.compilations) {
		c := s.compilations[k]
		b, err := protojson.Marshal(c.C)
		if err != nil {
			return err
		}
		st.Compilations = append(st.Compilations, storedCompilation{Result: b, Actions: marshalList(c.Actions)})
	}
	for _, k := range sortedKeys(s.invocations) {
		i := s.invocations[k]
		b, err := protojson.Marshal(i.I)
		if err != nil {
			return err
		}
		st.Invocations = append(st.Invocations, storedInvocation{Invocation: b, Actions: marshalList(i.Actions)})
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.State)
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.opts.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("dataform state %s: %w", s.opts.State, err)
	}
	errs := []error{
		unmarshalAll(st.Repositories, func() *dfpb.Repository { return &dfpb.Repository{} }, (*dfpb.Repository).GetName, s.repos),
		unmarshalAll(st.Workspaces, func() *dfpb.Workspace { return &dfpb.Workspace{} }, (*dfpb.Workspace).GetName, s.workspaces),
		unmarshalAll(st.ReleaseConfigs, func() *dfpb.ReleaseConfig { return &dfpb.ReleaseConfig{} }, (*dfpb.ReleaseConfig).GetName, s.releases),
		unmarshalAll(st.WorkflowConfig, func() *dfpb.WorkflowConfig { return &dfpb.WorkflowConfig{} }, (*dfpb.WorkflowConfig).GetName, s.workflows),
		unmarshalAll(st.Folders, func() *dfpb.Folder { return &dfpb.Folder{} }, (*dfpb.Folder).GetName, s.folders),
		unmarshalAll(st.TeamFolders, func() *dfpb.TeamFolder { return &dfpb.TeamFolder{} }, (*dfpb.TeamFolder).GetName, s.teamFolders),
		unmarshalAll(st.Configs, func() *dfpb.Config { return &dfpb.Config{} }, (*dfpb.Config).GetName, s.configs),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("dataform state %s: %w", s.opts.State, err)
	}
	for _, c := range st.Compilations {
		r := &compilation{C: &dfpb.CompilationResult{}}
		if err := protojson.Unmarshal(c.Result, r.C); err != nil {
			return err
		}
		for _, a := range c.Actions {
			x := &dfpb.CompilationResultAction{}
			if err := protojson.Unmarshal(a, x); err != nil {
				return err
			}
			r.Actions = append(r.Actions, x)
		}
		s.compilations[r.C.GetName()] = r
	}
	now := timestamppb.New(s.opts.Now())
	for _, c := range st.Invocations {
		r := &invocation{I: &dfpb.WorkflowInvocation{}}
		if err := protojson.Unmarshal(c.Invocation, r.I); err != nil {
			return err
		}
		for _, a := range c.Actions {
			x := &dfpb.WorkflowInvocationAction{}
			if err := protojson.Unmarshal(a, x); err != nil {
				return err
			}
			r.Actions = append(r.Actions, x)
		}
		// An invocation the instance stopped in the middle of cannot
		// resume: its running and pending actions are cancelled.
		if r.I.GetState() == dfpb.WorkflowInvocation_RUNNING || r.I.GetState() == dfpb.WorkflowInvocation_CANCELING {
			r.I.State = dfpb.WorkflowInvocation_CANCELLED
			endTiming(r.I.GetInvocationTiming(), now)
			for _, a := range r.Actions {
				if a.GetState() == dfpb.WorkflowInvocationAction_RUNNING || a.GetState() == dfpb.WorkflowInvocationAction_PENDING {
					a.State = dfpb.WorkflowInvocationAction_CANCELLED
				}
			}
		}
		s.invocations[r.I.GetName()] = r
	}
	if st.Policies != nil {
		s.policies = st.Policies
	}
	if st.Keys != nil {
		s.sealed = st.Keys
	}
	if st.WorkspaceExtras != nil {
		s.wsExtras = st.WorkspaceExtras
	}
	for k, t := range st.Triggers {
		if err := t.unmarshal(); err != nil {
			return fmt.Errorf("dataform state %s: trigger of %s: %w", s.opts.State, k, err)
		}
		if t.Seen == nil {
			t.Seen = map[string]time.Time{}
		}
		s.triggers[k] = t
	}
	return nil
}

// ---- names ----

var (
	locationRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	repositoryRE   = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/repositories/([^/]+)$`)
	workspaceRE    = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/workspaces/([^/]+)$`)
	releaseRE      = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/releaseConfigs/([^/]+)$`)
	workflowRE     = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/workflowConfigs/([^/]+)$`)
	compilationRE  = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/compilationResults/([^/]+)$`)
	invocationRE   = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/workflowInvocations/([^/]+)$`)
	folderRE       = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/folders/([^/]+)$`)
	teamFolderRE   = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/teamFolders/([^/]+)$`)
	gitLinkRE      = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/connections/[^/]+/gitRepositoryLinks/[^/]+$`)
	configRE       = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/config$`)
	operationRE    = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/operations/([^/]+)$`)
	resourceIDRE   = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)
	secretVersRE   = regexp.MustCompile(`^projects/[^/]+/(locations/[^/]+/)?secrets/[^/]+/versions/[^/]+$`)
	serviceAcctRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z0-9.-]+\.iam\.gserviceaccount\.com$|^[^@\s]+@[a-z0-9.-]+$`)
	kmsKeyRE       = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)
	displayFilterR = regexp.MustCompile(`^\s*display_name\s*(=|:)\s*"([^"]*)"\s*$`)
)

func checkLocation(field, name string) (string, error) {
	m := locationRE.FindStringSubmatch(name)
	if m == nil {
		return "", apierror.InvalidArgument("%s %q must be projects/{project}/locations/{location}", field, name)
	}
	if !validLocation(m[2]) {
		return "", apierror.InvalidArgument("location %q is not a Dataform location", m[2])
	}
	return m[1], nil
}

// Locations are Dataform's locations: Google Cloud's regions.
func Locations() []string { return locations.Regions }

func validLocation(id string) bool {
	for _, r := range Locations() {
		if r == id {
			return true
		}
	}
	return false
}

func checkID(field, id string) error {
	if !resourceIDRE.MatchString(id) {
		return apierror.InvalidArgument("%s %q must be 1 to 63 letters, digits, underscores or hyphens", field, id)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func childrenOf[V any](m map[string]V, parent, collection string) []string {
	prefix := parent + "/" + collection + "/"
	var out []string
	for _, k := range sortedKeys(m) {
		if strings.HasPrefix(k, prefix) && !strings.Contains(k[len(prefix):], "/") {
			out = append(out, k)
		}
	}
	return out
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func hashDir(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:12])
}

func (s *Server) repoDir(name string) string {
	return filepath.Join(s.opts.DataDir, "repos", hashDir(name)+".git")
}

func (s *Server) workspaceDir(name string) string {
	return filepath.Join(s.opts.DataDir, "workspaces", hashDir(name))
}

func (s *Server) lock(key string) func() {
	s.mu.Lock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func clone[M proto.Message](m M) M { return proto.Clone(m).(M) }

func (s *Server) now() *timestamppb.Timestamp { return timestamppb.New(s.opts.Now()) }

func hasPath(mask *fieldmaskpb.FieldMask, path string) bool {
	for _, p := range mask.GetPaths() {
		if p == path {
			return true
		}
	}
	return false
}

// page is an offset page of n items in their order; the token is the
// offset, scoped to what was asked.
func page(scope, token string, size int32, n int) (start, end int, next string, err error) {
	if size < 0 {
		return 0, 0, "", apierror.InvalidArgument("page_size must not be negative")
	}
	if size == 0 {
		size = defaultPageSize
	}
	if size > 1000 {
		size = 1000
	}
	if token != "" {
		var off int
		var sc string
		if _, err := fmt.Sscanf(decodeToken(token), "%d|%s", &off, &sc); err != nil || sc != hashDir(scope) || off < 0 || off > n {
			return 0, 0, "", apierror.InvalidArgument("invalid page_token")
		}
		start = off
	}
	end = start + int(size)
	if end >= n {
		return start, n, "", nil
	}
	return start, end, encodeToken(fmt.Sprintf("%d|%s", end, hashDir(scope))), nil
}

func encodeToken(s string) string { return hex.EncodeToString([]byte(s)) }

func decodeToken(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// done records a finished operation in location with resp as its response.
// The caller holds mu.
func (s *Server) done(location string, md, resp proto.Message) (*longrunningpb.Operation, error) {
	op := &longrunningpb.Operation{Name: location + "/operations/" + newUUID(), Done: true}
	if md != nil {
		a, err := anypb.New(md)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		op.Metadata = a
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op.Result = &longrunningpb.Operation_Response{Response: r}
	s.ops[op.Name] = op
	return clone(op), nil
}

// ---- Repositories ----

func (s *Server) CreateRepository(ctx context.Context, req *dfpb.CreateRepositoryRequest) (*dfpb.Repository, error) {
	if _, err := checkLocation("parent", req.GetParent()); err != nil {
		return nil, err
	}
	if err := checkID("repository_id", req.GetRepositoryId()); err != nil {
		return nil, err
	}
	r := clone(req.GetRepository())
	if r == nil {
		r = &dfpb.Repository{}
	}
	if err := s.checkRepository(ctx, r); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/repositories/" + req.GetRepositoryId()
	m := repoPrefixRE.FindStringSubmatch(name)
	var sealed *cmek.Sealed
	if key := s.repositoryKey(r, m[1], m[2]); key != "" {
		sl, err := s.sealRepository(ctx, name, key)
		if err != nil {
			return nil, err
		}
		r.KmsKeyName = key
		sealed = &sl
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[name]; ok {
		return nil, apierror.AlreadyExists("Repository %s already exists", name)
	}
	if err := s.checkContainer(req.GetParent(), r.GetContainingFolder()); err != nil {
		return nil, err
	}
	r.Name = name
	r.CreateTime = s.now()
	r.DataEncryptionState = nil
	if sealed != nil {
		r.DataEncryptionState = &dfpb.DataEncryptionState{KmsKeyVersionName: sealed.KeyVersion}
		s.sealed[name] = *sealed
	}
	r.InternalMetadata = nil
	s.setTeamFolder(r)
	if gr := r.GetGitRemoteSettings(); gr != nil {
		gr.EffectiveDefaultBranch = gr.GetDefaultBranch()
		gr.TokenStatus = dfpb.Repository_GitRemoteSettings_TOKEN_STATUS_UNSPECIFIED
	}
	if err := initBare(ctx, s.repoDir(name)); err != nil {
		return nil, apierror.Internal(err, "create the repository's git repository")
	}
	s.repos[name] = r
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(r), nil
}

// checkRepository validates a repository's settable fields.
func (s *Server) checkRepository(ctx context.Context, r *dfpb.Repository) error {
	if r.GetKmsKeyName() != "" {
		if !kmsKeyRE.MatchString(r.GetKmsKeyName()) {
			return apierror.InvalidArgument("kms_key_name %q must be projects/*/locations/*/keyRings/*/cryptoKeys/*", r.GetKmsKeyName())
		}
	}
	if v := r.GetNpmrcEnvironmentVariablesSecretVersion(); v != "" && !secretVersRE.MatchString(v) {
		return apierror.InvalidArgument("npmrc_environment_variables_secret_version %q must be projects/*/secrets/*/versions/*", v)
	}
	if sa := r.GetServiceAccount(); sa != "" && !strings.Contains(sa, "@") {
		return apierror.InvalidArgument("service_account %q must be a service account email", sa)
	}
	if r.GetEndUserAuthConfig() != nil {
		return apierror.Unimplemented("end-user authentication for Dataform repositories is not served by CloudBurrow")
	}
	gr := r.GetGitRemoteSettings()
	if gr == nil {
		return nil
	}
	if gr.GetSshAuthenticationConfig() != nil {
		return apierror.Unimplemented("SSH git remotes are not served by CloudBurrow: use the Secure Source Manager repository's HTTPS URL and an access token")
	}
	if link := gr.GetGitRepositoryLink(); link != "" {
		// "Resource name for the GitRepositoryLink used for machine
		// credentials": the link's read-write token authenticates git.
		if !gitLinkRE.MatchString(link) {
			return apierror.InvalidArgument("git_remote_settings.git_repository_link %q must be projects/*/locations/*/connections/*/gitRepositoryLinks/*", link)
		}
		if s.opts.LinkToken == nil {
			return apierror.FailedPrecondition("git_remote_settings.git_repository_link %s: Developer Connect is not served on this instance", link)
		}
		if _, err := s.opts.LinkToken(ctx, link); err != nil {
			return apierror.InvalidArgument("git_remote_settings.git_repository_link %s: %v", link, err)
		}
	}
	if gr.GetUrl() == "" {
		return apierror.InvalidArgument("git_remote_settings.url is required")
	}
	if gr.GetDefaultBranch() == "" {
		return apierror.InvalidArgument("git_remote_settings.default_branch is required")
	}
	if err := s.checkRemoteURL(gr.GetUrl()); err != nil {
		return err
	}
	if v := gr.GetAuthenticationTokenSecretVersion(); v != "" && !secretVersRE.MatchString(v) {
		return apierror.InvalidArgument("git_remote_settings.authentication_token_secret_version %q must be projects/*/secrets/*/versions/*", v)
	}
	return nil
}

// checkRemoteURL accepts only this instance's Secure Source Manager
// repositories, so a workspace's git never leaves the instance.
func (s *Server) checkRemoteURL(u string) error {
	base := ""
	if s.opts.GitBase != nil {
		base = strings.TrimSuffix(s.opts.GitBase(), "/")
	}
	if base == "" {
		return apierror.FailedPrecondition("git remote %q: Secure Source Manager is not served on this instance, and CloudBurrow's Dataform pushes and pulls only to its repositories", u)
	}
	prefix := base + "/sourcemanager/git/"
	rest, ok := strings.CutPrefix(u, prefix)
	if !ok || !strings.HasSuffix(rest, ".git") || strings.Count(rest, "/") != 2 {
		return apierror.InvalidArgument("git_remote_settings.url %q must be a Secure Source Manager repository of this instance, %sPROJECT/LOCATION/REPOSITORY.git (its git_https URI)", u, prefix)
	}
	return nil
}

func (s *Server) checkContainer(location, container string) error {
	if container == "" {
		return nil
	}
	if m := folderRE.FindStringSubmatch(container); m != nil && m[1] == location {
		if _, ok := s.folders[container]; !ok {
			return apierror.NotFound("Folder %s not found", container)
		}
		return nil
	}
	if m := teamFolderRE.FindStringSubmatch(container); m != nil && m[1] == location {
		if _, ok := s.teamFolders[container]; !ok {
			return apierror.NotFound("TeamFolder %s not found", container)
		}
		return nil
	}
	return apierror.InvalidArgument("containing folder %q must be a Folder or TeamFolder in %s", container, location)
}

// teamFolderOf is the team folder a container is (or is in), or "".
func (s *Server) teamFolderOf(container string) string {
	for i := 0; i < 64 && container != ""; i++ {
		if teamFolderRE.MatchString(container) {
			return container
		}
		f, ok := s.folders[container]
		if !ok {
			return ""
		}
		container = f.GetContainingFolder()
	}
	return ""
}

func (s *Server) setTeamFolder(r *dfpb.Repository) {
	if tf := s.teamFolderOf(r.GetContainingFolder()); tf != "" {
		r.TeamFolderName = proto.String(tf)
	} else {
		r.TeamFolderName = nil
	}
}

func (s *Server) GetRepository(_ context.Context, req *dfpb.GetRepositoryRequest) (*dfpb.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repo(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(r), nil
}

// repo finds a repository; the caller holds mu.
func (s *Server) repo(name string) (*dfpb.Repository, error) {
	if !repositoryRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*", name)
	}
	r, ok := s.repos[name]
	if !ok {
		return nil, apierror.NotFound("Repository %s not found", name)
	}
	return r, nil
}

func (s *Server) ListRepositories(_ context.Context, req *dfpb.ListRepositoriesRequest) (*dfpb.ListRepositoriesResponse, error) {
	if _, err := checkLocation("parent", req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*dfpb.Repository
	for _, k := range childrenOf(s.repos, req.GetParent(), "repositories") {
		all = append(all, clone(s.repos[k]))
	}
	s.mu.Unlock()
	all, err := filterBy(all, req.GetFilter(), func(r *dfpb.Repository) map[string]string {
		return map[string]string{"name": r.GetName(), "display_name": r.GetDisplayName()}
	})
	if err != nil {
		return nil, err
	}
	if err := orderBy(all, req.GetOrderBy(), func(r *dfpb.Repository) map[string]string {
		return map[string]string{"name": r.GetName(), "display_name": r.GetDisplayName(), "create_time": r.GetCreateTime().AsTime().Format(time.RFC3339Nano)}
	}); err != nil {
		return nil, err
	}
	start, end, next, err := page("repos:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.ListRepositoriesResponse{Repositories: all[start:end], NextPageToken: next}, nil
}

func (s *Server) UpdateRepository(ctx context.Context, req *dfpb.UpdateRepositoryRequest) (*dfpb.Repository, error) {
	in := req.GetRepository()
	if in == nil {
		return nil, apierror.InvalidArgument("repository is required")
	}
	s.mu.Lock()
	cur, err := s.repo(in.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	next := clone(cur)
	s.mu.Unlock()
	settable := []string{"display_name", "git_remote_settings", "npmrc_environment_variables_secret_version", "workspace_compilation_overrides", "labels", "service_account", "set_authenticated_user_admin"}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = settable
	}
	for _, p := range paths {
		switch p {
		case "display_name":
			next.DisplayName = in.GetDisplayName()
		case "git_remote_settings":
			next.GitRemoteSettings = clone(in.GetGitRemoteSettings())
		case "npmrc_environment_variables_secret_version":
			next.NpmrcEnvironmentVariablesSecretVersion = in.GetNpmrcEnvironmentVariablesSecretVersion()
		case "workspace_compilation_overrides":
			next.WorkspaceCompilationOverrides = clone(in.GetWorkspaceCompilationOverrides())
		case "labels":
			next.Labels = in.GetLabels()
		case "service_account":
			next.ServiceAccount = in.GetServiceAccount()
		case "set_authenticated_user_admin":
			next.SetAuthenticatedUserAdmin = in.GetSetAuthenticatedUserAdmin()
		case "kms_key_name":
			// "It is not possible to add or update the encryption key
			// after the repository is created."
			if in.GetKmsKeyName() != cur.GetKmsKeyName() {
				return nil, apierror.InvalidArgument("kms_key_name cannot be added or changed after the repository is created")
			}
		case "containing_folder":
			return nil, apierror.InvalidArgument("containing_folder can only be changed with MoveRepository")
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of Repository that can be updated", p)
		}
	}
	if err := s.checkRepository(ctx, next); err != nil {
		return nil, err
	}
	if gr := next.GetGitRemoteSettings(); gr != nil {
		gr.EffectiveDefaultBranch = gr.GetDefaultBranch()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[next.GetName()]; !ok {
		return nil, apierror.NotFound("Repository %s not found", next.GetName())
	}
	s.repos[next.GetName()] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (s *Server) DeleteRepository(_ context.Context, req *dfpb.DeleteRepositoryRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetName()); err != nil {
		return nil, err
	}
	if err := s.deleteRepoLocked(req.GetName(), req.GetForce()); err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// deleteRepoLocked deletes a repository; without force it must have no
// child resources, as Google refuses ("force: If set to true, any child
// resources of this repository will also be deleted. Otherwise, the
// request will only succeed if the repository has no child resources.").
func (s *Server) deleteRepoLocked(name string, force bool) error {
	children := func() []string {
		var out []string
		for k := range s.workspaces {
			if strings.HasPrefix(k, name+"/") {
				out = append(out, k)
			}
		}
		for k := range s.releases {
			if strings.HasPrefix(k, name+"/") {
				out = append(out, k)
			}
		}
		for k := range s.workflows {
			if strings.HasPrefix(k, name+"/") {
				out = append(out, k)
			}
		}
		return out
	}()
	if len(children) > 0 && !force {
		return apierror.FailedPrecondition("Repository %s has child resources (%d); delete them or set force", name, len(children))
	}
	for name2, c := range s.cancels {
		if strings.HasPrefix(name2, name+"/") {
			c()
		}
	}
	in := func(k string) bool { return strings.HasPrefix(k, name+"/") }
	for k := range s.workspaces {
		if in(k) {
			_ = os.RemoveAll(s.workspaceDir(k))
		}
	}
	dropIn(s.workspaces, in)
	dropIn(s.releases, in)
	dropIn(s.workflows, in)
	dropIn(s.compilations, in)
	dropIn(s.invocations, in)
	dropIn(s.policies, func(k string) bool { return k == name || in(k) })
	_ = os.RemoveAll(s.repoDir(name))
	delete(s.repos, name)
	delete(s.sealed, name)
	return nil
}

func (s *Server) MoveRepository(_ context.Context, req *dfpb.MoveRepositoryRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repo(req.GetName())
	if err != nil {
		return nil, err
	}
	loc := repositoryRE.FindStringSubmatch(req.GetName())[1]
	dest := req.GetDestinationContainingFolder()
	if err := s.checkContainer(loc, dest); err != nil {
		return nil, err
	}
	start := s.now()
	r.ContainingFolder = nil
	if dest != "" {
		r.ContainingFolder = proto.String(dest)
	}
	s.setTeamFolder(r)
	md := &dfpb.MoveRepositoryMetadata{CreateTime: start, EndTime: s.now(), Target: req.GetName(), State: dfpb.MoveRepositoryMetadata_SUCCESS, PercentComplete: 100}
	op, err := s.done(loc, md, &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

// ---- Folders and TeamFolders ----

func (s *Server) CreateFolder(ctx context.Context, req *dfpb.CreateFolderRequest) (*dfpb.Folder, error) {
	if _, err := checkLocation("parent", req.GetParent()); err != nil {
		return nil, err
	}
	f := clone(req.GetFolder())
	if f == nil || strings.TrimSpace(f.GetDisplayName()) == "" {
		return nil, apierror.InvalidArgument("folder.display_name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkContainer(req.GetParent(), f.GetContainingFolder()); err != nil {
		return nil, err
	}
	id, _ := ctx.Value(resourceIDKey).(string)
	if id == "" {
		id = newUUID()
	}
	f.Name = req.GetParent() + "/folders/" + id
	if _, ok := s.folders[f.Name]; ok {
		return nil, apierror.AlreadyExists("Folder %s already exists", f.Name)
	}
	f.CreateTime, f.UpdateTime = s.now(), s.now()
	f.TeamFolderName = s.teamFolderOf(f.GetContainingFolder())
	f.InternalMetadata = nil
	f.CreatorIamPrincipal = proto.String(callerOf(ctx))
	s.folders[f.Name] = f
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(f), nil
}

func (s *Server) folder(name string) (*dfpb.Folder, error) {
	if !folderRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/folders/*", name)
	}
	f, ok := s.folders[name]
	if !ok {
		return nil, apierror.NotFound("Folder %s not found", name)
	}
	return f, nil
}

func (s *Server) GetFolder(_ context.Context, req *dfpb.GetFolderRequest) (*dfpb.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.folder(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(f), nil
}

func (s *Server) UpdateFolder(_ context.Context, req *dfpb.UpdateFolderRequest) (*dfpb.Folder, error) {
	in := req.GetFolder()
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.folder(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"display_name"}
	}
	next := clone(f)
	for _, p := range paths {
		switch p {
		case "display_name":
			if strings.TrimSpace(in.GetDisplayName()) == "" {
				return nil, apierror.InvalidArgument("folder.display_name is required")
			}
			next.DisplayName = in.GetDisplayName()
		case "containing_folder":
			return nil, apierror.InvalidArgument("containing_folder can only be changed with MoveFolder")
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of Folder that can be updated", p)
		}
	}
	next.UpdateTime = s.now()
	s.folders[next.Name] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

// folderEmpty reports whether nothing is in a container.
func (s *Server) folderEmpty(container string) bool {
	for _, f := range s.folders {
		if f.GetContainingFolder() == container {
			return false
		}
	}
	for _, r := range s.repos {
		if r.GetContainingFolder() == container {
			return false
		}
	}
	return true
}

func (s *Server) DeleteFolder(_ context.Context, req *dfpb.DeleteFolderRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.folder(req.GetName()); err != nil {
		return nil, err
	}
	if !s.folderEmpty(req.GetName()) {
		return nil, apierror.FailedPrecondition("Folder %s is not empty; use DeleteFolderTree", req.GetName())
	}
	delete(s.folders, req.GetName())
	delete(s.policies, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// deleteTreeLocked deletes a container's folders and repositories, and the
// container itself when it is a folder.
func (s *Server) deleteTreeLocked(container string, force bool) error {
	var repos, folders []string
	var walk func(c string)
	walk = func(c string) {
		for _, k := range sortedKeys(s.folders) {
			if s.folders[k].GetContainingFolder() == c {
				folders = append(folders, k)
				walk(k)
			}
		}
		for _, k := range sortedKeys(s.repos) {
			if s.repos[k].GetContainingFolder() == c {
				repos = append(repos, k)
			}
		}
	}
	walk(container)
	if !force {
		for _, r := range repos {
			for k := range s.releases {
				if strings.HasPrefix(k, r+"/") {
					return apierror.FailedPrecondition("Repository %s in the tree has release configs; set force to delete them", r)
				}
			}
			for k := range s.workflows {
				if strings.HasPrefix(k, r+"/") {
					return apierror.FailedPrecondition("Repository %s in the tree has workflow configs; set force to delete them", r)
				}
			}
		}
	}
	for _, r := range repos {
		if err := s.deleteRepoLocked(r, true); err != nil {
			return err
		}
	}
	for _, f := range folders {
		delete(s.folders, f)
		delete(s.policies, f)
	}
	return nil
}

func (s *Server) DeleteFolderTree(_ context.Context, req *dfpb.DeleteFolderTreeRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.folder(req.GetName()); err != nil {
		return nil, err
	}
	start := s.now()
	if err := s.deleteTreeLocked(req.GetName(), req.GetForce()); err != nil {
		return nil, err
	}
	delete(s.folders, req.GetName())
	delete(s.policies, req.GetName())
	loc := folderRE.FindStringSubmatch(req.GetName())[1]
	md := &dfpb.DeleteFolderTreeMetadata{CreateTime: start, EndTime: s.now(), Target: req.GetName(), State: dfpb.DeleteFolderTreeMetadata_SUCCEEDED, PercentComplete: 100}
	op, err := s.done(loc, md, &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

func (s *Server) MoveFolder(_ context.Context, req *dfpb.MoveFolderRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.folder(req.GetName())
	if err != nil {
		return nil, err
	}
	loc := folderRE.FindStringSubmatch(req.GetName())[1]
	dest := req.GetDestinationContainingFolder()
	if err := s.checkContainer(loc, dest); err != nil {
		return nil, err
	}
	// A folder cannot move into itself or one of its own subfolders.
	for c := dest; c != ""; {
		if c == req.GetName() {
			return nil, apierror.InvalidArgument("Folder %s cannot be moved into itself or its subfolder", req.GetName())
		}
		pf, ok := s.folders[c]
		if !ok {
			break
		}
		c = pf.GetContainingFolder()
	}
	start := s.now()
	f.ContainingFolder = dest
	f.UpdateTime = s.now()
	// Everything under it now belongs to the destination's team folder.
	tf := s.teamFolderOf(dest)
	var walk func(c string)
	walk = func(c string) {
		for _, x := range s.folders {
			if x.GetContainingFolder() == c {
				x.TeamFolderName = tf
				walk(x.GetName())
			}
		}
		for _, r := range s.repos {
			if r.GetContainingFolder() == c {
				s.setTeamFolder(r)
			}
		}
	}
	f.TeamFolderName = tf
	walk(f.GetName())
	md := &dfpb.MoveFolderMetadata{CreateTime: start, EndTime: s.now(), Target: req.GetName(), State: dfpb.MoveFolderMetadata_SUCCESS, PercentComplete: 100}
	op, err := s.done(loc, md, &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

// contents is a container's folders, then its repositories, filtered by
// display name and ordered as QueryFolderContents documents. The caller
// holds mu.
func (s *Server) contents(container, location, filter, order string) (folders []*dfpb.Folder, repos []*dfpb.Repository, err error) {
	want := ""
	exact := false
	if strings.TrimSpace(filter) != "" {
		m := displayFilterR.FindStringSubmatch(filter)
		if m == nil {
			return nil, nil, apierror.InvalidArgument("filter %q: only display_name=\"...\" is supported", filter)
		}
		want, exact = m[2], m[1] == "="
	}
	match := func(d string) bool {
		if want == "" && !exact {
			return true
		}
		if exact {
			return d == want
		}
		return strings.Contains(strings.ToLower(d), strings.ToLower(want))
	}
	for _, k := range sortedKeys(s.folders) {
		f := s.folders[k]
		if f.GetContainingFolder() == container && strings.HasPrefix(k, location+"/") && match(f.GetDisplayName()) {
			folders = append(folders, clone(f))
		}
	}
	for _, k := range sortedKeys(s.repos) {
		r := s.repos[k]
		if r.GetContainingFolder() == container && strings.HasPrefix(k, location+"/") && match(r.GetDisplayName()) {
			repos = append(repos, clone(r))
		}
	}
	key, desc, err := parseOrder(order, map[string]string{"display_name": "display_name", "create_time": "create_time", "created_at": "create_time", "last_modified_time": "update_time", "last_modified_at": "update_time"})
	if err != nil {
		return nil, nil, err
	}
	fv := func(f *dfpb.Folder) string {
		switch key {
		case "create_time":
			return f.GetCreateTime().AsTime().Format(time.RFC3339Nano)
		case "update_time":
			return f.GetUpdateTime().AsTime().Format(time.RFC3339Nano)
		}
		return f.GetDisplayName()
	}
	rv := func(r *dfpb.Repository) string {
		switch key {
		case "create_time", "update_time":
			return r.GetCreateTime().AsTime().Format(time.RFC3339Nano)
		}
		if r.GetDisplayName() != "" {
			return r.GetDisplayName()
		}
		return r.GetName()[strings.LastIndex(r.GetName(), "/")+1:]
	}
	sort.SliceStable(folders, func(i, j int) bool { return less(fv(folders[i]), fv(folders[j]), desc) })
	sort.SliceStable(repos, func(i, j int) bool { return less(rv(repos[i]), rv(repos[j]), desc) })
	return folders, repos, nil
}

func less(a, b string, desc bool) bool {
	if desc {
		return a > b
	}
	return a < b
}

func parseOrder(order string, keys map[string]string) (key string, desc bool, err error) {
	f := strings.Fields(order)
	if len(f) == 0 {
		return "display_name", false, nil
	}
	k, ok := keys[f[0]]
	if !ok || len(f) > 2 || (len(f) == 2 && f[1] != "desc" && f[1] != "asc") {
		return "", false, apierror.InvalidArgument("order_by %q is not supported", order)
	}
	return k, len(f) == 2 && f[1] == "desc", nil
}

func (s *Server) QueryFolderContents(_ context.Context, req *dfpb.QueryFolderContentsRequest) (*dfpb.QueryFolderContentsResponse, error) {
	s.mu.Lock()
	if _, err := s.folder(req.GetFolder()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	loc := folderRE.FindStringSubmatch(req.GetFolder())[1]
	fs, rs, err := s.contents(req.GetFolder(), loc, req.GetFilter(), req.GetOrderBy())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*dfpb.QueryFolderContentsResponse_FolderContentsEntry
	for _, f := range fs {
		all = append(all, &dfpb.QueryFolderContentsResponse_FolderContentsEntry{Entry: &dfpb.QueryFolderContentsResponse_FolderContentsEntry_Folder{Folder: f}})
	}
	for _, r := range rs {
		all = append(all, &dfpb.QueryFolderContentsResponse_FolderContentsEntry{Entry: &dfpb.QueryFolderContentsResponse_FolderContentsEntry_Repository{Repository: r}})
	}
	start, end, next, err := page("folder:"+req.GetFolder()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryFolderContentsResponse{Entries: all[start:end], NextPageToken: next}, nil
}

func (s *Server) QueryUserRootContents(_ context.Context, req *dfpb.QueryUserRootContentsRequest) (*dfpb.QueryUserRootContentsResponse, error) {
	if _, err := checkLocation("location", req.GetLocation()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	fs, rs, err := s.contents("", req.GetLocation(), req.GetFilter(), req.GetOrderBy())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*dfpb.QueryUserRootContentsResponse_RootContentsEntry
	for _, f := range fs {
		all = append(all, &dfpb.QueryUserRootContentsResponse_RootContentsEntry{Entry: &dfpb.QueryUserRootContentsResponse_RootContentsEntry_Folder{Folder: f}})
	}
	for _, r := range rs {
		all = append(all, &dfpb.QueryUserRootContentsResponse_RootContentsEntry{Entry: &dfpb.QueryUserRootContentsResponse_RootContentsEntry_Repository{Repository: r}})
	}
	start, end, next, err := page("root:"+req.GetLocation()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryUserRootContentsResponse{Entries: all[start:end], NextPageToken: next}, nil
}

func (s *Server) CreateTeamFolder(ctx context.Context, req *dfpb.CreateTeamFolderRequest) (*dfpb.TeamFolder, error) {
	if _, err := checkLocation("parent", req.GetParent()); err != nil {
		return nil, err
	}
	t := clone(req.GetTeamFolder())
	if t == nil || strings.TrimSpace(t.GetDisplayName()) == "" {
		return nil, apierror.InvalidArgument("team_folder.display_name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := ctx.Value(resourceIDKey).(string)
	if id == "" {
		id = newUUID()
	}
	t.Name = req.GetParent() + "/teamFolders/" + id
	if _, ok := s.teamFolders[t.Name]; ok {
		return nil, apierror.AlreadyExists("TeamFolder %s already exists", t.Name)
	}
	t.CreateTime, t.UpdateTime = s.now(), s.now()
	t.InternalMetadata = nil
	t.CreatorIamPrincipal = proto.String(callerOf(ctx))
	s.teamFolders[t.Name] = t
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(t), nil
}

func (s *Server) teamFolder(name string) (*dfpb.TeamFolder, error) {
	if !teamFolderRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/teamFolders/*", name)
	}
	t, ok := s.teamFolders[name]
	if !ok {
		return nil, apierror.NotFound("TeamFolder %s not found", name)
	}
	return t, nil
}

func (s *Server) GetTeamFolder(_ context.Context, req *dfpb.GetTeamFolderRequest) (*dfpb.TeamFolder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.teamFolder(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(t), nil
}

func (s *Server) UpdateTeamFolder(_ context.Context, req *dfpb.UpdateTeamFolderRequest) (*dfpb.TeamFolder, error) {
	in := req.GetTeamFolder()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.teamFolder(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(t)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"display_name"}
	}
	for _, p := range paths {
		if p != "display_name" {
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of TeamFolder that can be updated", p)
		}
		if strings.TrimSpace(in.GetDisplayName()) == "" {
			return nil, apierror.InvalidArgument("team_folder.display_name is required")
		}
		next.DisplayName = in.GetDisplayName()
	}
	next.UpdateTime = s.now()
	s.teamFolders[next.Name] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (s *Server) DeleteTeamFolder(_ context.Context, req *dfpb.DeleteTeamFolderRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.teamFolder(req.GetName()); err != nil {
		return nil, err
	}
	if !s.folderEmpty(req.GetName()) {
		return nil, apierror.FailedPrecondition("TeamFolder %s is not empty; use DeleteTeamFolderTree", req.GetName())
	}
	delete(s.teamFolders, req.GetName())
	delete(s.policies, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) DeleteTeamFolderTree(_ context.Context, req *dfpb.DeleteTeamFolderTreeRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.teamFolder(req.GetName()); err != nil {
		return nil, err
	}
	start := s.now()
	if err := s.deleteTreeLocked(req.GetName(), req.GetForce()); err != nil {
		return nil, err
	}
	delete(s.teamFolders, req.GetName())
	delete(s.policies, req.GetName())
	loc := teamFolderRE.FindStringSubmatch(req.GetName())[1]
	md := &dfpb.DeleteFolderTreeMetadata{CreateTime: start, EndTime: s.now(), Target: req.GetName(), State: dfpb.DeleteFolderTreeMetadata_SUCCEEDED, PercentComplete: 100}
	op, err := s.done(loc, md, &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

func (s *Server) QueryTeamFolderContents(_ context.Context, req *dfpb.QueryTeamFolderContentsRequest) (*dfpb.QueryTeamFolderContentsResponse, error) {
	s.mu.Lock()
	if _, err := s.teamFolder(req.GetTeamFolder()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	loc := teamFolderRE.FindStringSubmatch(req.GetTeamFolder())[1]
	fs, rs, err := s.contents(req.GetTeamFolder(), loc, req.GetFilter(), req.GetOrderBy())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*dfpb.QueryTeamFolderContentsResponse_TeamFolderContentsEntry
	for _, f := range fs {
		all = append(all, &dfpb.QueryTeamFolderContentsResponse_TeamFolderContentsEntry{Entry: &dfpb.QueryTeamFolderContentsResponse_TeamFolderContentsEntry_Folder{Folder: f}})
	}
	for _, r := range rs {
		all = append(all, &dfpb.QueryTeamFolderContentsResponse_TeamFolderContentsEntry{Entry: &dfpb.QueryTeamFolderContentsResponse_TeamFolderContentsEntry_Repository{Repository: r}})
	}
	start, end, next, err := page("team:"+req.GetTeamFolder()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryTeamFolderContentsResponse{Entries: all[start:end], NextPageToken: next}, nil
}

func (s *Server) SearchTeamFolders(_ context.Context, req *dfpb.SearchTeamFoldersRequest) (*dfpb.SearchTeamFoldersResponse, error) {
	if _, err := checkLocation("location", req.GetLocation()); err != nil {
		return nil, err
	}
	want, exact := "", false
	if strings.TrimSpace(req.GetFilter()) != "" {
		m := displayFilterR.FindStringSubmatch(req.GetFilter())
		if m == nil {
			return nil, apierror.InvalidArgument("filter %q: only display_name=\"...\" is supported", req.GetFilter())
		}
		want, exact = m[2], m[1] == "="
	}
	key, desc, err := parseOrder(req.GetOrderBy(), map[string]string{"display_name": "display_name", "create_time": "create_time", "last_modified_time": "update_time"})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var ts []*dfpb.TeamFolder
	for _, k := range childrenOf(s.teamFolders, req.GetLocation(), "teamFolders") {
		t := s.teamFolders[k]
		d := t.GetDisplayName()
		if (exact && d != want) || (!exact && want != "" && !strings.Contains(strings.ToLower(d), strings.ToLower(want))) {
			continue
		}
		ts = append(ts, clone(t))
	}
	s.mu.Unlock()
	v := func(t *dfpb.TeamFolder) string {
		switch key {
		case "create_time":
			return t.GetCreateTime().AsTime().Format(time.RFC3339Nano)
		case "update_time":
			return t.GetUpdateTime().AsTime().Format(time.RFC3339Nano)
		}
		return t.GetDisplayName()
	}
	sort.SliceStable(ts, func(i, j int) bool { return less(v(ts[i]), v(ts[j]), desc) })
	start, end, next, err := page("search:"+req.GetLocation()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(ts))
	if err != nil {
		return nil, err
	}
	out := &dfpb.SearchTeamFoldersResponse{NextPageToken: next}
	for _, t := range ts[start:end] {
		out.Results = append(out.Results, &dfpb.SearchTeamFoldersResponse_TeamFolderSearchResult{Entry: &dfpb.SearchTeamFoldersResponse_TeamFolderSearchResult_TeamFolder{TeamFolder: t}})
	}
	return out, nil
}

// ---- Config ----

func (s *Server) GetConfig(_ context.Context, req *dfpb.GetConfigRequest) (*dfpb.Config, error) {
	m := configRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/config", req.GetName())
	}
	if _, err := checkLocation("name", m[1]); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.configs[req.GetName()]; ok {
		return clone(c), nil
	}
	return &dfpb.Config{Name: req.GetName()}, nil
}

func (s *Server) UpdateConfig(ctx context.Context, req *dfpb.UpdateConfigRequest) (*dfpb.Config, error) {
	in := req.GetConfig()
	cur, err := s.GetConfig(ctx, &dfpb.GetConfigRequest{Name: in.GetName()})
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"default_kms_key_name"}
	}
	for _, p := range paths {
		if p != "default_kms_key_name" {
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of Config that can be updated", p)
		}
		if k := in.GetDefaultKmsKeyName(); k != "" {
			m := configRE.FindStringSubmatch(cur.GetName())
			loc := m[1][strings.LastIndex(m[1], "/")+1:]
			if err := cmek.CheckName("default_kms_key_name", k, loc); err != nil {
				return nil, keyError("default_kms_key_name", err)
			}
			project := strings.Split(cur.GetName(), "/")[1]
			if err := s.opts.Keys.Usable(ctx, project, cmek.ServiceAgent(project, agentDomain), k); err != nil {
				return nil, keyError("default_kms_key_name cannot be used", err)
			}
			cur.DefaultKmsKeyName = k
			continue
		}
		cur.DefaultKmsKeyName = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[cur.Name] = cur
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(cur), nil
}

// ---- IAM ----

// iamResource checks the resource is one Dataform keeps a policy on and
// that it exists. The caller holds mu.
func (s *Server) iamResource(name string) error {
	var ok bool
	switch {
	case repositoryRE.MatchString(name):
		_, ok = s.repos[name]
	case workspaceRE.MatchString(name):
		_, ok = s.workspaces[name]
	case folderRE.MatchString(name):
		_, ok = s.folders[name]
	case teamFolderRE.MatchString(name):
		_, ok = s.teamFolders[name]
	default:
		return apierror.InvalidArgument("resource %q is not a Dataform repository, workspace, folder or team folder", name)
	}
	if !ok {
		return apierror.NotFound("%s not found", name)
	}
	return nil
}

func (s *Server) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.iamResource(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Get(s.policies[req.GetResource()], req.GetOptions())
}

func (s *Server) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.iamResource(req.GetResource()); err != nil {
		return nil, err
	}
	next, err := iampolicy.Set(s.policies[req.GetResource()], req)
	if err != nil {
		return nil, err
	}
	s.policies[req.GetResource()] = &next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return next.Proto(), nil
}

func (s *Server) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.iamResource(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}
