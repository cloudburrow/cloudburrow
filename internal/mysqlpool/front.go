package mysqlpool

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUsage is a command line Run refused.
var ErrUsage = errors.New("usage")

// The capability flags the front reads or changes
// (https://dev.mysql.com/doc/dev/mysql-server/latest/group__group__cs__capabilities__flags.html).
const (
	capCompress         = 1 << 5
	capProtocol41       = 1 << 9
	capSSL              = 1 << 11
	capDeprecateEOF     = 1 << 24
	capOptionalMetadata = 1 << 25
	capZstd             = 1 << 26
)

// Server status flags.
const (
	statusInTrans      = 0x0001
	statusMoreResults  = 0x0008
	statusCursorExists = 0x0040
)

// Commands.
const (
	comQuit           = 0x01
	comInitDB         = 0x02
	comQuery          = 0x03
	comFieldList      = 0x04
	comStatistics     = 0x09
	comProcessInfo    = 0x0a
	comChangeUser     = 0x11
	comBinlogDump     = 0x12
	comRegisterSlave  = 0x15
	comStmtPrepare    = 0x16
	comStmtExecute    = 0x17
	comStmtLongData   = 0x18
	comStmtClose      = 0x19
	comStmtFetch      = 0x1c
	comBinlogDumpGTID = 0x1e
	comClone          = 0x20
)

// maxFrame is a frame's largest payload: a longer packet goes on in the
// next frame.
const maxFrame = 0xffffff

// Front serves the thread pool in front of a MySQL server.
type Front struct {
	// Upstream is the server's address on the pod's loopback; PodUpstream,
	// when set, is the one a connection from outside the pod is made to,
	// so the server sees it come from the pod's address, as it did
	// through the Service, and not from localhost (whose accounts are the
	// managed service's own).
	Upstream, PodUpstream string
	Pool                  *Pool
	// TLS serves a client that asks for it.
	TLS *tls.Config
	// Logf, when set, logs a connection's failure.
	Logf func(format string, args ...any)
}

// Run is `cloudburrow-storage mysql-thread-pool`.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mysql-thread-pool", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address the pool serves the MySQL protocol on")
	upstream := fs.String("upstream", "", "the server's address on the pod's loopback")
	size := fs.Int("max-pool-size", 0, "max_pool_size: the threads statements run on")
	status := fs.String("status", "", "address of the pool's figures, plain HTTP (optional)")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *listen == "" || *upstream == "" || *size < 1 {
		fmt.Fprintln(stderr, "mysql-thread-pool: --listen, --upstream and --max-pool-size (at least 1) are required")
		return ErrUsage
	}
	cert, err := SelfSigned()
	if err != nil {
		return err
	}
	f := &Front{Upstream: *upstream, PodUpstream: podAddr(*upstream), Pool: NewPool(*size),
		TLS:  &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Logf: func(format string, a ...any) { fmt.Fprintf(stderr, "mysql-thread-pool: "+format+"\n", a...) }}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	if *status != "" {
		sl, err := net.Listen("tcp", *status)
		if err != nil {
			return err
		}
		go func() {
			<-ctx.Done()
			_ = sl.Close()
		}()
		go func() { _ = http.Serve(sl, f.StatusHandler()) }() //nolint:gosec // a pod-local figures page.
	}
	fmt.Fprintf(stdout, "mysql-thread-pool: %d threads on %s for %s\n", *size, ln.Addr(), *upstream)
	err = f.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// podAddr is upstream at the pod's own address, or "" when the host name
// does not resolve to one.
func podAddr(upstream string) string {
	_, port, err := net.SplitHostPort(upstream)
	if err != nil {
		return ""
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return ""
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil && !v4.IsLoopback() {
			return net.JoinHostPort(v4.String(), port)
		}
	}
	return ""
}

// StatusHandler answers the pool's figures, one name and value a line.
func (f *Front) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := f.Pool.Stats()
		fmt.Fprintf(w, "max_pool_size %d\nthreads_active %d\nthreads_idle %d\npending_connection %d\navg_wait_time_us %d\nmax_active %d\nwait_count %d\nwait_total_us %d\n",
			s.Size, s.Active, s.Idle, s.Pending, s.AvgWait.Microseconds(), s.MaxActive, s.Waits, s.Waited.Microseconds())
	})
}

