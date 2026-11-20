package firestoreadmin

// The Listen and Write streams in the usage counts (#2050).
//
// Google bills a listener "a read each time a document in the result set
// is added or updated", and "a read when a document is removed from the
// result set because the document has changed" (not when it is deleted),
// with "a minimum charge of one document read for each query that you
// perform, even if the query returns no results"
// (firebase.google.com/docs/firestore/pricing). A write stream's writes
// are billed as any other write. So the front reads the gRPC frames of
// both streams as they pass, both ways:
//   - Listen: each DocumentChange that adds a document to a target's
//     results, or delivers a new version of one, is a read, LOOKUP for a
//     documents target and QUERY for a query target, as is each
//     DocumentRemove (the emulator resets a target and resends every
//     result on a change, which Google neither sends nor bills); a query target that becomes
//     current with no document delivered counts one QUERY read.
//   - Write: before each request's writes pass, the emulator is asked
//     which documents exist; the response that answers them (the streams
//     answer requests in order) counts each as CREATE, UPDATE or a delete,
//     as a commit's.
// The web SDK's WebChannel transport carries the same streams in a framing
// of its own, which the front does not decode.

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"sync"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/proto"
)

// streamMethods are the bidirectional streams the front counts.
var streamMethods = map[string]bool{"Listen": true, "Write": true}

// wrapStream counts a Listen or Write stream as it passes.
func (u *Usage) wrapStream(method string, next http.Handler, w http.ResponseWriter, r *http.Request) {
	s := &streamCount{u: u, ctx: r.Context(), targets: map[int32]string{}, delivered: map[int32]bool{}, current: map[int32]bool{},
		results: map[int32]map[string]string{}}
	var onReq, onRes func([]byte)
	if method == "Listen" {
		onReq, onRes = s.listenRequest, s.listenResponse
	} else {
		onReq, onRes = s.writeRequest, s.writeResponse
	}
	r.Body = &frameReader{rc: r.Body, onMsg: onReq}
	next.ServeHTTP(&frameWriter{ResponseWriter: w, onMsg: onRes}, r)
}

// streamCount is one stream's state.
type streamCount struct {
	u   *Usage
	ctx context.Context

	mu sync.Mutex
	db string
	// Listen: each target's read type, whether a document was delivered
	// for it, and whether it has been current.
	targets   map[int32]string
	delivered map[int32]bool
	current   map[int32]bool
	// results are each target's documents, by name, with the update time
	// each was delivered at: the emulator answers a change by resetting
	// the target and sending every result again, while Google sends, and
	// bills, only the documents added or updated.
	results map[int32]map[string]string
	// Write: each request's writes awaiting their response, with the
	// documents that existed before them.
	pending []pendingWrites
}

type pendingWrites struct {
	req     *firestorepb.CommitRequest
	existed map[string]bool
}

