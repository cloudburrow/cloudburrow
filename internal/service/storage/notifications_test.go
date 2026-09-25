package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type published struct {
	topic string
	data  []byte
	attrs map[string]string
}

// recorder is a Publisher that keeps what it was given, and fails the first
// failures publishes.
type recorder struct {
	mu       sync.Mutex
	got      []published
	failures int
}

func (p *recorder) Publish(_ context.Context, topic string, data []byte, attrs map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures > 0 {
		p.failures--
		return errors.New("unavailable")
	}
	p.got = append(p.got, published{topic, data, attrs})
	return nil
}

func (p *recorder) take() []published {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.got
	p.got = nil
	return out
}

func notifyServer(t *testing.T, bucketBody string) (*Server, *recorder, string) {
	t.Helper()
	pub := &recorder{}
	s, err := NewServer(Options{Publisher: pub})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p", bucketBody); code != 200 {
		t.Fatal(body)
	}
	return s, pub, h.URL
}

func addConfig(t *testing.T, base, bucket, body string) string {
	t.Helper()
	code, resp := raw(t, "POST", base+"/storage/v1/b/"+bucket+"/notificationConfigs", body)
	var c struct {
		ID string `json:"id"`
	}
	if code != 200 || json.Unmarshal([]byte(resp), &c) != nil {
		t.Fatalf("add config = %d %s", code, resp)
	}
	return c.ID
}

func dispatch(t *testing.T, s *Server, pub *recorder) []published {
	t.Helper()
	if _, err := s.DispatchNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pub.take()
}

func events(ps []published) string {
	var out []string
	for _, p := range ps {
		e := p.attrs["eventType"] + " " + p.attrs["objectId"] + "#" + p.attrs["objectGeneration"]
		if g := p.attrs["overwroteGeneration"]; g != "" {
			e += " overwrote=" + g
		}
		if g := p.attrs["overwrittenByGeneration"]; g != "" {
			e += " by=" + g
		}
		out = append(out, e)
	}
	return strings.Join(out, "; ")
}

func genOf(t *testing.T, body string) string {
	t.Helper()
	var o struct {
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatal(body)
	}
	return o.Generation
}

// An overwrite emits OBJECT_FINALIZE with overwroteGeneration, and the
// replaced version's OBJECT_DELETE (unversioned) or OBJECT_ARCHIVE
// (versioned) with overwrittenByGeneration (pubsub-notifications docs).
func TestNotificationOverwroteGenerationAttributes(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprint("versioned=", versioned), func(t *testing.T) {
			body := `{"name":"notes"}`
			if versioned {
				body = `{"name":"notes","versioning":{"enabled":true}}`
			}
			s, pub, base := notifyServer(t, body)
			addConfig(t, base, "notes", `{"topic":"projects/p/topics/t","payload_format":"JSON_API_V1"}`)
			_, b1 := raw(t, "POST", base+"/upload/storage/v1/b/notes/o?uploadType=media&name=o", "one")
			g1 := genOf(t, b1)
			if got := events(dispatch(t, s, pub)); got != "OBJECT_FINALIZE o#"+g1 {
				t.Errorf("first upload = %s", got)
			}
			_, b2 := raw(t, "POST", base+"/upload/storage/v1/b/notes/o?uploadType=media&name=o", "two")
			g2 := genOf(t, b2)
			old := "OBJECT_DELETE"
			if versioned {
				old = "OBJECT_ARCHIVE"
			}
			want := old + " o#" + g1 + " by=" + g2 + "; OBJECT_FINALIZE o#" + g2 + " overwrote=" + g1
			if got := events(dispatch(t, s, pub)); got != want {
				t.Errorf("overwrite = %s\nwant       %s", got, want)
			}
		})
	}
}

