package pubsubfront

// net/http's side of the front (mux.go): HTTP/1.1, and HTTP/2 connections
// whose first request is not gRPC, every request routed by Handler.
//
// An HTTP/1.1 request asking to switch to h2c (RFC 7540 section 3.2:
// "Upgrade: h2c", "Connection: Upgrade, HTTP2-Settings" and one
// HTTP2-Settings header), which net/http does not take, is taken here
// (#950): it is answered 101 Switching Protocols, and the connection is
// served as HTTP/2 from then on, the request itself as its stream 1, as
// `curl --http2` asks for on an http:// URL. A request that asks with a
// malformed HTTP2-Settings is served over HTTP/1.1 as if it had not asked;
// its body is held whole for stream 1, so one over maxRESTBody is refused
// 400, as a body the REST side reads is. RFC 9113 has since deprecated the
// mechanism; it is served because clients such as curl still send it.

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/textproto"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

// frontHTTP is net/http's server for the front, and the connections it
// switched to h2c, which net/http no longer tracks.
type frontHTTP struct {
	*http.Server
	h2 *http2.Server

	mu       sync.Mutex
	upgraded map[net.Conn]bool
	closed   bool
}

// httpServer is net/http's server for the front: HTTP/1.1 and h2c, each
// request routed by Handler, and an HTTP/1.1 Upgrade: h2c taken.
func (f *Front) httpServer(grpcSrv *grpc.Server) *frontHTTP {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	s := &frontHTTP{
		// grpc-go's own transport allows any number of streams on a
		// connection; net/http's default is 100 or so.
		h2:       &http2.Server{MaxConcurrentStreams: 1 << 16},
		upgraded: map[net.Conn]bool{},
	}
	h := f.Handler(grpcSrv)
	s.Server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 1 && s.upgrade(w, r, h) {
				return
			}
			h.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: time.Minute,
		Protocols:         &protocols,
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: 1 << 16},
	}
	return s
}

// Close closes the server and every connection it switched to h2c.
func (s *frontHTTP) Close() {
	_ = s.Server.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for c := range s.upgraded {
		_ = c.Close()
	}
}

// upgrade serves r's connection as HTTP/2 when r asks to switch to h2c, and
// reports whether it did.
func (s *frontHTTP) upgrade(w http.ResponseWriter, r *http.Request, h http.Handler) bool {
	settings, ok := h2cSettings(r.Header)
	if !ok {
		return false
	}
	body, err := readBody(r)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		setBody(r, body)
		return false
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return true
	}
	s.upgraded[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.upgraded, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"); err != nil {
		return true
	}
	if err := rw.Flush(); err != nil {
		return true
	}
	setBody(r, body)
	// The request was HTTP/1.1; stream 1 is HTTP/2.
	r.Header.Del("Upgrade")
	r.Header.Del("Connection")
	r.Header.Del("Http2-Settings")
	// What the client sent after the request, its preface and more, may
	// be in rw's buffer already.
	s.h2.ServeConn(&peeked{Conn: c, r: rw.Reader}, &http2.ServeConnOpts{
		Context:        context.WithoutCancel(r.Context()),
		BaseConfig:     s.Server,
		Handler:        h,
		UpgradeRequest: r,
		Settings:       settings,
	})
	return true
}

// h2cSettings is the SETTINGS payload of a request that asks to switch to
// h2c, and whether it asks.
func h2cSettings(h http.Header) ([]byte, bool) {
	if !httpguts.HeaderValuesContainsToken(h[textproto.CanonicalMIMEHeaderKey("Upgrade")], "h2c") ||
		!httpguts.HeaderValuesContainsToken(h[textproto.CanonicalMIMEHeaderKey("Connection")], "HTTP2-Settings") {
		return nil, false
	}
	v := h[textproto.CanonicalMIMEHeaderKey("HTTP2-Settings")]
	if len(v) != 1 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(v[0])
	if err != nil || len(b)%6 != 0 {
		return nil, false
	}
	return b, true
}
