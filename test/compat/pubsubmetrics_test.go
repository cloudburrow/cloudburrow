//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// TestPubSubCallsReachMetricsAndTheRequestLog.
//
// Pub/Sub's calls went through its front to the emulator and nowhere else,
// so /metrics, /admin/events and the console's request log saw none of them
// and named Pub/Sub "not measured" (#1266). Calls made with the official
// client must now be counted in /metrics by method and code, appear in
// /admin/events and the console's request log with their method, code,
// resource and duration, and Pub/Sub must no longer be listed unmeasured.
func TestPubSubCallsReachMetricsAndTheRequestLog(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvPubSub) == "" {
		t.Skip(EnvPubSub + " is not set")
	}
	control, console := h.Endpoint(EnvControl), consoleAddr(t, h)
	since := time.Now().Add(-time.Second)
	c := pubsubClient(t, h)
	id := fmt.Sprintf("metrics-%d", time.Now().UnixNano())
	topicName := topic(t, h, c, id)
	const marker = "COMPAT-PUBSUB-PAYLOAD-MARKER"
	if _, err := c.Publisher(topicName).Publish(h.Context(), &pubsub.Message{Data: []byte(marker)}).Get(h.Context()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	missing := fmt.Sprintf("projects/%s/topics/never-created-%s", h.Project(), id)
	if _, err := c.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: missing}); err == nil {
		t.Fatal("GetTopic of a missing topic succeeded")
	}

	// The CLI reads the front's calls every two seconds.
	var ev map[string]adminEvent
	deadline := time.Now().Add(20 * time.Second)
	for {
		ev = methodsOf(events(t, h, control, "pubsub", since))
		if _, ok := ev["GetTopic"]; ok {
			if _, ok := ev["Publish"]; ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pub/Sub calls in /admin/events: %v; want CreateTopic, Publish and GetTopic", ev)
		}
		time.Sleep(250 * time.Millisecond)
	}
	check := func(method, code, resource string) {
		t.Helper()
		e, ok := ev[method]
		switch {
		case !ok:
			t.Errorf("%s was not recorded", method)
		case e.Detail["code"] != code || e.Detail["resource"] != resource || e.Detail["duration_ms"] == "" ||
			e.Detail["transport"] != "grpc":
			t.Errorf("%s recorded %v; want code %s, resource %s, a duration, over grpc", method, e.Detail, code, resource)
		}
	}
	check("CreateTopic", "OK", topicName)
	check("Publish", "OK", topicName)
	check("GetTopic", "NOT_FOUND", missing)
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), marker) {
		t.Fatalf("a message payload reached the event log: %s", raw)
	}

	// /metrics on the control port.
	code, body := adminDo(t, http.MethodGet, "http://"+control+"/metrics", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: %d", code)
	}
	for _, want := range []string{
		`cloudburrow_requests_total{service="pubsub",method="Publish",code="OK"}`,
		`cloudburrow_requests_total{service="pubsub",method="GetTopic",code="NOT_FOUND"}`,
		`cloudburrow_request_duration_seconds_count{service="pubsub",method="Publish",code="OK"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	if strings.Contains(body, `cloudburrow_service_measured{service="pubsub"} 0`) {
		t.Error("/metrics still reports Pub/Sub unmeasured")
	}

	// The console's request log and request metrics.
	code, body = consoleDo(t, console, http.MethodGet, "/api/requests?service=pubsub&project="+h.Project(), "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/requests: %d", code)
	}
	var log struct {
		Requests []struct {
			Method, Resource, Code string
		}
		Unobserved []struct{ Service string }
	}
	if err := json.Unmarshal([]byte(body), &log); err != nil {
		t.Fatalf("decode /api/requests: %v", err)
	}
	found := false
	for _, r := range log.Requests {
		if strings.HasSuffix(r.Method, "/Publish") && r.Resource == topicName && r.Code == "OK" {
			found = true
		}
	}
	if !found {
		t.Errorf("the console's request log has no Publish to %s: %.500s", topicName, body)
	}
	for _, u := range log.Unobserved {
		if u.Service == "pubsub" {
			t.Error("the console's request log still names Pub/Sub unobserved")
		}
	}
	code, body = consoleDo(t, console, http.MethodGet, "/api/metrics/requests", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/metrics/requests: %d", code)
	}
	var rm struct{ Unmeasured []string }
	if err := json.Unmarshal([]byte(body), &rm); err != nil {
		t.Fatalf("decode /api/metrics/requests: %v", err)
	}
	for _, s := range rm.Unmeasured {
		if s == "pubsub" {
			t.Error("the console's request metrics still name Pub/Sub unmeasured")
		}
	}
}
