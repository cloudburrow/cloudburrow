package pubsubfront

// The REST half of #891 (#908): a PATCH .../subscriptions/{s} whose
// updateMask names expirationPolicy is applied by the front, as gRPC's
// UpdateSubscription is (handleUpdate). The path is checked by Google's
// rules (Front.checkUpdate), taken out of the mask the emulator sees, and
// the policy kept once the rest of the update succeeds; a PATCH of that
// path alone never reaches the emulator and is answered with the
// subscription as it now reads. Every Subscription the REST API answers
// with names the kept policy (restAnswer), and a PUT of the name drops it.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// pendingPolicy is the request context key of the expiration policy a PATCH
// sets, which restAnswer keeps once the emulator has accepted the rest.
type pendingPolicy struct{}

// withoutExpiration is a PATCH body with expirationPolicy out of its
// updateMask and its subscription, every other field as it was sent, and
// whether any other path is left.
func withoutExpiration(body []byte) ([]byte, bool) {
	var o jsonObject
	if json.Unmarshal(body, &o) != nil || o == nil {
		return body, false
	}
	raw, key := o.field("updateMask", "update_mask")
	var mask string
	if key == "" || json.Unmarshal(raw, &mask) != nil {
		return body, false
	}
	var rest []string
	for _, p := range strings.Split(mask, ",") {
		if p = strings.TrimSpace(p); p != "" && !masks([]string{p}, "expiration_policy") {
			rest = append(rest, p)
		}
	}
	var err error
	if o[key], err = encodeJSON(strings.Join(rest, ",")); err != nil {
		return body, false
	}
	if sub, name := o.field("subscription"); name != "" {
		var s jsonObject
		if json.Unmarshal(sub, &s) == nil && s != nil {
			delete(s, "expirationPolicy")
			delete(s, "expiration_policy")
			if o[name], err = encodeJSON(s); err != nil {
				return body, false
			}
		}
	}
	out, err := encodeJSON(o)
	if err != nil {
		return body, false
	}
	return out, len(rest) > 0
}

// restPatchExpiration applies the expiration part of a checked PATCH. It
// reports whether it answered the request itself; otherwise r is to be
// forwarded as it now is.
func (f *Front) restPatchExpiration(w http.ResponseWriter, r *http.Request, name string, body []byte, req *pubsubpb.UpdateSubscriptionRequest) bool {
	if !masks(req.GetUpdateMask().GetPaths(), "expiration_policy") {
		return false
	}
	policy := updatedPolicy(req.GetSubscription().GetExpirationPolicy())
	b, forward := withoutExpiration(body)
	if forward {
		setBody(r, b)
		*r = *r.WithContext(context.WithValue(r.Context(), pendingPolicy{}, policy))
		return false
	}
	// The expiration alone: checkUpdate has read the subscription, so it
	// exists, and the emulator is never sent the path.
	cur, err := f.subscription(r.Context(), name)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	f.setPolicy(name, policy)
	f.withPolicy(cur)
	f.fromRelay(cur)
	out, err := protojson.Marshal(cur)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(out)
	return true
}

// withPolicyJSON is a Subscription's JSON with the expiration policy the
// front keeps for it in place of the emulator's, and whether it changed.
func (f *Front) withPolicyJSON(sub json.RawMessage) (json.RawMessage, bool) {
	var s jsonObject
	if json.Unmarshal(sub, &s) != nil || s == nil {
		return sub, false
	}
	var name string
	if raw, key := s.field("name"); key == "" || json.Unmarshal(raw, &name) != nil {
		return sub, false
	}
	f.mu.Lock()
	p, ok := f.policies[name]
	f.mu.Unlock()
	if !ok {
		return sub, false
	}
	pj, err := protojson.Marshal(p)
	if err != nil {
		return sub, false
	}
	delete(s, "expiration_policy")
	s["expirationPolicy"] = pj
	out, err := encodeJSON(s)
	if err != nil {
		return sub, false
	}
	return out, true
}
