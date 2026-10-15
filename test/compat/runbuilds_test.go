//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
)

func buildsClient(t *testing.T, h *Harness) *run.BuildsClient {
	t.Helper()
	c, err := run.NewBuildsClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewBuildsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sourceArchive tars and gzips files, as `gcloud run deploy --source` uploads
// a source directory.
func sourceArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uploadSource writes the archive to a fresh bucket in the instance's Cloud
// Storage and returns the bucket name.
func uploadSource(ctx context.Context, t *testing.T, h *Harness, object string, archive []byte) string {
	t.Helper()
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(),
		option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	bh := bucket(t, h, sc)
	w := bh.Object(object).NewWriter(ctx)
	if _, err := w.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload the source: %v", err)
	}
	return bh.BucketName()
}

// waitBuild polls the build operation through the Builds client's own
// Operations mixin until it is done.
func waitBuild(ctx context.Context, t *testing.T, bc *run.BuildsClient, op *longrunningpb.Operation) *longrunningpb.Operation {
	t.Helper()
	for {
		got, err := bc.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
		if err != nil {
			t.Fatalf("GetOperation %s: %v", op.GetName(), err)
		}
		if got.GetDone() {
			return got
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the build never finished: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// covers: google.cloud.run.v2.Builds/SubmitBuild
//
// TestRunSubmitBuildDockerDeploys (#1268) uploads a source archive (a
// Dockerfile and a static server built here from testdata/sourcebuild) to the
// instance's Cloud Storage, builds it through the official BuildsClient's
// SubmitBuild with docker_build, waits for the build operation, deploys the
// image_uri through CreateService and reads the server's answer through the
// Knative ingress. It also asserts the refusals: a missing object is
// NOT_FOUND, and a field that needs Cloud Build is UNIMPLEMENTED naming it.
func TestRunSubmitBuildDockerDeploys(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: SubmitBuild builds with the host's Docker")
	}
	bc := buildsClient(t, h)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The server, static, for the host's architecture, which is the kind
	// nodes' architecture; the build targets the nodes' platform.
	bin := filepath.Join(t.TempDir(), "server")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	build.Dir = filepath.Join(moduleRoot(t), "testdata", "sourcebuild")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the fixture server: %v\n%s", err, out)
	}
	server, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	archive := sourceArchive(t, map[string][]byte{
		"Dockerfile": []byte("FROM scratch\nCOPY server /server\nENTRYPOINT [\"/server\"]\n"),
		"server":     server,
	})
	bucketName := uploadSource(ctx, t, h, "source/compat-built.tgz", archive)

	parent := runParent(h)
	imageURI := "us-central1-docker.pkg.dev/" + h.Project() + "/cloud-run-source-deploy/compat-built"
	req := &runpb.SubmitBuildRequest{
		Parent:    parent,
		Source:    &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: bucketName, Object: "source/compat-built.tgz"}},
		ImageUri:  imageURI,
		BuildType: &runpb.SubmitBuildRequest_DockerBuild_{DockerBuild: &runpb.SubmitBuildRequest_DockerBuild{}},
	}

	// Refusals first: they must not start a build.
	missing := &runpb.SubmitBuildRequest{Parent: parent, ImageUri: imageURI, BuildType: req.BuildType,
		Source: &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: bucketName, Object: "no-such.tgz"}}}
	if _, err := bc.SubmitBuild(ctx, missing); status.Code(err) != codes.NotFound {
		t.Errorf("SubmitBuild of a missing object = %v; want NotFound", err)
	}
	pool := &runpb.SubmitBuildRequest{Parent: parent, ImageUri: imageURI, BuildType: req.BuildType, Source: req.Source,
		WorkerPool: parent + "/workerPools/pool"}
	if _, err := bc.SubmitBuild(ctx, pool); status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "worker_pool") {
		t.Errorf("SubmitBuild with worker_pool = %v; want Unimplemented naming worker_pool", err)
	}

	resp, err := bc.SubmitBuild(ctx, req)
	if err != nil {
		t.Fatalf("SubmitBuild: %v", err)
	}
	op := resp.GetBuildOperation()
	if !strings.HasPrefix(op.GetName(), parent+"/operations/") {
		t.Errorf("build operation name = %q; want it under %s/operations/", op.GetName(), parent)
	}
	done := waitBuild(ctx, t, bc, op)
	if done.GetError() != nil {
		t.Fatalf("the build failed: %v", done.GetError())
	}

	// The image_uri is usable by a following CreateService.
	id := "compat-built"
	name := parent + "/services/" + id
	cop, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: imageURI}}}}})
	if err != nil {
		t.Fatalf("CreateService with the built image: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("the service built from source never became ready: %v", err)
	}
	if got := svc.GetTemplate().GetContainers()[0].GetImage(); !strings.HasPrefix(got, imageURI) {
		t.Errorf("the service reads back image %q; want the image_uri %q", got, imageURI)
	}
	code, body := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), "", nil)
	if code != http.StatusOK || body != "built from source by SubmitBuild" {
		t.Errorf("the deployed service answered %d %q; want the fixture's greeting", code, body)
	}
}

// covers: google.cloud.run.v2.Builds/SubmitBuild
//
// TestFunctionsSubmitBuildBuildpacks (#1268) builds testdata/function with
// buildpack_build and function_target through SubmitBuild, and deploys it.
// Gated with the other Functions tests (functions.yml): it pulls Google's
// builder, needs pack, and needs amd64 nodes.
func TestFunctionsSubmitBuildBuildpacks(t *testing.T) {
	requireBuildTools(t)
	h := New(t)
	bc := buildsClient(t, h)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	removeNewPackVolumes(t)
	if runtime.GOARCH != strings.TrimPrefix(buildpacks.BuilderPlatform, "linux/") {
		t.Skipf("the Google builder produces %s images only", buildpacks.BuilderPlatform)
	}

	src := filepath.Join(moduleRoot(t), "testdata", "function")
	files := map[string][]byte{}
	for _, f := range []string{"function.go", "go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = b
	}
	bucketName := uploadSource(ctx, t, h, "fn.tgz", sourceArchive(t, files))
	parent := runParent(h)
	imageURI := "us-central1-docker.pkg.dev/" + h.Project() + "/cloud-run-source-deploy/compat-built-fn:v1"
	resp, err := bc.SubmitBuild(ctx, &runpb.SubmitBuildRequest{
		Parent:   parent,
		Source:   &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: bucketName, Object: "fn.tgz"}},
		ImageUri: imageURI,
		BuildType: &runpb.SubmitBuildRequest_BuildpackBuild{BuildpackBuild: &runpb.SubmitBuildRequest_BuildpacksBuild{
			FunctionTarget: "Hello"}},
	})
	if err != nil {
		t.Fatalf("SubmitBuild: %v", err)
	}
	if done := waitBuild(ctx, t, bc, resp.GetBuildOperation()); done.GetError() != nil {
		t.Fatalf("the build failed: %v", done.GetError())
	}
	svc := deployFunction(ctx, t, h, rc, "compat-built-fn", imageURI)
	code, body := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), `{"name":"SubmitBuild"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello SubmitBuild"`) {
		t.Errorf("POST / = %d %q; want the greeting", code, body)
	}
}
