package mysqlpool

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// fakeServer is a MySQL server enough for the go-sql-driver client: any
// password signs in (mysql_native_password), TLS is served when asked,
// "SELECT SLEEP" answers one row after a pause, BEGIN and COMMIT set and
// clear the transaction flag; it counts its sessions and the most
// statements it ran at once.
type fakeServer struct {
	ln       net.Listener
	cert     tls.Certificate
	sessions atomic.Int64
	running  atomic.Int64
	most     atomic.Int64
	tlsSeen  atomic.Int64
}

const fakeCaps = capProtocol41 | 1<<15 /* secure connection */ | 1<<19 /* plugin auth */ | capDeprecateEOF | 1<<13 /* transactions */ |
	capSSL | capCompress | 1<<3 /* connect with db */

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := SelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, cert: cert}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func writePkt(w io.Writer, seq byte, payload []byte) error {
	h := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	_, err := w.Write(append(h, payload...))
	return err
}

func readPkt(r *bufio.Reader) (byte, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return h[3], b, err
}

func okPkt(header byte, status uint16) []byte {
	return []byte{header, 0, 0, byte(status), byte(status >> 8), 0, 0}
}

func lenencStr(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func (s *fakeServer) serve(c net.Conn) {
	defer c.Close()
	s.sessions.Add(1)
	var rw net.Conn = c
	r := bufio.NewReader(rw)
	g := []byte{10}
	g = append(g, "8.4.0-fake"...)
	g = append(g, 0, 1, 0, 0, 0)
	g = append(g, "abcdefgh"...)
	g = append(g, 0)
	g = binary.LittleEndian.AppendUint16(g, uint16(fakeCaps&0xffff))
	g = append(g, 255, 2, 0)
	g = binary.LittleEndian.AppendUint16(g, uint16(fakeCaps>>16))
	g = append(g, 21, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	g = append(g, "ijklmnopqrst"...)
	g = append(g, 0)
	g = append(g, "mysql_native_password"...)
	g = append(g, 0)
	if writePkt(rw, 0, g) != nil {
		return
	}
	seq, resp, err := readPkt(r)
	if err != nil {
		return
	}
	caps := binary.LittleEndian.Uint32(resp)
	if caps&capCompress != 0 {
		panic("the client asked for compression the front stripped")
	}
	if caps&capSSL != 0 && len(resp) == 32 {
		s.tlsSeen.Add(1)
		tc := tls.Server(&conn{Conn: c, r: r}, &tls.Config{Certificates: []tls.Certificate{s.cert}})
		if tc.Handshake() != nil {
			return
		}
		rw = tc
		r = bufio.NewReader(rw)
		if seq, _, err = readPkt(r); err != nil {
			return
		}
	}
	if writePkt(rw, seq+1, okPkt(0, 2)) != nil {
		return
	}
	status := uint16(2)
	for {
		_, cmd, err := readPkt(r)
		if err != nil || len(cmd) == 0 || cmd[0] == comQuit {
			return
		}
		switch q := strings.ToUpper(string(cmd[1:])); {
		case cmd[0] == comQuery && strings.HasPrefix(q, "SELECT SLEEP"):
			n := s.running.Add(1)
			for {
				m := s.most.Load()
				if n <= m || s.most.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			s.running.Add(-1)
			col := append(append(append(append(append(lenencStr("def"), lenencStr("")...), lenencStr("")...), lenencStr("")...),
				lenencStr("x")...), lenencStr("")...)
			col = append(col, 0x0c, 63, 0, 1, 0, 0, 0, 8, 0, 0, 0, 0, 0)
			_ = writePkt(rw, 1, []byte{1})
			_ = writePkt(rw, 2, col)
			_ = writePkt(rw, 3, lenencStr("1"))
			_ = writePkt(rw, 4, okPkt(0xfe, status))
		case cmd[0] == comQuery && q == "BEGIN":
			status |= statusInTrans
			_ = writePkt(rw, 1, okPkt(0, status))
		case cmd[0] == comQuery && q == "COMMIT":
			status &^= statusInTrans
			_ = writePkt(rw, 1, okPkt(0, status))
		default:
			_ = writePkt(rw, 1, okPkt(0, status))
		}
	}
}

func startFront(t *testing.T, upstream string, size int) (string, *Front) {
	t.Helper()
	cert, err := SelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	f := &Front{Upstream: upstream, Pool: NewPool(size), TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Logf: t.Logf}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = f.Serve(ln) }()
	return ln.Addr().String(), f
}

// TestThreadPool: eight clients, each with its own server session, run
// their statements on at most max_pool_size (2) threads at once, over
// plain TCP and over TLS, and their transactions commit.
func TestThreadPool(t *testing.T) {
	for _, tlsMode := range []string{"false", "skip-verify"} {
		t.Run("tls="+tlsMode, func(t *testing.T) {
			srv := newFakeServer(t)
			addr, f := startFront(t, srv.ln.Addr().String(), 2)
			cfg := mysql.NewConfig()
			cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "tcp", addr, "u", "p"
			cfg.AllowNativePasswords = true
			cfg.TLSConfig = tlsMode
			conn, err := mysql.NewConnector(cfg)
			if err != nil {
				t.Fatal(err)
			}
			db := sql.OpenDB(conn)
			defer db.Close()
			db.SetMaxOpenConns(8)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conns := make([]*sql.Conn, 8)
			for i := range conns {
				if conns[i], err = db.Conn(ctx); err != nil {
					t.Fatal(err)
				}
				defer conns[i].Close()
			}
			var wg sync.WaitGroup
			start := time.Now()
			errs := make(chan error, 8)
			for _, c := range conns {
				wg.Add(1)
				go func(c *sql.Conn) {
					defer wg.Done()
					tx, err := c.BeginTx(ctx, nil)
					if err != nil {
						errs <- err
						return
					}
					var x int
					if err := tx.QueryRowContext(ctx, "SELECT SLEEP(0.15)").Scan(&x); err != nil {
						errs <- err
					}
					if err := tx.Commit(); err != nil {
						errs <- err
					}
				}(c)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			if n := srv.sessions.Load(); n != 8 {
				t.Errorf("%d server sessions for 8 clients; want 8 (a dedicated session each)", n)
			}
			if m := srv.most.Load(); m > 2 || m < 1 {
				t.Errorf("the server ran %d statements at once; want at most max_pool_size, 2", m)
			}
			if el := time.Since(start); el < 4*150*time.Millisecond {
				t.Errorf("8 statements of 150ms on 2 threads took %v; want at least 600ms", el)
			}
			if st := f.Pool.Stats(); st.MaxActive != 2 || st.Active != 0 || st.Pending != 0 {
				t.Errorf("pool stats %+v", st)
			}
			if tlsMode == "skip-verify" && srv.tlsSeen.Load() != 8 {
				t.Errorf("%d sessions reached the server over TLS; want 8", srv.tlsSeen.Load())
			}
		})
	}
}

// TestPoolPriority: a waiter inside a transaction is given the next
// thread before one that came first outside.
func TestPoolPriority(t *testing.T) {
	p := NewPool(1)
	ctx := context.Background()
	rel, _ := p.Acquire(ctx, false)
	order := make(chan string, 2)
	go func() {
		r, _ := p.Acquire(ctx, false)
		order <- "outside"
		r()
	}()
	for p.Stats().Pending != 1 {
		time.Sleep(time.Millisecond)
	}
	go func() {
		r, _ := p.Acquire(ctx, true)
		order <- "in a transaction"
		r()
	}()
	for p.Stats().Pending != 2 {
		time.Sleep(time.Millisecond)
	}
	rel()
	if first := <-order; first != "in a transaction" {
		t.Errorf("first served: %s", first)
	}
	<-order
	cctx, cancel := context.WithCancel(ctx)
	rel, _ = p.Acquire(ctx, false)
	cancel()
	if _, err := p.Acquire(cctx, false); err == nil {
		t.Error("a cancelled wait was given a thread")
	}
	rel()
	if st := p.Stats(); st.Active != 0 || st.Pending != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestLenenc(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		v    uint64
		size int
	}{{[]byte{5}, 5, 1}, {[]byte{0xfc, 1, 2}, 0x201, 3}, {[]byte{0xfd, 1, 2, 3}, 0x30201, 4}, {[]byte{0xfe, 1, 0, 0, 0, 0, 0, 0, 0}, 1, 9}} {
		if v, n := lenenc(c.in); v != c.v || n != c.size {
			t.Errorf("lenenc(%v) = %d, %d", c.in, v, n)
		}
	}
}