func (s *streamCount) listenRequest(b []byte) {
	var m firestorepb.ListenRequest
	if proto.Unmarshal(b, &m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := dbPrefix.FindString(m.GetDatabase()); d != "" {
		s.db = d
	}
	if t := m.GetAddTarget(); t != nil {
		typ := "QUERY"
		if t.GetDocuments() != nil {
			typ = "LOOKUP"
		}
		s.targets[t.GetTargetId()] = typ
		delete(s.delivered, t.GetTargetId())
		delete(s.current, t.GetTargetId())
		delete(s.results, t.GetTargetId())
	}
	if id := m.GetRemoveTarget(); id != 0 {
		delete(s.targets, id)
		delete(s.results, id)
	}
}

// readType is the type of a read for targets: LOOKUP when one of them is
// a documents target.
func (s *streamCount) readType(ids []int32) string {
	for _, id := range ids {
		if s.targets[id] == "LOOKUP" {
			return "LOOKUP"
		}
	}
	return "QUERY"
}

func (s *streamCount) listenResponse(b []byte) {
	var m firestorepb.ListenResponse
	if proto.Unmarshal(b, &m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case m.GetDocumentChange() != nil:
		dc := m.GetDocumentChange()
		ids := dc.GetTargetIds()
		name := dc.GetDocument().GetName()
		version := dc.GetDocument().GetUpdateTime().AsTime().String()
		changed := false
		for _, id := range ids {
			s.delivered[id] = true
			r := s.results[id]
			if r == nil {
				r = map[string]string{}
				s.results[id] = r
			}
			if v, ok := r[name]; !ok || v != version {
				changed = true
			}
			r[name] = version
		}
		for _, id := range dc.GetRemovedTargetIds() {
			delete(s.results[id], name)
		}
		if changed {
			s.u.addReads(s.db, s.readType(ids), 1)
		}
	case m.GetDocumentRemove() != nil:
		dr := m.GetDocumentRemove()
		for _, id := range dr.GetRemovedTargetIds() {
			delete(s.results[id], dr.GetDocument())
		}
		s.u.addReads(s.db, s.readType(dr.GetRemovedTargetIds()), 1)
	case m.GetDocumentDelete() != nil:
		dd := m.GetDocumentDelete()
		for _, id := range dd.GetRemovedTargetIds() {
			delete(s.results[id], dd.GetDocument())
		}
	case m.GetTargetChange() != nil:
		tc := m.GetTargetChange()
		if tc.GetTargetChangeType() == firestorepb.TargetChange_ADD {
			for _, id := range tc.GetTargetIds() {
				if _, ok := s.targets[id]; !ok {
					s.targets[id] = "QUERY"
				}
			}
		}
		if tc.GetTargetChangeType() != firestorepb.TargetChange_CURRENT {
			return
		}
		ids := tc.GetTargetIds()
		if len(ids) == 0 {
			// Every target of the stream.
			for id := range s.targets {
				ids = append(ids, id)
			}
		}
		for _, id := range ids {
			if s.current[id] {
				continue
			}
			s.current[id] = true
			if s.targets[id] == "QUERY" && !s.delivered[id] {
				s.u.addReads(s.db, "QUERY", 1)
			}
		}
	}
}

func (s *streamCount) writeRequest(b []byte) {
	var m firestorepb.WriteRequest
	if proto.Unmarshal(b, &m) != nil {
		return
	}
	s.mu.Lock()
	if d := dbPrefix.FindString(m.GetDatabase()); d != "" {
		s.db = d
	}
	db := s.db
	s.mu.Unlock()
	if len(m.GetWrites()) == 0 || db == "" {
		return
	}
	req := &firestorepb.CommitRequest{Database: db, Writes: m.GetWrites()}
	existed := s.u.existing(s.ctx, usageCall{method: "Commit", db: db, grpc: true, req: req})
	s.mu.Lock()
	s.pending = append(s.pending, pendingWrites{req: req, existed: existed})
	s.mu.Unlock()
}

func (s *streamCount) writeResponse(b []byte) {
	var m firestorepb.WriteResponse
	if proto.Unmarshal(b, &m) != nil {
		return
	}
	if len(m.GetWriteResults()) == 0 && m.GetCommitTime() == nil {
		// The handshake: a stream ID and token.
		return
	}
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return
	}
	p := s.pending[0]
	s.pending = s.pending[1:]
	s.mu.Unlock()
	// A response carries the request's writes' results: all applied.
	s.u.countWrites(p.req.GetDatabase(), p.req, nil, p.existed)
}

// frameReader passes a gRPC request stream on, each message only once
// onMsg has seen it, so a write's existence is asked before it applies.
type frameReader struct {
	rc      io.ReadCloser
	onMsg   func([]byte)
	pending []byte // bytes of a frame not yet whole
	out     []byte // whole frames, to pass on
	err     error
}

func (f *frameReader) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		if f.err != nil {
			if len(f.pending) > 0 {
				f.out, f.pending = f.pending, nil
				break
			}
			return 0, f.err
		}
		buf := make([]byte, 32<<10)
		n, err := f.rc.Read(buf)
		f.pending = append(f.pending, buf[:n]...)
		f.err = err
		for len(f.pending) >= 5 {
			size := int(binary.BigEndian.Uint32(f.pending[1:5]))
			if len(f.pending) < 5+size {
				break
			}
			if f.pending[0] == 0 && f.onMsg != nil {
				f.onMsg(f.pending[5 : 5+size])
			}
			f.out = append(f.out, f.pending[:5+size]...)
			f.pending = f.pending[5+size:]
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

func (f *frameReader) Close() error { return f.rc.Close() }

// frameWriter passes a gRPC response stream on, showing onMsg each
// message.
type frameWriter struct {
	http.ResponseWriter
	onMsg   func([]byte)
	partial []byte
}

func (w *frameWriter) Write(b []byte) (int, error) {
	w.partial = append(w.partial, b...)
	for len(w.partial) >= 5 {
		n := int(binary.BigEndian.Uint32(w.partial[1:5]))
		if len(w.partial) < 5+n {
			break
		}
		if w.partial[0] == 0 {
			w.onMsg(w.partial[5 : 5+n])
		}
		w.partial = w.partial[5+n:]
	}
	if len(w.partial) == 0 {
		w.partial = nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *frameWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *frameWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
