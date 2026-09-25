//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// The remaining bucket fields (#503, closing #374): every settable Bucket
// property is kept and echoed, or refused with 400 naming it, never
// accepted and dropped. fake-gcs-server drops most of them (#374), so these
// run against the builtin server.

// TestStorageBucketLabelsAndStorageClassRoundTrip: create and patch keep
// labels and the storage class, and a null label removes that one key.
// covers: storage.buckets.insert, storage.buckets.patch, storage.buckets.get
func TestStorageBucketLabelsAndStorageClassRoundTrip(t *testing.T) {
	builtinOnly(t, "labels and storageClass are dropped, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-labels")
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{StorageClass: "NEARLINE", Labels: map[string]string{"env": "dev", "team": "a"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	var u storage.BucketAttrsToUpdate
	u.SetLabel("tier", "gold")
	u.DeleteLabel("team")
	u.StorageClass = "COLDLINE"
	if _, err := bh.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.StorageClass != "COLDLINE" || a.Labels["env"] != "dev" || a.Labels["tier"] != "gold" || a.Labels["team"] != "" || len(a.Labels) != 2 {
		t.Errorf("after the patch: class %s, labels %v; want COLDLINE and env=dev, tier=gold", a.StorageClass, a.Labels)
	}
}

// TestStorageBucketFieldsNeverSilentlyDropped: each settable Bucket field is
// echoed on create, or refused with 400 naming it (the unit test of the same
// name also checks this list against the discovery schema).
func TestStorageBucketFieldsNeverSilentlyDropped(t *testing.T) {
	builtinOnly(t, "most bucket fields are dropped, #374")
	h := New(t)
	samples := map[string]string{
		"autoclass": `{"enabled":true}`, "billing": `{"requesterPays":true}`, "cors": `[{"origin":["*"],"method":["GET"]}]`,
		"customPlacementConfig": `{"dataLocations":["US-EAST1","US-WEST1"]}`, "defaultEventBasedHold": `true`,
		"encryption": `{"defaultKmsKeyName":"projects/p/locations/us/keyRings/r/cryptoKeys/k"}`, "hierarchicalNamespace": `{"enabled":true}`,
		"iamConfiguration": `{"uniformBucketLevelAccess":{"enabled":true}}`, "labels": `{"env":"dev"}`,
		"lifecycle": `{"rule":[{"action":{"type":"Delete"},"condition":{"age":30}}]}`, "location": `"EU"`,
		"logging": `{"logBucket":"logs"}`, "retentionPolicy": `{"retentionPeriod":"60"}`, "rpo": `"ASYNC_TURBO"`,
		"softDeletePolicy": `{"retentionDurationSeconds":"0"}`, "storageClass": `"NEARLINE"`, "versioning": `{"enabled":true}`,
		"website":  `{"mainPageSuffix":"index.html"}`,
		"ipFilter": `{"mode":"Enabled"}`, "acl": `[{"entity":"allUsers","role":"READER"}]`, "defaultObjectAcl": `[{"entity":"allUsers","role":"READER"}]`,
	}
	refused := map[string]bool{"ipFilter": true, "acl": true, "defaultObjectAcl": true}
	i := 0
	for k, v := range samples {
		i++
		name := fmt.Sprintf("%s-f%02d", h.Project(), i)
		code, body := rawStorage(t, h, "POST", "/storage/v1/b?project="+h.Project()+"&prettyPrint=false", fmt.Sprintf(`{"name":%q,%q:%s}`, name, k, v))
		if code == http.StatusOK {
			t.Cleanup(func() { rawStorage(t, h, "DELETE", "/storage/v1/b/"+name, "") })
		}
		switch {
		case refused[k] && (code != http.StatusBadRequest || !strings.Contains(body, k)):
			t.Errorf("%s = %d %s; want 400 naming it", k, code, body)
		case !refused[k] && (code != http.StatusOK || !strings.Contains(body, `"`+k+`":`)):
			t.Errorf("%s = %d %s; want it echoed", k, code, body)
		}
	}
}
