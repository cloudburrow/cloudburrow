package sqladmin

// The connector listener's side of IAM database authentication (#1944,
// iamauth.go): after the TLS handshake, the first messages of the
// database protocol are read here. A sign-in as an IAM user of an
// instance whose IAM flag is on is checked against the access token, and
// then made to the server as the role with the role's own password; the
// server's answer from AuthenticationOk (PostgreSQL) or OK (MySQL) on is
// passed through, and the connection is spliced as any other. Any other
// sign-in is passed to the server untouched.

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // PostgreSQL's md5 method is MD5 by definition.
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // MySQL's scrambles are SHA-1 by definition.
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// bufConn is a connection read through a buffered reader that may hold
// bytes already read from it.
type bufConn struct {
	*bufio.Reader
	net.Conn
}

func (b bufConn) Read(p []byte) (int, error) { return b.Reader.Read(p) }

// CloseWrite half-closes the underlying connection.
func (b bufConn) CloseWrite() error {
	switch c := b.Conn.(type) {
	case *net.TCPConn:
		return c.CloseWrite()
	case *tls.Conn:
		return c.CloseWrite()
	}
	return nil
}

// certTokenHash is the access token's SHA-256 the client certificate
// carries, "" for none.
func certTokenHash(tc *tls.Conn) string {
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return ""
	}
	for _, u := range certs[0].URIs {
		if h, ok := strings.CutPrefix(u.String(), tokenURNPrefix); ok {
			return h
		}
	}
	return ""
}

// iamSignIn reads the client's first messages and connects to the server
// at addr: the client side to read from next, and the server's. An IAM
// sign-in is checked and made here; a refused one is answered as the
// server answers a failed sign-in, and returns an error.
func (s *Server) iamSignIn(tc *tls.Conn, project string, e Engine, certHash, addr string) (net.Conn, net.Conn, error) {
	if !s.iamFlagOn(project, e) {
		up, err := net.DialTimeout("tcp", addr, 10*time.Second)
		return tc, up, err
	}
	if _, ok := e.(iamUserEngine); !ok {
		up, err := net.DialTimeout("tcp", addr, 10*time.Second)
		return tc, up, err
	}
	if isMySQLEngine(e) {
		return s.mysqlSignIn(tc, project, e, certHash, addr)
	}
	return s.pgSignIn(tc, project, e, certHash, addr)
}

// --- PostgreSQL ---

const (
	pgSSLRequest    = 80877103
	pgGSSENCRequest = 80877104
	pgCancelRequest = 80877102
)

func pgReadStartup(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n < 8 || n > 1<<16 {
		return nil, errors.New("bad startup message length")
	}
	msg := make([]byte, n)
	copy(msg, hdr[:])
	if _, err := io.ReadFull(r, msg[4:]); err != nil {
		return nil, err
	}
	return msg, nil
}

// pgReadMsg reads one typed message: its type and body.
func pgReadMsg(r *bufio.Reader) (byte, []byte, error) {
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n < 4 || n > 1<<24 {
		return 0, nil, errors.New("bad message length")
	}
	body := make([]byte, n-4)
	_, err = io.ReadFull(r, body)
	return t, body, err
}

func pgMsg(t byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:], uint32(4+len(body)))
	return append(out, body...)
}

func pgAuth(code uint32, extra []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, code)
	return pgMsg('R', append(b, extra...))
}

// pgFatal is an ErrorResponse, as the server sends a refused sign-in.
func pgFatal(code, msg string) []byte {
	var b []byte
	for _, f := range [][2]string{{"S", "FATAL"}, {"V", "FATAL"}, {"C", code}, {"M", msg}} {
		b = append(b, f[0][0])
		b = append(append(b, f[1]...), 0)
	}
	return pgMsg('E', append(b, 0))
}

func pgStartupParams(msg []byte) map[string]string {
	out := map[string]string{}
	parts := strings.Split(string(msg[8:]), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "" {
			break
		}
		out[parts[i]] = parts[i+1]
	}
	return out
}

