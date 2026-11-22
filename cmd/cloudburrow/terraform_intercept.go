package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/buildinfo"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// `cloudburrow terraform --intercept-hardcoded` (#1578): an opt-in, local
// HTTPS intercept for the calls hashicorp/google makes to a fixed Google
// host whatever its custom endpoints say.
//
// The two locations data sources (8.4.0 to 8.12.0 and main, re-read
// 2026-11-21) list at literal URLs: google_cloud_run_locations at
// https://run.googleapis.com/v1/projects/{{project}}/locations and
// google_artifact_registry_locations at
// https://artifactregistry.googleapis.com/v1/projects/{{project}}/locations.
// Without the flag the wrapper refuses a module declaring either; with it,
// they are served by the Run port (Cloud Run v1's list, and Artifact
// Registry's under ArtifactRegistryPrefix) and no longer refused.
//
// When a plan's google_bigquery_table schema drops a column, the provider
// lists the table's row access policies at
// https://bigquery.googleapis.com/bigquery/v2/projects/P/datasets/D/tables/T/rowAccessPolicies
// (bigQueryTableHasRowAccessPolicy in resource_bigquery_table.go, 8.4.0 to
// 8.12.0 and main), whatever big_query_custom_endpoint or universe_domain
// say: the URL is a literal, SendRequest sends it as given, and the
// provider's HTTP client has no host rewriting. It ignores that call's
// error, so without this the plan goes ahead in place even where Google's
// would replace the table.
//
// With the flag, for every subcommand that calls the provider (not init),
// the wrapper:
//   - mints a CA and a leaf certificate for the intercepted hosts alone, in
//     memory, valid one day, the CA name-constrained to those hosts. Only
//     the CA certificate is written, to a directory of its own in the
//     instance's state directory, removed when the run ends; neither private
//     key is ever written or printed. Nothing is installed in a keychain or
//     system trust store.
//   - starts an HTTPS proxy on loopback that accepts CONNECT only to the
//     intercepted hosts on 443, terminates TLS there with the leaf, and
//     sends each request, path unchanged, to the instance's own front for
//     that service. Every other CONNECT and every plain proxy request is
//     refused 403: nothing is ever forwarded to the internet.
//   - runs the binary alone with HTTPS_PROXY and HTTP_PROXY at that proxy,
//     NO_PROXY reset to loopback (a user's NO_PROXY could otherwise exempt
//     googleapis.com), SSL_CERT_FILE and SSL_CERT_DIR naming only that CA,
//     and GODEBUG x509sslcertoverrideplatform=1, so a provider built with Go
//     1.27 or later uses that file on macOS too. A provider built with an
//     earlier Go on macOS ignores SSL_CERT_FILE and keeps the system's trust;
//     its call then fails the TLS check (still locally) and the wrapper says
//     so. Linux honours SSL_CERT_FILE with every Go version.

// interceptedHost is one fixed Google host the intercept serves locally.
type interceptedHost struct {
	host    string         // the provider's literal host, port 443
	service config.Service // the terraformEndpoints row whose front serves it
	attr    string         // that row's provider argument, for its host and port
	prefix  string         // the only paths sent on; anything else is 403
	under   string         // prepended to the path on the front ("" for none)
	unlocks []string       // the data sources unpointedResources no longer refuses
}

var interceptedHosts = []interceptedHost{
	{host: "bigquery.googleapis.com", service: config.ServiceBigQuery, attr: "big_query_custom_endpoint", prefix: "/bigquery/v2/"},
	// cloud_run_v2_custom_endpoint for the Run port's address: the v1
	// argument's host is {{location}}-run.localhost on loopback, a template.
	{host: "run.googleapis.com", service: config.ServiceRun, attr: "cloud_run_v2_custom_endpoint", prefix: "/v1/projects/",
		unlocks: []string{"google_cloud_run_locations"}},
	// The Run port's plain /v1/projects/{p}/locations is Cloud Run v1's
	// list; Artifact Registry's is under run.ArtifactRegistryPrefix.
	{host: "artifactregistry.googleapis.com", service: config.ServiceRun, attr: "artifact_registry_custom_endpoint", prefix: "/v1/projects/",
		under: runadapter.ArtifactRegistryPrefix, unlocks: []string{"google_artifact_registry_locations"}},
}

