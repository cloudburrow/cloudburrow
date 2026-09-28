package pubsubfront

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// seenEndpoint is the push endpoint in a Subscription's JSON as the REST
// upstream received it, under key when it is wrapped (a PATCH's
// "subscription"), and "" if there is none.
func seenEndpoint(t *testing.T, body []byte, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the upstream got %q: %v", body, err)
	}
	if key != "" {
		m, _ = m[key].(map[string]any)
	}
	pc, _ := m["pushConfig"].(map[string]any)
	ep, _ := pc["pushEndpoint"].(string)
	return ep
}

func answeredEndpoint(body map[string]any) string {
	pc, _ := body["pushConfig"].(map[string]any)
	ep, _ := pc["pushEndpoint"].(string)
	return ep
}

// A push endpoint sent over REST reaches the emulator as the relay's, on
// PUT, on a PATCH that masks pushConfig and on :modifyPushConfig, with every
// other field as it was sent; each answer names the real endpoint.
func TestRESTPushEndpointsGoToTheRelay(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureOpts(t, true, up.addr())
	relay := fx.front.relaying()
	path := "/v1/projects/" + project + "/subscriptions/push"
	real := "https://example.test/push?token=a&b=<c>"

	code, body := fx.rest(t, http.MethodPut, path,
		`{"topic":"projects/p/topics/t","ackDeadlineSeconds":42,"pushConfig":{"pushEndpoint":"`+real+`","attributes":{"x-goog-version":"v1"}}}`)
	if code != http.StatusOK || answeredEndpoint(body) != real {
		t.Errorf("PUT answered %d %v, want the real endpoint", code, body)
	}
	calls := up.calls()
	got := calls[len(calls)-1].body
	if ep := seenEndpoint(t, got, ""); ep != fx.front.relayEndpoint(subName("push"), real) || !strings.HasPrefix(ep, relay) {
		t.Errorf("the emulator was sent %q, want the relay's endpoint", ep)
	}
	var sent map[string]any
	_ = json.Unmarshal(got, &sent)
	pc, _ := sent["pushConfig"].(map[string]any)
	attrs, _ := pc["attributes"].(map[string]any)
	if sent["ackDeadlineSeconds"] != float64(42) || attrs["x-goog-version"] != "v1" || sent["expirationPolicy"] == nil {
		t.Errorf("the emulator was sent %s; want every other field kept and the default policy", got)
	}

	// PATCH with pushConfig in the mask; the spelling protojson also reads.
	code, body = fx.rest(t, http.MethodPatch, path,
		`{"subscription":{"push_config":{"push_endpoint":"`+real+`"}},"updateMask":"pushConfig"}`)
	calls = up.calls()
	var patched map[string]map[string]map[string]string
	_ = json.Unmarshal(calls[len(calls)-1].body, &patched)
	if ep := patched["subscription"]["push_config"]["push_endpoint"]; !strings.HasPrefix(ep, relay) {
		t.Errorf("PATCH sent %s, want the relay's endpoint", calls[len(calls)-1].body)
	}
	if code != http.StatusOK {
		t.Errorf("PATCH answered %d %v", code, body)
	}
	// A PATCH that does not mask pushConfig is sent as it came.
	const other = `{"subscription":{"pushConfig":{"pushEndpoint":"https://example.test/x"}},"updateMask":"ackDeadlineSeconds"}`
	fx.rest(t, http.MethodPatch, path, other)
	if calls = up.calls(); string(calls[len(calls)-1].body) != other {
		t.Errorf("a PATCH without pushConfig in its mask was sent as %s", calls[len(calls)-1].body)
	}

	code, _ = fx.rest(t, http.MethodPost, path+":modifyPushConfig", `{"pushConfig":{"pushEndpoint":"`+real+`"}}`)
	calls = up.calls()
	if ep := seenEndpoint(t, calls[len(calls)-1].body, ""); code != http.StatusOK || ep != fx.front.relayEndpoint(subName("push"), real) {
		t.Errorf(":modifyPushConfig = %d, sent %q; want the relay's endpoint", code, ep)
	}
	// Back to pull: nothing to rewrite.
	fx.rest(t, http.MethodPost, path+":modifyPushConfig", `{"pushConfig":{}}`)
	if calls = up.calls(); string(calls[len(calls)-1].body) != `{"pushConfig":{}}` {
		t.Errorf("a pull :modifyPushConfig was sent as %s", calls[len(calls)-1].body)
	}
}