func (s *Server) pgSignIn(tc *tls.Conn, project string, e Engine, certHash, addr string) (net.Conn, net.Conn, error) {
	cr := bufio.NewReader(tc)
	client := bufConn{cr, tc}
	var startup []byte
	for {
		msg, err := pgReadStartup(cr)
		if err != nil {
			return nil, nil, err
		}
		code := binary.BigEndian.Uint32(msg[4:8])
		if code == pgSSLRequest || code == pgGSSENCRequest {
			// The connector's TLS is the encryption; none inside it.
			if _, err := tc.Write([]byte{'N'}); err != nil {
				return nil, nil, err
			}
			continue
		}
		startup = msg
		break
	}
	dial := func() (net.Conn, error) {
		up, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			return nil, err
		}
		if _, err := up.Write(startup); err != nil {
			up.Close()
			return nil, err
		}
		return up, nil
	}
	if binary.BigEndian.Uint32(startup[4:8]) == pgCancelRequest {
		up, err := dial()
		return client, up, err
	}
	user := pgStartupParams(startup)["user"]
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	u, isIAM := User{}, false
	if strings.Contains(user, "@") {
		u, isIAM = iamUserNamed(ctx, e, user)
	}
	if !isIAM {
		up, err := dial()
		return client, up, err
	}
	hash := certHash
	if hash == "" {
		// Manual IAM sign-in: the token is the password.
		if _, err := tc.Write(pgAuth(3, nil)); err != nil {
			return nil, nil, err
		}
		t, body, err := pgReadMsg(cr)
		if err != nil {
			return nil, nil, err
		}
		if t != 'p' {
			return nil, nil, errors.New("expected a password message")
		}
		hash = tokenHash(strings.TrimRight(string(body), "\x00"))
	}
	login, err := s.checkIAMLogin(project, e, u, hash)
	if err != nil {
		_, _ = tc.Write(pgFatal("28000", err.Error()))
		return nil, nil, err
	}
	up, err := dial()
	if err != nil {
		return nil, nil, err
	}
	ur := bufio.NewReader(up)
	var scram *scramClient
	for {
		t, body, err := pgReadMsg(ur)
		if err != nil {
			up.Close()
			return nil, nil, err
		}
		if t == 'E' {
			_, _ = tc.Write(pgMsg(t, body))
			up.Close()
			return nil, nil, errors.New("the server refused the sign-in")
		}
		if t != 'R' || len(body) < 4 {
			if _, err := tc.Write(pgMsg(t, body)); err != nil {
				up.Close()
				return nil, nil, err
			}
			continue
		}
		var reply []byte
		switch code := binary.BigEndian.Uint32(body[:4]); code {
		case 0: // AuthenticationOk
			if _, err := tc.Write(pgMsg(t, body)); err != nil {
				up.Close()
				return nil, nil, err
			}
			return client, bufConn{ur, up}, nil
		case 3: // cleartext
			reply = pgMsg('p', append([]byte(login.password), 0))
		case 5: // md5
			if len(body) < 8 {
				up.Close()
				return nil, nil, errors.New("bad md5 request")
			}
			reply = pgMsg('p', append([]byte(pgMD5(login.role, login.password, body[4:8])), 0))
		case 10: // SASL
			if !strings.Contains(string(body[4:]), "SCRAM-SHA-256\x00") {
				up.Close()
				return nil, nil, errors.New("the server offers no SCRAM-SHA-256")
			}
			scram = newScram(login.password)
			first := scram.first()
			b := append([]byte("SCRAM-SHA-256\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(first)))...)
			reply = pgMsg('p', append(b, first...))
		case 11: // SASLContinue
			if scram == nil {
				up.Close()
				return nil, nil, errors.New("SASLContinue before SASL")
			}
			final, err := scram.final(string(body[4:]))
			if err != nil {
				up.Close()
				return nil, nil, err
			}
			reply = pgMsg('p', []byte(final))
		case 12: // SASLFinal
			continue
		default:
			up.Close()
			return nil, nil, fmt.Errorf("authentication method %d is not served", code)
		}
		if _, err := up.Write(reply); err != nil {
			up.Close()
			return nil, nil, err
		}
	}
}

func pgMD5(user, password string, salt []byte) string {
	inner := md5hex([]byte(password + user))
	return "md5" + md5hex(append([]byte(inner), salt...))
}

// scramClient is SCRAM-SHA-256's client side (RFC 5802, RFC 7677), as
// PostgreSQL's SASL authentication takes it.
type scramClient struct {
	password, cnonce, firstBare string
	serverSig                   []byte
}

func newScram(password string) *scramClient {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return &scramClient{password: password, cnonce: base64.RawStdEncoding.EncodeToString(b)}
}

func (c *scramClient) first() string {
	c.firstBare = "n=,r=" + c.cnonce
	return "n,," + c.firstBare
}

func (c *scramClient) final(serverFirst string) (string, error) {
	var nonce, salt string
	iter := 0
	for _, kv := range strings.Split(serverFirst, ",") {
		switch {
		case strings.HasPrefix(kv, "r="):
			nonce = kv[2:]
		case strings.HasPrefix(kv, "s="):
			salt = kv[2:]
		case strings.HasPrefix(kv, "i="):
			iter, _ = strconv.Atoi(kv[2:])
		}
	}
	if !strings.HasPrefix(nonce, c.cnonce) || iter <= 0 {
		return "", errors.New("bad SCRAM server-first message")
	}
	saltB, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, c.password, saltB, iter, 32)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + nonce
	authMsg := c.firstBare + "," + serverFirst + "," + withoutProof
	sig := hmacSHA256(stored[:], authMsg)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ sig[i]
	}
	c.serverSig = hmacSHA256(hmacSHA256(salted, "Server Key"), authMsg)
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// --- MySQL ---

