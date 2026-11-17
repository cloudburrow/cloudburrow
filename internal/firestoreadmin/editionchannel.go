package firestoreadmin

// The Write stream over WebChannel on Standard databases (#1744).
//
// In a browser the Firebase Web SDK opens the Write stream over WebChannel,
// not gRPC: a session of plain HTTP requests on
// /google.firestore.v1.Firestore/Write/channel, each naming the database in
// its `database` parameter. The client's requests go up in forward-channel
// POSTs, a form of count=N&ofs=K&reqI___data__=<WriteRequest as JSON>; the
// first (the handshake, no SID) opens the session and answers its id. The
// server's answers come down a backchannel GET (TYPE=xmlhttp) as chunks of
// "<length>\n[[<array id>,[<message>]],...]", and an error ends the stream
// as a message of its own, {"error":{"code":400,"message":"...","status":
// "INVALID_ARGUMENT"}}, Google's JSON error, which the SDK turns into the
// same FirestoreError the gRPC stream's status gives
// (WebChannelConnection.openStream reads its status and message).
//
// The emulator serves the transport itself. On a Standard database the
// guard claims each of a session's requests and passes it on through the
// same pass-through the front uses, reading as it goes: a POST whose request
// carries an Enterprise-only value is sent on with the requests before it
// alone, so the emulator never sees it or any after it, and the refusal the
// gRPC stream gets ("Value type INT is not allowed", "Nested arrays are not
// allowed") goes down the session's backchannel with the next array id, the
// open one's, or the next one the client opens; the client then closes the
// channel. Every other database's channel goes to the emulator unclaimed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// writeChannelPath is the Write stream's WebChannel path.
const writeChannelPath = firestoreService + "Write/channel"

// channelIdle is how long a session the guard has seen nothing of is kept.
const channelIdle = 30 * time.Minute

// channelSession is what the guard keeps of a WebChannel session.
type channelSession struct {
	lastID  int64         // the last array id sent down
	refusal error         // a refusal not yet sent down
	wake    chan struct{} // closed when refusal is set; anew once it is sent
	seen    time.Time
}

// channels are the Standard databases' WebChannel Write sessions, by SID.
type channels struct {
	once     sync.Once
	upstream http.Handler

	mu       sync.Mutex
	sessions map[string]*channelSession
}

// claimsWriteChannel reports whether r is a request of a Write stream over
// WebChannel on a Standard database.
func (g *EditionGuard) claimsWriteChannel(r *http.Request) bool {
	return r.URL.Path == writeChannelPath &&
		g.standard(databaseOf(r.URL.Query().Get("database")))
}

// session is the session sid, made if new; nil for no sid.
func (c *channels) session(sid string) *channelSession {
	if sid == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.sessions == nil {
		c.sessions = map[string]*channelSession{}
	}
	s := c.sessions[sid]
	if s == nil {
		for k, old := range c.sessions {
			if now.Sub(old.seen) > channelIdle {
				delete(c.sessions, k)
			}
		}
		s = &channelSession{lastID: -1, wake: make(chan struct{})}
		c.sessions[sid] = s
	}
	s.seen = now
	return s
}

func (c *channels) drop(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, sid)
}

// refuse keeps err to send down sid's backchannel.
func (c *channels) refuse(sid string, err error) {
	s := c.session(sid)
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.refusal == nil {
		s.refusal = err
		close(s.wake)
	}
}

// saw records the array ids of what went down s's backchannel.
func (c *channels) saw(s *channelSession, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id > s.lastID {
		s.lastID = id
	}
}

// takeRefusal is s's refusal and the array id to send it with, and marks
// it sent; nil if there is none.
func (c *channels) takeRefusal(s *channelSession) (error, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := s.refusal
	if err == nil {
		return nil, 0
	}
	s.refusal, s.wake = nil, make(chan struct{})
	s.lastID++
	return err, s.lastID
}

// serveWriteChannel answers a request claimsWriteChannel claimed.
func (g *EditionGuard) serveWriteChannel(w http.ResponseWriter, r *http.Request) {
	c := &g.channels
	c.once.Do(func() { c.upstream = netfwd.PassThrough(g.a.emulator, nil) })
	q := r.URL.Query()
	sid := q.Get("SID")
	switch {
	case q.Get("TYPE") == "terminate":
		c.drop(sid)
		c.upstream.ServeHTTP(w, r)
	case r.Method == http.MethodGet && sid != "":
		g.serveBackchannel(w, r, sid)
	case r.Method == http.MethodPost:
		g.serveForward(w, r, sid)
	default:
		c.upstream.ServeHTTP(w, r)
	}
}

