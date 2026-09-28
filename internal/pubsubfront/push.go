package pubsubfront

// The push relay (#880): the emulator makes a push subscription's pushes
// itself, so the front could not see whether one succeeded, and a push
// subscription never expired. Google counts "successful pushes" as activity
// (https://cloud.google.com/pubsub/docs/subscription-properties). With the
// relay on, the front hands the emulator a push endpoint of its own for every
// push subscription, an address on the pod's loopback that names the
// subscription and the real endpoint, and restores the real endpoint in
// every subscription the emulator returns, so no client ever sees the relay's.
// The relay forwards each push to the real endpoint unchanged, with its
// headers and body, gives the emulator the endpoint's own answer, and counts
// a success, as Google defines one, as activity on the subscription.
//
// The relay's address is stateless: it carries the subscription and the
// endpoint, base64url encoded, so nothing is lost if the front restarts, and
// a subscription saved by `cloudburrow state save` is saved with its real
// endpoint.

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// relayPath is the relay's path prefix.
const relayPath = "/push/"

// pushSucceeded is Google's list of the status codes a push endpoint
// acknowledges a message with: "To acknowledge the message, return one of
// the following status codes: 102, 200, 201, 202, 204"
// (https://cloud.google.com/pubsub/docs/push). Anything else is a failed
// push.
func pushSucceeded(code int) bool {
	switch code {
	case 102, 200, 201, 202, 204:
		return true
	}
	return false
}

// RelayPushes serves the push relay on l until ctx ends, and from then on
// the front rewrites push endpoints to it. It must be called before the
// front serves its first call.
func (f *Front) RelayPushes(ctx context.Context, l net.Listener) {
	f.mu.Lock()
	f.relayBase = "http://" + l.Addr().String() + relayPath
	f.mu.Unlock()
	srv := &http.Server{Handler: http.HandlerFunc(f.relay), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.logf("pubsub front: push relay: %v", err)
		}
	}()
}

// relaying is the relay's URL prefix, or "" when the relay is off.
func (f *Front) relaying() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relayBase
}

var b64 = base64.RawURLEncoding

// relayEndpoint is what the emulator is given in place of endpoint, the push
// endpoint of subscription sub. With the relay off, or no endpoint, it is
// endpoint unchanged.
func (f *Front) relayEndpoint(sub, endpoint string) string {
	base := f.relaying()
	if base == "" || endpoint == "" || strings.HasPrefix(endpoint, base) {
		return endpoint
	}
	return base + b64.EncodeToString([]byte(sub)) + "/" + b64.EncodeToString([]byte(endpoint))
}

// parseRelay reads a relay path's subscription and endpoint.
func parseRelay(rest string) (sub, endpoint string, ok bool) {
	a, b, found := strings.Cut(rest, "/")
	if !found {
		return "", "", false
	}
	s, err1 := b64.DecodeString(a)
	e, err2 := b64.DecodeString(b)
	if err1 != nil || err2 != nil || len(s) == 0 || len(e) == 0 {
		return "", "", false
	}
	return string(s), string(e), true
}

// realEndpoint is the endpoint a relay URL stands for, or u itself.
func (f *Front) realEndpoint(u string) string {
	base := f.relaying()
	if base == "" || !strings.HasPrefix(u, base) {
		return u
	}
	if _, e, ok := parseRelay(strings.TrimPrefix(u, base)); ok {
		return e
	}
	return u
}

// toRelay rewrites a subscription's push endpoint to the relay; it reports
// whether it changed anything.
func (f *Front) toRelay(s *pubsubpb.Subscription) bool {
	p := s.GetPushConfig()
	if p.GetPushEndpoint() == "" {
		return false
	}
	r := f.relayEndpoint(s.GetName(), p.GetPushEndpoint())
	if r == p.PushEndpoint {
		return false
	}
	p.PushEndpoint = r
	return true
}

// fromRelay restores a subscription's real push endpoint; it reports whether
// it changed anything.
func (f *Front) fromRelay(s *pubsubpb.Subscription) bool {
	p := s.GetPushConfig()
	if p.GetPushEndpoint() == "" {
		return false
	}
	e := f.realEndpoint(p.GetPushEndpoint())
	if e == p.PushEndpoint {
		return false
	}
	p.PushEndpoint = e
	return true
}

// hopByHop are the headers a proxy does not pass on (RFC 9110, 7.6.1).
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Proxy-Connection": true,
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[k] = append([]string(nil), v...)
	}
}

// relay forwards one push to its real endpoint.
func (f *Front) relay(w http.ResponseWriter, r *http.Request) {
	sub, endpoint, ok := parseRelay(strings.TrimPrefix(r.URL.Path, relayPath))
	if !ok || !strings.HasPrefix(r.URL.Path, relayPath) {
		http.Error(w, "not a push relay address", http.StatusNotFound)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint, r.Body)
	if err != nil {
		http.Error(w, "push endpoint: "+err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.ContentLength = r.ContentLength
	resp, err := f.pushClient.Do(req)
	if err != nil {
		// A push that reached nothing failed; the emulator retries it.
		http.Error(w, "push endpoint: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if pushSucceeded(resp.StatusCode) {
		f.touch(sub)
	}
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
