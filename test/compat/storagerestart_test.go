//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// Cloud Storage across a restart, by mode (#512; the #481 lesson: state is
// kept or dropped by --mode, never by whether a store exists). CI runs
// TestStorageAcrossRestart three times against one --data-dir: setup, then
// after a persistent restart expecting everything present, then after an
// ephemeral restart expecting everything absent.
const envStorageRestartProbe = "CLOUDBURROW_TEST_STORAGE_RESTART_PROBE"

type storageRestartProbe struct {
	Bucket   string `json:"bucket"`
	Versions int    `json:"versions"`
	Session  string `json:"session"` // a resumable session URI, 256 KiB in
}

// TestStorageAcrossRestart: a bucket, a versioned object and an in-progress
// resumable session are present after a persistent restart and absent
// after an ephemeral one.
func TestStorageAcrossRestart(t *testing.T) {
	phase, path, _ := strings.Cut(os.Getenv(envStorageRestartProbe), ":")
	if phase == "" {
		t.Skipf("%s is not set: CI runs this around restarts of the builtin server", envStorageRestartProbe)
	}
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	switch phase {
	case "setup":
		bh := c.Bucket("restart-probe")
		if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
			t.Fatal(err)
		}
		putObject(t, ctx, bh.Object("doc.txt"), "one")
		putObject(t, ctx, bh.Object("doc.txt"), "two")
		resp, _ := xmlCall(t, h, "POST", "/restart-probe/pending.bin", "", map[string]string{"x-goog-resumable": "start"})
		uri := resp.Header.Get("Location")
		if resp, _ := xmlCall(t, h, "PUT", uri, strings.Repeat("x", 256<<10), map[string]string{"Content-Range": "bytes 0-262143/*"}); resp.StatusCode != http.StatusPermanentRedirect {
			t.Fatalf("the session's first chunk = %d", resp.StatusCode)
		}
		b, _ := json.Marshal(storageRestartProbe{Bucket: "restart-probe", Versions: 2, Session: uri})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	case "present", "absent":
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the probe the setup left: %v", err)
		}
		var p storageRestartProbe
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		bh := c.Bucket(p.Bucket)
		_, err = bh.Attrs(ctx)
		resp, _ := xmlCall(t, h, "PUT", p.Session, "", map[string]string{"Content-Range": "bytes */*"})
		if phase == "absent" {
			if err == nil {
				t.Error("ephemeral mode kept the bucket from the earlier run")
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("ephemeral mode kept the resumable session: %d", resp.StatusCode)
			}
			return
		}
		if err != nil {
			t.Fatalf("persistent mode lost the bucket: %v", err)
		}
		if n := len(versionsOf(t, ctx, bh, true)); n != p.Versions {
			t.Errorf("persistent mode kept %d versions, want %d", n, p.Versions)
		}
		if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Range") != "bytes=0-262143" {
			t.Errorf("persistent mode lost the session's bytes: %d %q", resp.StatusCode, resp.Header.Get("Range"))
		}
	default:
		t.Fatalf("%s must be setup, present or absent with :<file>, not %q", envStorageRestartProbe, phase)
	}
}
