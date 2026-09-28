package storage

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// mfServer is a server with a bucket, "mfb", whose uniform bucket-level
// access is on, as a managed folder needs.
func mfServer(t *testing.T) *httptest.Server {
	t.Helper()
	_, h := sdk(t)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"mfb","iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`); code != 200 {
		t.Fatal(body)
	}
	return h
}

func mfURL(h *httptest.Server, name string) string {
	return h.URL + "/storage/v1/b/mfb/managedFolders/" + url.PathEscape(name)
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return m
}

// insert returns the managed folder as the API does, its name ending in /;
// get reads it back; a second insert is 409; a name without the trailing /
// is the same managed folder.
func TestManagedFolderInsertGet(t *testing.T) {
	h := mfServer(t)
	code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders", `{"name":"logs/reports"}`)
	if code != 200 {
		t.Fatalf("insert = %d %s", code, body)
	}
	got := decode(t, body)
	if got["kind"] != "storage#managedFolder" || got["name"] != "logs/reports/" || got["bucket"] != "mfb" ||
		got["id"] != "mfb/logs/reports/" || got["metageneration"] != "1" || got["createTime"] == nil || got["updateTime"] == nil ||
		!strings.HasSuffix(got["selfLink"].(string), "/storage/v1/b/mfb/managedFolders/logs%2Freports%2F") {
		t.Errorf("insert returned %v", got)
	}
	for _, name := range []string{"logs/reports/", "logs/reports"} {
		code, body = raw(t, "GET", mfURL(h, name), "")
		if code != 200 || decode(t, body)["name"] != "logs/reports/" {
			t.Errorf("get %q = %d %s", name, code, body)
		}
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders", `{"name":"logs/reports/"}`); code != 409 {
		t.Errorf("a second insert = %d %s, want 409", code, body)
	}
	if code, _ := raw(t, "GET", mfURL(h, "absent/"), ""); code != 404 {
		t.Errorf("get of an absent managed folder = %d, want 404", code)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/nobucket/managedFolders/x%2F", ""); code != 404 {
		t.Errorf("get in an absent bucket = %d, want 404", code)
	}
	if code, _ := raw(t, "GET", mfURL(h, "logs/reports/")+"?ifMetagenerationMatch=2", ""); code != 412 {
		t.Errorf("get with a stale ifMetagenerationMatch = %d, want 412", code)
	}
	if code, _ := raw(t, "GET", mfURL(h, "logs/reports/")+"?ifMetagenerationNotMatch=1", ""); code != 304 {
		t.Errorf("get with a matching ifMetagenerationNotMatch = %d, want 304", code)
	}
}

