// Package redistlsfront is the TLS endpoint of a Memorystore for Redis
// server before Redis 6.0 with in-transit encryption (#1768):
// `cloudburrow-storage redis-tls-front`, a second container in the
// server's pod.
//
// Redis 3.2, 4.0 and 5.0 have no TLS of their own ("Native TLS was not
// supported prior to open source Redis version 6.0", Memorystore's
// in-transit encryption page), and Memorystore serves in-transit
// encryption on 4.0 and 5.0 with an endpoint of its own in front of the
// server: the page's connection limits for those versions (1,000 on M1)
// are those of a terminating proxy, not of the server. The front is that
// endpoint, as Memorystore's is to its clients: TLS 1.2 or later on port
// 6378 with the instance CA's server certificate, clients not asked for
// one (SERVER_AUTHENTICATION), and the server itself on the pod's
// loopback alone, so a plain TCP client reaches nothing.
//
// Replication between a Standard tier instance's nodes goes over TLS too:
// a replica's server, which can only dial plain TCP, is made a replica of
// a relay on its pod's loopback (RelayPort), which the front forwards over
// TLS, verifying the instance's CA, to the node (or the instance's
// Service) that port names.
package redistlsfront

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/frontready"
)

// ErrUsage is a command line Run refused.
var ErrUsage = errors.New("usage")

// The environment the front's certificate, key and the instance's CA
// certificates are given in, as PEM.
const (
	EnvCert = "MEMORYSTORE_TLS_CERT"
	EnvKey  = "MEMORYSTORE_TLS_KEY"
	EnvCA   = "MEMORYSTORE_TLS_CA"
)

// TLSPort is Memorystore's in-transit encryption port, which the front
// serves and every node's front is dialled on.
const TLSPort = 6378

// RelayBase is the loopback port of the relay to the instance's own
// Service; node N's relay is RelayBase+1+N, for N below RelayNodes.
const (
	RelayBase  = 16400
	RelayNodes = 64
)

var nodeSuffix = regexp.MustCompile(`-n([0-9]+)$`)

// RelayPort is the loopback port of the relay to server, a Standard tier
// node ("<instance>-nN") or the instance's own Service; false for a node
// past RelayNodes.
func RelayPort(server string) (int, bool) {
	m := nodeSuffix.FindStringSubmatch(server)
	if m == nil {
		return RelayBase, true
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n >= RelayNodes {
		return 0, false
	}
	return RelayBase + 1 + n, true
}

// Base is the instance's server name of server, a node's or its own.
func Base(server string) string { return nodeSuffix.ReplaceAllString(server, "") }

// Run is `cloudburrow-storage redis-tls-front`.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("redis-tls-front", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address the front serves TLS on")
	upstream := fs.String("upstream", "", "the server's plain address, on the pod's loopback")
	instance := fs.String("instance", "", "the instance's server name, whose nodes the relays reach")
	namespace := fs.String("namespace", "", "the namespace of the instance's Services")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *listen == "" || *upstream == "" || (*instance == "") != (*namespace == "") {
		fmt.Fprintln(stderr, "redis-tls-front: --listen and --upstream are required, --instance and --namespace go together")
		return ErrUsage
	}
	cert, err := tls.X509KeyPair([]byte(os.Getenv(EnvCert)), []byte(os.Getenv(EnvKey)))
	if err != nil {
		return fmt.Errorf("the server certificate in %s and %s: %w", EnvCert, EnvKey, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(os.Getenv(EnvCA))) {
		return fmt.Errorf("%s holds no CA certificate", EnvCA)
	}
	f := &Front{Upstream: *upstream, Server: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Roots: pool}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	var lns []net.Listener
	lns = append(lns, ln)
	if *instance != "" {
		for n := -1; n < RelayNodes; n++ {
			server := *instance
			if n >= 0 {
				server += "-n" + strconv.Itoa(n)
			}
			port, _ := RelayPort(server)
			rl, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				closeAll(lns)
				return err
			}
			lns = append(lns, rl)
			host := server + "." + *namespace + ".svc.cluster.local"
			go f.ServeRelay(rl, host)
		}
	}
	go func() {
		<-ctx.Done()
		closeAll(lns)
	}()
	fmt.Fprintf(stdout, "redis-tls-front: TLS on %s for %s\n", ln.Addr(), *upstream)
	err = f.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func closeAll(lns []net.Listener) {
	for _, l := range lns {
		_ = l.Close()
	}
}

// Front terminates TLS for Upstream.
type Front struct {
	Upstream string
	// Server is the TLS configuration clients are served with.
	Server *tls.Config
	// Roots are the instance's CA certificates, which a relay verifies a
	// node's certificate against.
	Roots *x509.CertPool
	// PeerPort is the port a relay dials its node's front on: TLSPort
	// when 0.
	PeerPort int
}

// handshakeTimeout bounds a client's TLS handshake, or its readiness
// request.
const handshakeTimeout = 10 * time.Second

// Serve accepts clients on ln until it is closed. A connection that opens
// with an HTTP GET is the kubelet's readiness probe of the server's
// container (frontready.Path), answered in plain HTTP and nothing else;
// every other one must be TLS.
func (f *Front) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go f.serveConn(c)
	}
}

func (f *Front) serveConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	pc := &peekedConn{Conn: c, r: br}
	if first[0] == 'G' {
		req, err := http.ReadRequest(br)
		if err != nil || req.URL.Path != frontready.Path {
			_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		status := "200 OK"
		if ok, _ := frontready.Ready(f.Upstream); !ok {
			status = "503 Service Unavailable"
		}
		_, _ = io.WriteString(c, "HTTP/1.1 "+status+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	tc := tls.Server(pc, f.Server)
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	up, err := net.DialTimeout("tcp", f.Upstream, handshakeTimeout)
	if err != nil {
		return
	}
	pipe(tc, up)
}

// ServeRelay accepts the server's replication connections on ln, each
// forwarded over TLS to host's front, which must present a certificate of
// the instance's CA for host.
func (f *Front) ServeRelay(ln net.Listener, host string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			d := &net.Dialer{Timeout: handshakeTimeout}
			port := f.PeerPort
			if port == 0 {
				port = TLSPort
			}
			up, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, strconv.Itoa(port)),
				&tls.Config{RootCAs: f.Roots, ServerName: host, MinVersion: tls.VersionTLS12})
			if err != nil {
				return
			}
			pipe(c, up)
		}()
	}
}

// pipe copies a and b both ways until either ends.
func pipe(a, b net.Conn) {
	var once sync.Once
	done := make(chan struct{})
	end := func() { once.Do(func() { close(done) }) }
	go func() { _, _ = io.Copy(a, b); end() }()
	go func() { _, _ = io.Copy(b, a); end() }()
	<-done
	_ = a.Close()
	_ = b.Close()
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }
