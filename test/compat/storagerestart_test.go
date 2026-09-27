//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

// Cloud Storage across a restart, by mode (#512; the #481 lesson: state is
// kept or dropped by --mode, never by whether a store exists). CI runs
// TestStorageAcrossRestart three times against one --data-dir: setup, then
// after a persistent restart expecting everything present, then after an
// ephemeral restart expecting everything absent. The compat job does the same
// against the in-cluster server of an instance, through its tunnel (#596).
const envStorageRestartProbe = "CLOUDBURROW_TEST_STORAGE_RESTART_PROBE"

type storageRestartProbe struct {
	Bucket   string `json:"bucket"`
	Versions int    `json:"versions"`
	// Session is a resumable session's path and query, 256 KiB in. Not the
	// whole URI: an instance's tunnel port is OS-assigned, so it changes
	// across a restart, and the session is resumed at the new endpoint.
	Session string `json:"session"`
	// Notification is a notificationConfig's ID on the bucket, and Topic
	// the topic it names. Empty when the server has no Pub/Sub emulator to
	// deliver to, so refuses to create one (501): the check job's builtin
	// server. An instance (CLOUDBURROW_TEST_PUBSUB set) must create it.
	Notification string `json:"notification,omitempty"`
	Topic        string `json:"topic,omitempty"`
}

// TestStorageAcrossRestart: a bucket, a versioned object, an in-progress
// resumable session and a notificationConfig are present after a persistent
// restart and absent after an ephemeral one.
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
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Path == "" {
			t.Fatalf("the session's Location = %q, %v", resp.Header.Get("Location"), err)
		}
		uri := loc.RequestURI()
		if resp, _ := xmlCall(t, h, "PUT", uri, strings.Repeat("x", 256<<10), map[string]string{"Content-Range": "bytes 0-262143/*"}); resp.StatusCode != http.StatusPermanentRedirect {
			t.Fatalf("the session's first chunk = %d", resp.StatusCode)
		}
		p := storageRestartProbe{Bucket: "restart-probe", Versions: 2, Session: uri}
		// After the uploads, so no event is queued for a topic nobody reads.
		n, err := bh.AddNotification(ctx, &storage.Notification{
			TopicProjectID: h.Project(), TopicID: "restart-probe-topic", PayloadFormat: storage.JSONPayload,
		})
		var ge *googleapi.Error
		switch {
		case err == nil:
			p.Notification, p.Topic = n.ID, n.TopicID
		case os.Getenv(EnvPubSub) == "" && errors.As(err, &ge) && ge.Code == http.StatusNotImplemented:
			t.Logf("the server has no Pub/Sub emulator, so no notificationConfig is probed: %v", err)
		default:
			t.Fatalf("AddNotification: %v", err)
		}
		b, _ := json.Marshal(p)
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
			if p.Notification != "" {
				all, err := bh.Notifications(ctx)
				var ge *googleapi.Error
				if err != nil && !(errors.As(err, &ge) && ge.Code == http.StatusNotFound) {
					t.Errorf("list the notificationConfigs after an ephemeral restart: %v", err)
				} else if _, ok := all[p.Notification]; ok {
					t.Errorf("ephemeral mode kept notificationConfig %s", p.Notification)
				}
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
		if p.Notification != "" {
			all, err := bh.Notifications(ctx)
			if err != nil {
				t.Fatalf("list the notificationConfigs after a persistent restart: %v", err)
			}
			if n, ok := all[p.Notification]; !ok || n.TopicID != p.Topic {
				t.Errorf("persistent mode lost notificationConfig %s on topic %s: %v", p.Notification, p.Topic, all)
			}
		}
	default:
		t.Fatalf("%s must be setup, present or absent with :<file>, not %q", envStorageRestartProbe, phase)
	}
}
