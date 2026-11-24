package run

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	cb2pb "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cloud Build repositories, 2nd gen (#2197): google.devtools.cloudbuild.v2
// RepositoryManager, its connections to a git host and the repositories
// linked through them, as repositories.proto
// (https://raw.githubusercontent.com/googleapis/googleapis/master/google/devtools/cloudbuild/v2/repositories.proto,
// read 2026-11-23) and Google's "Connect to a GitLab host" page document
// them. A 2nd-gen trigger names a linked repository in
// repositoryEventConfig.repository, and a push to it runs the trigger's
// build (cloudbuilddevconnect.go).
//
// The git host. An instance reaches no hosted provider, so the host a
// connection can install on is this instance's own git server, the Secure
// Source Manager git URLs on the Run port (securesourcemanagergit.go):
// a GitHub Enterprise, GitLab or Bitbucket Data Center config whose hostUri
// is that server's origin (http://127.0.0.1:PORT, any loopback name) is
// installed (stage COMPLETE), and its repositories are that server's,
// remoteUri being a repository's git URL. A hostUri elsewhere, which
// Google's server would contact to check the tokens, is refused
// FAILED_PRECONDITION. A githubConfig (github.com) or bitbucketCloudConfig
// (bitbucket.org) is stored with the installation stage Google gives one
// it has not yet installed (PENDING_USER_OAUTH with no authorizer
// credential, else PENDING_INSTALL_APP), as nothing on github.com can be
// installed from here; its repository calls are FAILED_PRECONDITION, as
// they are on Google for a connection that is not installed.
//
// Secrets. Every *_secret_version must be a secret version name,
// projects/*/secrets/*/versions/*, that this instance's Secret Manager
// serves with a payload; Google reads them as the Cloud Build service agent
// when it installs the connection. A missing or unreadable one is
// FAILED_PRECONDITION, naming the field.
//
// CloudBurrow's choices where Google documents none: a connection's
// deletion deletes its repositories (gcloud builds connections delete does
// not ask for them first); FetchLinkableRepositories lists every repository
// the host serves; a token (FetchReadToken, FetchReadWriteToken) is the git
// server's, scoped to the repository, as Developer Connect's are
// (developerconnecttokens.go); etags change on every write; webhookId is
// empty, as no webhook is registered with the host (the instance runs the
// triggers itself on push).

// RepositoryManagerServer serves Cloud Build v2's connections and
// repositories.
type RepositoryManagerServer struct {
	cb2pb.UnimplementedRepositoryManagerServer
	b *BuildsServer
}

// RepositoryManager returns the Cloud Build v2 service, which reads the
// connections' secrets from this Builds service's Secret Manager.
func (b *BuildsServer) RepositoryManager() *RepositoryManagerServer {
	return &RepositoryManagerServer{b: b}
}

// Register adds the service to a gRPC server or a JSON transcoder.
func (m *RepositoryManagerServer) Register(g grpc.ServiceRegistrar) {
	cb2pb.RegisterRepositoryManagerServer(g, m)
}

// cb2State holds the connections and repositories, under ssmState.mu, as a
// repository names a Secure Source Manager repository.
var cb2State = struct {
	connections map[string]*cb2pb.Connection
	repos       map[string]*cb2pb.Repository
	loaded      string
}{connections: map[string]*cb2pb.Connection{}, repos: map[string]*cb2pb.Repository{}}

const cb2StateFile = "cloudbuildv2.json"

type cb2Saved struct {
	Connections  []json.RawMessage           `json:"connections"`
	Repositories []json.RawMessage           `json:"repositories"`
	Policies     map[string]iampolicy.Stored `json:"policies,omitempty"`
}

func loadCB2Locked() {
	root, err := ssmRootLocked()
	if err != nil || cb2State.loaded == root {
		return
	}
	cb2State.loaded = root
	b, err := os.ReadFile(filepath.Join(root, cb2StateFile))
	if err != nil {
		return
	}
	var saved cb2Saved
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for _, raw := range saved.Connections {
		c := &cb2pb.Connection{}
		if protojson.Unmarshal(raw, c) == nil {
			cb2State.connections[c.GetName()] = c
		}
	}
	for _, raw := range saved.Repositories {
		r := &cb2pb.Repository{}
		if protojson.Unmarshal(raw, r) == nil {
			cb2State.repos[r.GetName()] = r
		}
	}
	for k, v := range saved.Policies {
		cb2Policies[k] = v
	}
}

func saveCB2Locked() {
	root, err := ssmRootLocked()
	if err != nil {
		return
	}
	saved := cb2Saved{Policies: cb2Policies}
	for _, name := range sortedKeys(cb2State.connections) {
		b, _ := protojson.Marshal(cb2State.connections[name])
		saved.Connections = append(saved.Connections, b)
	}
	for _, name := range sortedKeys(cb2State.repos) {
		b, _ := protojson.Marshal(cb2State.repos[name])
		saved.Repositories = append(saved.Repositories, b)
	}
	b, _ := json.MarshalIndent(saved, "", "  ")
	tmp := filepath.Join(root, cb2StateFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(root, cb2StateFile))
	}
}

