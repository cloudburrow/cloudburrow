package storagejobs

import (
	"testing"
	"time"

	sipb "cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
)

// A snapshot's activity rows count the requests of its window on the
// buckets and projects of its scope (#1979).
func TestActivityRows(t *testing.T) {
	rt := NewRuntime(nil, nil, "")
	s := &InsightsServer{}
	s.rt = rt
	now := time.Now()
	at := rt.Clock.Now()
	rt.RecordActivity([]ActivityRequest{
		{Time: now.Add(-time.Minute), Method: "storage.objects.insert", Bucket: "b", Object: "dir/a", ProjectNumber: 7, Status: 200,
			RequestBytes: 10, HTTPMethod: "POST", RequestID: "r1"},
		{Time: now.Add(-time.Minute), Method: "storage.objects.get", Bucket: "b", Object: "dir/a", ProjectNumber: 7, Status: 200,
			ResponseBytes: 10},
		{Time: now.Add(-time.Minute), Method: "storage.objects.list", Bucket: "b", ProjectNumber: 7, Status: 403},
		{Time: now.Add(-time.Minute), Method: "storage.objects.get", Bucket: "b", Object: "x", ProjectNumber: 7, Status: 429},
		{Time: now.Add(-time.Minute), Method: "storage.objects.delete", Bucket: "other", Object: "o", ProjectNumber: 7, Status: 204},
		{Time: now.Add(-time.Minute), Method: "storage.objects.get", Bucket: "c", Object: "o", ProjectNumber: 8, Status: 200},
		{Time: now.Add(-48 * time.Hour), Method: "storage.objects.get", Bucket: "b", Object: "old", ProjectNumber: 7, Status: 200},
	})
	sn := snapshot{Time: at, Rows: map[string][]map[string]any{}}
	buckets := map[string]map[string]any{"b": {"name": "b", "location": "US-CENTRAL1", "generation": float64(5)}}
	objects := map[string]map[string]any{"b/dir/a": {"size": "10", "generation": "3", "storageClass": "STANDARD"}}
	s.activityRows(&sn, at.Add(-snapshotInterval), at, "us-central1", map[int64]bool{7: true}, buckets, objects)

	if got := sn.Rows["project_activity"]; len(got) != 1 || got[0]["totalRequests"] != int64(5) || got[0]["totalWrites"] != int64(1) ||
		got[0]["totalDeletes"] != int64(1) || got[0]["totalLists"] != int64(1) || got[0]["count403"] != int64(1) ||
		got[0]["count429"] != int64(1) || got[0]["count2xx"] != int64(3) {
		t.Errorf("project_activity = %v", got)
	}
	ba := sn.Rows["bucket_activity"]
	if len(ba) != 1 || ba[0]["name"] != "b" || ba[0]["totalRequests"] != int64(4) || ba[0]["generation"] != int64(5) ||
		ba[0]["totalRequestBytes"] != int64(10) || ba[0]["totalResponseBytes"] != int64(10) {
		t.Fatalf("bucket_activity = %v", ba)
	}
	top := ba[0]["topPrefixesHighestOperations"].([]map[string]any)
	if len(top) != 2 || top[0]["prefix"] != "dir/" || top[0]["count"] != int64(2) || top[1]["prefix"] != "" {
		t.Errorf("topPrefixesHighestOperations = %v", top)
	}
	if e := ba[0]["topPrefixes429Errors"].([]map[string]any); len(e) != 1 || e[0]["prefix"] != "" {
		t.Errorf("topPrefixes429Errors = %v", e)
	}
	oe := sn.Rows["object_events"]
	if len(oe) != 2 {
		t.Fatalf("object_events = %v", oe)
	}
	for _, r := range oe {
		switch r["objectName"] {
		case "dir/a":
			if r["requestOperation"] != "write" || r["size"] != int64(10) || r["generation"] != int64(3) || r["requestId"] != "r1" {
				t.Errorf("write event = %v", r)
			}
		case "x":
			if r["errorReason"] != "rateLimitExceeded" || r["responseStatus"] != 429 || r["requestOperation"] != nil {
				t.Errorf("429 event = %v", r)
			}
		default:
			t.Errorf("unexpected event %v", r)
		}
	}
	if r := sn.Rows["bucket_region_activity"]; len(r) != 1 || r[0]["requestLocation"] != nil || r[0]["bucketLocation"] != "US-CENTRAL1" {
		t.Errorf("bucket_region_activity = %v", r)
	}
	codes := 0
	for _, e := range sn.Rows["events"] {
		if e["eventCode"] == "3" {
			codes++
		}
	}
	if codes != 4 {
		t.Errorf("eventCode 3 rows = %d", codes)
	}
}

// Activity rows are kept for activityDataRetentionPeriodDays, which is the
// dataset's retention when unset; a snapshot past the dataset's retention
// keeps its activity rows alone.
func TestActivityRetention(t *testing.T) {
	now := time.Now()
	ten := int32(10)
	c := &datasetConfig{Config: &sipb.DatasetConfig{RetentionPeriodDays: 2}, Activity: &ten}
	old := snapshot{Time: now.Add(-5 * snapshotInterval), Rows: map[string][]map[string]any{
		"bucket_attributes": {{"name": "b"}}, "bucket_activity": {{"name": "b"}},
		"events": {{"eventCode": "1"}, {"eventCode": "3"}}}}
	c.Snapshots = []snapshot{old, {Time: now.Add(-20 * snapshotInterval), Rows: map[string][]map[string]any{}}}
	c.prune(now)
	if len(c.Snapshots) != 1 {
		t.Fatalf("snapshots = %v", c.Snapshots)
	}
	sn := c.Snapshots[0]
	if len(sn.Rows["bucket_attributes"]) != 0 || len(sn.Rows["bucket_activity"]) != 1 || len(sn.Rows["events"]) != 1 {
		t.Errorf("pruned rows = %v", sn.Rows)
	}
	if !c.keeps("bucket_activity", nil, old, now) || c.keeps("bucket_attributes", nil, old, now) {
		t.Error("keeps: activity rows go by the activity retention, others by the dataset's")
	}
	zero := int32(0)
	c.Activity = &zero
	if c.keeps("bucket_activity", nil, snapshot{Time: now}, now) {
		t.Error("an activity retention of 0 keeps activity rows")
	}
	if activityDays(30, nil) != 30 {
		t.Error("an unset activity retention is not the dataset's")
	}
}
