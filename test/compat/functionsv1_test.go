//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fv1client "cloud.google.com/go/functions/apiv1"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func functionsV1Client(t *testing.T, h *Harness) *fv1client.CloudFunctionsClient {
	t.Helper()
	c, err := fv1client.NewCloudFunctionsClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatalf("functions.NewCloudFunctionsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.cloud.functions.v1.CloudFunctionsService/GenerateUploadUrl, google.cloud.functions.v1.CloudFunctionsService/GenerateDownloadUrl, google.cloud.functions.v1.CloudFunctionsService/CreateFunction, google.cloud.functions.v1.CloudFunctionsService/GetFunction, google.cloud.functions.v1.CloudFunctionsService/ListFunctions, google.cloud.functions.v1.CloudFunctionsService/UpdateFunction, google.cloud.functions.v1.CloudFunctionsService/DeleteFunction, google.cloud.functions.v1.CloudFunctionsService/CallFunction, google.cloud.functions.v1.CloudFunctionsService/SetIamPolicy, google.cloud.functions.v1.CloudFunctionsService/GetIamPolicy, google.cloud.functions.v1.CloudFunctionsService/TestIamPermissions
//
// TestFunctionsV1API (#1428): the official Cloud Functions v1 client, over
// gRPC on the Run port, through everything but a build that succeeds, so
// it runs on any instance with --services run,storage. A source PUT to a v1
// upload URL is read back through GenerateDownloadUrl; a 1st-gen HTTP
// function built from it (not a zip) is DEPLOY_IN_PROGRESS, its v1
// operation fails with the build's error, and it is OFFLINE, with its
// httpsTrigger url and Google's 1st-gen defaults; the v2 API reads it as
// environment GEN_1 and refuses to update it; v1 list finds it and
// operations.list its operations; a description update completes at once;
// its IAM policy is stored; call of a function that is not ACTIVE, a
// sourceRepository, a legacy object.change trigger and a v2 function read
// through v1 are refused; delete removes it.
func TestFunctionsV1API(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	c := functionsV1Client(t, h)
	fc := functionsClient(t, h)

	locs := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + h.Project()})
	if l, err := locs.Next(); err != nil || !strings.HasPrefix(l.GetName(), "projects/"+h.Project()+"/locations/") {
		t.Errorf("ListLocations = %v, %v", l, err)
	}
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage) // the upload URL is a bucket's
	up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
	if err != nil || up.GetUploadUrl() == "" {
		t.Fatalf("GenerateUploadUrl = %v, %v", up, err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("this is not a zip archive"))

	name := parent + "/functions/sdk-v1-fn"
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := c.DeleteFunction(dctx, &fv1.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{
		Name: name, Description: "v1", Runtime: "go123", EntryPoint: "Hello",
		SourceCode:           &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
		Trigger:              &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{SecurityLevel: fv1.HttpsTrigger_SECURE_ALWAYS}},
		AvailableMemoryMb:    512,
		Timeout:              durationpb.New(30 * time.Second),
		EnvironmentVariables: map[string]string{"GREETING": "hi"},
		Labels:               map[string]string{"team": "a"},
	}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	if !strings.HasPrefix(op.Name(), "operations/") {
		t.Errorf("the v1 operation is named %q; want operations/{id}", op.Name())
	}
	if _, err := op.Wait(ctx); err == nil || !strings.Contains(err.Error(), "Build failed") {
		t.Errorf("CreateFunction.Wait of a source that is not a zip = %v; want the build's error", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetType() != fv1.OperationType_CREATE_FUNCTION || md.GetTarget() != name {
		t.Errorf("the operation's metadata = %v, %v; want CREATE_FUNCTION of %s", md, err, name)
	}
	got, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetFunction: %v", err)
	}
	if got.GetStatus() != fv1.CloudFunctionStatus_OFFLINE || got.GetRuntime() != "go123" || got.GetEntryPoint() != "Hello" ||
		got.GetAvailableMemoryMb() != 512 || got.GetTimeout().AsDuration() != 30*time.Second ||
		got.GetServiceAccountEmail() != h.Project()+"@appspot.gserviceaccount.com" ||
		got.GetHttpsTrigger().GetUrl() != "https://us-central1-"+h.Project()+".cloudfunctions.net/sdk-v1-fn" ||
		got.GetHttpsTrigger().GetSecurityLevel() != fv1.HttpsTrigger_SECURE_ALWAYS ||
		got.GetSourceUploadUrl() != up.GetUploadUrl() || got.GetVersionId() != 1 || got.GetLabels()["team"] != "a" {
		t.Errorf("GetFunction = %v; want it OFFLINE with what was created and 1st gen's defaults", got)
	}
	if !strings.HasPrefix(parent, "projects/"+h.Project()+"/locations/us-central1") {
		t.Logf("parent %s is not us-central1; the url check above names us-central1", parent)
	}

	// Through v2, the same function is a GEN_1 one.
	v2fn, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
	if err != nil || v2fn.GetEnvironment() != functionspb.Environment_GEN_1 || v2fn.GetState() != functionspb.Function_FAILED {
		t.Errorf("v2 GetFunction = %v, %v; want it GEN_1 and FAILED", v2fn, err)
	}
	if _, err := fc.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{Function: &functionspb.Function{Name: name, Description: "x"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("v2 UpdateFunction of a 1st gen function = %v; want FAILED_PRECONDITION", err)
	}

	found := false
	it := c.ListFunctions(ctx, &fv1.ListFunctionsRequest{Parent: parent})
	for {
		fn, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFunctions: %v", err)
		}
		found = found || fn.GetName() == name
	}
	if !found {
		t.Errorf("ListFunctions did not list %s", name)
	}
	ops := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: "operations", Filter: "project:" + h.Project() + ",latest:true"})
	if o, err := ops.Next(); err != nil || !strings.HasPrefix(o.GetName(), "operations/") {
		t.Errorf("ListOperations = %v, %v; want the function's operation", o, err)
	}

	dl, err := c.GenerateDownloadUrl(ctx, &fv1.GenerateDownloadUrlRequest{Name: name})
	if err != nil {
		t.Fatalf("GenerateDownloadUrl: %v", err)
	}
	resp, err := http.Get(dl.GetDownloadUrl())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("GET the download URL = %v, %v", resp, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	uop, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: &fv1.CloudFunction{Name: name, Description: "updated"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	if upd, err := uop.Wait(ctx); err != nil || upd.GetDescription() != "updated" || upd.GetVersionId() != 2 || upd.GetRuntime() != "go123" {
		t.Errorf("UpdateFunction of the description = %v, %v; want it updated, version 2, the rest kept", upd, err)
	}
	if _, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: &fv1.CloudFunction{Name: name,
		Trigger: &fv1.CloudFunction_EventTrigger{EventTrigger: &fv1.EventTrigger{EventType: "google.pubsub.topic.publish", Resource: "projects/" + h.Project() + "/topics/t"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"eventTrigger"}}}); err == nil {
		t.Errorf("UpdateFunction from an HTTP trigger to an event trigger succeeded; want it refused")
	}

	member := "user:dev@example.com"
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/cloudfunctions.invoker", Members: []string{member}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); err != nil || len(p.GetBindings()) != 1 || p.GetBindings()[0].GetMembers()[0] != member {
		t.Errorf("GetIamPolicy = %v, %v", p, err)
	}
	if p, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: []string{"cloudfunctions.functions.invoke"}}); err != nil || len(p.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", p, err)
	}

	if _, err := c.CallFunction(ctx, &fv1.CallFunctionRequest{Name: name, Data: `{"name":"x"}`}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CallFunction of an OFFLINE function = %v; want FAILED_PRECONDITION", err)
	}
	for _, bad := range []*fv1.CloudFunction{
		{Name: parent + "/functions/sdk-v1-repo", Runtime: "go123",
			SourceCode: &fv1.CloudFunction_SourceRepository{SourceRepository: &fv1.SourceRepository{Url: "https://source.developers.google.com/projects/p/repos/r"}},
			Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}},
		{Name: parent + "/functions/sdk-v1-change", Runtime: "go123",
			SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
			Trigger:    &fv1.CloudFunction_EventTrigger{EventTrigger: &fv1.EventTrigger{EventType: "providers/cloud.storage/eventTypes/object.change", Resource: "projects/_/buckets/b"}}},
	} {
		if _, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: bad}); status.Code(err) != codes.Unimplemented {
			t.Errorf("CreateFunction %s = %v; want UNIMPLEMENTED", bad.GetName(), err)
		}
	}
	if _, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{Name: name, Runtime: "go123",
		SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
		Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateFunction = %v; want ALREADY_EXISTS", err)
	}

	// A 2nd-gen function is the v2 API's.
	v2name := parent + "/functions/sdk-v1-gen2"
	v2up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	putSource(ctx, t, v2up.GetUploadUrl(), []byte("not a zip"))
	if op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "sdk-v1-gen2", Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: v2up.GetStorageSource()}}}}}); err == nil {
		_, _ = op.Wait(ctx)
		if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: v2name}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("v1 GetFunction of a 2nd gen function = %v; want FAILED_PRECONDITION", err)
		}
		if dop, err := fc.DeleteFunction(ctx, &functionspb.DeleteFunctionRequest{Name: v2name}); err == nil {
			_ = dop.Wait(ctx)
		}
	} else {
		t.Errorf("v2 CreateFunction: %v", err)
	}

	dop, err := c.DeleteFunction(ctx, &fv1.DeleteFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Errorf("DeleteFunction.Wait: %v", err)
	}
	if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetFunction after delete = %v; want NOT_FOUND", err)
	}
}

