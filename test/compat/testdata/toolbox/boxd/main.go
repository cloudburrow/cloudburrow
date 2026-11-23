// Command boxd runs inside the compat harness's tool box, the container in
// which every gcloud, gsutil, bq and Terraform command of test/compat runs
// (test/compat/toolbox_test.go, #2174). It is built for linux by the test.
//
// The box is on an internal Docker network, which has no route out, so no
// connection off the machine can be made from it whatever a client does
// with proxy variables. boxd gives the tools the two things they still
// need, and nothing more:
//
//   - `boxd serve` (in the box): a DNS resolver, which the box's nat table
//     sends every DNS query to, whichever server it was for. It answers
//     localhost and *.localhost with 127.0.0.1, REFUSED for every Google
//     name (*.googleapis.com, *.google.com, metadata.google.internal and the
//     other Google domains below), and NXDOMAIN for anything else; each
//     answer is logged ("DNS name type rcode"). And the loopback relay: the
//     nat table redirects every TCP connection to 127.0.0.0/8 here, where
//     the instance's ports are named; boxd reads the port it was for, serves
//     it from a listener in the box when there is one, and otherwise sends
//     it to the relay container, prefixed with the box's token and the
//     port, for the host's loopback.
//   - `boxd relay` (in the relay, the one container that can reach the
//     host): forwards its port to the harness's listener on the host,
//     byte for byte.
//
// Standard library only.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: boxd serve|relay ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "relay":
		relayMain(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}

var logMu sync.Mutex

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Printf(format+"\n", args...)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "boxd:", err)
	os.Exit(1)
}

func relayMain(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", ":7000", "address to listen on")
	target := fs.String("target", "", "host:port of the harness's listener")
	_ = fs.Parse(args)
	if *target == "" {
		fatal(fmt.Errorf("relay: no -target"))
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fatal(err)
	}
	logf("ready relay %s -> %s", *listen, *target)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			u, err := net.DialTimeout("tcp", *target, 10*time.Second)
			if err != nil {
				return
			}
			pipe(c, u)
		}()
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	relay := fs.String("relay", "", "host:port of the relay container")
	token := fs.String("token", os.Getenv("BOX_TOKEN"), "the token the host's listener checks")
	dnsAddr := fs.String("dns", "127.0.0.1:5353", "the resolver's address")
	loop := fs.String("loopback", "127.0.0.1:15001", "the loopback relay's address")
	_ = fs.Parse(args)
	if *relay == "" || *token == "" {
		fatal(fmt.Errorf("serve: -relay and a token are required"))
	}
	pc, err := net.ListenPacket("udp", *dnsAddr)
	if err != nil {
		fatal(err)
	}
	go serveDNSUDP(pc)
	dl, err := net.Listen("tcp", *dnsAddr)
	if err != nil {
		fatal(err)
	}
	go serveDNSTCP(dl)
	ln, err := net.Listen("tcp", *loop)
	if err != nil {
		fatal(err)
	}
	_, ownPort, _ := net.SplitHostPort(*loop)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				continue
			}
			go loopback(c, *relay, *token, ownPort)
		}
	}()
	logf("ready")
	select {}
}

// loopback serves one connection the nat table redirected from 127.0.0.0/8.
func loopback(c net.Conn, relay, token, ownPort string) {
	defer c.Close()
	ip, port, ok := originalDst(c)
	if !ok || strconv.Itoa(port) == ownPort {
		return
	}
	if !ip.IsLoopback() {
		// The nat table sends nothing else here; refuse it all the same.
		logf("REFUSED tcp %s:%d", ip, port)
		return
	}
	var u net.Conn
	var err error
	if listening(port) {
		// A server in the box itself (boxd's own traffic is exempt from
		// the redirect, so this reaches it).
		u, err = net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)), 10*time.Second)
	} else {
		u, err = net.DialTimeout("tcp", relay, 10*time.Second)
		if err == nil {
			_, err = fmt.Fprintf(u, "%s %d\n", token, port)
		}
	}
	if err != nil {
		if u != nil {
			u.Close()
		}
		return
	}
	pipe(c, u)
}

