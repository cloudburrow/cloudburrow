package logging

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func obsCall(t *testing.T, a *Admin, method, path, body string, out proto.Message) int {
	t.Helper()
	code, raw := a.CallObservability(method, path, []byte(body))
	if code == http.StatusOK && out != nil {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decoding %s: %v", method, path, raw, err)
		}
	}
	return code
}

// opResponse is a done operation's response.
func opResponse(t *testing.T, op *longrunningpb.Operation, out proto.Message) {
	t.Helper()
	if !op.GetDone() || op.GetResponse() == nil {
		t.Fatalf("operation %v is not done with a response", op)
	}
	if err := op.GetResponse().UnmarshalTo(out); err != nil {
		t.Fatalf("operation response: %v", err)
	}
	md := &obspb.OperationMetadata{}
	if err := op.GetMetadata().UnmarshalTo(md); err != nil || md.GetApiVersion() != "v1" || md.GetVerb() == "" || md.GetTarget() == "" {
		t.Errorf("operation metadata = %v, %v", md, err)
	}
}

// TestObservabilitySettingsAndOperations (#2117): settings start empty with
// the project's service account; updateSettings answers a done operation
// kept for operations.get and list; an unsupported location and a key whose
// location is not the default storage location are refused.
func TestObservabilitySettingsAndOperations(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	const base = "/v1/projects/proj-1/locations/global"
	st := &obspb.Settings{}
	if code := obsCall(t, a, http.MethodGet, base+"/settings", "", st); code != http.StatusOK ||
		st.GetName() != "projects/proj-1/locations/global/settings" || !strings.HasSuffix(st.GetServiceAccountId(), "@gcp-sa-observability.iam.gserviceaccount.com") ||
		!strings.HasPrefix(st.GetServiceAccountId(), "service-") || st.GetDefaultStorageLocation() != "" {
		t.Fatalf("getSettings = %d %v", code, st)
	}
	op := &longrunningpb.Operation{}
	if code := obsCall(t, a, http.MethodPatch, base+"/settings?updateMask=defaultStorageLocation", `{"defaultStorageLocation":"eu"}`, op); code != http.StatusOK {
		t.Fatalf("updateSettings = %d", code)
	}
	got := &obspb.Settings{}
	opResponse(t, op, got)
	if got.GetDefaultStorageLocation() != "eu" {
		t.Errorf("updateSettings response = %v", got)
	}
	if code := obsCall(t, a, http.MethodGet, base+"/settings", "", st); code != http.StatusOK || st.GetDefaultStorageLocation() != "eu" {
		t.Errorf("getSettings after update = %v", st)
	}
	again := &longrunningpb.Operation{}
	if code := obsCall(t, a, http.MethodGet, "/v1/"+op.GetName(), "", again); code != http.StatusOK || again.GetName() != op.GetName() {
		t.Errorf("operations.get = %d %v", code, again)
	}
	list := &longrunningpb.ListOperationsResponse{}
	if code := obsCall(t, a, http.MethodGet, base+"/operations", "", list); code != http.StatusOK || len(list.GetOperations()) != 1 {
		t.Errorf("operations.list = %d %v", code, list)
	}
	for path, body := range map[string]string{
		base + "/settings?updateMask=defaultStorageLocation":                       `{"defaultStorageLocation":"mars1"}`,
		base + "/settings?updateMask=serviceAccountId":                             `{"serviceAccountId":"x"}`,
		"/v1/projects/proj-1/locations/us-central1/settings?updateMask=kmsKeyName": `{"kmsKeyName":"projects/k/locations/europe-west1/keyRings/r/cryptoKeys/k"}`,
		"/v1/projects/proj-1/locations/nowhere/settings":                           `{}`,
	} {
		if code := obsCall(t, a, http.MethodPatch, path, body, nil); code != http.StatusBadRequest {
			t.Errorf("PATCH %s %s = %d, want 400", path, body, code)
		}
	}
	// A key in us-central1 while the default storage location is eu.
	if code := obsCall(t, a, http.MethodPatch, "/v1/projects/proj-1/locations/us-central1/settings?updateMask=kmsKeyName",
		`{"kmsKeyName":"projects/k/locations/us-central1/keyRings/r/cryptoKeys/k"}`, nil); code != http.StatusBadRequest {
		t.Errorf("a key outside the default storage location = %d, want 400 (FAILED_PRECONDITION)", code)
	}
	if code := obsCall(t, a, http.MethodGet, "/v1/organizations/1/locations/global/settings", "", nil); code != http.StatusNotImplemented {
		t.Errorf("an organization without Resource Manager = %d, want 501", code)
	}
}

