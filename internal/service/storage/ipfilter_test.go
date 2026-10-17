package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A bucket's ipFilter is stored and returned; with mode Enabled a caller
// outside publicNetworkSource is refused 403, except on the bucket's own
// metadata (#1505).
func TestIPFilterStoredAndEnforced(t *testing.T) {
	_, h := sdk(t)
	b := h.URL + "/storage/v1/b"
	for _, body := range []string{
		`{"name":"ipf1","ipFilter":{"mode":"On"}}`,
		`{"name":"ipf1","ipFilter":{"publicNetworkSource":{"allowedIpCidrRanges":["10.0.0.0/8"]}}}`,
		`{"name":"ipf1","ipFilter":{"mode":"Enabled","publicNetworkSource":{"allowedIpCidrRanges":["10.0.0.300/8"]}}}`,
		`{"name":"ipf1","ipFilter":{"mode":"Enabled","vpcNetworkSources":[{"network":"default"}]}}`,
	} {
		if code, resp := raw(t, "POST", b+"?project=p", body); code != 400 {
			t.Errorf("%s = %d %s, want 400", body, code, resp)
		}
	}
	code, resp := raw(t, "POST", b+"?project=p", `{"name":"ipf1","ipFilter":{"mode":"Enabled","publicNetworkSource":{"allowedIpCidrRanges":["203.0.113.0/24"]},"vpcNetworkSources":[{"network":"projects/p/global/networks/default","allowedIpCidrRanges":["10.0.0.0/8"]}]}}`)
	if code != 200 || !strings.Contains(resp, "203.0.113.0/24") {
		t.Fatalf("create = %d %s", code, resp)
	}
	if code, resp := raw(t, "GET", b+"/ipf1", ""); code != 200 || !strings.Contains(resp, `"mode": "Enabled"`) {
		t.Errorf("bucket get, exempt = %d %s", code, resp)
	}
	if code, resp := raw(t, "GET", b+"/ipf1/o", ""); code != 403 || !strings.Contains(resp, "IP filter") {
		t.Errorf("object list from 127.0.0.1 = %d %s, want 403", code, resp)
	}
	if code, _ := raw(t, "GET", h.URL+"/ipf1/x", ""); code != 403 {
		t.Errorf("XML get = %d, want 403", code)
	}
	// The tunnel's header is believed from a loopback peer.
	req, _ := http.NewRequest("GET", b+"/ipf1/o", nil)
	req.Header.Set(clientAddrHeader, "203.0.113.9")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Errorf("object list from 203.0.113.9 = %v %v, want 200", r, err)
	}
	if code, resp := raw(t, "PATCH", b+"/ipf1", `{"ipFilter":{"mode":"Enabled","publicNetworkSource":{"allowedIpCidrRanges":["127.0.0.0/8"]}}}`); code != 200 {
		t.Fatalf("patch = %d %s", code, resp)
	}
	if code, resp := raw(t, "GET", b+"/ipf1/o", ""); code != 200 {
		t.Errorf("object list allowed = %d %s", code, resp)
	}
	if code, _ := raw(t, "PATCH", b+"/ipf1", `{"ipFilter":{"mode":"Disabled"}}`); code != 200 {
		t.Fatal("disable")
	}
	if code, _ := raw(t, "GET", b+"/ipf1/o", ""); code != 200 {
		t.Errorf("disabled filter refused")
	}
}

// Rapid Caches: create, get, list, update and disable as operations, and a
// managed folder's rapidCacheConfig naming one (#1505).
func TestRapidCachesAndManagedFolderConfig(t *testing.T) {
	old := pendingDelay
	pendingDelay = 0
	defer func() { pendingDelay = old }()
	_, h := sdk(t)
	b := h.URL + "/storage/v1/b"
	raw(t, "POST", b+"?project=p", `{"name":"rcb","location":"us-central1","iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`)
	if code, resp := raw(t, "POST", b+"/rcb/rapidCaches", `{"zone":"us-east1-b"}`); code != 400 {
		t.Errorf("zone outside location = %d %s", code, resp)
	}
	if code, resp := raw(t, "POST", b+"/rcb/rapidCaches", `{"zone":"us-central1-a","cacheType":"fast"}`); code != 400 {
		t.Errorf("bad cacheType = %d %s", code, resp)
	}
	code, resp := raw(t, "POST", b+"/rcb/rapidCaches", `{"zone":"us-central1-a","cacheType":"rapid-cache-ultra","admissionPolicy":"no_read_admission"}`)
	if code != 200 || !strings.Contains(resp, "CreateRapidCacheMetadata") {
		t.Fatalf("insert = %d %s", code, resp)
	}
	if code, _ := raw(t, "POST", b+"/rcb/rapidCaches", `{"zone":"us-central1-a"}`); code != 409 {
		t.Errorf("second insert = %d, want 409", code)
	}
	time.Sleep(time.Millisecond)
	if code, resp := raw(t, "GET", b+"/rcb/rapidCaches/us-central1-a", ""); code != 200 || !strings.Contains(resp, `"state": "running"`) || !strings.Contains(resp, "rapid-cache-ultra") {
		t.Errorf("get = %d %s", code, resp)
	}
	if code, resp := raw(t, "PATCH", b+"/rcb/rapidCaches/us-central1-a", `{"ttl":"7200s"}`); code != 200 || !strings.Contains(resp, "UpdateRapidCacheMetadata") {
		t.Errorf("update = %d %s", code, resp)
	}
	if code, resp := raw(t, "GET", b+"/rcb/rapidCaches", ""); code != 200 || !strings.Contains(resp, `"ttl": "7200s"`) {
		t.Errorf("list = %d %s", code, resp)
	}
	raw(t, "POST", b+"/rcb/managedFolders", `{"name":"team"}`)
	code, resp = raw(t, "PATCH", b+"/rcb/managedFolders/team%2F", `{"rapidCacheConfig":{"policies":{"us-central1-a":{"ingestOnWrite":"enabled"}}}}`)
	if code != 200 || !strings.Contains(resp, `"ingestOnWrite": "enabled"`) || !strings.Contains(resp, `"metageneration": "2"`) {
		t.Errorf("managed folder update = %d %s", code, resp)
	}
	if code, resp := raw(t, "PATCH", b+"/rcb/managedFolders/team%2F?ifMetagenerationMatch=1", `{"rapidCacheConfig":null}`); code != 412 {
		t.Errorf("stale update = %d %s, want 412", code, resp)
	}
	if code, resp := raw(t, "PATCH", b+"/rcb/managedFolders/team%2F", `{"rapidCacheConfig":{"policies":{"us-central1-a":null}}}`); code != 200 || strings.Contains(resp, "rapidCacheConfig") {
		t.Errorf("remove policy = %d %s", code, resp)
	}
	if code, resp := raw(t, "POST", b+"/rcb/rapidCaches/us-central1-a/disable", ""); code != 200 || !strings.Contains(resp, "DisableRapidCacheMetadata") {
		t.Errorf("disable = %d %s", code, resp)
	}
	time.Sleep(time.Millisecond)
	if code, resp := raw(t, "GET", b+"/rcb/rapidCaches/us-central1-a", ""); code != 200 || !strings.Contains(resp, `"state": "disabled"`) {
		t.Errorf("get disabled = %d %s", code, resp)
	}
}
