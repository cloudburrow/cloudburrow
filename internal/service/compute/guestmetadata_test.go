package compute

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

type fakeGuestTokens struct {
	minted []string
	scopes [][]string
}

func (f *fakeGuestTokens) MintAccessTokenScoped(email string, scopes []string, ttl time.Duration) string {
	f.scopes = append(f.scopes, scopes)
	return f.MintAccessTokenFor(email, ttl)
}

func (f *fakeGuestTokens) MintAccessTokenFor(email string, ttl time.Duration) string {
	f.minted = append(f.minted, email)
	return "tok-" + email
}

func (f *fakeGuestTokens) MintIDTokenWithClaims(email, aud string, extra map[string]any, _ time.Time, _ time.Duration) (string, error) {
	b, _ := json.Marshal(map[string]any{"email": email, "aud": aud, "extra": extra})
	return string(b), nil
}

// A VM's metadata server (#2177) answers its own instance/ and project/
// trees as Google's does: leaves, listings, recursive JSON, ETags,
// wait_for_change, the Metadata-Flavor and X-Forwarded-For rules, its
// service account's tokens, and guest attributes, which
// instances.getGuestAttributes reads.
func TestVMMetadataServer2177(t *testing.T) {
	toks := &fakeGuestTokens{}
	s, err := New(Options{VMs: fakeVMs{}, Tokens: toks})
	if err != nil {
		t.Fatal(err)
	}
	z := "zones/us-central1-a/"
	sa := "app@p.iam.gserviceaccount.com"
	if _, err := vmCall(t, s, "POST", z+"instances", obj{"name": "vm1", "machineType": z + "machineTypes/e2-small",
		"description":       "the web tier",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}},
		"metadata":          obj{"items": []any{obj{"key": "role", "value": "web"}}},
		"serviceAccounts":   []any{obj{"email": sa, "scopes": []any{"https://www.googleapis.com/auth/cloud-platform"}}}}); err != nil {
		t.Fatal(err)
	}
	var token, rel string
	for k, o := range s.res {
		if strings.HasSuffix(k, "/instances/vm1") {
			token, rel = str(o, "_guestToken"), k
		}
	}
	inst, _ := s.Call("GET", "p", z+"instances/vm1", nil, nil)
	type resp struct {
		code        int
		body, etag  string
		ctype, flav string
	}
	do := func(method, path string, hdr map[string]string, body string) resp {
		t.Helper()
		req := httptest.NewRequest(method, GuestPrefix+token+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return resp{rec.Code, rec.Body.String(), rec.Header().Get("ETag"), rec.Header().Get("Content-Type"), rec.Header().Get("Metadata-Flavor")}
	}
	flavor := map[string]string{"Metadata-Flavor": "Google"}
	get := func(path string) resp { t.Helper(); return do("GET", "/computeMetadata/v1/"+path, flavor, "") }

	// The root probe needs no header; everything else does.
	if r := do("GET", "/", nil, ""); r.code != 200 || r.flav != "Google" || r.body != "computeMetadata/\n" {
		t.Errorf("root probe: %+v", r)
	}
	if r := do("GET", "/computeMetadata/v1/instance/id", nil, ""); r.code != 403 || r.flav != "Google" {
		t.Errorf("no header: %+v", r)
	}
	if r := do("GET", "/computeMetadata/v1/instance/id", map[string]string{"Metadata-Flavor": "Google", "X-Forwarded-For": "1.2.3.4"}, ""); r.code != 403 {
		t.Errorf("X-Forwarded-For: %+v", r)
	}
	number := strconv.FormatInt(resource.ProjectNumber("p"), 10)
	for path, want := range map[string]string{
		"instance/id":                              str(inst, "id"),
		"instance/name":                            "vm1",
		"instance/description":                     "the web tier",
		"instance/zone":                            "projects/" + number + "/zones/us-central1-a",
		"instance/machine-type":                    "projects/" + number + "/machineTypes/e2-small",
		"instance/hostname":                        "vm1.us-central1-a.c.p.internal",
		"instance/maintenance-event":               "NONE",
		"instance/attributes/role":                 "web",
		"instance/attributes/":                     "role\n",
		"instance/scheduling/preemptible":          "FALSE",
		"instance/service-accounts/":               "app@p.iam.gserviceaccount.com/\ndefault/\n",
		"instance/service-accounts/default/email":  sa,
		"instance/service-accounts/default/scopes": "https://www.googleapis.com/auth/cloud-platform\n",
		"instance/service-accounts/default/":       "aliases\nemail\nidentity\nscopes\ntoken\n",
		"instance/disks/0/device-name":             "vm1",
		"instance/virtual-clock/drift-token":       "0",
		"project/project-id":                       "p",
		"project/numeric-project-id":               number,
		"project/attributes/":                      "",
		"":                                         "instance/\nproject/\n",
	} {
		r := get(path)
		if r.code != 200 || r.body != want || r.etag == "" || r.flav != "Google" {
			t.Errorf("%s: %d %q (etag %q), want %q", path, r.code, r.body, r.etag, want)
		}
	}
	if r := get("instance/"); r.code != 200 || !strings.Contains(r.body, "network-interfaces/\n") || !strings.Contains(r.body, "guest-attributes/\n") || !strings.Contains(r.body, "service-accounts/\n") {
		t.Errorf("instance/: %q", r.body)
	}
	// recursive=true is JSON in camelCase, without tokens.
	r := get("instance/?recursive=true")
	var tree map[string]any
	if err := json.Unmarshal([]byte(r.body), &tree); err != nil || r.ctype != "application/json" {
		t.Fatalf("recursive: %v %q", err, r.body)
	}
	nics, _ := tree["networkInterfaces"].([]any)
	if len(nics) != 1 || tree["machineType"] == nil || objOf(tree["attributes"])["role"] != "web" {
		t.Errorf("recursive tree: %v", tree)
	}
	if acs, _ := objOf(nics[0])["accessConfigs"].([]any); len(acs) != 1 {
		t.Errorf("accessConfigs: %v", nics[0])
	}
	if d := objOf(objOf(tree["serviceAccounts"])["default"]); d["email"] != sa || d["token"] != nil {
		t.Errorf("serviceAccounts: %v", d)
	}
	if r := get("instance/disks/?recursive=true&alt=text"); !strings.Contains(r.body, "0/device-name vm1\n") {
		t.Errorf("alt=text: %q", r.body)
	}
	// The account's token and identity.
	if r := get("instance/service-accounts/default/token"); r.code != 200 || !strings.Contains(r.body, `"access_token":"tok-`+sa+`"`) {
		t.Errorf("token: %+v", r)
	}
	if r := get("instance/service-accounts/" + sa + "/identity"); r.code != 400 {
		t.Errorf("identity without audience: %+v", r)
	}
	if r := get("instance/service-accounts/default/identity?audience=https://x&format=full"); r.code != 200 || !strings.Contains(r.body, `"compute_engine"`) || !strings.Contains(r.body, `"instance_name":"vm1"`) {
		t.Errorf("identity: %+v", r)
	}
	if r := get("instance/service-accounts/other@p.iam.gserviceaccount.com/token"); r.code != 404 {
		t.Errorf("other account: %+v", r)
	}
	if r := get("instance/service-accounts/default/token?wait_for_change=true"); r.code != 400 {
		t.Errorf("wait on token: %+v", r)
	}
	if r := get("instance/attributes/?wait_for_change=true"); r.code != 400 {
		t.Errorf("wait on listing: %+v", r)
	}
	// wait_for_change: a different last_etag answers at once; the current
	// one waits for setMetadata.
	cur := get("instance/attributes/role")
	if r := get("instance/attributes/role?wait_for_change=true&last_etag=0"); r.code != 200 || r.body != "web" {
		t.Errorf("stale etag: %+v", r)
	}
	if r := get("instance/attributes/role?wait_for_change=true&timeout_sec=0&last_etag=" + cur.etag); r.body != "web" || r.etag != cur.etag {
		t.Errorf("timeout: %+v", r)
	}
	done := make(chan resp, 1)
	go func() {
		done <- get("instance/attributes/role?wait_for_change=true&timeout_sec=20&last_etag=" + cur.etag)
	}()
	time.Sleep(300 * time.Millisecond)
	fp := str(objOf(inst["metadata"]), "fingerprint")
	if _, err := vmCall(t, s, "POST", z+"instances/vm1/setMetadata", obj{"fingerprint": fp, "items": []any{obj{"key": "role", "value": "db"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.body != "db" || r.etag == cur.etag {
			t.Errorf("changed: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait_for_change did not return on change")
	}
	// Guest attributes: refused until enabled.
	if r := do("PUT", "/computeMetadata/v1/instance/guest-attributes/app/state", flavor, "ready"); r.code != 403 {
		t.Errorf("disabled PUT: %+v", r)
	}
	inst, _ = s.Call("GET", "p", z+"instances/vm1", nil, nil)
	fp = str(objOf(inst["metadata"]), "fingerprint")
	if _, err := vmCall(t, s, "POST", z+"instances/vm1/setMetadata", obj{"fingerprint": fp, "items": []any{obj{"key": "enable-guest-attributes", "value": "TRUE"}}}); err != nil {
		t.Fatal(err)
	}
	if r := do("PUT", "/computeMetadata/v1/instance/guest-attributes/app/state", flavor, "ready"); r.code != 200 {
		t.Errorf("PUT: %+v", r)
	}
	if r := do("PUT", "/computeMetadata/v1/instance/guest-attributes/app/state", nil, "x"); r.code != 403 {
		t.Errorf("PUT without header: %+v", r)
	}
	if r := do("PUT", "/computeMetadata/v1/instance/guest-attributes/bad.ns/k", flavor, "x"); r.code != 400 {
		t.Errorf("bad namespace: %+v", r)
	}
	if r := do("PUT", "/computeMetadata/v1/instance/guest-attributes/app/big", flavor, strings.Repeat("x", 256<<10+1)); r.code != 400 {
		t.Errorf("big value: %d", r.code)
	}
	if r := get("instance/guest-attributes/app/state"); r.body != "ready" {
		t.Errorf("GET attribute: %+v", r)
	}
	if r := get("instance/guest-attributes/app/"); r.body != "state\n" {
		t.Errorf("namespace: %+v", r)
	}
	ga, err := s.Call("GET", "p", z+"instances/vm1/getGuestAttributes", map[string][]string{"queryPath": {"app/"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := listOf(objOf(ga["queryValue"])["items"])
	if len(items) != 1 || objOf(items[0])["value"] != "ready" || objOf(items[0])["namespace"] != "app" {
		t.Errorf("getGuestAttributes: %v", ga)
	}
	ga, err = s.Call("GET", "p", z+"instances/vm1/getGuestAttributes", map[string][]string{"variableKey": {"app/state"}}, nil)
	if err != nil || ga["variableValue"] != "ready" {
		t.Errorf("variableKey: %v %v", ga, err)
	}
	if _, err := s.Call("GET", "p", z+"instances/vm1/getGuestAttributes", map[string][]string{"variableKey": {"app/none"}}, nil); err == nil {
		t.Error("a missing key was found")
	}
	if r := do("DELETE", "/computeMetadata/v1/instance/guest-attributes/app/state", flavor, ""); r.code != 200 {
		t.Errorf("DELETE: %+v", r)
	}
	if r := get("instance/guest-attributes/app/state"); r.code != 404 {
		t.Errorf("deleted: %+v", r)
	}
	if r := do("POST", "/computeMetadata/v1/instance/id", flavor, ""); r.code != 405 {
		t.Errorf("POST: %+v", r)
	}
	_ = rel
	// An instance without a service account lists none.
	if _, err := vmCall(t, s, "POST", z+"instances", obj{"name": "vm2", "machineType": z + "machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}}); err != nil {
		t.Fatal(err)
	}
	for k, o := range s.res {
		if strings.HasSuffix(k, "/instances/vm2") {
			token = str(o, "_guestToken")
		}
	}
	if r := get("instance/service-accounts/"); r.code != 200 || r.body != "" {
		t.Errorf("vm2 accounts: %+v", r)
	}
	if r := get("instance/service-accounts/default/token"); r.code != 404 {
		t.Errorf("vm2 token: %+v", r)
	}
}