// TestObservabilityBucketsDatasetsViewsLinks (#2117): _Trace is made when
// trace data arrives, in the default storage location, with Spans and
// _AllSpans; buckets are created, patched and reprovisioned once; a link
// makes the BigQuery dataset, and a dataset holds one link.
func TestObservabilityBucketsDatasetsViewsLinks(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	ctx := context.Background()
	if code := obsCall(t, a, http.MethodPatch, "/v1/projects/proj-1/locations/global/settings?updateMask=defaultStorageLocation", `{"defaultStorageLocation":"eu"}`, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if code := obsCall(t, a, http.MethodGet, "/v1/projects/proj-1/locations/eu/buckets/_Trace", "", nil); code != http.StatusNotFound {
		t.Errorf("_Trace before any trace data = %d, want 404", code)
	}
	if err := a.EnsureTraceBucket(ctx, "proj-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureTraceBucket(ctx, "proj-1"); err != nil {
		t.Fatal(err)
	}
	b := &obspb.Bucket{}
	if code := obsCall(t, a, http.MethodGet, "/v1/projects/proj-1/locations/eu/buckets/_Trace", "", b); code != http.StatusOK || b.GetCreateTime() == nil {
		t.Fatalf("_Trace = %d %v", code, b)
	}
	ds := &obspb.ListDatasetsResponse{}
	if code := obsCall(t, a, http.MethodGet, "/v1/projects/proj-1/locations/eu/buckets/_Trace/datasets", "", ds); code != http.StatusOK ||
		len(ds.GetDatasets()) != 1 || !strings.HasSuffix(ds.GetDatasets()[0].GetName(), "/datasets/Spans") {
		t.Errorf("datasets.list = %d %v", code, ds)
	}
	v := &obspb.View{}
	if code := obsCall(t, a, http.MethodGet, "/v1/projects/proj-1/locations/eu/buckets/_Trace/datasets/Spans/views/_AllSpans", "", v); code != http.StatusOK {
		t.Errorf("views.get _AllSpans = %d", code)
	}
	if code := obsCall(t, a, http.MethodPost, "/v1/projects/proj-1/locations/us/buckets?bucketId=_Trace", `{}`, nil); code != http.StatusConflict {
		t.Errorf("a second _Trace = %d, want 409", code)
	}
	for _, path := range []string{"/v1/projects/proj-1/locations/global/buckets?bucketId=ok", "/v1/projects/proj-1/locations/us/buckets?bucketId=_mine", "/v1/projects/proj-1/locations/us/buckets"} {
		if code := obsCall(t, a, http.MethodPost, path, `{}`, nil); code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", path, code)
		}
	}
	op := &longrunningpb.Operation{}
	if code := obsCall(t, a, http.MethodPost, "/v1/projects/proj-1/locations/us/buckets?bucketId=mine", `{"displayName":"Mine"}`, op); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	mine := &obspb.Bucket{}
	opResponse(t, op, mine)
	if mine.GetDisplayName() != "Mine" || mine.GetName() != "projects/proj-1/locations/us/buckets/mine" {
		t.Errorf("create response = %v", mine)
	}
	if code := obsCall(t, a, http.MethodPatch, "/v1/"+mine.GetName()+"?updateMask=description", `{"description":"d"}`, op); code != http.StatusOK {
		t.Errorf("patch = %d", code)
	}
	all := &obspb.ListBucketsResponse{}
	if code := obsCall(t, a, http.MethodGet, "/v1/projects/proj-1/locations/-/buckets", "", all); code != http.StatusOK || len(all.GetBuckets()) != 2 {
		t.Errorf("buckets.list - = %d %v", code, all)
	}
	dropped := ""
	a.OnTraceBucketReprovision(func(p string) { dropped = p })
	if code := obsCall(t, a, http.MethodPost, "/v1/projects/proj-1/locations/eu/buckets/_Trace:reprovision", `{}`, op); code != http.StatusOK || dropped != "proj-1" {
		t.Errorf("reprovision = %d, dropped %q", code, dropped)
	}
	if code := obsCall(t, a, http.MethodPost, "/v1/projects/proj-1/locations/eu/buckets/_Trace:reprovision", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("a second reprovision = %d, want 400 (FAILED_PRECONDITION)", code)
	}

	const links = "/v1/projects/proj-1/locations/eu/buckets/_Trace/datasets/Spans/links"
	if code := obsCall(t, a, http.MethodPost, links+"?linkId=spans", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("a link without BigQuery = %d, want 400", code)
	}
	fl := newFakeLinked()
	a.UseLinkedDatasets(fl)
	if code := obsCall(t, a, http.MethodPost, links+"?linkId=bad-id", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("a link ID with a hyphen = %d", code)
	}
	if code := obsCall(t, a, http.MethodPost, links+"?linkId=spans", `{"description":"to BQ"}`, op); code != http.StatusOK {
		t.Fatalf("links.create = %d", code)
	}
	l := &obspb.Link{}
	opResponse(t, op, l)
	if ok, _ := fl.Exists(ctx, "proj-1", "spans"); !ok || l.GetDescription() != "to BQ" {
		t.Errorf("links.create made %v, dataset %v", l, ok)
	}
	if ids := fl.viewIDs("proj-1:spans"); len(ids) != 1 || ids[0] != AllSpansViewID {
		t.Errorf("linked views = %v", ids)
	}
	if code := obsCall(t, a, http.MethodPost, links+"?linkId=other", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("a second link = %d, want 400 (FAILED_PRECONDITION)", code)
	}
	if err := a.AppendSpans(ctx, "proj-1", []map[string]any{{"trace_id": "t", "span_id": "s"}}, []string{"t/s"}); err != nil {
		t.Fatal(err)
	}
	if n := len(fl.datasets["proj-1:spans"]); n != 1 {
		t.Errorf("AppendSpans rows = %d", n)
	}
	if code := obsCall(t, a, http.MethodPatch, "/v1/"+l.GetName()+"?updateMask=displayName", `{"displayName":"S"}`, op); code != http.StatusOK {
		t.Errorf("links.patch = %d", code)
	}
	ll := &obspb.ListLinksResponse{}
	if code := obsCall(t, a, http.MethodGet, links, "", ll); code != http.StatusOK || len(ll.GetLinks()) != 1 || ll.GetLinks()[0].GetDisplayName() != "S" {
		t.Errorf("links.list = %d %v", code, ll)
	}
	if code := obsCall(t, a, http.MethodDelete, "/v1/"+l.GetName(), "", op); code != http.StatusOK {
		t.Errorf("links.delete = %d", code)
	}
	if ok, _ := fl.Exists(ctx, "proj-1", "spans"); ok {
		t.Error("the linked dataset outlived its link")
	}
	if code := obsCall(t, a, http.MethodGet, "/v1/"+l.GetName(), "", nil); code != http.StatusNotFound {
		t.Errorf("links.get after delete = %d", code)
	}
	if code := obsCall(t, a, http.MethodGet, "/v1/projects/proj-1/locations/us/buckets/mine/datasets/Spans", "", nil); code != http.StatusNotFound {
		t.Errorf("a dataset of a bucket other than _Trace = %d", code)
	}
}
