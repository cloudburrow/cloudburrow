// Package vmrelay carries a VM's own identity across the hop from its
// guest to this machine's listeners (#2160).
//
// A VM is a pod of the instance's Kubernetes cluster, and its connections
// to this machine arrive NATed through host.docker.internal: every one
// comes from the same address, so a load balancer's authorization policy
// or Secure Web Proxy could never tell one VM from another by the address
// a connection comes from. The guest instead sends what it addresses to a
// load balancer's or gateway's VPC address (a forwarding rule's IPAddress,
// a gateway's address) to a local relay (nftables redirects it), which
// opens a connection to this package's Server and begins it with a PROXY
// protocol version 2 header
// (https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt): the
// source is the VM's internal IP, the destination the VPC address and
// port it dialled, and a TLV of type TLVToken holds the instance's guest
// token, the secret its container alone is given.
//
// The header is trusted only on the Server's own listener, and only with
// a token that names a running instance whose internal IP is the header's
// source: no other listener reads a PROXY header, so a client cannot claim
// an identity anywhere else. The Server hands the connection, its source
// and destination those of the VPC, to the listener of what it dialled
// (Listener.Inject), in process, so the load balancer or proxy sees the
// VM's address as the connection's remote address and maps it to the
// instance, its service account and its secure tags through Compute
// Engine's records.
package vmrelay

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// signature begins every PROXY protocol version 2 header.
var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// TLVToken is the TLV type that carries the guest token: the first of the
// range the specification leaves to applications (PP2_TYPE_MIN_CUSTOM).
const TLVToken = 0xE0

// maxHeader bounds a header's length (the 16 bytes before the addresses
// and the addresses with their TLVs).
const maxHeader = 16 + 1024

// Header is a relayed connection's PROXY header.
type Header struct {
	// Src is the VM's internal IP (and the port its client used, 0 when
	// the guest does not know it).
	Src netip.AddrPort
	// Dst is the VPC address and port the VM dialled.
	Dst netip.AddrPort
	// Token is the instance's guest token.
	Token string
	// UDP is a relayed UDP flow (the header's DGRAM family), whose
	// datagrams follow the header as frames (WriteFrame, ReadFrame); a
	// TCP connection's bytes follow it as they are.
	UDP bool
}

// Proto is the header's transport: "tcp" or "udp".
func (h Header) Proto() string {
	if h.UDP {
		return "udp"
	}
	return "tcp"
}

// Encode is h as a PROXY protocol version 2 header (PROXY command, TCP
// or UDP over IPv4), its token in a TLVToken TLV.
func Encode(h Header) []byte {
	var b bytes.Buffer
	b.Write(signature)
	b.WriteByte(0x21) // version 2, PROXY
	if h.UDP {
		b.WriteByte(0x12) // AF_INET, DGRAM
	} else {
		b.WriteByte(0x11) // AF_INET, STREAM
	}
	src, dst := h.Src.Addr().As4(), h.Dst.Addr().As4()
	body := make([]byte, 0, 12+3+len(h.Token))
	body = append(body, src[:]...)
	body = append(body, dst[:]...)
	body = binary.BigEndian.AppendUint16(body, h.Src.Port())
	body = binary.BigEndian.AppendUint16(body, h.Dst.Port())
	body = append(body, TLVToken)
	body = binary.BigEndian.AppendUint16(body, uint16(len(h.Token)))
	body = append(body, h.Token...)
	_ = binary.Write(&b, binary.BigEndian, uint16(len(body)))
	b.Write(body)
	return b.Bytes()
}

// Read reads a PROXY protocol version 2 header from r. Only the PROXY
// command over TCP or UDP and IPv4 with a token is accepted.
func Read(r *bufio.Reader) (Header, error) {
	head := make([]byte, 16)
	if _, err := io.ReadFull(r, head); err != nil {
		return Header{}, fmt.Errorf("the PROXY header: %w", err)
	}
	if !bytes.Equal(head[:12], signature) {
		return Header{}, errors.New("not a PROXY protocol version 2 header")
	}
	if head[12] != 0x21 {
		return Header{}, fmt.Errorf("PROXY header version and command 0x%02x, want 0x21", head[12])
	}
	if head[13] != 0x11 && head[13] != 0x12 {
		return Header{}, fmt.Errorf("PROXY header family 0x%02x, want 0x11 or 0x12 (TCP or UDP over IPv4)", head[13])
	}
	n := int(binary.BigEndian.Uint16(head[14:16]))
	if n < 12 || 16+n > maxHeader {
		return Header{}, fmt.Errorf("PROXY header length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Header{}, fmt.Errorf("the PROXY header: %w", err)
	}
	h := Header{
		Src: netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10])),
		Dst: netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[4:8])), binary.BigEndian.Uint16(body[10:12])),
		UDP: head[13] == 0x12,
	}
	for tlv := body[12:]; len(tlv) > 0; {
		if len(tlv) < 3 {
			return Header{}, errors.New("a truncated PROXY header TLV")
		}
		l := int(binary.BigEndian.Uint16(tlv[1:3]))
		if len(tlv) < 3+l {
			return Header{}, errors.New("a truncated PROXY header TLV")
		}
		if tlv[0] == TLVToken {
			h.Token = string(tlv[3 : 3+l])
		}
		tlv = tlv[3+l:]
	}
	if h.Token == "" {
		return Header{}, errors.New("the PROXY header has no guest token")
	}
	return h, nil
}

