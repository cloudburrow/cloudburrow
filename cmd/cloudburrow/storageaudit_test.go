package main

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Cloud Storage's audit entries (#1979): Admin Activity always, Data
// Access only of the types the project's policy enables, none by default.
func TestStorageAuditEntries(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.EnsureExists("p"); err != nil {
		t.Fatal(err)
	}
	var got []*loggingpb.LogEntry
	a := &storageAudit{registry: func() *resourcemanager.Registry { return reg },
		write: func(_ context.Context, e []*loggingpb.LogEntry) error { got = append(got, e...); return nil }}
	now := time.Now()
	reqs := []gcsbuiltin.Request{
		{Time: now, Method: "storage.objects.get", Permissions: []string{"storage.objects.get"}, Bucket: "b", Object: "o",
			Project: "p", Location: "US-CENTRAL1", Status: 200, RequestID: "r1"},
		{Time: now, Method: "storage.objects.insert", Permissions: []string{"storage.objects.create"}, Bucket: "b", Object: "n",
			Project: "p", Status: 200, RequestID: "r2"},
		{Time: now, Method: "storage.buckets.get", Permissions: []string{"storage.buckets.get"}, Bucket: "b", Project: "p", Status: 200,
			RequestID: "r3"},
		{Time: now, Method: "storage.buckets.patch", Permissions: []string{"storage.buckets.update"}, Bucket: "b", Project: "p",
			Status: 403, RequestID: "r4"},
		{Time: now, Method: "storage.objects.copy", Permissions: []string{"storage.objects.get", "storage.objects.create"}, Bucket: "b",
			Object: "o", DestBucket: "d", DestObject: "c", Project: "p", Status: 200, RequestID: "r5"},
	}
	a.handle(context.Background(), reqs)
	if len(got) != 1 || got[0].GetLogName() != "projects/p/logs/cloudaudit.googleapis.com%2Factivity" {
		t.Fatalf("with no audit config: %v", got)
	}
	var al auditpb.AuditLog
	if err := got[0].GetProtoPayload().UnmarshalTo(&al); err != nil {
		t.Fatal(err)
	}
	if al.GetMethodName() != "storage.buckets.patch" || al.GetStatus().GetCode() != 7 || al.GetAuthorizationInfo()[0].GetGranted() {
		t.Errorf("admin activity = %v", &al)
	}

	_, err := reg.SetIAMPolicy("projects/p", &iampb.SetIamPolicyRequest{Resource: "projects/p", Policy: &iampb.Policy{
		AuditConfigs: []*iampb.AuditConfig{{Service: "storage.googleapis.com", AuditLogConfigs: []*iampb.AuditLogConfig{
			{LogType: iampb.AuditLogConfig_DATA_READ}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag", "audit_configs"}}})
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	a.handle(context.Background(), reqs)
	// DATA_READ: the get and the copy's source read; and the patch's Admin
	// Activity entry.
	var reads []string
	for _, e := range got {
		if e.GetLogName() == "projects/p/logs/cloudaudit.googleapis.com%2Fdata_access" {
			var al auditpb.AuditLog
			_ = e.GetProtoPayload().UnmarshalTo(&al)
			reads = append(reads, al.GetResourceName())
			if e.GetResource().GetType() != "gcs_bucket" || e.GetResource().GetLabels()["bucket_name"] != "b" {
				t.Errorf("resource = %v", e.GetResource())
			}
		}
	}
	if len(got) != 3 || len(reads) != 2 || reads[0] != "projects/_/buckets/b/objects/o" {
		t.Errorf("with DATA_READ: %d entries, reads %v", len(got), reads)
	}

	_, err = reg.SetIAMPolicy("projects/p", &iampb.SetIamPolicyRequest{Resource: "projects/p", Policy: &iampb.Policy{
		AuditConfigs: []*iampb.AuditConfig{{Service: "allServices", AuditLogConfigs: []*iampb.AuditLogConfig{
			{LogType: iampb.AuditLogConfig_DATA_WRITE}, {LogType: iampb.AuditLogConfig_ADMIN_READ}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"audit_configs"}}})
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	a.handle(context.Background(), reqs)
	// DATA_WRITE: the insert and the copy's destination; ADMIN_READ: the
	// bucket get; and the Admin Activity entry.
	if len(got) != 4 {
		t.Fatalf("with allServices DATA_WRITE and ADMIN_READ: %d entries", len(got))
	}
	var dest bool
	for _, e := range got {
		var al auditpb.AuditLog
		_ = e.GetProtoPayload().UnmarshalTo(&al)
		dest = dest || al.GetResourceName() == "projects/_/buckets/d/objects/c"
	}
	if !dest {
		t.Error("the copy's destination write has no entry")
	}
}
