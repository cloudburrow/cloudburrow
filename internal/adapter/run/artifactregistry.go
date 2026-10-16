package run

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ArtifactRegistryServer serves the repository calls of
// google.devtools.artifactregistry.v1.ArtifactRegistry that `gcloud run
// deploy --source` makes (#1347): get, create and list, and the update and
// delete Terraform's google_artifact_registry_repository makes (#1304).
// A DOCKER repository is also a Docker registry (#1383): its registryUri,
// {location}-docker.pkg.dev/{project}/{repository}, is served by the
// Docker Registry HTTP API v2 on this port, and its packages, versions,
// tags, Docker images and files are read from what that registry holds
// (artifactregistrycontent.go). Images built on this instance are pushed
// there too, and still loaded into the cluster as dev.local/<image URI>
// for Cloud Run's fast path. A repository's IAM policy is stored, never
// enforced (ADR-0006; artifactregistryiam.go). Other formats, remote and virtual
// repositories and every other method are UNIMPLEMENTED.
type ArtifactRegistryServer struct {
	arpb.UnimplementedArtifactRegistryServer
	s *Server
}

// ArtifactRegistry returns the Artifact Registry repository service.
func (s *Server) ArtifactRegistry() *ArtifactRegistryServer { return &ArtifactRegistryServer{s: s} }

// Register adds the service to a gRPC server or a JSON transcoder.
func (a *ArtifactRegistryServer) Register(g grpc.ServiceRegistrar) {
	arpb.RegisterArtifactRegistryServer(g, a)
}

// repositories is process-wide, like localBuilds: the images a repository's
// URL resolves to live in this instance's cluster for as long as it runs.
var repositories = struct {
	mu    sync.Mutex
	repos map[string]*arpb.Repository
	// policies are the repositories' stored IAM policies, by repository
	// name (artifactregistryiam.go).
	policies map[string]iampolicy.Stored
}{repos: map[string]*arpb.Repository{}, policies: map[string]iampolicy.Stored{}}

// repositoryID is AIP-122's resource ID shape Artifact Registry enforces:
// lower case letters, digits and hyphens, starting with a letter.
var repositoryID = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// RepositoryURL is the Docker registry path of a repository name,
// {location}-docker.pkg.dev/{project}/{repository}, which images built here
// are named under.
func RepositoryURL(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) != 6 {
		return ""
	}
	return fmt.Sprintf("%s-docker.pkg.dev/%s/%s", parts[3], parts[1], parts[5])
}

func parseRepositoryName(name string) (parent, id string, err error) {
	i := strings.LastIndex(name, "/repositories/")
	if i < 0 {
		return "", "", apierror.InvalidArgument("invalid repository name %q: want projects/*/locations/*/repositories/*", name)
	}
	parent, id = name[:i], name[i+len("/repositories/"):]
	if _, _, err := resource.ParseLocation(parent); err != nil || id == "" || strings.Contains(id, "/") {
		return "", "", apierror.InvalidArgument("invalid repository name %q: want projects/*/locations/*/repositories/*", name)
	}
	return parent, id, nil
}

// GetRepository reads a repository.
func (a *ArtifactRegistryServer) GetRepository(_ context.Context, req *arpb.GetRepositoryRequest) (*arpb.Repository, error) {
	if _, _, err := parseRepositoryName(req.GetName()); err != nil {
		return nil, err
	}
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	r, ok := repositories.repos[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return proto.Clone(r).(*arpb.Repository), nil
}

// ListRepositories lists a location's repositories, by name.
func (a *ArtifactRegistryServer) ListRepositories(_ context.Context, req *arpb.ListRepositoriesRequest) (*arpb.ListRepositoriesResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetPageToken() != "" || req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("ListRepositories page_token, filter and order_by are not supported")
	}
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	out := &arpb.ListRepositoriesResponse{}
	for name, r := range repositories.repos {
		if strings.HasPrefix(name, req.GetParent()+"/repositories/") {
			out.Repositories = append(out.Repositories, proto.Clone(r).(*arpb.Repository))
		}
	}
	sort.Slice(out.Repositories, func(i, j int) bool { return out.Repositories[i].Name < out.Repositories[j].Name })
	return out, nil
}

// CreateRepository stores a DOCKER repository and returns its operation,
// already done with the repository, as Artifact Registry's completes in
// moments. Other formats, and remote or virtual repositories, are refused:
// nothing here serves their packages.
func (a *ArtifactRegistryServer) CreateRepository(_ context.Context, req *arpb.CreateRepositoryRequest) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !repositoryID.MatchString(req.GetRepositoryId()) {
		return nil, apierror.InvalidArgument("invalid repository_id %q", req.GetRepositoryId())
	}
	r := proto.Clone(req.GetRepository()).(*arpb.Repository)
	if r == nil {
		r = &arpb.Repository{}
	}
	if r.GetFormat() != arpb.Repository_DOCKER {
		return nil, apierror.Unimplemented("only DOCKER repositories are supported: images built on this instance are its only artifacts")
	}
	if m := r.GetMode(); m != arpb.Repository_MODE_UNSPECIFIED && m != arpb.Repository_STANDARD_REPOSITORY {
		return nil, apierror.Unimplemented("repository mode %v is not supported", m)
	}
	// Cleanup policies are stored and read back, as documented; nothing
	// here runs them, as Artifact Registry runs them only daily.
	if r.GetKmsKeyName() != "" || r.GetFormatConfig() != nil || r.GetModeConfig() != nil {
		return nil, apierror.Unimplemented("kms_key_name and format or mode configs are not supported")
	}
	name := req.GetParent() + "/repositories/" + req.GetRepositoryId()
	now := timestamppb.New(time.Now())
	r.Name, r.Mode, r.CreateTime, r.UpdateTime = name, arpb.Repository_STANDARD_REPOSITORY, now, now
	r.RegistryUri = RepositoryURL(name)
	repositories.mu.Lock()
	if _, ok := repositories.repos[name]; ok {
		repositories.mu.Unlock()
		return nil, apierror.AlreadyExists("the repository already exists")
	}
	repositories.repos[name] = r
	saveRepositoriesLocked()
	repositories.mu.Unlock()
	op := a.s.ops.Create(req.GetParent(), name)
	_ = a.s.ops.SetMetadata(op.Name, &arpb.OperationMetadata{})
	_ = a.s.ops.Succeed(op.Name, proto.Clone(r))
	return a.s.toProtoOperation(op.Name)
}

