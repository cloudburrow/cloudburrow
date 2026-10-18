package bigqueryfront

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// The engine reads a client request repeats (#1623). Several checks the
// front makes of one request each read the same dataset or table from the
// engine: a streamed insert read its table twice (refuseSnapshotWrite,
// then insertAll), a DML statement its target twice, and each read costs
// an engine request, which finds its whole project. Within one client
// request, a datasets.get or tables.get the engine answered 200 is
// answered again from that answer, until anything else is sent to the
// engine: any other request, a write or not, forgets every answer kept,
// before it is sent and again once it is answered. A read never outlives
// the client request that made it, so another client's change, and the
// engine's restart, are seen by the next request as before.

type readMemoKey struct{}

// readMemo is one client request's engine answers.
type readMemo struct {
	mu sync.Mutex
	// gen counts what was sent to the engine other than a kept read, so
	// that a read answered while it was in flight is not kept.
	gen     uint64
	answers map[string]*recorder
}

// withReadMemo is ctx with a memo of its own, for one client request.
func withReadMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, readMemoKey{}, &readMemo{})
}

// withRequestReads gives each request h serves a memo of its own.
func withRequestReads(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withReadMemo(r.Context())))
	})
}

// memoKey is the key of a datasets.get or tables.get, or "" for any
// other request.
func memoKey(r *http.Request) string {
	if r.Method != http.MethodGet {
		return ""
	}
	m := route.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil || m[3] == "" || m[5] != "" || strings.HasSuffix(r.URL.EscapedPath(), "/tables") {
		return ""
	}
	return r.Host + " " + r.URL.EscapedPath() + "?" + r.URL.RawQuery
}

// memoReads is next, the engine, with the repeated reads of a request
// whose context has a memo (withReadMemo) answered from the memo.
func memoReads(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		memo, _ := r.Context().Value(readMemoKey{}).(*readMemo)
		if memo == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := memoKey(r)
		if key == "" {
			memo.forget()
			defer memo.forget()
			next.ServeHTTP(w, r)
			return
		}
		memo.mu.Lock()
		kept, gen := memo.answers[key], memo.gen
		memo.mu.Unlock()
		if kept != nil {
			kept.copyCloneTo(w)
			return
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusOK {
			memo.mu.Lock()
			if memo.gen == gen {
				if memo.answers == nil {
					memo.answers = map[string]*recorder{}
				}
				memo.answers[key] = rec
			}
			memo.mu.Unlock()
		}
		rec.copyCloneTo(w)
	})
}

// forget drops every answer kept.
func (m *readMemo) forget() {
	m.mu.Lock()
	m.gen++
	m.answers = nil
	m.mu.Unlock()
}

// copyCloneTo writes the recorded answer to w and leaves it as it was,
// so that it can be written again.
func (r *recorder) copyCloneTo(w http.ResponseWriter) {
	for k, v := range r.header {
		w.Header()[k] = append([]string(nil), v...)
	}
	w.Header().Set("Content-Length", fmt.Sprint(r.body.Len()))
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(bytes.Clone(r.body.Bytes()))
}
