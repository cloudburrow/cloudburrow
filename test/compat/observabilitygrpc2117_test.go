//go:build compat

package compat

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obs "cloud.google.com/go/observability/apiv1"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Observability API over gRPC (#2117): google.cloud.observability.v1
// as googleapis publishes it, through the official
// cloud.google.com/go/observability/apiv1 client's NewClient (gRPC), on the
// Logging port, with its Locations and Operations mixins.

func observabilityGRPCClient(t *testing.T, h *Harness, endpoint string) *obs.Client {
	t.Helper()
	if endpoint == "" {
		t.Fatal("no Logging endpoint")
	}
	c, err := obs.NewClient(h.Context(), option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("observability.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: observability.projects.locations.getSettings, observability.projects.locations.updateSettings, observability.projects.locations.operations.get, observability.projects.locations.operations.list, observability.projects.locations.operations.delete, observability.projects.locations.get, observability.projects.locations.list, observability.projects.locations.buckets.create, observability.projects.locations.buckets.get, observability.projects.locations.buckets.list, observability.projects.locations.buckets.patch, observability.projects.locations.buckets.datasets.list, observability.projects.locations.buckets.datasets.get, observability.projects.locations.buckets.datasets.views.list, observability.projects.locations.buckets.datasets.views.get, observability.projects.locations.buckets.datasets.links.list, observability.projects.locations.scopes.get, observability.projects.locations.scopes.patch, observability.projects.locations.traceScopes.create, observability.projects.locations.traceScopes.get, observability.projects.locations.traceScopes.list, observability.projects.locations.traceScopes.patch, observability.projects.locations.traceScopes.delete, observability.folders.locations.getSettings
//
// TestObservabilityGRPC2117: every Observability method the proto has,
// over gRPC: settings and their operation (Wait, metadata, GetOperation,
// ListOperations with and without done=true, DeleteOperation), the
// locations, _Trace with its Spans dataset and _AllSpans view, a trace
// scope made, read, listed, updated, set as the project's scope and
// deleted, a folder's settings; and the canonical codes over gRPC:
// NOT_FOUND, ALREADY_EXISTS, INVALID_ARGUMENT and FAILED_PRECONDITION.
func TestObservabilityGRPC2117(t *testing.T) {
	h := New(t)
	logging := h.Endpoint(EnvLogging)
	c := observabilityGRPCClient(t, h, logging)
	ctx := h.Context()
	project := fmt.Sprintf("obsg2117-%d", time.Now().UnixNano()%1e9)
	global := "projects/" + project + "/locations/global"

	// Settings, over an operation.
	st, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"})
	if err != nil || st.GetName() != global+"/settings" || !strings.HasSuffix(st.GetServiceAccountId(), "@gcp-sa-observability.iam.gserviceaccount.com") {
		t.Fatalf("GetSettings = %v, %v", st, err)
	}
	op, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: "us-central1"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got, err := op.Wait(ctx); err != nil || got.GetDefaultStorageLocation() != "us-central1" {
		t.Fatalf("UpdateSettings.Wait = %v, %v", got, err)
	}
	if md, err := op.Metadata(); err != nil || md.GetVerb() != "update" || md.GetTarget() != global+"/settings" {
		t.Errorf("UpdateSettings metadata = %v, %v", md, err)
	}
	if _, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: "moon1"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an unsupported default storage location: %v; want INVALID_ARGUMENT", err)
	}
	if o, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()}); err != nil || !o.GetDone() || o.GetName() != op.Name() {
		t.Errorf("GetOperation = %v, %v", o, err)
	}
	listOps := func(filter string) []string {
		var names []string
		for it := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global, Filter: filter}); ; {
			o, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return names
			}
			if err != nil {
				t.Fatalf("ListOperations %q: %v", filter, err)
			}
			names = append(names, o.GetName())
		}
	}
	for _, f := range []string{"", "done=true"} {
		if got := listOps(f); len(got) != 1 || got[0] != op.Name() {
			t.Errorf("ListOperations %q = %v; want [%s]", f, got, op.Name())
		}
	}
	if got := listOps("done=false"); len(got) != 0 {
		t.Errorf("ListOperations done=false = %v; want none", got)
	}
	if err := c.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.Name()}); err != nil {
		t.Errorf("DeleteOperation: %v", err)
	}
	if _, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetOperation after DeleteOperation: %v; want NOT_FOUND", err)
	}

	// Locations.
	if l, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: "projects/" + project + "/locations/us"}); err != nil || l.GetLocationId() != "us" {
		t.Errorf("GetLocation = %v, %v", l, err)
	}
	var locs int
	for it := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project}); ; {
		_, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListLocations: %v", err)
		}
		locs++
	}
	if locs < 2 {
		t.Errorf("ListLocations = %d locations", locs)
	}

	// _Trace, its dataset and view.
	us := "projects/" + project + "/locations/us"
	if _, err := c.GetBucket(ctx, &obspb.GetBucketRequest{Name: us + "/buckets/_Trace"}); status.Code(err) != codes.NotFound {
		t.Errorf("_Trace before it is made: %v; want NOT_FOUND", err)
	}
	bop, err := c.CreateBucket(ctx, &obspb.CreateBucketRequest{Parent: us, BucketId: "_Trace", Bucket: &obspb.Bucket{DisplayName: "Traces"}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	b, err := bop.Wait(ctx)
	if err != nil || b.GetName() != us+"/buckets/_Trace" || b.GetDisplayName() != "Traces" {
		t.Fatalf("CreateBucket.Wait = %v, %v", b, err)
	}
	if _, err := c.CreateBucket(ctx, &obspb.CreateBucketRequest{Parent: "projects/" + project + "/locations/eu", BucketId: "_Trace"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second _Trace: %v; want ALREADY_EXISTS", err)
	}
	if got, err := c.GetBucket(ctx, &obspb.GetBucketRequest{Name: b.GetName()}); err != nil || got.GetDisplayName() != "Traces" {
		t.Errorf("GetBucket = %v, %v", got, err)
	}
	var buckets []string
	for it := c.ListBuckets(ctx, &obspb.ListBucketsRequest{Parent: "projects/" + project + "/locations/-"}); ; {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListBuckets: %v", err)
		}
		buckets = append(buckets, x.GetName())
	}
	if len(buckets) != 1 || buckets[0] != b.GetName() {
		t.Errorf("ListBuckets = %v", buckets)
	}
	uop, err := c.UpdateBucket(ctx, &obspb.UpdateBucketRequest{Bucket: &obspb.Bucket{Name: b.GetName(), Description: "spans"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatalf("UpdateBucket: %v", err)
	}
	if got, err := uop.Wait(ctx); err != nil || got.GetDescription() != "spans" {
		t.Errorf("UpdateBucket.Wait = %v, %v", got, err)
	}
	spans := b.GetName() + "/datasets/Spans"
	if ds, err := c.ListDatasets(ctx, &obspb.ListDatasetsRequest{Parent: b.GetName()}).Next(); err != nil || ds.GetName() != spans {
		t.Errorf("ListDatasets = %v, %v", ds, err)
	}
	if ds, err := c.GetDataset(ctx, &obspb.GetDatasetRequest{Name: spans}); err != nil || ds.GetName() != spans {
		t.Errorf("GetDataset = %v, %v", ds, err)
	}
	if v, err := c.ListViews(ctx, &obspb.ListViewsRequest{Parent: spans}).Next(); err != nil || v.GetName() != spans+"/views/_AllSpans" {
		t.Errorf("ListViews = %v, %v", v, err)
	}
	if _, err := c.GetView(ctx, &obspb.GetViewRequest{Name: spans + "/views/_AllSpans"}); err != nil {
		t.Errorf("GetView: %v", err)
	}
	if _, err := c.ListLinks(ctx, &obspb.ListLinksRequest{Parent: spans}).Next(); !errors.Is(err, iterator.Done) {
		t.Errorf("ListLinks before any link: %v; want none", err)
	}
	if _, err := c.CreateLink(ctx, &obspb.CreateLinkRequest{Parent: spans, LinkId: "bad-id"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateLink with a hyphen: %v; want INVALID_ARGUMENT", err)
	}
	if _, err := c.GetLink(ctx, &obspb.GetLinkRequest{Name: spans + "/links/none"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLink of no link: %v; want NOT_FOUND", err)
	}

	// A trace scope, and the project's scope set to it.
	id := "grpc"
	ts, err := c.CreateTraceScope(ctx, &obspb.CreateTraceScopeRequest{Parent: global, TraceScopeId: id,
		TraceScope: &obspb.TraceScope{ResourceNames: []string{"projects/" + project}, Description: "one project"}})
	if err != nil || ts.GetName() != global+"/traceScopes/"+id || ts.GetCreateTime() == nil {
		t.Fatalf("CreateTraceScope = %v, %v", ts, err)
	}
	if _, err := c.CreateTraceScope(ctx, &obspb.CreateTraceScopeRequest{Parent: global, TraceScopeId: id,
		TraceScope: &obspb.TraceScope{ResourceNames: []string{"projects/" + project}}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateTraceScope: %v; want ALREADY_EXISTS", err)
	}
	if got, err := c.GetTraceScope(ctx, &obspb.GetTraceScopeRequest{Name: ts.GetName()}); err != nil || got.GetDescription() != "one project" {
		t.Errorf("GetTraceScope = %v, %v", got, err)
	}
	var scopes []string
	for it := c.ListTraceScopes(ctx, &obspb.ListTraceScopesRequest{Parent: global}); ; {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListTraceScopes: %v", err)
		}
		scopes = append(scopes, x.GetName())
	}
	if !containsString(scopes, ts.GetName()) {
		t.Errorf("ListTraceScopes = %v; want %s among them", scopes, ts.GetName())
	}
	if got, err := c.UpdateTraceScope(ctx, &obspb.UpdateTraceScopeRequest{TraceScope: &obspb.TraceScope{Name: ts.GetName(), Description: "renamed"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil || got.GetDescription() != "renamed" {
		t.Errorf("UpdateTraceScope = %v, %v", got, err)
	}
	scopeName := global + "/scopes/_Default"
	if sc, err := c.GetScope(ctx, &obspb.GetScopeRequest{Name: scopeName}); err != nil || sc.GetName() != scopeName {
		t.Errorf("GetScope = %v, %v", sc, err)
	}
	if sc, err := c.UpdateScope(ctx, &obspb.UpdateScopeRequest{Scope: &obspb.Scope{Name: scopeName, TraceScope: ts.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"trace_scope"}}}); err != nil || sc.GetTraceScope() != ts.GetName() {
		t.Errorf("UpdateScope = %v, %v", sc, err)
	}
	if _, err := c.UpdateScope(ctx, &obspb.UpdateScopeRequest{Scope: &obspb.Scope{Name: scopeName, TraceScope: global + "/traceScopes/none"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"trace_scope"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateScope to no trace scope: %v; want NOT_FOUND", err)
	}
	if _, err := c.UpdateScope(ctx, &obspb.UpdateScopeRequest{Scope: &obspb.Scope{Name: scopeName, TraceScope: global + "/traceScopes/_Default"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"trace_scope"}}}); err != nil {
		t.Errorf("UpdateScope back to _Default: %v", err)
	}
	if err := c.DeleteTraceScope(ctx, &obspb.DeleteTraceScopeRequest{Name: ts.GetName()}); err != nil {
		t.Errorf("DeleteTraceScope: %v", err)
	}
	if _, err := c.GetTraceScope(ctx, &obspb.GetTraceScopeRequest{Name: ts.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTraceScope after delete: %v; want NOT_FOUND", err)
	}

	// A folder's settings, on its own service account.
	folder := fmt.Sprintf("folders/%d", time.Now().UnixNano()%1e9+1)
	if fs, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: folder + "/locations/global/settings"}); err != nil {
		// A folder the instance does not hold is refused, as REST does.
		if status.Code(err) != codes.NotFound && status.Code(err) != codes.PermissionDenied {
			t.Errorf("GetSettings of a folder: %v", err)
		}
	} else if !strings.HasPrefix(fs.GetServiceAccountId(), "service-folder-") {
		t.Errorf("GetSettings of a folder = %v", fs)
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
