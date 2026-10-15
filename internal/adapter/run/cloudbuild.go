package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cloud Build (#1347). Every source build this instance runs, through Cloud
// Run's SubmitBuild, is a google.devtools.cloudbuild.v1.Build: its
// operation, projects/{p}/locations/{l}/operations/{build id}, carries
// BuildOperationMetadata while it runs and answers with the Build, which is
// what `gcloud run deploy --source` reconstructs and polls. GetBuild and
// ListBuilds read them back; CreateBuild and CancelBuild
// (cloudbuildcreate.go) run and stop builds of their own. The other methods
// are UNIMPLEMENTED.

// BuildLogURLBase, when set, is where a build's logUrl points: the
// instance's console, which the CLI sets. A build's log is the builder's
// output, kept in the build's statusDetail on failure.
var BuildLogURLBase string

// cloudBuilds is process-wide, like localBuilds.
var cloudBuilds = struct {
	mu     sync.Mutex
	builds map[string]*cbpb.Build
}{builds: map[string]*cbpb.Build{}}

// newCloudBuild records a QUEUED build of image under parent
// (projects/{p}/locations/{l}).
func newCloudBuild(parent, image string, kind BuildKind, bucket, object string) *cbpb.Build {
	project, location, _ := resource.ParseLocation(parent)
	id := uuid.NewString()
	step := &cbpb.BuildStep{Name: "docker", Args: []string{"build", "-t", image, "."}, Status: cbpb.Build_QUEUED}
	if kind == BuildBuildpacks {
		step = &cbpb.BuildStep{Name: "pack", Args: []string{"build", image}, Status: cbpb.Build_QUEUED}
	}
	b := &cbpb.Build{
		Name: parent + "/builds/" + id, Id: id, ProjectId: project, Status: cbpb.Build_QUEUED,
		Source: &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: object}}},
		Steps:  []*cbpb.BuildStep{step}, Images: []string{image}, CreateTime: timestamppb.Now(),
	}
	if BuildLogURLBase != "" {
		b.LogUrl = strings.TrimSuffix(BuildLogURLBase, "/") + "/run/builds?project=" + project + "&location=" + location + "&id=" + id
	}
	cloudBuilds.mu.Lock()
	cloudBuilds.builds[b.Name] = b
	cloudBuilds.mu.Unlock()
	return proto.Clone(b).(*cbpb.Build)
}

// updateCloudBuild changes a recorded build under the lock and returns a copy.
func updateCloudBuild(name string, f func(*cbpb.Build)) *cbpb.Build {
	cloudBuilds.mu.Lock()
	defer cloudBuilds.mu.Unlock()
	b := cloudBuilds.builds[name]
	f(b)
	return proto.Clone(b).(*cbpb.Build)
}

