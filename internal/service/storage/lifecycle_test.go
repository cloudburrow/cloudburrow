package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// clockServer is a server on a FakeClock with one bucket made from body.
func clockServer(t *testing.T, start time.Time, body string) (*Server, *sched.FakeClock, string) {
	t.Helper()
	clock := sched.NewFakeClock(start)
	s, err := NewServer(Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	if code, resp := raw(t, "POST", h.URL+"/storage/v1/b?project=p", body); code != 200 {
		t.Fatalf("create bucket = %d %s", code, resp)
	}
	return s, clock, h.URL
}

func put(t *testing.T, base, bucket, name, data string) {
	t.Helper()
	if code, body := raw(t, "POST", base+"/upload/storage/v1/b/"+bucket+"/o?uploadType=media&name="+name, data); code != 200 {
		t.Fatalf("upload %s = %d %s", name, code, body)
	}
}

func exists(t *testing.T, base, bucket, name string) bool {
	t.Helper()
	code, _ := raw(t, "GET", base+"/storage/v1/b/"+bucket+"/o/"+name, "")
	return code == 200
}

func runLifecycle(t *testing.T, base string) LifecycleResult {
	t.Helper()
	code, body := raw(t, "POST", base+lifecyclePath, "")
	var res LifecycleResult
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil {
		t.Fatalf("lifecycle trigger = %d %s", code, body)
	}
	return res
}

// Age 0 is met at midnight UTC after creation; age N, N days after it
// (lifecycle docs, "age").
func TestLifecycleAgeDeletesAtUTCMidnight(t *testing.T) {
	_, clock, base := clockServer(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		`{"name":"aged","lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":0,"matchesPrefix":["zero/"]}},`+
			`{"action":{"type":"Delete"},"condition":{"age":1,"matchesPrefix":["one/"]}}]}}`)
	put(t, base, "aged", "zero%2Fo", "x")
	put(t, base, "aged", "one%2Fo", "x")
	clock.Advance(14*time.Hour - time.Second) // 23:59:59
	runLifecycle(t, base)
	if !exists(t, base, "aged", "zero%2Fo") {
		t.Fatal("age 0 deleted before midnight UTC")
	}
	clock.Advance(time.Second) // midnight
	if res := runLifecycle(t, base); res.Deleted != 1 || exists(t, base, "aged", "zero%2Fo") {
		t.Errorf("age 0 at midnight UTC: %+v", res)
	}
	clock.Advance(10*time.Hour - time.Second) // a second before 24 hours
	runLifecycle(t, base)
	if !exists(t, base, "aged", "one%2Fo") {
		t.Fatal("age 1 deleted before a full day")
	}
	clock.Advance(time.Second)
	if res := runLifecycle(t, base); res.Deleted != 1 || exists(t, base, "aged", "one%2Fo") {
		t.Errorf("age 1 after a day: %+v", res)
	}
}

// A hold, or an unmet retention period, keeps Delete from taking effect.
func TestLifecycleHoldBlocksDelete(t *testing.T) {
	_, clock, base := clockServer(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		`{"name":"held","lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":1}}]}}`)
	put(t, base, "held", "o", "x")
	if code, body := raw(t, "PATCH", base+"/storage/v1/b/held/o/o", `{"temporaryHold":true}`); code != 200 {
		t.Fatal(body)
	}
	clock.Advance(48 * time.Hour)
	if res := runLifecycle(t, base); res.Deleted != 0 || res.Protected != 1 || !exists(t, base, "held", "o") {
		t.Errorf("under a hold: %+v", res)
	}
	raw(t, "PATCH", base+"/storage/v1/b/held/o/o", `{"temporaryHold":false}`)
	if code, body := raw(t, "PATCH", base+"/storage/v1/b/held", `{"retentionPolicy":{"retentionPeriod":"864000"}}`); code != 200 {
		t.Fatal(body)
	}
	if res := runLifecycle(t, base); res.Deleted != 0 || res.Protected != 1 {
		t.Errorf("under an unmet retention period: %+v", res)
	}
	clock.Advance(10 * 24 * time.Hour)
	if res := runLifecycle(t, base); res.Deleted != 1 || exists(t, base, "held", "o") {
		t.Errorf("after the retention period: %+v", res)
	}
}

// numNewerVersions N deletes a version with at least N newer ones, the live
// one included; deleting a live version makes it noncurrent.
func TestLifecycleNumNewerVersions(t *testing.T) {
	_, _, base := clockServer(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		`{"name":"vers","versioning":{"enabled":true},"lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"numNewerVersions":2}}]}}`)
	for _, d := range []string{"1", "2", "3", "4"} {
		put(t, base, "vers", "o", d)
	}
	if res := runLifecycle(t, base); res.Deleted != 2 {
		t.Errorf("deleted %+v, want the 2 versions with 2 or more newer", res)
	}
	_, body := raw(t, "GET", base+"/storage/v1/b/vers/o?versions=true", "")
	if n := strings.Count(body, `"generation"`); n != 2 {
		t.Errorf("versions left = %d, want 2: %s", n, body)
	}
}

// Delete takes precedence over SetStorageClass, and among SetStorageClass
// rules the cheapest class wins.
func TestLifecycleDeleteBeatsSetStorageClass(t *testing.T) {
	_, clock, base := clockServer(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		`{"name":"classes","lifecycle":{"rule":[`+
			`{"action":{"type":"SetStorageClass","storageClass":"NEARLINE"},"condition":{"age":1}},`+
			`{"action":{"type":"SetStorageClass","storageClass":"COLDLINE"},"condition":{"age":1}},`+
			`{"action":{"type":"Delete"},"condition":{"age":1,"matchesSuffix":[".tmp"]}}]}}`)
	put(t, base, "classes", "keep.dat", "x")
	put(t, base, "classes", "scratch.tmp", "x")
	clock.Advance(25 * time.Hour)
	if res := runLifecycle(t, base); res.Deleted != 1 || res.ClassChanged != 1 {
		t.Errorf("result = %+v", res)
	}
	if exists(t, base, "classes", "scratch.tmp") {
		t.Error("Delete did not beat SetStorageClass")
	}
	_, body := raw(t, "GET", base+"/storage/v1/b/classes/o/keep.dat?prettyPrint=false", "")
	if !strings.Contains(body, `"storageClass":"COLDLINE"`) || !strings.Contains(body, `"timeStorageClassUpdated":"2026-09-26T11:00:00.000Z"`) {
		t.Errorf("keep.dat = %s; want COLDLINE, the cheaper class, changed now", body)
	}
	if res := runLifecycle(t, base); res.ClassChanged != 0 {
		t.Errorf("a second pass moved the class again: %+v", res)
	}
}

// Conditions combine with AND.
func TestLifecycleConditionsAnd(t *testing.T) {
	_, clock, base := clockServer(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		`{"name":"and","lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":1,"matchesPrefix":["logs/"],"sizeAboveBytes":2}}]}}`)
	put(t, base, "and", "logs%2Fbig", "xxxx")
	put(t, base, "and", "logs%2Fsmall", "x")
	put(t, base, "and", "data%2Fbig", "xxxx")
	clock.Advance(25 * time.Hour)
	if res := runLifecycle(t, base); res.Deleted != 1 || exists(t, base, "and", "logs%2Fbig") ||
		!exists(t, base, "and", "logs%2Fsmall") || !exists(t, base, "and", "data%2Fbig") {
		t.Errorf("AND of age, prefix and size: %+v", res)
	}
}

// An unknown action or condition is 400 naming it, and the old
// configuration stays.
func TestStorageLifecycleUnknownCondition400InProcess(t *testing.T) {
	_, h := sdk(t)
	good := `{"rule":[{"action":{"type":"Delete"},"condition":{"age":30}}]}`
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"lc-bucket","lifecycle":`+good+`}`); code != 200 {
		t.Fatal(body)
	}
	for name, lc := range map[string]string{
		"ageInWeeks":    `{"rule":[{"action":{"type":"Delete"},"condition":{"ageInWeeks":3}}]}`,
		"Archive":       `{"rule":[{"action":{"type":"Archive"},"condition":{"age":3}}]}`,
		"isLive":        `{"rule":[{"action":{"type":"AbortIncompleteMultipartUpload"},"condition":{"isLive":true}}]}`,
		"storage class": `{"rule":[{"action":{"type":"SetStorageClass","storageClass":"COLD"},"condition":{"age":3}}]}`,
	} {
		if code, body := raw(t, "PATCH", h.URL+"/storage/v1/b/lc-bucket", `{"lifecycle":`+lc+`}`); code != 400 || !strings.Contains(body, name) {
			t.Errorf("%s = %d %s; want 400 naming it", name, code, body)
		}
	}
	_, body := raw(t, "GET", h.URL+"/storage/v1/b/lc-bucket?prettyPrint=false", "")
	if !strings.Contains(body, `"age":30`) {
		t.Errorf("the old configuration did not stay: %s", body)
	}
}

// The official client's lifecycle round-trips.
func TestStorageLifecycleConfigRoundTripInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "lc-sdk")
	ctx := context.Background()
	want := gcs.Lifecycle{Rules: []gcs.LifecycleRule{
		{Action: gcs.LifecycleAction{Type: gcs.DeleteAction}, Condition: gcs.LifecycleCondition{AgeInDays: 30, MatchesPrefix: []string{"tmp/"}}},
		{Action: gcs.LifecycleAction{Type: gcs.SetStorageClassAction, StorageClass: "COLDLINE"},
			Condition: gcs.LifecycleCondition{MatchesStorageClasses: []string{"STANDARD"}, NumNewerVersions: 3, Liveness: gcs.Archived}},
	}}
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{Lifecycle: &want}); err != nil {
		t.Fatal(err)
	}
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := a.Lifecycle.Rules
	if len(got) != 2 || got[0].Condition.AgeInDays != 30 || got[0].Condition.MatchesPrefix[0] != "tmp/" ||
		got[1].Action.StorageClass != "COLDLINE" || got[1].Condition.NumNewerVersions != 3 || got[1].Condition.Liveness != gcs.Archived {
		t.Errorf("lifecycle = %+v", got)
	}
	if code, _ := raw(t, "GET", lastHTTP.URL+lifecyclePath, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the trigger = %d", code)
	}
}
