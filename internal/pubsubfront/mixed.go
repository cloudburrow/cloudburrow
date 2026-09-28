package pubsubfront

// A connection grpc-go serves that then carries a request that is not gRPC
// (#963).
//
// The router (mux.go) gives an HTTP/2 connection whose first request is
// gRPC to grpc-go's own transport, which is much faster than ServeHTTP
// (BenchmarkTransport*, #950), and which answers any later non-gRPC request
// on that connection 415. An HTTP/2 proxy that pools gRPC and h2c REST on
// one upstream connection, gRPC first, would get that 415 for its REST.
//
// So the router keeps reading such a connection's requests as they pass to
// grpc-go (grpcWatch): it walks the client's frames and decodes each header
// block, with an HPACK decoder of its own kept in step with the client's
// encoder, and copies the bytes through unchanged but as below. When a
// request arrives whose content type is not gRPC, grpc-go never sees it:
//
//   - the router sends the client a GOAWAY (NO_ERROR) whose last stream ID
//     is the last gRPC request grpc-go was given. RFC 9113 section 6.8 says
//     the streams above it were not processed and may be retried on a new
//     connection, which HTTP/2 clients do on their own (Go's net/http
//     retries such a request; grpc-go's client retries such a call);
//   - that request, and any frame of a stream above the last ID, is not
//     passed on; the connection flow-control window the client spent on
//     their DATA is given back with a WINDOW_UPDATE;
//   - the calls grpc-go already has on the connection run to their end, and
//     the client closes the connection once they have;
//   - the router remembers the client's address, and routes each of its
//     later HTTP/2 connections to net/http, which routes every request on
//     its own (Handler): that client has shown it mixes the two, so it is
//     served per request from then on, at ServeHTTP's cost for its gRPC.
//     Every other client keeps grpc-go's transport.
//
// The router writes its frames between grpc-go's (grpcWrites), at a frame
// boundary, so the two never interleave inside a frame. The HPACK decoder
// costs a gRPC request's header block one more decoding; the benchmarks
// measure that (#963's PR has the numbers).
//
// Client trailers (a HEADERS frame ending a stream grpc-go has) are not
// given to grpc-go either: grpc-go's transport takes any header block on a
// stream it has as a protocol error and closes the connection, calls and
// all, and its server has no API that would show a client's trailers. The
// router passes on an empty DATA frame ending that stream in their place,
// which is how gRPC clients end a request (#981).
//
// grpc-go's HPACK decoder never sees a block the router keeps from it (a
// refused request, a stream above the GOAWAY's last ID, client trailers),
// while the client's encoder may have added entries to its dynamic table
// with it. So from the first such block on (resync), every block given to
// grpc-go is re-encoded from what the router's decoder, still in step with
// the client, decoded: by an encoder of the router's own whose first block
// sets grpc-go's table size to 0, emptying it, and which indexes nothing
// after, so each block grpc-go reads names static entries and literals
// alone (#981). A connection that never has a block kept from grpc-go
// passes every byte through as before.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http2/hpack"
)

const (
	frameData         = 0x0
	frameGoAway       = 0x7
	frameWindowUpdate = 0x8
	// errCodeNo is HTTP/2's NO_ERROR.
	errCodeNo = 0
	// flagEndStream is END_STREAM, on HEADERS and DATA.
	flagEndStream = 0x1
)

// goAwayDebug is the GOAWAY's debug data, which a client may log.
const goAwayDebug = "cloudburrow: a request that is not gRPC on a connection grpc-go serves; retry it on a new connection"

// grpcWatch reads a connection grpc-go serves, as the comment above says.
// It is read by grpc-go's reader alone.
type grpcWatch struct {
	br  *bufio.Reader
	dec *hpack.Decoder
	w   *grpcWrites
	// onMixed is told once, when the connection carries a request that is
	// not gRPC.
	onMixed func()

	// pend is what is read before anything else: a header block's frames
	// held until its end decided their fate.
	pend []byte
	// left is what remains of a frame being passed through.
	left int
	// ackPending: the client's acknowledgement of the router's SETTINGS is
	// still to come, and is not grpc-go's to see (ackFilter).
	ackPending bool
	// blind: a frame the router cannot read (malformed, or larger than it
	// buffers); everything from there on is passed through unread, and
	// grpc-go answers it as it would.
	blind bool

	// The header block being read: its stream, its HEADERS frame's flags,
	// its fragments so far, and the frames that carried them, held until
	// its end.
	inBlock     bool
	blockStream uint32
	blockFlags  byte
	block       []byte
	held        []byte

	// resync: a block was kept from grpc-go, so its decoder is no longer in
	// step with the client's encoder, and every block given to it is
	// re-encoded (enc, into encBuf) from the fields decoded (fields).
	resync bool
	fields []hpack.HeaderField
	enc    *hpack.Encoder
	encBuf bytes.Buffer

	// contentType is the last decoded block's content-type.
	contentType string
	sawType     bool

	// maxStream is the highest stream a request opened on grpc-go.
	maxStream uint32
	// refused is set once a request that is not gRPC came; every frame of a
	// stream above maxStream is then dropped.
	refused bool
}