// A relayed UDP flow is one connection to the Server: the header (its
// family DGRAM), then each datagram the VM's client sends as a frame, a
// 2-byte big-endian length and the datagram, and each reply back the same
// way. The stream keeps the datagrams' boundaries, as the PROXY protocol
// specification leaves the framing of a datagram flow over a stream to
// the application.

// MaxDatagram is the largest datagram a frame carries.
const MaxDatagram = 65535

// WriteFrame writes b as one frame.
func WriteFrame(w io.Writer, b []byte) error {
	if len(b) > MaxDatagram {
		return fmt.Errorf("a datagram of %d bytes", len(b))
	}
	f := make([]byte, 2, 2+len(b))
	binary.BigEndian.PutUint16(f, uint16(len(b)))
	_, err := w.Write(append(f, b...))
	return err
}

// ReadFrame reads one frame's datagram.
func ReadFrame(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Conn is a relayed connection as the listener it is handed to sees it:
// its remote address the VM's, its local address the VPC address the VM
// dialled.
type Conn struct {
	net.Conn
	r             io.Reader
	remote, local net.Addr
}

// NewConn is c, whose PROXY header br has read, as from src to dst.
func NewConn(c net.Conn, br *bufio.Reader, src, dst netip.AddrPort) *Conn {
	return &Conn{Conn: c, r: br, remote: net.TCPAddrFromAddrPort(src), local: net.TCPAddrFromAddrPort(dst)}
}

func (c *Conn) Read(b []byte) (int, error) { return c.r.Read(b) }

// RemoteAddr is the VM's internal address.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// LocalAddr is the VPC address the VM dialled.
func (c *Conn) LocalAddr() net.Addr { return c.local }

// Listener is a listener that also accepts the connections handed to it
// by Inject.
type Listener struct {
	net.Listener
	in   chan net.Conn
	acc  chan net.Conn
	errc chan error
	done chan struct{}
	once sync.Once
}

// NewListener wraps ln.
func NewListener(ln net.Listener) *Listener {
	l := &Listener{Listener: ln, in: make(chan net.Conn), acc: make(chan net.Conn), errc: make(chan error, 1), done: make(chan struct{})}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue
				}
				l.errc <- err
				return
			}
			select {
			case l.acc <- c:
			case <-l.done:
				_ = c.Close()
				return
			}
		}
	}()
	return l
}

// Accept is the next connection, accepted or injected.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.acc:
		return c, nil
	case c := <-l.in:
		return c, nil
	case err := <-l.errc:
		l.errc <- err
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Inject hands c to the listener's server; false when it is closed or
// does not accept within five seconds.
func (l *Listener) Inject(c net.Conn) bool {
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case l.in <- c:
		return true
	case <-l.done:
	case <-t.C:
	}
	return false
}

// Close closes the listener.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// Resolve authenticates a relayed connection's header and names where it
// goes: deliver hands the connection (as from src) to the listener of
// what the VM dialled. An error refuses the connection.
type Resolve func(h Header) (src netip.AddrPort, deliver func(net.Conn) bool, err error)

// Server is the relay's listener on this machine.
type Server struct {
	ln      net.Listener
	resolve Resolve
	wg      sync.WaitGroup
	// Refused, when set, is told of each refused connection.
	Refused func(error)
}

// Listen starts a Server on addr.
func Listen(addr string, resolve Resolve) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, resolve: resolve}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Addr is the Server's bound address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Port is the port the Server listens on.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close stops accepting.
func (s *Server) Close() error {
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	h, err := Read(br)
	if err == nil && h.Src.Port() == 0 {
		// The guest's relay does not know its client's port: the hop's
		// own stands in, so each connection's source differs.
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			h.Src = netip.AddrPortFrom(h.Src.Addr(), uint16(a.Port))
		}
	}
	if err == nil {
		var src netip.AddrPort
		var deliver func(net.Conn) bool
		src, deliver, err = s.resolve(h)
		if err == nil {
			_ = c.SetReadDeadline(time.Time{})
			if deliver(NewConn(c, br, src, h.Dst)) {
				return
			}
			err = fmt.Errorf("nothing accepted the connection to %s", h.Dst)
		}
	}
	if s.Refused != nil {
		s.Refused(err)
	}
	_ = c.Close()
}