// interceptRoutes are the intercepted hosts cfg's instance serves, each
// with its front's scheme and address.
func interceptRoutes(cfg config.Config) (map[string]*url.URL, error) {
	set, _ := terraformSettings(cfg)
	routes := map[string]*url.URL{}
	for _, h := range interceptedHosts {
		for _, e := range set {
			if e.service != h.service || e.attr != h.attr {
				continue
			}
			u, err := url.Parse(e.url)
			if err != nil {
				return nil, err
			}
			routes[h.host] = &url.URL{Scheme: u.Scheme, Host: u.Host}
		}
	}
	return routes, nil
}

// interceptUnlocked drops from families the data sources the intercept
// serves for cfg's instance, so a module declaring one is no longer refused.
func interceptUnlocked(cfg config.Config, families []unpointedPrefix) []unpointedPrefix {
	routes, err := interceptRoutes(cfg)
	if err != nil {
		return families
	}
	unlocked := map[string]bool{}
	for _, h := range interceptedHosts {
		if routes[h.host] != nil {
			for _, d := range h.unlocks {
				unlocked[d] = true
			}
		}
	}
	var out []unpointedPrefix
	for _, f := range families {
		if !unlocked[f.prefix] {
			out = append(out, f)
		}
	}
	return out
}

// interceptFlag is the wrapper's own flag that turns the intercept on.
const interceptFlag = "intercept-hardcoded"

// terraformIntercept is a running intercept: its proxy and CA directory.
type terraformIntercept struct {
	dir      string
	caFile   string
	proxy    *http.Server
	inner    *http.Server
	listener net.Listener
	conns    *connListener
	hosts    []string                 // the hosts served, in interceptedHosts' order
	served   map[string]*atomic.Int64 // per host
	refused  sync.Map                 // host -> struct{}
	log      io.Writer
}

// startIntercept starts the intercept for cfg's instance. It fails when no
// intercepted host's service is served, since there is nowhere to send it.
func startIntercept(cfg config.Config, log io.Writer) (*terraformIntercept, error) {
	routes, err := interceptRoutes(cfg)
	if err != nil {
		return nil, err
	}
	byHost := map[string]interceptedHost{}
	var hosts []string
	for _, h := range interceptedHosts {
		if routes[h.host] != nil {
			byHost[h.host] = h
			hosts = append(hosts, h.host)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("--%s: this instance serves none of the services it intercepts (BigQuery, Cloud Run, Artifact Registry)", interceptFlag)
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(cfg.InstanceDir(), "terraform-intercept-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	caPEM, leaf, err := mintInterceptCerts(hosts, time.Now())
	if err != nil {
		return nil, err
	}
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	served := map[string]*atomic.Int64{}
	for _, h := range hosts {
		served[h] = new(atomic.Int64)
	}
	ic := &terraformIntercept{dir: dir, caFile: caFile, listener: ln, log: log, hosts: hosts, served: served,
		conns: &connListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: ln.Addr()}}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			under := byHost[hostOnly(r.In.Host)].under
			r.SetURL(routes[hostOnly(r.In.Host)])
			r.Out.URL.Path = under + r.In.URL.Path
			if r.In.URL.RawPath != "" {
				r.Out.URL.RawPath = under + r.In.URL.RawPath
			} else {
				r.Out.URL.RawPath = ""
			}
		},
	}
	ic.inner = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := hostOnly(r.Host)
			if routes[h] == nil || !strings.HasPrefix(r.URL.Path, byHost[h].prefix) {
				ic.refuse(w, h+r.URL.Path)
				return
			}
			ic.served[h].Add(1)
			rp.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	go func() { _ = ic.inner.Serve(tls.NewListener(ic.conns, tlsCfg)) }()
	ic.proxy = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, port, err := net.SplitHostPort(r.Host)
			if r.Method != http.MethodConnect || err != nil || port != "443" || routes[host] == nil {
				ic.refuse(w, r.Host)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "cannot hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
				_ = conn.Close()
				return
			}
			if !ic.conns.push(conn) {
				_ = conn.Close()
			}
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() { _ = ic.proxy.Serve(ln) }()
	ok = true
	return ic, nil
}

// refuse answers 403 for anything but an intercepted host, and remembers it
// so the run ends naming what was refused.
func (ic *terraformIntercept) refuse(w http.ResponseWriter, what string) {
	ic.refused.Store(what, struct{}{})
	http.Error(w, "cloudburrow terraform --"+interceptFlag+": refused "+what+": only "+
		strings.Join(interceptedHostNames(), ", ")+" are served, locally; nothing is forwarded to the internet", http.StatusForbidden)
}

