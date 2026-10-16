package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func clockedServer(t *testing.T) (*httptest.Server, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s, err := NewServer(Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return h, clock
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func opID(t *testing.T, op map[string]any) string {
	t.Helper()
	name, _ := op["name"].(string)
	return name[strings.LastIndex(name, "/")+1:]
}

func TestAnywhereCacheLifecycle(t *testing.T) {
	h, clock := clockedServer(t)
	base := h.URL + "/storage/v1/b"
	if st, b := doJSON(t, "POST", base+"?project=p", `{"name":"cbk","location":"US-CENTRAL1"}`); st != 200 {
		t.Log(b)
		t.Fatalf("bucket insert = %d", st)
	}
	caches := base + "/cbk/anywhereCaches"
	for body, want := range map[string]int{
		`{}`:                                  400,
		`{"zone":"us-east1-b"}`:               400, // not in US-CENTRAL1
		`{"zone":"us-central1-a","ttl":"1s"}`: 400,
		`{"zone":"us-central1-a","admissionPolicy":"never"}`: 400,
	} {
		if st, _ := doJSON(t, "POST", caches, body); st != want {
			t.Errorf("insert %s = %d; want %d", body, st, want)
		}
	}
	st, op := doJSON(t, "POST", caches, `{"zone":"us-central1-a","ttl":"172800s"}`)
	if st != 200 || op["done"] != false || op["kind"] != "storage#operation" {
		t.Fatalf("insert = %d %v", st, op)
	}
	md, _ := op["metadata"].(map[string]any)
	if md["ttl"] != "172800s" || md["anywhereCacheId"] != "us-central1-a" || !strings.HasSuffix(md["@type"].(string), "CreateAnywhereCacheMetadata") {
		t.Errorf("metadata = %v", md)
	}
	if st, _ := doJSON(t, "POST", caches, `{"zone":"us-central1-a"}`); st != 409 {
		t.Errorf("second insert in the zone = %d; want 409", st)
	}
	if _, c := doJSON(t, "GET", caches+"/us-central1-a", ""); c["state"] != "creating" || c["admissionPolicy"] != admitFirstMiss {
		t.Errorf("cache while creating = %v", c)
	}
	if st, _ := doJSON(t, "POST", caches+"/us-central1-a/pause", ""); st != 400 {
		t.Errorf("pause while creating = %d; want 400", st)
	}
	clock.Add(pendingDelay)
	if _, o := doJSON(t, "GET", base+"/cbk/operations/"+opID(t, op), ""); o["done"] != true || o["response"] == nil {
		t.Errorf("create operation after the delay = %v", o)
	}
	if _, c := doJSON(t, "GET", caches+"/us-central1-a", ""); c["state"] != "running" || c["ttl"] != "172800s" {
		t.Errorf("cache after create = %v", c)
	}
	// update
	st, up := doJSON(t, "PATCH", caches+"/us-central1-a", `{"ttl":"86400s","admissionPolicy":"admit-on-second-miss"}`)
	if st != 200 || up["done"] != false {
		t.Fatalf("update = %d %v", st, up)
	}
	if _, c := doJSON(t, "GET", caches+"/us-central1-a", ""); c["pendingUpdate"] != true || c["ttl"] != "172800s" {
		t.Errorf("cache while updating = %v", c)
	}
	if st, _ := doJSON(t, "PATCH", caches+"/us-central1-a", `{"ttl":"3600s"}`); st != 400 {
		t.Errorf("second update while pending = %d; want 400", st)
	}
	if st, _ := doJSON(t, "POST", base+"/cbk/operations/"+opID(t, up)+"/cancel", ""); st != 400 {
		t.Errorf("cancel an update = %d; want 400", st)
	}
	clock.Add(pendingDelay)
	if _, c := doJSON(t, "GET", caches+"/us-central1-a", ""); c["pendingUpdate"] != false || c["ttl"] != "86400s" || c["admissionPolicy"] != admitSecondMiss {
		t.Errorf("cache after update = %v", c)
	}
	// pause, resume, disable
	for _, step := range []struct{ verb, want string }{{"pause", "paused"}, {"resume", "running"}, {"disable", "disabled"}, {"resume", "running"}, {"disable", "disabled"}} {
		if st, c := doJSON(t, "POST", caches+"/us-central1-a/"+step.verb, ""); st != 200 || c["state"] != step.want {
			t.Errorf("%s = %d %v; want %s", step.verb, st, c, step.want)
		}
	}
	if st, _ := doJSON(t, "POST", caches+"/us-central1-a/pause", ""); st != 400 {
		t.Errorf("pause a disabled cache = %d; want 400", st)
	}
	clock.Add(disabledGrace)
	if st, _ := doJSON(t, "GET", caches+"/us-central1-a", ""); st != 404 {
		t.Errorf("cache an hour after disable = %d; want 404", st)
	}
	// cancel a create
	_, op = doJSON(t, "POST", caches, `{"zone":"us-central1-b"}`)
	if st, _ := doJSON(t, "POST", base+"/cbk/operations/"+opID(t, op)+"/cancel", ""); st != 204 {
		t.Errorf("cancel a create = %d; want 204", st)
	}
	if _, o := doJSON(t, "GET", base+"/cbk/operations/"+opID(t, op), ""); o["done"] != true || o["error"] == nil {
		t.Errorf("cancelled create = %v", o)
	}
	if _, l := doJSON(t, "GET", caches, ""); len(l["items"].([]any)) != 0 {
		t.Errorf("caches after the cancel = %v", l)
	}
	if st, _ := doJSON(t, "POST", base+"/cbk/operations/"+opID(t, op)+"/cancel", ""); st != 400 {
		t.Errorf("cancel a done operation = %d; want 400", st)
	}
}

func TestBucketRelocation(t *testing.T) {
	h, clock := clockedServer(t)
	base := h.URL + "/storage/v1/b"
	doJSON(t, "POST", base+"?project=p", `{"name":"rbk","location":"US-CENTRAL1"}`)
	doJSON(t, "POST", base+"?project=p", `{"name":"dbk","location":"US","customPlacementConfig":{"dataLocations":["US-CENTRAL1","US-EAST1"]}}`)
	for body, want := range map[string]int{
		`{}`:                                    400,
		`{"destinationLocation":"US-CENTRAL1"}`: 400,
		`{"destinationLocation":"ME-WEST1"}`:    400,
		`{"destinationLocation":"NOWHERE"}`:     400,
		`{"destinationLocation":"US-EAST1","destinationKmsKeyName":"k"}`: 501,
	} {
		if st, _ := doJSON(t, "POST", base+"/rbk/relocate", body); st != want {
			t.Errorf("relocate %s = %d; want %d", body, st, want)
		}
	}
	// dry run
	st, op := doJSON(t, "POST", base+"/rbk/relocate", `{"destinationLocation":"US-EAST1","validateOnly":true}`)
	if st != 200 || op["done"] != true {
		t.Errorf("dry run = %d %v", st, op)
	}
	if _, b := doJSON(t, "GET", base+"/rbk", ""); b["location"] != "US-CENTRAL1" {
		t.Errorf("bucket after a dry run = %v", b["location"])
	}
	// with write downtime
	_, op = doJSON(t, "POST", base+"/rbk/relocate", `{"destinationLocation":"us-east1"}`)
	id := opID(t, op)
	if st, _ := doJSON(t, "POST", base+"/rbk/relocate", `{"destinationLocation":"US-WEST1"}`); st != 409 {
		t.Errorf("second relocation = %d; want 409", st)
	}
	if st, _ := doJSON(t, "POST", base+"/rbk/operations/"+id+"/advanceRelocateBucket", `{}`); st != 400 {
		t.Errorf("advance while syncing = %d; want 400", st)
	}
	clock.Add(pendingDelay)
	_, o := doJSON(t, "GET", base+"/rbk/operations/"+id, "")
	md, _ := o["metadata"].(map[string]any)
	if o["done"] != false || md["finalizationState"] != "READY" || md["relocationState"] != "SYNCING" {
		t.Errorf("relocation after sync = %v", o)
	}
	if st, _ := doJSON(t, "POST", base+"/rbk/operations/"+id+"/advanceRelocateBucket", `{"ttl":"3600s"}`); st != 204 {
		t.Errorf("advance = %d; want 204", st)
	}
	_, o = doJSON(t, "GET", base+"/rbk/operations/"+id, "")
	md, _ = o["metadata"].(map[string]any)
	if o["done"] != true || md["relocationState"] != "SUCCEEDED" || md["finalizationState"] != "FINALIZED" || md["writeDowntimeExpireTime"] == nil {
		t.Errorf("relocation after advance = %v", o)
	}
	if _, b := doJSON(t, "GET", base+"/rbk", ""); b["location"] != "US-EAST1" || b["locationType"] != "region" {
		t.Errorf("bucket after relocation = %v %v", b["location"], b["locationType"])
	}
	// cancel
	_, op = doJSON(t, "POST", base+"/rbk/relocate", `{"destinationLocation":"EU"}`)
	if st, _ := doJSON(t, "POST", base+"/rbk/operations/"+opID(t, op)+"/cancel", ""); st != 204 {
		t.Errorf("cancel = %d", st)
	}
	_, o = doJSON(t, "GET", base+"/rbk/operations/"+opID(t, op), "")
	if md, _ := o["metadata"].(map[string]any); o["done"] != true || md["relocationState"] != "CANCELLED" {
		t.Errorf("cancelled relocation = %v", o)
	}
	// without write downtime: a configurable dual-region in US to another
	_, op = doJSON(t, "POST", base+"/dbk/relocate", `{"destinationLocation":"US","destinationCustomPlacementConfig":{"dataLocations":["US-EAST1","US-WEST1"]}}`)
	if md, _ := op["metadata"].(map[string]any); md["finalizationState"] != "NOT_REQUIRED" {
		t.Errorf("no-downtime relocation = %v", op)
	}
	clock.Add(pendingDelay)
	if _, o := doJSON(t, "GET", base+"/dbk/operations/"+opID(t, op), ""); o["done"] != true {
		t.Errorf("no-downtime relocation after sync = %v", o)
	}
	_, b := doJSON(t, "GET", base+"/dbk", "")
	if pc, _ := b["customPlacementConfig"].(map[string]any); b["location"] != "US" || len(pc["dataLocations"].([]any)) != 2 || pc["dataLocations"].([]any)[1] != "US-WEST1" {
		t.Errorf("dual-region after relocation = %v", b)
	}
}