// insert refuses what the API refuses: no name, a bad name, an unknown
// field, a bucket without uniform bucket-level access, an absent bucket,
// and rapidCacheConfig by name as not implemented.
func TestManagedFolderInsertRefusals(t *testing.T) {
	h := mfServer(t)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"fine-grained"}`); code != 200 {
		t.Fatal(body)
	}
	for _, c := range []struct {
		bucket, body string
		code         int
		says         string
	}{
		{"mfb", `{}`, 400, "name"},
		{"mfb", `{"name":"/lead"}`, 400, "empty segment"},
		{"mfb", `{"name":"a//b"}`, 400, "empty segment"},
		{"mfb", `{"name":"a/../b"}`, 400, `..\" segment`},
		{"mfb", `{"name":"a\nb"}`, 400, "line feed"},
		{"mfb", `{"name":"` + strings.Repeat("a", 1025) + `"}`, 400, "1024"},
		{"mfb", `{"name":"a","colour":"red"}`, 400, "colour is not a ManagedFolder field"},
		{"mfb", `{"name":"a","rapidCacheConfig":{"enabled":true}}`, 501, "rapidCacheConfig"},
		{"fine-grained", `{"name":"a"}`, 400, "uniform bucket-level access"},
		{"nobucket", `{"name":"a"}`, 404, "bucket"},
	} {
		code, body := raw(t, "POST", h.URL+"/storage/v1/b/"+c.bucket+"/managedFolders", c.body)
		if code != c.code || !strings.Contains(body, c.says) {
			t.Errorf("insert %s into %s = %d %s; want %d saying %q", c.body, c.bucket, code, body, c.code, c.says)
		}
	}
	// Output-only fields are ignored, as Google ignores them.
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders",
		`{"name":"out","kind":"storage#managedFolder","metageneration":"9","bucket":"other"}`); code != 200 ||
		decode(t, body)["metageneration"] != "1" || decode(t, body)["bucket"] != "mfb" {
		t.Errorf("insert with output-only fields = %d %s", code, body)
	}
	// The legacy name of the setting counts too.
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"legacy","iamConfiguration":{"bucketPolicyOnly":{"enabled":true}}}`); code != 200 {
		t.Fatal(body)
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/legacy/managedFolders", `{"name":"a"}`); code != 200 {
		t.Errorf("insert into a bucketPolicyOnly bucket = %d %s", code, body)
	}
}

// list returns what exists, by name, under a prefix, a page at a time.
func TestManagedFolderList(t *testing.T) {
	h := mfServer(t)
	for _, n := range []string{"b/", "a/", "a/x/", "c/"} {
		if code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders", `{"name":"`+n+`"}`); code != 200 {
			t.Fatal(body)
		}
	}
	names := func(body string) (out []string, next string) {
		var l struct {
			Kind          string
			Items         []struct{ Name string }
			NextPageToken string
		}
		if err := json.Unmarshal([]byte(body), &l); err != nil || l.Kind != "storage#managedFolders" {
			t.Fatalf("list = %v %s", err, body)
		}
		for _, i := range l.Items {
			out = append(out, i.Name)
		}
		return out, l.NextPageToken
	}
	base := h.URL + "/storage/v1/b/mfb/managedFolders"
	_, body := raw(t, "GET", base, "")
	if got, next := names(body); strings.Join(got, ",") != "a/,a/x/,b/,c/" || next != "" {
		t.Errorf("list = %v, next %q", got, next)
	}
	_, body = raw(t, "GET", base+"?prefix=a/", "")
	if got, _ := names(body); strings.Join(got, ",") != "a/,a/x/" {
		t.Errorf("list prefix=a/ = %v", got)
	}
	var all []string
	tok := ""
	for pages := 0; pages < 5; pages++ {
		_, body = raw(t, "GET", base+"?pageSize=3&pageToken="+url.QueryEscape(tok), "")
		got, next := names(body)
		all = append(all, got...)
		if tok = next; tok == "" {
			break
		}
	}
	if strings.Join(all, ",") != "a/,a/x/,b/,c/" {
		t.Errorf("paged list = %v", all)
	}
	if code, _ := raw(t, "GET", base+"?pageToken=!!!", ""); code != 400 {
		t.Errorf("a bad page token = %d, want 400", code)
	}
}

// delete: a managed folder with an object or a managed folder under it is
// refused unless allowNonEmpty, which keeps the objects; the metageneration
// preconditions apply; a deleted or absent one is 404. A bucket holding a
// managed folder cannot be deleted.
func TestManagedFolderDelete(t *testing.T) {
	h := mfServer(t)
	for _, n := range []string{"a/", "a/x/", "e/"} {
		if code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders", `{"name":"`+n+`"}`); code != 200 {
			t.Fatal(body)
		}
	}
	if code, body := raw(t, "POST", h.URL+"/upload/storage/v1/b/mfb/o?uploadType=media&name=e/obj.txt", "bytes"); code != 200 {
		t.Fatal(body)
	}
	if code, body := raw(t, "DELETE", mfURL(h, "a/"), ""); code != 409 || !strings.Contains(body, "allowNonEmpty") {
		t.Errorf("delete with a managed folder under it = %d %s, want 409", code, body)
	}
	if code, body := raw(t, "DELETE", mfURL(h, "e/"), ""); code != 409 {
		t.Errorf("delete with an object under it = %d %s, want 409", code, body)
	}
	if code, _ := raw(t, "DELETE", mfURL(h, "e/")+"?allowNonEmpty=true&ifMetagenerationMatch=2", ""); code != 412 {
		t.Errorf("delete with a stale ifMetagenerationMatch = %d, want 412", code)
	}
	if code, _ := raw(t, "DELETE", mfURL(h, "e/")+"?allowNonEmpty=true&ifMetagenerationNotMatch=1", ""); code != 412 {
		t.Errorf("delete with a matching ifMetagenerationNotMatch = %d, want 412", code)
	}
	if code, _ := raw(t, "DELETE", mfURL(h, "e/")+"?allowNonEmpty=maybe", ""); code != 400 {
		t.Errorf("allowNonEmpty=maybe = %d, want 400", code)
	}
	if code, body := raw(t, "DELETE", mfURL(h, "e/")+"?allowNonEmpty=true&ifMetagenerationMatch=1", ""); code != 204 {
		t.Fatalf("delete with allowNonEmpty = %d %s", code, body)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/mfb/o/"+url.PathEscape("e/obj.txt"), ""); code != 200 {
		t.Errorf("the object under a deleted managed folder = %d, want it kept", code)
	}
	if code, _ := raw(t, "GET", mfURL(h, "e/"), ""); code != 404 {
		t.Errorf("get after delete = %d, want 404", code)
	}
	if code, _ := raw(t, "DELETE", mfURL(h, "e/"), ""); code != 404 {
		t.Errorf("a second delete = %d, want 404", code)
	}
	if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/mfb/o/"+url.PathEscape("e/obj.txt"), ""); code != 204 {
		t.Fatal("delete the object")
	}
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/mfb", ""); code != 409 || !strings.Contains(body, "managed folders") {
		t.Errorf("delete a bucket holding managed folders = %d %s, want 409", code, body)
	}
	for _, n := range []string{"a/x/", "a/"} {
		if code, body := raw(t, "DELETE", mfURL(h, n), ""); code != 204 {
			t.Fatalf("delete %s = %d %s", n, code, body)
		}
	}
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/mfb", ""); code != 204 {
		t.Errorf("delete the emptied bucket = %d %s", code, body)
	}
}

// A managed folder's IAM policy is kept on it, as a bucket's is: a set
// reads back, a stale etag is 412, a condition is 501, and
// testIamPermissions returns every permission asked for. An absent managed
// folder is 404 on each.
func TestManagedFolderIAM(t *testing.T) {
	h := mfServer(t)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders", `{"name":"team/"}`); code != 200 {
		t.Fatal(body)
	}
	iam := mfURL(h, "team/") + "/iam"
	code, body := raw(t, "GET", iam, "")
	first := decode(t, body)
	if code != 200 || first["resourceId"] != "projects/_/buckets/mfb/managedFolders/team/" || first["version"] != float64(1) {
		t.Fatalf("getIamPolicy = %d %s", code, body)
	}
	set := `{"etag":"` + first["etag"].(string) + `","bindings":[{"role":"roles/storage.objectViewer","members":["user:dev@example.com"]}]}`
	if code, body := raw(t, "PUT", iam, set); code != 200 || !strings.Contains(body, "dev@example.com") {
		t.Fatalf("setIamPolicy = %d %s", code, body)
	}
	if code, body := raw(t, "GET", iam, ""); code != 200 || !strings.Contains(body, "roles/storage.objectViewer") {
		t.Errorf("getIamPolicy after set = %d %s", code, body)
	}
	if code, _ := raw(t, "PUT", iam, set); code != 412 {
		t.Errorf("a set with a stale etag = %d, want 412", code)
	}
	if code, body := raw(t, "PUT", iam, `{"bindings":[{"role":"r","members":["user:a@example.com"],"condition":{"expression":"true"}}]}`); code != 501 || !strings.Contains(body, "condition") {
		t.Errorf("a conditional binding = %d %s, want 501", code, body)
	}
	if code, body := raw(t, "GET", iam+"/testPermissions?permissions=storage.objects.get&permissions=storage.objects.list", ""); code != 200 ||
		!strings.Contains(body, "storage.objects.get") || !strings.Contains(body, "storage.objects.list") {
		t.Errorf("testIamPermissions = %d %s", code, body)
	}
	if code, _ := raw(t, "GET", iam+"/testPermissions", ""); code != 400 {
		t.Errorf("testIamPermissions with none = %d, want 400", code)
	}
	// The bucket's own policy is untouched by the managed folder's.
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/mfb/iam", ""); code != 200 || strings.Contains(body, "dev@example.com") {
		t.Errorf("the bucket's policy = %d %s", code, body)
	}
	absent := mfURL(h, "absent/") + "/iam"
	for _, c := range []struct{ m, u, b string }{{"GET", absent, ""}, {"PUT", absent, `{"bindings":[]}`},
		{"GET", absent + "/testPermissions?permissions=storage.objects.get", ""}} {
		if code, _ := raw(t, c.m, c.u, c.b); code != 404 {
			t.Errorf("%s %s = %d, want 404", c.m, c.u, code)
		}
	}
	// managedFolders.update stays 501 by name.
	if code, body := raw(t, "PATCH", mfURL(h, "team/"), `{}`); code != 501 || !strings.Contains(body, "storage.managedFolders.update") {
		t.Errorf("update = %d %s, want 501 naming it", code, body)
	}
}

// Managed folders, with their policies, persist across a restart of the
// durable store, and a reset, an export and an import carry them.
func TestManagedFolderPersistsResetsAndSnapshots(t *testing.T) {
	dir := t.TempDir()
	open := func() (*httptest.Server, *LogMetaStore) {
		meta, err := OpenLogMetaStore(filepath.Join(dir, "meta"))
		if err != nil {
			t.Fatal(err)
		}
		srv, err := NewServer(Options{Meta: meta})
		if err != nil {
			t.Fatal(err)
		}
		return httptest.NewServer(srv), meta
	}
	h, meta := open()
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"mfb","iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/mfb/managedFolders", `{"name":"kept/"}`); code != 200 {
		t.Fatal(body)
	}
	raw(t, "PUT", mfURL(h, "kept/")+"/iam", `{"bindings":[{"role":"roles/storage.objectViewer","members":["user:dev@example.com"]}]}`)
	h.Close()
	meta.Close()

	h, meta = open()
	defer meta.Close()
	defer h.Close()
	if code, body := raw(t, "GET", mfURL(h, "kept/"), ""); code != 200 {
		t.Fatalf("after a restart the managed folder = %d %s", code, body)
	}
	if _, body := raw(t, "GET", mfURL(h, "kept/")+"/iam", ""); !strings.Contains(body, "dev@example.com") {
		t.Errorf("after a restart the managed folder's policy = %s", body)
	}

	// Export, reset (which removes it), import (which brings it back).
	code, tarball := raw(t, "GET", h.URL+statePath, "")
	if code != 200 {
		t.Fatalf("export = %d", code)
	}
	if code, _ := raw(t, "POST", h.URL+resetPath+"?project=p", ""); code >= 300 {
		t.Fatalf("reset = %d", code)
	}
	if code, _ := raw(t, "GET", mfURL(h, "kept/"), ""); code != 404 {
		t.Errorf("after a project reset the managed folder = %d, want 404", code)
	}
	if code, body := raw(t, "PUT", h.URL+statePath, tarball); code >= 300 {
		t.Fatalf("import = %d %s", code, body)
	}
	if code, body := raw(t, "GET", mfURL(h, "kept/")+"/iam", ""); code != 200 || !strings.Contains(body, "dev@example.com") {
		t.Errorf("after an import the managed folder's policy = %d %s", code, body)
	}
}
