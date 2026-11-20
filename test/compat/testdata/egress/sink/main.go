// Command sink counts the connection attempts a client makes off the
// machine, inside the no-egress container of TestServedClientsSendNothing
// OffTheMachine (test/compat/egresscontainer_test.go).
//
// The container is on an internal Docker network, which has no route out.
// Its entrypoint adds a default route onto that network only so that the
// kernel gets as far as the nat table, which redirects every TCP connection
// and every UDP datagram to a non-loopback address here. So:
//
//   - a DNS query, to any server, reaches the resolver on :53, which logs the
//     name and answers every A query with 198.18.0.1 (a benchmarking
//     address, routed nowhere);
//   - a TCP connection to any non-loopback address reaches :9999, which logs
//     its original destination and closes it, before a byte is read;
//   - a UDP datagram to any non-loopback address reaches :9998 and is logged.
//
// Each attempt is one "EGRESS ..." line on stdout. The relay address is the
// one exception, and only the forwarders below dial it: -forward
// 127.0.0.1:P=relay:P serves the instance's port P on the container's
// loopback, where the ADC fixture and the clients expect it.
//
// Standard library only: it is built for linux by the test.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

type forwards []string

func (f *forwards) String() string     { return strings.Join(*f, ",") }
func (f *forwards) Set(v string) error { *f = append(*f, v); return nil }

var mu sync.Mutex

func egress(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("EGRESS "+format+"\n", args...)
}

func main() {
	var fw forwards
	flag.Var(&fw, "forward", "listen=target, repeated")
	flag.Parse()

	for _, f := range fw {
		listen, target, ok := strings.Cut(f, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "bad -forward", f)
			os.Exit(2)
		}
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go relay(ln, target)
	}

	dns, err := net.ListenPacket("udp", ":53")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go serveDNS(dns)
	dnsTCP, err := net.Listen("tcp", ":53")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go catchTCP(dnsTCP, "dns-tcp")
	tcp, err := net.Listen("tcp", ":9999")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go catchTCP(tcp, "tcp")
	udp, err := net.ListenPacket("udp", ":9998")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go catchUDP(udp)

	fmt.Fprintln(os.Stderr, "sink ready")
	select {}
}

func relay(ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("tcp", target)
			if err != nil {
				return
			}
			defer u.Close()
			go io.Copy(u, c)
			io.Copy(c, u)
		}()
	}
}

// catchTCP logs each connection's original destination and closes it.
func catchTCP(ln net.Listener, kind string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		egress("%s %s", kind, originalDst(c))
		c.Close()
	}
}

func catchUDP(pc net.PacketConn) {
	buf := make([]byte, 65536)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		egress("udp %d bytes from %s", n, from)
	}
}

// originalDst reads SO_ORIGINAL_DST, the address a redirected connection
// was made to.
func originalDst(c net.Conn) string {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return "?"
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return "?"
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
		return "?"
	}
	port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&addr.Port))[:])
	return fmt.Sprintf("%d.%d.%d.%d:%d", addr.Addr[0], addr.Addr[1], addr.Addr[2], addr.Addr[3], port)
}

// serveDNS logs every question and answers A with 198.18.0.1, anything
// else with no records.
func serveDNS(pc net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil || n < 12 {
			continue
		}
		q := buf[:n]
		name, end, ok := questionName(q, 12)
		if !ok || end+4 > n {
			continue
		}
		qtype := binary.BigEndian.Uint16(q[end:])
		egress("dns %s type %d", name, qtype)

		resp := make([]byte, 0, end+4+16)
		resp = append(resp, q[0], q[1])
		resp = append(resp, 0x81, 0x80) // response, recursion desired and available, NOERROR
		resp = append(resp, 0, 1)       // one question
		if qtype == 1 {
			resp = append(resp, 0, 1)
		} else {
			resp = append(resp, 0, 0)
		}
		resp = append(resp, 0, 0, 0, 0)
		resp = append(resp, q[12:end+4]...)
		if qtype == 1 {
			resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4, 198, 18, 0, 1)
		}
		_, _ = pc.WriteTo(resp, from)
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