// newGRPCWatch watches br, the rest of a connection whose first request,
// on stream sid, had header block first, a gRPC one.
func newGRPCWatch(br *bufio.Reader, w *grpcWrites, first []byte, sid uint32, acked bool, onMixed func()) *grpcWatch {
	g := &grpcWatch{br: br, w: w, onMixed: onMixed, ackPending: !acked, maxStream: sid}
	g.dec = hpack.NewDecoder(4096, func(f hpack.HeaderField) {
		if f.Name == "content-type" {
			g.contentType, g.sawType = f.Value, true
		}
		if g.resync {
			g.fields = append(g.fields, f)
		}
	})
	if _, err := g.dec.Write(first); err != nil || g.dec.Close() != nil {
		g.blind = true
	}
	return g
}

func (g *grpcWatch) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(g.pend) > 0 {
			m := copy(p[n:], g.pend)
			g.pend = g.pend[m:]
			n += m
			continue
		}
		if g.blind {
			if n > 0 {
				return n, nil
			}
			return g.br.Read(p)
		}
		if g.left > 0 {
			k := min(g.left, len(p)-n)
			if n > 0 && g.br.Buffered() == 0 {
				return n, nil
			}
			m, err := g.br.Read(p[n : n+k])
			n += m
			g.left -= m
			if err != nil {
				return n, err
			}
			continue
		}
		// A frame's header next. Once something is read, only what is
		// already buffered is looked at: grpc-go gets what there is.
		if n > 0 && g.br.Buffered() < frameHeaderLen {
			return n, nil
		}
		h, err := g.br.Peek(frameHeaderLen)
		if err != nil {
			if n > 0 {
				return n, nil
			}
			if len(h) > 0 && errors.Is(err, io.EOF) {
				g.blind = true // a torn frame: grpc-go reads what there is
				continue
			}
			return 0, err
		}
		length := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
		typ, flags := h[3], h[4]
		sid := binary.BigEndian.Uint32(h[5:9]) & (1<<31 - 1)
		switch {
		case g.ackPending && typ == frameSettings && flags&flagAck != 0 && length == 0:
			_, _ = g.br.Discard(frameHeaderLen)
			g.ackPending = false
		case g.refused && sid > g.maxStream:
			// A stream grpc-go was never given: dropped, and the window
			// its data spent given back.
			if n > 0 && g.br.Buffered() < frameHeaderLen+length {
				return n, nil
			}
			if typ == frameHeaders || typ == frameContinuation {
				// Decoded all the same, to keep the decoder in step.
				if frameHeaderLen+length > g.br.Size() {
					g.blind = true
					continue
				}
				g.readBlockFrame(typ, flags, sid, length)
				continue
			}
			if _, err := g.br.Discard(frameHeaderLen + length); err != nil {
				return n, err
			}
			if typ == frameData && length > 0 {
				g.w.inject(windowUpdate(uint32(length)))
			}
		case typ == frameHeaders || typ == frameContinuation || g.inBlock:
			if frameHeaderLen+length > g.br.Size() {
				g.blind = true
				continue
			}
			if n > 0 && g.br.Buffered() < frameHeaderLen+length {
				return n, nil
			}
			g.readBlockFrame(typ, flags, sid, length)
		default:
			g.left = frameHeaderLen + length
		}
	}
	return n, nil
}

