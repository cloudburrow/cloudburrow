package main

import (
	"bufio"
	"crypto/tls"
	"net"
	"sync"
	"time"

	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Proxyless gRPC security (#2160, internal/service/compute/lbxdssec.go):
// the client and server TLS policies as the xDS API delivers them, and the
// Compute Engine port taking the xDS API's TLS beside its plaintext HTTP.

// XDSClientTLS implements compute.XDSPolicies.
func (c computeNetSec) XDSClientTLS(name string) (compute.XDSClientTLS, bool) {
	p, ok := c.ns.ClientTLSPolicy(name)
	if !ok {
		return compute.XDSClientTLS{}, false
	}
	out := compute.XDSClientTLS{SNI: p.GetSni(), CertInstance: p.GetClientCertificate().GetCertificateProviderInstance().GetPluginInstance()}
	out.CAInstances = caInstances(p.GetServerValidationCa())
	return out, true
}

// XDSServerTLS implements compute.XDSPolicies.
func (c computeNetSec) XDSServerTLS(name string) (compute.XDSServerTLS, bool) {
	p, ok := c.ns.ServerTLSPolicy(name)
	if !ok {
		return compute.XDSServerTLS{}, false
	}
	out := compute.XDSServerTLS{AllowOpen: p.GetAllowOpen(), CertInstance: p.GetServerCertificate().GetCertificateProviderInstance().GetPluginInstance()}
	if mt := p.GetMtlsPolicy(); mt != nil && len(mt.GetClientValidationCa()) > 0 {
		out.MTLS = true
		out.CAInstances = caInstances(mt.GetClientValidationCa())
	}
	return out, true
}

// XDSEndpointPolicies implements compute.XDSPolicies: Network Services'
// endpoint policies, when they are served (c.endpoints).
func (c computeNetSec) XDSEndpointPolicies(project string) []compute.XDSEndpointPolicy {
	if c.endpoints == nil {
		return nil
	}
	return c.endpoints(project)
}

func caInstances(l []*nspb.ValidationCA) []string {
	var out []string
	for _, v := range l {
		if n := v.GetCertificateProviderInstance().GetPluginInstance(); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// tlsSniffListener hands a connection whose first byte is a TLS record
// (0x16, a handshake) to TLS with cfg and any other as it is, so one port
// serves plaintext HTTP and the xDS API over TLS. Each connection is
// sniffed on its own goroutine, so a slow client holds up no other.
type tlsSniffListener struct {
	net.Listener
	cfg   *tls.Config
	conns chan net.Conn
	errs  chan error
	once  sync.Once
}

func newTLSSniffListener(ln net.Listener, cfg *tls.Config) *tlsSniffListener {
	return &tlsSniffListener{Listener: ln, cfg: cfg, conns: make(chan net.Conn), errs: make(chan error, 1)}
}

// peekedConn is a connection whose first bytes were read into r.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

func (l *tlsSniffListener) Accept() (net.Conn, error) {
	l.once.Do(func() {
		go func() {
			for {
				c, err := l.Listener.Accept()
				if err != nil {
					l.errs <- err
					return
				}
				go func() {
					_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
					br := bufio.NewReader(c)
					b, err := br.Peek(1)
					_ = c.SetReadDeadline(time.Time{})
					if err != nil {
						_ = c.Close()
						return
					}
					var out net.Conn = &peekedConn{Conn: c, r: br}
					if b[0] == 0x16 {
						out = tls.Server(out, l.cfg)
					}
					l.conns <- out
				}()
			}
		}()
	})
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		l.errs <- err
		return nil, err
	}
}

var _ compute.XDSPolicies = computeNetSec{}
