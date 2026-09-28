package pubsubfront

import (
	"encoding/json"
	"net/http"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// #928: Terraform's google provider sends a PATCH's mask in the URL,
// camelCase, with bigqueryConfig in every update. The front reads it as it
// reads the body's: an expirationPolicy change is applied by the front, and
// answered by it when nothing else is left; the rest reaches the emulator in
// the body, in snake_case, and never in the URL, which the emulator reads as
// one path; a bigqueryConfig that sets nothing on a subscription with none
// is left out, and one that sets something is not; the push relay and
// Google's checks apply to a URL mask too.
func TestRESTPatchReadsTheMaskInTheURL(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureOpts(t, true, up.addr())
	topic := fx.topic(t, "t")
	name := subName("tf")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(1)}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/" + project + "/subscriptions/tf"
	last := func() (restCall, map[string]any) {
		t.Helper()
		calls := up.calls()
		if len(calls) == 0 {
			t.Fatal("the emulator was sent nothing")
		}
		var sent map[string]any
		_ = json.Unmarshal(calls[len(calls)-1].body, &sent)
		return calls[len(calls)-1], sent
	}

	code, body := fx.rest(t, http.MethodPatch, path+"?updateMask=expirationPolicy,bigqueryConfig",
		`{"subscription":{"expirationPolicy":{"ttl":"259200s"}}}`)
	if code != http.StatusOK || answeredTTL(body) != "259200s" {
		t.Errorf("PATCH ?updateMask=expirationPolicy,bigqueryConfig = %d %v, want the 3-day ttl", code, body)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the emulator was sent %d calls, want none", n)
	}

	code, _ = fx.rest(t, http.MethodPatch, path+"?updateMask=ackDeadlineSeconds,bigqueryConfig",
		`{"subscription":{"ackDeadlineSeconds":30}}`)
	call, sent := last()
	if code != http.StatusOK || call.query != "" || sent["updateMask"] != "ack_deadline_seconds" {
		t.Errorf("a URL mask with the deadline = %d, sent ?%s %s; want the deadline alone, in the body", code, call.query, call.body)
	}

	code, _ = fx.rest(t, http.MethodPatch, path+"?updateMask=bigqueryConfig",
		`{"subscription":{"bigqueryConfig":{"table":"p.d.t"}}}`)
	if call, sent = last(); code != http.StatusOK || sent["updateMask"] != "bigquery_config" {
		t.Errorf("a PATCH that sets a BigQuery export = %d, sent %s; want its path kept", code, call.body)
	}

	code, _ = fx.rest(t, http.MethodPatch, path+"?updateMask=pushConfig",
		`{"subscription":{"pushConfig":{"pushEndpoint":"https://example.test/push"}}}`)
	if call, _ = last(); code != http.StatusOK || seenEndpoint(t, call.body, "subscription") != fx.front.relayEndpoint(name, "https://example.test/push") {
		t.Errorf("a URL mask naming pushConfig = %d, sent %s; want the relay's endpoint", code, call.body)
	}

	before := len(up.calls())
	for _, q := range []string{
		"?updateMask=expirationPolicy",                                     // 12h
		"?updateMask=messageRetentionDuration&updateMask=expirationPolicy", // under the retention
	} {
		code, body := fx.rest(t, http.MethodPatch, path+q,
			`{"subscription":{"expirationPolicy":{"ttl":"43200s"},"messageRetentionDuration":"259200s"}}`)
		if code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d %v, want 400", q, code, body)
		}
	}
	if len(up.calls()) != before {
		t.Error("a refused PATCH reached the emulator")
	}
}

func TestCamelToSnake(t *testing.T) {
	for in, want := range map[string]string{
		"expirationPolicy": "expiration_policy", "pushConfig.pushEndpoint": "push_config.push_endpoint",
		"ack_deadline_seconds": "ack_deadline_seconds", "labels": "labels",
	} {
		if got := camelToSnake(in); got != want {
			t.Errorf("camelToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}
