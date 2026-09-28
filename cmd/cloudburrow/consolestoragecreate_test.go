package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// Create bucket sets the options the backend keeps at creation: labels, the
// default storage class, uniform bucket-level access, versioning, the soft
// delete retention and object retention, which can be enabled at creation
// only. The official client's Attrs reads each back as the form gave it
// (#852); the form's own defaults make a bucket the API's defaults would, with
// uniform access on as the documented form defaults it; a value the API
// refuses is refused with its message and creates nothing.
func TestStorageCreateBucketOptionsThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)

	if _, err := p.Create(ctx, "p", map[string]string{
		"name": "made-with-options", "labels": `{"team":"blue"}`, "storageClass": "NEARLINE",
		"uniformAccess": "true", "versioning": "true", "softDeleteSeconds": "864000", "objectRetention": "true",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	a, err := c.Bucket("made-with-options").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Labels["team"] != "blue" || a.StorageClass != "NEARLINE" || !a.UniformBucketLevelAccess.Enabled ||
		!a.VersioningEnabled || a.SoftDeletePolicy == nil || a.SoftDeletePolicy.RetentionDuration != 10*24*time.Hour ||
		a.ObjectRetentionMode != "Enabled" {
		t.Errorf("the bucket reads labels %v, class %s, uniform %v, versioning %v, soft delete %+v, object retention %q",
			a.Labels, a.StorageClass, a.UniformBucketLevelAccess.Enabled, a.VersioningEnabled, a.SoftDeletePolicy, a.ObjectRetentionMode)
	}

	// What the browser submits for the form untouched.
	_, fields := p.CreateForm()
	values := map[string]string{}
	for _, f := range fields {
		values[f.Name] = f.Default
	}
	values["name"] = "made-with-defaults"
	if _, err := p.Create(ctx, "p", values); err != nil {
		t.Fatalf("create with the form's defaults: %v", err)
	}
	d, err := c.Bucket("made-with-defaults").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.StorageClass != "STANDARD" || !d.UniformBucketLevelAccess.Enabled || d.VersioningEnabled ||
		d.SoftDeletePolicy.RetentionDuration != 7*24*time.Hour || d.ObjectRetentionMode == "Enabled" || len(d.Labels) != 0 {
		t.Errorf("the form's defaults made class %s, uniform %v, versioning %v, soft delete %+v, object retention %q, labels %v",
			d.StorageClass, d.UniformBucketLevelAccess.Enabled, d.VersioningEnabled, d.SoftDeletePolicy, d.ObjectRetentionMode, d.Labels)
	}

	_, err = p.Create(ctx, "p", map[string]string{"name": "too-short-soft-delete", "softDeleteSeconds": "5"})
	if err == nil || !strings.Contains(err.Error(), "softDeletePolicy.retentionDurationSeconds") {
		t.Errorf("a 5-second soft delete = %v; want buckets.insert's own refusal", err)
	}
	if _, err := c.Bucket("too-short-soft-delete").Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("a refused create left a bucket: %v", err)
	}
	if _, err := p.Create(ctx, "p", map[string]string{"name": "bad-labels", "labels": "not json"}); err == nil {
		t.Error("malformed labels were accepted")
	}
}
