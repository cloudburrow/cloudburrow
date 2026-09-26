package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// bucketSamples is a meaningful value for each settable Bucket property,
// the ones a client may send. Every such discovery property must have one,
// so a property added upstream cannot go untested.
var bucketSamples = map[string]string{
	"autoclass":             `{"enabled":true,"terminalStorageClass":"ARCHIVE"}`,
	"billing":               `{"requesterPays":true}`,
	"cors":                  `[{"origin":["*"],"method":["GET"]}]`,
	"customPlacementConfig": `{"dataLocations":["US-EAST1","US-WEST1"]}`,
	"defaultEventBasedHold": `true`,
	"encryption":            `{"defaultKmsKeyName":"projects/p/locations/us/keyRings/r/cryptoKeys/k"}`,
	"hierarchicalNamespace": `{"enabled":true}`,
	"iamConfiguration":      `{"uniformBucketLevelAccess":{"enabled":true}}`,
	"labels":                `{"env":"dev"}`,
	"lifecycle":             `{"rule":[{"action":{"type":"Delete"},"condition":{"age":30}}]}`,
	"location":              `"EU"`,
	"logging":               `{"logBucket":"logs","logObjectPrefix":"p"}`,
	"retentionPolicy":       `{"retentionPeriod":"60"}`,
	"rpo":                   `"ASYNC_TURBO"`,
	"softDeletePolicy":      `{"retentionDurationSeconds":"0"}`,
	"storageClass":          `"NEARLINE"`,
	"versioning":            `{"enabled":true}`,
	"website":               `{"mainPageSuffix":"index.html","notFoundPage":"404.html"}`,
	"ipFilter":              `{"mode":"Enabled"}`,
	"acl":                   `[{"entity":"allUsers","role":"READER"}]`,
	"defaultObjectAcl":      `[{"entity":"allUsers","role":"READER"}]`,
}

// Every settable discovery Bucket property is either kept and echoed on a
// create and a patch, or refused with 400 naming it: never accepted and
// dropped (#503, #374).
func TestStorageBucketFieldsNeverSilentlyDropped(t *testing.T) {
	var d struct {
		Schemas map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal(storageAPI, &d); err != nil {
		t.Fatal(err)
	}
	var props []string
	for k := range d.Schemas["Bucket"].Properties {
		props = append(props, k)
	}
	sort.Strings(props)
	if len(props) != 38 {
		t.Errorf("the Bucket schema has %d properties; bucketFields was written for 38", len(props))
	}
	_, h := sdk(t)
	for i, k := range props {
		how, ok := bucketFields[k]
		if !ok {
			t.Errorf("%s: not classified in bucketFields", k)
			continue
		}
		if how == "output" {
			continue
		}
		sample, ok := bucketSamples[k]
		if !ok {
			t.Errorf("%s: settable but no sample value", k)
			continue
		}
		name := fmt.Sprintf("fields-%02d", i)
		code, resp := raw(t, "POST", h.URL+"/storage/v1/b?project=p&prettyPrint=false", fmt.Sprintf(`{"name":%q,%q:%s}`, name, k, sample))
		refused := how != "kept" && how != "stored"
		switch {
		case refused && (code != 400 || !strings.Contains(resp, k)):
			t.Errorf("%s (%s): create = %d %s; want 400 naming it", k, how, code, resp)
		case !refused && (code != 200 || !strings.Contains(resp, `"`+k+`":`)):
			t.Errorf("%s: create = %d %s; want it echoed", k, code, resp)
		}
		if refused {
			continue
		}
		raw(t, "POST", h.URL+"/storage/v1/b?project=p", fmt.Sprintf(`{"name":"%s-p"}`, name))
		code, resp = raw(t, "PATCH", h.URL+"/storage/v1/b/"+name+"-p?prettyPrint=false", fmt.Sprintf(`{%q:%s}`, k, sample))
		// A bucket's location is fixed at creation; a change is refused by
		// name, which is not a silent drop either.
		if k == "location" && code == 400 && strings.Contains(resp, k) {
			continue
		}
		if code != 200 || !strings.Contains(resp, `"`+k+`":`) {
			t.Errorf("%s: patch = %d %s; want it echoed", k, code, resp)
		}
	}
}

// buckets.getStorageLayout (#517) reports the layout fields buckets.get
// does, and a disabled hierarchical namespace when none was set.
func TestStorageBucketStorageLayout(t *testing.T) {
	h := rawServer(t)
	code, body := raw(t, "GET", h.URL+"/storage/v1/b/raw/storageLayout", "")
	if code != 200 {
		t.Fatalf("storageLayout = %d %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "storage#storageLayout" || got["bucket"] != "raw" || got["location"] != "US" || got["locationType"] == nil {
		t.Errorf("storageLayout = %v", got)
	}
	if ns, _ := got["hierarchicalNamespace"].(map[string]any); ns["enabled"] != false {
		t.Errorf("hierarchicalNamespace = %v, want enabled false", got["hierarchicalNamespace"])
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/absent/storageLayout", ""); code != 404 {
		t.Errorf("an absent bucket's layout = %d, want 404", code)
	}
}

// managedFolders.list (#517) is empty, since none can be created; insert
// stays 501, and an absent bucket is 404.
func TestStorageManagedFoldersListEmpty(t *testing.T) {
	h := rawServer(t)
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/raw/managedFolders", ""); code != 200 || !strings.Contains(body, "storage#managedFolders") || strings.Contains(body, "items") {
		t.Errorf("managedFolders.list = %d %s", code, body)
	}
	if code, _ := raw(t, "POST", h.URL+"/storage/v1/b/raw/managedFolders", `{"name":"f/"}`); code != 501 {
		t.Errorf("managedFolders.insert = %d, want 501", code)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/absent/managedFolders", ""); code != 404 {
		t.Errorf("an absent bucket's managed folders = %d, want 404", code)
	}
}
