package vmrelay

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{Src: netip.MustParseAddrPort("10.0.0.5:0"), Dst: netip.MustParseAddrPort("10.0.0.9:443"), Token: "tok-123"}
	b := Encode(h)
	if !bytes.HasPrefix(b, signature) || b[12] != 0x21 || b[13] != 0x11 {
		t.Fatalf("header % x", b[:16])
	}
	got, err := Read(bufio.NewReader(bytes.NewReader(append(b, "GET /"...))))
	if err != nil || got != h {
		t.Fatalf("Read: %+v %v", got, err)
	}
	for name, bad := range map[string][]byte{
		"v1":       []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2\r\n"),
		"no token": Encode(Header{Src: h.Src, Dst: h.Dst}),
		"local":    append(append([]byte{}, signature...), 0x20, 0x11, 0, 12, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
		"short":    b[:20],
	} {
		if _, err := Read(bufio.NewReader(bytes.NewReader(bad))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestServerHandsTheConnectionOn relays a connection with a good token to
// an injecting listener, its remote address the header's source, and
// refuses one with a bad token.
func TestServerHandsTheConnectionOn(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inj := NewListener(base)
	defer inj.Close()
	refused := make(chan error, 4)
	s, err := Listen("127.0.0.1:0", func(h Header) (netip.AddrPort, func(net.Conn) bool, error) {
		if h.Token != "good" {
			return netip.AddrPort{}, nil, errors.New("bad token")
		}
		return h.Src, inj.Inject, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Refused = func(err error) { refused <- err }
	defer s.Close()
	go func() {
		for {
			c, err := inj.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, c.RemoteAddr().String()+" "+c.LocalAddr().String()+" "+line)
			}()
		}
	}()
	dial := func(tok string) string {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port())))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(Encode(Header{Src: netip.MustParseAddrPort("10.1.2.3:4000"), Dst: netip.MustParseAddrPort("10.1.2.9:80"), Token: tok}))
		_, _ = io.WriteString(c, "hello\n")
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if got := dial("good"); got != "10.1.2.3:4000 10.1.2.9:80 hello\n" {
		t.Errorf("relayed: %q", got)
	}
	if got := dial("bad"); got != "" {
		t.Errorf("a bad token was relayed: %q", got)
	}
	select {
	case err := <-refused:
		if !strings.Contains(err.Error(), "bad token") {
			t.Errorf("refused: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the bad token was not refused")
	}
	// The wrapped listener still accepts its own connections, as they are.
	c, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "direct\n")
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "127.0.0.1:") || !strings.HasSuffix(string(b), " direct\n") {
		t.Errorf("direct: %q", b)
	}
}