// readBlockFrame reads one frame of a header block and, at the block's
// end, decides on it: a request that opens a stream is given to grpc-go
// when it is gRPC, and refused when it is not; client trailers are given to
// it as the end of their stream; any other block is passed on. The frame's
// bytes are held until the block's end. It reports whether the block ended.
func (g *grpcWatch) readBlockFrame(typ, flags byte, sid uint32, length int) bool {
	f, err := g.br.Peek(frameHeaderLen + length)
	if err != nil {
		g.blind = true
		return false
	}
	payload := f[frameHeaderLen:]
	if typ == frameHeaders && !g.inBlock && flags&flagEndHeaders != 0 && sid > g.maxStream && !g.refused && !g.resync {
		// A request in one frame, as gRPC clients send them: decided on
		// as it stands in the buffer, and passed through from there.
		frag, ok := headerFragment(flags, payload)
		if !ok {
			g.blind = true
			return false
		}
		switch g.decide(frag) {
		case passBlock:
			g.maxStream = sid
			g.left = len(f)
		case refuseBlock:
			_, _ = g.br.Discard(len(f))
			g.resync = true
			g.refuse()
		case blindBlock:
			g.blind = true
		}
		return true
	}
	var frag []byte
	switch {
	case typ == frameHeaders && !g.inBlock:
		var ok bool
		if frag, ok = headerFragment(flags, payload); !ok {
			g.blind = true
			return false
		}
		g.inBlock, g.blockStream, g.blockFlags, g.block = true, sid, flags, g.block[:0]
	case typ == frameContinuation && g.inBlock && sid == g.blockStream:
		frag = payload
	default:
		// Out of order: grpc-go answers the protocol error.
		g.blind = true
		return false
	}
	g.block = append(g.block, frag...)
	g.held = append(g.held, f...)
	_, _ = g.br.Discard(len(f))
	if flags&flagEndHeaders == 0 {
		return false
	}
	g.inBlock = false
	verdict := g.decide(g.block)
	opens := g.blockStream > g.maxStream
	switch {
	case verdict == blindBlock:
		g.blind = true
		g.pend = append(g.pend, g.held...)
	case opens && g.refused:
		// A stream above the last ID: dropped, as its other frames are.
		g.resync = true
	case opens && verdict == passBlock:
		g.maxStream = g.blockStream
		g.pass()
	case opens:
		g.resync = true
		g.refuse()
	case g.blockFlags&flagEndStream != 0:
		// Client trailers: grpc-go is given the end of the stream.
		g.pend = appendFrame(g.pend, frameData, flagEndStream, g.blockStream, nil)
		g.resync = true
	default:
		// A HEADERS frame on a stream grpc-go has that does not end it:
		// grpc-go answers the protocol error.
		g.pass()
	}
	g.held = g.held[:0]
	return true
}

// pass gives grpc-go the block just read: its frames as the client sent
// them while grpc-go's decoder is in step with the client's encoder, and
// the block re-encoded once it is not.
func (g *grpcWatch) pass() {
	if !g.resync {
		g.pend = append(g.pend, g.held...)
		return
	}
	if g.enc == nil {
		g.enc = hpack.NewEncoder(&g.encBuf)
		// The first block re-encoded starts with a table size update to
		// 0, which empties grpc-go's table; nothing is indexed after.
		g.enc.SetMaxDynamicTableSizeLimit(0)
	}
	g.encBuf.Reset()
	for _, f := range g.fields {
		_ = g.enc.WriteField(f)
	}
	block := g.encBuf.Bytes()
	typ, flags := byte(frameHeaders), g.blockFlags&flagEndStream
	for {
		k := min(len(block), maxFrame)
		fl := flags
		if k == len(block) {
			fl |= flagEndHeaders
		}
		g.pend = appendFrame(g.pend, typ, fl, g.blockStream, block[:k])
		if block = block[k:]; len(block) == 0 {
			return
		}
		typ, flags = frameContinuation, 0
	}
}

// appendFrame appends a frame to dst.
func appendFrame(dst []byte, typ, flags byte, sid uint32, payload []byte) []byte {
	n := len(payload)
	dst = append(dst, byte(n>>16), byte(n>>8), byte(n), typ, flags)
	dst = binary.BigEndian.AppendUint32(dst, sid)
	return append(dst, payload...)
}

type blockVerdict int

const (
	passBlock blockVerdict = iota
	refuseBlock
	blindBlock
)

// decide decodes a whole header block: gRPC is passed, anything else
// refused, and a block the decoder cannot read leaves the router blind.
func (g *grpcWatch) decide(block []byte) blockVerdict {
	g.sawType, g.contentType, g.fields = false, "", g.fields[:0]
	if _, err := g.dec.Write(block); err != nil || g.dec.Close() != nil {
		return blindBlock
	}
	if g.sawType && isGRPCContentType(g.contentType) {
		return passBlock
	}
	return refuseBlock
}

// refuse sends the GOAWAY: the request just read, and every stream above
// the last one grpc-go was given, are not its.
func (g *grpcWatch) refuse() {
	g.refused = true
	g.w.inject(goAway(g.maxStream))
	if g.onMixed != nil {
		g.onMixed()
	}
}

