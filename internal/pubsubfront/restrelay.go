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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

// rewriteJSON is a Subscription's JSON as a client reads it: its real push
// endpoint and the expiration policy and labels the front keeps for it.
func (f *Front) rewriteJSON(sub json.RawMessage) (json.RawMessage, bool) {
	a, relayed := f.fromRelayJSON(sub)
	b, kept := f.withPolicyJSON(a)
	c, labelled := f.withLabelsJSON(b)
	return c, relayed || kept || labelled
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

// restAnswer rewrites the emulator's answer so every subscription in it
// names its real push endpoint and the expiration policy and labels the
// front keeps for it (restexpiry.go), and every topic the labels (#949,
// restlabels.go). A successful create drops what the front kept, and a
// successful PATCH keeps what it set. An answer over maxRESTBody is not
// held whole: a list is rewritten as it streams, and a single subscription
// passed on as it came (#926). It is the reverse proxy's ModifyResponse.
func (f *Front) restAnswer(resp *http.Response) error {
	if resp.Request == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	kind, list, ok := answersKept(resp.Request)
	if !ok {
		return nil
	}
	rewrite := f.rewriteJSON
	name, _, one := topicPath(resp.Request.URL.Path)
	if kind == "topics" {
		rewrite = f.withLabelsJSON
	} else {
		_, name, _, one = restPath(resp.Request.URL.Path)
	}
	if one && !list {
		switch resp.Request.Method {
		case http.MethodPut:
			// A new subscription or topic: its policy and labels are the
			// ones it was created with, which the emulator keeps.
			f.dropKept(name)
		case http.MethodPatch:
			if p, ok := resp.Request.Context().Value(pendingKey{}).(*pending); ok {
				if p.policy != nil {
					f.setPolicy(name, p.policy)
				}
				if p.labels != nil {
					f.setLabels(name, p.labels)
				}
			}
		}
	}
	if resp.Header.Get("Content-Encoding") != "" {
		// serveREST asks for no encoding; one the emulator applied anyway
		// is passed on as it came.
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRESTBody+1))
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	if len(body) > maxRESTBody {
		// Too big to hold whole (#926): what was read and the rest are
		// passed on together, a list rewritten one subscription at a time
		// as it streams, a single subscription (never this big) as it came.
		whole := readCloser{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		if !list {
			resp.Body = whole
			return nil
		}
		// Set before the body is read: the transport reads the length
		// once it has been.
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Body = f.streamList(whole, kind, rewrite)
		return nil
	}
	// Closed once the answer is set, as the transport reads its length
	// when the body is closed.
	defer func(orig io.Closer) { _ = orig.Close() }(resp.Body)
	out := body
	if list {
		out, _ = editField(body, func(v json.RawMessage) (json.RawMessage, bool) {
			var items []json.RawMessage
			if json.Unmarshal(v, &items) != nil {
				return v, false
			}
			changed := false
			for i, s := range items {
				var c bool
				items[i], c = rewrite(s)
				changed = changed || c
			}
			if !changed {
				return v, false
			}
			b, err := encodeJSON(items)
			return b, err == nil
		}, kind)
	} else {
		out, _ = rewrite(body)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// readCloser reads from one reader and closes another.
type readCloser struct {
	io.Reader
	io.Closer
}

// streamList is a list's JSON, read from body, with every item under key
// ("subscriptions" or "topics") rewritten by rewrite, as it streams: only
// one item is held at a time, whatever the size of the page. Every other
// field is passed on as it came. An answer that is not such an object ends
// the stream with an error, so the client sees a failed read, never a short
// answer.
func (f *Front) streamList(body io.ReadCloser, key string, rewrite func(json.RawMessage) (json.RawMessage, bool)) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer body.Close()
		w := bufio.NewWriter(pw)
		err := rewriteListOf(w, body, key, rewrite)
		if err == nil {
			err = w.Flush()
		}
		if err != nil {
			f.logf("pubsub front: stream a list of %s: %v", key, err)
		}
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// rewriteList copies a ListSubscriptionsResponse from r to w, rewriting
// each subscription.
func (f *Front) rewriteList(w io.Writer, r io.Reader) error {
	return rewriteListOf(w, r, "subscriptions", f.rewriteJSON)
}

// rewriteListOf copies a list from r to w, rewriting each item under key.
func rewriteListOf(w io.Writer, r io.Reader, listKey string, rewrite func(json.RawMessage) (json.RawMessage, bool)) error {
	dec := json.NewDecoder(r)
	delim := func(want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); !ok || d != want {
			return fmt.Errorf("the emulator's answer has %v where %v belongs", tok, want)
		}
		return nil
	}
	write := func(b []byte) error { _, err := w.Write(b); return err }
	if err := delim('{'); err != nil {
		return err
	}
	if err := write([]byte("{")); err != nil {
		return err
	}
	for first := true; dec.More(); first = false {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		k, err := encodeJSON(key)
		if err != nil {
			return err
		}
		if !first {
			k = append([]byte(","), k...)
		}
		if err := write(append(k, ':')); err != nil {
			return err
		}
		if key != listKey {
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return err
			}
			if err := write(v); err != nil {
				return err
			}
			continue
		}
		if err := delim('['); err != nil {
			return err
		}
		if err := write([]byte("[")); err != nil {
			return err
		}
		for i := 0; dec.More(); i++ {
			var s json.RawMessage
			if err := dec.Decode(&s); err != nil {
				return err
			}
			out, _ := rewrite(s)
			if i > 0 {
				out = append([]byte(","), out...)
			}
			if err := write(out); err != nil {
				return err
			}
		}
		if err := delim(']'); err != nil {
			return err
		}
		if err := write([]byte("]")); err != nil {
			return err
		}
	}
	if err := delim('}'); err != nil {
		return err
	}
	return write([]byte("}"))
}
