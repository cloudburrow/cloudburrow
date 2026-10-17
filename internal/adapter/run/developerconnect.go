package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Developer Connect (#1546): connections and git repository links of
// google.cloud.developerconnect.v1, for the one provider an instance
// hosts, its own Secure Source Manager. Google's "Connect to Secure Source
// Manager" page documents the connection (secureSourceManagerInstanceConfig,
// in the instance's region) and its links, one per repository; a
// repository's .cloudbuild/triggers.yaml then names a link in a trigger's
// devConnectGitRepositoryLink (securesourcemanagergit.go).
//
// A connection is installed (stage COMPLETE) when its create returns: the
// instance is this one's, so nothing is authorized. A link's cloneUri must
// be the git URL of a repository on the connection's instance; its
// gitProxyUri is that URL too, as this port serves the repository itself.
// GitHub, GitLab, Bitbucket and generic HTTP connections are refused
// UNIMPLEMENTED: they authorize a hosted provider, which no instance
// reaches. The tokens (FetchReadToken, FetchReadWriteToken), account
// connectors and insights configs are UNIMPLEMENTED.

// DeveloperConnectServer serves Developer Connect's connections and links.
type DeveloperConnectServer struct {
	dcpb.UnimplementedDeveloperConnectServer
	s *Server
}

// DeveloperConnect returns the Developer Connect service.
func (s *Server) DeveloperConnect() *DeveloperConnectServer { return &DeveloperConnectServer{s: s} }

// Register adds the service to a gRPC server or a JSON transcoder.
func (d *DeveloperConnectServer) Register(g grpc.ServiceRegistrar) {
	dcpb.RegisterDeveloperConnectServer(g, d)
}

// dcState holds the connections and links, under ssmState.mu: a link names
// a Secure Source Manager repository, whose lock it shares.
var dcState = struct {
	connections map[string]*dcpb.Connection
	links       map[string]*dcpb.GitRepositoryLink
	loaded      string // the root they were loaded from
}{connections: map[string]*dcpb.Connection{}, links: map[string]*dcpb.GitRepositoryLink{}}

const dcStateFile = "developerconnect.json"

type dcSaved struct {
	Connections []json.RawMessage `json:"connections"`
	Links       []json.RawMessage `json:"gitRepositoryLinks"`
}

// loadDCLocked reads what a previous run saved beside the repositories.
func loadDCLocked() {
	root, err := ssmRootLocked()
	if err != nil || dcState.loaded == root {
		return
	}
	dcState.loaded = root
	b, err := os.ReadFile(filepath.Join(root, dcStateFile))
	if err != nil {
		return
	}
	var saved dcSaved
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for _, raw := range saved.Connections {
		c := &dcpb.Connection{}
		if protojson.Unmarshal(raw, c) == nil {
			dcState.connections[c.GetName()] = c
		}
	}
	for _, raw := range saved.Links {
		l := &dcpb.GitRepositoryLink{}
		if protojson.Unmarshal(raw, l) == nil {
			dcState.links[l.GetName()] = l
		}
	}
}

func saveDCLocked() {
	root, err := ssmRootLocked()
	if err != nil {
		return
	}
	var saved dcSaved
	for _, name := range sortedKeys(dcState.connections) {
		b, _ := protojson.Marshal(dcState.connections[name])
		saved.Connections = append(saved.Connections, b)
	}
	for _, name := range sortedKeys(dcState.links) {
		b, _ := protojson.Marshal(dcState.links[name])
		saved.Links = append(saved.Links, b)
	}
	b, _ := json.MarshalIndent(saved, "", "  ")
	tmp := filepath.Join(root, dcStateFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(root, dcStateFile))
	}
}

