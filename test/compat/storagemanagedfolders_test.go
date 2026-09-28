//go:build compat

package compat

import (
	"context"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	storagev1 "google.golang.org/api/storage/v1"
)

// Managed folders (#828). The official Go client's storage.Client has no
// managed folder call, and the Storage Control client's REST transport speaks
// the v2 API rather than the JSON API, so Google's generated JSON API client,
// google.golang.org/api/storage/v1, drives each method here, as gcloud
// storage managed-folders and Terraform's google_storage_managed_folder do.

// managedFolderBucket is a bucket with uniform bucket-level access, which a
// managed folder requires, made through the official client. Its cleanup
// deletes the managed folders the test left, then the bucket.
func managedFolderBucket(t *testing.T, h *Harness, c *storage.Client, s *storagev1.Service) *storage.BucketHandle {
	t.Helper()
	bh := c.Bucket(h.Project() + "-mf")
	if err := bh.Create(h.Context(), h.Project(), &storage.BucketAttrs{
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = s.ManagedFolders.List(bh.BucketName()).Pages(ctx, func(p *storagev1.ManagedFolders) error {
			for _, f := range p.Items {
				_ = s.ManagedFolders.Delete(bh.BucketName(), f.Name).AllowNonEmpty(true).Context(ctx).Do()
			}
			return nil
		})
		emptyAndDelete(ctx, bh)
	})
	return bh
}

// TestStorageManagedFolderLifecycle: a managed folder inserted through the
// JSON API client reads back with its name ending in /, its bucket and a
// metageneration of 1; a second insert is 409; list returns it under its
// prefix and pages; delete with a stale ifMetagenerationMatch is 412, a
// non-empty one is 409 unless allowNonEmpty, which keeps the objects; a
// deleted one is 404; and a bucket holding a managed folder cannot be
// deleted.
//
// unverified: storage.managedFolders.insert 400: a bucket without uniform bucket-level access (the managed folders page states the requirement, not the status)
// unverified: storage.managedFolders.delete 409: a managed folder with objects or managed folders under it, without allowNonEmpty
// unverified: storage.buckets.delete 409: a bucket that holds a managed folder
// covers: storage.managedFolders.insert, storage.managedFolders.get, storage.managedFolders.list, storage.managedFolders.delete
func TestStorageManagedFolderLifecycle(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := managedFolderBucket(t, h, c, s)
	b := bh.BucketName()

	f, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: "logs/reports/"}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("managedFolders.insert: %v", err)
	}
	if f.Name != "logs/reports/" || f.Bucket != b || f.Metageneration != 1 || f.CreateTime == "" || f.Kind != "storage#managedFolder" {
		t.Errorf("insert returned %+v", f)
	}
	if _, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: "logs/reports/"}).Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("a second insert = %v; want 409", err)
	}
	got, err := s.ManagedFolders.Get(b, "logs/reports/").Context(ctx).Do()
	if err != nil || got.Name != "logs/reports/" || got.Metageneration != 1 {
		t.Fatalf("managedFolders.get = %+v, %v", got, err)
	}
	if _, err := s.ManagedFolders.Get(b, "absent/").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("get of an absent managed folder = %v; want 404", err)
	}

	for _, n := range []string{"logs/", "other/"} {
		if _, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: n}).Context(ctx).Do(); err != nil {
			t.Fatalf("insert %s: %v", n, err)
		}
	}
	var names []string
	err = s.ManagedFolders.List(b).Prefix("logs/").PageSize(1).Pages(ctx, func(p *storagev1.ManagedFolders) error {
		for _, f := range p.Items {
			names = append(names, f.Name)
		}
		return nil
	})
	if err != nil || len(names) != 2 || names[0] != "logs/" || names[1] != "logs/reports/" {
		t.Errorf("list prefix=logs/ one at a time = %v, %v", names, err)
	}

	putObject(t, ctx, bh.Object("logs/reports/r.txt"), "r")
	if err := s.ManagedFolders.Delete(b, "logs/reports/").IfMetagenerationMatch(2).Context(ctx).Do(); httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("delete with a stale ifMetagenerationMatch = %v; want 412", err)
	}
	if err := s.ManagedFolders.Delete(b, "logs/reports/").Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("delete of a managed folder with an object under it = %v; want 409", err)
	}
	if err := s.ManagedFolders.Delete(b, "logs/").Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("delete of a managed folder with a managed folder under it = %v; want 409", err)
	}
	if err := s.ManagedFolders.Delete(b, "logs/reports/").AllowNonEmpty(true).IfMetagenerationMatch(1).Context(ctx).Do(); err != nil {
		t.Fatalf("delete with allowNonEmpty: %v", err)
	}
	if _, err := bh.Object("logs/reports/r.txt").Attrs(ctx); err != nil {
		t.Errorf("the object under a deleted managed folder: %v", err)
	}
	if _, err := s.ManagedFolders.Get(b, "logs/reports/").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("get after delete = %v; want 404", err)
	}
	if err := s.ManagedFolders.Delete(b, "logs/reports/").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("a second delete = %v; want 404", err)
	}

	if err := bh.Object("logs/reports/r.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bh.Delete(ctx); httpCode(err) != http.StatusConflict {
		t.Errorf("delete of a bucket holding managed folders = %v; want 409", err)
	}

	// A bucket without uniform bucket-level access cannot hold one.
	plain := bucket(t, h, c)
	if _, err := s.ManagedFolders.Insert(plain.BucketName(), &storagev1.ManagedFolder{Name: "x/"}).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("insert into a bucket without uniform access = %v; want 400", err)
	}
}