// UpdateRepository applies the update mask's description, labels,
// cleanup_policies and cleanup_policy_dry_run, the fields a stored
// repository can change here; any other path is refused, as those fields
// are refused at create. An empty mask replaces all four.
func (a *ArtifactRegistryServer) UpdateRepository(_ context.Context, req *arpb.UpdateRepositoryRequest) (*arpb.Repository, error) {
	in := req.GetRepository()
	if _, _, err := parseRepositoryName(in.GetName()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"description", "labels", "cleanup_policies", "cleanup_policy_dry_run"}
	}
	for _, p := range paths {
		switch p {
		case "description", "labels", "cleanup_policies", "cleanup_policy_dry_run":
		default:
			return nil, apierror.Unimplemented("update_mask path %q is not supported: only description, labels and cleanup policies can change", p)
		}
	}
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	r, ok := repositories.repos[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	r = proto.Clone(r).(*arpb.Repository)
	for _, p := range paths {
		switch p {
		case "description":
			r.Description = in.GetDescription()
		case "labels":
			r.Labels = in.GetLabels()
		case "cleanup_policies":
			r.CleanupPolicies = in.GetCleanupPolicies()
		case "cleanup_policy_dry_run":
			r.CleanupPolicyDryRun = in.GetCleanupPolicyDryRun()
		}
	}
	r.UpdateTime = timestamppb.New(time.Now())
	repositories.repos[in.GetName()] = r
	saveRepositoriesLocked()
	return proto.Clone(r).(*arpb.Repository), nil
}

// DeleteRepository removes a repository, and every image its registry
// holds, and returns its operation, already done. Images built under its
// URL stay loaded in the cluster: they are this instance's build cache.
func (a *ArtifactRegistryServer) DeleteRepository(_ context.Context, req *arpb.DeleteRepositoryRequest) (*longrunningpb.Operation, error) {
	parent, _, err := parseRepositoryName(req.GetName())
	if err != nil {
		return nil, err
	}
	if !RemoveRepository(req.GetName()) {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	op := a.s.ops.Create(parent, req.GetName())
	_ = a.s.ops.SetMetadata(op.Name, &arpb.OperationMetadata{})
	_ = a.s.ops.Succeed(op.Name, &emptypb.Empty{})
	return a.s.toProtoOperation(op.Name)
}

// RemoveRepository deletes a stored repository by name, for
// DeleteRepository and the console, and reports whether it was there.
func RemoveRepository(name string) bool {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	if _, ok := repositories.repos[name]; !ok {
		return false
	}
	delete(repositories.repos, name)
	saveRepositoriesLocked()
	if _, ok := repositories.policies[name]; ok {
		delete(repositories.policies, name)
		saveRepositoryPoliciesLocked()
	}
	if st := RegistryStore(); st != nil {
		_ = st.DeleteRepository(repoOf(name))
	}
	return true
}

// Repositories lists every stored repository, for the console.
func Repositories() []*arpb.Repository {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	out := make([]*arpb.Repository, 0, len(repositories.repos))
	for _, r := range repositories.repos {
		out = append(out, proto.Clone(r).(*arpb.Repository))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// repositoriesFile is where the repositories are saved, beside the
// registry's content, so both survive a restart of the instance.
const repositoriesFile = "repositories.json"

// saveRepositoriesLocked writes every repository to the registry's state
// directory; with no store, repositories are kept in memory only.
func saveRepositoriesLocked() {
	st := RegistryStore()
	if st == nil {
		return
	}
	list := &arpb.ListRepositoriesResponse{}
	for _, r := range repositories.repos {
		list.Repositories = append(list.Repositories, r)
	}
	sort.Slice(list.Repositories, func(i, j int) bool { return list.Repositories[i].Name < list.Repositories[j].Name })
	b, err := protojson.Marshal(list)
	if err != nil {
		return
	}
	_ = st.WriteState(repositoriesFile, b)
}

// loadRepositories reads the repositories a previous run saved.
func loadRepositories(st *registry.Store) {
	b, err := st.ReadState(repositoriesFile)
	if err != nil {
		return
	}
	list := &arpb.ListRepositoriesResponse{}
	if protojson.Unmarshal(b, list) != nil {
		return
	}
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	for _, r := range list.GetRepositories() {
		repositories.repos[r.GetName()] = r
	}
	loadRepositoryPoliciesLocked(st)
}