// resetCB2Project forgets a project's connections and repositories.
func resetCB2Project(project string) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	prefix := "projects/" + project + "/"
	changed := false
	for name := range cb2State.connections {
		if strings.HasPrefix(name, prefix) {
			delete(cb2State.connections, name)
			delete(cb2Policies, name)
			changed = true
		}
	}
	for name := range cb2State.repos {
		if strings.HasPrefix(name, prefix) {
			delete(cb2State.repos, name)
			changed = true
		}
	}
	if changed {
		saveCB2Locked()
	}
}

var (
	// cb2IDRE is repositories.proto's connection_id and repository_id:
	// "alphanumeric characters and any of -._~%!$&'()*+,;=@".
	cb2IDRE = regexp.MustCompile(`^[A-Za-z0-9\-._~%!$&'()*+,;=@]+$`)
	// cb2RepoRE is a repository's name.
	cb2RepoRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/connections/[^/]+/repositories/[^/]+$`)
	// cb2SecretRE is the proto's secret version format.
	cb2SecretRE  = regexp.MustCompile(`^projects/[^/]+/secrets/[^/]+/versions/[^/]+$`)
	cb2ServiceRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/namespaces/[^/]+/services/[^/]+$`)
)

func cb2Etag() string { return uuid.NewString() }

func (m *RepositoryManagerServer) done(name, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	parent, _, _ := strings.Cut(name, "/connections/")
	ops := m.b.s.ops
	op := ops.Create(parent, name)
	now := timestamppb.Now()
	_ = ops.SetMetadata(op.Name, &cb2pb.OperationMetadata{CreateTime: now, EndTime: now, Target: name, Verb: verb, ApiVersion: "v2"})
	_ = ops.Succeed(op.Name, resp)
	return m.b.s.toProtoOperation(op.Name)
}

// localGitOrigin is this instance's git server, scheme://host:port.
func localGitOrigin() string {
	f, ok := ssmGitBase.Load().(func() string)
	if !ok || f == nil {
		return ""
	}
	return strings.TrimSuffix(f(), "/")
}