// ParseStatus reads StatusHandler's page.
func ParseStatus(page []byte) (Stats, error) {
	var s Stats
	seen := 0
	for _, line := range strings.Split(string(page), "\n") {
		name, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Stats{}, fmt.Errorf("the pool's figures: %s: %w", name, err)
		}
		seen++
		switch name {
		case "max_pool_size":
			s.Size = int(n)
		case "threads_active":
			s.Active = int(n)
		case "threads_idle":
			s.Idle = int(n)
		case "pending_connection":
			s.Pending = int(n)
		case "avg_wait_time_us":
			s.AvgWait = time.Duration(n) * time.Microsecond
		case "max_active":
			s.MaxActive = int(n)
		case "wait_count":
			s.Waits = n
		case "wait_total_us":
			s.Waited = time.Duration(n) * time.Microsecond
		default:
			seen--
		}
	}
	if seen == 0 {
		return Stats{}, errors.New("the pool's figures page is empty")
	}
	return s, nil
}

// SelfSigned is a certificate made now, as mysqld makes its own when it
// is given none.
func SelfSigned() (tls.Certificate, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:   pkix.Name{CommonName: "MySQL_Server_Auto_Generated_Server_Certificate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, nil
}

// Serve serves ln until it is closed.
func (f *Front) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			if err := f.serve(c); err != nil && f.Logf != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				f.Logf("%s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

// conn is one side of a session: a connection read through a buffer.
type conn struct {
	net.Conn
	r *bufio.Reader
}

func newConn(c net.Conn) *conn { return &conn{Conn: c, r: bufio.NewReaderSize(c, 32<<10)} }

func (c *conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// packet is one logical packet: its frames' bytes as read (headers
// included), and its payload joined.
type packet struct {
	raw     []byte
	payload []byte
	seq     byte
}

// readPacket reads one logical packet.
func (c *conn) readPacket() (*packet, error) {
	p := &packet{}
	for first := true; ; first = false {
		var h [4]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return nil, err
		}
		n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
		if first {
			p.seq = h[3]
		}
		buf := make([]byte, 4+n)
		copy(buf, h[:])
		if _, err := io.ReadFull(c.r, buf[4:]); err != nil {
			return nil, err
		}
		p.raw = append(p.raw, buf...)
		p.payload = append(p.payload, buf[4:]...)
		if n < maxFrame {
			return p, nil
		}
	}
}

// session is a client connection and its server session.
type session struct {
	f              *Front
	client, server *conn
	caps           uint32
	inTx           bool
}

func (f *Front) serve(raw net.Conn) error {
	defer raw.Close()
	up := f.Upstream
	if f.PodUpstream != "" {
		if a, ok := raw.RemoteAddr().(*net.TCPAddr); ok && !a.IP.IsLoopback() {
			up = f.PodUpstream
		}
	}
	srv, err := net.DialTimeout("tcp", up, 10*time.Second)
	if err != nil {
		return fmt.Errorf("the server: %w", err)
	}
	defer srv.Close()
	s := &session{f: f, client: newConn(raw), server: newConn(srv)}
	if err := s.connect(); err != nil {
		return err
	}
	return s.commands()
}

// connect is the connection phase: the server's greeting, without the
// compression and optional metadata the front does not read, TLS when
// the client asks for it, and the authentication exchange, relayed.
func (s *session) connect() error {
	g, err := s.server.readPacket()
	if err != nil {
		return err
	}
	if len(g.payload) > 0 && g.payload[0] == 0xff {
		_, _ = s.client.Write(g.raw)
		return io.EOF
	}
	if err := stripGreeting(g); err != nil {
		return err
	}
	if _, err := s.client.Write(g.raw); err != nil {
		return err
	}
	resp, err := s.client.readPacket()
	if err != nil {
		return err
	}
	if len(resp.payload) < 4 {
		return errors.New("a handshake response too short")
	}
	s.caps = binary.LittleEndian.Uint32(resp.payload)
	if s.caps&capProtocol41 == 0 {
		return errors.New("a client of the protocol before 4.1")
	}
	if s.caps&capSSL != 0 && len(resp.payload) == 32 {
		// An SSLRequest: TLS on both sides.
		if s.f.TLS == nil {
			return errors.New("TLS asked for, and the front has no certificate")
		}
		if _, err := s.server.Write(resp.raw); err != nil {
			return err
		}
		tc := tls.Client(s.server, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the pod's own server, over its loopback.
		if err := tc.Handshake(); err != nil {
			return fmt.Errorf("TLS to the server: %w", err)
		}
		s.server = newConn(tc)
		ts := tls.Server(s.client, s.f.TLS)
		if err := ts.Handshake(); err != nil {
			return fmt.Errorf("TLS with the client: %w", err)
		}
		s.client = newConn(ts)
		if resp, err = s.client.readPacket(); err != nil {
			return err
		}
		if len(resp.payload) >= 4 {
			s.caps = binary.LittleEndian.Uint32(resp.payload)
		}
	}
	if _, err := s.server.Write(resp.raw); err != nil {
		return err
	}
	return s.auth()
}

// stripGreeting clears the capabilities the front does not serve from the
// server's greeting (protocol version 10), so the client asks for none.
func stripGreeting(g *packet) error {
	p := g.payload
	if len(p) < 1 || p[0] != 10 {
		return fmt.Errorf("a greeting of protocol %v", p[:min(1, len(p))])
	}
	i := 1
	for i < len(p) && p[i] != 0 {
		i++
	}
	i++         // the version's NUL
	i += 4 + 9  // connection id, auth data part 1 and its filler
	lo := i     // capabilities, lower two bytes
	hi := i + 5 // after them, the character set and the status flags
	if hi+2 > len(p) {
		return errors.New("a greeting too short")
	}
	caps := uint32(binary.LittleEndian.Uint16(p[lo:])) | uint32(binary.LittleEndian.Uint16(p[hi:]))<<16
	caps &^= capCompress | capOptionalMetadata | capZstd
	binary.LittleEndian.PutUint16(p[lo:], uint16(caps))
	binary.LittleEndian.PutUint16(p[hi:], uint16(caps>>16))
	// The greeting is one frame: write the payload back into it.
	copy(g.raw[4:], p)
	return nil
}

// auth relays the authentication exchange until the server's OK or ERR.
func (s *session) auth() error {
	for {
		p, err := s.server.readPacket()
		if err != nil {
			return err
		}
		if _, err := s.client.Write(p.raw); err != nil {
			return err
		}
		if len(p.payload) == 0 {
			return errors.New("an empty packet in the authentication exchange")
		}
		switch p.payload[0] {
		case 0x00:
			s.okStatus(p.payload)
			return nil
		case 0xff:
			return io.EOF
		case 0x01:
			if len(p.payload) == 2 && p.payload[1] == 0x03 {
				// caching_sha2_password's fast authentication: its OK follows.
				continue
			}
		}
		c, err := s.client.readPacket()
		if err != nil {
			return err
		}
		if _, err := s.server.Write(c.raw); err != nil {
			return err
		}
	}
}

// commands relays the command phase, each statement on a thread.
func (s *session) commands() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for {
		req, err := s.client.readPacket()
		if err != nil {
			return err
		}
		if len(req.payload) == 0 {
			return errors.New("an empty command")
		}
		cmd := req.payload[0]
		switch cmd {
		case comQuit:
			_, _ = s.server.Write(req.raw)
			return nil
		case comStmtClose, comStmtLongData:
			// No answer.
			if _, err := s.server.Write(req.raw); err != nil {
				return err
			}
			continue
		case comBinlogDump, comBinlogDumpGTID, comRegisterSlave, comClone:
			// Replication: the session is the server's from now on, on
			// no thread of the pool.
			if _, err := s.server.Write(req.raw); err != nil {
				return err
			}
			return s.splice()
		}
		release, err := s.f.Pool.Acquire(ctx, s.inTx)
		if err != nil {
			return err
		}
		err = s.statement(cmd, req)
		release()
		if err != nil {
			return err
		}
	}
}

// statement sends one command and relays its whole answer.
func (s *session) statement(cmd byte, req *packet) error {
	if _, err := s.server.Write(req.raw); err != nil {
		return err
	}
	switch cmd {
	case comQuery, comStmtExecute, comProcessInfo:
		return s.results(cmd == comQuery)
	case comStmtPrepare:
		return s.prepared()
	case comStmtFetch, comFieldList:
		_, err := s.rows()
		return err
	case comChangeUser:
		s.inTx = false
		return s.auth()
	case comStatistics:
		return s.one()
	default:
		// comPing, comInitDB, comStmtReset, comSetOption, comResetConnection,
		// comProcessKill, comDebug, comRefresh: one OK, EOF or ERR.
		return s.one()
	}
}

// relay reads one packet from the server and sends it to the client.
func (s *session) relay() (*packet, error) {
	p, err := s.server.readPacket()
	if err != nil {
		return nil, err
	}
	if _, err := s.client.Write(p.raw); err != nil {
		return nil, err
	}
	return p, nil
}

// one relays a single-packet answer.
func (s *session) one() error {
	p, err := s.relay()
	if err != nil {
		return err
	}
	if len(p.payload) > 0 && (p.payload[0] == 0x00 || isEOF(p.payload, s.caps)) {
		s.okStatus(p.payload)
	}
	return nil
}

// results relays a text or binary protocol answer: OK, ERR, a LOCAL
// INFILE request, or result sets, as long as the server says more
// follow.
func (s *session) results(query bool) error {
	for {
		p, err := s.relay()
		if err != nil {
			return err
		}
		if len(p.payload) == 0 {
			return errors.New("an empty answer")
		}
		more := false
		switch h := p.payload[0]; {
		case h == 0x00:
			more = s.okStatus(p.payload)&statusMoreResults != 0
		case h == 0xff:
			return nil
		case h == 0xfb && query:
			// LOAD DATA LOCAL INFILE: the client's file, to its empty
			// packet, then the server's OK or ERR.
			for {
				c, err := s.client.readPacket()
				if err != nil {
					return err
				}
				if _, err := s.server.Write(c.raw); err != nil {
					return err
				}
				if len(c.payload) == 0 {
					break
				}
			}
			continue
		default:
			// A result set: its column count, its columns, the EOF the
			// protocol before CLIENT_DEPRECATE_EOF ends them with, its rows.
			n, _ := lenenc(p.payload)
			for i := uint64(0); i < n; i++ {
				if _, err := s.relay(); err != nil {
					return err
				}
			}
			if s.caps&capDeprecateEOF == 0 {
				e, err := s.relay()
				if err != nil {
					return err
				}
				if st := s.okStatus(e.payload); st&statusCursorExists != 0 {
					// A cursor: its rows come by COM_STMT_FETCH.
					return nil
				}
			}
			st, err := s.rows()
			if err != nil {
				return err
			}
			more = st&statusMoreResults != 0
		}
		if !more {
			return nil
		}
	}
}

// rows relays rows to their terminator, answering its status flags.
func (s *session) rows() (uint16, error) {
	for {
		p, err := s.relay()
		if err != nil {
			return 0, err
		}
		if len(p.payload) == 0 {
			continue
		}
		if p.payload[0] == 0xff {
			return 0, nil
		}
		if isEOF(p.payload, s.caps) {
			return s.okStatus(p.payload), nil
		}
	}
}

// prepared relays COM_STMT_PREPARE's answer: ERR, or its OK, the
// parameters' and the columns' definitions.
func (s *session) prepared() error {
	p, err := s.relay()
	if err != nil {
		return err
	}
	if len(p.payload) < 9 || p.payload[0] != 0x00 {
		return nil
	}
	cols := int(binary.LittleEndian.Uint16(p.payload[5:]))
	params := int(binary.LittleEndian.Uint16(p.payload[7:]))
	for _, n := range []int{params, cols} {
		if n == 0 {
			continue
		}
		for i := 0; i < n; i++ {
			if _, err := s.relay(); err != nil {
				return err
			}
		}
		if s.caps&capDeprecateEOF == 0 {
			if _, err := s.relay(); err != nil {
				return err
			}
		}
	}
	return nil
}

// isEOF is whether payload ends a list of rows or columns: an EOF packet,
// or, with CLIENT_DEPRECATE_EOF, the OK packet sent with EOF's header.
func isEOF(payload []byte, caps uint32) bool {
	if len(payload) == 0 || payload[0] != 0xfe {
		return false
	}
	if caps&capDeprecateEOF != 0 {
		return len(payload) < maxFrame
	}
	return len(payload) < 9
}

// okStatus answers an OK or EOF packet's status flags, and keeps whether
// the session is inside a transaction.
func (s *session) okStatus(p []byte) uint16 {
	var st uint16
	switch {
	case len(p) >= 5 && p[0] == 0xfe && len(p) < 9 && s.caps&capDeprecateEOF == 0:
		// EOF: its warnings, then the status.
		st = binary.LittleEndian.Uint16(p[3:])
	case len(p) > 0 && (p[0] == 0x00 || p[0] == 0xfe):
		_, n1 := lenenc(p[1:])
		_, n2 := lenenc(p[1+n1:])
		i := 1 + n1 + n2
		if i+2 > len(p) {
			return 0
		}
		st = binary.LittleEndian.Uint16(p[i:])
	default:
		return 0
	}
	s.inTx = st&statusInTrans != 0
	return st
}

// lenenc reads a length-encoded integer, answering it and its size.
func lenenc(p []byte) (uint64, int) {
	if len(p) == 0 {
		return 0, 0
	}
	switch h := p[0]; {
	case h < 0xfb:
		return uint64(h), 1
	case h == 0xfc && len(p) >= 3:
		return uint64(binary.LittleEndian.Uint16(p[1:])), 3
	case h == 0xfd && len(p) >= 4:
		return uint64(p[1]) | uint64(p[2])<<8 | uint64(p[3])<<16, 4
	case h == 0xfe && len(p) >= 9:
		return binary.LittleEndian.Uint64(p[1:]), 9
	}
	return 0, 1
}

// splice copies both ways until either side closes.
func (s *session) splice() error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(s.client.Conn, s.server)
		_ = s.client.Close()
	}()
	_, _ = io.Copy(s.server.Conn, s.client)
	_ = s.server.Close()
	wg.Wait()
	return nil
}
