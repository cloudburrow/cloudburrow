package alloydb

// The instances' addresses (#2191). On Google every instance of a
// private-services-access cluster has a private IP address of its own,
// where it serves PostgreSQL on port 5432 (and its managed connection
// pooler on 6432), and the connectors reach it at that address on port
// 5433 (docs.cloud.google.com/alloydb/docs/connect-overview, read
// 2026-11-23). The connectors dial the address GetConnectionInfo answers
// and send no TLS server name for an address, so the address dialled is
// how the server-side proxy tells the instances apart.
//
// Here each instance takes a loopback address of its own from
// Options.InstanceAddresses (127.0.0.2 onwards: every address of
// 127.0.0.0/8 is the host's on Linux; on macOS only those an
// administrator aliased onto lo0), and the server listens there on the
// connectors' port, on 5432 and on 6432. An instance that finds no free
// address shares Options.ConnectorHost (the bind address), where a
// connection reaches the one instance of the client certificate's cluster
// that has no address of its own, or its primary; such an instance is
// reached directly only through the console's or `cloudburrow env`'s
// server address.
//
// A direct connection on 5432 (or 6432) speaks PostgreSQL's SSL
// negotiation (an SSLRequest, or TLS at once, PostgreSQL 17's direct SSL)
// with a certificate the cluster's CA signs for the address, as Google's
// servers do; a plaintext one is refused unless the instance's
// ssl_config.ssl_mode is ALLOW_UNENCRYPTED_AND_ENCRYPTED, and every one is
// refused when client_connection_config.require_connectors is set.

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"cloud.google.com/go/alloydb/connectors/apiv1alpha/connectorspb"
)

// DirectPort is the port an instance serves PostgreSQL on at its address.
const DirectPort = 5432

// DefaultInstanceAddresses are the loopback addresses instances take, in
// order: 127.0.0.2 to 127.0.0.254.
func DefaultInstanceAddresses() []string {
	out := make([]string, 0, 253)
	for i := 2; i <= 254; i++ {
		out = append(out, "127.0.0."+strconv.Itoa(i))
	}
	return out
}

// ListenPorts move the instance addresses' ports, for tests: 0 for
// Connector is the shared listener's.
type ListenPorts struct{ Connector, Direct, Pooler int }

type listenKind int

const (
	kindConnector listenKind = iota
	kindDirect
	kindPooler
)

// Connector is the server's listeners: the connectors' on the shared
// address, and each instance's own.
type Connector struct {
	s     *Server
	ports [3]int // by listenKind

	mu     sync.Mutex
	shared net.Listener
	// own are the listeners at each instance address, by address.
	own map[string][]net.Listener
	wg  sync.WaitGroup
}

// ServeConnector listens on addr (host:5433) for the connectors, and at
// the recorded instances' own addresses, until Close.
func (s *Server) ServeConnector(addr string) (*Connector, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	c := &Connector{s: s, shared: ln, own: map[string][]net.Listener{}}
	c.ports = [3]int{port, DirectPort, PoolerPort}
	if p := s.opts.Ports; p != nil {
		c.ports[kindDirect], c.ports[kindPooler] = p.Direct, p.Pooler
		if p.Connector != 0 {
			c.ports[kindConnector] = p.Connector
		}
	}
	c.wg.Add(1)
	go c.accept(ln, kindConnector, "")
	s.mu.Lock()
	s.conn = c
	host := s.connectorHost()
	var addrs []string
	for _, k := range sortedKeys(s.instances, "") {
		if ip := s.instances[k].GetIpAddress(); ip != "" && ip != host {
			addrs = append(addrs, ip)
		}
	}
	s.mu.Unlock()
	for _, ip := range addrs {
		if err := c.listenAt(ip); err != nil {
			s.logf("alloydb: listening at instance address %s: %v", ip, err)
		}
	}
	return c, nil
}

// Addr is the shared listener's address.
func (c *Connector) Addr() string {
	if c == nil || c.shared == nil {
		return ""
	}
	return c.shared.Addr().String()
}