// TestStorageManagedFolderIAM: a managed folder's policy, set through the
// JSON API client, reads back with its binding and leaves the bucket's alone;
// a stale etag is 412; testIamPermissions returns every permission asked
// for, as nothing is enforced (ADR-0006).
//
// unverified: storage.managedFolders.setIamPolicy 412: a stale etag (as for buckets.setIamPolicy)
// covers: storage.managedFolders.getIamPolicy, storage.managedFolders.setIamPolicy, storage.managedFolders.testIamPermissions
func TestStorageManagedFolderIAM(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := managedFolderBucket(t, h, c, s)
	b := bh.BucketName()
	if _, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: "team/"}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	p, err := s.ManagedFolders.GetIamPolicy(b, "team/").Context(ctx).Do()
	if err != nil {
		t.Fatalf("getIamPolicy: %v", err)
	}
	stale := p.Etag
	p.Bindings = append(p.Bindings, &storagev1.PolicyBindings{Role: "roles/storage.objectViewer", Members: []string{"user:dev@example.com"}})
	if _, err := s.ManagedFolders.SetIamPolicy(b, "team/", p).Context(ctx).Do(); err != nil {
		t.Fatalf("setIamPolicy: %v", err)
	}
	got, err := s.ManagedFolders.GetIamPolicy(b, "team/").Context(ctx).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:dev@example.com" {
		t.Errorf("policy after set = %+v, %v", got, err)
	}
	p.Etag = stale
	if _, err := s.ManagedFolders.SetIamPolicy(b, "team/", p).Context(ctx).Do(); httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("a set with a stale etag = %v; want 412", err)
	}
	bp, err := bh.IAM().Policy(ctx)
	if err != nil || bp.HasRole("user:dev@example.com", "roles/storage.objectViewer") {
		t.Errorf("the bucket's policy took the managed folder's binding: %v", err)
	}
	perms, err := s.ManagedFolders.TestIamPermissions(b, "team/", []string{"storage.objects.get", "storage.objects.list"}).Context(ctx).Do()
	if err != nil || len(perms.Permissions) != 2 {
		t.Errorf("testIamPermissions = %+v, %v; want every requested permission", perms, err)
	}
	if _, err := s.ManagedFolders.GetIamPolicy(b, "absent/").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("getIamPolicy of an absent managed folder = %v; want 404", err)
	}
}
