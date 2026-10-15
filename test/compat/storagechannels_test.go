//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storagev1 "google.golang.org/api/storage/v1"
)

// TestStorageObjectChangeChannels: object change notification (#1281). A
// channel registered with objects.watchAll gets the documented sync message,
// then one POST per change (exists for an upload, not_exists for a delete)
// with the X-Goog-Channel-ID, -Token, Resource-State, -Resource-ID,
// -Resource-URI and -Message-Number headers and the object resource as its
// body; channels.stop, through the generated client's Channels.Stop, ends it.
//
// The pinned google.golang.org/api (discovery revision 20260821) no longer
// generates Objects.WatchAll, since Google removed it from the discovery
// document, so watchAll is sent as the older client sent it: POST
// b/{bucket}/o/watch with the Channel as the body, decoded into the
// generated client's storagev1.Channel.
//
// The receiver is an httptest server on this machine's loopback, so the
// storage server must run here too: the builtin `cloudburrow storage-server`,
// as CI's Builtin Cloud Storage step runs it.
// covers: storage.channels.stop
func TestStorageObjectChangeChannels(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	svc := storageJSON(t, h)
	c := storageClient(t, h)
	bucket := h.Project() + "-channels"
	if err := c.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := c.Bucket(bucket).Objects(context.Background(), nil)
		for o, err := it.Next(); err == nil; o, err = it.Next() {
			_ = c.Bucket(bucket).Object(o.Name).Delete(context.Background())
		}
		_ = c.Bucket(bucket).Delete(context.Background())
	})

	type msg struct {
		h    http.Header
		body []byte
	}
	got := make(chan msg, 16)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- msg{r.Header.Clone(), b}
	}))
	t.Cleanup(hook.Close)

	in, _ := json.Marshal(storagev1.Channel{Id: "compat-channel", Type: "web_hook", Address: hook.URL + "/notify",
		Token: "compat-token", Expiration: time.Now().Add(time.Hour).UnixMilli()})
	resp, err := http.Post(svc.BasePath+"b/"+bucket+"/o/watch", "application/json", bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var ch storagev1.Channel
	err = json.NewDecoder(resp.Body).Decode(&ch)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || ch.Kind != "api#channel" || ch.Id != "compat-channel" || ch.ResourceId == "" {
		t.Fatalf("objects.watchAll = %d %+v (%v)", resp.StatusCode, ch, err)
	}

	next := func(state, number string) msg {
		t.Helper()
		select {
		case m := <-got:
			for k, want := range map[string]string{
				"X-Goog-Channel-ID": "compat-channel", "X-Goog-Channel-Token": "compat-token",
				"X-Goog-Resource-ID": ch.ResourceId, "X-Goog-Resource-URI": ch.ResourceUri,
				"X-Goog-Resource-State": state, "X-Goog-Message-Number": number,
			} {
				if m.h.Get(k) != want {
					t.Errorf("%s message: %s = %q, want %q", state, k, m.h.Get(k), want)
				}
			}
			return m
		case <-time.After(15 * time.Second):
			t.Fatalf("no %s message within 15s", state)
		}
		return msg{}
	}
	if m := next("sync", "1"); len(m.body) != 0 {
		t.Errorf("the sync message has a body: %s", m.body)
	}
	w := c.Bucket(bucket).Object("watched.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var obj storagev1.Object
	if m := next("exists", "2"); json.Unmarshal(m.body, &obj) != nil || obj.Name != "watched.txt" || obj.Bucket != bucket || obj.Size != 5 {
		t.Errorf("the exists message's body is not the object: %s", m.body)
	}
	if err := c.Bucket(bucket).Object("watched.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if m := next("not_exists", "3"); !strings.Contains(string(m.body), `"watched.txt"`) {
		t.Errorf("the not_exists message's body is not the object: %s", m.body)
	}

	if err := svc.Channels.Stop(&storagev1.Channel{Id: ch.Id, ResourceId: ch.ResourceId}).Context(ctx).Do(); err != nil {
		t.Fatalf("Channels.Stop: %v", err)
	}
	if err := svc.Channels.Stop(&storagev1.Channel{Id: ch.Id, ResourceId: ch.ResourceId}).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("a second Channels.Stop = %v, want 404", err)
	}
	w = c.Bucket(bucket).Object("after-stop.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		t.Errorf("a stopped channel got a message: %v", m.h)
	case <-time.After(2 * time.Second):
	}
}
