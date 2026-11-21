//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obs "cloud.google.com/go/observability/apiv1"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	tracev2 "cloud.google.com/go/trace/apiv2"
	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	observability "google.golang.org/api/observability/v1"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Observability API's settings, operations, buckets, datasets, views
// and links (#2117), through Google's official
// cloud.google.com/go/observability/apiv1 client over REST, on the Logging
// port.

func observabilityClient(t *testing.T, h *Harness, endpoint string) *obs.Client {
	t.Helper()
	if endpoint == "" {
		t.Fatal("no Logging endpoint")
	}
	c, err := obs.NewRESTClient(h.Context(), option.WithEndpoint("http://"+endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("observability.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// obsCode is an error's canonical code. The REST client's error carries
// only the HTTP status, which several codes share (400 is both
// INVALID_ARGUMENT and FAILED_PRECONDITION, 409 both ABORTED and
// ALREADY_EXISTS), so the code is read from the body's error.status.
func obsCode(err error) codes.Code {
	var he *googleapi.Error
	if errors.As(err, &he) {
		var body struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(he.Body), &body) == nil {
			for c := codes.OK; c <= codes.Unauthenticated; c++ {
				if strings.EqualFold(strings.ReplaceAll(c.String(), "_", ""), strings.ReplaceAll(body.Error.Status, "_", "")) {
					return c
				}
			}
		}
	}
	return status.Code(err)
}

// covers: observability.projects.locations.getSettings, observability.projects.locations.updateSettings, observability.projects.locations.operations.get, observability.projects.locations.operations.list, observability.projects.locations.operations.delete, observability.projects.locations.buckets.create
//
// TestObservabilitySettings2117: a project's settings in global start with
// no default storage location and its service account
// service-NUMBER@gcp-sa-observability.iam.gserviceaccount.com; UpdateSettings
// is an operation whose Wait returns the settings, kept for GetOperation and
// ListOperations; an unsupported location is INVALID_ARGUMENT, and a
// default key outside the default storage location FAILED_PRECONDITION,
// while a key in that location is taken.
func TestObservabilitySettings2117(t *testing.T) {
	h := New(t)
	logging := h.Endpoint(EnvLogging)
	h.Endpoint(EnvKMS)
	c := observabilityClient(t, h, logging)
	ctx := h.Context()
	project := fmt.Sprintf("obs2117-%d", time.Now().UnixNano()%1e9)
	global := "projects/" + project + "/locations/global"

	st, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"})
	if err != nil || st.GetName() != global+"/settings" || st.GetDefaultStorageLocation() != "" ||
		!strings.HasPrefix(st.GetServiceAccountId(), "service-") || !strings.HasSuffix(st.GetServiceAccountId(), "@gcp-sa-observability.iam.gserviceaccount.com") {
		t.Fatalf("GetSettings = %v, %v", st, err)
	}
	op, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: "us-central1"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	got, err := op.Wait(ctx)
	if err != nil || got.GetDefaultStorageLocation() != "us-central1" {
		t.Fatalf("UpdateSettings.Wait = %v, %v", got, err)
	}
	if md, err := op.Metadata(); err != nil || md.GetVerb() != "update" || md.GetTarget() != global+"/settings" || md.GetApiVersion() != "v1" {
		t.Errorf("UpdateSettings metadata = %v, %v", md, err)
	}
	if st, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"}); err != nil || st.GetDefaultStorageLocation() != "us-central1" {
		t.Errorf("GetSettings after update = %v, %v", st, err)
	}
	if o, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()}); err != nil || !o.GetDone() {
		t.Errorf("GetOperation = %v, %v", o, err)
	}
	var names []string
	for it := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global}); ; {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		names = append(names, o.GetName())
	}
	if len(names) != 1 || names[0] != op.Name() {
		t.Errorf("ListOperations = %v; want [%s]", names, op.Name())
	}

	if _, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: "moon1"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}}); obsCode(err) != codes.InvalidArgument {
		t.Errorf("an unsupported default storage location: %v; want INVALID_ARGUMENT", err)
	}
	keys := cmekKeys(t, h, kmsClients(t, h)["grpc"], "us-central1", "europe-west1")
	regional := "projects/" + project + "/locations/europe-west1/settings"
	if _, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: regional, KmsKeyName: keys["europe-west1/k"]},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}}); obsCode(err) != codes.FailedPrecondition {
		t.Errorf("a default key outside the default storage location: %v; want FAILED_PRECONDITION", err)
	}
	central := "projects/" + project + "/locations/us-central1/settings"
	kop, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: central, KmsKeyName: keys["us-central1/k"]},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}})
	if err != nil {
		t.Fatalf("UpdateSettings kms_key_name: %v", err)
	}
	if s, err := kop.Wait(ctx); err != nil || s.GetKmsKeyName() != keys["us-central1/k"] {
		t.Errorf("UpdateSettings kms_key_name.Wait = %v, %v", s, err)
	}
	// A bucket made in us-central1 takes the location's default key.
	bop, err := c.CreateBucket(ctx, &obspb.CreateBucketRequest{Parent: "projects/" + project + "/locations/us-central1", BucketId: "keyed"})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if b, err := bop.Wait(ctx); err != nil || b.GetCmekSettings().GetKmsKey() != keys["us-central1/k"] || b.GetCmekSettings().GetKmsKeyVersion() == "" ||
		b.GetCmekSettings().GetServiceAccountId() != st.GetServiceAccountId() {
		t.Errorf("CreateBucket under a default key = %v, %v", b, err)
	}
	if err := c.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.Name()}); err != nil {
		t.Errorf("DeleteOperation: %v", err)
	}
	if _, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()}); obsCode(err) != codes.NotFound {
		t.Errorf("GetOperation after DeleteOperation: %v; want NOT_FOUND", err)
	}
}

