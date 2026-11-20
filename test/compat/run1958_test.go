//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.run.v2.Instances/UpdateInstance, google.cloud.run.v2.Instances/GetIamPolicy, google.cloud.run.v2.Instances/SetIamPolicy, google.cloud.run.v2.Instances/TestIamPermissions
//
// TestRunUpdateInstance (#1958): through the official InstancesClient, an
// instance's update_mask names the fields taken from the request; the
// change is a new generation that rolls the instance's container, whose URL
// then serves it; validate_only changes nothing; a stale etag is ABORTED;
// an output-only path is INVALID_ARGUMENT; a missing instance is NOT_FOUND,
// and with allow_missing is created. The v2 IAM methods store and read
// back the instance's policy.
func TestRunUpdateInstance(t *testing.T) {
	h := New(t)
	ctx := runWait(t)
	c, err := run.NewInstancesClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewInstancesClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	parent := runParent(h)
	id := "compat-update-instance"
	name := parent + "/instances/" + id
	missingName := parent + "/instances/compat-update-missing"
	for _, n := range []string{name, missingName} {
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &runpb.DeleteInstanceRequest{Name: n}); err == nil {
				_, _ = waitLRO(context.Background(), op)
			}
		})
	}
	hello := func(target string) *runpb.Container {
		return &runpb.Container{Image: "ghcr.io/knative/helloworld-go:latest",
			Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: target}}}}
	}
	op, err := c.CreateInstance(ctx, &runpb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &runpb.Instance{Description: "before", Labels: map[string]string{"team": "a"}, Containers: []*runpb.Container{hello("one")}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	created, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("waiting for CreateInstance: %v", err)
	}

	// validate_only changes nothing.
	vop, err := c.UpdateInstance(ctx, &runpb.UpdateInstanceRequest{ValidateOnly: true,
		Instance:   &runpb.Instance{Name: name, Description: "validated"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatalf("UpdateInstance validate_only: %v", err)
	}
	if got, err := waitLRO(ctx, vop); err != nil || got.GetDescription() != "validated" {
		t.Errorf("validate_only's operation = %v, %v; want the instance as it would be", got, err)
	}
	if got, _ := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}); got.GetDescription() != "before" || got.GetGeneration() != 1 {
		t.Errorf("after validate_only, the instance = %v; want it unchanged", got)
	}
	if _, err := c.UpdateInstance(ctx, &runpb.UpdateInstanceRequest{Instance: &runpb.Instance{Name: name, Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateInstance with a stale etag = %v; want ABORTED", err)
	}
	if _, err := c.UpdateInstance(ctx, &runpb.UpdateInstanceRequest{Instance: &runpb.Instance{Name: name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"uid"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateInstance of uid = %v; want INVALID_ARGUMENT", err)
	}

	// The mask's fields change; labels, which it leaves out, are kept.
	uop, err := c.UpdateInstance(ctx, &runpb.UpdateInstanceRequest{
		Instance:   &runpb.Instance{Name: name, Etag: created.GetEtag(), Description: "after", Containers: []*runpb.Container{hello("two")}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "containers"}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	updated, err := waitLRO(ctx, uop)
	if err != nil {
		t.Fatalf("waiting for UpdateInstance: %v", err)
	}
	if updated.GetGeneration() != 2 || updated.GetDescription() != "after" || updated.GetLabels()["team"] != "a" ||
		updated.GetContainers()[0].GetEnv()[0].GetValue() != "two" || updated.GetUid() != created.GetUid() {
		t.Errorf("updated = %v; want generation 2 with the new description and env, the labels kept", updated)
	}
	if st := updated.GetTerminalCondition().GetState(); st != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("UpdateInstance's operation completed with the instance %v; want ready", st)
	}
	deadline := time.Now().Add(2 * time.Minute)
	body, code := "", 0
	for time.Now().Before(deadline) {
		if body, code = secretEnvGet(t, ingress(t), hostOf(t, updated.GetUrls()[0])); code == http.StatusOK && strings.Contains(body, "Hello two") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if code != http.StatusOK || !strings.Contains(body, "Hello two") {
		t.Errorf("GET %s after the update = %d %q; want Hello two from the rolled container", updated.GetUrls()[0], code, body)
	}

	// A missing instance.
	if _, err := c.UpdateInstance(ctx, &runpb.UpdateInstanceRequest{Instance: &runpb.Instance{Name: missingName,
		Containers: []*runpb.Container{hello("x")}}}); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateInstance of a missing instance = %v; want NOT_FOUND", err)
	}
	aop, err := c.UpdateInstance(ctx, &runpb.UpdateInstanceRequest{AllowMissing: true,
		Instance: &runpb.Instance{Name: missingName, Containers: []*runpb.Container{hello("made")}}})
	if err != nil {
		t.Fatalf("UpdateInstance allow_missing: %v", err)
	}
	if made, err := waitLRO(ctx, aop); err != nil || made.GetName() != missingName || made.GetGeneration() != 1 {
		t.Errorf("allow_missing made %v, %v; want the instance created", made, err)
	}

	// IAM on the v2 Instances service.
	member := "user:dev@example.com"
	pol, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/run.invoker", Members: []string{member}}}}})
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != member || string(got.GetEtag()) != string(pol.GetEtag()) {
		t.Errorf("GetIamPolicy = %v, %v; want the binding set", got, err)
	}
	perms, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: []string{"run.instances.get"}})
	if err != nil || len(perms.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", perms, err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent + "/instances/no-such"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy of a missing instance = %v; want NOT_FOUND", err)
	}
}

// uploadSourceMultipart POSTs an archive to UploadSource's media path as
// gcloud's generated client sends it: uploadType=multipart, the request's
// JSON and then the archive.
func uploadSourceMultipart(t *testing.T, h *Harness, meta string, archive []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	jh := textproto.MIMEHeader{}
	jh.Set("Content-Type", "application/json")
	jp, _ := mw.CreatePart(jh)
	_, _ = jp.Write([]byte(meta))
	ah := textproto.MIMEHeader{}
	ah.Set("Content-Type", "application/gzip")
	ap, _ := mw.CreatePart(ah)
	_, _ = ap.Write(archive)
	_ = mw.Close()
	u := "http://" + h.Endpoint(EnvRun) + "/upload/v2/" + runParent(h) + ":uploadSource?uploadType=multipart&alt=json"
	req, err := http.NewRequest(http.MethodPost, u, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// covers: google.cloud.run.v2.SourceUpload/UploadSource
//
// TestRunUploadSource (#1958): SourceUpload.UploadSource through the
// official SourceUploadClient carries no archive over gRPC and is
// INVALID_ARGUMENT, naming the media path; the media upload gcloud makes
// (uploadType=multipart) is written to run-sources-{project}-us-central1
// under services/{service}/, as the response's cloudStorageSource names
// it, and the official Storage client reads the archive back.
func TestRunUploadSource(t *testing.T) {
	h := New(t)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	ctx := runWait(t)
	uc, err := run.NewSourceUploadClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewSourceUploadClient: %v", err)
	}
	t.Cleanup(func() { _ = uc.Close() })
	if _, err := uc.UploadSource(ctx, &runpb.UploadSourceRequest{Parent: runParent(h),
		Target: &runpb.UploadSourceRequest_Service{Service: "svc"}}); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), ":uploadSource") {
		t.Errorf("UploadSource with no media = %v; want INVALID_ARGUMENT naming the upload path", err)
	}
	archive := []byte("\x1f\x8b\x08\x00compat-archive")
	code, body := uploadSourceMultipart(t, h, `{"service":"upload-svc"}`, archive)
	if code != http.StatusOK {
		t.Fatalf("the media upload = %d %s", code, body)
	}
	var resp struct {
		CloudStorageSource struct{ Bucket, Object string } `json:"cloudStorageSource"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("the upload's response %s: %v", body, err)
	}
	cs := resp.CloudStorageSource
	if cs.Bucket != "run-sources-"+h.Project()+"-us-central1" || !strings.HasPrefix(cs.Object, "services/upload-svc/") ||
		!strings.HasSuffix(cs.Object, ".tar.gz") {
		t.Errorf("cloudStorageSource = %+v; want services/upload-svc/... in run-sources-%s-us-central1", cs, h.Project())
	}
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	t.Cleanup(func() { emptyAndDelete(context.Background(), sc.Bucket(cs.Bucket)) })
	r, err := sc.Bucket(cs.Bucket).Object(cs.Object).NewReader(ctx)
	if err != nil {
		t.Fatalf("read gs://%s/%s: %v", cs.Bucket, cs.Object, err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, archive) {
		t.Errorf("the stored archive = %q; want %q", got, archive)
	}
	if code, body := uploadSourceMultipart(t, h, `{"service":"a","job":"b"}`, archive); code != http.StatusBadRequest {
		t.Errorf("an upload naming a service and a job = %d %s; want 400", code, body)
	}
}

// covers: google.cloud.run.v2.SourceUpload/UploadSource
//
// TestGcloudBetaRunDeployRunUpload (#1958): `gcloud beta run deploy
// --source --run-upload`, through gcloud-setup's configuration alone,
// egress blocked: gcloud uploads the source through the Cloud Run API's
// UploadSource (the media path) instead of writing the bucket itself; the
// archive is in run-sources-{project}-us-central1 under
// services/{service}/, and the build and deploy then run as
// TestGcloudRunDeploySource's do.
func TestGcloudBetaRunDeployRunUpload(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: the build runs with the host's Docker")
	}
	g := newGcloudSession(t, h)
	id := "gcloud-run-upload"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	src := sourceBuildDir(t)
	out, err := g.run(nil, "beta", "run", "deploy", id, "--source="+src, "--run-upload", region, "--no-allow-unauthenticated", "--quiet")
	t.Logf("gcloud beta run deploy --source --run-upload:\n%s", out)
	if err != nil {
		t.Fatalf("gcloud beta run deploy --run-upload: %v", err)
	}
	ctx := runWait(t)
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	bucketName := "run-sources-" + g.project + "-us-central1"
	it := sc.Bucket(bucketName).Objects(ctx, &storage.Query{Prefix: "services/" + id + "/"})
	n := 0
	for {
		o, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("list gs://%s: %v", bucketName, err)
		}
		if filepath.Ext(o.Name) == ".zip" || strings.HasSuffix(o.Name, ".tar.gz") {
			n++
		}
	}
	if n == 0 {
		t.Errorf("no archive under gs://%s/services/%s/; want the source gcloud uploaded through UploadSource", bucketName, id)
	}
	url := strings.TrimSpace(g.must("run", "services", "describe", id, region, "--format=value(status.url)"))
	if code, got := functionRequest(t, ingress(t), hostOf(t, url), "", nil); code != http.StatusOK ||
		got != "built from source by SubmitBuild" {
		t.Errorf("the deployed service answered %d %q", code, got)
	}
}

// covers: google.cloud.functions.v1.CloudFunctionsService/GetFunction, google.cloud.functions.v1.CloudFunctionsService/GenerateDownloadUrl, google.cloud.functions.v2.FunctionService/GetFunction, google.cloud.functions.v2.FunctionService/ListFunctions
//
// TestV1FunctionVersionsAndV2ListPaging (#1958): through the official v1 and
// v2 clients, a 1st-gen function's update makes version 2 and keeps version
// 1, which v1 GetFunction's version_id and v2 GetFunction's revision read
// back as it stood, and v1 GenerateDownloadUrl's version_id names that
// version's source; an unknown version is NOT_FOUND, and a revision of a
// 2nd-gen function INVALID_ARGUMENT. v2 ListFunctions pages with page_size
// and page_token, and order_by "name desc" reverses the order.
func TestV1FunctionVersionsAndV2ListPaging(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	c, fc := functionsV1Client(t, h), functionsClient(t, h)
	up := func() string {
		u, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
		if err != nil {
			t.Fatalf("GenerateUploadUrl: %v", err)
		}
		putSource(ctx, t, u.GetUploadUrl(), []byte("not a zip"))
		return u.GetUploadUrl()
	}
	first, second := up(), up()
	names := []string{}
	for _, id := range []string{"ver-a", "ver-b", "ver-c"} {
		name := parent + "/functions/" + id
		names = append(names, name)
		t.Cleanup(func() {
			if op, err := c.DeleteFunction(context.Background(), &fv1.DeleteFunctionRequest{Name: name}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{
			Name: name, Description: "first", Runtime: "go123", EntryPoint: "Hello",
			SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: first},
			Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}}})
		if err != nil {
			t.Fatalf("CreateFunction %s: %v", id, err)
		}
		_, _ = op.Wait(ctx) // the source does not build: OFFLINE
	}
	name := names[0]
	op, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{
		Function:   &fv1.CloudFunction{Name: name, Description: "second", SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: second}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "sourceUploadUrl"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	_, _ = op.Wait(ctx)
	cur, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
	if err != nil || cur.GetVersionId() != 2 || cur.GetDescription() != "second" || cur.GetSourceUploadUrl() != second {
		t.Fatalf("GetFunction = %v, %v; want version 2", cur, err)
	}
	old, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name, VersionId: 1})
	if err != nil || old.GetVersionId() != 1 || old.GetDescription() != "first" || old.GetSourceUploadUrl() != first {
		t.Errorf("GetFunction(version_id 1) = %v, %v; want version 1 as it stood", old, err)
	}
	if _, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name, VersionId: 7}); status.Code(err) != codes.NotFound {
		t.Errorf("GetFunction(version_id 7) = %v; want NOT_FOUND", err)
	}
	dl1, err := c.GenerateDownloadUrl(ctx, &fv1.GenerateDownloadUrlRequest{Name: name, VersionId: 1})
	if err != nil {
		t.Fatalf("GenerateDownloadUrl(version_id 1): %v", err)
	}
	dl2, err := c.GenerateDownloadUrl(ctx, &fv1.GenerateDownloadUrlRequest{Name: name})
	if err != nil || dl1.GetDownloadUrl() == dl2.GetDownloadUrl() {
		t.Errorf("GenerateDownloadUrl = %v, %v; want version 1's source to differ from version 2's (%s)", dl2, err, dl1.GetDownloadUrl())
	}
	resp, err := http.Get(dl1.GetDownloadUrl())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "not a zip" {
		t.Errorf("version 1's download = %d %q", resp.StatusCode, b)
	}
	v2old, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name, Revision: "1"})
	if err != nil || v2old.GetDescription() != "first" || v2old.GetEnvironment() != functionspb.Environment_GEN_1 {
		t.Errorf("v2 GetFunction(revision 1) = %v, %v; want version 1", v2old, err)
	}

	// v2 ListFunctions paging and order.
	var paged []string
	token := ""
	for {
		// The official client's pager, one function a page, so each page
		// is a ListFunctions call with page_size 1 and the last page token.
		var page []*functionspb.Function
		next, err := iterator.NewPager(fc.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent, Filter: `environment="GEN_1"`}), 1, token).NextPage(&page)
		if err != nil {
			t.Fatalf("ListFunctions page: %v", err)
		}
		if len(page) > 1 {
			t.Fatalf("a page of size 1 had %d functions", len(page))
		}
		for _, f := range page {
			if strings.Contains(f.GetName(), "/functions/ver-") {
				paged = append(paged, f.GetName())
			}
		}
		if token = next; token == "" {
			break
		}
	}
	if strings.Join(paged, ",") != strings.Join(names, ",") {
		t.Errorf("paged = %v; want %v in name order", paged, names)
	}
	var got []string
	dit := fc.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent, OrderBy: "name desc"})
	for {
		f, err := dit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFunctions order_by: %v", err)
		}
		if strings.Contains(f.GetName(), "/functions/ver-") {
			got = append(got, f.GetName())
		}
	}
	if len(got) != 3 || got[0] != names[2] || got[2] != names[0] {
		t.Errorf("order_by name desc = %v", got)
	}
}

