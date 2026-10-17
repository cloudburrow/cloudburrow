//go:build compat

package compat

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
	"github.com/cloudburrow/cloudburrow/internal/images"
)

func functionsClient(t *testing.T, h *Harness) *functions.FunctionClient {
	t.Helper()
	c, err := functions.NewFunctionClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatalf("functions.NewFunctionClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// putSource PUTs data to a GenerateUploadUrl URL with no credentials, as
// gcloud and the console do.
func putSource(ctx context.Context, t *testing.T, uploadURL string, data []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/zip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT the source: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT the source = %s %s", resp.Status, b)
	}
}

// covers: google.cloud.functions.v2.FunctionService/GenerateUploadUrl, google.cloud.functions.v2.FunctionService/GenerateDownloadUrl, google.cloud.functions.v2.FunctionService/CreateFunction, google.cloud.functions.v2.FunctionService/GetFunction, google.cloud.functions.v2.FunctionService/ListFunctions, google.cloud.functions.v2.FunctionService/UpdateFunction, google.cloud.functions.v2.FunctionService/DeleteFunction, google.cloud.functions.v2.FunctionService/ListRuntimes
//
// TestFunctionsAPI (#1377): the official Cloud Functions v2 client, over
// gRPC on the Run port, through everything but a build that succeeds, so
// it runs on any instance with --services run,storage. A source PUT to a
// GenerateUploadUrl URL is read back through GenerateDownloadUrl; a function
// built from it (not a zip) is created DEPLOYING, its operation fails with
// the build's error, and it is FAILED, with service_config's defaults; get
// and list find it; a description and labels update changes nothing that
// runs and completes at once; its IAM policy is stored and read back, not
// enforced; delete removes it. An audit log trigger (#1414 serves Pub/Sub
// and Cloud Storage ones), a second create and a missing function are refused.
func TestFunctionsAPI(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	fc := functionsClient(t, h)

	foundGo := false
	rt, err := fc.ListRuntimes(ctx, &functionspb.ListRuntimesRequest{Parent: parent})
	if err != nil {
		t.Fatalf("ListRuntimes: %v", err)
	}
	for _, r := range rt.GetRuntimes() {
		foundGo = foundGo || (r.GetName() == "go123" && r.GetEnvironment() == functionspb.Environment_GEN_2)
	}
	if !foundGo {
		t.Errorf("ListRuntimes = %v; want go123 among them", rt.GetRuntimes())
	}
	locs := fc.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + h.Project()})
	if l, err := locs.Next(); err != nil || !strings.HasPrefix(l.GetName(), "projects/"+h.Project()+"/locations/") {
		t.Errorf("ListLocations = %v, %v", l, err)
	}

	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage) // the upload URL is a bucket's
	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	if up.GetUploadUrl() == "" || up.GetStorageSource().GetBucket() == "" || !strings.HasSuffix(up.GetStorageSource().GetObject(), ".zip") {
		t.Fatalf("GenerateUploadUrl = %v", up)
	}
	notAZip := []byte("this is not a zip archive")
	putSource(ctx, t, up.GetUploadUrl(), notAZip)

	id := "sdk-fn"
	name := parent + "/functions/" + id
	in := &functionspb.Function{
		Description: "sdk",
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source:               &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}},
			EnvironmentVariables: map[string]string{"BUILD_VAR": "1"}},
		ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: "512M", TimeoutSeconds: 30,
			EnvironmentVariables: map[string]string{"GREETING": "hi"}, MaxInstanceCount: 3, MinInstanceCount: 0,
			ServiceAccountEmail: "fn@" + h.Project() + ".iam.gserviceaccount.com"},
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := fc.DeleteFunction(dctx, &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: in})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetTarget() != name || md.GetOperationType() != functionspb.OperationType_CREATE_FUNCTION {
		t.Errorf("CreateFunction metadata = %v, %v", md, err)
	}
	if _, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: in}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateFunction = %v; want ALREADY_EXISTS", err)
	}
	if _, err := op.Wait(ctx); err == nil || !strings.Contains(err.Error(), "Build failed") {
		t.Fatalf("CreateFunction.Wait of a source that is not a zip = %v; want the build's error", err)
	}

	got, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetFunction: %v", err)
	}
	sc := got.GetServiceConfig()
	if got.GetState() != functionspb.Function_FAILED || got.GetEnvironment() != functionspb.Environment_GEN_2 ||
		got.GetBuildConfig().GetRuntime() != "go123" || got.GetBuildConfig().GetEntryPoint() != "Hello" ||
		sc.GetAvailableMemory() != "512M" || sc.GetTimeoutSeconds() != 30 || sc.GetMaxInstanceCount() != 3 ||
		sc.GetEnvironmentVariables()["GREETING"] != "hi" || sc.GetServiceAccountEmail() != in.GetServiceConfig().GetServiceAccountEmail() ||
		len(got.GetStateMessages()) == 0 {
		t.Errorf("GetFunction = %v; want it FAILED with what was asked for", got)
	}
	listed := false
	it := fc.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFunctions: %v", err)
		}
		listed = listed || f.GetName() == name
	}
	if !listed {
		t.Errorf("ListFunctions did not list %s", name)
	}

	dl, err := fc.GenerateDownloadUrl(ctx, &functionspb.GenerateDownloadUrlRequest{Name: name})
	if err != nil {
		t.Fatalf("GenerateDownloadUrl: %v", err)
	}
	resp, err := http.Get(dl.GetDownloadUrl())
	if err != nil {
		t.Fatalf("GET the download URL: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, notAZip) {
		t.Errorf("GET the download URL = %s %q; want the uploaded source", resp.Status, body)
	}

	uop, err := fc.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{
		Function:   &functionspb.Function{Name: name, Description: "updated"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	if upd, err := uop.Wait(ctx); err != nil || upd.GetDescription() != "updated" || upd.GetBuildConfig().GetRuntime() != "go123" {
		t.Errorf("UpdateFunction of the description = %v, %v", upd, err)
	}

	policy, err := fc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/cloudfunctions.invoker", Members: []string{"allUsers"}}}}})
	if err != nil || len(policy.GetBindings()) != 1 {
		t.Fatalf("SetIamPolicy = %v, %v", policy, err)
	}
	if p, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); err != nil ||
		len(p.GetBindings()) != 1 || p.GetBindings()[0].GetMembers()[0] != "allUsers" {
		t.Errorf("GetIamPolicy = %v, %v", p, err)
	}
	if p, err := fc.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name,
		Permissions: []string{"cloudfunctions.functions.invoke"}}); err != nil || len(p.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", p, err)
	}

	trig := &functionspb.Function{BuildConfig: in.GetBuildConfig(),
		EventTrigger: &functionspb.EventTrigger{EventType: "google.cloud.audit.log.v1.written"}}
	if _, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "sdk-fn-trigger", Function: trig}); status.Code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "event_type") {
		t.Errorf("CreateFunction with an audit log trigger = %v; want UNIMPLEMENTED naming the event type", err)
	}

	dop, err := fc.DeleteFunction(ctx, &functionspb.DeleteFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Errorf("DeleteFunction.Wait: %v", err)
	}
	if _, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetFunction after delete = %v; want NOT_FOUND", err)
	}
}