// Close stops every listener; connections in progress go on.
func (c *Connector) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	err := c.shared.Close()
	for _, lns := range c.own {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}
	c.own = map[string][]net.Listener{}
	c.mu.Unlock()
	c.wg.Wait()
	return err
}

// listenAt listens at an instance's own address ip on the connectors',
// the direct and the pooler's ports. The connectors' port must bind; a
// direct port that another program holds is skipped, and logged.
func (c *Connector) listenAt(ip string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.own[ip]; ok {
		return nil
	}
	first, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(c.ports[kindConnector])))
	if err != nil {
		return err
	}
	lns := []net.Listener{first}
	c.wg.Add(1)
	go c.accept(first, kindConnector, ip)
	for _, k := range []listenKind{kindDirect, kindPooler} {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(c.ports[k])))
		if err != nil {
			c.s.logf("alloydb: instance address %s serves no connections on port %d: %v", ip, c.ports[k], err)
			continue
		}
		lns = append(lns, ln)
		c.wg.Add(1)
		go c.accept(ln, k, ip)
	}
	c.own[ip] = lns
	return nil
}

// release stops the listeners at ip.
func (c *Connector) release(ip string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	lns := c.own[ip]
	delete(c.own, ip)
	c.mu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// allocateAddress is an address for a new instance: the first of
// Options.InstanceAddresses no instance has that the listeners bind, or
// the shared address; the caller holds mu.
func (s *Server) allocateAddress() string {
	host := s.connectorHost()
	if s.conn == nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return host
	}
	taken := map[string]bool{}
	for _, in := range s.instances {
		taken[in.GetIpAddress()] = true
	}
	for _, ip := range s.opts.InstanceAddresses {
		if taken[ip] || ip == host {
			continue
		}
		err := s.conn.listenAt(ip)
		if err == nil {
			return ip
		}
		if !errors.Is(err, syscall.EADDRNOTAVAIL) {
			s.logf("alloydb: instance address %s: %v", ip, err)
		}
	}
	return host
}

// releaseAddress stops the listeners at a removed instance's own address;
// the caller holds mu.
func (s *Server) releaseAddress(in *alloydbpb.Instance) {
	if ip := in.GetIpAddress(); ip != "" && ip != s.connectorHost() {
		s.conn.release(ip)
	}
}

func (c *Connector) accept(ln net.Listener, kind listenKind, ip string) {
	defer c.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		switch kind {
		case kindConnector:
			go c.serveConnector(conn, ip)
		default:
			go c.serveDirect(conn, ip, kind == kindPooler)
		}
	}
}

func (c *Connector) tlsConfig(local string, clientCert bool) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = local
			}
			return c.s.ca.serverCert(name)
		},
	}
	if clientCert {
		cfg.MinVersion = tls.VersionTLS13
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = c.s.ca.pool
	}
	return cfg
}

// serveConnector serves one connector's connection: TLS with its client
// certificate, the metadata exchange, and then the instance's PostgreSQL.
// ip is the instance address it was made to, "" for the shared one.
func (c *Connector) serveConnector(raw net.Conn, ip string) {
	defer raw.Close()
	host := ip
	if host == "" {
		host = hostOnly(raw.LocalAddr().String())
		if h := c.s.connectorHost(); net.ParseIP(h) != nil {
			host = h
		}
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	conn := tls.Server(raw, c.tlsConfig(host, true))
	if err := conn.Handshake(); err != nil {
		return
	}
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return
	}
	client := st.PeerCertificates[0]
	cluster := client.Subject.CommonName
	mdx := false
	for _, ou := range client.Subject.OrganizationalUnit {
		mdx = mdx || ou == mdxMarker
	}
	r := bufio.NewReader(conn)
	var authType connectorspb.MetadataExchangeRequest_AuthType
	var token string
	if mdx {
		req, err := readMDX(r)
		if err != nil {
			return
		}
		authType, token = req.GetAuthType(), req.GetOauth2Token()
	}
	addr, err := c.s.route(cluster, st.ServerName, ip, authType, token)
	if mdx {
		resp := &connectorspb.MetadataExchangeResponse{ResponseCode: connectorspb.MetadataExchangeResponse_OK}
		if err != nil {
			resp = &connectorspb.MetadataExchangeResponse{ResponseCode: connectorspb.MetadataExchangeResponse_ERROR, Error: err.Error()}
		}
		if werr := writeMDX(conn, resp); werr != nil {
			return
		}
	}
	if err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	pipe(conn, r, addr)
}

