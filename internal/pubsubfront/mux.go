package pubsubfront

// Which server takes a connection (#950).
//
// Google's emulator serves gRPC and its REST API on one port, so the front
// must too. Since #909 the front served every connection with net/http,
// gRPC through grpc.Server.ServeHTTP, so that each request is routed by its
// own content type. Measured (BenchmarkTransport*, #950), that cost gRPC
// more than half its publish throughput, over a third of its streaming-pull
// throughput and over a quarter more unary latency against grpc-go's own
// transport, so a connection is now routed by its first request:
//
//   - one that does not open with the HTTP/2 client preface is HTTP/1.1, and
//     goes to net/http (REST, and an Upgrade: h2c, httpServer);
//   - an HTTP/2 connection whose first request is gRPC (an application/grpc
//     content type) goes to grpc-go's own transport, grpc.Server.Serve, as
//     every official client's connection does;
//   - any other HTTP/2 connection, h2c REST first, goes to net/http, which
//     routes each of its requests as Handler does, so a proxy that carries
//     REST and then gRPC on one connection is served whole.
//
// A gRPC client waits for the server's SETTINGS before it sends a request,
// and the server cannot be chosen before the request is read, so the router
// sends an empty SETTINGS frame itself, reads the client's frames up to the
// end of its first HEADERS, and hands the chosen server every byte it read
// but the client's acknowledgement of that SETTINGS, which the server never
// sent (ackFilter). The server then sends its own SETTINGS, which the client
// acknowledges as usual: RFC 9113 lets a server send SETTINGS at any time.
//
// A connection that went to grpc-go on a gRPC request and then carries a
// non-gRPC one is answered 415 by grpc-go, as before #909. CloudBurrow's
// host tunnel keeps gRPC and h2c REST on connections of their own
// (internal/netfwd), so it never sends one.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/http2/hpack"
)

const (
	// clientPreface opens every HTTP/2 connection; no HTTP/1.1 method is
	// PRI.
	clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	// sniffTimeout bounds how long a connection may stay silent before its
	// first bytes are read; a tcpSocket readiness probe connects and says
	// nothing, and is closed.
	sniffTimeout = 30 * time.Second
	// firstRequestTimeout bounds the wait for an HTTP/2 connection's first
	// request; one that sends none by then goes to net/http, which serves
	// anything HTTP/2.
	firstRequestTimeout = 10 * time.Second
	// maxFrame is HTTP/2's default SETTINGS_MAX_FRAME_SIZE, which the empty
	// SETTINGS the router sends leaves in force until the server's own.
	maxFrame = 16384
	// maxSniff bounds what is read before the first request's headers end.
	maxSniff = 256 << 10

	frameHeaderLen    = 9
	frameHeaders      = 0x1
	frameSettings     = 0x4
	frameContinuation = 0x9
	flagAck           = 0x1
	flagEndHeaders    = 0x4
	flagPadded        = 0x8
	flagPriority      = 0x20
)

// emptySettings is a SETTINGS frame with no parameters, on stream 0.
var emptySettings = []byte{0, 0, 0, frameSettings, 0, 0, 0, 0, 0}

// Split routes l's connections as the comment above says: gRPC ones to the
// first listener, the rest to the second. Closing either closes l.
func Split(l net.Listener) (grpcL, httpL net.Listener) {
	s := &splitter{l: l, done: make(chan struct{})}
	g := &routed{s: s, conns: make(chan net.Conn)}
	h := &routed{s: s, conns: make(chan net.Conn)}
	go s.run(g, h)
	return g, h
}

type splitter struct {
	l    net.Listener
	once sync.Once
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (s *splitter) close(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
		_ = s.l.Close()
	})
}

func (s *splitter) run(g, h *routed) {
	for {
		c, err := s.l.Accept()
		if err != nil {
			s.close(err)
			return
		}
		go s.route(c, g, h)
	}
}

func (s *splitter) route(c net.Conn, g, h *routed) {
	to, conn := classify(c)
	if conn == nil {
		_ = c.Close()
		return
	}
	dst := h
	if to == toGRPC {
		dst = g
	}
	select {
	case dst.conns <- conn:
	case <-s.done:
		_ = c.Close()
	}
}

type destination int

const (
	toHTTP destination = iota
	toGRPC
)

// classify reads as much of c as it takes to choose its server, and returns
// the choice and the connection that server is to read, which replays what
// was read; a nil connection is to be closed.
func classify(c net.Conn) (destination, net.Conn) {
	br := bufio.NewReaderSize(c, 2*maxFrame)
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	b, err := br.Peek(3)
	if err != nil {
		return toHTTP, nil
	}
	if string(b) != clientPreface[:3] {
		return toHTTP, &peeked{Conn: c, r: br}
	}
	if b, err := br.Peek(len(clientPreface)); err != nil || string(b) != clientPreface {
		// Not HTTP/2 after all: net/http refuses it.
		return toHTTP, &peeked{Conn: c, r: br}
	}
	_ = c.SetWriteDeadline(time.Now().Add(sniffTimeout))
	_, err = c.Write(emptySettings)
	_ = c.SetWriteDeadline(time.Time{})
	if err != nil {
		return toHTTP, nil
	}
	_ = c.SetReadDeadline(time.Now().Add(firstRequestTimeout))
	grpc, seen, acked := firstRequest(br)
	var rest io.Reader = br
	if !acked {
		rest = &ackFilter{r: br}
	}
	conn := &peeked{Conn: c, r: io.MultiReader(bytes.NewReader(seen), rest)}
	if grpc {
		return toGRPC, conn
	}
	return toHTTP, conn
}