// serveForward passes a forward-channel POST on, up to the first request
// that carries an Enterprise-only value, whose refusal is kept for the
// backchannel.
func (g *EditionGuard) serveForward(w http.ResponseWriter, r *http.Request, sid string) {
	c := &g.channels
	b, err := io.ReadAll(io.LimitReader(r.Body, maxGuardedBody+1))
	if err != nil || len(b) > maxGuardedBody {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
		c.upstream.ServeHTTP(w, r)
		return
	}
	form, perr := url.ParseQuery(string(b))
	count, cerr := strconv.Atoi(form.Get("count"))
	kept, refusal := count, error(nil)
	if perr == nil && cerr == nil {
		for i := 0; i < count; i++ {
			req := &firestorepb.WriteRequest{}
			data := form.Get("req" + strconv.Itoa(i) + "___data__")
			if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(data), req) != nil {
				continue // the emulator's to refuse
			}
			if refusal = checkStandardValues(req.ProtoReflect()); refusal != nil {
				kept = i
				break
			}
		}
	}
	if refusal == nil {
		r.Body = io.NopCloser(bytes.NewReader(b))
		c.upstream.ServeHTTP(w, r)
		return
	}

	// The requests before the refused one go on; it and those after never
	// reach the emulator.
	for i := kept; i < count; i++ {
		form.Del("req" + strconv.Itoa(i) + "___data__")
	}
	form.Set("count", strconv.Itoa(kept))
	if sid == "" {
		// A handshake: the session's id is in the emulator's answer.
		body := form.Encode()
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Length")
		hw := &handshakeWriter{ResponseWriter: w}
		c.upstream.ServeHTTP(hw, r)
		if hw.sid != "" {
			s := c.session(hw.sid)
			c.saw(s, 0)
			c.refuse(hw.sid, refusal)
		}
		return
	}
	s := c.session(sid)
	if aid, err := strconv.ParseInt(queryParam(r, "AID"), 10, 64); err == nil {
		c.saw(s, aid)
	}
	if kept > 0 {
		body := form.Encode()
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Length")
		c.upstream.ServeHTTP(w, r)
		c.refuse(sid, refusal)
		return
	}
	c.refuse(sid, refusal)
	// The forward channel's own answer: [backchannel present, the last
	// array id sent, bytes outstanding].
	c.mu.Lock()
	last := s.lastID
	c.mu.Unlock()
	channelHeaders(w, r)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, channelChunk(fmt.Sprintf("[1,%d,0]", last)))
}

func queryParam(r *http.Request, k string) string { return r.URL.Query().Get(k) }

// serveBackchannel passes a backchannel GET on, reading the array ids that
// go down it, and ends it with the session's refusal when there is one.
func (g *EditionGuard) serveBackchannel(w http.ResponseWriter, r *http.Request, sid string) {
	c := &g.channels
	s := c.session(sid)
	if aid, err := strconv.ParseInt(queryParam(r, "AID"), 10, 64); err == nil {
		c.saw(s, aid)
	}
	bw := &backchannelWriter{w: w, c: c, s: s, r: r}
	c.mu.Lock()
	wake := s.wake
	c.mu.Unlock()
	select {
	case <-wake:
		bw.sendRefusal()
		return
	default:
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The proxy aborts with http.ErrAbortHandler when the refusal
		// ends the backchannel under it.
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		c.upstream.ServeHTTP(bw, r.WithContext(ctx))
	}()
	select {
	case <-done:
	case <-wake:
		bw.sendRefusal()
		cancel()
		<-done
	}
}

// channelHeaders are the headers the emulator answers a channel request
// with, for an answer the guard writes itself.
func channelHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "private, max-age=0")
	h.Set("X-Content-Type-Options", "nosniff")
	if o := r.Header.Get("Origin"); o != "" {
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Expose-Headers", "x-client-wire-protocol, x-http-session-id, x-http-initial-response")
		h.Add("Vary", "Origin")
	}
}