// pipe copies between client (read through r) and the server at addr.
func pipe(client net.Conn, r io.Reader, addr string) {
	backend, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return
	}
	defer backend.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, r); closeWrite(backend); done <- struct{}{} }()
	go func() {
		_, _ = io.Copy(client, backend)
		if t, ok := client.(*tls.Conn); ok {
			_ = t.CloseWrite()
		} else {
			closeWrite(client)
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// The PostgreSQL protocol's negotiation codes.
const (
	pgSSLRequest    = 80877103
	pgGSSENCRequest = 80877104
)

// pgRefuse sends a FATAL ErrorResponse with code and msg.
func pgRefuse(w io.Writer, code, msg string) {
	var body []byte
	for _, f := range []struct {
		t byte
		v string
	}{{'S', "FATAL"}, {'V', "FATAL"}, {'C', code}, {'M', msg}} {
		body = append(append(append(body, f.t), f.v...), 0)
	}
	body = append(body, 0)
	out := []byte{'E', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(body)+4))
	_, _ = w.Write(append(out, body...))
}

// serveDirect serves one direct connection to an instance's address: on
// 5432 its PostgreSQL, on 6432 its pooler.
func (c *Connector) serveDirect(raw net.Conn, ip string, pooled bool) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	in, ok := c.s.instanceAt(ip)
	if !ok {
		return
	}
	var client net.Conn = raw
	r := bufio.NewReader(raw)
	encrypted := false
	for {
		first, err := r.Peek(1)
		if err != nil {
			return
		}
		if first[0] == 0x16 {
			// TLS at once: PostgreSQL 17's sslnegotiation=direct.
			t := tls.Server(&peekedConn{Conn: raw, r: r}, c.tlsConfig(ip, false))
			if err := t.Handshake(); err != nil {
				return
			}
			client, r, encrypted = t, bufio.NewReader(t), true
			break
		}
		head, err := r.Peek(8)
		if err != nil {
			return
		}
		code := binary.BigEndian.Uint32(head[4:])
		if code == pgGSSENCRequest {
			_, _ = r.Discard(8)
			_, _ = raw.Write([]byte{'N'})
			continue
		}
		if code == pgSSLRequest && !encrypted {
			_, _ = r.Discard(8)
			_, _ = raw.Write([]byte{'S'})
			t := tls.Server(&peekedConn{Conn: raw, r: r}, c.tlsConfig(ip, false))
			if err := t.Handshake(); err != nil {
				return
			}
			client, r, encrypted = t, bufio.NewReader(t), true
		}
		break
	}
	cc := in.GetClientConnectionConfig()
	switch {
	case cc.GetRequireConnectors():
		// UNVERIFIED: Google's message.
		// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/configure-connectivity.)
		pgRefuse(client, "28000", "this instance only accepts connections through the AlloyDB Auth Proxy or connectors (require_connectors)")
		return
	case !encrypted && cc.GetSslConfig().GetSslMode() != alloydbpb.SslConfig_ALLOW_UNENCRYPTED_AND_ENCRYPTED:
		pgRefuse(client, "28000", "connection requires SSL: the instance's ssl_mode is "+cc.GetSslConfig().GetSslMode().String())
		return
	}
	addr, err := c.s.serverAddr(in, pooled, false)
	if err != nil {
		pgRefuse(client, "08006", err.Error())
		return
	}
	_ = raw.SetDeadline(time.Time{})
	pipe(client, r, addr)
}

// peekedConn reads what a bufio.Reader buffered before the connection.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// instanceAt is the instance whose own address ip is.
func (s *Server) instanceAt(ip string) (*alloydbpb.Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range sortedKeys(s.instances, "") {
		if s.instances[k].GetIpAddress() == ip {
			return clone(s.instances[k]), true
		}
	}
	return nil, false
}

