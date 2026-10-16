//go:build compat

package compat

import (
	"archive/zip"
	"bytes"
	"context"
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
// enforced; delete removes it. An event trigger, a second create and a
// missing function are refused.
func TestFunctionsAPI(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(h.Context(), 3*time.Minute)
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
		EventTrigger: &functionspb.EventTrigger{EventType: "google.cloud.pubsub.topic.v1.messagePublished", PubsubTopic: "projects/" + h.Project() + "/topics/t"}}
	if _, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "sdk-fn-trigger", Function: trig}); status.Code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "Eventarc") {
		t.Errorf("CreateFunction with an event trigger = %v; want UNIMPLEMENTED naming Eventarc", err)
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
	ctx, cancel := context.WithTimeout(h.Context(), 30*time.Minute)
	t.Cleanup(cancel)
	requireAmd64Functions(ctx, t)
	parent := runParent(h)
	fc := functionsClient(t, h)
	rc := runClient(t, h)

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