func isLoopbackName(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isInstanceGitHost reports whether a host URI is this instance's git server.
func isInstanceGitHost(hostURI string) bool {
	local, err := url.Parse(localGitOrigin())
	if err != nil || local.Host == "" {
		return false
	}
	u, err := url.Parse(strings.TrimSuffix(hostURI, "/"))
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	if u.Port() != local.Port() || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return u.Hostname() == local.Hostname() || (isLoopbackName(u.Hostname()) && isLoopbackName(local.Hostname()))
}

// connectionHost is the git host a connection's config names, and the
// config's field name.
func connectionHost(c *cb2pb.Connection) (host, field string) {
	switch {
	case c.GetGithubConfig() != nil:
		return "https://github.com", "githubConfig"
	case c.GetGithubEnterpriseConfig() != nil:
		return c.GetGithubEnterpriseConfig().GetHostUri(), "githubEnterpriseConfig"
	case c.GetGitlabConfig() != nil:
		if h := c.GetGitlabConfig().GetHostUri(); h != "" {
			return h, "gitlabConfig"
		}
		return "https://gitlab.com", "gitlabConfig"
	case c.GetBitbucketDataCenterConfig() != nil:
		return c.GetBitbucketDataCenterConfig().GetHostUri(), "bitbucketDataCenterConfig"
	case c.GetBitbucketCloudConfig() != nil:
		return "https://bitbucket.org", "bitbucketCloudConfig"
	}
	return "", ""
}

// ConnectionHost is a connection's git host, for the console.
func ConnectionHost(c *cb2pb.Connection) string { h, _ := connectionHost(c); return h }

// checkSecret validates one secret version field: its name, and that this
// instance's Secret Manager serves it with a payload.
func (m *RepositoryManagerServer) checkSecret(ctx context.Context, field, version string, required bool) error {
	if version == "" {
		if required {
			return apierror.InvalidArgument("invalid connection: %s is required", field)
		}
		return nil
	}
	if !cb2SecretRE.MatchString(version) {
		return apierror.InvalidArgument("invalid connection: %s %q is not a secret version, projects/*/secrets/*/versions/*", field, version)
	}
	if m.b == nil || m.b.secretManager == nil {
		return apierror.FailedPrecondition("%s is read from this instance's Secret Manager, which is not enabled: start it with secretmanager in --services", field)
	}
	payload, err := m.b.secretManager.AccessSecretVersion(ctx, version)
	if err != nil {
		return apierror.FailedPrecondition("%s: the Cloud Build service agent cannot access secret version %q: %v", field, version, err)
	}
	if len(payload) == 0 {
		return apierror.FailedPrecondition("%s: secret version %q is empty", field, version)
	}
	return nil
}

func (m *RepositoryManagerServer) checkUserCredential(ctx context.Context, field string, u *cb2pb.UserCredential) error {
	if u == nil {
		return apierror.InvalidArgument("invalid connection: %s is required", field)
	}
	return m.checkSecret(ctx, field+".userTokenSecretVersion", u.GetUserTokenSecretVersion(), true)
}

func checkServiceDirectory(field string, s *cb2pb.ServiceDirectoryConfig) error {
	if s != nil && !cb2ServiceRE.MatchString(s.GetService()) {
		return apierror.InvalidArgument("invalid connection: %s.serviceDirectoryConfig.service %q: want "+
			"projects/{project}/locations/{location}/namespaces/{namespace}/services/{service}", field, s.GetService())
	}
	return nil
}

// checkConnection validates a connection's config and returns its
// installation state.
func (m *RepositoryManagerServer) checkConnection(ctx context.Context, c *cb2pb.Connection) (*cb2pb.InstallationState, error) {
	host, field := connectionHost(c)
	if field == "" {
		return nil, apierror.InvalidArgument("invalid connection: a connection config is required: one of githubConfig, " +
			"githubEnterpriseConfig, gitlabConfig, bitbucketDataCenterConfig or bitbucketCloudConfig")
	}
	unreachable := func() error {
		return apierror.FailedPrecondition("%s: the host %q is not reachable from this instance, which reaches no hosted git provider; "+
			"connect this instance's git server, %s, whose repositories are its Secure Source Manager repositories", field, host, localGitOrigin())
	}
	complete := &cb2pb.InstallationState{Stage: cb2pb.InstallationState_COMPLETE}
	switch {
	case c.GetGithubConfig() != nil:
		g := c.GetGithubConfig()
		if err := m.checkSecret(ctx, "githubConfig.authorizerCredential.oauthTokenSecretVersion", g.GetAuthorizerCredential().GetOauthTokenSecretVersion(), false); err != nil {
			return nil, err
		}
		if g.GetAuthorizerCredential().GetOauthTokenSecretVersion() == "" {
			return &cb2pb.InstallationState{Stage: cb2pb.InstallationState_PENDING_USER_OAUTH,
				Message: "Please log in to https://github.com using a robot account and authorize Cloud Build to access it. " +
					"github.com is not reachable from this local instance, so this connection cannot be installed here."}, nil
		}
		return &cb2pb.InstallationState{Stage: cb2pb.InstallationState_PENDING_INSTALL_APP,
			Message: "The Cloud Build GitHub App must be installed on github.com, which this local instance does not reach; " +
				"connect a GitHub Enterprise host at " + localGitOrigin() + " instead."}, nil
	case c.GetGithubEnterpriseConfig() != nil:
		g := c.GetGithubEnterpriseConfig()
		if g.GetHostUri() == "" {
			return nil, apierror.InvalidArgument("invalid connection: githubEnterpriseConfig.hostUri is required")
		}
		if g.GetApiKey() == "" {
			return nil, apierror.InvalidArgument("invalid connection: githubEnterpriseConfig.apiKey is required")
		}
		if err := checkServiceDirectory(field, g.GetServiceDirectoryConfig()); err != nil {
			return nil, err
		}
		if !isInstanceGitHost(g.GetHostUri()) {
			return nil, unreachable()
		}
		for _, s := range []struct{ f, v string }{{"githubEnterpriseConfig.privateKeySecretVersion", g.GetPrivateKeySecretVersion()},
			{"githubEnterpriseConfig.webhookSecretSecretVersion", g.GetWebhookSecretSecretVersion()}} {
			if err := m.checkSecret(ctx, s.f, s.v, false); err != nil {
				return nil, err
			}
		}
		switch {
		case g.GetAppId() == 0 || g.GetPrivateKeySecretVersion() == "" || g.GetWebhookSecretSecretVersion() == "":
			return &cb2pb.InstallationState{Stage: cb2pb.InstallationState_PENDING_CREATE_APP,
				Message: "Create the GitHub App: set appId, appSlug, privateKeySecretVersion and webhookSecretSecretVersion."}, nil
		case g.GetAppInstallationId() == 0:
			return &cb2pb.InstallationState{Stage: cb2pb.InstallationState_PENDING_INSTALL_APP,
				Message: "Install the GitHub App: set appInstallationId."}, nil
		}
		return complete, nil
	case c.GetGitlabConfig() != nil:
		g := c.GetGitlabConfig()
		if err := checkServiceDirectory(field, g.GetServiceDirectoryConfig()); err != nil {
			return nil, err
		}
		if g.GetWebhookSecretSecretVersion() == "" {
			return nil, apierror.InvalidArgument("invalid connection: gitlabConfig.webhookSecretSecretVersion is required")
		}
		if g.GetReadAuthorizerCredential() == nil || g.GetAuthorizerCredential() == nil {
			return nil, apierror.InvalidArgument("invalid connection: gitlabConfig.readAuthorizerCredential and authorizerCredential are required")
		}
		if !isInstanceGitHost(host) {
			return nil, unreachable()
		}
		if err := m.checkSecret(ctx, "gitlabConfig.webhookSecretSecretVersion", g.GetWebhookSecretSecretVersion(), true); err != nil {
			return nil, err
		}
		if err := m.checkUserCredential(ctx, "gitlabConfig.readAuthorizerCredential", g.GetReadAuthorizerCredential()); err != nil {
			return nil, err
		}
		if err := m.checkUserCredential(ctx, "gitlabConfig.authorizerCredential", g.GetAuthorizerCredential()); err != nil {
			return nil, err
		}
		return complete, nil
	case c.GetBitbucketDataCenterConfig() != nil:
		g := c.GetBitbucketDataCenterConfig()
		if g.GetHostUri() == "" {
			return nil, apierror.InvalidArgument("invalid connection: bitbucketDataCenterConfig.hostUri is required")
		}
		if err := checkServiceDirectory(field, g.GetServiceDirectoryConfig()); err != nil {
			return nil, err
		}
		if g.GetWebhookSecretSecretVersion() == "" {
			return nil, apierror.InvalidArgument("invalid connection: bitbucketDataCenterConfig.webhookSecretSecretVersion is required")
		}
		if g.GetReadAuthorizerCredential() == nil || g.GetAuthorizerCredential() == nil {
			return nil, apierror.InvalidArgument("invalid connection: bitbucketDataCenterConfig.readAuthorizerCredential and authorizerCredential are required")
		}
		if !isInstanceGitHost(host) {
			return nil, unreachable()
		}
		if err := m.checkSecret(ctx, "bitbucketDataCenterConfig.webhookSecretSecretVersion", g.GetWebhookSecretSecretVersion(), true); err != nil {
			return nil, err
		}
		if err := m.checkUserCredential(ctx, "bitbucketDataCenterConfig.readAuthorizerCredential", g.GetReadAuthorizerCredential()); err != nil {
			return nil, err
		}
		if err := m.checkUserCredential(ctx, "bitbucketDataCenterConfig.authorizerCredential", g.GetAuthorizerCredential()); err != nil {
			return nil, err
		}
		return complete, nil
	default: // bitbucketCloudConfig
		g := c.GetBitbucketCloudConfig()
		if g.GetWorkspace() == "" {
			return nil, apierror.InvalidArgument("invalid connection: bitbucketCloudConfig.workspace is required")
		}
		if g.GetWebhookSecretSecretVersion() == "" {
			return nil, apierror.InvalidArgument("invalid connection: bitbucketCloudConfig.webhookSecretSecretVersion is required")
		}
		if g.GetReadAuthorizerCredential() == nil || g.GetAuthorizerCredential() == nil {
			return nil, apierror.InvalidArgument("invalid connection: bitbucketCloudConfig.readAuthorizerCredential and authorizerCredential are required")
		}
		return nil, unreachable()
	}
}

// clearOutputs drops a caller's output-only connection fields.
func clearConnectionOutputs(c *cb2pb.Connection) {
	c.CreateTime, c.UpdateTime, c.InstallationState, c.Reconciling = nil, nil, nil, false
	if g := c.GetGithubConfig(); g != nil && g.GetAuthorizerCredential() != nil {
		g.AuthorizerCredential.Username = ""
	}
	for _, u := range userCredentials(c) {
		u.Username = ""
	}
	if g := c.GetGithubEnterpriseConfig(); g != nil {
		g.ServerVersion = ""
	}
	if g := c.GetGitlabConfig(); g != nil {
		g.ServerVersion = ""
	}
	if g := c.GetBitbucketDataCenterConfig(); g != nil {
		g.ServerVersion = ""
	}
}

func userCredentials(c *cb2pb.Connection) []*cb2pb.UserCredential {
	var out []*cb2pb.UserCredential
	add := func(us ...*cb2pb.UserCredential) {
		for _, u := range us {
			if u != nil {
				out = append(out, u)
			}
		}
	}
	if g := c.GetGitlabConfig(); g != nil {
		add(g.GetReadAuthorizerCredential(), g.GetAuthorizerCredential())
	}
	if g := c.GetBitbucketDataCenterConfig(); g != nil {
		add(g.GetReadAuthorizerCredential(), g.GetAuthorizerCredential())
	}
	if g := c.GetBitbucketCloudConfig(); g != nil {
		add(g.GetReadAuthorizerCredential(), g.GetAuthorizerCredential())
	}
	return out
}

func checkConnectionParent(parent string) error {
	_, location, err := resource.ParseLocation(parent)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	if location == "global" {
		return apierror.InvalidArgument("connections are regional: %q cannot be global", parent)
	}
	return nil
}

// CreateConnection validates and stores a connection.
func (m *RepositoryManagerServer) CreateConnection(ctx context.Context, req *cb2pb.CreateConnectionRequest) (*longrunningpb.Operation, error) {
	if err := checkConnectionParent(req.GetParent()); err != nil {
		return nil, err
	}
	if !cb2IDRE.MatchString(req.GetConnectionId()) {
		return nil, apierror.InvalidArgument("invalid connection_id %q: alphanumeric characters and any of -._~%%!$&'()*+,;=@", req.GetConnectionId())
	}
	if req.GetConnection() == nil {
		return nil, apierror.InvalidArgument("connection is required")
	}
	return m.create(ctx, req.GetParent()+"/connections/"+req.GetConnectionId(), req.GetConnection())
}

func (m *RepositoryManagerServer) create(ctx context.Context, name string, in *cb2pb.Connection) (*longrunningpb.Operation, error) {
	c := proto.Clone(in).(*cb2pb.Connection)
	clearConnectionOutputs(c)
	state, err := m.checkConnection(ctx, c)
	if err != nil {
		return nil, err
	}
	now := timestamppb.New(time.Now())
	c.Name, c.CreateTime, c.UpdateTime, c.InstallationState, c.Etag = name, now, now, state, cb2Etag()
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	if _, ok := cb2State.connections[name]; ok {
		return nil, apierror.AlreadyExists("connection %q already exists", name)
	}
	cb2State.connections[name] = c
	saveCB2Locked()
	return m.done(name, "create", proto.Clone(c))
}

func checkConnectionName(name string) error {
	parent, _, err := parseSSMName(name, "connections")
	if err != nil {
		return err
	}
	if strings.Contains(parent, "/connections/") {
		return apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/connections/*", name)
	}
	return nil
}

// GetConnection reads a connection.
func (m *RepositoryManagerServer) GetConnection(_ context.Context, req *cb2pb.GetConnectionRequest) (*cb2pb.Connection, error) {
	if err := checkConnectionName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	c, ok := cb2State.connections[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("connection %q not found", req.GetName())
	}
	return proto.Clone(c).(*cb2pb.Connection), nil
}

// ListConnections lists a location's connections, by name.
func (m *RepositoryManagerServer) ListConnections(_ context.Context, req *cb2pb.ListConnectionsRequest) (*cb2pb.ListConnectionsResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	ssmState.mu.Lock()
	loadCB2Locked()
	var all []*cb2pb.Connection
	for _, name := range sortedKeys(cb2State.connections) {
		if strings.HasPrefix(name, req.GetParent()+"/connections/") {
			all = append(all, proto.Clone(cb2State.connections[name]).(*cb2pb.Connection))
		}
	}
	ssmState.mu.Unlock()
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"/connections")
	if err != nil {
		return nil, err
	}
	return &cb2pb.ListConnectionsResponse{Connections: items, NextPageToken: next}, nil
}

var cb2ConfigPaths = map[string]bool{"github_config": true, "github_enterprise_config": true, "gitlab_config": true,
	"bitbucket_data_center_config": true, "bitbucket_cloud_config": true}

// snakePath is a mask path's top field in proto form.
func snakePath(p string) string {
	top, _, _ := strings.Cut(p, ".")
	var b strings.Builder
	for _, r := range top {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// UpdateConnection changes a connection's config, annotations or disabled,
// or creates it when allow_missing is set and it does not exist.
func (m *RepositoryManagerServer) UpdateConnection(ctx context.Context, req *cb2pb.UpdateConnectionRequest) (*longrunningpb.Operation, error) {
	in := req.GetConnection()
	if err := checkConnectionName(in.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	loadCB2Locked()
	cur, ok := cb2State.connections[in.GetName()]
	if ok {
		cur = proto.Clone(cur).(*cb2pb.Connection)
	}
	ssmState.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			parent, _, _ := parseSSMName(in.GetName(), "connections")
			if err := checkConnectionParent(parent); err != nil {
				return nil, err
			}
			return m.create(ctx, in.GetName(), in)
		}
		return nil, apierror.NotFound("connection %q not found", in.GetName())
	}
	etag := req.GetEtag()
	if etag == "" {
		etag = in.GetEtag()
	}
	if etag != "" && etag != cur.GetEtag() {
		return nil, apierror.Aborted("connection %q: etag %q does not match the current etag", in.GetName(), etag)
	}
	var paths []string
	for _, p := range req.GetUpdateMask().GetPaths() {
		paths = append(paths, snakePath(p))
	}
	if len(paths) == 0 {
		paths = []string{"annotations", "disabled"}
		if _, f := connectionHost(in); f != "" {
			paths = append(paths, snakePath(f))
		}
	}
	next := proto.Clone(cur).(*cb2pb.Connection)
	configChanged := false
	for _, p := range paths {
		switch {
		case p == "annotations":
			next.Annotations = in.GetAnnotations()
		case p == "disabled":
			next.Disabled = in.GetDisabled()
		case p == "etag" || p == "name":
		case cb2ConfigPaths[p]:
			_, curField := connectionHost(cur)
			_, inField := connectionHost(in)
			if inField == "" || snakePath(inField) != p || snakePath(curField) != p {
				return nil, apierror.InvalidArgument("update_mask path %q: a connection's config type cannot change", p)
			}
			if old, now := webhookSecretOf(cur), webhookSecretOf(in); (p == "gitlab_config" || p == "bitbucket_data_center_config") && old != now {
				return nil, apierror.InvalidArgument("%s.webhook_secret_secret_version is immutable", p)
			}
			cfg := proto.Clone(in).(*cb2pb.Connection)
			clearConnectionOutputs(cfg)
			next.ConnectionConfig = cfg.ConnectionConfig
			configChanged = true
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a connection field that can change", p)
		}
	}
	if configChanged {
		state, err := m.checkConnection(ctx, next)
		if err != nil {
			return nil, err
		}
		next.InstallationState = state
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, ok := cb2State.connections[in.GetName()]; !ok {
		return nil, apierror.NotFound("connection %q not found", in.GetName())
	}
	next.UpdateTime, next.Etag = timestamppb.Now(), cb2Etag()
	cb2State.connections[in.GetName()] = next
	saveCB2Locked()
	return m.done(in.GetName(), "update", proto.Clone(next))
}

func webhookSecretOf(c *cb2pb.Connection) string {
	switch {
	case c.GetGitlabConfig() != nil:
		return c.GetGitlabConfig().GetWebhookSecretSecretVersion()
	case c.GetBitbucketDataCenterConfig() != nil:
		return c.GetBitbucketDataCenterConfig().GetWebhookSecretSecretVersion()
	}
	return ""
}

// DeleteConnection removes a connection and its repositories.
func (m *RepositoryManagerServer) DeleteConnection(_ context.Context, req *cb2pb.DeleteConnectionRequest) (*longrunningpb.Operation, error) {
	if err := checkConnectionName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	c, ok := cb2State.connections[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("connection %q not found", req.GetName())
	}
	if req.GetEtag() != "" && req.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("connection %q: etag %q does not match the current etag", req.GetName(), req.GetEtag())
	}
	if !req.GetValidateOnly() {
		delete(cb2State.connections, req.GetName())
		delete(cb2Policies, req.GetName())
		for name := range cb2State.repos {
			if strings.HasPrefix(name, req.GetName()+"/repositories/") {
				delete(cb2State.repos, name)
			}
		}
		saveCB2Locked()
	}
	return m.done(req.GetName(), "delete", &emptypb.Empty{})
}

// readyConnectionLocked is a repository parent's connection, which must be
// installed and enabled.
func readyConnectionLocked(parent string) (*cb2pb.Connection, error) {
	if err := checkConnectionName(parent); err != nil {
		return nil, err
	}
	c, ok := cb2State.connections[parent]
	if !ok {
		return nil, apierror.NotFound("connection %q not found", parent)
	}
	if c.GetDisabled() {
		return nil, apierror.FailedPrecondition("connection %q is disabled", parent)
	}
	if st := c.GetInstallationState().GetStage(); st != cb2pb.InstallationState_COMPLETE {
		return nil, apierror.FailedPrecondition("connection %q is not ready: its installation stage is %s", parent, st)
	}
	return c, nil
}

// repoOfRemoteLocked is the Secure Source Manager repository a remoteUri on
// a connection's host names.
func repoOfRemoteLocked(c *cb2pb.Connection, remote string) (string, error) {
	host, _ := connectionHost(c)
	u, err := url.Parse(remote)
	if remote == "" || err != nil || u.Host == "" {
		return "", apierror.InvalidArgument("invalid repository: remoteUri %q is not a git clone URI", remote)
	}
	if !isInstanceGitHost(u.Scheme + "://" + u.Host) {
		return "", apierror.InvalidArgument("invalid repository: remoteUri %q is not on the connection's host %s", remote, host)
	}
	repo := ssmRepoOfURI(remote)
	if repo == "" {
		return "", apierror.InvalidArgument("invalid repository: remoteUri %q is not a repository URL of %s", remote, host)
	}
	if _, ok := ssmState.repos[repo]; !ok {
		return "", apierror.NotFound("repository %q not found on %s", remote, host)
	}
	return repo, nil
}

func (m *RepositoryManagerServer) newRepositoryLocked(parent, id string, in *cb2pb.Repository, batch map[string]bool) (*cb2pb.Repository, error) {
	if !cb2IDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("invalid repository_id %q: alphanumeric characters and any of -._~%%!$&'()*+,;=@", id)
	}
	if in == nil {
		return nil, apierror.InvalidArgument("repository is required")
	}
	c, err := readyConnectionLocked(parent)
	if err != nil {
		return nil, err
	}
	if _, err := repoOfRemoteLocked(c, in.GetRemoteUri()); err != nil {
		return nil, err
	}
	name := parent + "/repositories/" + id
	if _, ok := cb2State.repos[name]; ok || batch[name] {
		return nil, apierror.AlreadyExists("repository %q already exists", name)
	}
	for n, r := range cb2State.repos {
		if strings.HasPrefix(n, parent+"/repositories/") && r.GetRemoteUri() == in.GetRemoteUri() {
			return nil, apierror.AlreadyExists("remoteUri %q is already linked as %q", in.GetRemoteUri(), n)
		}
	}
	now := timestamppb.Now()
	return &cb2pb.Repository{Name: name, RemoteUri: in.GetRemoteUri(), Annotations: in.GetAnnotations(),
		CreateTime: now, UpdateTime: now, Etag: cb2Etag()}, nil
}

// CreateRepository links a repository of the connection's host.
func (m *RepositoryManagerServer) CreateRepository(_ context.Context, req *cb2pb.CreateRepositoryRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	r, err := m.newRepositoryLocked(req.GetParent(), req.GetRepositoryId(), req.GetRepository(), nil)
	if err != nil {
		return nil, err
	}
	cb2State.repos[r.GetName()] = r
	saveCB2Locked()
	return m.done(r.GetName(), "create", proto.Clone(r))
}

// BatchCreateRepositories links several repositories, all or none.
func (m *RepositoryManagerServer) BatchCreateRepositories(_ context.Context, req *cb2pb.BatchCreateRepositoriesRequest) (*longrunningpb.Operation, error) {
	if len(req.GetRequests()) == 0 {
		return nil, apierror.InvalidArgument("requests is required")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	batch := map[string]bool{}
	var made []*cb2pb.Repository
	for i, r := range req.GetRequests() {
		if r.GetParent() != "" && r.GetParent() != req.GetParent() {
			return nil, apierror.InvalidArgument("requests[%d].parent %q must be empty or match parent %q", i, r.GetParent(), req.GetParent())
		}
		repo, err := m.newRepositoryLocked(req.GetParent(), r.GetRepositoryId(), r.GetRepository(), batch)
		if err != nil {
			return nil, err
		}
		for _, o := range made {
			if o.GetRemoteUri() == repo.GetRemoteUri() {
				return nil, apierror.AlreadyExists("remoteUri %q is named twice", repo.GetRemoteUri())
			}
		}
		batch[repo.GetName()] = true
		made = append(made, repo)
	}
	for _, r := range made {
		cb2State.repos[r.GetName()] = r
	}
	saveCB2Locked()
	out := &cb2pb.BatchCreateRepositoriesResponse{}
	for _, r := range made {
		out.Repositories = append(out.Repositories, proto.Clone(r).(*cb2pb.Repository))
	}
	return m.done(req.GetParent(), "create", out)
}

func checkRepositoryName(name string) error {
	if !cb2RepoRE.MatchString(name) {
		return apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/connections/*/repositories/*", name)
	}
	return nil
}

// GetRepository reads a repository.
func (m *RepositoryManagerServer) GetRepository(_ context.Context, req *cb2pb.GetRepositoryRequest) (*cb2pb.Repository, error) {
	if err := checkRepositoryName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	r, ok := cb2State.repos[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("repository %q not found", req.GetName())
	}
	return proto.Clone(r).(*cb2pb.Repository), nil
}

// ListRepositories lists a connection's repositories, by name, with
// AIP-160 filtering.
func (m *RepositoryManagerServer) ListRepositories(_ context.Context, req *cb2pb.ListRepositoriesRequest) (*cb2pb.ListRepositoriesResponse, error) {
	if err := checkConnectionName(req.GetParent()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	loadCB2Locked()
	if _, ok := cb2State.connections[req.GetParent()]; !ok {
		ssmState.mu.Unlock()
		return nil, apierror.NotFound("connection %q not found", req.GetParent())
	}
	var all []*cb2pb.Repository
	for _, name := range sortedKeys(cb2State.repos) {
		if strings.HasPrefix(name, req.GetParent()+"/repositories/") {
			all = append(all, proto.Clone(cb2State.repos[name]).(*cb2pb.Repository))
		}
	}
	ssmState.mu.Unlock()
	all, err := aipfilter.List(req.GetFilter(), "", all)
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"/repositories|"+req.GetFilter())
	if err != nil {
		return nil, err
	}
	return &cb2pb.ListRepositoriesResponse{Repositories: items, NextPageToken: next}, nil
}

// DeleteRepository unlinks a repository; the host's repository stays.
func (m *RepositoryManagerServer) DeleteRepository(_ context.Context, req *cb2pb.DeleteRepositoryRequest) (*longrunningpb.Operation, error) {
	if err := checkRepositoryName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	r, ok := cb2State.repos[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("repository %q not found", req.GetName())
	}
	if req.GetEtag() != "" && req.GetEtag() != r.GetEtag() {
		return nil, apierror.Aborted("repository %q: etag %q does not match the current etag", req.GetName(), req.GetEtag())
	}
	if !req.GetValidateOnly() {
		delete(cb2State.repos, req.GetName())
		saveCB2Locked()
	}
	return m.done(req.GetName(), "delete", &emptypb.Empty{})
}

// cb2RepoTarget is the Secure Source Manager repository a linked
// repository names, its connection installed and enabled.
func cb2RepoTarget(name string) (string, error) {
	if err := checkRepositoryName(name); err != nil {
		return "", err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	return cb2RepoTargetLocked(name)
}

func cb2RepoTargetLocked(name string) (string, error) {
	r, ok := cb2State.repos[name]
	if !ok {
		return "", apierror.NotFound("repository %q not found", name)
	}
	c, err := readyConnectionLocked(strings.SplitN(name, "/repositories/", 2)[0])
	if err != nil {
		return "", err
	}
	return repoOfRemoteLocked(c, r.GetRemoteUri())
}

// FetchReadToken mints a token that clones the repository.
func (m *RepositoryManagerServer) FetchReadToken(ctx context.Context, req *cb2pb.FetchReadTokenRequest) (*cb2pb.FetchReadTokenResponse, error) {
	repo, err := cb2RepoTarget(req.GetRepository())
	if err != nil {
		return nil, err
	}
	tok, exp, err := mintGitToken(ctx, req.GetRepository(), repo, false)
	if err != nil {
		return nil, err
	}
	return &cb2pb.FetchReadTokenResponse{Token: tok, ExpirationTime: timestamppb.New(exp)}, nil
}

// FetchReadWriteToken mints a token that clones and pushes to the
// repository.
func (m *RepositoryManagerServer) FetchReadWriteToken(ctx context.Context, req *cb2pb.FetchReadWriteTokenRequest) (*cb2pb.FetchReadWriteTokenResponse, error) {
	repo, err := cb2RepoTarget(req.GetRepository())
	if err != nil {
		return nil, err
	}
	tok, exp, err := mintGitToken(ctx, req.GetRepository(), repo, true)
	if err != nil {
		return nil, err
	}
	return &cb2pb.FetchReadWriteTokenResponse{Token: tok, ExpirationTime: timestamppb.New(exp)}, nil
}

// FetchLinkableRepositories lists the repositories the connection's host
// serves, by remoteUri; the proto's default page size is 20.
func (m *RepositoryManagerServer) FetchLinkableRepositories(_ context.Context, req *cb2pb.FetchLinkableRepositoriesRequest) (*cb2pb.FetchLinkableRepositoriesResponse, error) {
	ssmState.mu.Lock()
	loadCB2Locked()
	if _, err := readyConnectionLocked(req.GetConnection()); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*cb2pb.Repository
	for _, name := range sortedKeys(ssmState.repos) {
		if uri := ssmState.repos[name].GetUris().GetGitHttps(); uri != "" {
			all = append(all, &cb2pb.Repository{RemoteUri: uri})
		}
	}
	ssmState.mu.Unlock()
	size := req.GetPageSize()
	if size <= 0 {
		size = 20
	}
	items, next, err := page(all, size, req.GetPageToken(), req.GetConnection()+":fetchLinkableRepositories")
	if err != nil {
		return nil, err
	}
	return &cb2pb.FetchLinkableRepositoriesResponse{Repositories: items, NextPageToken: next}, nil
}

// FetchGitRefs lists the repository's branches or tags, by short name.
func (m *RepositoryManagerServer) FetchGitRefs(ctx context.Context, req *cb2pb.FetchGitRefsRequest) (*cb2pb.FetchGitRefsResponse, error) {
	var prefix string
	switch req.GetRefType() {
	case cb2pb.FetchGitRefsRequest_BRANCH:
		prefix = "refs/heads/"
	case cb2pb.FetchGitRefsRequest_TAG:
		prefix = "refs/tags/"
	default:
		return nil, apierror.InvalidArgument("ref_type is required: BRANCH or TAG")
	}
	repo, err := cb2RepoTarget(req.GetRepository())
	if err != nil {
		return nil, err
	}
	parts := strings.Split(repo, "/")
	_, dir, ok := lookupSSMRepo(parts[1], parts[3], parts[5])
	if !ok {
		return nil, apierror.FailedPrecondition("the host's repository of %q was deleted", req.GetRepository())
	}
	var all []string
	for ref := range gitRefs(ctx, dir) {
		if short, ok := strings.CutPrefix(ref, prefix); ok {
			all = append(all, short)
		}
	}
	sort.Strings(all)
	return &cb2pb.FetchGitRefsResponse{RefNames: all}, nil
}

// cb2RemoteURI is a linked repository's remoteUri, or "".
func cb2RemoteURI(name string) string {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	return cb2State.repos[name].GetRemoteUri()
}

// cb2RepositoryType is a 2nd-gen trigger's repositoryType, by its
// repository's connection.
func cb2RepositoryType(name string) string {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	c := cb2State.connections[strings.SplitN(name, "/repositories/", 2)[0]]
	switch {
	case c.GetGithubConfig() != nil:
		return "GITHUB"
	case c.GetGithubEnterpriseConfig() != nil:
		return "GITHUB_ENTERPRISE"
	case c.GetGitlabConfig() != nil:
		return "GITLAB_ENTERPRISE"
	case c.GetBitbucketDataCenterConfig() != nil:
		return "BITBUCKET_DATA_CENTER"
	case c.GetBitbucketCloudConfig() != nil:
		return "BITBUCKET_CLOUD"
	}
	return ""
}

// CloudBuildConnections lists a project's connections in every location,
// for the console.
func CloudBuildConnections(project string) []*cb2pb.Connection {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	var out []*cb2pb.Connection
	for _, name := range sortedKeys(cb2State.connections) {
		if strings.HasPrefix(name, "projects/"+project+"/") {
			out = append(out, proto.Clone(cb2State.connections[name]).(*cb2pb.Connection))
		}
	}
	return out
}

// CloudBuildRepositories lists a connection's repositories, for the
// console.
func CloudBuildRepositories(connection string) []*cb2pb.Repository {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	var out []*cb2pb.Repository
	for _, name := range sortedKeys(cb2State.repos) {
		if strings.HasPrefix(name, connection+"/repositories/") {
			out = append(out, proto.Clone(cb2State.repos[name]).(*cb2pb.Repository))
		}
	}
	return out
}

// LocalGitHost is the host URI a connection names to install on this
// instance's git server, for the console.
func LocalGitHost() string { return localGitOrigin() }

// cb2Path is a Cloud Build v2 JSON path: its connections and repositories,
// under /v2/projects/*/locations/*/connections.
var cb2Path = regexp.MustCompile(`^/v2/projects/[^/]+/locations/[^/]+/connections($|[/:])`)

// CloudBuildV2HandlerOr serves Cloud Build v2's JSON API on the Run port,
// at Google's paths (gcloud's cloudbuild override and Terraform's
// cloudbuildv2_custom_endpoint name the port's root and /v2/), and passes
// every other request, its operations among them (Cloud Run v2's
// /v2/projects/*/locations/*/operations/*, the same operations store), to
// next.
func CloudBuildV2HandlerOr(register func(grpc.ServiceRegistrar), next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.devtools.cloudbuild.v2"}, Mixins: cb2IAMMixins}
	register(t)
	iampb.RegisterIAMPolicyServer(t, cb2ConnectionIAM{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cb2Path.MatchString(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		t.ServeHTTP(w, r)
	})
}

// checkRepositoryTrigger checks a 2nd-gen trigger: it must be in its
// repository's region, as Google's "Create and manage build triggers"
// page says of Cloud Build repositories, and answers its repositoryType,
// which is output only, from the repository's connection.
func checkRepositoryTrigger(t *cbpb.BuildTrigger, location string) error {
	rc := t.GetRepositoryEventConfig()
	if rc == nil || !cb2RepoRE.MatchString(rc.GetRepository()) {
		return nil
	}
	_, repoLocation, _ := resource.ParseLocation(strings.SplitN(rc.GetRepository(), "/connections/", 2)[0])
	if repoLocation != location {
		return apierror.InvalidArgument("invalid trigger: a trigger on a Cloud Build repository must be in the repository's region %s, not %s",
			repoLocation, location)
	}
	rc.RepositoryType = cbpb.RepositoryEventConfig_RepositoryType(cbpb.RepositoryEventConfig_RepositoryType_value[cb2RepositoryType(rc.GetRepository())])
	return nil
}