// goAway is a GOAWAY frame, NO_ERROR, with last as its last stream ID.
func goAway(last uint32) []byte {
	b := make([]byte, frameHeaderLen+8+len(goAwayDebug))
	putFrameHeader(b, 8+len(goAwayDebug), frameGoAway, 0)
	binary.BigEndian.PutUint32(b[9:], last)
	binary.BigEndian.PutUint32(b[13:], errCodeNo)
	copy(b[17:], goAwayDebug)
	return b
}

// windowUpdate is a WINDOW_UPDATE frame for the connection.
func windowUpdate(n uint32) []byte {
	b := make([]byte, frameHeaderLen+4)
	putFrameHeader(b, 4, frameWindowUpdate, 0)
	binary.BigEndian.PutUint32(b[9:], n)
	return b
}

// putFrameHeader writes a stream-0 frame header.
func putFrameHeader(b []byte, length int, typ byte, flags byte) {
	b[0], b[1], b[2] = byte(length>>16), byte(length>>8), byte(length)
	b[3], b[4] = typ, flags
	binary.BigEndian.PutUint32(b[5:9], 0)
}

// grpcWrites is the write side of a connection grpc-go serves: grpc-go's
// frames pass through as written, and the router's own (inject) are put
// between two of them, never inside one.
type grpcWrites struct {
	conn net.Conn

	// mu is held for each write to conn, and guards the frame state: left
	// is what remains of the frame being written, hdr and hdrN the part of
	// a frame header written so far.
	mu   sync.Mutex
	left int
	hdr  [frameHeaderLen]byte
	hdrN int

	hasPending atomic.Bool
	pmu        sync.Mutex
	pending    []byte
	flushing   bool
}

// Write writes grpc-go's bytes, and the router's frames waiting for a
// boundary before or after them.
func (w *grpcWrites) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hasPending.Load() && w.atBoundary() {
		w.writePending()
	}
	n, err := w.conn.Write(p)
	w.track(p[:n])
	if err == nil && w.hasPending.Load() && w.atBoundary() {
		w.writePending()
	}
	return n, err
}

func (w *grpcWrites) atBoundary() bool { return w.left == 0 && w.hdrN == 0 }

// track follows the frames in p, written.
func (w *grpcWrites) track(p []byte) {
	for len(p) > 0 {
		if w.left > 0 {
			k := min(w.left, len(p))
			w.left -= k
			p = p[k:]
			continue
		}
		k := copy(w.hdr[w.hdrN:], p)
		w.hdrN += k
		p = p[k:]
		if w.hdrN == frameHeaderLen {
			w.left = int(w.hdr[0])<<16 | int(w.hdr[1])<<8 | int(w.hdr[2])
			w.hdrN = 0
		}
	}
}

// writePending writes the router's frames; mu is held, at a boundary.
func (w *grpcWrites) writePending() {
	w.pmu.Lock()
	b := w.pending
	w.pending = nil
	w.hasPending.Store(false)
	w.pmu.Unlock()
	_, _ = w.conn.Write(b)
}

// inject queues frames of the router's own. They are written at the next
// boundary: at once when grpc-go is not writing, or by grpc-go's next
// write. It never blocks, since grpc-go's reader calls it.
func (w *grpcWrites) inject(frames []byte) {
	w.pmu.Lock()
	w.pending = append(w.pending, frames...)
	w.hasPending.Store(true)
	start := !w.flushing
	w.flushing = true
	w.pmu.Unlock()
	if start {
		go func() {
			w.mu.Lock()
			w.pmu.Lock()
			w.flushing = false
			w.pmu.Unlock()
			if w.hasPending.Load() && w.atBoundary() {
				w.writePending()
			}
			w.mu.Unlock()
		}()
	}
}

// watched is a connection grpc-go serves: what it reads is watched, what
// it writes may have the router's frames between.
type watched struct {
	net.Conn
	r io.Reader
	w *grpcWrites
}

func (c *watched) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *watched) Write(b []byte) (int, error) { return c.w.Write(b) }

// mixedHosts is the clients whose connections mixed gRPC with other
// requests; their later HTTP/2 connections go to net/http.
type mixedHosts struct {
	mu    sync.Mutex
	hosts map[string]bool
}

// maxMixedHosts bounds what is remembered; past it, a new such client's
// connections are refused per connection (GOAWAY), not remembered.
const maxMixedHosts = 4096

func (m *mixedHosts) add(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hosts == nil {
		m.hosts = map[string]bool{}
	}
	if len(m.hosts) < maxMixedHosts {
		m.hosts[host] = true
	}
}

func (m *mixedHosts) has(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hosts[host]
}

// hostOf is a connection's remote host, without its port.
func hostOf(c net.Conn) string {
	a := c.RemoteAddr()
	if a == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}
