package logging

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Buckets (#1264): the two built-ins, a user bucket's life cycle, the lock,
// and views.
func TestBucketsAndViews(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	now := time.Now()
	a.now = func() time.Time { return now }
	cs := a.ConfigServer()
	ctx := context.Background()
	code := func(err error) codes.Code { return status.Code(err) }
	mask := func(p ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: p} }
	const g = "projects/proj-1/locations/global"

	list, err := cs.ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: "projects/proj-1/locations/-"})
	if err != nil || len(list.GetBuckets()) != 2 || list.Buckets[0].GetName() != g+"/buckets/_Default" ||
		list.Buckets[0].GetRetentionDays() != 30 || !list.Buckets[1].GetLocked() || list.Buckets[1].GetRetentionDays() != 400 {
		t.Fatalf("ListBuckets of a new project = %v, %v; want _Default (30 days) and _Required (400, locked)", list, err)
	}
	if b, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: g + "/buckets/_Default",
		Bucket: &loggingpb.LogBucket{RetentionDays: 90}, UpdateMask: mask("retention_days")}); err != nil || b.GetRetentionDays() != 90 {
		t.Errorf("UpdateBucket _Default = %v, %v", b, err)
	}
	if b, _ := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: g + "/buckets/_Default"}); b.GetRetentionDays() != 90 {
		t.Errorf("_Default after update = %v", b)
	}
	for what, err := range map[string]error{
		"update _Required": func() error {
			_, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: g + "/buckets/_Required", Bucket: &loggingpb.LogBucket{}, UpdateMask: mask("description")})
			return err
		}(),
		"delete _Default": func() error {
			_, err := cs.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: g + "/buckets/_Default"})
			return err
		}(),
	} {
		if code(err) != codes.FailedPrecondition {
			t.Errorf("%s = %v; want FailedPrecondition", what, err)
		}
	}

	b, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/us-central1", BucketId: "mine",
		Bucket: &loggingpb.LogBucket{Description: "d"}})
	if err != nil || b.GetRetentionDays() != 30 || b.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE {
		t.Fatalf("CreateBucket = %v, %v", b, err)
	}
	name := "projects/proj-1/locations/us-central1/buckets/mine"
	if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/us-central1", BucketId: "mine"}); code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateBucket = %v", err)
	}
	for _, bad := range []*loggingpb.CreateBucketRequest{
		{Parent: g, BucketId: "_x"},
		{Parent: g, BucketId: "a b"},
		{Parent: g, BucketId: "x", Bucket: &loggingpb.LogBucket{RetentionDays: 4000}},
		{Parent: "projects/proj-1/locations/-", BucketId: "x"},
	} {
		if _, err := cs.CreateBucket(ctx, bad); code(err) != codes.InvalidArgument {
			t.Errorf("CreateBucket(%v) = %v; want InvalidArgument", bad, err)
		}
	}
	if l, _ := cs.ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: "projects/proj-1/locations/us-central1"}); len(l.GetBuckets()) != 1 {
		t.Errorf("ListBuckets of us-central1 = %v; want mine alone", l)
	}

	// Views: _AllLogs, a created one, update, delete.
	if _, err := cs.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: name, ViewId: "v1", View: &loggingpb.LogView{Filter: `LOG_ID("stdout")`}}); err != nil {
		t.Fatal(err)
	}
	vs, err := cs.ListViews(ctx, &loggingpb.ListViewsRequest{Parent: name})
	if err != nil || len(vs.GetViews()) != 2 {
		t.Errorf("ListViews = %v, %v; want _AllLogs and v1", vs, err)
	}
	if v, err := cs.UpdateView(ctx, &loggingpb.UpdateViewRequest{Name: name + "/views/v1", View: &loggingpb.LogView{Description: "x"}, UpdateMask: mask("description")}); err != nil || v.GetDescription() != "x" || v.GetFilter() == "" {
		t.Errorf("UpdateView = %v, %v", v, err)
	}
	if _, err := cs.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: name + "/views/v1"}); err != nil {
		t.Error(err)
	}
	if _, err := cs.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: g + "/buckets/_Default/views/_AllLogs"}); code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteView of _Default's _AllLogs = %v", err)
	}

	// Lock, then delete and undelete.
	if _, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: name, Bucket: &loggingpb.LogBucket{Locked: true}, UpdateMask: mask("locked")}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: name, Bucket: &loggingpb.LogBucket{RetentionDays: 60, Locked: true}, UpdateMask: mask("retention_days")}); code(err) != codes.FailedPrecondition {
		t.Errorf("retention change on a locked bucket = %v", err)
	}
	if _, err := cs.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: name}); code(err) != codes.FailedPrecondition {
		t.Errorf("delete of a locked bucket = %v", err)
	}
	if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: g, BucketId: "gone"}); err != nil {
		t.Fatal(err)
	}
	gone := g + "/buckets/gone"
	if _, err := cs.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: gone}); err != nil {
		t.Fatal(err)
	}
	if b, _ := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: gone}); b.GetLifecycleState() != loggingpb.LifecycleState_DELETE_REQUESTED {
		t.Errorf("a deleted bucket = %v; want DELETE_REQUESTED", b)
	}
	if _, err := cs.UndeleteBucket(ctx, &loggingpb.UndeleteBucketRequest{Name: gone}); err != nil {
		t.Error(err)
	}
	if _, err := cs.UndeleteBucket(ctx, &loggingpb.UndeleteBucketRequest{Name: gone}); code(err) != codes.FailedPrecondition {
		t.Errorf("undelete of an active bucket = %v", err)
	}
	if _, err := cs.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: gone}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8 * 24 * time.Hour)
	if _, err := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: gone}); code(err) != codes.NotFound {
		t.Errorf("a bucket deleted 8 days ago = %v; want NotFound", err)
	}
	if _, err := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "organizations/1/locations/global/buckets/x"}); code(err) != codes.Unimplemented {
		t.Errorf("an organization bucket = %v", err)
	}
}
