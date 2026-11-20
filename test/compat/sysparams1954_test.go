//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	bq "google.golang.org/api/bigquery/v2"
	cloudkms "google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv2 "google.golang.org/api/run/v2"
	smv1 "google.golang.org/api/secretmanager/v1"
	storagev1 "google.golang.org/api/storage/v1"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The REST system parameters (#1954,
// https://docs.cloud.google.com/apis/docs/system-parameters), through the
// official discovery-based clients' .Fields(...) and raw HTTP.

// sysGet is a raw GET of base+path with the given headers: the status, the
// content type and the body.
func sysGet(t *testing.T, url string, header map[string]string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), b
}

// wantBadMask asserts err is Google's answer to a mask naming a field the
// response has not: 400 "Invalid field selection <path>".
func wantBadMask(t *testing.T, what string, err error, path string) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest || !strings.Contains(ge.Message, "Invalid field selection "+path) {
		t.Errorf("%s: %v; want 400 Invalid field selection %s", what, err, path)
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/CreateKeyRing, google.cloud.kms.v1.KeyManagementService/GetKeyRing, google.cloud.kms.v1.KeyManagementService/ListKeyRings
//
// TestSystemParametersKMS: the generated Cloud KMS JSON client's
// .Fields(...) on create, get and list (with items(...) nesting) returns
// only the fields named; a mask naming no field of the response is 400
// "Invalid field selection"; X-Goog-FieldMask is the same mask as a
// header and x-goog-fieldmask as gRPC metadata; alt=proto returns the
// binary KeyRing (and a binary google.rpc.Status on error); $.xgafv=1
// adds the v1 errors[] list; callback wraps the answer in JSONP; quotaUser
// and userIp are accepted.
func TestSystemParametersKMS(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvKMS)
	if addr == "" {
		t.Fatal("no Cloud KMS endpoint")
	}
	ctx := h.Context()
	js, err := cloudkms.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	loc := "projects/" + h.Project() + "/locations/global"
	id := fmt.Sprintf("sysparams-%d", time.Now().UnixNano())
	created, err := js.Projects.Locations.KeyRings.Create(loc, &cloudkms.KeyRing{}).KeyRingId(id).Fields("name").Do()
	if err != nil {
		t.Fatalf("keyRings.create: %v", err)
	}
	ring := loc + "/keyRings/" + id
	if created.Name != ring || created.CreateTime != "" {
		t.Errorf("create with fields=name = %+v; want the name alone", created)
	}
	got, err := js.Projects.Locations.KeyRings.Get(ring).Fields("createTime").Do()
	if err != nil || got.Name != "" || got.CreateTime == "" {
		t.Errorf("get with fields=createTime = %+v, %v; want createTime alone", got, err)
	}
	list, err := js.Projects.Locations.KeyRings.List(loc).Fields("keyRings(name),nextPageToken").Do()
	if err != nil || len(list.KeyRings) == 0 || list.TotalSize != 0 {
		t.Fatalf("list with fields = %+v, %v", list, err)
	}
	for _, r := range list.KeyRings {
		if r.Name == "" || r.CreateTime != "" {
			t.Errorf("list item %+v; want its name alone", r)
		}
	}
	if _, err := js.Projects.Locations.KeyRings.Get(ring).Fields("bogus").Do(); err != nil {
		wantBadMask(t, "fields=bogus", err, "bogus")
	} else {
		t.Error("fields=bogus succeeded")
	}
	_, err = js.Projects.Locations.KeyRings.List(loc).Fields("keyRings(nope)").Do()
	wantBadMask(t, "fields=keyRings(nope)", err, "keyRings/nope")

	base := "http://" + addr + "/v1/" + ring
	// The header form, and the ignored quota parameters.
	code, _, body := sysGet(t, base+"?quotaUser=u&userIp=10.0.0.1&prettyPrint=false", map[string]string{"X-Goog-FieldMask": "name"})
	if code != 200 || string(body) != `{"name":"`+ring+`"}` {
		t.Errorf("X-Goog-FieldMask: %d %s", code, body)
	}
	// alt=proto: the binary KeyRing, pruned.
	code, ct, body := sysGet(t, base+"?alt=proto&fields=name", nil)
	var kr kmspb.KeyRing
	if err := proto.Unmarshal(body, &kr); code != 200 || ct != "application/x-protobuf" || err != nil || kr.GetName() != ring || kr.GetCreateTime() != nil {
		t.Errorf("alt=proto = %d %s %v (%v)", code, ct, &kr, err)
	}
	code, ct, body = sysGet(t, "http://"+addr+"/v1/"+loc+"/keyRings/absent-ring?alt=proto", nil)
	var st spb.Status
	if err := proto.Unmarshal(body, &st); code != 404 || ct != "application/x-protobuf" || err != nil || st.GetCode() != int32(codes.NotFound) {
		t.Errorf("alt=proto error = %d %s %v (%v)", code, ct, &st, err)
	}
	// $.xgafv: 1 adds errors[]; 2 is the default envelope.
	code, _, body = sysGet(t, "http://"+addr+"/v1/"+loc+"/keyRings/absent-ring?$.xgafv=1", nil)
	var v1 struct {
		Error struct {
			Code   int
			Status string
			Errors []struct{ Domain, Reason string }
		}
	}
	if err := json.Unmarshal(body, &v1); code != 404 || err != nil || v1.Error.Status != "NOT_FOUND" || len(v1.Error.Errors) != 1 || v1.Error.Errors[0].Reason != "notFound" {
		t.Errorf("$.xgafv=1 = %d %s", code, body)
	}
	if code, _, body = sysGet(t, "http://"+addr+"/v1/"+loc+"/keyRings/absent-ring?$.xgafv=2", nil); code != 404 || strings.Contains(string(body), `"errors"`) {
		t.Errorf("$.xgafv=2 = %d %s", code, body)
	}
	// JSONP.
	code, ct, body = sysGet(t, base+"?fields=name&callback=handle&prettyPrint=false", nil)
	if code != 200 || !strings.HasPrefix(ct, "text/javascript") || string(body) != "// API callback\nhandle({\"name\":\""+ring+"\"});\n" {
		t.Errorf("callback = %d %s %q", code, ct, body)
	}

	// gRPC: x-goog-fieldmask.
	kc, err := kms.NewKeyManagementClient(ctx, kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	mctx := metadata.AppendToOutgoingContext(ctx, "x-goog-fieldmask", "name")
	g, err := kc.GetKeyRing(mctx, &kmspb.GetKeyRingRequest{Name: ring})
	if err != nil || g.GetName() != ring || g.GetCreateTime() != nil {
		t.Errorf("gRPC GetKeyRing with x-goog-fieldmask=name = %v, %v", g, err)
	}
	_, err = kc.GetKeyRing(metadata.AppendToOutgoingContext(ctx, "x-goog-fieldmask", "bogus"), &kmspb.GetKeyRingRequest{Name: ring})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "Invalid field selection bogus") {
		t.Errorf("gRPC GetKeyRing with x-goog-fieldmask=bogus = %v", err)
	}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/ListSecrets
//
// TestSystemParametersSecretManager: Secret Manager's hand-written JSON
// API under the shared wrapper: the generated client's .Fields(...) on
// create, get and list returns only the fields named, a malformed mask is
// 400, and x-goog-fieldmask prunes a gRPC answer.
func TestSystemParametersSecretManager(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvSecrets)
	if ep == "" {
		t.Fatal("no Secret Manager endpoint")
	}
	ctx := h.Context()
	s, err := smv1.NewService(ctx, option.WithEndpoint("http://"+ep+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	parent := secretsParent(h)
	id := fmt.Sprintf("sysparams-%d", time.Now().UnixNano())
	name := parent + "/secrets/" + id
	sec := &smv1.Secret{Replication: &smv1.Replication{Automatic: &smv1.Automatic{}}, Labels: map[string]string{"k": "v"}}
	created, err := s.Projects.Secrets.Create(parent, sec).SecretId(id).Fields("name").Do()
	if err != nil {
		t.Fatalf("secrets.create: %v", err)
	}
	t.Cleanup(func() { _, _ = s.Projects.Secrets.Delete(name).Do() })
	if created.Name != name || created.Replication != nil || created.CreateTime != "" {
		t.Errorf("create with fields=name = %+v", created)
	}
	got, err := s.Projects.Secrets.Get(name).Fields("labels,replication/automatic").Do()
	if err != nil || got.Name != "" || got.Labels["k"] != "v" || got.Replication == nil || got.Replication.Automatic == nil || got.CreateTime != "" {
		t.Errorf("get with fields = %+v, %v", got, err)
	}
	list, err := s.Projects.Secrets.List(parent).Fields("secrets(name)").Do()
	if err != nil || len(list.Secrets) == 0 {
		t.Fatalf("list with fields = %+v, %v", list, err)
	}
	for _, it := range list.Secrets {
		if it.Name == "" || it.Replication != nil || it.CreateTime != "" {
			t.Errorf("list item %+v; want its name alone", it)
		}
	}
	var ge *googleapi.Error
	if _, err := s.Projects.Secrets.Get(name).Fields("labels(").Do(); !errors.As(err, &ge) || ge.Code != 400 || !strings.Contains(ge.Message, "Invalid field selection") {
		t.Errorf("fields=labels( = %v; want 400 Invalid field selection", err)
	}

	c := secretsClient(t, h)
	g, err := c.GetSecret(metadata.AppendToOutgoingContext(ctx, "x-goog-fieldmask", "name"), &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil || g.GetName() != name || g.GetReplication() != nil || len(g.GetLabels()) != 0 {
		t.Errorf("gRPC GetSecret with x-goog-fieldmask=name = %v, %v", g, err)
	}
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/GetJob, google.cloud.run.v2.Jobs/ListJobs, google.cloud.run.v2.Jobs/DeleteJob
//
// TestSystemParametersCloudRun: the generated Cloud Run v2 JSON client's
// .Fields(...) on a long-running operation (create, delete), a get and a
// list, and a mask naming no field of the response refused as Google
// refuses it.
func TestSystemParametersCloudRun(t *testing.T) {
	h := New(t)
	s := runV2REST(t, h)
	parent := runParent(h)
	id := fmt.Sprintf("sysparams-%d", time.Now().Unix()%1000000)
	name := parent + "/jobs/" + id
	job := &runv2.GoogleCloudRunV2Job{Template: &runv2.GoogleCloudRunV2ExecutionTemplate{Template: &runv2.GoogleCloudRunV2TaskTemplate{
		Containers: []*runv2.GoogleCloudRunV2Container{{Image: jobImage, Command: []string{"true"}}}}}}
	op, err := s.Projects.Locations.Jobs.Create(parent, job).JobId(id).Fields("name,done,metadata").Do()
	if err != nil {
		t.Fatalf("jobs.create: %v", err)
	}
	if op.Name == "" || op.Response != nil {
		t.Errorf("create with fields=name,done,metadata = %+v; want no response", op)
	}
	op = waitRunV2Op(t, s, op)
	t.Cleanup(func() {
		if d, err := s.Projects.Locations.Jobs.Delete(name).Fields("name,done").Do(); err == nil {
			waitRunV2Op(t, s, d)
		}
	})
	got, err := s.Projects.Locations.Jobs.Get(name).Fields("name,uid").Do()
	if err != nil || got.Name != name || got.Uid == "" || got.Template != nil || got.CreateTime != "" {
		t.Errorf("get with fields=name,uid = %+v, %v", got, err)
	}
	got, err = s.Projects.Locations.Jobs.Get(name).Fields("template/template/containers(image)").Do()
	if err != nil || got.Name != "" || got.Template == nil || got.Template.Template == nil || len(got.Template.Template.Containers) != 1 ||
		got.Template.Template.Containers[0].Image != jobImage || len(got.Template.Template.Containers[0].Command) != 0 {
		t.Errorf("get with a nested mask = %+v, %v", got, err)
	}
	list, err := s.Projects.Locations.Jobs.List(parent).Fields("jobs(name)").Do()
	if err != nil || len(list.Jobs) == 0 {
		t.Fatalf("list with fields = %+v, %v", list, err)
	}
	for _, j := range list.Jobs {
		if j.Name == "" || j.Uid != "" || j.Template != nil {
			t.Errorf("list item %+v; want its name alone", j)
		}
	}
	_, err = s.Projects.Locations.Jobs.Get(name).Fields("template/nope").Do()
	wantBadMask(t, "fields=template/nope", err, "template/nope")
}

// covers: storage.buckets.insert, storage.buckets.get, storage.objects.insert, storage.objects.list
//
// TestSystemParametersStorage: the generated Cloud Storage JSON API
// client's .Fields(...) on a bucket and an object list (items(...)
// nesting), $fields and X-Goog-FieldMask over raw HTTP, and a JSONP
// callback.
func TestSystemParametersStorage(t *testing.T) {
	h := New(t)
	s := storageJSON(t, h)
	ctx := h.Context()
	bucket := strings.ToLower(fmt.Sprintf("sysparams-%d", time.Now().UnixNano()))
	b, err := s.Buckets.Insert(h.Project(), &storagev1.Bucket{Name: bucket, Labels: map[string]string{"k": "v"}}).Fields("name").Context(ctx).Do()
	if err != nil {
		t.Fatalf("buckets.insert: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Objects.Delete(bucket, "a.txt").Do()
		_ = s.Buckets.Delete(bucket).Do()
	})
	if b.Name != bucket || b.Id != "" || len(b.Labels) != 0 {
		t.Errorf("insert with fields=name = %+v", b)
	}
	if b, err = s.Buckets.Get(bucket).Fields("labels,storageClass").Do(); err != nil || b.Name != "" || b.Labels["k"] != "v" || b.StorageClass == "" {
		t.Errorf("get with fields = %+v, %v", b, err)
	}
	if _, err := s.Objects.Insert(bucket, &storagev1.Object{Name: "a.txt"}).Media(strings.NewReader("hello")).Do(); err != nil {
		t.Fatalf("objects.insert: %v", err)
	}
	list, err := s.Objects.List(bucket).Fields("items(name,size),nextPageToken").Do()
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "a.txt" || list.Items[0].Size != 5 || list.Items[0].Bucket != "" || list.Kind != "" {
		t.Errorf("list with fields = %+v, %v", list, err)
	}
	base := h.Endpoint(EnvStorage) + "/storage/v1/b/" + bucket
	if code, _, body := sysGet(t, base+"?$fields=name&prettyPrint=false", nil); code != 200 || string(body) != `{"name":"`+bucket+`"}` {
		t.Errorf("$fields = %d %s", code, body)
	}
	if code, _, body := sysGet(t, base+"?prettyPrint=false", map[string]string{"X-Goog-FieldMask": "name"}); code != 200 || string(body) != `{"name":"`+bucket+`"}` {
		t.Errorf("X-Goog-FieldMask = %d %s", code, body)
	}
	code, ct, body := sysGet(t, base+"?fields=name&prettyPrint=false&callback=cb", nil)
	if code != 200 || !strings.HasPrefix(ct, "text/javascript") || string(body) != "// API callback\ncb({\"name\":\""+bucket+"\"});\n" {
		t.Errorf("callback = %d %s %q", code, ct, body)
	}
	if code, _, body := sysGet(t, base+"?fields=a(", nil); code != 400 || !strings.Contains(string(body), "Invalid field selection") {
		t.Errorf("fields=a( = %d %s", code, body)
	}
}

// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.list
//
// TestSystemParametersBigQuery: the generated BigQuery v2 client's
// .Fields(...) on a dataset's insert, get and list, through the front.
func TestSystemParametersBigQuery(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	ctx := h.Context()
	svc, err := bq.NewService(ctx, option.WithEndpoint(ep), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_sysparams"
	ds, err := svc.Datasets.Insert(project, &bq.Dataset{DatasetReference: &bq.DatasetReference{ProjectId: project, DatasetId: id},
		Description: "d", Labels: map[string]string{"k": "v"}}).Fields("datasetReference").Do()
	if err != nil {
		t.Fatalf("datasets.insert: %v", err)
	}
	t.Cleanup(func() { _ = svc.Datasets.Delete(project, id).DeleteContents(true).Do() })
	if ds.DatasetReference == nil || ds.DatasetReference.DatasetId != id || ds.Description != "" || ds.Id != "" {
		t.Errorf("insert with fields=datasetReference = %+v", ds)
	}
	ds, err = svc.Datasets.Get(project, id).Fields("description,labels").Do()
	if err != nil || ds.Description != "d" || ds.Labels["k"] != "v" || ds.DatasetReference != nil || ds.Etag != "" {
		t.Errorf("get with fields = %+v, %v", ds, err)
	}
	list, err := svc.Datasets.List(project).Fields("datasets(datasetReference/datasetId)").Do()
	if err != nil || len(list.Datasets) == 0 || list.Kind != "" {
		t.Fatalf("list with fields = %+v, %v", list, err)
	}
	for _, d := range list.Datasets {
		if d.DatasetReference == nil || d.DatasetReference.DatasetId == "" || d.DatasetReference.ProjectId != "" || d.Id != "" {
			t.Errorf("list item %+v; want datasetReference.datasetId alone", d)
		}
	}
}
