package run

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SourceFetcher reads a source archive from the instance's Cloud Storage.
//
// It is an interface so the adapter does not import the storage
// implementation; the CLI wires it to the instance's storage endpoint.
type SourceFetcher interface {
	// Fetch opens bucket/object, at generation when it is not zero. A
	// missing object is reported as an apierror NotFound.
	Fetch(ctx context.Context, bucket, object string, generation int64) (io.ReadCloser, error)
}

// BuildKind is how a source is turned into an image.
type BuildKind int

const (
	// BuildDocker builds a Dockerfile in the source.
	BuildDocker BuildKind = iota + 1
	// BuildBuildpacks builds with Google's buildpacks builder.
	BuildBuildpacks
)

// BuildSpec is one build, as the adapter hands it to an ImageBuilder.
type BuildSpec struct {
	Kind BuildKind
	// Archive is the gzipped tarball SubmitBuild's storage_source names.
	Archive io.Reader
	// LocalImage is the dev.local reference the image must be loaded into
	// the cluster as; Knative runs it without resolving it remotely.
	LocalImage string
	// Dockerfile is the path of the Dockerfile inside the source, for
	// BuildDocker.
	Dockerfile string
	// FunctionTarget and Env are the buildpacks build's inputs.
	FunctionTarget string
	Env            map[string]string
}

// ImageBuilder builds a source archive into an image in the cluster.
type ImageBuilder interface {
	Build(ctx context.Context, spec BuildSpec) error
}

// builtImages records the images SubmitBuild produced, by the image_uri the
// caller named, so a following CreateService or UpdateService that names
// that same image_uri runs the locally loaded image rather than pulling a
// registry that cannot serve it.
type builtImages struct {
	mu sync.Mutex
	// local maps a normalised image_uri to its dev.local reference.
	local map[string]string
}

// localBuilds is process-wide: an instance runs one Cloud Run adapter per
// process, and renderContainers, which consults it, is shared by services and
// jobs that do not carry the adapter.
var localBuilds = &builtImages{local: map[string]string{}}

// tagged returns ref with :latest when it carries neither a tag nor a
// digest, as a registry push of an untagged reference does.
func tagged(ref string) string {
	if images.RequireTagged(ref) != nil {
		return ref + ":latest"
	}
	return ref
}