// buildDigest is the digest a build reports for its image. The image is
// loaded into the cluster rather than pushed, so there is no registry
// manifest digest; this one is derived from the build's ID, and a service
// that names image@digest runs the build's local image.
func buildDigest(id string) string {
	sum := sha256.Sum256([]byte("cloudburrow-build:" + id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CloudBuildServer serves google.devtools.cloudbuild.v1.CloudBuild's
// GetBuild and ListBuilds over the builds this instance ran.
type CloudBuildServer struct {
	cbpb.UnimplementedCloudBuildServer
	// builds runs CreateBuild's builds: nil leaves CreateBuild
	// FAILED_PRECONDITION.
	builds *BuildsServer
}

// CloudBuild returns the Cloud Build service, which reads builds but cannot
// create them; BuildsServer.CloudBuild returns one that can.
func (s *Server) CloudBuild() *CloudBuildServer { return &CloudBuildServer{} }

// CloudBuild returns the Cloud Build service, whose CreateBuild runs builds
// with this Builds service's source fetcher and local builder.
func (b *BuildsServer) CloudBuild() *CloudBuildServer { return &CloudBuildServer{builds: b} }

// Register adds the service to a gRPC server or a JSON transcoder.
func (c *CloudBuildServer) Register(g grpc.ServiceRegistrar) { cbpb.RegisterCloudBuildServer(g, c) }

// GetBuild reads a build by name, or by project_id and id (the global
// form, which finds it in any location).
func (c *CloudBuildServer) GetBuild(_ context.Context, req *cbpb.GetBuildRequest) (*cbpb.Build, error) {
	cloudBuilds.mu.Lock()
	defer cloudBuilds.mu.Unlock()
	if req.GetName() != "" {
		if b, ok := cloudBuilds.builds[req.GetName()]; ok {
			return proto.Clone(b).(*cbpb.Build), nil
		}
		return nil, apierror.NotFound("build %s not found", req.GetName())
	}
	for _, b := range cloudBuilds.builds {
		if b.ProjectId == req.GetProjectId() && b.Id == req.GetId() {
			return proto.Clone(b).(*cbpb.Build), nil
		}
	}
	return nil, apierror.NotFound("build %s not found", req.GetId())
}

// GetDefaultServiceAccount names the project's build service account,
// projects/{p}/serviceAccounts/{number}@cloudbuild.gserviceaccount.com:
// Cloud Build's legacy account, which gcloud accepts without asking IAM
// to describe it. Builds here run as the host's Docker, under no account.
func (c *CloudBuildServer) GetDefaultServiceAccount(_ context.Context, req *cbpb.GetDefaultServiceAccountRequest) (*cbpb.DefaultServiceAccount, error) {
	loc, ok := strings.CutSuffix(req.GetName(), "/defaultServiceAccount")
	project, _, err := resource.ParseLocation(loc)
	if !ok || err != nil {
		return nil, apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/defaultServiceAccount", req.GetName())
	}
	email := fmt.Sprintf("%d@cloudbuild.gserviceaccount.com", resource.ProjectNumber(project))
	return &cbpb.DefaultServiceAccount{Name: req.GetName(),
		ServiceAccountEmail: "projects/" + project + "/serviceAccounts/" + email}, nil
}

// ListBuilds lists a project's builds, newest first, in one location when
// parent names one.
func (c *CloudBuildServer) ListBuilds(_ context.Context, req *cbpb.ListBuildsRequest) (*cbpb.ListBuildsResponse, error) {
	if req.GetPageToken() != "" || req.GetFilter() != "" {
		return nil, apierror.Unimplemented("ListBuilds page_token and filter are not supported")
	}
	project, prefix := req.GetProjectId(), ""
	if req.GetParent() != "" {
		p, _, err := resource.ParseLocation(req.GetParent())
		if err != nil {
			return nil, apierror.InvalidArgument("%v", err)
		}
		project, prefix = p, req.GetParent()+"/builds/"
	}
	return &cbpb.ListBuildsResponse{Builds: CloudBuilds(project, prefix)}, nil
}

// CloudBuilds lists a project's recorded builds whose names start with
// prefix, newest first; the console's build history reads it.
func CloudBuilds(project, prefix string) []*cbpb.Build {
	cloudBuilds.mu.Lock()
	defer cloudBuilds.mu.Unlock()
	var out []*cbpb.Build
	for name, b := range cloudBuilds.builds {
		if b.ProjectId == project && strings.HasPrefix(name, prefix) {
			out = append(out, proto.Clone(b).(*cbpb.Build))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetCreateTime().AsTime().After(out[j].GetCreateTime().AsTime()) })
	return out
}

// sourceDeployMixins bind the Operations mixin at the v1 paths Cloud Build
// (cloudbuild_v1.yaml) and Artifact Registry (artifactregistry_v1.yaml) use.
// Both are answered by the adapter's one operations store.
var sourceDeployMixins = []*annotations.HttpRule{
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=operations/**}"}},
}

// SourceDeployHandler serves the Cloud Build and Artifact Registry v1 JSON
// APIs on the Run port beside next, which gets every other request. gcloud
// reaches them through its cloudbuild and artifactregistry endpoint
// overrides, which `cloudburrow gcloud-setup` points at this port.
func SourceDeployHandler(register func(grpc.ServiceRegistrar), next http.Handler) http.Handler {
	t := &rest.Transcoder{
		Packages: []string{"google.devtools.cloudbuild.v1", "google.devtools.artifactregistry.v1"},
		Mixins:   sourceDeployMixins,
	}
	register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSourceDeployPath(r.URL.Path) {
			t.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isSourceDeployPath reports whether a path is Cloud Build's or Artifact
// Registry's: Cloud Run's v1 and v2 paths never take these shapes.
func isSourceDeployPath(p string) bool {
	if strings.HasPrefix(p, "/v1/operations/") {
		return true
	}
	rest, ok := strings.CutPrefix(p, "/v1/projects/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) >= 2 && strings.HasPrefix(parts[1], "builds") {
		return true // projects/{p}/builds
	}
	if len(parts) >= 4 && parts[1] == "locations" {
		switch strings.SplitN(parts[3], ":", 2)[0] {
		case "builds", "repositories", "operations", "defaultServiceAccount":
			return true
		}
	}
	return false
}