// What the emulator answers a GET of one subscription, or of a project's
// subscriptions, names the real endpoint and keeps every other field; with
// the relay off, nothing is rewritten.
func TestRESTReadsNameTheRealEndpoint(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureOpts(t, true, up.addr())
	real := "https://example.test/push"
	held := fx.front.relayEndpoint(subName("push"), real)
	one := `{"name":"` + subName("push") + `","topic":"projects/p/topics/t","ackDeadlineSeconds":10,"pushConfig":{"pushEndpoint":"` + held + `"}}`
	up.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions/push", one)
	up.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions",
		`{"subscriptions":[`+one+`,{"name":"`+subName("pull")+`","pushConfig":{}}],"nextPageToken":"n"}`)

	code, body := fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/subscriptions/push", "")
	if code != http.StatusOK || answeredEndpoint(body) != real || body["ackDeadlineSeconds"] != float64(10) {
		t.Errorf("GET = %d %v, want the real endpoint and every other field", code, body)
	}
	code, body = fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/subscriptions", "")
	subs, _ := body["subscriptions"].([]any)
	if code != http.StatusOK || len(subs) != 2 || body["nextPageToken"] != "n" {
		t.Fatalf("list = %d %v", code, body)
	}
	if first, _ := subs[0].(map[string]any); answeredEndpoint(first) != real {
		t.Errorf("the list names %v, want the real endpoint", first)
	}

	// Without the relay, the answer is the emulator's.
	up2 := newRESTUpstream(t)
	plain := newFixtureREST(t, up2.addr())
	up2.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions/push", one)
	if _, body := plain.rest(t, http.MethodGet, "/v1/projects/"+project+"/subscriptions/push", ""); answeredEndpoint(body) != held {
		t.Errorf("without the relay, GET = %v, want the emulator's answer", body)
	}
}

// Exactly-once delivery with push or an export is refused on REST as on
// gRPC: on PUT, on a PATCH whose result would have both, and on
// :modifyPushConfig for an exactly-once subscription. None reaches the
// emulator.
func TestRESTRefusesExactlyOnceWithPush(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureOpts(t, true, up.addr())
	topic := fx.topic(t, "t")
	// Made over gRPC, so the front's reads (from the in-memory Pub/Sub)
	// find them.
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("eod"), Topic: topic, EnableExactlyOnceDelivery: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("pushed"), Topic: topic,
		PushConfig: &pubsubpb.PushConfig{PushEndpoint: "https://example.test/p"}}); err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/" + project + "/subscriptions/"
	for _, c := range []struct{ name, method, path, body string }{
		{"PUT push", http.MethodPut, base + "new", `{"topic":"` + topic + `","enableExactlyOnceDelivery":true,"pushConfig":{"pushEndpoint":"https://example.test/p"}}`},
		{"PUT bigquery", http.MethodPut, base + "new", `{"topic":"` + topic + `","enable_exactly_once_delivery":true,"bigqueryConfig":{"table":"p.d.t"}}`},
		{"PATCH push onto exactly-once", http.MethodPatch, base + "eod", `{"subscription":{"pushConfig":{"pushEndpoint":"https://example.test/p"}},"updateMask":"pushConfig"}`},
		{"PATCH exactly-once onto push", http.MethodPatch, base + "pushed", `{"subscription":{"enableExactlyOnceDelivery":true},"updateMask":"enableExactlyOnceDelivery"}`},
		{":modifyPushConfig", http.MethodPost, base + "eod:modifyPushConfig", `{"pushConfig":{"pushEndpoint":"https://example.test/p"}}`},
	} {
		before := len(up.calls())
		code, body := fx.rest(t, c.method, c.path, c.body)
		e, _ := body["error"].(map[string]any)
		msg, _ := e["message"].(string)
		if code != http.StatusBadRequest || e["status"] != "INVALID_ARGUMENT" || !strings.Contains(msg, "exactly-once") {
			t.Errorf("%s = %d %v, want 400 INVALID_ARGUMENT", c.name, code, body)
		}
		if len(up.calls()) != before {
			t.Errorf("%s reached the emulator", c.name)
		}
	}
	// Turning exactly-once on for a pull subscription passes.
	if code, body := fx.rest(t, http.MethodPatch, base+"eod",
		`{"subscription":{"enableExactlyOnceDelivery":true},"updateMask":"enableExactlyOnceDelivery"}`); code != http.StatusOK {
		t.Errorf("exactly-once on a pull subscription = %d %v", code, body)
	}
}

// A project named only in REST paths is listed by ProjectsMethod.
func TestRESTProjectsAreListed(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	fx.rest(t, http.MethodPut, "/v1/projects/rest-only/topics/t", `{}`)
	fx.rest(t, http.MethodGet, "/v1/projects/rest-listed/subscriptions", "")
	fx.rest(t, http.MethodGet, "/v1/projects/NOT-VALID/topics", "")
	var l structpb.ListValue
	if err := fx.conn.Invoke(context.Background(), ProjectsMethod, &emptypb.Empty{}, &l, grpc.WaitForReady(true)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range l.GetValues() {
		got = append(got, v.GetStringValue())
	}
	if strings.Join(got, ",") != "rest-listed,rest-only" {
		t.Errorf("projects = %v, want rest-listed and rest-only", got)
	}
}
