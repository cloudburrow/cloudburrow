//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.logging.v2.ConfigServiceV2/CreateBucketAsync
// covers: google.logging.v2.ConfigServiceV2/UpdateBucketAsync
// covers: google.logging.v2.ConfigServiceV2/GetSettings
// covers: google.logging.v2.ConfigServiceV2/UpdateSettings
// covers: google.logging.v2.ConfigServiceV2/GetCmekSettings
// covers: google.logging.v2.ConfigServiceV2/UpdateCmekSettings
//
// TestLoggingLinksSettingsAndAsyncBuckets (#1292), through the official
// ConfigClient: CreateBucketAsync and UpdateBucketAsync answer operations
// whose Wait returns the bucket; the project's settings are read, changed
// and read back, and a KMS key is refused UNIMPLEMENTED. The links, which
// make BigQuery datasets, are TestLoggingLinksToBigQuery's, so this runs on
// an instance with Cloud Logging and no BigQuery (#1304).
func TestLoggingLinksSettingsAndAsyncBuckets(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envSinkLogging, EnvLogging)
	ctx := h.Context()
	c, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	project := "projects/" + h.Project()
	global := project + "/locations/global"
	id := fmt.Sprintf("links-%d", time.Now().UnixNano())

	op, err := c.CreateBucketAsync(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: id,
		Bucket: &loggingpb.LogBucket{Description: "async", RetentionDays: 7}})
	if err != nil {
		t.Fatalf("CreateBucketAsync: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetName() != global+"/buckets/"+id || b.GetRetentionDays() != 7 {
		t.Fatalf("CreateBucketAsync.Wait = %v, %v", b, err)
	}
	if md, err := op.Metadata(); err != nil || md.GetState() != loggingpb.OperationState_OPERATION_STATE_SUCCEEDED {
		t.Errorf("CreateBucketAsync metadata = %v, %v", md, err)
	}
	t.Cleanup(func() { _ = c.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: b.GetName()}) })

	uop, err := c.UpdateBucketAsync(ctx, &loggingpb.UpdateBucketRequest{Name: b.GetName(),
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"analytics_enabled"}}})
	if err != nil {
		t.Fatalf("UpdateBucketAsync: %v", err)
	}
	if ub, err := uop.Wait(ctx); err != nil || !ub.GetAnalyticsEnabled() {
		t.Fatalf("UpdateBucketAsync.Wait = %v, %v", ub, err)
	}

	st, err := c.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: project + "/settings"})
	if err != nil || st.GetName() != project+"/settings" || st.GetKmsServiceAccountId() == "" {
		t.Fatalf("GetSettings = %v, %v", st, err)
	}
	t.Cleanup(func() {
		_, _ = c.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: project + "/settings", Settings: st,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink"}}})
	})
	up, err := c.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: project + "/settings",
		Settings:   &loggingpb.Settings{StorageLocation: "europe-west1", DisableDefaultSink: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink"}}})
	if err != nil || up.GetStorageLocation() != "europe-west1" || !up.GetDisableDefaultSink() {
		t.Fatalf("UpdateSettings = %v, %v", up, err)
	}
	if got, err := c.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: project + "/settings"}); err != nil || got.GetStorageLocation() != "europe-west1" || !got.GetDisableDefaultSink() {
		t.Errorf("GetSettings after UpdateSettings = %v, %v", got, err)
	}
	if _, err := c.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: project + "/settings",
		Settings: &loggingpb.Settings{KmsKeyName: "projects/p/locations/global/keyRings/r/cryptoKeys/k"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateSettings with a KMS key = %v; want UNIMPLEMENTED", err)
	}
	cmek, err := c.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: project + "/cmekSettings"})
	if err != nil || cmek.GetKmsKeyName() != "" || cmek.GetServiceAccountId() == "" {
		t.Errorf("GetCmekSettings = %v, %v", cmek, err)
	}
	if _, err := c.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: project + "/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{KmsKeyName: "projects/p/locations/global/keyRings/r/cryptoKeys/k"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateCmekSettings with a key = %v; want UNIMPLEMENTED", err)
	}
	// Since #1395 the local organization's settings are served
	// (TestLoggingFolderOrganizationAndBillingParents); another is NOT_FOUND.
	if _, err := c.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: "organizations/1/settings"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSettings of an organization that does not exist = %v; want NOT_FOUND", err)
	}
}

// covers: google.logging.v2.ConfigServiceV2/CreateLink
// covers: google.logging.v2.ConfigServiceV2/GetLink
// covers: google.logging.v2.ConfigServiceV2/ListLinks
// covers: google.logging.v2.ConfigServiceV2/DeleteLink
//
// TestLoggingLinksToBigQuery (#1292), through the official ConfigClient: a
// link on a bucket without Log Analytics is FAILED_PRECONDITION, and on one
// with it is created, read, listed and deleted, each through its
// operation. A link makes a BigQuery dataset, so the test asks for
// BigQuery before it starts, and runs beside it under SINK_LOGGING in the
// emulators shard (#1304).
func TestLoggingLinksToBigQuery(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	logging := runShardEndpoint(h, envSinkLogging, EnvLogging)
	// The bucket's project must be the one the instance's BigQuery serves
	// (#1423).
	_, bqProject := bigqueryClient(t, h)
	c, err := vlogging.NewConfigClient(ctx, option.WithEndpoint(logging), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	project := "projects/" + bqProject
	global := project + "/locations/global"
	id := fmt.Sprintf("links-%d", time.Now().UnixNano())
	b, err := c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: id,
		Bucket: &loggingpb.LogBucket{Description: "links", RetentionDays: 7}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: b.GetName()}) })

	if _, err := c.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: b.GetName(), LinkId: "early"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateLink without Log Analytics = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: b.GetName(),
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"analytics_enabled"}}}); err != nil {
		t.Fatalf("UpdateBucket: %v", err)
	}

	lop, err := c.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: b.GetName(), LinkId: "my-link", Link: &loggingpb.Link{Description: "to BigQuery"}})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	link, err := lop.Wait(ctx)
	if err != nil || link.GetName() != b.GetName()+"/links/my-link" || link.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE ||
		link.GetBigqueryDataset().GetDatasetId() != "bigquery.googleapis.com/"+project+"/datasets/my_link" {
		t.Fatalf("CreateLink.Wait = %v, %v", link, err)
	}
	if _, err := c.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: b.GetName(), LinkId: "my-link"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateLink again = %v; want ALREADY_EXISTS", err)
	}
	if got, err := c.GetLink(ctx, &loggingpb.GetLinkRequest{Name: link.GetName()}); err != nil || got.GetDescription() != "to BigQuery" {
		t.Errorf("GetLink = %v, %v", got, err)
	}
	var listed []string
	for it := c.ListLinks(ctx, &loggingpb.ListLinksRequest{Parent: b.GetName()}); ; {
		l, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLinks: %v", err)
		}
		listed = append(listed, l.GetName())
	}
	if len(listed) != 1 || listed[0] != link.GetName() {
		t.Errorf("ListLinks = %v", listed)
	}
	dop, err := c.DeleteLink(ctx, &loggingpb.DeleteLinkRequest{Name: link.GetName()})
	if err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteLink.Wait: %v", err)
	}
	if _, err := c.GetLink(ctx, &loggingpb.GetLinkRequest{Name: link.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLink after DeleteLink = %v; want NOT_FOUND", err)
	}

}
