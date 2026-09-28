package pubsubfront

// The REST half of #949 (labels.go): a PATCH .../topics/{t} or
// .../subscriptions/{s} (restexpiry.go) whose updateMask names labels is
// applied by the front. The path is taken out of the mask the emulator
// sees, from the body and the URL alike, and the rest is sent in the body,
// snake_case, as #928 does for a subscription; the labels are kept once the
// emulator has accepted the rest (restAnswer), and a PATCH of labels alone
// never reaches the emulator and is answered with the resource as it now
// reads. Every Topic the REST API answers with (PUT, GET, PATCH, and GET
// .../topics) names the kept labels, a PUT of the name drops them, and a
// DELETE forgets them.

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// topicPath is the topic a REST path names, /v1/projects/{p}/topics/{t},
// and the custom verb after a colon, if any.
func topicPath(path string) (name, verb string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v1/")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[1] == "" || parts[2] != "topics" {
		return "", "", false
	}
	id, verb, _ := strings.Cut(parts[3], ":")
	if id == "" {
		return "", "", false
	}
	return "projects/" + parts[1] + "/topics/" + id, verb, true
}

// answersKept reports whether the emulator's answer to r is one the front
// rewrites: a Subscription or a Topic, or a list of them. kind is the
// collection, "subscriptions" or "topics", which is also a list's key.
func answersKept(r *http.Request) (kind string, list, ok bool) {
	if list, ok := answersSubscriptions(r); ok {
		return "subscriptions", list, true
	}
	if _, verb, ok := topicPath(r.URL.Path); ok {
		switch {
		case verb != "":
			return "", false, false
		case r.Method == http.MethodPut, r.Method == http.MethodPatch, r.Method == http.MethodGet:
			return "topics", false, true
		}
		return "", false, false
	}
	// GET /v1/projects/{p}/topics.
	rest, found := strings.CutPrefix(r.URL.Path, "/v1/projects/")
	if !found || r.Method != http.MethodGet {
		return "", false, false
	}
	p, c, _ := strings.Cut(rest, "/")
	return "topics", true, p != "" && c == "topics"
}

// serveTopic serves a request on /v1/projects/{p}/topics/{t}: a PUT's
// labels are checked (#962), a PATCH of labels is checked and applied by the
// front, a DELETE forgets the kept labels, and everything else is forwarded
// as it came.
func (f *Front) serveTopic(w http.ResponseWriter, r *http.Request, name, verb string, next http.Handler) {
	switch {
	case verb == "" && r.Method == http.MethodPatch:
		body, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return
		}
		if f.restPatchTopic(w, r, name, body) {
			return
		}
		next.ServeHTTP(w, r)
	case verb == "" && r.Method == http.MethodPut:
		// CreateTopic: its labels are checked (#962).
		body, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return
		}
		var t pubsubpb.Topic
		if restJSON.Unmarshal(body, &t) == nil {
			if err := CheckLabels(t.GetLabels()); err != nil {
				writeRESTError(w, err)
				return
			}
		} // else the emulator's refusal is the answer
		next.ServeHTTP(w, r)
	case verb == "" && r.Method == http.MethodDelete:
		next.ServeHTTP(&statusWriter{ResponseWriter: w, onOK: func() { f.forget(name) }}, r)
	default:
		next.ServeHTTP(w, r)
	}
}

// restPatchTopic applies the labels of a PATCH of a topic, as gRPC's
// UpdateTopic is, and reports whether it answered the request itself;
// otherwise r is to be forwarded as it now is.
func (f *Front) restPatchTopic(w http.ResponseWriter, r *http.Request, name string, body []byte) bool {
	o, key, paths, ok := restUpdateMask(r, body)
	if !ok || !masks(paths, "labels") {
		return false
	}
	var t pubsubpb.Topic
	raw, topicKey := o.field("topic")
	if raw != nil && restJSON.Unmarshal(raw, &t) != nil {
		return false // the emulator's refusal is the answer
	}
	if err := CheckLabels(t.GetLabels()); err != nil {
		writeRESTError(w, err)
		return true
	}
	kept := &pending{labels: updatedLabels(t.GetLabels())}
	forward := without(paths, "labels")
	if len(forward) == 0 {
		cur, err := f.topic(r.Context(), name)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		f.setLabels(name, kept.labels)
		f.withTopicLabels(cur)
		out, err := protojson.Marshal(cur)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(out)
		return true
	}
	if topicKey != "" {
		if b, changed := editObject(o[topicKey], func(t jsonObject) { delete(t, "labels") }); changed {
			o[topicKey] = b
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), pendingKey{}, kept))
	forwardMask(r, o, key, forward)
	return false
}
