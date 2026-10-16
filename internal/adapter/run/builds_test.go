package run

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/grpc/codes"
)

type fakeFetcher struct{ objects map[string]string }

func (f fakeFetcher) Fetch(_ context.Context, bucket, object string, _ int64) (io.ReadCloser, error) {
	body, ok := f.objects[bucket+"/"+object]
	if !ok {
		return nil, apierror.NotFound("storage_source gs://%s/%s not found", bucket, object)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

type fakeBuilder struct {
	got  chan BuildSpec
	fail error
}

func (b *fakeBuilder) Build(_ context.Context, spec BuildSpec) error {
	data, _ := io.ReadAll(spec.Archive)
	spec.Archive = strings.NewReader(string(data))
	spec.Dockerfile += "|" + string(data)
	b.got <- spec
	return b.fail
}

func submit(img string) *runpb.SubmitBuildRequest {
	return &runpb.SubmitBuildRequest{
		Parent:    "projects/proj-1/locations/us-central1",
		Source:    &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: "b", Object: "src.tgz"}},
		ImageUri:  img,
		BuildType: &runpb.SubmitBuildRequest_DockerBuild_{DockerBuild: &runpb.SubmitBuildRequest_DockerBuild{}},
	}
}

func waitOp(t *testing.T, s *Server, name string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if op, err := s.Operation(name); err == nil && op.GetDone() {
			return
		}
	}
	t.Fatalf("operation %s never finished", name)
}

func TestSubmitBuildDockerMakesImageUsable(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 1)}
	bs := s.Builds(fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}, b)
	const uri = "us-central1-docker.pkg.dev/p/cloud-run-source-deploy/unit-built-svc"
	resp, err := bs.SubmitBuild(context.Background(), submit(uri))
	if err != nil {
		t.Fatalf("SubmitBuild: %v", err)
	}
	op := resp.GetBuildOperation()
	if !strings.HasPrefix(op.GetName(), "projects/proj-1/locations/us-central1/operations/") {
		t.Errorf("operation name = %q", op.GetName())
	}
	spec := <-b.got
	if spec.Kind != BuildDocker || spec.Dockerfile != "Dockerfile|archive" || spec.LocalImage != "dev.local/"+uri+":latest" {
		t.Errorf("spec = %+v", spec)
	}
	waitOp(t, s, op.GetName())
	if got, _ := s.Operation(op.GetName()); got.GetError() != nil {
		t.Fatalf("operation failed: %v", got.GetError())
	}
	if got := containerImage(uri); got != "dev.local/"+uri+":latest" {
		t.Errorf("containerImage(%q) = %q; want the loaded local image", uri, got)
	}
	if got := containerImage(uri + ":latest"); got != "dev.local/"+uri+":latest" {
		t.Errorf("the tagged form did not resolve: %q", got)
	}
	if got, ok := localBuilds.original("dev.local/" + uri + ":latest"); !ok || got != uri+":latest" {
		t.Errorf("original = %q, %v", got, ok)
	}
}

func TestSubmitBuildFailureFailsOperation(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 1), fail: errors.New("no Dockerfile")}
	bs := s.Builds(fakeFetcher{objects: map[string]string{"b/src.tgz": "x"}}, b)
	resp, err := bs.SubmitBuild(context.Background(), submit("gcr.io/p/unit-failed:v1"))
	if err != nil {
		t.Fatalf("SubmitBuild: %v", err)
	}
	<-b.got
	waitOp(t, s, resp.GetBuildOperation().GetName())
	got, _ := s.Operation(resp.GetBuildOperation().GetName())
	if got.GetError() == nil || !strings.Contains(got.GetError().GetMessage(), "no Dockerfile") {
		t.Errorf("operation = %v; want the build's failure", got)
	}
	if _, ok := localBuilds.resolve("gcr.io/p/unit-failed:v1"); ok {
		t.Error("a failed build was recorded as usable")
	}
}

func TestSubmitBuildRefusals(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	bs := s.Builds(fakeFetcher{}, &fakeBuilder{got: make(chan BuildSpec, 1)})
	cases := []struct {
		name string
		mut  func(*runpb.SubmitBuildRequest)
		code codes.Code
		want string
	}{
		{"bad parent", func(r *runpb.SubmitBuildRequest) { r.Parent = "projects/p" }, codes.InvalidArgument, ""},
		{"no source", func(r *runpb.SubmitBuildRequest) { r.Source = nil }, codes.InvalidArgument, "storage_source"},
		{"no image", func(r *runpb.SubmitBuildRequest) { r.ImageUri = "" }, codes.InvalidArgument, "image_uri"},
		{"digest", func(r *runpb.SubmitBuildRequest) { r.ImageUri = "gcr.io/p/a@sha256:" + strings.Repeat("a", 64) }, codes.InvalidArgument, "digest"},
		{"no build type", func(r *runpb.SubmitBuildRequest) { r.BuildType = nil }, codes.InvalidArgument, "buildpack_build or docker_build"},
		{"worker pool", func(r *runpb.SubmitBuildRequest) { r.WorkerPool = "projects/p/locations/l/workerPools/w" }, codes.Unimplemented, "worker_pool"},
		{"base image", func(r *runpb.SubmitBuildRequest) {
			r.BuildType = &runpb.SubmitBuildRequest_BuildpackBuild{BuildpackBuild: &runpb.SubmitBuildRequest_BuildpacksBuild{BaseImage: "x"}}
		}, codes.Unimplemented, "buildpack_build.base_image"},
		{"missing object", func(r *runpb.SubmitBuildRequest) {}, codes.NotFound, "gs://b/src.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := submit("gcr.io/p/a:v1")
			tc.mut(req)
			_, err := bs.SubmitBuild(context.Background(), req)
			e := apierror.From(err)
			if err == nil || e.Code != tc.code || !strings.Contains(e.Message, tc.want) {
				t.Errorf("SubmitBuild = %v; want %v naming %q", err, tc.code, tc.want)
			}
		})
	}
	// No Cloud Storage on the instance: said plainly.
	_, err := s.Builds(nil, &fakeBuilder{}).SubmitBuild(context.Background(), submit("gcr.io/p/a:v1"))
	if e := apierror.From(err); e.Code != codes.FailedPrecondition || !strings.Contains(e.Message, "storage") {
		t.Errorf("without storage: %v", err)
	}
}

// A build is recorded under its tag and its digest; the fallback lookup
// must answer the same every time (Go's map order varies), and a service's
// own record keeps whichever reference was deployed.
func TestBuiltImageReadsBackAsDeployed(t *testing.T) {
	const uri = "us-central1-docker.pkg.dev/p/cloud-run-source-deploy/unit-readback"
	const local = "dev.local/" + uri + ":latest"
	digest := untagged(uri) + "@sha256:" + strings.Repeat("ab", 32)
	localBuilds.record(uri, local)
	localBuilds.record(digest, local)
	for i := 0; i < 50; i++ {
		if got, ok := localBuilds.original(local); !ok || got != uri+":latest" {
			t.Fatalf("original = %q, %v; want the tag every time", got, ok)
		}
	}
	for _, deployed := range []string{uri + ":latest", digest} {
		got := builtImagesOf([]*runpb.Container{{Image: deployed}})
		if got[local] != deployed {
			t.Errorf("builtImagesOf(%q) = %v; want %s recorded for %s", deployed, got, deployed, local)
		}
	}
}