// env is the binary's environment: base without any proxy or trust setting
// of its own, plus the intercept's.
func (ic *terraformIntercept) env(base []string, bindAddress string) []string {
	drop := map[string]bool{"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	var out []string
	godebug := ""
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		if drop[strings.ToUpper(k)] {
			continue
		}
		if k == "GODEBUG" {
			godebug = v
			continue
		}
		out = append(out, kv)
	}
	if godebug != "" {
		godebug += ","
	}
	proxy := "http://" + ic.listener.Addr().String()
	noProxy := "127.0.0.1,localhost,::1"
	if bindAddress != "" && !strings.Contains(noProxy, bindAddress) {
		noProxy += "," + bindAddress
	}
	return append(out,
		"HTTPS_PROXY="+proxy, "https_proxy="+proxy, "HTTP_PROXY="+proxy, "http_proxy="+proxy,
		"NO_PROXY="+noProxy, "no_proxy="+noProxy,
		"SSL_CERT_FILE="+ic.caFile, "SSL_CERT_DIR="+ic.dir,
		"GODEBUG="+godebug+"x509sslcertoverrideplatform=1")
}

// stop shuts the proxy down, removes the CA directory and reports what the
// intercept served and refused.
func (ic *terraformIntercept) stop() {
	_ = ic.proxy.Close()
	ic.conns.close()
	_ = ic.inner.Close()
	_ = os.RemoveAll(ic.dir)
	var refused []string
	ic.refused.Range(func(k, _ any) bool { refused = append(refused, k.(string)); return true })
	sort.Strings(refused)
	for _, h := range ic.hosts {
		fmt.Fprintf(ic.log, "cloudburrow terraform: --%s served %d request(s) to %s locally\n", interceptFlag, ic.served[h].Load(), h)
	}
	if len(refused) != 0 {
		fmt.Fprintf(ic.log, "cloudburrow terraform: --%s refused %s\n", interceptFlag, strings.Join(refused, ", "))
	}
}

func interceptedHostNames() []string {
	var out []string
	for _, h := range interceptedHosts {
		out = append(out, h.host)
	}
	return out
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// mintInterceptCerts makes a CA, name-constrained to hosts, and a leaf for
// hosts signed by it. It returns the CA certificate's PEM, and the leaf with
// its key, which stays in memory. The CA's key is dropped on return.
func mintInterceptCerts(hosts []string, now time.Time) ([]byte, tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	serial := func() *big.Int {
		n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
		return n
	}
	ca := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "cloudburrow terraform intercept (ephemeral)"},
		NotBefore:                   now.Add(-time.Minute),
		NotAfter:                    now.Add(24 * time.Hour),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, nil
}

// interceptTrustWarning names, on macOS, google providers in the module's
// .terraform directory built with a Go older than 1.27, which ignore
// SSL_CERT_FILE there. "" when there are none or this is not macOS.
func interceptTrustWarning(dir, goos string) string {
	if goos != "darwin" {
		return ""
	}
	bins, _ := filepath.Glob(filepath.Join(dir, ".terraform", "providers", "*", "hashicorp", "google", "*", "*", "terraform-provider-google*"))
	var old []string
	for _, b := range bins {
		info, err := buildinfo.ReadFile(b)
		if err != nil || !goBefore127(info.GoVersion) {
			continue
		}
		old = append(old, filepath.Base(b)+" ("+info.GoVersion+")")
	}
	if len(old) == 0 {
		return ""
	}
	return fmt.Sprintf("cloudburrow terraform: --%s: %s was built with a Go before 1.27, which on macOS ignores SSL_CERT_FILE; "+
		"its calls to %s will fail their TLS check (locally: nothing reaches Google)", interceptFlag, strings.Join(old, ", "),
		strings.Join(interceptedHostNames(), ", "))
}

// goBefore127 reports whether a "go1.N[.P]" version is before go1.27.
func goBefore127(v string) bool {
	rest, ok := strings.CutPrefix(v, "go1.")
	if !ok {
		return false
	}
	minor := rest
	if i := strings.IndexAny(rest, ".-+ "); i >= 0 {
		minor = rest[:i]
	}
	n := 0
	for _, c := range minor {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n < 27
}

// connListener hands hijacked CONNECT tunnels to the inner TLS server.
type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func (l *connListener) push(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) close()         { l.once.Do(func() { close(l.done) }) }
func (l *connListener) Close() error   { l.close(); return nil }
func (l *connListener) Addr() net.Addr { return l.addr }