// channelChunk is one chunk of a channel answer: its length, then itself.
// The length is in UTF-16 code units, as the client counts; the guard's
// own chunks are ASCII.
func channelChunk(j string) string { return strconv.Itoa(len(j)) + "\n" + j }

// channelError is err as the message a WebChannel stream ends with:
// Google's JSON error.
func channelError(err error) string {
	st, _ := status.FromError(err)
	var m struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	m.Error.Code, m.Error.Message, m.Error.Status = apierror.From(err).HTTPStatus(), st.Message(), rpccode.Code(st.Code()).String()
	b, _ := json.Marshal(m)
	return string(b)
}

// backchannelWriter passes the emulator's backchannel on, reading each
// chunk's array ids, until the guard ends it with a refusal.
type backchannelWriter struct {
	w http.ResponseWriter
	c *channels
	s *channelSession
	r *http.Request

	mu      sync.Mutex
	header  bool // a status has been written
	ended   bool // the refusal went down; the emulator's rest is dropped
	pending []byte
}

func (b *backchannelWriter) Header() http.Header { return b.w.Header() }

func (b *backchannelWriter) WriteHeader(code int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended || b.header {
		return
	}
	b.header = true
	b.w.WriteHeader(code)
}

func (b *backchannelWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return len(p), nil
	}
	b.header = true
	n, err := b.w.Write(p)
	b.pending = append(b.pending, p[:n]...)
	b.readChunks()
	return n, err
}

func (b *backchannelWriter) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if f, ok := b.w.(http.Flusher); ok && !b.ended {
		f.Flush()
	}
}

// readChunks records the array ids of the whole chunks pending holds.
func (b *backchannelWriter) readChunks() {
	for {
		nl := bytes.IndexByte(b.pending, '\n')
		if nl < 0 {
			return
		}
		if _, err := strconv.Atoi(string(bytes.TrimSpace(b.pending[:nl]))); err != nil {
			b.pending = nil // not a chunk stream; nothing more to learn
			return
		}
		dec := json.NewDecoder(bytes.NewReader(b.pending[nl+1:]))
		var arrays []json.RawMessage
		if err := dec.Decode(&arrays); err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return // the rest of the chunk is to come
			}
			b.pending = nil
			return
		}
		for _, a := range arrays {
			var head []json.RawMessage
			if json.Unmarshal(a, &head) == nil && len(head) > 0 {
				if id, err := strconv.ParseInt(string(head[0]), 10, 64); err == nil {
					b.c.saw(b.s, id)
				}
			}
		}
		b.pending = b.pending[nl+1+int(dec.InputOffset()):]
	}
}

// sendRefusal sends the session's refusal down and ends the backchannel.
func (b *backchannelWriter) sendRefusal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return
	}
	err, id := b.c.takeRefusal(b.s)
	if err == nil {
		return
	}
	b.ended = true
	if !b.header {
		channelHeaders(b.w, b.r)
		b.w.WriteHeader(http.StatusOK)
		b.header = true
	}
	_, _ = io.WriteString(b.w, channelChunk(fmt.Sprintf("[[%d,[%s]]]", id, channelError(err))))
	if f, ok := b.w.(http.Flusher); ok {
		f.Flush()
	}
}

// handshakeWriter passes a handshake's answer on, reading the session id
// its first chunk, [[0,["c","<SID>",...]]], names.
type handshakeWriter struct {
	http.ResponseWriter
	buf []byte
	sid string
}

func (h *handshakeWriter) Write(p []byte) (int, error) {
	if h.sid == "" && len(h.buf) < 4096 {
		h.buf = append(h.buf, p...)
		if nl := bytes.IndexByte(h.buf, '\n'); nl >= 0 {
			var arrays [][]json.RawMessage
			if json.NewDecoder(bytes.NewReader(h.buf[nl+1:])).Decode(&arrays) == nil && len(arrays) > 0 && len(arrays[0]) > 1 {
				var msg []json.RawMessage
				var c, sid string
				if json.Unmarshal(arrays[0][1], &msg) == nil && len(msg) > 1 &&
					json.Unmarshal(msg[0], &c) == nil && c == "c" && json.Unmarshal(msg[1], &sid) == nil {
					h.sid = sid
				}
			}
		}
	}
	return h.ResponseWriter.Write(p)
}

func (h *handshakeWriter) Flush() {
	if f, ok := h.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