const (
	myClientConnectWithDB     = 0x00000008
	myClientProtocol41        = 0x00000200
	myClientSSL               = 0x00000800
	myClientSecureConn        = 0x00008000
	myClientPluginAuth        = 0x00080000
	myClientPluginAuthLenEnc  = 0x00200000
	myNativePassword          = "mysql_native_password"
	myCachingSHA2Password     = "caching_sha2_password"
	myClearPassword           = "mysql_clear_password"
	myAccessDeniedCode        = 1045
	myMaxHandshakeResponseLen = 1 << 16
)

func myReadPacket(r *bufio.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return hdr[3], body, err
}

func myPacket(seq byte, body []byte) []byte {
	n := len(body)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, body...)
}

// myAccessDenied is the ERR packet of a refused sign-in.
func myAccessDenied(msg string) []byte {
	b := []byte{0xff, myAccessDeniedCode & 0xff, myAccessDeniedCode >> 8, '#'}
	b = append(b, "28000"...)
	return append(b, msg...)
}

func cstring(b []byte) (string, []byte, bool) {
	i := strings.IndexByte(string(b), 0)
	if i < 0 {
		return "", nil, false
	}
	return string(b[:i]), b[i+1:], true
}

// myHandshake is what the sign-in needs from the server's greeting.
type myHandshake struct {
	nonce  []byte
	plugin string
}

func parseMyHandshake(b []byte) (myHandshake, bool) {
	var h myHandshake
	if len(b) < 1 || b[0] != 10 {
		return h, false
	}
	_, rest, ok := cstring(b[1:])
	if !ok || len(rest) < 4+8+1+2+1+2+2+1+10 {
		return h, false
	}
	rest = rest[4:]
	h.nonce = append([]byte{}, rest[:8]...)
	rest = rest[8+1:]
	rest = rest[2+1+2+2:]
	dataLen := int(rest[0])
	rest = rest[1+10:]
	n := max(13, dataLen-8)
	if len(rest) < n {
		return h, false
	}
	part2 := rest[:n]
	h.nonce = append(h.nonce, part2[:12]...)
	rest = rest[n:]
	h.plugin, _, _ = cstring(append(rest, 0))
	return h, true
}

func myScramble(plugin, password string, nonce []byte) []byte {
	switch plugin {
	case myNativePassword:
		h1 := sha1.Sum([]byte(password))                             //nolint:gosec
		h2 := sha1.Sum(h1[:])                                        //nolint:gosec
		h3 := sha1.Sum(append(append([]byte{}, nonce...), h2[:]...)) //nolint:gosec
		for i := range h1 {
			h1[i] ^= h3[i]
		}
		return h1[:]
	case myClearPassword:
		return append([]byte(password), 0)
	default: // caching_sha2_password
		h1 := sha256.Sum256([]byte(password))
		h2 := sha256.Sum256(h1[:])
		h3 := sha256.Sum256(append(h2[:], nonce...))
		for i := range h1 {
			h1[i] ^= h3[i]
		}
		return h1[:]
	}
}

