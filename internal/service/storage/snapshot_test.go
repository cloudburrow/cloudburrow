package storage

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// populate gives s versions, holds, retention, customTime, a soft-deleted
// object, a notification config, an IAM policy and an HMAC key.
func populate(t *testing.T, base string) (hmacID string) {
	t.Helper()
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/storage/v1/b?project=p", `{"name":"snap","versioning":{"enabled":true},"labels":{"env":"dev"},"cors":[{"origin":["*"],"method":["GET"]}]}`},
		{"POST", "/storage/v1/b/snap/notificationConfigs", `{"topic":"projects/p/topics/t"}`},
		{"PUT", "/storage/v1/b/snap/iam", `{"bindings":[{"role":"roles/storage.admin","members":["user:a@example.com"]}]}`},
		{"POST", "/upload/storage/v1/b/snap/o?uploadType=media&name=v", "one"},
		{"POST", "/upload/storage/v1/b/snap/o?uploadType=media&name=v", "two"},
		{"PATCH", "/storage/v1/b/snap/o/v", `{"temporaryHold":true,"customTime":"2026-01-02T03:04:05Z","metadata":{"k":"v"}}`},
		{"POST", "/storage/v1/b?project=p", `{"name":"plain-snap"}`},
		{"POST", "/upload/storage/v1/b/plain-snap/o?uploadType=media&name=gone", "soft"},
		{"DELETE", "/storage/v1/b/plain-snap/o/gone", ""},
	} {
		if code, body := raw(t, c.m, base+c.p, c.b); code >= 300 {
			t.Fatalf("%s %s = %d %s", c.m, c.p, code, body)
		}
	}
	id, _ := hmacKeyFor(t, base)
	return id
}

func snapServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, err := NewServer(Options{Publisher: &recorder{}})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return s, h.URL
}

// Versions, holds, customTime, soft-deleted objects, bucket fields,
// notification configs and IAM policies survive an export and import into
// a fresh server.
func TestSnapshotRoundTripKeepsVersionsAndHolds(t *testing.T) {
	src, sbase := snapServer(t)
	populate(t, sbase)
	_, before := raw(t, "GET", sbase+"/storage/v1/b/snap/o?versions=true&prettyPrint=false", "")
	var buf bytes.Buffer
	if err := src.ExportState(&buf); err != nil {
		t.Fatal(err)
	}
	dst, dbase := snapServer(t)
	raw(t, "POST", dbase+"/storage/v1/b?project=p", `{"name":"stale"}`)
	if err := dst.ImportState(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	before = strings.ReplaceAll(before, sbase, "<h>")
	if _, after := raw(t, "GET", dbase+"/storage/v1/b/snap/o?versions=true&prettyPrint=false", ""); strings.ReplaceAll(after, dbase, "<h>") != before {
		t.Errorf("versions after the round trip:\n%s\nbefore:\n%s", strings.ReplaceAll(after, dbase, "<h>"), before)
	}
	for path, want := range map[string]string{
		"/storage/v1/b/snap?prettyPrint=false":                          `"env":"dev"`,
		"/storage/v1/b/snap/o/v?prettyPrint=false":                      `"temporaryHold":true`,
		"/storage/v1/b/snap/notificationConfigs?prettyPrint=false":      `"topic":"//pubsub.googleapis.com/projects/p/topics/t"`,
		"/storage/v1/b/snap/iam?prettyPrint=false":                      `roles/storage.admin`,
		"/storage/v1/b/plain-snap/o?softDeleted=true&prettyPrint=false": `"name":"gone"`,
	} {
		if _, body := raw(t, "GET", dbase+path, ""); !strings.Contains(body, want) {
			t.Errorf("%s = %s; want %s", path, body, want)
		}
	}
	if code, _ := raw(t, "GET", dbase+"/storage/v1/b/stale", ""); code != http.StatusNotFound {
		t.Errorf("state from before the import survived: %d", code)
	}
	if code, body := raw(t, "GET", dbase+"/download/storage/v1/b/snap/o/v?alt=media", ""); code != 200 || body != "two" {
		t.Errorf("the live bytes = %d %q", code, body)
	}
	if code, body := raw(t, "DELETE", dbase+"/storage/v1/b/snap/o/v", ""); code != 403 || !strings.Contains(body, "objectUnderActiveHold") {
		t.Errorf("the restored hold does not hold: %d %s", code, body)
	}
}

// HMAC key secrets are excluded, and the archive says so; the restored key
// cannot sign.
func TestSnapshotExcludesHMACSecrets(t *testing.T) {
	src, sbase := snapServer(t)
	code, body := raw(t, "POST", sbase+"/storage/v1/projects/p/hmacKeys?serviceAccountEmail=a@p.iam.gserviceaccount.com", "")
	if code != 200 {
		t.Fatal(body)
	}
	secret := body[strings.Index(body, `"secret"`):]
	secret = strings.Split(secret, `"`)[3]
	var buf bytes.Buffer
	if err := src.ExportState(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatal("the archive holds an HMAC secret")
	}
	if !strings.Contains(buf.String(), `"secretsExcluded":true`) {
		t.Error("the archive does not say secrets were excluded")
	}
	dst, dbase := snapServer(t)
	if err := dst.ImportState(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	id := strings.Split(body[strings.Index(body, `"accessId"`):], `"`)[3]
	if code, _ := raw(t, "GET", dbase+"/storage/v1/projects/p/hmacKeys/"+id, ""); code != 200 {
		t.Errorf("the key's metadata did not survive: %d", code)
	}
	raw(t, "POST", dbase+"/storage/v1/b?project=p", `{"name":"signed"}`)
	if resp, _ := xmlDo(t, "GET", signHMACV4(t, dbase, "/signed/x", id, secret, timeNow(), 60), "", nil); resp.StatusCode != 403 {
		t.Errorf("a restored key signed a URL: %d", resp.StatusCode)
	}
}

// A blob whose bytes do not match fails the import and changes nothing.
func TestSnapshotImportVerifiesBytes(t *testing.T) {
	src, sbase := snapServer(t)
	populate(t, sbase)
	var buf bytes.Buffer
	if err := src.ExportState(&buf); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(buf.Bytes(), []byte("two"), []byte("TWO"), 1)
	dst, dbase := snapServer(t)
	raw(t, "POST", dbase+"/storage/v1/b?project=p", `{"name":"kept"}`)
	if err := dst.ImportState(bytes.NewReader(tampered)); err == nil {
		t.Fatal("a tampered archive was imported")
	}
	if code, _ := raw(t, "GET", dbase+"/storage/v1/b/kept", ""); code != 200 {
		t.Errorf("a failed import changed the state: %d", code)
	}
}

func timeNow() time.Time { return time.Now() }
