//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.logging.v2.ConfigServiceV2/ListBuckets
// covers: google.logging.v2.ConfigServiceV2/GetBucket
// covers: google.logging.v2.ConfigServiceV2/CreateBucket
// covers: google.logging.v2.ConfigServiceV2/UpdateBucket
// covers: google.logging.v2.ConfigServiceV2/DeleteBucket
// covers: google.logging.v2.ConfigServiceV2/UndeleteBucket
// covers: google.logging.v2.ConfigServiceV2/ListViews
// covers: google.logging.v2.ConfigServiceV2/GetView
// covers: google.logging.v2.ConfigServiceV2/CreateView
// covers: google.logging.v2.ConfigServiceV2/UpdateView
// covers: google.logging.v2.ConfigServiceV2/DeleteView
//
// TestLoggingBucketsAndViews (#1264): through the official ConfigClient,
// a project lists its _Default (30 days) and _Required (400 days, locked)
// buckets; _Default's retention is changed, as Terraform's
// google_logging_project_bucket_config does, and _Required refuses changes;
// a user bucket is created, updated through an update_mask, given a view,
// deleted (DELETE_REQUESTED) and undeleted; a locked bucket refuses a
// retention change and deletion.
func TestLoggingBucketsAndViews(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	global := "projects/" + h.Project() + "/locations/global"
	byName := map[string]*loggingpb.LogBucket{}
	for it := c.ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: "projects/" + h.Project() + "/locations/-"}); ; {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListBuckets: %v", err)
		}
		byName[b.GetName()] = b
	}
	def, req := byName[global+"/buckets/_Default"], byName[global+"/buckets/_Required"]
	if def == nil || req == nil || !req.GetLocked() || req.GetRetentionDays() != 400 {
		t.Fatalf("ListBuckets = %v; want _Default and a locked _Required of 400 days", byName)
	}
	old := def.GetRetentionDays()
	upd, err := c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: global + "/buckets/_Default",
		Bucket: &loggingpb.LogBucket{RetentionDays: 45}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}})
	if err != nil || upd.GetRetentionDays() != 45 {
		t.Fatalf("UpdateBucket _Default = %v, %v", upd, err)
	}
	t.Cleanup(func() {
		_, _ = c.UpdateBucket(h.Context(), &loggingpb.UpdateBucketRequest{Name: global + "/buckets/_Default",
			Bucket: &loggingpb.LogBucket{RetentionDays: old}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}})
	})
	if got, err := c.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: global + "/buckets/_Default"}); err != nil || got.GetRetentionDays() != 45 {
		t.Errorf("GetBucket _Default = %v, %v; want 45 days", got, err)
	}
	_, err = c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: global + "/buckets/_Required",
		Bucket: &loggingpb.LogBucket{Description: "x"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	wantLoggingCode(t, "updating _Required", err, codes.FailedPrecondition)
	err = c.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: global + "/buckets/_Default"})
	wantLoggingCode(t, "deleting _Default", err, codes.FailedPrecondition)

	id := fmt.Sprint("b-", time.Now().UnixNano())
	parent := "projects/" + h.Project() + "/locations/us-central1"
	name := parent + "/buckets/" + id
	b, err := c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: id,
		Bucket: &loggingpb.LogBucket{Description: "compat", RetentionDays: 10}})
	if err != nil || b.GetName() != name || b.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE || b.GetCreateTime() == nil {
		t.Fatalf("CreateBucket = %v, %v", b, err)
	}
	_, err = c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: id})
	wantLoggingCode(t, "a duplicate bucket", err, codes.AlreadyExists)
	_, err = c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: "_reserved"})
	wantLoggingCode(t, "a reserved bucket ID", err, codes.InvalidArgument)
	upd, err = c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: name,
		Bucket: &loggingpb.LogBucket{Description: "changed", RetentionDays: 99}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || upd.GetDescription() != "changed" || upd.GetRetentionDays() != 10 {
		t.Errorf("UpdateBucket(description) = %v, %v; want only the description changed", upd, err)
	}

	v, err := c.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: name, ViewId: "stdout",
		View: &loggingpb.LogView{Filter: `LOG_ID("stdout")`}})
	if err != nil || v.GetName() != name+"/views/stdout" {
		t.Fatalf("CreateView = %v, %v", v, err)
	}
	views := map[string]bool{}
	for it := c.ListViews(ctx, &loggingpb.ListViewsRequest{Parent: name}); ; {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListViews: %v", err)
		}
		views[v.GetName()] = true
	}
	if !views[name+"/views/_AllLogs"] || !views[name+"/views/stdout"] {
		t.Errorf("ListViews = %v; want _AllLogs and stdout", views)
	}
	if v, err := c.UpdateView(ctx, &loggingpb.UpdateViewRequest{Name: name + "/views/stdout",
		View: &loggingpb.LogView{Description: "only stdout"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil || v.GetDescription() != "only stdout" || v.GetFilter() != `LOG_ID("stdout")` {
		t.Errorf("UpdateView = %v, %v", v, err)
	}
	if got, err := c.GetView(ctx, &loggingpb.GetViewRequest{Name: name + "/views/stdout"}); err != nil || got.GetDescription() != "only stdout" {
		t.Errorf("GetView = %v, %v", got, err)
	}
	if err := c.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: name + "/views/stdout"}); err != nil {
		t.Errorf("DeleteView: %v", err)
	}
	_, err = c.GetView(ctx, &loggingpb.GetViewRequest{Name: name + "/views/stdout"})
	wantLoggingCode(t, "a deleted view", err, codes.NotFound)

	if err := c.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: name}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if got, err := c.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name}); err != nil || got.GetLifecycleState() != loggingpb.LifecycleState_DELETE_REQUESTED {
		t.Errorf("GetBucket after delete = %v, %v; want DELETE_REQUESTED", got, err)
	}
	if err := c.UndeleteBucket(ctx, &loggingpb.UndeleteBucketRequest{Name: name}); err != nil {
		t.Errorf("UndeleteBucket: %v", err)
	}

	if _, err := c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: name,
		Bucket: &loggingpb.LogBucket{Locked: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"locked"}}}); err != nil {
		t.Fatalf("locking: %v", err)
	}
	_, err = c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: name,
		Bucket: &loggingpb.LogBucket{RetentionDays: 20}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}})
	wantLoggingCode(t, "a locked bucket's retention", err, codes.FailedPrecondition)
	err = c.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: name})
	wantLoggingCode(t, "deleting a locked bucket", err, codes.FailedPrecondition)
}
