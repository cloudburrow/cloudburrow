package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Secure Source Manager (#1521): instances and repositories of
// google.cloud.securesourcemanager.v1, each repository a bare git
// repository in the instance's state directory, served over git's smart
// HTTP protocol on this port (securesourcemanagergit.go). A push runs the
// push triggers of the repository's .cloudbuild/triggers.yaml as Cloud
// Builds, as Google's "Connect to Cloud Build" page documents: the
// repository is the hosted git service a Cloud Build trigger needs here.
//
// An instance is ACTIVE when its create returns, since nothing is
// provisioned. Its host_config names this port rather than
// {instance}-{project number}-git.{location}.sourcemanager.dev, which
// resolves only on Google. Hooks, branch rules, pull requests, issues and
// the Fetch* reads are UNIMPLEMENTED; so are private instances (they need
// Private Service Connect) and a repository's gitignores and license
// templates, which are Google's.

// SecureSourceManagerServer serves Secure Source Manager's instances and
// repositories.
type SecureSourceManagerServer struct {
	ssmpb.UnimplementedSecureSourceManagerServer
	s *Server
	// gitBase is this port's http://host:port, or "" before it listens.
	gitBase func() string
}

// SecureSourceManager returns the Secure Source Manager service; gitBase is
// the port's base URL, which repositories' git URLs name.
func (s *Server) SecureSourceManager(gitBase func() string) *SecureSourceManagerServer {
	return &SecureSourceManagerServer{s: s, gitBase: gitBase}
}

// Register adds the service to a gRPC server or a JSON transcoder.
func (m *SecureSourceManagerServer) Register(g grpc.ServiceRegistrar) {
	ssmpb.RegisterSecureSourceManagerServer(g, m)
}

var ssmState = struct {
	mu        sync.Mutex
	root      string // "" keeps repositories in a temporary directory
	instances map[string]*ssmpb.Instance
	repos     map[string]*ssmpb.Repository
	// policies are the instances' and repositories' IAM policies, by
	// resource name (securesourcemanageriam.go).
	policies map[string]iampolicy.Stored
}{instances: map[string]*ssmpb.Instance{}, repos: map[string]*ssmpb.Repository{}, policies: map[string]iampolicy.Stored{}}

// ssmIDRE is the instance and repository ID shape Google documents: lower
// case letters, digits and hyphens, starting with a letter, at most 63.
var ssmIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

const ssmStateFile = "state.json"

// UseSourceManagerRoot sets the directory that holds the repositories and
// their metadata, and loads what a previous run saved there.
func UseSourceManagerRoot(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	ssmState.root = dir
	b, err := os.ReadFile(filepath.Join(dir, ssmStateFile))
	if err != nil {
		return nil
	}
	var saved struct {
		Instances    []json.RawMessage           `json:"instances"`
		Repositories []json.RawMessage           `json:"repositories"`
		Policies     map[string]iampolicy.Stored `json:"policies,omitempty"`
	}
	if json.Unmarshal(b, &saved) != nil {
		return nil
	}
	for _, raw := range saved.Instances {
		in := &ssmpb.Instance{}
		if protojson.Unmarshal(raw, in) == nil {
			ssmState.instances[in.GetName()] = in
		}
	}
	for _, raw := range saved.Repositories {
		r := &ssmpb.Repository{}
		if protojson.Unmarshal(raw, r) == nil {
			ssmState.repos[r.GetName()] = r
		}
	}
	for name, p := range saved.Policies {
		if ssmState.instances[name] != nil || ssmState.repos[name] != nil {
			ssmState.policies[name] = p
		}
	}
	return nil
}

// ssmRootLocked is the repositories' directory, made on first use.
func ssmRootLocked() (string, error) {
	if ssmState.root == "" {
		dir, err := os.MkdirTemp("", "cloudburrow-ssm-")
		if err != nil {
			return "", err
		}
		ssmState.root = dir
	}
	return ssmState.root, nil
}

func saveSSMLocked() {
	root, err := ssmRootLocked()
	if err != nil {
		return
	}
	var saved struct {
		Instances    []json.RawMessage           `json:"instances"`
		Repositories []json.RawMessage           `json:"repositories"`
		Policies     map[string]iampolicy.Stored `json:"policies,omitempty"`
	}
	saved.Policies = ssmState.policies
	for _, name := range sortedKeys(ssmState.instances) {
		b, _ := protojson.Marshal(ssmState.instances[name])
		saved.Instances = append(saved.Instances, b)
	}
	for _, name := range sortedKeys(ssmState.repos) {
		b, _ := protojson.Marshal(ssmState.repos[name])
		saved.Repositories = append(saved.Repositories, b)
	}
	b, _ := json.MarshalIndent(saved, "", "  ")
	tmp := filepath.Join(root, ssmStateFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(root, ssmStateFile))
	}
}