// v1EventFunction is eventFunction for a 1st-gen function: the event
// probe as its Cloud Run service, a v1 create with trigger from a source
// that does not build (OFFLINE), then an update of availableMemoryMb, which
// serves the service's image: the function is ACTIVE on the probe with its
// trigger made.
func v1EventFunction(ctx context.Context, t *testing.T, h *Harness, id string, trigger *fv1.EventTrigger) (*fv1.CloudFunction, string) {
	t.Helper()
	rc, c := runClient(t, h), functionsV1Client(t, h)
	parent := runParent(h)
	svcName := parent + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: eventProbeImage}},
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 1}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: svcName})
	})
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("the probe service never became ready: %v", err)
	}
	up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("not a zip: the probe's image is served"))
	name := parent + "/functions/" + id
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if op, err := c.DeleteFunction(dctx, &fv1.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	cop, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{
		Name: name, Runtime: "go123", EntryPoint: "Probe", MinInstances: 1, MaxInstances: 1,
		SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
		Trigger:    &fv1.CloudFunction_EventTrigger{EventTrigger: trigger}}})
	if err != nil {
		t.Fatalf("CreateFunction with an event trigger: %v", err)
	}
	if _, err := cop.Wait(ctx); err == nil || !strings.Contains(err.Error(), "Build failed") {
		t.Fatalf("CreateFunction.Wait = %v; want the build's error", err)
	}
	uop, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: &fv1.CloudFunction{Name: name, AvailableMemoryMb: 512},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"availableMemoryMb"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	fn, err := uop.Wait(ctx)
	if err != nil {
		t.Fatalf("UpdateFunction.Wait: %v", err)
	}
	if fn.GetStatus() != fv1.CloudFunctionStatus_ACTIVE {
		t.Fatalf("the function is %v; want ACTIVE", fn.GetStatus())
	}
	return fn, name
}