// dcIDRE is the connection and link ID shape: lower case letters, digits
// and hyphens, starting with a letter, at most 63.
var dcIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// parseLinkName checks a link's name,
// projects/*/locations/*/connections/*/gitRepositoryLinks/*.
func parseLinkName(name string) (parent string, err error) {
	parent, id, ok := strings.Cut(name, "/gitRepositoryLinks/")
	if _, _, perr := parseSSMName(parent, "connections"); !ok || perr != nil || id == "" || strings.Contains(id, "/") {
		return "", apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/connections/*/gitRepositoryLinks/*", name)
	}
	return parent, nil
}

func dcEtag(t *timestamppb.Timestamp) string { return fmt.Sprintf("%x", t.AsTime().UnixNano()) }

func (d *DeveloperConnectServer) done(parent, target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	// Operations are a location's, a link's too.
	parent, _, _ = strings.Cut(parent, "/connections/")
	op := d.s.ops.Create(parent, target)
	now := timestamppb.Now()
	_ = d.s.ops.SetMetadata(op.Name, &dcpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	_ = d.s.ops.Succeed(op.Name, resp)
	return d.s.toProtoOperation(op.Name)
}

// unsupportedConnection names why a connection cannot be made here, or
// nil for a Secure Source Manager one.
func unsupportedConnection(c *dcpb.Connection) error {
	hosted := func(field string) error {
		return apierror.Unimplemented("Connection field %s is not supported: it authorizes a hosted git provider, "+
			"which no instance reaches; connect this instance's own Secure Source Manager instead "+
			"(secureSourceManagerInstanceConfig)", field)
	}
	switch {
	case c.GetGithubConfig() != nil:
		return hosted("githubConfig")
	case c.GetGithubEnterpriseConfig() != nil:
		return hosted("githubEnterpriseConfig")
	case c.GetGitlabConfig() != nil:
		return hosted("gitlabConfig")
	case c.GetGitlabEnterpriseConfig() != nil:
		return hosted("gitlabEnterpriseConfig")
	case c.GetBitbucketDataCenterConfig() != nil:
		return hosted("bitbucketDataCenterConfig")
	case c.GetBitbucketCloudConfig() != nil:
		return hosted("bitbucketCloudConfig")
	case c.GetHttpConfig() != nil:
		return hosted("httpConfig")
	case c.GetSecureSourceManagerInstanceConfig() == nil:
		return apierror.InvalidArgument("connection needs a connection config: secureSourceManagerInstanceConfig is the one served")
	case c.GetCryptoKeyConfig() != nil:
		return apierror.Unimplemented("Connection field cryptoKeyConfig is not supported: it encrypts the provider's credentials, which a Secure Source Manager connection has none of")
	case c.GetGitProxyConfig().GetEnabled():
		return apierror.Unimplemented("Connection field gitProxyConfig is not supported: this instance serves the repository's git URL itself")
	}
	return nil
}

// CreateConnection connects a Secure Source Manager instance of this
// instance, in the connection's location.
func (d *DeveloperConnectServer) CreateConnection(_ context.Context, req *dcpb.CreateConnectionRequest) (*longrunningpb.Operation, error) {
	_, location, err := resource.ParseLocation(req.GetParent())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !dcIDRE.MatchString(req.GetConnectionId()) {
		return nil, apierror.InvalidArgument("invalid connection_id %q: lower case letters, digits and hyphens, starting with a letter", req.GetConnectionId())
	}
	c := proto.Clone(req.GetConnection()).(*dcpb.Connection)
	if req.GetConnection() == nil {
		return nil, apierror.InvalidArgument("connection is required")
	}
	if err := unsupportedConnection(c); err != nil {
		return nil, err
	}
	inst := c.GetSecureSourceManagerInstanceConfig().GetInstance()
	_, il, err := resource.ParseLocation(strings.SplitN(inst, "/instances/", 2)[0])
	if err != nil || !strings.Contains(inst, "/instances/") {
		return nil, apierror.InvalidArgument("invalid secureSourceManagerInstanceConfig.instance %q: want projects/*/locations/*/instances/*", inst)
	}
	if il != location {
		return nil, apierror.InvalidArgument("the connection must be in the Secure Source Manager instance's region %s, not %s", il, location)
	}
	name := req.GetParent() + "/connections/" + req.GetConnectionId()
	now := timestamppb.New(time.Now())
	c.Name, c.CreateTime, c.UpdateTime, c.Uid = name, now, now, uuid.NewString()
	c.Etag, c.Reconciling = dcEtag(now), false
	c.InstallationState = &dcpb.InstallationState{Stage: dcpb.InstallationState_COMPLETE}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	if _, ok := ssmState.instances[inst]; !ok {
		return nil, apierror.NotFound("Secure Source Manager instance %q not found", inst)
	}
	if _, ok := dcState.connections[name]; ok {
		return nil, apierror.AlreadyExists("connection %q already exists", name)
	}
	if req.GetValidateOnly() {
		return d.done(req.GetParent(), name, "create", proto.Clone(c))
	}
	dcState.connections[name] = c
	saveDCLocked()
	return d.done(req.GetParent(), name, "create", proto.Clone(c))
}

// GetConnection reads a connection.
func (d *DeveloperConnectServer) GetConnection(_ context.Context, req *dcpb.GetConnectionRequest) (*dcpb.Connection, error) {
	if _, _, err := parseSSMName(req.GetName(), "connections"); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	c, ok := dcState.connections[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("connection %q not found", req.GetName())
	}
	return proto.Clone(c).(*dcpb.Connection), nil
}

// ListConnections lists a location's connections, by name.
func (d *DeveloperConnectServer) ListConnections(_ context.Context, req *dcpb.ListConnectionsRequest) (*dcpb.ListConnectionsResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("ListConnections filter and order_by are not supported")
	}
	ssmState.mu.Lock()
	var all []*dcpb.Connection
	loadDCLocked()
	for _, name := range sortedKeys(dcState.connections) {
		if strings.HasPrefix(name, req.GetParent()+"/connections/") {
			all = append(all, proto.Clone(dcState.connections[name]).(*dcpb.Connection))
		}
	}
	ssmState.mu.Unlock()
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"/connections")
	if err != nil {
		return nil, err
	}
	return &dcpb.ListConnectionsResponse{Connections: items, NextPageToken: next}, nil
}

// UpdateConnection changes a connection's labels, annotations and
// disabled; its instance is immutable.
func (d *DeveloperConnectServer) UpdateConnection(_ context.Context, req *dcpb.UpdateConnectionRequest) (*longrunningpb.Operation, error) {
	in := req.GetConnection()
	parent, _, err := parseSSMName(in.GetName(), "connections")
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"labels", "annotations", "disabled"}
	}
	for _, p := range paths {
		switch p {
		case "labels", "annotations", "disabled":
		default:
			return nil, apierror.Unimplemented("update_mask path %q is not supported: only labels, annotations and disabled can change", p)
		}
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	c, ok := dcState.connections[in.GetName()]
	if !ok {
		if req.GetAllowMissing() {
			return nil, apierror.Unimplemented("UpdateConnection allow_missing is not supported: create the connection")
		}
		return nil, apierror.NotFound("connection %q not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("connection %q etag %q does not match %q", in.GetName(), in.GetEtag(), c.GetEtag())
	}
	next := proto.Clone(c).(*dcpb.Connection)
	for _, p := range paths {
		switch p {
		case "labels":
			next.Labels = in.GetLabels()
		case "annotations":
			next.Annotations = in.GetAnnotations()
		case "disabled":
			next.Disabled = in.GetDisabled()
		}
	}
	if req.GetValidateOnly() {
		return d.done(parent, in.GetName(), "update", next)
	}
	next.UpdateTime = timestamppb.Now()
	next.Etag = dcEtag(next.UpdateTime)
	dcState.connections[in.GetName()] = next
	saveDCLocked()
	return d.done(parent, in.GetName(), "update", proto.Clone(next))
}

// DeleteConnection removes a connection; one that still has links is
// refused, as their triggers would lose them.
func (d *DeveloperConnectServer) DeleteConnection(_ context.Context, req *dcpb.DeleteConnectionRequest) (*longrunningpb.Operation, error) {
	parent, _, err := parseSSMName(req.GetName(), "connections")
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	c, ok := dcState.connections[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("connection %q not found", req.GetName())
	}
	if req.GetEtag() != "" && req.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("connection %q etag %q does not match %q", req.GetName(), req.GetEtag(), c.GetEtag())
	}
	for name := range dcState.links {
		if strings.HasPrefix(name, req.GetName()+"/gitRepositoryLinks/") {
			return nil, apierror.FailedPrecondition("connection %q still has git repository links: delete them first", req.GetName())
		}
	}
	if !req.GetValidateOnly() {
		delete(dcState.connections, req.GetName())
		saveDCLocked()
	}
	return d.done(parent, req.GetName(), "delete", &emptypb.Empty{})
}

// ssmRepoOfURI is the Secure Source Manager repository a git URL on this
// port names, by the path its host serves, or "".
func ssmRepoOfURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	p, ok := strings.CutPrefix(u.Path, ssmGitPrefix)
	if !ok {
		return ""
	}
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".git") {
		return ""
	}
	return "projects/" + parts[0] + "/locations/" + parts[1] + "/repositories/" + strings.TrimSuffix(parts[2], ".git")
}

// connectionLocked is a link parent's connection.
func connectionLocked(parent string) (*dcpb.Connection, error) {
	if _, _, err := parseSSMName(parent, "connections"); err != nil {
		return nil, err
	}
	c, ok := dcState.connections[parent]
	if !ok {
		return nil, apierror.NotFound("connection %q not found", parent)
	}
	return c, nil
}

// CreateGitRepositoryLink links a repository of the connection's instance,
// named by its git URL.
func (d *DeveloperConnectServer) CreateGitRepositoryLink(_ context.Context, req *dcpb.CreateGitRepositoryLinkRequest) (*longrunningpb.Operation, error) {
	if !dcIDRE.MatchString(req.GetGitRepositoryLinkId()) {
		return nil, apierror.InvalidArgument("invalid git_repository_link_id %q: lower case letters, digits and hyphens, starting with a letter", req.GetGitRepositoryLinkId())
	}
	if req.GetGitRepositoryLink() == nil {
		return nil, apierror.InvalidArgument("git_repository_link is required")
	}
	l := proto.Clone(req.GetGitRepositoryLink()).(*dcpb.GitRepositoryLink)
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	c, err := connectionLocked(req.GetParent())
	if err != nil {
		return nil, err
	}
	repo := ssmRepoOfURI(l.GetCloneUri())
	r, ok := ssmState.repos[repo]
	if repo == "" || !ok {
		return nil, apierror.InvalidArgument("cloneUri %q is not the git URL of a Secure Source Manager repository on this instance", l.GetCloneUri())
	}
	if r.GetInstance() != c.GetSecureSourceManagerInstanceConfig().GetInstance() {
		return nil, apierror.InvalidArgument("repository %q is on instance %q, not the connection's %q", repo, r.GetInstance(),
			c.GetSecureSourceManagerInstanceConfig().GetInstance())
	}
	name := req.GetParent() + "/gitRepositoryLinks/" + req.GetGitRepositoryLinkId()
	if _, ok := dcState.links[name]; ok {
		return nil, apierror.AlreadyExists("git repository link %q already exists", name)
	}
	now := timestamppb.New(time.Now())
	l.Name, l.CreateTime, l.UpdateTime, l.Uid, l.Etag = name, now, now, uuid.NewString(), dcEtag(now)
	l.CloneUri, l.GitProxyUri, l.Reconciling, l.WebhookId = r.GetUris().GetGitHttps(), r.GetUris().GetGitHttps(), false, ""
	if req.GetValidateOnly() {
		return d.done(req.GetParent(), name, "create", proto.Clone(l))
	}
	dcState.links[name] = l
	saveDCLocked()
	return d.done(req.GetParent(), name, "create", proto.Clone(l))
}

// GetGitRepositoryLink reads a link.
func (d *DeveloperConnectServer) GetGitRepositoryLink(_ context.Context, req *dcpb.GetGitRepositoryLinkRequest) (*dcpb.GitRepositoryLink, error) {
	if _, err := parseLinkName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	l, ok := dcState.links[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("git repository link %q not found", req.GetName())
	}
	return proto.Clone(l).(*dcpb.GitRepositoryLink), nil
}

// ListGitRepositoryLinks lists a connection's links, by name.
func (d *DeveloperConnectServer) ListGitRepositoryLinks(_ context.Context, req *dcpb.ListGitRepositoryLinksRequest) (*dcpb.ListGitRepositoryLinksResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("ListGitRepositoryLinks filter and order_by are not supported")
	}
	ssmState.mu.Lock()
	loadDCLocked()
	if _, err := connectionLocked(req.GetParent()); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*dcpb.GitRepositoryLink
	for _, name := range sortedKeys(dcState.links) {
		if strings.HasPrefix(name, req.GetParent()+"/gitRepositoryLinks/") {
			all = append(all, proto.Clone(dcState.links[name]).(*dcpb.GitRepositoryLink))
		}
	}
	ssmState.mu.Unlock()
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"/gitRepositoryLinks")
	if err != nil {
		return nil, err
	}
	return &dcpb.ListGitRepositoryLinksResponse{GitRepositoryLinks: items, NextPageToken: next}, nil
}

// DeleteGitRepositoryLink removes a link; its repository stays.
func (d *DeveloperConnectServer) DeleteGitRepositoryLink(_ context.Context, req *dcpb.DeleteGitRepositoryLinkRequest) (*longrunningpb.Operation, error) {
	parent, err := parseLinkName(req.GetName())
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	l, ok := dcState.links[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("git repository link %q not found", req.GetName())
	}
	if req.GetEtag() != "" && req.GetEtag() != l.GetEtag() {
		return nil, apierror.Aborted("git repository link %q etag %q does not match %q", req.GetName(), req.GetEtag(), l.GetEtag())
	}
	if !req.GetValidateOnly() {
		delete(dcState.links, req.GetName())
		saveDCLocked()
	}
	return d.done(parent, req.GetName(), "delete", &emptypb.Empty{})
}

// FetchLinkableGitRepositories lists the repositories of the connection's
// instance, by their git URLs.
func (d *DeveloperConnectServer) FetchLinkableGitRepositories(_ context.Context, req *dcpb.FetchLinkableGitRepositoriesRequest) (*dcpb.FetchLinkableGitRepositoriesResponse, error) {
	ssmState.mu.Lock()
	loadDCLocked()
	c, err := connectionLocked(req.GetConnection())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*dcpb.LinkableGitRepository
	for _, name := range sortedKeys(ssmState.repos) {
		if r := ssmState.repos[name]; r.GetInstance() == c.GetSecureSourceManagerInstanceConfig().GetInstance() {
			all = append(all, &dcpb.LinkableGitRepository{CloneUri: r.GetUris().GetGitHttps()})
		}
	}
	ssmState.mu.Unlock()
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetConnection()+":fetchLinkableGitRepositories")
	if err != nil {
		return nil, err
	}
	return &dcpb.FetchLinkableGitRepositoriesResponse{LinkableGitRepositories: items, NextPageToken: next}, nil
}

// FetchGitRefs lists a linked repository's branches or tags, by short
// name.
func (d *DeveloperConnectServer) FetchGitRefs(ctx context.Context, req *dcpb.FetchGitRefsRequest) (*dcpb.FetchGitRefsResponse, error) {
	var prefix string
	switch req.GetRefType() {
	case dcpb.FetchGitRefsRequest_BRANCH:
		prefix = "refs/heads/"
	case dcpb.FetchGitRefsRequest_TAG:
		prefix = "refs/tags/"
	default:
		return nil, apierror.InvalidArgument("ref_type is required: BRANCH or TAG")
	}
	ssmState.mu.Lock()
	loadDCLocked()
	l, ok := dcState.links[req.GetGitRepositoryLink()]
	ssmState.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("git repository link %q not found", req.GetGitRepositoryLink())
	}
	parts := strings.Split(ssmRepoOfURI(l.GetCloneUri()), "/")
	if len(parts) != 6 {
		return nil, apierror.FailedPrecondition("git repository link %q names no repository", req.GetGitRepositoryLink())
	}
	_, dir, ok := lookupSSMRepo(parts[1], parts[3], parts[5])
	if !ok {
		return nil, apierror.FailedPrecondition("the repository of git repository link %q was deleted", req.GetGitRepositoryLink())
	}
	var all []string
	for ref := range gitRefs(ctx, dir) {
		if short, ok := strings.CutPrefix(ref, prefix); ok {
			all = append(all, short)
		}
	}
	sort.Strings(all)
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetGitRepositoryLink()+":fetchGitRefs")
	if err != nil {
		return nil, err
	}
	return &dcpb.FetchGitRefsResponse{RefNames: items, NextPageToken: next}, nil
}

// devConnectLinkRepo is the Secure Source Manager repository a git
// repository link names, for a triggers file's devConnectGitRepositoryLink.
func devConnectLinkRepo(link string) (string, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	l, ok := dcState.links[link]
	if !ok {
		return "", fmt.Errorf("git repository link %q not found", link)
	}
	if c, ok := dcState.connections[strings.SplitN(link, "/gitRepositoryLinks/", 2)[0]]; ok && c.GetDisabled() {
		return "", fmt.Errorf("the connection of git repository link %q is disabled", link)
	}
	return ssmRepoOfURI(l.GetCloneUri()), nil
}

// DeveloperConnectConnections lists a project's connections in every
// location, for the console.
func DeveloperConnectConnections(project string) []*dcpb.Connection {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	var out []*dcpb.Connection
	for _, name := range sortedKeys(dcState.connections) {
		if strings.HasPrefix(name, "projects/"+project+"/") {
			out = append(out, proto.Clone(dcState.connections[name]).(*dcpb.Connection))
		}
	}
	return out
}

// DeveloperConnectLinks lists a connection's links, for the console.
func DeveloperConnectLinks(connection string) []*dcpb.GitRepositoryLink {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	var out []*dcpb.GitRepositoryLink
	for _, name := range sortedKeys(dcState.links) {
		if strings.HasPrefix(name, connection+"/gitRepositoryLinks/") {
			out = append(out, proto.Clone(dcState.links[name]).(*dcpb.GitRepositoryLink))
		}
	}
	return out
}

// dcJSONPrefix is where Developer Connect's JSON API is served on this
// port: its v1 paths (/v1/projects/*/locations/*/connections) sit beside
// Artifact Registry's and Cloud Run's, so gcloud's developerconnect
// override and Terraform's developer_connect_custom_endpoint carry it.
const dcJSONPrefix = "/developerconnect"

// DeveloperConnectHandlerOr serves Developer Connect's JSON API (register
// adds it and the Operations service it polls) and passes every other
// request to next.
func DeveloperConnectHandlerOr(register func(grpc.ServiceRegistrar), next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.cloud.developerconnect.v1"}, Mixins: sourceDeployMixins}
	register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, dcJSONPrefix+"/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, dcJSONPrefix)
		r2.URL.RawPath = ""
		t.ServeHTTP(w, r2)
	})
}

// SourceManagerCloneURI is a repository's git URL, or "" when there is no
// such repository, for the console's Link repository.
func SourceManagerCloneURI(name string) string {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	return ssmState.repos[name].GetUris().GetGitHttps()
}
