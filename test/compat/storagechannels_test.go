//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
// The receiver is an httptest server on this machine's loopback. It passes
// against the builtin `cloudburrow storage-server` on this machine, as CI's
// Builtin Cloud Storage step runs it, and on an `up` instance, as CI's
// storage shard runs it, where the in-cluster server keeps the messages and
// the CLI takes them through the tunnel and POSTs them from here (#1290).
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

// TestStorageObjectChangeChannelSelection: objects.watchAll takes
// objects.list's parameters, and a channel delivers the changes of exactly
// the objects a listing with them shows (#1393): delimiter with
// includeTrailingDelimiter, startOffset and endOffset, matchGlob and prefix,
// each checked against the generated client's Objects.List with the same
// parameters. With versions=true a noncurrent version's metadata update is
// delivered; without it, it is not.
//
// watchAll is sent as in TestStorageObjectChangeChannels, since the pinned
// client no longer generates it.
// covers: storage.objects.list
func TestStorageObjectChangeChannelSelection(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	svc := storageJSON(t, h)
	bucket := h.Project() + "-chansel"
	if _, err := svc.Buckets.Insert(h.Project(), &storagev1.Bucket{Name: bucket,
		Versioning: &storagev1.BucketVersioning{Enabled: true}}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		l, _ := svc.Objects.List(bucket).Versions(true).Do()
		if l != nil {
			for _, o := range l.Items {
				_ = svc.Objects.Delete(bucket, o.Name).Generation(o.Generation).Do()
			}
		}
		_ = svc.Buckets.Delete(bucket).Do()
	})

	var mu sync.Mutex
	got := map[string][]string{} // channel id: the names of its changes
	var last time.Time
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var o storagev1.Object
		_ = json.NewDecoder(r.Body).Decode(&o)
		mu.Lock()
		defer mu.Unlock()
		last = time.Now()
		if r.Header.Get("X-Goog-Resource-State") != "sync" {
			id := r.Header.Get("X-Goog-Channel-ID")
			got[id] = append(got[id], r.Header.Get("X-Goog-Resource-State")+" "+o.Name)
		}
	}))
	t.Cleanup(hook.Close)

	type sel struct {
		query url.Values
		list  func(*storagev1.ObjectsListCall) *storagev1.ObjectsListCall
	}
	cases := map[string]sel{
		"delim": {url.Values{"delimiter": {"/"}, "includeTrailingDelimiter": {"true"}},
			func(c *storagev1.ObjectsListCall) *storagev1.ObjectsListCall {
				return c.Delimiter("/").IncludeTrailingDelimiter(true)
			}},
		"under": {url.Values{"prefix": {"d/"}, "delimiter": {"/"}},
			func(c *storagev1.ObjectsListCall) *storagev1.ObjectsListCall { return c.Prefix("d/").Delimiter("/") }},
		"range": {url.Values{"startOffset": {"b"}, "endOffset": {"d"}},
			func(c *storagev1.ObjectsListCall) *storagev1.ObjectsListCall {
				return c.StartOffset("b").EndOffset("d")
			}},
		"glob": {url.Values{"matchGlob": {"**.json"}},
			func(c *storagev1.ObjectsListCall) *storagev1.ObjectsListCall { return c.MatchGlob("**.json") }},
	}
	var stops []*storagev1.Channel
	watch := func(id string, q url.Values) {
		t.Helper()
		in, _ := json.Marshal(storagev1.Channel{Id: id, Type: "web_hook", Address: hook.URL + "/" + id})
		resp, err := http.Post(svc.BasePath+"b/"+bucket+"/o/watch?"+q.Encode(), "application/json", bytes.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		var ch storagev1.Channel
		err = json.NewDecoder(resp.Body).Decode(&ch)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || ch.Id != id {
			t.Fatalf("objects.watchAll %s = %d %+v (%v)", q.Encode(), resp.StatusCode, ch, err)
		}
		stops = append(stops, &ch)
	}
	t.Cleanup(func() {
		for _, ch := range stops {
			_ = svc.Channels.Stop(&storagev1.Channel{Id: ch.Id, ResourceId: ch.ResourceId}).Do()
		}
	})
	for id, c := range cases {
		watch(id, c.query)
	}
	watch("live", url.Values{"prefix": {"v/"}})
	watch("vers", url.Values{"prefix": {"v/"}, "versions": {"true"}})

	put := func(name string) *storagev1.Object {
		t.Helper()
		o, err := svc.Objects.Insert(bucket, &storagev1.Object{Name: name}).Media(strings.NewReader("x")).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	for _, n := range []string{"a.txt", "b.json", "c.txt", "d.txt", "d/", "d/e.json", "d/f/g.txt"} {
		put(n)
	}
	first := put("v/x")
	put("v/x") // the first becomes noncurrent
	if _, err := svc.Objects.Patch(bucket, "v/x", &storagev1.Object{Metadata: map[string]string{"k": "v"}}).
		Generation(first.Generation).Context(ctx).Do(); err != nil {
		t.Fatalf("patching the noncurrent version: %v", err)
	}
	// Wait until no message has arrived for 3 seconds.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		quiet := !last.IsZero() && time.Since(last) > 3*time.Second
		mu.Unlock()
		if quiet {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for id, c := range cases {
		l, err := c.list(svc.Objects.List(bucket)).Context(ctx).Do()
		if err != nil {
			t.Fatalf("Objects.List for %s: %v", id, err)
		}
		var want []string
		for _, o := range l.Items {
			want = append(want, "exists "+o.Name)
		}
		if strings.Join(got[id], ", ") != strings.Join(want, ", ") {
			t.Errorf("channel %s (%s) got [%s]; Objects.List with the same parameters lists [%s]",
				id, c.query.Encode(), strings.Join(got[id], ", "), strings.Join(want, ", "))
		}
	}
	if w := "exists v/x, exists v/x"; strings.Join(got["live"], ", ") != w {
		t.Errorf("channel live got [%s], want [%s]: no noncurrent change", strings.Join(got["live"], ", "), w)
	}
	if w := "exists v/x, not_exists v/x, exists v/x, exists v/x"; strings.Join(got["vers"], ", ") != w {
		t.Errorf("channel vers (versions=true) got [%s], want [%s]", strings.Join(got["vers"], ", "), w)
	}
}