// covers: google.cloud.functions.v1.CloudFunctionsService/CreateFunction, google.cloud.functions.v1.CloudFunctionsService/CallFunction, google.cloud.functions.v1.CloudFunctionsService/UpdateFunction, google.cloud.functions.v1.CloudFunctionsService/DeleteFunction
//
// TestFunctionsV1BuildsAndServes (#1428), gated as the Functions tests are:
// testdata/function, zipped and PUT to a v1 upload URL, is created as a
// 1st-gen HTTP function with entry point Hello; it becomes ACTIVE, the v2
// API reads it as GEN_1 with its Cloud Run service, call answers its
// greeting, an environment update rolls out without a rebuild, and delete
// removes it.
func TestFunctionsV1BuildsAndServes(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	requireAmd64Functions(ctx, t)
	parent := runParent(h)
	c := functionsV1Client(t, h)
	fc := functionsClient(t, h)

	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage) // the upload URL is a bucket's
	up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), zipFunction(t, filepath.Join(moduleRoot(t), "testdata", "function")))
	name := parent + "/functions/sdk-v1-built"
	op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{
		Name: name, Runtime: "go123", EntryPoint: "Hello",
		SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
		Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := c.DeleteFunction(dctx, &fv1.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	fn, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFunction.Wait: %v", err)
	}
	if fn.GetStatus() != fv1.CloudFunctionStatus_ACTIVE || fn.GetBuildId() == "" || fn.GetHttpsTrigger().GetUrl() == "" {
		t.Fatalf("the created function = %v; want it ACTIVE with its build and url", fn)
	}
	v2fn, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
	if err != nil || v2fn.GetEnvironment() != functionspb.Environment_GEN_1 || v2fn.GetServiceConfig().GetService() != parent+"/services/sdk-v1-built" {
		t.Errorf("v2 GetFunction = %v, %v; want GEN_1 with its service", v2fn, err)
	}
	call, err := c.CallFunction(ctx, &fv1.CallFunctionRequest{Name: name, Data: `{"name":"v1"}`})
	if err != nil || !strings.Contains(call.GetResult(), `"greeting":"hello v1"`) || call.GetExecutionId() == "" {
		t.Errorf("CallFunction = %v, %v; want the greeting", call, err)
	}
	uop, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: &fv1.CloudFunction{Name: name, EnvironmentVariables: map[string]string{"MODE": "updated"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"environmentVariables"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	if upd, err := uop.Wait(ctx); err != nil || upd.GetBuildId() != fn.GetBuildId() || upd.GetStatus() != fv1.CloudFunctionStatus_ACTIVE {
		t.Errorf("UpdateFunction of the environment = %v, %v; want it ACTIVE on the same build", upd, err)
	}
	dop, err := c.DeleteFunction(ctx, &fv1.DeleteFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Errorf("DeleteFunction.Wait: %v", err)
	}
}

// covers: google.cloud.functions.v1.CloudFunctionsService/CreateFunction, google.cloud.functions.v1.CloudFunctionsService/GetFunction, google.cloud.functions.v1.CloudFunctionsService/ListFunctions, google.cloud.functions.v1.CloudFunctionsService/DeleteFunction, google.cloud.functions.v1.CloudFunctionsService/GenerateUploadUrl
//
// TestGcloudFunctionsV1 (#1428): the pinned gcloud, through gcloud-setup's
// overrides alone, egress to Google blocked. Ungated: `functions deploy
// --no-gen2` with --trigger-http, and with --trigger-topic, of a gs://
// source that is not a zip, which fails with this instance's build error;
// `describe --no-gen2` shows the v1 trigger gcloud sent and OFFLINE;
// `list` shows them as 1st gen; `delete --no-gen2` removes them. Gated as
// the Functions tests are: `deploy --no-gen2 --trigger-http` of
// testdata/function's Hello becomes ACTIVE, and `call` answers its greeting.
func TestGcloudFunctionsV1(t *testing.T) {
	h := New(t)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	g := newGcloudSession(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, g.project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	c := functionsV1Client(t, h)
	parent := "projects/" + g.project + "/locations/us-central1"

	topicID := "gcloud-v1-events"
	tn := "projects/" + g.project + "/topics/" + topicID
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tn}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: tn})
	})
	bh := bucket(t, h, sc)
	w := bh.Object("src/not-a-zip.zip").NewWriter(ctx)
	_, _ = w.Write([]byte("not a zip"))
	if err := w.Close(); err != nil {
		t.Fatalf("upload the source: %v", err)
	}
	deploy := func(id, source string, trigger ...string) (string, error) {
		args := append([]string{"functions", "deploy", id, "--no-gen2", "--region=us-central1", "--runtime=go123",
			"--entry-point=Hello", "--source=" + source, "--memory=256MB"}, trigger...)
		out, err := g.run(nil, args...)
		t.Logf("gcloud %s:\n%s", strings.Join(args, " "), out)
		return out, err
	}
	describe := func(id string) map[string]any {
		t.Helper()
		cmd := exec.Command(g.bin, "functions", "describe", id, "--no-gen2", "--region=us-central1", "--format=json")
		cmd.Env = g.env
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gcloud functions describe %s: %v\n%s", id, err, stderr.String())
		}
		var d map[string]any
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatalf("gcloud functions describe %s: %v\n%s", id, err, out)
		}
		return d
	}

	bad := "gs://" + bh.BucketName() + "/src/not-a-zip.zip"
	for _, cs := range []struct {
		id      string
		trigger []string
		check   func(map[string]any) bool
	}{
		{"gcloud-v1-http-bad", []string{"--trigger-http"}, func(d map[string]any) bool {
			ht, _ := d["httpsTrigger"].(map[string]any)
			return ht != nil && strings.HasSuffix(ht["url"].(string), "/gcloud-v1-http-bad")
		}},
		{"gcloud-v1-topic-bad", []string{"--trigger-topic=" + topicID}, func(d map[string]any) bool {
			et, _ := d["eventTrigger"].(map[string]any)
			return et != nil && et["resource"] == tn && strings.Contains(et["eventType"].(string), "topic.publish")
		}},
	} {
		t.Cleanup(func() {
			_, _ = g.run(nil, "functions", "delete", cs.id, "--no-gen2", "--region=us-central1", "--quiet")
		})
		out, err := deploy(cs.id, bad, cs.trigger...)
		if err == nil || !strings.Contains(out, "Build failed") {
			t.Errorf("gcloud functions deploy --no-gen2 %s of a source that is not a zip = %v; want this instance's build error:\n%s", cs.id, err, out)
		}
		d := describe(cs.id)
		if d["status"] != "OFFLINE" || d["runtime"] != "go123" || !cs.check(d) {
			t.Errorf("gcloud functions describe --no-gen2 %s = %v; want it OFFLINE with its trigger", cs.id, d)
		}
	}
	// gcloud functions list reads 2nd gen through v2 (environment="GEN_2")
	// and 1st gen through v1; the v2 API reads a 1st-gen one as GEN_1.
	if out := g.must("functions", "list", "--regions=us-central1", "--format=value(name)"); !strings.Contains(out, "gcloud-v1-http-bad") || !strings.Contains(out, "gcloud-v1-topic-bad") {
		t.Errorf("gcloud functions list = %q; want both 1st-gen functions", out)
	}
	if fn, err := functionsClient(t, h).GetFunction(ctx, &functionspb.GetFunctionRequest{Name: parent + "/functions/gcloud-v1-topic-bad"}); err != nil ||
		fn.GetEnvironment() != functionspb.Environment_GEN_1 || fn.GetEventTrigger().GetPubsubTopic() != tn {
		t.Errorf("v2 GetFunction of a function gcloud deployed --no-gen2 = %v, %v; want GEN_1 with its topic", fn, err)
	}
	for _, id := range []string{"gcloud-v1-http-bad", "gcloud-v1-topic-bad"} {
		g.must("functions", "delete", id, "--no-gen2", "--region=us-central1", "--quiet")
		if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: parent + "/functions/" + id}); status.Code(err) != codes.NotFound {
			t.Errorf("GetFunction after gcloud functions delete = %v; want NOT_FOUND", err)
		}
	}

	requireAmd64Functions(ctx, t)
	src := filepath.Join(moduleRoot(t), "testdata", "function")
	if _, err := deploy("gcloud-v1-built", src, "--trigger-http", "--allow-unauthenticated"); err != nil {
		t.Fatalf("gcloud functions deploy --no-gen2: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.run(nil, "functions", "delete", "gcloud-v1-built", "--no-gen2", "--region=us-central1", "--quiet")
	})
	if d := describe("gcloud-v1-built"); d["status"] != "ACTIVE" {
		t.Fatalf("gcloud functions describe after deploy = %v; want ACTIVE", d)
	}
	if out := g.must("functions", "call", "gcloud-v1-built", "--no-gen2", "--region=us-central1", `--data={"name":"gcloud"}`); !strings.Contains(out, "hello gcloud") {
		t.Errorf("gcloud functions call = %q; want the greeting", out)
	}
	g.must("functions", "delete", "gcloud-v1-built", "--no-gen2", "--region=us-central1", "--quiet")
}