// zipFunction zips a fixture directory, as gcloud functions deploy does.
func zipFunction(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// requireAmd64Functions skips unless the Functions tests are asked for and
// the cluster's nodes are amd64, which Google's builder's images need.
func requireAmd64Functions(ctx context.Context, t *testing.T) {
	t.Helper()
	if os.Getenv(EnvFunctions) == "" {
		t.Skipf("%s is not set; this test builds a function from source with Google's builder, so it runs only when asked for", EnvFunctions)
	}
	if _, err := exec.LookPath("pack"); err != nil {
		t.Fatalf("pack is required: %v", err)
	}
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if cluster == "" || kubeconfig == "" {
		t.Fatalf("CLOUDBURROW_TEST_CLUSTER and %s are required to read the node architecture", envKubeconfig)
	}
	loader := &images.Loader{ClusterName: cluster, Runner: buildpacks.ExecRunner{}}
	arch, err := loader.NodeArchitecture(ctx, kubeconfig)
	if err != nil {
		t.Fatalf("read the node architecture: %v", err)
	}
	if want := strings.TrimPrefix(buildpacks.BuilderPlatform, "linux/"); arch != want {
		t.Skipf("the cluster's nodes are %s; the Google builder produces %s images only, which cannot run here", arch, want)
	}
}

// TestFunctionsAPIBuildsAndServes (#1377), gated as the Functions tests are:
// the testdata/function fixture, zipped and PUT to an upload URL, is
// created as a go123 function with entry point Hello; it becomes ACTIVE with
// its url and service_config.service, the Cloud Run service, which answers
// the greeting through the ingress; an update of service_config's
// environment rolls out a new revision without a rebuild, keeping the
// build; delete removes the service too.
func TestFunctionsAPIBuildsAndServes(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	requireAmd64Functions(ctx, t)
	parent := runParent(h)
	fc := functionsClient(t, h)
	rc := runClient(t, h)

	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage) // the upload URL is a bucket's
	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), zipFunction(t, filepath.Join(moduleRoot(t), "testdata", "function")))
	name := parent + "/functions/sdk-fn-built"
	op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "sdk-fn-built", Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}},
		ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: "256M"},
	}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := fc.DeleteFunction(dctx, &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	fn, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFunction.Wait: %v", err)
	}
	if fn.GetState() != functionspb.Function_ACTIVE || fn.GetUrl() == "" ||
		fn.GetServiceConfig().GetService() != parent+"/services/sdk-fn-built" || fn.GetBuildConfig().GetBuild() == "" {
		t.Fatalf("the created function = %v; want it ACTIVE with its url, service and build", fn)
	}
	code, body := functionRequest(t, ingress(t), hostOf(t, fn.GetUrl()), `{"name":"Functions"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello Functions"`) {
		t.Errorf("POST / = %d %q; want 200 and the greeting", code, body)
	}

	uop, err := fc.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{
		Function:   &functionspb.Function{Name: name, ServiceConfig: &functionspb.ServiceConfig{EnvironmentVariables: map[string]string{"MODE": "updated"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"service_config.environment_variables"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	upd, err := uop.Wait(ctx)
	if err != nil || upd.GetBuildConfig().GetBuild() != fn.GetBuildConfig().GetBuild() || upd.GetServiceConfig().GetRevision() == fn.GetServiceConfig().GetRevision() {
		t.Errorf("UpdateFunction of the environment = %v, %v; want a new revision of the same build", upd, err)
	}

	dop, err := fc.DeleteFunction(ctx, &functionspb.DeleteFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Errorf("DeleteFunction.Wait: %v", err)
	}
	if _, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: fn.GetServiceConfig().GetService()}); status.Code(err) != codes.NotFound {
		t.Errorf("the function's service after delete = %v; want NOT_FOUND", err)
	}
}

// TestGcloudFunctions (#1377): the pinned gcloud, through gcloud-setup's
// cloudfunctions override alone, egress to Google blocked. Ungated:
// `functions list --v2`, `describe` and `delete` of a 2nd-gen function created
// through the client (FAILED, its source not a zip). Gated as the Functions
// tests are: `functions deploy --gen2` of testdata/function, which uploads
// the source to a GenerateUploadUrl URL and polls the operation, and the
// deployed function's url.
func TestGcloudFunctions(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	g := newGcloudSession(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	fc := functionsClient(t, h)
	// gcloud names the instance's project, which is what it lists.
	parent := "projects/" + g.project + "/locations/us-central1"

	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("not a zip"))
	op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "gcloud-fn", Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}}}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	_, _ = op.Wait(ctx)

	if out := g.must("functions", "list", "--v2", "--regions=us-central1", "--format=value(name,state)"); !strings.Contains(out, "gcloud-fn") || !strings.Contains(out, "FAILED") {
		t.Errorf("gcloud functions list = %q; want gcloud-fn FAILED", out)
	}
	if out := g.must("functions", "describe", "gcloud-fn", "--gen2", "--region=us-central1", "--format=value(buildConfig.runtime,buildConfig.entryPoint,state)"); !strings.Contains(out, "go123") || !strings.Contains(out, "Hello") {
		t.Errorf("gcloud functions describe = %q", out)
	}
	g.must("functions", "delete", "gcloud-fn", "--gen2", "--region=us-central1", "--quiet")
	if _, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: parent + "/functions/gcloud-fn"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetFunction after gcloud functions delete = %v; want NOT_FOUND", err)
	}

	// A deploy whose runtime is not served: gcloud reads ListRuntimes from
	// this instance and refuses it before uploading anything.
	src := filepath.Join(moduleRoot(t), "testdata", "function")
	out, err := g.run(nil, "functions", "deploy", "gcloud-fn-bad", "--gen2", "--region=us-central1", "--runtime=go999",
		"--entry-point=Hello", "--trigger-http", "--source="+src)
	t.Logf("gcloud functions deploy --runtime=go999:\n%s", out)
	if err == nil || !strings.Contains(out, "go999 is not a supported runtime") {
		t.Errorf("gcloud functions deploy --runtime=go999 = %v:\n%s\nwant the runtime refused", err, out)
	}

	requireAmd64Functions(ctx, t)
	out, err = g.run(nil, "functions", "deploy", "gcloud-fn-built", "--gen2", "--region=us-central1", "--runtime=go123",
		"--entry-point=Hello", "--trigger-http", "--source="+src, "--memory=256Mi", "--set-env-vars=MODE=gcloud")
	t.Logf("gcloud functions deploy:\n%s", out)
	if err != nil {
		t.Fatalf("gcloud functions deploy --gen2: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.run(nil, "functions", "delete", "gcloud-fn-built", "--gen2", "--region=us-central1", "--quiet")
	})
	desc := g.must("functions", "describe", "gcloud-fn-built", "--gen2", "--region=us-central1", "--format=value(state,url)")
	if f := strings.Fields(desc); len(f) != 2 || f[0] != "ACTIVE" {
		t.Fatalf("gcloud functions describe after deploy = %q; want ACTIVE and its url", desc)
	}
	code, body := functionRequest(t, ingress(t), hostOf(t, strings.Fields(desc)[1]), `{"name":"gcloud"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello gcloud"`) {
		t.Errorf("POST / = %d %q; want 200 and the greeting", code, body)
	}
	g.must("functions", "delete", "gcloud-fn-built", "--gen2", "--region=us-central1", "--quiet")
}

