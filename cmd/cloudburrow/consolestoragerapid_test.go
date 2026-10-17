package main

import (
	"encoding/json"
	"testing"
	"time"
)

// TestConsoleRapidBucketForm: the Create form's zone makes a Rapid storage
// bucket's buckets.insert body, the configuration tab shows the placement,
// and an open appendable object reads as not finalized (#1443).
func TestConsoleRapidBucketForm(t *testing.T) {
	raw, _, err := bucketCreateRequest("b", map[string]string{"storageClass": "RAPID", "zone": "us-central1-a"})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Location, StorageClass string
		CustomPlacementConfig  struct{ DataLocations []string }
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Location != "us-central1" || body.StorageClass != "RAPID" ||
		len(body.CustomPlacementConfig.DataLocations) != 1 || body.CustomPlacementConfig.DataLocations[0] != "us-central1-a" {
		t.Errorf("buckets.insert body is %s", raw)
	}
	if raw, _, _ := bucketCreateRequest("b", map[string]string{}); json.Valid(raw) && string(raw) != `{"name":"b"}` {
		t.Errorf("no zone sends %s", raw)
	}
	var b bucketMeta
	if err := json.Unmarshal([]byte(`{"customPlacementConfig":{"dataLocations":["us-central1-a"]}}`), &b); err != nil || b.placement() != "us-central1-a" {
		t.Errorf("placement = %q, %v", b.placement(), err)
	}
	if (bucketMeta{}).placement() != "None" {
		t.Errorf("no placement reads %q", (bucketMeta{}).placement())
	}
	if finalizedText(time.Time{}) == "" || finalizedText(time.Unix(1, 0)) == finalizedText(time.Time{}) {
		t.Error("finalizedText does not tell an open object from a finalized one")
	}
}
