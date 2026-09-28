package pubsubfront

// The REST half of #891 (#908): a PATCH .../subscriptions/{s} whose
// updateMask names expirationPolicy is applied by the front, as gRPC's
// UpdateSubscription is (handleUpdate). The path is checked by Google's
// rules (Front.checkUpdate), taken out of the mask the emulator sees, and
// the policy kept once the rest of the update succeeds; a PATCH of that
// path alone never reaches the emulator and is answered with the
// subscription as it now reads. Every Subscription the REST API answers
// with names the kept policy (restAnswer), and a PUT of the name drops it.
//
// The mask is read from the body's updateMask and from the URL's
// ?updateMask=, which Terraform's google provider sends (#928), and the
// emulator is sent it in the body alone, in snake_case: it reads a query
// mask as one path, commas and all, and refuses camelCase there (measured:
// "ack_deadline_seconds,bigquery_config is not a known Subscription
// field"), while the same paths in the body are applied. An export config
// path (bigqueryConfig, cloudStorageConfig, bigtableConfig) that sets
// nothing on a subscription that has none is a no-op, which the emulator
// would refuse ("bigquery_config is not a known Subscription field") and
// the provider names in every update, so it is taken out too.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// pendingPolicy is the request context key of the expiration policy a PATCH
// sets, which restAnswer keeps once the emulator has accepted the rest.
type pendingPolicy struct{}

// exportPaths are the export configs a subscription may have.
var exportPaths = []string{"bigquery_config", "cloud_storage_config", "bigtable_config"}

// camelToSnake is a field path in snake_case: "pushConfig.pushEndpoint" is
// "push_config.push_endpoint", and a snake_case path is itself.
func camelToSnake(p string) string {
	var b strings.Builder
	for _, c := range p {
		if unicode.IsUpper(c) {
			b.WriteByte('_')
			c = unicode.ToLower(c)
		}
		b.WriteRune(c)
	}
	return b.String()
}

// splitMask is a FieldMask's JSON form, or a query value, as paths.
func splitMask(v string, into []string) []string {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			into = append(into, camelToSnake(p))
		}
	}
	return into
}

// restUpdateMask reads a PATCH: its body as a JSON object, the paths of its
// update mask, from the body and the URL, in snake_case and each once, and
// the key the body's mask is under ("updateMask" when it has none). ok is
// false for a body that is not an object, whose refusal is the emulator's.
func restUpdateMask(r *http.Request, body []byte) (o jsonObject, key string, paths []string, ok bool) {
	if json.Unmarshal(body, &o) != nil || o == nil {
		return nil, "", nil, false
	}
	key = "updateMask"
	raw, name := o.field("updateMask", "update_mask")
	if name != "" {
		key = name
		var mask string
		if json.Unmarshal(raw, &mask) != nil {
			return nil, "", nil, false
		}
		paths = splitMask(mask, paths)
	}
	q := r.URL.Query()
	for _, k := range []string{"updateMask", "update_mask"} {
		for _, v := range q[k] {
			paths = splitMask(v, paths)
		}
	}
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return o, key, out, true
}