// covers: google.cloud.functions.v1.CloudFunctionsService/CallFunction, google.cloud.eventarc.v1.Eventarc/CreateTrigger
//
// TestCallEventTriggeredFunctions (#1958), on an instance with --services
// run,storage,pubsub, each function serving the event probe: v1
// CallFunction of a 1st-gen Pub/Sub function, through the official v1
// client, delivers data as the message of a messagePublished CloudEvent
// (the Functions Framework hands it to a background function as its event
// data), and of a Cloud Storage function as the object of a finalized one;
// `gcloud functions call --data` does the same through gcloud-setup's
// configuration, egress blocked. An Eventarc trigger whose destination is
// a 2nd-gen function (destination.cloudFunction) delivers a published
// message to the function's service.
func TestCallEventTriggeredFunctions(t *testing.T) {
	h := New(t)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	loadEventProbe(ctx, t)
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
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
	base := ingress(t)
	project := h.Project()

	// Pub/Sub.
	tn := topic(t, h, pc, "fn-call-events")
	_, name := v1EventFunction(ctx, t, h, "callev-pubsub", &fv1.EventTrigger{EventType: "google.pubsub.topic.publish", Resource: tn})
	fn2, err := functionsClient(t, h).GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	host := hostOf(t, fn2.GetServiceConfig().GetUri())
	data := base64.StdEncoding.EncodeToString([]byte("called through the API"))
	res, err := c.CallFunction(ctx, &fv1.CallFunctionRequest{Name: name, Data: `{"data":"` + data + `","attributes":{"k":"v"}}`})
	if err != nil || res.GetError() != "" || res.GetExecutionId() == "" {
		t.Fatalf("CallFunction = %v, %v", res, err)
	}
	type mpd struct {
		Message struct {
			Data       string            `json:"data"`
			Attributes map[string]string `json:"attributes"`
			MessageID  string            `json:"messageId"`
		} `json:"message"`
	}
	find := func(want string) probedEvent {
		t.Helper()
		ev, all := probedEvents(t, base, host, func(e probedEvent) bool {
			var m mpd
			return json.Unmarshal([]byte(e.Data), &m) == nil && m.Message.Data == want
		})
		if ev.Headers == nil {
			t.Fatalf("no CloudEvent with data %q reached the function; it got %v", want, all)
		}
		return ev
	}
	ev := find(data)
	var m mpd
	_ = json.Unmarshal([]byte(ev.Data), &m)
	if ev.Headers["ce-type"] != "google.cloud.pubsub.topic.v1.messagePublished" || ev.Headers["ce-source"] != "//pubsub.googleapis.com/"+tn ||
		m.Message.Attributes["k"] != "v" || m.Message.MessageID == "" {
		t.Errorf("the call's CloudEvent = %v %s", ev.Headers, ev.Data)
	}

	// gcloud functions call of the same function.
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_RESOURCEMANAGER", EnvResourceManager)
	g := newGcloudSession(t, h)
	// The function is in the harness's project, which --project names.
	gdata := base64.StdEncoding.EncodeToString([]byte("called by gcloud"))
	out, err := g.run(nil, "functions", "call", "callev-pubsub", "--project="+project, "--region=us-central1", `--data={"data":"`+gdata+`"}`)
	t.Logf("gcloud functions call:\n%s", out)
	if err != nil {
		t.Errorf("gcloud functions call of an event function: %v", err)
	} else {
		find(gdata)
	}

	// Cloud Storage.
	bh := bucket(t, h, sc)
	_, sname := v1EventFunction(ctx, t, h, "callev-storage", &fv1.EventTrigger{EventType: "google.storage.object.finalize",
		Resource: "projects/_/buckets/" + bh.BucketName()})
	sfn, err := functionsClient(t, h).GetFunction(ctx, &functionspb.GetFunctionRequest{Name: sname})
	if err != nil {
		t.Fatal(err)
	}
	shost := hostOf(t, sfn.GetServiceConfig().GetUri())
	if res, err := c.CallFunction(ctx, &fv1.CallFunctionRequest{Name: sname, Data: `{"name":"called.txt","contentType":"text/plain"}`}); err != nil || res.GetError() != "" {
		t.Fatalf("CallFunction of the storage function = %v, %v", res, err)
	}
	sev, all := probedEvents(t, base, shost, func(e probedEvent) bool { return e.Headers["ce-subject"] == "objects/called.txt" })
	if sev.Headers == nil {
		t.Fatalf("no finalized CloudEvent for called.txt reached the function; it got %v", all)
	}
	if sev.Headers["ce-type"] != "google.cloud.storage.object.v1.finalized" || sev.Headers["ce-bucket"] != bh.BucketName() ||
		!strings.Contains(sev.Data, `"bucket":"`+bh.BucketName()+`"`) {
		t.Errorf("the call's CloudEvent = %v %s", sev.Headers, sev.Data)
	}

	// An Eventarc trigger to a 2nd-gen function.
	efn, ename := eventFunction(ctx, t, h, "eventarc-fn-dest", nil)
	ehost := hostOf(t, efn.GetServiceConfig().GetUri())
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	etn := topic(t, h, pc, "eventarc-to-function")
	top, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: runParent(h), TriggerId: "to-function", Trigger: &eventarcpb.Trigger{
		EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}},
		Destination:  &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudFunction{CloudFunction: ename}},
		Transport:    &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{Topic: etn}}}}})
	if err != nil {
		t.Fatalf("CreateTrigger to a function: %v", err)
	}
	tr, err := top.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTrigger to a function: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: tr.GetName(), AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if tr.GetDestination().GetCloudFunction() != ename {
		t.Errorf("the trigger's destination = %v; want %s", tr.GetDestination(), ename)
	}
	pr, err := pc.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: etn, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("to a function")}}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgID := pr.GetMessageIds()[0]
	eev, all := probedEvents(t, base, ehost, func(e probedEvent) bool { return e.Headers["ce-id"] == msgID })
	if eev.Headers == nil {
		t.Fatalf("no CloudEvent with ce-id %s reached the function through Eventarc; it got %v", msgID, all)
	}
	if eev.Headers["ce-type"] != "google.cloud.pubsub.topic.v1.messagePublished" || eev.Headers["ce-source"] != "//pubsub.googleapis.com/"+etn {
		t.Errorf("Eventarc's CloudEvent to the function = %v", eev.Headers)
	}
}