// covers: observability.projects.locations.buckets.create, observability.projects.locations.buckets.get, observability.projects.locations.buckets.list, observability.projects.locations.buckets.patch, observability.projects.locations.buckets.reprovision, observability.projects.locations.buckets.datasets.list, observability.projects.locations.buckets.datasets.get, observability.projects.locations.buckets.datasets.views.list, observability.projects.locations.buckets.datasets.views.get, observability.projects.locations.buckets.datasets.links.list
//
// TestObservabilityBuckets2117: a project has no _Trace until it is made;
// CreateBucket makes _Trace (an operation) with its Spans dataset and
// _AllSpans view, read back and listed; a second _Trace, a bucket in global
// and a reserved ID are refused; UpdateBucket changes the description;
// reprovision (the REST method the generated client lacks, through the
// discovery client) works once and is FAILED_PRECONDITION the second time;
// and on an instance without BigQuery a link is FAILED_PRECONDITION.
func TestObservabilityBuckets2117(t *testing.T) {
	h := New(t)
	logging := h.Endpoint(EnvLogging)
	c := observabilityClient(t, h, logging)
	ctx := h.Context()
	project := fmt.Sprintf("obsb2117-%d", time.Now().UnixNano()%1e9)
	us := "projects/" + project + "/locations/us"

	if _, err := c.GetBucket(ctx, &obspb.GetBucketRequest{Name: us + "/buckets/_Trace"}); obsCode(err) != codes.NotFound {
		t.Errorf("_Trace before any trace data: %v; want NOT_FOUND", err)
	}
	op, err := c.CreateBucket(ctx, &obspb.CreateBucketRequest{Parent: us, BucketId: "_Trace", Bucket: &obspb.Bucket{DisplayName: "Traces"}})
	if err != nil {
		t.Fatalf("CreateBucket _Trace: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetName() != us+"/buckets/_Trace" || b.GetCreateTime() == nil || b.GetDisplayName() != "Traces" {
		t.Fatalf("CreateBucket.Wait = %v, %v", b, err)
	}
	if got, err := c.GetBucket(ctx, &obspb.GetBucketRequest{Name: b.GetName()}); err != nil || got.GetDisplayName() != "Traces" {
		t.Errorf("GetBucket = %v, %v", got, err)
	}
	for what, req := range map[string]*obspb.CreateBucketRequest{
		"a second _Trace": {Parent: "projects/" + project + "/locations/eu", BucketId: "_Trace"},
	} {
		if _, err := c.CreateBucket(ctx, req); obsCode(err) != codes.AlreadyExists {
			t.Errorf("%s: %v; want ALREADY_EXISTS", what, err)
		}
	}
	for what, req := range map[string]*obspb.CreateBucketRequest{
		"a bucket in global": {Parent: "projects/" + project + "/locations/global", BucketId: "g"},
		"a reserved ID":      {Parent: us, BucketId: "_Mine"},
	} {
		if _, err := c.CreateBucket(ctx, req); obsCode(err) != codes.InvalidArgument {
			t.Errorf("%s: %v; want INVALID_ARGUMENT", what, err)
		}
	}
	var buckets []string
	for it := c.ListBuckets(ctx, &obspb.ListBucketsRequest{Parent: us}); ; {
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
	if got, err := uop.Wait(ctx); err != nil || got.GetDescription() != "spans" || got.GetDisplayName() != "Traces" {
		t.Errorf("UpdateBucket.Wait = %v, %v", got, err)
	}

	var datasets []string
	for it := c.ListDatasets(ctx, &obspb.ListDatasetsRequest{Parent: b.GetName()}); ; {
		d, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListDatasets: %v", err)
		}
		datasets = append(datasets, d.GetName())
	}
	spans := b.GetName() + "/datasets/Spans"
	if len(datasets) != 1 || datasets[0] != spans {
		t.Errorf("ListDatasets = %v; want [%s]", datasets, spans)
	}
	if d, err := c.GetDataset(ctx, &obspb.GetDatasetRequest{Name: spans}); err != nil || d.GetName() != spans {
		t.Errorf("GetDataset = %v, %v", d, err)
	}
	var views []string
	for it := c.ListViews(ctx, &obspb.ListViewsRequest{Parent: spans}); ; {
		v, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListViews: %v", err)
		}
		views = append(views, v.GetName())
	}
	if len(views) != 1 || views[0] != spans+"/views/_AllSpans" {
		t.Errorf("ListViews = %v", views)
	}
	if _, err := c.GetView(ctx, &obspb.GetViewRequest{Name: spans + "/views/_AllSpans"}); err != nil {
		t.Errorf("GetView _AllSpans: %v", err)
	}
	if _, err := c.GetView(ctx, &obspb.GetViewRequest{Name: spans + "/views/other"}); obsCode(err) != codes.NotFound {
		t.Errorf("GetView other: %v; want NOT_FOUND", err)
	}

	// reprovision: Google's generated discovery client, as the gapic one
	// has no method for it.
	svc := observabilityService(t, h)
	if _, err := svc.Projects.Locations.Buckets.Reprovision(b.GetName(), &observability.ReprovisionBucketRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	if _, err := svc.Projects.Locations.Buckets.Reprovision(b.GetName(), &observability.ReprovisionBucketRequest{}).Context(ctx).Do(); httpCode(err) != 400 ||
		!strings.Contains(err.Error(), "FAILED_PRECONDITION") && !strings.Contains(err.Error(), "failedPrecondition") && !strings.Contains(err.Error(), "already") {
		t.Errorf("a second reprovision: %v; want 400 FAILED_PRECONDITION", err)
	}

	if _, err := c.CreateLink(ctx, &obspb.CreateLinkRequest{Parent: spans, LinkId: "bad-id"}); obsCode(err) != codes.InvalidArgument {
		t.Errorf("CreateLink with a hyphen: %v; want INVALID_ARGUMENT", err)
	}
	it := c.ListLinks(ctx, &obspb.ListLinksRequest{Parent: spans})
	if _, err := it.Next(); !errors.Is(err, iterator.Done) {
		t.Errorf("ListLinks before any link: %v; want none", err)
	}
	// The links themselves make BigQuery datasets: TestObservabilityLinks2117.
}

// covers: observability.projects.locations.buckets.datasets.links.create, observability.projects.locations.buckets.datasets.links.get, observability.projects.locations.buckets.datasets.links.patch, observability.projects.locations.buckets.datasets.links.delete, observability.projects.locations.buckets.datasets.links.list, observability.projects.locations.buckets.list
//
// TestObservabilityLinks2117: a link on the _Trace bucket's Spans dataset
// makes a BigQuery dataset named after it with the _AllSpans view, read
// through the official BigQuery client; a second link on the dataset is
// FAILED_PRECONDITION; the link is read, listed, updated and deleted (each
// through its operation), and deleting it deletes the dataset. It runs
// beside BigQuery under SINK_LOGGING, as TestLoggingLinksToBigQuery does.
func TestObservabilityLinks2117(t *testing.T) {
	h := New(t)
	logging := runShardEndpoint(h, envSinkLogging, EnvLogging)
	bq, project := bigqueryClient(t, h)
	c := observabilityClient(t, h, logging)
	ctx := h.Context()

	// The project's _Trace, made here or by an earlier run.
	us := "projects/" + project + "/locations/us"
	if op, err := c.CreateBucket(ctx, &obspb.CreateBucketRequest{Parent: us, BucketId: "_Trace"}); err == nil {
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	} else if obsCode(err) != codes.AlreadyExists {
		t.Fatalf("CreateBucket _Trace: %v", err)
	}
	var trace string
	for it := c.ListBuckets(ctx, &obspb.ListBucketsRequest{Parent: "projects/" + project + "/locations/-"}); ; {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListBuckets: %v", err)
		}
		if strings.HasSuffix(x.GetName(), "/buckets/_Trace") {
			trace = x.GetName()
		}
	}
	if trace == "" {
		t.Fatal("no _Trace bucket")
	}
	spans := trace + "/datasets/Spans"
	id := fmt.Sprintf("spans_%d", time.Now().UnixNano()%1e9)
	op, err := c.CreateLink(ctx, &obspb.CreateLinkRequest{Parent: spans, LinkId: id, Link: &obspb.Link{Description: "to BigQuery"}})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	l, err := op.Wait(ctx)
	if err != nil || l.GetName() != spans+"/links/"+id || l.GetCreateTime() == nil {
		t.Fatalf("CreateLink.Wait = %v, %v", l, err)
	}
	t.Cleanup(func() {
		if dop, err := c.DeleteLink(context.Background(), &obspb.DeleteLinkRequest{Name: l.GetName()}); err == nil {
			_ = dop.Wait(context.Background())
		}
	})
	if _, err := bq.Dataset(id).Table("_AllSpans").Metadata(ctx); err != nil {
		t.Errorf("the linked dataset's _AllSpans view: %v", err)
	}
	// A span written to Cloud Trace afterwards is a row of the linked
	// dataset (#2118), when the shard serves Cloud Trace.
	if strings.TrimSpace(os.Getenv(EnvTrace)) != "" {
		w, err := tracev2.NewClient(ctx, traceGRPCOpts(h.Endpoint(EnvTrace))...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		traceID, spanID := newTraceID("obslink"), "00f067aa0ba90201"
		if err := w.BatchWriteSpans(ctx, &cloudtracev2pb.BatchWriteSpansRequest{Name: "projects/" + project,
			Spans: []*cloudtracev2pb.Span{v2Span(project, traceID, spanID, "", "linked span", time.Now(), time.Millisecond, nil)}}); err != nil {
			t.Fatalf("BatchWriteSpans: %v", err)
		}
		q := "SELECT name, trace_id FROM `" + project + "." + id + "._AllSpans` WHERE span_id = '" + spanID + "' AND trace_id = '" + traceID + "'"
		var got []map[string]bigquery.Value
		for deadline := time.Now().Add(30 * time.Second); len(got) == 0 && time.Now().Before(deadline); time.Sleep(time.Second) {
			it, err := bq.Query(q).Read(ctx)
			if err != nil {
				continue
			}
			for {
				var r map[string]bigquery.Value
				if err := it.Next(&r); err != nil {
					break
				}
				got = append(got, r)
			}
		}
		if len(got) != 1 || got[0]["name"] != "linked span" {
			t.Errorf("%s = %v; want the span written to Cloud Trace", q, got)
		}
	}
	if _, err := c.CreateLink(ctx, &obspb.CreateLinkRequest{Parent: spans, LinkId: id + "_2"}); obsCode(err) != codes.FailedPrecondition {
		t.Errorf("a second link on the dataset: %v; want FAILED_PRECONDITION", err)
	}
	if got, err := c.GetLink(ctx, &obspb.GetLinkRequest{Name: l.GetName()}); err != nil || got.GetDescription() != "to BigQuery" {
		t.Errorf("GetLink = %v, %v", got, err)
	}
	uop, err := c.UpdateLink(ctx, &obspb.UpdateLinkRequest{Link: &obspb.Link{Name: l.GetName(), DisplayName: "Spans in BigQuery"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateLink: %v", err)
	}
	if got, err := uop.Wait(ctx); err != nil || got.GetDisplayName() != "Spans in BigQuery" {
		t.Errorf("UpdateLink.Wait = %v, %v", got, err)
	}
	var links []string
	for it := c.ListLinks(ctx, &obspb.ListLinksRequest{Parent: spans}); ; {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListLinks: %v", err)
		}
		links = append(links, x.GetName())
	}
	if len(links) != 1 || links[0] != l.GetName() {
		t.Errorf("ListLinks = %v", links)
	}
	dop, err := c.DeleteLink(ctx, &obspb.DeleteLinkRequest{Name: l.GetName()})
	if err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteLink.Wait: %v", err)
	}
	if _, err := c.GetLink(ctx, &obspb.GetLinkRequest{Name: l.GetName()}); obsCode(err) != codes.NotFound {
		t.Errorf("GetLink after delete: %v; want NOT_FOUND", err)
	}
	if _, err := bq.Dataset(id).Metadata(ctx); err == nil {
		t.Error("the linked BigQuery dataset outlived its link")
	}
}

// gcloudValue is a value gcloud printed: its last line, as gcloud also
// writes notes such as "Parsed [location] resource" to the merged output.
func gcloudValue(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// TestGcloudObservability2117: gcloud observability settings describe and
// update --default-storage-location, buckets create, describe, list and
// update, buckets datasets list and views list, and buckets datasets links
// list, through the harness's gcloud session (egress blocked).
func TestGcloudObservability2117(t *testing.T) {
	h := New(t)
	_ = observabilityService(t, h) // asks for the Logging endpoint first
	g := newGcloudSession(t, h)
	project := fmt.Sprintf("obsg2117-%d", time.Now().UnixNano()%1e9)
	p := "--project=" + project

	if got := g.must("observability", "settings", "describe", "--location=global", p, "--format=value(serviceAccountId)"); !strings.Contains(got, "@gcp-sa-observability.iam.gserviceaccount.com") {
		t.Errorf("settings describe = %q", got)
	}
	g.must("observability", "settings", "update", "--location=global", p, "--update-mask=default_storage_location", "--default-storage-location=eu")
	if got := g.must("observability", "settings", "describe", "--location=global", p, "--format=value(defaultStorageLocation)"); gcloudValue(got) != "eu" {
		t.Errorf("settings describe after update = %q", got)
	}
	g.must("observability", "buckets", "create", "_Trace", "--location=eu", p, "--display-name=Traces")
	if got := g.must("observability", "buckets", "describe", "_Trace", "--location=eu", p, "--format=value(displayName)"); gcloudValue(got) != "Traces" {
		t.Errorf("buckets describe = %q", got)
	}
	if got := g.must("observability", "buckets", "list", "--location=eu", p, "--format=value(name)"); !strings.Contains(got, "/buckets/_Trace") {
		t.Errorf("buckets list = %q", got)
	}
	g.must("observability", "buckets", "update", "_Trace", "--location=eu", p, "--description=from gcloud")
	if got := g.must("observability", "buckets", "describe", "_Trace", "--location=eu", p, "--format=value(description)"); gcloudValue(got) != "from gcloud" {
		t.Errorf("buckets describe after update = %q", got)
	}
	if got := g.must("observability", "buckets", "datasets", "list", "--bucket=_Trace", "--location=eu", p, "--format=value(name)"); !strings.Contains(got, "/datasets/Spans") {
		t.Errorf("datasets list = %q", got)
	}
	if got := g.must("observability", "buckets", "datasets", "views", "list", "--dataset=Spans", "--bucket=_Trace", "--location=eu", p, "--format=value(name)"); !strings.Contains(got, "/views/_AllSpans") {
		t.Errorf("views list = %q", got)
	}
	g.must("observability", "buckets", "datasets", "links", "list", "--dataset=Spans", "--bucket=_Trace", "--location=eu", p)
}

// covers: observability.folders.locations.getSettings, observability.folders.locations.updateSettings, observability.folders.locations.operations.get, observability.folders.locations.operations.list, observability.folders.locations.operations.cancel, observability.folders.locations.operations.delete, observability.folders.locations.get, observability.folders.locations.list
// covers: observability.organizations.locations.getSettings, observability.organizations.locations.updateSettings, observability.organizations.locations.operations.get, observability.organizations.locations.operations.list, observability.organizations.locations.operations.cancel, observability.organizations.locations.operations.delete, observability.organizations.locations.get, observability.organizations.locations.list
// covers: observability.projects.locations.get, observability.projects.locations.list, observability.projects.locations.operations.cancel
//
// TestObservabilityParents2117: a folder Resource Manager holds and the
// organization have settings of their own, with their service accounts
// (service-org-ORGANIZATION_ID@gcp-sa-observability for the organization),
// updated through operations read back, listed, cancelled (a done
// operation is left as it is) and deleted; a folder that does not exist is
// NOT_FOUND; and every parent lists and reads its locations: global and the
// observability bucket locations.
func TestObservabilityParents2117(t *testing.T) {
	h := New(t)
	folder, child := loggingParentsFixture(t, h, "obs-")
	c := observabilityClient(t, h, h.Endpoint(EnvLogging))
	ctx := h.Context()
	for _, parent := range []string{folder, localOrg, "projects/" + child} {
		global := parent + "/locations/global"
		st, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"})
		if err != nil || !strings.HasSuffix(st.GetServiceAccountId(), "@gcp-sa-observability.iam.gserviceaccount.com") {
			t.Fatalf("GetSettings(%s) = %v, %v", parent, st, err)
		}
		if parent == localOrg && st.GetServiceAccountId() != "service-org-"+strings.TrimPrefix(localOrg, "organizations/")+"@gcp-sa-observability.iam.gserviceaccount.com" {
			t.Errorf("the organization's service account = %q", st.GetServiceAccountId())
		}
		before := st.GetDefaultStorageLocation()
		op, err := c.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: "europe-west1"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}})
		if err != nil {
			t.Fatalf("UpdateSettings(%s): %v", parent, err)
		}
		t.Cleanup(func() {
			if op, err := c.UpdateSettings(context.Background(), &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: before},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		if got, err := op.Wait(ctx); err != nil || got.GetDefaultStorageLocation() != "europe-west1" {
			t.Errorf("UpdateSettings(%s).Wait = %v, %v", parent, got, err)
		}
		if o, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()}); err != nil || !o.GetDone() {
			t.Errorf("GetOperation(%s) = %v, %v", op.Name(), o, err)
		}
		found := false
		for it := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global}); ; {
			o, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("ListOperations(%s): %v", global, err)
			}
			found = found || o.GetName() == op.Name()
		}
		if !found {
			t.Errorf("ListOperations(%s) lacks %s", global, op.Name())
		}
		if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.Name()}); err != nil {
			t.Errorf("CancelOperation(%s): %v", op.Name(), err)
		}
		if err := c.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.Name()}); err != nil {
			t.Errorf("DeleteOperation(%s): %v", op.Name(), err)
		}
		if l, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent + "/locations/us"}); err != nil || l.GetLocationId() != "us" {
			t.Errorf("GetLocation(%s/locations/us) = %v, %v", parent, l, err)
		}
		n := 0
		for it := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: parent}); ; {
			_, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("ListLocations(%s): %v", parent, err)
			}
			n++
		}
		if n < 40 {
			t.Errorf("ListLocations(%s) = %d locations", parent, n)
		}
	}
	if _, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: "folders/999999999/locations/global/settings"}); obsCode(err) != codes.NotFound {
		t.Errorf("a folder that does not exist: %v; want NOT_FOUND", err)
	}
}