// TestTerraformCloudFunctions2Function (#1377): google_cloudfunctions2_function
// through `cloudburrow terraform`, which points cloudfunctions2_custom_endpoint
// at the Run port, egress to Google blocked. Ungated: a function whose
// source object is not a zip fails to apply with this instance's build
// error, and its IAM member is never reached. Gated as the Functions tests
// are: testdata/function, zipped into a bucket object, applies ACTIVE with
// its url and service, plans clean, updates its service_config in place and
// destroys.
func TestTerraformCloudFunctions2Function(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	fc := functionsClient(t, h)
	suffix := strings.TrimPrefix(m.project, "cb-test-")
	bucketName := "tf-fn-src-" + strings.ToLower(strings.ReplaceAll(suffix, "_", "-"))
	zipPath := filepath.Join(t.TempDir(), "function.zip")
	module := func(id, archive, mode string) string {
		if err := os.WriteFile(zipPath, []byte(archive), 0o644); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`
resource "google_storage_bucket" "src" {
  project       = %[1]q
  name          = %[2]q
  location      = "US"
  force_destroy = true
}
resource "google_storage_bucket_object" "zip" {
  name   = "function.zip"
  bucket = google_storage_bucket.src.name
  source = %[3]q
}
resource "google_cloudfunctions2_function" "f" {
  project  = %[1]q
  name     = %[4]q
  location = "us-central1"
  build_config {
    runtime     = "go123"
    entry_point = "Hello"
    source {
      storage_source {
        bucket = google_storage_bucket.src.name
        object = google_storage_bucket_object.zip.name
      }
    }
  }
  service_config {
    available_memory      = "256M"
    timeout_seconds       = 60
    max_instance_count    = 2
    environment_variables = { MODE = %[5]q }
  }
}
`, m.project, bucketName, zipPath, id, mode)
	}

	m.write(module("tf-fn-bad", "not a zip", "bad"))
	m.must("init", "-input=false", "-no-color")
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil || !strings.Contains(out, "Build failed") {
		t.Errorf("apply of a source that is not a zip = %v; want this instance's build error:\n%s", err, lastLines(out, 30))
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: "projects/" + m.project + "/locations/us-central1/functions/tf-fn-bad"}); status.Code(err) != codes.NotFound {
		t.Errorf("the failed function after destroy = %v; want NOT_FOUND", err)
	}

	requireAmd64Functions(ctx, t)
	archive := string(zipFunction(t, filepath.Join(moduleRoot(t), "testdata", "function")))
	name := "projects/" + m.project + "/locations/us-central1/functions/tf-fn"
	m.write(module("tf-fn", archive, "first"))
	m.apply()
	fn, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
	if err != nil || fn.GetState() != functionspb.Function_ACTIVE || fn.GetUrl() == "" || fn.GetServiceConfig().GetService() == "" {
		t.Fatalf("after apply, the function = %v, %v; want it ACTIVE with its url and service", fn, err)
	}
	m.planClean()
	m.write(module("tf-fn", archive, "second"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	if fn, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); err != nil || fn.GetServiceConfig().GetEnvironmentVariables()["MODE"] != "second" {
		t.Errorf("after the change, the function = %v, %v", fn, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetFunction after destroy = %v; want NOT_FOUND", err)
	}
}