// Each mutation's event, the standard attributes, the payload, and the
// filters.
func TestNotificationEventsAndFilters(t *testing.T) {
	s, pub, base := notifyServer(t, `{"name":"evts"}`)
	id := addConfig(t, base, "evts", `{"topic":"//pubsub.googleapis.com/projects/p/topics/all","custom_attributes":{"team":"a","eventType":"LIE"}}`)
	addConfig(t, base, "evts", `{"topic":"projects/p/topics/deletes","event_types":["OBJECT_DELETE"],"object_name_prefix":"logs/","payload_format":"NONE"}`)
	raw(t, "POST", base+"/upload/storage/v1/b/evts/o?uploadType=media&name=logs%2Fa", "x")
	raw(t, "PATCH", base+"/storage/v1/b/evts/o/logs%2Fa", `{"metadata":{"k":"v"}}`)
	raw(t, "POST", base+"/storage/v1/b/evts/o/logs%2Fa/copyTo/b/evts/o/copy", "")
	raw(t, "DELETE", base+"/storage/v1/b/evts/o/logs%2Fa", "")
	got := dispatch(t, s, pub)
	var all, deletes []published
	for _, p := range got {
		switch p.topic {
		case "projects/p/topics/all":
			all = append(all, p)
		case "projects/p/topics/deletes":
			deletes = append(deletes, p)
		}
	}
	var types []string
	for _, p := range all {
		types = append(types, p.attrs["eventType"])
	}
	if strings.Join(types, ",") != "OBJECT_FINALIZE,OBJECT_METADATA_UPDATE,OBJECT_FINALIZE,OBJECT_DELETE" {
		t.Errorf("events on the unfiltered topic = %v", types)
	}
	p := all[0]
	if p.attrs["team"] != "a" || p.attrs["eventType"] != "OBJECT_FINALIZE" || p.attrs["bucketId"] != "evts" ||
		p.attrs["payloadFormat"] != "JSON_API_V1" || p.attrs["notificationConfig"] != "projects/_/buckets/evts/notificationConfigs/"+id ||
		p.attrs["eventTime"] == "" {
		t.Errorf("attributes = %v; a custom attribute must not override eventType", p.attrs)
	}
	var obj map[string]any
	if json.Unmarshal(p.data, &obj) != nil || obj["kind"] != "storage#object" || obj["name"] != "logs/a" ||
		!strings.HasPrefix(obj["selfLink"].(string), "https://storage.googleapis.com/storage/v1/") {
		t.Errorf("payload = %s", p.data)
	}
	if len(deletes) != 1 || deletes[0].attrs["eventType"] != "OBJECT_DELETE" || len(deletes[0].data) != 0 {
		t.Errorf("the filtered NONE configuration got %v", deletes)
	}
}

// A publish that fails stays in the outbox and is published on a later
// pass: at least once, never lost.
func TestNotificationRetriesAfterAFailedPublish(t *testing.T) {
	s, pub, base := notifyServer(t, `{"name":"retry"}`)
	addConfig(t, base, "retry", `{"topic":"projects/p/topics/t"}`)
	pub.failures = 1
	raw(t, "POST", base+"/upload/storage/v1/b/retry/o?uploadType=media&name=o", "x")
	if left, _ := s.DispatchNotifications(context.Background()); left != 1 || len(pub.take()) != 0 {
		t.Fatalf("after a failed publish: %d left", left)
	}
	if got := dispatch(t, s, pub); len(got) != 1 {
		t.Errorf("the retry published %d, want 1", len(got))
	}
	if left, _ := s.DispatchNotifications(context.Background()); left != 0 {
		t.Errorf("%d left after the retry", left)
	}
}

// At most 100 configurations per bucket and 10 custom attributes each; the
// CRUD round-trips; without a Pub/Sub emulator nothing can be configured.
func TestNotificationConfigLimits(t *testing.T) {
	_, _, base := notifyServer(t, `{"name":"limits"}`)
	url := base + "/storage/v1/b/limits/notificationConfigs"
	attrs := map[string]string{}
	for i := 0; i < 11; i++ {
		attrs[fmt.Sprint("k", i)] = "v"
	}
	ab, _ := json.Marshal(map[string]any{"topic": "projects/p/topics/t", "custom_attributes": attrs})
	if code, body := raw(t, "POST", url, string(ab)); code != http.StatusBadRequest || !strings.Contains(body, "custom attributes") {
		t.Errorf("11 custom attributes = %d %s", code, body)
	}
	for i := 0; i < 100; i++ {
		addConfig(t, base, "limits", fmt.Sprintf(`{"topic":"projects/p/topics/t%d"}`, i))
	}
	if code, _ := raw(t, "POST", url, `{"topic":"projects/p/topics/extra"}`); code != http.StatusBadRequest {
		t.Errorf("the 101st configuration = %d", code)
	}
	if code, body := raw(t, "GET", url+"/7", ""); code != 200 || !strings.Contains(body, "//pubsub.googleapis.com/projects/p/topics/t6") {
		t.Errorf("get = %d %s", code, body)
	}
	if code, _ := raw(t, "DELETE", url+"/7", ""); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if code, _ := raw(t, "GET", url+"/7", ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d", code)
	}
	if code, body := raw(t, "POST", url, `{"topic":"projects/p/topics/next"}`); code != 200 || !strings.Contains(body, `"id": "101"`) {
		t.Errorf("an ID after a delete = %d %s; want 101, never reused", code, body)
	}
	if code, _ := raw(t, "POST", url, `{"topic":"not-a-topic"}`); code != http.StatusBadRequest {
		t.Errorf("a bad topic = %d", code)
	}

	_, plain := sdk(t)
	raw(t, "POST", plain.URL+"/storage/v1/b?project=p", `{"name":"nopub"}`)
	if code, _ := raw(t, "POST", plain.URL+"/storage/v1/b/nopub/notificationConfigs", `{"topic":"projects/p/topics/t"}`); code != http.StatusNotImplemented {
		t.Errorf("a configuration with no Pub/Sub to deliver to = %d; want 501", code)
	}
}
