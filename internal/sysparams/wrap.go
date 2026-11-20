package sysparams

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

// Options configure Wrap.
type Options struct {
	// WriteError writes a malformed system parameter's error in the
	// front's own envelope (Cloud Storage's, BigQuery's). Nil is Google's
	// JSON mapping of google.rpc.Status, under $.xgafv.
	WriteError func(w http.ResponseWriter, r *http.Request, err error)
	// V1Errors says the front's errors are already in the v1 format (they
	// carry errors[]), so $.xgafv leaves them alone.
	V1Errors bool
}

type handledKey struct{}

// handled is set by a handler that applied the system parameters itself
// (the transcoder, Cloud Storage's writeResponse), so Wrap passes its
// answer through.
type handled struct{ done bool }

// MarkHandled tells Wrap, if r passed through it, that the handler has
// applied the system parameters to its answer.
func MarkHandled(r *http.Request) {
	if h, ok := r.Context().Value(handledKey{}).(*handled); ok {
		h.done = true
	}
}

// Wrap applies the system parameters to what h writes, for a hand-written
// JSON front: a JSON success is pruned to fields (or X-Goog-FieldMask),
// indented or compacted by prettyPrint and wrapped in a JSONP callback; a
// JSON error is written in the $.xgafv format and wrapped in the callback.
// Media (alt=media), streams (alt=sse, or a handler that flushes), gRPC and
// anything that is not JSON pass through as written. A request naming none
// of these parameters is not buffered at all.
func Wrap(h http.Handler, o Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") || !wants(r) {
			h.ServeHTTP(w, r)
			return
		}
		p, err := FromRequest(r)
		if err != nil {
			if o.WriteError != nil {
				o.WriteError(w, r, err)
			} else {
				p.WriteError(w, err)
			}
			return
		}
		if p.Alt == "media" || p.Alt == "sse" || p.Alt == "proto" {
			h.ServeHTTP(w, r)
			return
		}
		mark := &handled{}
		r = r.WithContext(context.WithValue(r.Context(), handledKey{}, mark))
		bw := &buffer{w: w, header: w.Header()}
		h.ServeHTTP(bw, r)
		if bw.streaming {
			return
		}
		status := bw.status
		if status == 0 {
			status = http.StatusOK
		}
		body := bw.buf.Bytes()
		ct := w.Header().Get("Content-Type")
		if !mark.done && strings.HasPrefix(ct, "application/json") && len(bytes.TrimSpace(body)) > 0 {
			switch {
			case status < 300:
				body, ct = p.body(body, true, true)
			case status >= 400 && o.V1Errors:
				body, ct = p.body(body, false, true)
			case status >= 400:
				body, ct = p.errorBody(body, true)
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

// wants reports whether r names a system parameter Wrap acts on.
func wants(r *http.Request) bool {
	if r.Header.Get("X-Goog-FieldMask") != "" {
		return true
	}
	q := r.URL.RawQuery
	if q == "" {
		return false
	}
	for _, n := range []string{"fields", "prettyPrint", "callback", "xgafv"} {
		if strings.Contains(q, n) {
			return true
		}
	}
	return false
}

// buffer holds a response until the handler returns, unless the handler
// flushes (a stream), when what it holds is written and the rest passes
// through.
type buffer struct {
	w         http.ResponseWriter
	header    http.Header
	status    int
	buf       bytes.Buffer
	streaming bool
}

func (b *buffer) Header() http.Header { return b.header }

func (b *buffer) WriteHeader(code int) {
	if b.streaming {
		b.w.WriteHeader(code)
		return
	}
	if b.status == 0 {
		b.status = code
	}
}

func (b *buffer) Write(p []byte) (int, error) {
	if b.streaming {
		return b.w.Write(p)
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

// Flush turns the buffer into a pass-through: a handler that flushes is
// streaming, and its answer is written as it comes.
func (b *buffer) Flush() {
	if !b.streaming {
		b.streaming = true
		if b.status == 0 {
			b.status = http.StatusOK
		}
		b.w.WriteHeader(b.status)
		_, _ = b.w.Write(b.buf.Bytes())
		b.buf.Reset()
	}
	if f, ok := b.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (b *buffer) Unwrap() http.ResponseWriter { return b.w }
