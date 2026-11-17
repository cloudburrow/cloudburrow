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
		extra := ""
		if k == "hierarchicalNamespace" {
			// Hierarchical namespace needs uniform bucket-level access (#1203).
			extra = `,"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}`
		}
		code, resp := raw(t, "POST", h.URL+"/storage/v1/b?project=p&prettyPrint=false", fmt.Sprintf(`{"name":%q,%q:%s%s}`, name, k, sample, extra))
		refused := how != "kept" && how != "stored" && how != "acl"
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
		code, resp = raw(t, "PATCH", h.URL+"/storage/v1/b/"+name+"-p?prettyPrint=false&projection=full", fmt.Sprintf(`{%q:%s}`, k, sample))
		// A bucket's location is fixed at creation; a change is refused by
		// name, which is not a silent drop either.
		// So are hierarchical namespace (#1203) and custom placement (#1443).
		if (k == "location" || k == "hierarchicalNamespace" || k == "customPlacementConfig") && code == 400 && strings.Contains(resp, k) {
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

// managedFolders.list (#517) of a bucket with none is empty, and an absent
// bucket is 404. Managed folders themselves are managedfolders_test.go (#828).
func TestStorageManagedFoldersListEmpty(t *testing.T) {
	h := rawServer(t)
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/raw/managedFolders", ""); code != 200 || !strings.Contains(body, "storage#managedFolders") || strings.Contains(body, "items") {
		t.Errorf("managedFolders.list = %d %s", code, body)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/absent/managedFolders", ""); code != 404 {
		t.Errorf("an absent bucket's managed folders = %d, want 404", code)
	}
}

// softDeletePolicy.hardDeletePause (#1702): enabled is taken with an output
// effectiveTime, which a client cannot set, is kept while the pause holds,
// and is dropped when it is disabled; a non-boolean enabled is 400.
func TestStorageHardDeletePause(t *testing.T) {
	h := newTestServer(t)
	pause := func(body string) map[string]any {
		t.Helper()
		var b struct {
			SoftDeletePolicy struct {
				HardDeletePause map[string]any `json:"hardDeletePause"`
			} `json:"softDeletePolicy"`
		}
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return b.SoftDeletePolicy.HardDeletePause
	}
	code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"hdp-on","softDeletePolicy":{"retentionDurationSeconds":"604800","hardDeletePause":{"enabled":true,"effectiveTime":"2000-01-01T00:00:00Z"}}}`)
	on := pause(body)
	if code != 200 || on["enabled"] != true || on["effectiveTime"] == nil || on["effectiveTime"] == "2000-01-01T00:00:00Z" {
		t.Errorf("hardDeletePause enabled on insert = %d %s, want it taken with a server effectiveTime", code, body)
	}
	code, body = raw(t, "PATCH", h.URL+"/storage/v1/b/hdp-on", `{"labels":{"a":"b"}}`)
	if got := pause(body); code != 200 || got["effectiveTime"] != on["effectiveTime"] {
		t.Errorf("an unrelated patch moved the pause's effectiveTime: %d %s", code, body)
	}
	code, body = raw(t, "PATCH", h.URL+"/storage/v1/b/hdp-on", `{"softDeletePolicy":{"hardDeletePause":{"enabled":false}}}`)
	if got := pause(body); code != 200 || got["enabled"] == true || got["effectiveTime"] != nil {
		t.Errorf("hardDeletePause disabled on patch = %d %s, want it off without effectiveTime", code, body)
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"hdp-off","softDeletePolicy":{"retentionDurationSeconds":"604800","hardDeletePause":{"enabled":false}}}`); code != 200 {
		t.Errorf("hardDeletePause disabled on insert = %d %s, want it taken", code, body)
	}
	code, body = raw(t, "PATCH", h.URL+"/storage/v1/b/hdp-off", `{"softDeletePolicy":{"hardDeletePause":{"enabled":true}}}`)
	if got := pause(body); code != 200 || got["enabled"] != true || got["effectiveTime"] == nil || !strings.Contains(body, `"retentionDurationSeconds": "604800"`) {
		t.Errorf("hardDeletePause enabled on patch = %d %s, want it on with effectiveTime and the retention kept", code, body)
	}
	if code, body := raw(t, "PATCH", h.URL+"/storage/v1/b/hdp-off", `{"softDeletePolicy":{"hardDeletePause":{"enabled":"yes"}}}`); code != 400 {
		t.Errorf("a string enabled = %d %s, want 400", code, body)
	}
}