func pipe(a, b net.Conn) {
	defer b.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// listening reports whether a socket in the box listens on port, on
// loopback or every address, from /proc/net/tcp.
func listening(port int) bool {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	want := fmt.Sprintf(":%04X", port)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != "0A" { // LISTEN
			continue
		}
		local := fields[1]
		if !strings.HasSuffix(local, want) {
			continue
		}
		addr := strings.TrimSuffix(local, want)
		if addr == "00000000" {
			return true
		}
		if b, err := hex.DecodeString(addr); err == nil && len(b) == 4 && b[0] == 127 {
			return true
		}
	}
	return false
}

// originalDst reads SO_ORIGINAL_DST, the address a redirected connection
// was made to.
func originalDst(c net.Conn) (net.IP, int, bool) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil, 0, false
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return nil, 0, false
	}
	var addr syscall.RawSockaddrInet4
	var gerr error
	_ = raw.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(addr))
		const soOriginalDst = 80
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Pointer(&size)), 0)
		if e != 0 {
			gerr = e
		}
	})
	if gerr != nil {
		return nil, 0, false
	}
	port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&addr.Port))[:])
	return net.IPv4(addr.Addr[0], addr.Addr[1], addr.Addr[2], addr.Addr[3]), int(port), true
}

// googleSuffixes are the names the resolver refuses: Google's API, auth,
// metadata and download domains. Anything else that is not localhost gets
// NXDOMAIN, so no name outside the machine resolves either way; these are
// refused by name so the log says so.
var googleSuffixes = []string{
	"googleapis.com", "google.com", "google.internal", "googleusercontent.com", "gstatic.com",
	"gcr.io", "pkg.dev", "appspot.com", "run.app", "cloudfunctions.net", "googlesource.com", "goog",
}

const (
	rcodeNoError  = 0
	rcodeNXDomain = 3
	rcodeRefused  = 5
)

// answer builds the response to the query q, or nil when q is no query.
func answer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	name, end, ok := questionName(q, 12)
	if !ok || end+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end:])
	lname := strings.ToLower(strings.TrimSuffix(name, "."))
	rcode := rcodeNXDomain
	local := lname == "localhost" || strings.HasSuffix(lname, ".localhost")
	switch {
	case local:
		rcode = rcodeNoError
	case isGoogle(lname):
		rcode = rcodeRefused
	}
	logf("DNS %s type %d rcode %d", lname, qtype, rcode)
	resp := make([]byte, 0, end+4+16)
	resp = append(resp, q[0], q[1])
	resp = append(resp, 0x81, 0x80|byte(rcode)) // response, RD, RA, rcode
	resp = append(resp, 0, 1)                   // one question
	withA := local && qtype == 1
	if withA {
		resp = append(resp, 0, 1)
	} else {
		resp = append(resp, 0, 0)
	}
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, q[12:end+4]...)
	if withA {
		resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4, 127, 0, 0, 1)
	}
	return resp
}

func isGoogle(name string) bool {
	for _, s := range googleSuffixes {
		if name == s || strings.HasSuffix(name, "."+s) {
			return true
		}
	}
	return false
}

func serveDNSUDP(pc net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if resp := answer(buf[:n]); resp != nil {
			_, _ = pc.WriteTo(resp, from)
		}
	}
}

func serveDNSTCP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			for {
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp := answer(q)
				if resp == nil {
					return
				}
				out := append([]byte{byte(len(resp) >> 8), byte(len(resp))}, resp...)
				if _, err := c.Write(out); err != nil {
					return
				}
			}
		}()
	}
}

func questionName(q []byte, off int) (string, int, bool) {
	var labels []string
	for off < len(q) {
		l := int(q[off])
		if l == 0 {
			return strings.Join(labels, "."), off + 1, true
		}
		if l&0xc0 != 0 || off+1+l > len(q) {
			return "", 0, false
		}
		labels = append(labels, string(q[off+1:off+1+l]))
		off += 1 + l
	}
	return "", 0, false
}