// restPatch checks and applies a PATCH of a subscription as gRPC's
// UpdateSubscription is, and reports whether it answered the request
// itself; otherwise r is to be forwarded as it now is.
func (f *Front) restPatch(w http.ResponseWriter, r *http.Request, name string, body []byte) bool {
	o, key, paths, ok := restUpdateMask(r, body)
	if !ok {
		return false
	}
	var s pubsubpb.Subscription
	if raw, _ := o.field("subscription"); raw != nil && restJSON.Unmarshal(raw, &s) != nil {
		return false // the emulator's refusal is the answer
	}
	s.Name = name
	req := &pubsubpb.UpdateSubscriptionRequest{Subscription: &s, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}
	if err := f.checkUpdate(r.Context(), req); err != nil {
		writeRESTError(w, err)
		return true
	}
	subKey := "subscription"
	if _, k := o.field("subscription"); k != "" {
		subKey = k
	}
	// edited is set when the body the emulator is sent is not the one that
	// came, which is otherwise forwarded as it came.
	edited := false
	if masks(paths, "push_config") {
		if b, changed := f.toRelayJSON(name, o[subKey]); changed {
			o[subKey], edited = b, true
		}
	}
	forward := paths
	var policy *pubsubpb.ExpirationPolicy
	if masks(paths, "expiration_policy") {
		policy = updatedPolicy(s.GetExpirationPolicy())
		forward = without(forward, "expiration_policy")
		if b, changed := editObject(o[subKey], func(s jsonObject) {
			delete(s, "expirationPolicy")
			delete(s, "expiration_policy")
		}); changed {
			o[subKey], edited = b, true
		}
	}
	if exports := noopExports(r.Context(), f, name, &s, forward); len(exports) > 0 {
		for _, p := range exports {
			forward = without(forward, p)
		}
	}
	if len(forward) == 0 && len(paths) > 0 {
		// Nothing is left for the emulator: checkUpdate or noopExports has
		// read the subscription, so it exists.
		return f.restAnswerLocally(w, r, name, policy)
	}
	if policy != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), pendingPolicy{}, policy))
	}
	q := r.URL.Query()
	fromQuery := q.Has("updateMask") || q.Has("update_mask")
	if len(paths) == 0 {
		return false // no mask: the emulator's to answer, as it was sent
	}
	if !edited && !fromQuery && len(forward) == len(paths) {
		return false
	}
	delete(o, "updateMask")
	delete(o, "update_mask")
	mask, err := encodeJSON(strings.Join(forward, ","))
	if err != nil {
		return false
	}
	o[key] = mask
	out, err := encodeJSON(o)
	if err != nil {
		return false
	}
	setBody(r, out)
	if fromQuery {
		q.Del("updateMask")
		q.Del("update_mask")
		r.URL.RawQuery = q.Encode()
	}
	return false
}

// without is paths less field and the paths within it.
func without(paths []string, field string) []string {
	var out []string
	for _, p := range paths {
		if !masks([]string{p}, field) {
			out = append(out, p)
		}
	}
	return out
}

// editObject applies edit to a JSON object and returns it, and whether it
// changed.
func editObject(raw json.RawMessage, edit func(jsonObject)) (json.RawMessage, bool) {
	var s jsonObject
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return raw, false
	}
	n := len(s)
	edit(s)
	if len(s) == n {
		return raw, false
	}
	out, err := encodeJSON(s)
	if err != nil {
		return raw, false
	}
	return out, true
}

// noopExports are the export config paths of an update that set nothing on
// a subscription that has none of that export.
func noopExports(ctx context.Context, f *Front, name string, u *pubsubpb.Subscription, paths []string) []string {
	var named []string
	for _, p := range exportPaths {
		for _, q := range paths {
			if q == p {
				named = append(named, p)
			}
		}
	}
	if len(named) == 0 {
		return nil
	}
	cur, err := f.subscription(ctx, name)
	if err != nil {
		return nil // the emulator answers
	}
	var out []string
	for _, p := range named {
		var set, has bool
		switch p {
		case "bigquery_config":
			set, has = u.GetBigqueryConfig() != nil, cur.GetBigqueryConfig() != nil
		case "cloud_storage_config":
			set, has = u.GetCloudStorageConfig() != nil, cur.GetCloudStorageConfig() != nil
		case "bigtable_config":
			set, has = u.GetBigtableConfig() != nil, cur.GetBigtableConfig() != nil
		}
		if !set && !has {
			out = append(out, p)
		}
	}
	return out
}

// restAnswerLocally answers a PATCH the emulator is not sent: it keeps
// policy, when the PATCH set one, and answers with the subscription as it
// now reads.
func (f *Front) restAnswerLocally(w http.ResponseWriter, r *http.Request, name string, policy *pubsubpb.ExpirationPolicy) bool {
	cur, err := f.subscription(r.Context(), name)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	if policy != nil {
		f.setPolicy(name, policy)
	}
	f.withPolicy(cur)
	f.fromRelay(cur)
	out, err := protojson.Marshal(cur)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	f.touch(name)
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