func lenEnc(n int) []byte {
	switch {
	case n < 251:
		return []byte{byte(n)}
	case n < 1<<16:
		return []byte{0xfc, byte(n), byte(n >> 8)}
	default:
		return []byte{0xfd, byte(n), byte(n >> 8), byte(n >> 16)}
	}
}

func readLenEnc(b []byte) (int, []byte, bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	switch b[0] {
	case 0xfc:
		if len(b) < 3 {
			return 0, nil, false
		}
		return int(b[1]) | int(b[2])<<8, b[3:], true
	case 0xfd:
		if len(b) < 4 {
			return 0, nil, false
		}
		return int(b[1]) | int(b[2])<<8 | int(b[3])<<16, b[4:], true
	case 0xfe, 0xff:
		return 0, nil, false
	}
	return int(b[0]), b[1:], true
}

// myResponse is a HandshakeResponse41, split where the sign-in rewrites it.
type myResponse struct {
	caps   uint32
	head   []byte // capabilities, max packet, charset and filler: 32 bytes
	user   string
	db     string
	attrs  []byte
	plugin string
}

func parseMyResponse(b []byte) (myResponse, bool) {
	var r myResponse
	if len(b) < 32 {
		return r, false
	}
	r.caps = binary.LittleEndian.Uint32(b[:4])
	if r.caps&myClientProtocol41 == 0 || r.caps&myClientSSL != 0 {
		return r, false
	}
	r.head = append([]byte{}, b[:32]...)
	user, rest, ok := cstring(b[32:])
	if !ok {
		return r, false
	}
	r.user = user
	switch {
	case r.caps&myClientPluginAuthLenEnc != 0:
		n, after, ok := readLenEnc(rest)
		if !ok || len(after) < n {
			return r, false
		}
		rest = after[n:]
	case r.caps&myClientSecureConn != 0:
		if len(rest) < 1 || len(rest) < 1+int(rest[0]) {
			return r, false
		}
		rest = rest[1+int(rest[0]):]
	default:
		_, after, ok := cstring(rest)
		if !ok {
			return r, false
		}
		rest = after
	}
	if r.caps&myClientConnectWithDB != 0 {
		db, after, ok := cstring(rest)
		if !ok {
			return r, false
		}
		r.db, rest = db, after
	}
	if r.caps&myClientPluginAuth != 0 {
		p, after, ok := cstring(rest)
		if ok {
			r.plugin, rest = p, after
		}
	}
	r.attrs = rest
	return r, true
}

// build is the response with auth as its auth data, for plugin.
func (r myResponse) build(plugin string, auth []byte) []byte {
	caps := r.caps
	out := append([]byte{}, r.head...)
	out = append(append(out, r.user...), 0)
	switch {
	case caps&myClientPluginAuthLenEnc != 0:
		out = append(append(out, lenEnc(len(auth))...), auth...)
	case caps&myClientSecureConn != 0:
		out = append(append(out, byte(len(auth))), auth...)
	default:
		out = append(append(out, auth...), 0)
	}
	if caps&myClientConnectWithDB != 0 {
		out = append(append(out, r.db...), 0)
	}
	if caps&myClientPluginAuth != 0 {
		out = append(append(out, plugin...), 0)
	}
	return append(out, r.attrs...)
}