// firstRequest reads an HTTP/2 connection's frames from br, which starts
// with the client preface, up to the end of its first request's headers.
// It returns whether that request is gRPC, every byte it consumed but the
// client's first SETTINGS acknowledgement, and whether it saw that
// acknowledgement. Anything it cannot read, or a request it does not see
// before the deadline, is not gRPC; what it left unread stays in br.
func firstRequest(br *bufio.Reader) (grpc bool, seen []byte, acked bool) {
	var out bytes.Buffer
	b, _ := br.Peek(len(clientPreface))
	out.Write(b)
	_, _ = br.Discard(len(b))
	var block []byte
	inHeaders := false
	for out.Len() < maxSniff {
		h, err := br.Peek(frameHeaderLen)
		if err != nil {
			break
		}
		length := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
		typ, flags := h[3], h[4]
		if length > maxFrame {
			break
		}
		if typ == frameSettings && flags&flagAck != 0 && !acked {
			if length != 0 {
				break
			}
			acked = true
			_, _ = br.Discard(frameHeaderLen)
			continue
		}
		f, err := br.Peek(frameHeaderLen + length)
		if err != nil {
			break
		}
		payload := f[frameHeaderLen:]
		switch {
		case typ == frameHeaders && !inHeaders:
			frag, ok := headerFragment(flags, payload)
			if !ok {
				return false, consume(&out, br, f), acked
			}
			block = append(block, frag...)
			inHeaders = true
		case typ == frameContinuation && inHeaders:
			block = append(block, payload...)
		case inHeaders:
			// Only CONTINUATION may follow an unfinished HEADERS.
			return false, consume(&out, br, f), acked
		}
		consume(&out, br, f)
		if inHeaders && flags&flagEndHeaders != 0 {
			return isGRPCBlock(block), out.Bytes(), acked
		}
	}
	return false, out.Bytes(), acked
}

// consume moves a peeked frame from br to out, and returns out's bytes.
func consume(out *bytes.Buffer, br *bufio.Reader, f []byte) []byte {
	out.Write(f)
	_, _ = br.Discard(len(f))
	return out.Bytes()
}

// headerFragment is a HEADERS frame's header block fragment, without its
// padding and priority.
func headerFragment(flags byte, p []byte) ([]byte, bool) {
	pad := 0
	if flags&flagPadded != 0 {
		if len(p) < 1 {
			return nil, false
		}
		pad = int(p[0])
		p = p[1:]
	}
	if flags&flagPriority != 0 {
		if len(p) < 5 {
			return nil, false
		}
		p = p[5:]
	}
	if pad > len(p) {
		return nil, false
	}
	return p[:len(p)-pad], true
}

// isGRPCBlock reports whether a connection's first header block has a gRPC
// content type. It is the first, so the decoder's table starts empty.
func isGRPCBlock(block []byte) bool {
	fields, err := hpack.NewDecoder(4096, nil).DecodeFull(block)
	if err != nil {
		return false
	}
	for _, f := range fields {
		if f.Name == "content-type" {
			return isGRPCContentType(f.Value)
		}
	}
	return false
}

// ackFilter passes an HTTP/2 client's frames through, but for the first
// SETTINGS acknowledgement, which answers the router's SETTINGS and which
// the server must not see: it would count an acknowledgement of a SETTINGS
// it never sent as a protocol error. It reads frame headers only until then.
type ackFilter struct {
	r *bufio.Reader
	// left is what remains of the frame being passed through.
	left    int
	dropped bool
}

func (a *ackFilter) Read(p []byte) (int, error) {
	for !a.dropped && a.left == 0 {
		h, err := a.r.Peek(frameHeaderLen)
		if err != nil {
			if len(h) > 0 && errors.Is(err, io.EOF) {
				// A torn frame: the server reads what there is.
				a.dropped = true
				break
			}
			return 0, err
		}
		length := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
		if h[3] == frameSettings && h[4]&flagAck != 0 && length == 0 {
			_, _ = a.r.Discard(frameHeaderLen)
			a.dropped = true
			break
		}
		a.left = frameHeaderLen + length
	}
	if a.dropped && a.left == 0 {
		return a.r.Read(p)
	}
	if len(p) > a.left {
		p = p[:a.left]
	}
	n, err := a.r.Read(p)
	a.left -= n
	return n, err
}

// routed is one side of a Split.
type routed struct {
	s     *splitter
	conns chan net.Conn
}

func (r *routed) Accept() (net.Conn, error) {
	select {
	case c := <-r.conns:
		return c, nil
	case <-r.s.done:
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		if r.s.err != nil {
			return nil, r.s.err
		}
		return nil, net.ErrClosed
	}
}

func (r *routed) Close() error {
	r.s.close(nil)
	return nil
}

func (r *routed) Addr() net.Addr { return r.s.l.Addr() }

// peeked is a connection whose first bytes were read to route it, and are
// read again from r.
type peeked struct {
	net.Conn
	r io.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }
