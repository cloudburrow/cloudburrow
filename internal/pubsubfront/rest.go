package pubsubfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// The emulator's REST API is Pub/Sub's v1 JSON API, the HTTP bindings of
// google/pubsub/v1/pubsub.proto. The calls on a subscription are:
//
//	PUT    /v1/projects/{p}/subscriptions/{s}                CreateSubscription, the body a Subscription
//	PATCH  /v1/projects/{p}/subscriptions/{s}                UpdateSubscription, the body {subscription, updateMask}
//	GET    /v1/projects/{p}/subscriptions/{s}                GetSubscription
//	DELETE /v1/projects/{p}/subscriptions/{s}                DeleteSubscription
//	POST   /v1/projects/{p}/subscriptions/{s}:{verb}         pull, acknowledge, modifyAckDeadline,
//	                                                         modifyPushConfig, seek, detach
//	PUT    /v1/projects/{p}/snapshots/{n}                    CreateSnapshot, the body {subscription}
//
// The front applies to them the rules it applies to gRPC: the same checks
// on a create or an update, the 31-day default, an update of the
// expiration policy applied by the front (restexpiry.go), the push relay and the
// refusal of exactly-once delivery with push or export (restrelay.go), and
// every call naming a subscription is activity on it. Every
// /v1/projects/{p}/... path records its project. Everything else passes
// through unchanged.

// maxRESTBody bounds the body the front reads to check a create or update;
// a Subscription is a few hundred bytes.
const maxRESTBody = 4 << 20

// RESTHandler serves the emulator's REST API, forwarding every request to
// it.
func (f *Front) RESTHandler() http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: f.rest})
	// A pull may wait for messages; nothing is buffered on the way back.
	proxy.FlushInterval = -1
	proxy.ModifyResponse = f.restAnswer
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeRESTError(w, status.Errorf(codes.Unavailable, "the Pub/Sub emulator: %v", err))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serveREST(w, r, proxy) })
}

// restPath is the resource a REST path names: the collection
// ("subscriptions" or "snapshots"), its full name, and the custom verb after
// a colon, if any.
func restPath(path string) (collection, name, verb string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/v1/")
	if !ok {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[1] == "" {
		return "", "", "", false
	}
	id, verb, _ := strings.Cut(parts[3], ":")
	if id == "" || (parts[2] != "subscriptions" && parts[2] != "snapshots") {
		return "", "", "", false
	}
	return parts[2], "projects/" + parts[1] + "/" + parts[2] + "/" + id, verb, true
}

var restJSON = protojson.UnmarshalOptions{DiscardUnknown: true}

func (f *Front) serveREST(w http.ResponseWriter, r *http.Request, next http.Handler) {
	f.restProject(r.URL.Path)
	if _, rewritten := answersSubscriptions(r); rewritten {
		// Its push endpoints and expiration policies are rewritten, which
		// needs the answer as it is; the transport asks for gzip itself and
		// undoes it.
		r.Header.Del("Accept-Encoding")
	}
	collection, name, verb, ok := restPath(r.URL.Path)
	if !ok {
		next.ServeHTTP(w, r)
		return
	}
	if collection == "snapshots" {
		// Creating a snapshot names its subscription in the body.
		if r.Method == http.MethodPut && verb == "" {
			body, err := readBody(r)
			if err != nil {
				writeRESTError(w, err)
				return
			}
			var req pubsubpb.CreateSnapshotRequest
			if restJSON.Unmarshal(body, &req) == nil {
				f.touch(req.GetSubscription())
			}
		}
		next.ServeHTTP(w, r)
		return
	}
	switch {
	case verb == "" && r.Method == http.MethodPut:
		f.restCreate(w, r, name, next)
	case verb == "" && r.Method == http.MethodPatch:
		body, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return
		}
		var req pubsubpb.UpdateSubscriptionRequest
		if restJSON.Unmarshal(body, &req) == nil {
			if req.Subscription == nil {
				req.Subscription = &pubsubpb.Subscription{}
			}
			req.Subscription.Name = name
			if err := f.checkUpdate(r.Context(), &req); err != nil {
				writeRESTError(w, err)
				return
			}
			if masks(req.GetUpdateMask().GetPaths(), "push_config") {
				if b, changed := editField(body, func(v json.RawMessage) (json.RawMessage, bool) {
					return f.toRelayJSON(name, v)
				}, "subscription"); changed {
					setBody(r, b)
					body = b
				}
			}
			if f.restPatchExpiration(w, r, name, body, &req) {
				f.touch(name)
				return
			}
		}
		f.touch(name)
		next.ServeHTTP(w, r)
		f.touch(name)
	case verb == "modifyPushConfig" && r.Method == http.MethodPost:
		f.touch(name)
		body, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return
		}
		var req pubsubpb.ModifyPushConfigRequest
		if restJSON.Unmarshal(body, &req) == nil {
			if err := f.checkModifyPush(r.Context(), name, req.GetPushConfig()); err != nil {
				writeRESTError(w, err)
				return
			}
			if b, changed := editField(body, func(v json.RawMessage) (json.RawMessage, bool) {
				return editPushConfig(v, func(ep string) string { return f.relayEndpoint(name, ep) })
			}, "pushConfig", "push_config"); changed {
				setBody(r, b)
			}
		}
		next.ServeHTTP(w, r)
		f.touch(name)
	case verb == "" && r.Method == http.MethodDelete:
		rec := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.ok() {
			f.forget(name)
		}
	default:
		// A pull is active until it returns.
		f.touch(name)
		next.ServeHTTP(w, r)
		f.touch(name)
	}
}

// restCreate checks a subscription's expiration policy, gives it the default
// when it has none, and records it once the emulator has created it.
func (f *Front) restCreate(w http.ResponseWriter, r *http.Request, name string, next http.Handler) {
	body, err := readBody(r)
	if err != nil {
		writeRESTError(w, err)
		return
	}
	var s pubsubpb.Subscription
	if restJSON.Unmarshal(body, &s) == nil {
		if err := checkPolicy(s.GetExpirationPolicy(), s.GetMessageRetentionDuration()); err != nil {
			writeRESTError(w, err)
			return
		}
		if err := checkExactlyOnce(&s); err != nil {
			writeRESTError(w, err)
			return
		}
		if s.ExpirationPolicy == nil {
			if b, err := withDefaultPolicy(body); err == nil {
				body = b
			}
		}
		if b, changed := f.toRelayJSON(name, body); changed {
			body = b
		}
		setBody(r, body)
	} // else the emulator's refusal is the answer
	rec := &statusWriter{ResponseWriter: w}
	next.ServeHTTP(rec, r)
	if rec.ok() {
		f.touch(name)
	}
}

// withDefaultPolicy is a Subscription's JSON with Google's default
// expiration policy added, and every other field as it was sent.
func withDefaultPolicy(body []byte) ([]byte, error) {
	m := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
	}
	// Present only as null, which protojson reads as unset.
	delete(m, "expiration_policy")
	m["expirationPolicy"] = json.RawMessage(fmt.Sprintf(`{"ttl":"%ds"}`, int64(DefaultTTL.Seconds())))
	return json.Marshal(m)
}

// readBody reads a request's body and puts it back, so it can still be
// forwarded.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxRESTBody+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "read the request: %v", err)
	}
	if len(b) > maxRESTBody {
		return nil, status.Errorf(codes.InvalidArgument, "the request body is over %d bytes", maxRESTBody)
	}
	setBody(r, b)
	return b, nil
}

func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// writeRESTError answers with Google's JSON error body for a gRPC status,
// which gcloud and the REST clients read the message from.
func writeRESTError(w http.ResponseWriter, err error) {
	st := status.Convert(err)
	code := httpStatus(st.Code())
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": st.Message(), "status": rpccode.Code_name[int32(st.Code())],
	}})
}

func httpStatus(c codes.Code) int {
	switch c {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// statusWriter remembers the status the emulator answered with.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if fl, ok := s.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *statusWriter) ok() bool { return s.code >= 200 && s.code < 300 }