// untagged is ref without its tag, for its by-digest form.
func untagged(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

func (b *builtImages) record(uri, local string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.local[tagged(uri)] = local
}

// resolve returns the local reference SubmitBuild loaded for uri, if any.
func (b *builtImages) resolve(uri string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.local[tagged(uri)]
	return l, ok
}

// original returns the image_uri a local reference was built for, so a
// service reads back with the image it was deployed with.
func (b *builtImages) original(local string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for uri, l := range b.local {
		if l == local {
			return uri, true
		}
	}
	return "", false
}

// containerImage is the reference a container runs: the locally built image
// for an image_uri SubmitBuild produced, otherwise the localised reference.
func containerImage(ref string) string {
	if l, ok := localBuilds.resolve(ref); ok {
		return l
	}
	// An untagged Artifact Registry image is :latest, as Cloud Run reads
	// it: `gcloud run deploy --source` validates the service (dryRun)
	// with the bare image it is about to build (#1347). Any other bare
	// reference is still refused as a mutable target.
	if isArtifactRegistryImage(ref) {
		return images.Localise(tagged(ref))
	}
	return images.Localise(ref)
}

// isArtifactRegistryImage reports whether ref is under an Artifact Registry
// Docker host, {location}-docker.pkg.dev.
func isArtifactRegistryImage(ref string) bool {
	host, _, ok := strings.Cut(ref, "/")
	return ok && strings.HasSuffix(host, "-docker.pkg.dev")
}

// localImageFor is the dev.local reference an image_uri is built as: the
// whole URI under the local prefix, so two URIs never share an image.
func localImageFor(uri string) string {
	return images.LocalPrefix + tagged(uri)
}

// BuildsServer serves google.cloud.run.v2.Builds (#1268): SubmitBuild builds
// a source archive from the instance's Cloud Storage into an image in the
// cluster, without Cloud Build.
type BuildsServer struct {
	runpb.UnimplementedBuildsServer
	s       *Server
	fetch   SourceFetcher
	builder ImageBuilder
	timeout time.Duration
}

// Builds returns the Builds service. fetch or builder nil leaves SubmitBuild
// FAILED_PRECONDITION, naming what is missing, rather than unregistered.
func (s *Server) Builds(fetch SourceFetcher, builder ImageBuilder) *BuildsServer {
	return &BuildsServer{s: s, fetch: fetch, builder: builder, timeout: 30 * time.Minute}
}

// Register adds the Builds service to a gRPC server or a JSON transcoder.
func (b *BuildsServer) Register(g grpc.ServiceRegistrar) { runpb.RegisterBuildsServer(g, b) }

// unsupportedBuild names the first request field that needs Cloud Build
// itself, which is not served.
func unsupportedBuild(req *runpb.SubmitBuildRequest) error {
	var fields []string
	add := func(set bool, f string) {
		if set {
			fields = append(fields, f)
		}
	}
	add(req.GetServiceAccount() != "", "service_account")
	add(req.GetWorkerPool() != "", "worker_pool")
	add(req.GetMachineType() != "", "machine_type")
	if bp := req.GetBuildpackBuild(); bp != nil {
		add(bp.GetRuntime() != "", "buildpack_build.runtime")
		add(bp.GetCacheImageUri() != "", "buildpack_build.cache_image_uri")
		add(bp.GetBaseImage() != "", "buildpack_build.base_image")
		add(bp.GetEnableAutomaticUpdates(), "buildpack_build.enable_automatic_updates")
		add(bp.GetProjectDescriptor() != "", "buildpack_build.project_descriptor")
	}
	if len(fields) == 0 {
		return nil
	}
	return apierror.Unimplemented(
		"SubmitBuild field %s is not supported: it needs Google Cloud Build, which CloudBurrow does not run; "+
			"builds run locally with Docker or pack", strings.Join(fields, ", "))
}

// SubmitBuild validates the request, starts the build and returns at once
// with its long-running operation, as Cloud Run does. The operation succeeds
// when the image is loaded into the cluster, after which image_uri is usable
// by CreateService and UpdateService on this instance.
func (b *BuildsServer) SubmitBuild(ctx context.Context, req *runpb.SubmitBuildRequest) (*runpb.SubmitBuildResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	src := req.GetStorageSource()
	if src == nil {
		return nil, apierror.InvalidArgument("storage_source is required")
	}
	if src.GetBucket() == "" || src.GetObject() == "" {
		return nil, apierror.InvalidArgument("storage_source.bucket and storage_source.object are required")
	}
	uri := req.GetImageUri()
	if uri == "" {
		return nil, apierror.InvalidArgument("image_uri is required")
	}
	if strings.Contains(uri, "@") {
		return nil, apierror.InvalidArgument("image_uri %q must name a tag, not a digest: the digest is the build's output", uri)
	}
	if strings.ContainsAny(uri, " \t\n") || strings.ToLower(uri) != uri {
		return nil, apierror.InvalidArgument("image_uri %q is not a valid image reference", uri)
	}
	spec := BuildSpec{LocalImage: localImageFor(uri)}
	switch t := req.GetBuildType().(type) {
	case *runpb.SubmitBuildRequest_DockerBuild_:
		spec.Kind, spec.Dockerfile = BuildDocker, "Dockerfile"
	case *runpb.SubmitBuildRequest_BuildpackBuild:
		spec.Kind = BuildBuildpacks
		spec.FunctionTarget = t.BuildpackBuild.GetFunctionTarget()
		spec.Env = t.BuildpackBuild.GetEnvironmentVariables()
	default:
		return nil, apierror.InvalidArgument("one of buildpack_build or docker_build is required")
	}
	if err := unsupportedBuild(req); err != nil {
		return nil, err
	}
	if b.fetch == nil {
		return nil, apierror.FailedPrecondition("SubmitBuild reads its source from Cloud Storage, which is not enabled on this instance: start it with --services run,storage")
	}
	if b.builder == nil {
		return nil, apierror.FailedPrecondition("SubmitBuild has no local builder on this instance")
	}
	// The source is opened before the operation exists, so a missing object
	// is the call's error rather than a failed build. It is read under the
	// build's context, not the call's, which ends when SubmitBuild returns.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.timeout)
	rc, err := b.fetch.Fetch(bctx, src.GetBucket(), src.GetObject(), src.GetGeneration())
	if err != nil {
		cancel()
		return nil, err
	}
	// The operation is Cloud Build's, named after the build's ID, as
	// gcloud reconstructs it from the metadata's build (#1347).
	build := newCloudBuild(req.GetParent(), tagged(uri), spec.Kind, src.GetBucket(), src.GetObject())
	op := b.s.ops.CreateNamed(req.GetParent()+"/operations/"+build.GetId(), uri)
	_ = b.s.ops.SetMetadata(op.Name, &cbpb.BuildOperationMetadata{Build: build})
	go func() {
		defer cancel()
		defer rc.Close()
		spec.Archive = rc
		started := updateCloudBuild(build.GetName(), func(cb *cbpb.Build) {
			cb.Status, cb.StartTime, cb.Steps[0].Status = cbpb.Build_WORKING, timestamppb.Now(), cbpb.Build_WORKING
		})
		_ = b.s.ops.SetMetadata(op.Name, &cbpb.BuildOperationMetadata{Build: started})
		if err := b.builder.Build(bctx, spec); err != nil {
			failed := updateCloudBuild(build.GetName(), func(cb *cbpb.Build) {
				cb.Status, cb.FinishTime, cb.Steps[0].Status = cbpb.Build_FAILURE, timestamppb.Now(), cbpb.Build_FAILURE
				cb.StatusDetail = err.Error()
				cb.FailureInfo = &cbpb.Build_FailureInfo{Type: cbpb.Build_FailureInfo_USER_BUILD_STEP, Detail: err.Error()}
			})
			_ = b.s.ops.SetMetadata(op.Name, &cbpb.BuildOperationMetadata{Build: failed})
			_ = b.s.ops.Fail(op.Name, apierror.FailedPrecondition("build failed: %v", err))
			return
		}
		localBuilds.record(uri, spec.LocalImage)
		digest := buildDigest(build.GetId())
		localBuilds.record(untagged(uri)+"@"+digest, spec.LocalImage)
		done := updateCloudBuild(build.GetName(), func(cb *cbpb.Build) {
			cb.Status, cb.FinishTime, cb.Steps[0].Status = cbpb.Build_SUCCESS, timestamppb.Now(), cbpb.Build_SUCCESS
			cb.Results = &cbpb.Results{Images: []*cbpb.BuiltImage{{Name: tagged(uri), Digest: digest}}}
		})
		_ = b.s.ops.SetMetadata(op.Name, &cbpb.BuildOperationMetadata{Build: done})
		_ = b.s.ops.Succeed(op.Name, done)
	}()
	pop, err := b.s.toProtoOperation(op.Name)
	if err != nil {
		return nil, err
	}
	return &runpb.SubmitBuildResponse{BuildOperation: pop}, nil
}

// String names a build kind in messages.
func (k BuildKind) String() string {
	switch k {
	case BuildDocker:
		return "docker"
	case BuildBuildpacks:
		return "buildpacks"
	}
	return fmt.Sprintf("BuildKind(%d)", int(k))
}
