//go:build compat

package compat

import (
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// Lifecycle configuration (#501), to docs.cloud.google.com/storage/docs/lifecycle.
// fake-gcs-server drops lifecycle on a patch (#374), so these run against
// the builtin server.

// TestStorageLifecycleConfigRoundTrip: rules set through the official
// client come back as set.
// covers: storage.buckets.patch, storage.buckets.get
func TestStorageLifecycleConfigRoundTrip(t *testing.T) {
	builtinOnly(t, "lifecycle rules, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	want := storage.Lifecycle{Rules: []storage.LifecycleRule{
		{Action: storage.LifecycleAction{Type: storage.DeleteAction}, Condition: storage.LifecycleCondition{AgeInDays: 30, MatchesPrefix: []string{"tmp/"}}},
		{Action: storage.LifecycleAction{Type: storage.SetStorageClassAction, StorageClass: "COLDLINE"},
			Condition: storage.LifecycleCondition{MatchesStorageClasses: []string{"STANDARD"}, NumNewerVersions: 3, Liveness: storage.Archived}},
	}}
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{Lifecycle: &want}); err != nil {
		t.Fatal(err)
	}
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := a.Lifecycle.Rules
	if len(got) != 2 || got[0].Condition.AgeInDays != 30 || len(got[0].Condition.MatchesPrefix) != 1 ||
		got[1].Action.StorageClass != "COLDLINE" || got[1].Condition.NumNewerVersions != 3 || got[1].Condition.Liveness != storage.Archived {
		t.Errorf("lifecycle after a round trip = %+v", got)
	}
}

// TestStorageLifecycleUnknownCondition400: an unknown condition is 400
// naming it, and the old configuration stays.
// covers: storage.buckets.patch
func TestStorageLifecycleUnknownCondition400(t *testing.T) {
	builtinOnly(t, "lifecycle rules, #374")
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	path := "/storage/v1/b/" + bh.BucketName()
	if code, body := rawStorage(t, h, "PATCH", path, `{"lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":30}}]}}`); code != http.StatusOK {
		t.Fatalf("set = %d %s", code, body)
	}
	code, body := rawStorage(t, h, "PATCH", path, `{"lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"ageInWeeks":3}}]}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "ageInWeeks") {
		t.Errorf("an unknown condition = %d %s; want 400 naming it", code, body)
	}
	if _, body := rawStorage(t, h, "GET", path+"?prettyPrint=false", ""); !strings.Contains(body, `"age":30`) {
		t.Errorf("the old configuration did not stay: %s", body)
	}
}
