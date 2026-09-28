package netfwd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// The Host check in front of a tunnel (#725).
//
// `kubectl port-forward` copies bytes. On its own it puts the emulators'
// HTTP APIs on the host with no Host check, so a DNS-rebound page could
// reach them on their fixed ports (ADR-0004). For a guarded target, kubectl
// listens on a loopback port nothing publishes, and the published address
// is this proxy. It speaks both protocols an emulator port carries:
//
//   - HTTP/1.1, the REST surface a browser can reach. Each request's Host is
//     checked, not only a connection's first one;
//   - cleartext HTTP/2 with prior knowledge (h2c), which is how the official
//     SDKs speak gRPC to an emulator. gRPC is passed without a check, as on
//     every listener CloudBurrow serves (hostguard.Wrap): no browser can
//     send it. A non-gRPC h2c request's :authority is still checked.
//
// Each request goes upstream in the protocol it arrived in, with its Host
// or :authority unchanged. gRPC streams in both directions at once: the
// request body is copied as the client sends it, each response frame is
// flushed as it arrives, and trailers are passed through, so StreamingPull
// and Firestore's Listen behave as they do on a raw tunnel.
//
// Measured against the pinned images (#725): Pub/Sub (8085), Firestore
// (8080) and Datastore (8081) serve REST over HTTP/1.1 and gRPC over h2c on
// the same port; BigQuery's REST port (9050) serves HTTP/1.1 only. Those are
// the guarded tunnels. Spanner's 9010, Bigtable's 8086 and BigQuery's
// Storage Read port 9060 speak gRPC only and do not answer HTTP/1.1,
// so a browser has nothing to reach there and they stay raw.

// guardProxy is the checked listener in front of one tunnel.
type guardProxy struct {
	srv *http.Server
	ln  net.Listener
	log *gatedLog
}

// startGuard listens on addr and proxies every request whose Host passes
// hostguard to upstream, kubectl's own listener.
func startGuard(addr, upstream string, logf func(string, ...any)) (*guardProxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	// Every line the guard logs goes through the gate, so close can promise
	// the caller's logf is not called once it returns.
	gl := &gatedLog{logf: logf}
	logf = gl.printf
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:   guardHandler(upstream, logf),
		Protocols: &protocols,
		// grpc-java, the server behind the Java emulators, sets no stream
		// limit; the Go default of 250 per connection could leave a
		// client's new calls queued behind its long-lived streams, so the
		// proxy is not made the tighter limit.
		HTTP2:    &http.HTTP2Config{MaxConcurrentStreams: 1 << 16},
		ErrorLog: log.New(logWriter(logf), "", 0),
	}
	go func() { _ = srv.Serve(ln) }()
	return &guardProxy{srv: srv, ln: ln, log: gl}, nil
}

// close stops the listener and drops every connection through it. Once it
// returns, the guard no longer calls logf.
//
// http.Server.Close does not wait for handlers: a proxied request whose
// connection it drops can still be copying a body, and logs the copy's
// error after Close has returned. The Forwarder's Logf may be gone by then
// (in tests it is t.Logf, which must not be called once the test is over),
// so anything logged after close is dropped: it can only be the teardown
// of connections the caller has already let go of.
func (g *guardProxy) close() {
	if g != nil {
		_ = g.srv.Close()
		g.log.stop()
	}
}

// gatedLog passes lines to logf until stop, and none after it.
type gatedLog struct {
	mu      sync.Mutex
	stopped bool
	logf    func(string, ...any)
}

func (l *gatedLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stopped && l.logf != nil {
		l.logf(format, args...)
	}
}

// stop returns once no call to logf is in progress, and none follows.
func (l *gatedLog) stop() {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
}

// guardHandler checks Host, then proxies to upstream.
//
// gRPC (cleartext HTTP/2 with an application/grpc content type) is not
// checked, as hostguard.Wrap does not check it: browsers speak HTTP/2 only
// over TLS, so no page can send it, and a gRPC client's :authority is
// whatever name it dialed, which a check could only break. Everything else,
// HTTP/1.1 and any other h2c request, is checked.
func guardHandler(upstream string, logf func(string, ...any)) http.Handler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dial := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext

	var h1 http.Protocols
	h1.SetHTTP1(true)
	var h2c http.Protocols
	h2c.SetUnencryptedHTTP2(true)
	transport := protocolTransport{
		// DisableCompression: the proxy passes bodies through as the
		// emulator wrote them, never asking for or undoing gzip itself.
		http1: &http.Transport{Protocols: &h1, DialContext: dial, DisableCompression: true,
			MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second},
		grpc: &http.Transport{Protocols: &h2c, DialContext: dial, DisableCompression: true,
			IdleConnTimeout: 90 * time.Second},
		h2c: &http.Transport{Protocols: &h2c, DialContext: dial, DisableCompression: true,
			IdleConnTimeout: 90 * time.Second},
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = upstream
			// The emulator sees the name the client used, as through a raw
			// tunnel.
			pr.Out.Host = pr.In.Host
		},
		Transport: transport,
		// No FlushInterval: ReverseProxy already flushes every write of a
		// response with no Content-Length, which is every gRPC stream and
		// every chunked REST response, so each is passed on as it arrives.
		// A gRPC error with no messages ("Trailers-Only") has an empty body,
		// is not flushed early, and leaves as the one HEADERS frame it
		// arrived as (TestGuardKeepsTrailersOnly).
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				// The client went away; there is nobody to answer.
				return
			}
			logf("tunnel guard: %s %s: %v", r.Method, r.URL.Path, err)
			if isGRPC(r) {
				writeGRPCStatus(w, grpcUnavailable, "cloudburrow: the tunnel to the emulator is not carrying: "+err.Error())
				return
			}
			http.Error(w, "cloudburrow: the tunnel to the emulator is not carrying: "+err.Error(), http.StatusBadGateway)
		},
		ErrorLog: log.New(logWriter(logf), "", 0),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isGRPC(r) {
			proxy.ServeHTTP(w, r)
			return
		}
		if !hostguard.Allowed(r.Host) {
			hostguard.Refuse(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// protocolTransport sends a request upstream in the protocol it arrived in.
// The emulators' REST handlers are HTTP/1.1, and gRPC needs HTTP/2. gRPC and
// any other h2c request go on connections of their own, never one pooled
// connection carrying both: the Pub/Sub front gives a connection whose first
// request is gRPC to grpc-go's own transport, which answers anything else
// 415 (internal/pubsubfront, #950).
type protocolTransport struct {
	http1, grpc, h2c http.RoundTripper
}

func (t protocolTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case isGRPC(r):
		return t.grpc.RoundTrip(r)
	case r.ProtoMajor == 2:
		return t.h2c.RoundTrip(r)
	}
	return t.http1.RoundTrip(r)
}

// grpcUnavailable is the gRPC status the proxy answers with when the
// tunnel behind it is down.
const grpcUnavailable = 14

func isGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// writeGRPCStatus answers a gRPC call with a Trailers-Only status, so the
// client reports code and message rather than an HTTP status it cannot
// map.
func writeGRPCStatus(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Status", fmt.Sprint(code))
	h.Set("Grpc-Message", grpcPercentEncode(msg))
	w.WriteHeader(http.StatusOK)
}

// grpcPercentEncode encodes a grpc-message value: every byte outside
// printable ASCII, and '%', as %XX (the gRPC HTTP/2 protocol spec).
func grpcPercentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// logWriter adapts logf to the io.Writer a log.Logger writes to.
type logWriter func(string, ...any)

func (l logWriter) Write(p []byte) (int, error) {
	if l != nil {
		l("tunnel guard: %s", strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