// serverAddr is the address of in's PostgreSQL, or of its pooler when
// pooled (the connectors' pool when connectors).
func (s *Server) serverAddr(in *alloydbpb.Instance, pooled, connectors bool) (string, error) {
	if in.GetState() != alloydbpb.Instance_READY {
		return "", fmt.Errorf("instance %s is %s", in.GetName(), in.GetState())
	}
	if s.opts.Engine == nil {
		return "", errors.New("no engine runs AlloyDB instances here")
	}
	server := serverName(in.GetName())
	if pooled {
		if !in.GetConnectionPoolConfig().GetEnabled() {
			return "", fmt.Errorf("instance %s does not pool connections (connection_pool_config.enabled)", in.GetName())
		}
		if a := s.opts.Engine.PoolerAddr(server, connectors); a != "" {
			return a, nil
		}
		return "", fmt.Errorf("the connection pooler of instance %s does not answer", in.GetName())
	}
	if a := s.opts.Engine.Addr(server); a != "" {
		return a, nil
	}
	return "", fmt.Errorf("instance %s does not answer", in.GetName())
}

// route is the PostgreSQL address of the instance a connector's
// connection reaches: the instance whose PSC DNS name sni is; or the one
// whose own address ip is; or, at the shared address, the one instance of
// cluster that has none of its own, or else its primary. An instance that
// pools connections is reached through its pooler. An AUTO_IAM
// connection's token must be one this instance minted, for a principal
// the cluster has an IAM user for.
func (s *Server) route(cluster, sni, ip string, auth connectorspb.MetadataExchangeRequest_AuthType, token string) (string, error) {
	s.mu.Lock()
	var in *alloydbpb.Instance
	insts := sortedKeys(s.instances, cluster+"/instances/")
	switch {
	case sni != "" && net.ParseIP(sni) == nil:
		for _, k := range insts {
			if PSCDNSName(k) == strings.TrimSuffix(strings.ToLower(sni), ".") {
				in = s.instances[k]
			}
		}
		if in == nil {
			s.mu.Unlock()
			return "", fmt.Errorf("no instance of cluster %s is named %s", cluster, sni)
		}
	case ip != "":
		for _, k := range insts {
			if s.instances[k].GetIpAddress() == ip {
				in = s.instances[k]
			}
		}
		if in == nil {
			s.mu.Unlock()
			return "", fmt.Errorf("no instance of cluster %s has the address %s", cluster, ip)
		}
	default:
		host := s.connectorHost()
		var shared []*alloydbpb.Instance
		for _, k := range insts {
			if s.instances[k].GetIpAddress() == host {
				shared = append(shared, s.instances[k])
			}
		}
		if len(shared) == 1 {
			in = shared[0]
		} else {
			in = s.primaryOf(cluster)
		}
	}
	if in == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("cluster %s has no primary instance", cluster)
	}
	in = clone(in)
	var iamUsers []string
	for _, k := range sortedKeys(s.users, cluster+"/users/") {
		if s.users[k].GetUserType() == alloydbpb.User_ALLOYDB_IAM_USER {
			iamUsers = append(iamUsers, strings.ToLower(k[strings.LastIndex(k, "/")+1:]))
		}
	}
	s.mu.Unlock()
	if auth == connectorspb.MetadataExchangeRequest_AUTO_IAM {
		if s.opts.IAMToken == nil {
			return "", errors.New("IAM database authentication: no token can be checked here")
		}
		email, ok := s.opts.IAMToken(token)
		if !ok {
			return "", errors.New("IAM database authentication failed: the OAuth 2.0 token is not valid")
		}
		found := false
		for _, u := range iamUsers {
			found = found || u == strings.ToLower(email)
		}
		if !found {
			return "", fmt.Errorf("IAM database authentication failed: %s is not an IAM user of cluster %s", email, cluster)
		}
	}
	return s.serverAddr(in, in.GetConnectionPoolConfig().GetEnabled(), true)
}
