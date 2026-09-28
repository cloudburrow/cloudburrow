package pubsubfront

// The REST half of #880's rules (#908). gcloud and Terraform's google
// provider reach the emulator over REST, so everything the front does to a
// gRPC call it does to the REST call that means the same:
//
//   - the push relay: a push endpoint sent in PUT .../subscriptions/{s}
//     (pushConfig.pushEndpoint), in PATCH with an updateMask naming
//     pushConfig, or in POST .../subscriptions/{s}:modifyPushConfig is
//     rewritten to the relay's, and every Subscription the emulator answers
//     with (PUT, GET, PATCH, and GET .../subscriptions) names the real one;
//   - exactly-once delivery with a push endpoint or an export is refused on
//     PUT, PATCH and :modifyPushConfig, by the same checks as gRPC;
//   - the project in a /v1/projects/{p}/... path is recorded for
//     ProjectsMethod, so `cloudburrow state save` finds what only REST made.
//
// The JSON is edited, not re-encoded from a message, so every field the
// front does not touch reaches the emulator, and the client, as it was.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// restProject records the project a /v1/projects/{p}/... path names.
func (f *Front) restProject(path string) {
	rest, ok := strings.CutPrefix(path, "/v1/projects/")
	if !ok {
		return
	}
	p, _, _ := strings.Cut(rest, "/")
	p, _, _ = strings.Cut(p, ":")
	f.sawProjectID(p)
}

// jsonObject is a JSON object with its values left encoded.
type jsonObject map[string]json.RawMessage

// field is the value of the first of names o has, and the name it is under.
func (o jsonObject) field(names ...string) (json.RawMessage, string) {
	for _, n := range names {
		if v, ok := o[n]; ok {
			return v, n
		}
	}
	return nil, ""
}

// encodeJSON is v as JSON, without HTML escaping.
func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// editPushConfig applies fn to a PushConfig's pushEndpoint (either
// spelling, as protojson reads either) and returns the PushConfig with every
// other field as it was, and whether it changed.
func editPushConfig(pushConfig json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	var pc jsonObject
	if json.Unmarshal(pushConfig, &pc) != nil || pc == nil {
		return pushConfig, false
	}
	raw, name := pc.field("pushEndpoint", "push_endpoint")
	var ep string
	if name == "" || json.Unmarshal(raw, &ep) != nil || ep == "" {
		return pushConfig, false
	}
	next := fn(ep)
	if next == ep {
		return pushConfig, false
	}
	var err error
	if pc[name], err = encodeJSON(next); err != nil {
		return pushConfig, false
	}
	out, err := encodeJSON(pc)
	if err != nil {
		return pushConfig, false
	}
	return out, true
}

// editPushEndpoint applies fn to a Subscription's push endpoint, as
// editPushConfig does.
func editPushEndpoint(sub json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	return editField(sub, func(v json.RawMessage) (json.RawMessage, bool) {
		return editPushConfig(v, fn)
	}, "pushConfig", "push_config")
}

// editField applies edit to one field of a JSON object (either spelling)
// and returns the object with every other field as it was, and whether it
// changed.
func editField(body []byte, edit func(json.RawMessage) (json.RawMessage, bool), names ...string) ([]byte, bool) {
	var o jsonObject
	if json.Unmarshal(body, &o) != nil || o == nil {
		return body, false
	}
	v, name := o.field(names...)
	if name == "" {
		return body, false
	}
	next, changed := edit(v)
	if !changed {
		return body, false
	}
	o[name] = next
	out, err := encodeJSON(o)
	if err != nil {
		return body, false
	}
	return out, true
}

// toRelayJSON is a Subscription's JSON with its push endpoint the relay's.
func (f *Front) toRelayJSON(sub string, body []byte) ([]byte, bool) {
	return editPushEndpoint(body, func(ep string) string { return f.relayEndpoint(sub, ep) })
}

// fromRelayJSON is a Subscription's JSON with its real push endpoint.
func (f *Front) fromRelayJSON(body []byte) ([]byte, bool) {
	return editPushEndpoint(body, f.realEndpoint)
}

// answersSubscriptions reports whether the emulator's answer to r is a
// Subscription, or a list of them, whose push endpoints the client must see
// as it sent them.
func answersSubscriptions(r *http.Request) (list, ok bool) {
	collection, _, verb, isSub := restPath(r.URL.Path)
	if isSub && collection == "subscriptions" && verb == "" {
		switch r.Method {
		case http.MethodPut, http.MethodPatch, http.MethodGet:
			return false, true
		}
		return false, false
	}
	// GET /v1/projects/{p}/subscriptions. A topic's subscriptions are names
	// only.
	rest, found := strings.CutPrefix(r.URL.Path, "/v1/projects/")
	if !found || r.Method != http.MethodGet {
		return false, false
	}
	p, c, _ := strings.Cut(rest, "/")
	return true, p != "" && c == "subscriptions"
}

// restoreEndpoints rewrites the emulator's answer so every subscription in it
// names its real push endpoint. It is the reverse proxy's ModifyResponse.
func (f *Front) restoreEndpoints(resp *http.Response) error {
	if f.relaying() == "" || resp.Request == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	list, ok := answersSubscriptions(resp.Request)
	if !ok {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		// serveREST asks for no encoding; one the emulator applied anyway
		// is passed on as it came.
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRESTBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxRESTBody {
		// Too big to have been read whole; nothing is rewritten, and what
		// was read cannot be put back, so the client is told.
		return errTooLarge
	}
	out := body
	if list {
		out, _ = editField(body, func(v json.RawMessage) (json.RawMessage, bool) {
			var subs []json.RawMessage
			if json.Unmarshal(v, &subs) != nil {
				return v, false
			}
			changed := false
			for i, s := range subs {
				var c bool
				subs[i], c = f.fromRelayJSON(s)
				changed = changed || c
			}
			if !changed {
				return v, false
			}
			b, err := encodeJSON(subs)
			return b, err == nil
		}, "subscriptions")
	} else {
		out, _ = f.fromRelayJSON(body)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

var errTooLarge = errors.New("the emulator's answer is over the size the front reads")
