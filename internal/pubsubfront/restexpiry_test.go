package pubsubfront

import (
	"encoding/json"
	"net/http"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

func answeredTTL(body map[string]any) any {
	p, _ := body["expirationPolicy"].(map[string]any)
	return p["ttl"]
}

// A REST PATCH of expirationPolicy (#891, #908) is applied by the front as
// gRPC's is: a ttl Google refuses is refused; the path alone never reaches
// the emulator and is answered with the subscription as it now reads; with
// other paths, the emulator is sent those alone; and every REST read, one
// subscription or the project's list, names the kept policy until a PUT of
// the name drops it.
func TestRESTPatchSetsTheExpirationPolicy(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "t")
	name := subName("s")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(1)}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/" + project + "/subscriptions/s"
	held := `{"name":"` + name + `","topic":"` + topic + `","ackDeadlineSeconds":10,"expirationPolicy":{"ttl":"172800s"}}`
	up.answer(http.MethodGet, path, held)
	up.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions", `{"subscriptions":[`+held+`]}`)

	for _, bad := range []string{
		`{"subscription":{"expirationPolicy":{"ttl":"43200s"}},"updateMask":"expirationPolicy"}`,
		`{"subscription":{"expirationPolicy":{"ttl":"172800s"},"messageRetentionDuration":"259200s"},"updateMask":"expirationPolicy,messageRetentionDuration"}`,
	} {
		if code, body := fx.rest(t, http.MethodPatch, path, bad); code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d %v, want 400", bad, code, body)
		}
	}
	if code, _ := fx.rest(t, http.MethodPatch, "/v1/projects/"+project+"/subscriptions/missing",
		`{"subscription":{"expirationPolicy":{"ttl":"259200s"}},"updateMask":"expirationPolicy"}`); code != http.StatusNotFound {
		t.Errorf("PATCH of a subscription that does not exist = %d, want 404", code)
	}

	code, body := fx.rest(t, http.MethodPatch, path, `{"subscription":{"expirationPolicy":{"ttl":"259200s"}},"updateMask":"expirationPolicy"}`)
	if code != http.StatusOK || answeredTTL(body) != "259200s" || body["name"] != name {
		t.Errorf("PATCH expirationPolicy = %d %v, want the subscription with a 3-day ttl", code, body)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the emulator saw %d calls, want none", n)
	}
	if _, body := fx.rest(t, http.MethodGet, path, ""); answeredTTL(body) != "259200s" || body["ackDeadlineSeconds"] != float64(10) {
		t.Errorf("GET = %v, want the kept 3-day ttl and every other field", body)
	}
	_, body = fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/subscriptions", "")
	if subs, _ := body["subscriptions"].([]any); len(subs) != 1 || answeredTTL(subs[0].(map[string]any)) != "259200s" {
		t.Errorf("the list = %v, want the kept 3-day ttl", body)
	}

	// Mixed, with snake_case keys (a JSON FieldMask is camelCase): the
	// emulator is sent the deadline alone.
	code, _ = fx.rest(t, http.MethodPatch, path,
		`{"subscription":{"ack_deadline_seconds":30,"expiration_policy":{}},"update_mask":"ackDeadlineSeconds,expirationPolicy"}`)
	calls := up.calls()
	var sent map[string]any
	_ = json.Unmarshal(calls[len(calls)-1].body, &sent)
	if sub, _ := sent["subscription"].(map[string]any); code != http.StatusOK || sent["update_mask"] != "ackDeadlineSeconds" ||
		sub["ack_deadline_seconds"] != float64(30) || sub["expiration_policy"] != nil {
		t.Errorf("a mixed PATCH = %d, sent %s; want the deadline alone", code, calls[len(calls)-1].body)
	}
	if _, body := fx.rest(t, http.MethodGet, path, ""); body["expirationPolicy"] == nil || answeredTTL(body) != nil {
		t.Errorf("GET after the mixed PATCH = %v, want a policy with no ttl", body)
	}

	// A PUT of the name is a new subscription with its own policy.
	fx.rest(t, http.MethodPut, path, `{"topic":"`+topic+`"}`)
	if _, body := fx.rest(t, http.MethodGet, path, ""); answeredTTL(body) != "172800s" {
		t.Errorf("GET after a PUT = %v, want the emulator's policy", body)
	}
}