// repoDir is a repository's bare git directory under root:
// {project}/{location}/{repository}.git, the path its git URL serves.
func repoDir(root, name string) string {
	parts := strings.Split(name, "/") // projects/p/locations/l/repositories/r
	return filepath.Join(root, parts[1], parts[3], parts[5]+".git")
}

// gitURL is a repository's clone URL on this port.
func (m *SecureSourceManagerServer) gitURL(name string) string {
	parts := strings.Split(name, "/")
	base := ""
	if m.gitBase != nil {
		base = strings.TrimSuffix(m.gitBase(), "/")
	}
	return base + ssmGitPrefix + parts[1] + "/" + parts[3] + "/" + parts[5] + ".git"
}

func parseSSMName(name, collection string) (parent, id string, err error) {
	i := strings.LastIndex(name, "/"+collection+"/")
	if i < 0 {
		return "", "", apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/%s/*", name, collection)
	}
	parent, id = name[:i], name[i+len(collection)+2:]
	if _, _, err := resource.ParseLocation(parent); err != nil || id == "" || strings.Contains(id, "/") {
		return "", "", apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/%s/*", name, collection)
	}
	return parent, id, nil
}

func (m *SecureSourceManagerServer) done(parent, target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	op := m.s.ops.Create(parent, target)
	now := timestamppb.Now()
	_ = m.s.ops.SetMetadata(op.Name, &ssmpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	_ = m.s.ops.Succeed(op.Name, resp)
	return m.s.toProtoOperation(op.Name)
}

// ListInstances lists a location's instances, by name.
func (m *SecureSourceManagerServer) ListInstances(_ context.Context, req *ssmpb.ListInstancesRequest) (*ssmpb.ListInstancesResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetPageToken() != "" || req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("ListInstances page_token, filter and order_by are not supported")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	out := &ssmpb.ListInstancesResponse{}
	for _, name := range sortedKeys(ssmState.instances) {
		if strings.HasPrefix(name, req.GetParent()+"/instances/") {
			out.Instances = append(out.Instances, proto.Clone(ssmState.instances[name]).(*ssmpb.Instance))
		}
	}
	return out, nil
}

// GetInstance reads an instance.
func (m *SecureSourceManagerServer) GetInstance(_ context.Context, req *ssmpb.GetInstanceRequest) (*ssmpb.Instance, error) {
	if _, _, err := parseSSMName(req.GetName(), "instances"); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	in, ok := ssmState.instances[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("instance %q not found", req.GetName())
	}
	return proto.Clone(in).(*ssmpb.Instance), nil
}

// CreateInstance stores an ACTIVE instance; its operation is done when it
// returns.
func (m *SecureSourceManagerServer) CreateInstance(_ context.Context, req *ssmpb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !ssmIDRE.MatchString(req.GetInstanceId()) {
		return nil, apierror.InvalidArgument("invalid instance_id %q: lower case letters, digits and hyphens, starting with a letter", req.GetInstanceId())
	}
	in := proto.Clone(req.GetInstance()).(*ssmpb.Instance)
	if in == nil {
		in = &ssmpb.Instance{}
	}
	if in.GetPrivateConfig().GetIsPrivate() {
		return nil, apierror.Unimplemented("private instances are not supported: they are reached through Private Service Connect, which no instance here provides")
	}
	name := req.GetParent() + "/instances/" + req.GetInstanceId()
	now := timestamppb.New(time.Now())
	in.Name, in.CreateTime, in.UpdateTime = name, now, now
	in.State, in.StateNote = ssmpb.Instance_ACTIVE, ssmpb.Instance_STATE_NOTE_UNSPECIFIED
	base := ""
	if m.gitBase != nil {
		base = strings.TrimSuffix(m.gitBase(), "/")
	}
	host := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	in.HostConfig = &ssmpb.Instance_HostConfig{Html: host, Api: host, GitHttp: host}
	ssmState.mu.Lock()
	if _, ok := ssmState.instances[name]; ok {
		ssmState.mu.Unlock()
		return nil, apierror.AlreadyExists("instance %q already exists", name)
	}
	ssmState.instances[name] = in
	saveSSMLocked()
	ssmState.mu.Unlock()
	return m.done(req.GetParent(), name, "create", proto.Clone(in))
}

// DeleteInstance removes an instance; one that still holds repositories is
// refused unless force is set, which deletes them too.
func (m *SecureSourceManagerServer) DeleteInstance(_ context.Context, req *ssmpb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	parent, _, err := parseSSMName(req.GetName(), "instances")
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	if _, ok := ssmState.instances[req.GetName()]; !ok {
		ssmState.mu.Unlock()
		return nil, apierror.NotFound("instance %q not found", req.GetName())
	}
	var held []string
	for name, r := range ssmState.repos {
		if r.GetInstance() == req.GetName() {
			held = append(held, name)
		}
	}
	if len(held) > 0 && !req.GetForce() {
		ssmState.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %q still has %d repositories: delete them, or set force", req.GetName(), len(held))
	}
	root, _ := ssmRootLocked()
	for _, name := range held {
		delete(ssmState.repos, name)
		delete(ssmState.policies, name)
		_ = os.RemoveAll(repoDir(root, name))
	}
	delete(ssmState.instances, req.GetName())
	delete(ssmState.policies, req.GetName())
	saveSSMLocked()
	ssmState.mu.Unlock()
	return m.done(parent, req.GetName(), "delete", &emptypb.Empty{})
}

// ListRepositories lists a location's repositories, or one instance's.
func (m *SecureSourceManagerServer) ListRepositories(_ context.Context, req *ssmpb.ListRepositoriesRequest) (*ssmpb.ListRepositoriesResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetPageToken() != "" || req.GetFilter() != "" {
		return nil, apierror.Unimplemented("ListRepositories page_token and filter are not supported")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	out := &ssmpb.ListRepositoriesResponse{}
	for _, name := range sortedKeys(ssmState.repos) {
		r := ssmState.repos[name]
		if strings.HasPrefix(name, req.GetParent()+"/repositories/") && (req.GetInstance() == "" || r.GetInstance() == req.GetInstance()) {
			out.Repositories = append(out.Repositories, proto.Clone(r).(*ssmpb.Repository))
		}
	}
	return out, nil
}

// GetRepository reads a repository.
func (m *SecureSourceManagerServer) GetRepository(_ context.Context, req *ssmpb.GetRepositoryRequest) (*ssmpb.Repository, error) {
	if _, _, err := parseSSMName(req.GetName(), "repositories"); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	r, ok := ssmState.repos[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("repository %q not found", req.GetName())
	}
	return proto.Clone(r).(*ssmpb.Repository), nil
}

// CreateRepository makes a bare git repository on the instance it names.
// initial_config.default_branch is its HEAD (main when unset); its
// gitignores, license and readme templates are refused.
func (m *SecureSourceManagerServer) CreateRepository(ctx context.Context, req *ssmpb.CreateRepositoryRequest) (*longrunningpb.Operation, error) {
	project, location, err := resource.ParseLocation(req.GetParent())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !ssmIDRE.MatchString(req.GetRepositoryId()) {
		return nil, apierror.InvalidArgument("invalid repository_id %q: lower case letters, digits and hyphens, starting with a letter", req.GetRepositoryId())
	}
	r := proto.Clone(req.GetRepository()).(*ssmpb.Repository)
	if r == nil {
		r = &ssmpb.Repository{}
	}
	if r.GetInstance() == "" {
		return nil, apierror.InvalidArgument("repository.instance is required")
	}
	ip, _, err := parseSSMName(r.GetInstance(), "instances")
	if err != nil {
		return nil, err
	}
	if ip != "projects/"+project+"/locations/"+location {
		return nil, apierror.InvalidArgument("repository.instance %q must be in %s", r.GetInstance(), req.GetParent())
	}
	ic := r.GetInitialConfig()
	if len(ic.GetGitignores()) > 0 || ic.GetLicense() != "" || ic.GetReadme() != "" {
		return nil, apierror.Unimplemented("initial_config gitignores, license and readme are not supported: their templates are Google's; push the files instead")
	}
	branch := ic.GetDefaultBranch()
	if branch == "" {
		branch = "main"
	}
	if exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branch).Run() != nil {
		return nil, apierror.InvalidArgument("initial_config.default_branch %q is not a valid branch name", branch)
	}
	name := req.GetParent() + "/repositories/" + req.GetRepositoryId()
	now := timestamppb.New(time.Now())
	r.Name, r.Uid, r.CreateTime, r.UpdateTime = name, uuid.NewString(), now, now
	r.Etag = fmt.Sprintf("%x", now.AsTime().UnixNano())
	r.Uris = &ssmpb.Repository_URIs{GitHttps: m.gitURL(name), Html: m.gitURL(name), Api: m.gitURL(name)}

	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, ok := ssmState.instances[r.GetInstance()]; !ok {
		return nil, apierror.NotFound("instance %q not found", r.GetInstance())
	}
	if _, ok := ssmState.repos[name]; ok {
		return nil, apierror.AlreadyExists("repository %q already exists", name)
	}
	root, err := ssmRootLocked()
	if err != nil {
		return nil, apierror.Internal(err, "repository directory")
	}
	dir := repoDir(root, name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, apierror.Internal(err, "repository directory")
	}
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet", "--initial-branch="+branch, dir).CombinedOutput(); err != nil {
		return nil, apierror.Internal(err, "git init: %s", out)
	}
	// git http-backend accepts pushes only from an authenticated user
	// unless the repository allows them.
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, apierror.Internal(err, "git config: %s", out)
	}
	ssmState.repos[name] = r
	saveSSMLocked()
	return m.done(req.GetParent(), name, "create", proto.Clone(r))
}

// UpdateRepository changes a repository's description, the one field the
// update mask may name here.
func (m *SecureSourceManagerServer) UpdateRepository(_ context.Context, req *ssmpb.UpdateRepositoryRequest) (*longrunningpb.Operation, error) {
	in := req.GetRepository()
	parent, _, err := parseSSMName(in.GetName(), "repositories")
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"description"}
	}
	for _, p := range paths {
		if p != "description" {
			return nil, apierror.Unimplemented("update_mask path %q is not supported: only description can change", p)
		}
	}
	ssmState.mu.Lock()
	r, ok := ssmState.repos[in.GetName()]
	if !ok {
		ssmState.mu.Unlock()
		return nil, apierror.NotFound("repository %q not found", in.GetName())
	}
	if req.GetValidateOnly() {
		ssmState.mu.Unlock()
		return m.done(parent, in.GetName(), "update", proto.Clone(r))
	}
	r.Description, r.UpdateTime = in.GetDescription(), timestamppb.Now()
	r.Etag = fmt.Sprintf("%x", r.UpdateTime.AsTime().UnixNano())
	out := proto.Clone(r)
	saveSSMLocked()
	ssmState.mu.Unlock()
	return m.done(parent, in.GetName(), "update", out)
}

// DeleteRepository removes a repository and its git directory.
func (m *SecureSourceManagerServer) DeleteRepository(_ context.Context, req *ssmpb.DeleteRepositoryRequest) (*longrunningpb.Operation, error) {
	parent, _, err := parseSSMName(req.GetName(), "repositories")
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	if _, ok := ssmState.repos[req.GetName()]; !ok {
		ssmState.mu.Unlock()
		if req.GetAllowMissing() {
			return m.done(parent, req.GetName(), "delete", &emptypb.Empty{})
		}
		return nil, apierror.NotFound("repository %q not found", req.GetName())
	}
	root, _ := ssmRootLocked()
	delete(ssmState.repos, req.GetName())
	delete(ssmState.policies, req.GetName())
	_ = os.RemoveAll(repoDir(root, req.GetName()))
	saveSSMLocked()
	ssmState.mu.Unlock()
	return m.done(parent, req.GetName(), "delete", &emptypb.Empty{})
}

// lookupSSMRepo finds the repository a git path names.
func lookupSSMRepo(project, location, id string) (*ssmpb.Repository, string, bool) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	name := "projects/" + project + "/locations/" + location + "/repositories/" + id
	r, ok := ssmState.repos[name]
	if !ok {
		return nil, "", false
	}
	root, err := ssmRootLocked()
	if err != nil {
		return nil, "", false
	}
	return proto.Clone(r).(*ssmpb.Repository), repoDir(root, name), true
}

// SourceManagerLocations are the locations of a project's instances, for
// the console, which lists each through ListInstances (#1527).
func SourceManagerLocations(project string) []string {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, name := range sortedKeys(ssmState.instances) {
		parts := strings.Split(name, "/")
		if parts[1] == project && !seen[parts[3]] {
			seen[parts[3]] = true
			out = append(out, parts[3])
		}
	}
	return out
}

// SourceManagerTrigger is one trigger of a repository's triggers file, as
// the console shows it (#1527).
type SourceManagerTrigger struct {
	Name, EventType, ConfigFilePath, IncludedGitRefs, IgnoredGitRefs, ServiceAccount string
	// GitRepositoryLink is the trigger's devConnectGitRepositoryLink (#1546).
	GitRepositoryLink string
	Disabled          bool
}

// SourceManagerTriggers reads the triggers file on a repository's default
// branch: none when it has no such file, and an error when it does not
// parse, which is also why a push would run no build.
func SourceManagerTriggers(ctx context.Context, name string) ([]SourceManagerTrigger, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 {
		return nil, fmt.Errorf("invalid repository name %q", name)
	}
	_, dir, ok := lookupSSMRepo(parts[1], parts[3], parts[5])
	if !ok {
		return nil, fmt.Errorf("repository %q not found", name)
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "HEAD:"+ssmTriggersFile).Output()
	if err != nil {
		return nil, nil
	}
	ts, err := ParseSourceManagerTriggers(raw)
	if err != nil {
		return nil, err
	}
	out := make([]SourceManagerTrigger, len(ts))
	for i, t := range ts {
		ev, cfg := t.EventType, t.ConfigFilePath
		if ev == "" {
			ev = "push"
		}
		if cfg == "" {
			cfg = ".cloudbuild/cloudbuild.yaml"
		}
		out[i] = SourceManagerTrigger{Name: t.Name, EventType: ev, ConfigFilePath: cfg, IncludedGitRefs: t.IncludedGitRefs,
			IgnoredGitRefs: t.IgnoredGitRefs, ServiceAccount: t.ServiceAccount, GitRepositoryLink: t.DevConnectGitRepositoryLink, Disabled: t.Disabled}
	}
	return out, nil
}

// SourceManagerTriggersFile is the triggers file's path in a repository.
const SourceManagerTriggersFile = ssmTriggersFile