func (s *Server) mysqlSignIn(tc *tls.Conn, project string, e Engine, certHash, addr string) (net.Conn, net.Conn, error) {
	up, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, nil, err
	}
	ur := bufio.NewReader(up)
	cr := bufio.NewReader(tc)
	client, server := bufConn{cr, tc}, bufConn{ur, up}
	fail := func(err error) (net.Conn, net.Conn, error) {
		up.Close()
		return nil, nil, err
	}
	seq, greeting, err := myReadPacket(ur)
	if err != nil {
		return fail(err)
	}
	if _, err := tc.Write(myPacket(seq, greeting)); err != nil {
		return fail(err)
	}
	hs, okHS := parseMyHandshake(greeting)
	cseq, resp, err := myReadPacket(cr)
	if err != nil {
		return fail(err)
	}
	r, okResp := parseMyResponse(resp)
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	u, isIAM := User{}, false
	if okHS && okResp && len(resp) < myMaxHandshakeResponseLen {
		u, isIAM = iamUserNamed(ctx, e, r.user)
	}
	if !isIAM {
		if _, err := up.Write(myPacket(cseq, resp)); err != nil {
			return fail(err)
		}
		return client, server, nil
	}
	next := cseq + 1 // the client's next sequence number
	hash := certHash
	if hash == "" {
		// Manual IAM sign-in: the token is the password, sent in the
		// clear (the mysql client's --enable-cleartext-plugin), which
		// only the connector's TLS carries.
		sw := append([]byte{0xfe}, myClearPassword...)
		if _, err := tc.Write(myPacket(next, append(sw, 0))); err != nil {
			return fail(err)
		}
		pseq, pw, err := myReadPacket(cr)
		if err != nil {
			return fail(err)
		}
		next = pseq + 1
		hash = tokenHash(strings.TrimRight(string(pw), "\x00"))
	}
	host, _, _ := net.SplitHostPort(tc.RemoteAddr().String())
	login, err := s.checkIAMLogin(project, e, u, hash)
	if err != nil {
		_, _ = tc.Write(myPacket(next, myAccessDenied(fmt.Sprintf("Access denied for user '%s'@'%s' (using password: YES): %s", u.Name, host, err.Error()))))
		return fail(err)
	}
	plugin, nonce := hs.plugin, hs.nonce
	if plugin == "" {
		plugin = myNativePassword
	}
	sseq := cseq
	if _, err := up.Write(myPacket(sseq, r.build(plugin, myScramble(plugin, login.password, nonce)))); err != nil {
		return fail(err)
	}
	for {
		pseq, pkt, err := myReadPacket(ur)
		if err != nil {
			return fail(err)
		}
		sseq = pseq + 1
		if len(pkt) == 0 {
			return fail(errors.New("empty packet"))
		}
		switch pkt[0] {
		case 0x00: // OK
			if _, err := tc.Write(myPacket(next, pkt)); err != nil {
				return fail(err)
			}
			return client, server, nil
		case 0xff: // ERR
			_, _ = tc.Write(myPacket(next, pkt))
			return fail(errors.New("the server refused the sign-in"))
		case 0xfe: // AuthSwitchRequest
			p, data, ok := cstring(pkt[1:])
			if !ok {
				p, data = string(pkt[1:]), nil
			}
			plugin = p
			if len(data) >= 20 {
				nonce = append([]byte{}, data[:20]...)
			}
			if _, err := up.Write(myPacket(sseq, myScramble(plugin, login.password, nonce))); err != nil {
				return fail(err)
			}
		case 0x01: // AuthMoreData
			if len(pkt) == 2 && pkt[1] == 3 { // fast auth succeeded
				continue
			}
			if len(pkt) == 2 && pkt[1] == 4 { // full auth: by RSA
				if _, err := up.Write(myPacket(sseq, []byte{2})); err != nil {
					return fail(err)
				}
				kseq, key, err := myReadPacket(ur)
				if err != nil {
					return fail(err)
				}
				enc, err := myRSAPassword(key, login.password, nonce)
				if err != nil {
					return fail(err)
				}
				if _, err := up.Write(myPacket(kseq+1, enc)); err != nil {
					return fail(err)
				}
				continue
			}
			return fail(errors.New("unexpected AuthMoreData"))
		default:
			return fail(fmt.Errorf("unexpected packet 0x%02x in the sign-in", pkt[0]))
		}
	}
}

// myRSAPassword is caching_sha2_password's full authentication over a
// connection without TLS: the password, NUL-terminated and XORed with
// the nonce, encrypted with the server's RSA key (OAEP, SHA-1).
func myRSAPassword(keyPkt []byte, password string, nonce []byte) ([]byte, error) {
	if len(keyPkt) < 2 || keyPkt[0] != 1 {
		return nil, errors.New("the server sent no public key")
	}
	b, _ := pem.Decode(keyPkt[1:])
	if b == nil {
		return nil, errors.New("the server's public key is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the server's public key is not RSA")
	}
	plain := append([]byte(password), 0)
	for i := range plain {
		plain[i] ^= nonce[i%len(nonce)]
	}
	return rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, plain, nil) //nolint:gosec
}

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec
	return hex.EncodeToString(sum[:])
}
