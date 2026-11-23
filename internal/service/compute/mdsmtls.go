package compute

import (
	"bufio"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-tpm/legacy/tpm2"
	"github.com/google/go-tpm/tpmutil"
)

// The HTTPS metadata server (#2179), as Google documents it
// (https://docs.cloud.google.com/compute/docs/metadata/overview#https-mds,
// read 2026-11-22), for Shielded VMs:
//
//   - each instance has a root certificate of its own, self-signed and
//     valid for 50 years, made again whenever the instance starts (the
//     root "is rotated any time an instance starts and stops"), which the
//     guest reads from the UEFI variable InstanceRootCACertificate
//     (vendor GUID a2858e46-a37f-456a-8c79-0c1fe48b65ff): CloudBurrow's
//     guest firmware unit writes it there from GuestPrefix TOKEN/
//     cloudburrow/firmware/InstanceRootCACertificate;
//   - a server identity certificate under it for metadata.google.internal,
//     169.254.169.254 and fd20:ce::254;
//   - client identity certificates under it, valid for 7 days, at
//     instance/credentials/mds-client-certificate: Google's guest agent's
//     GuestCredentialsResponse, the certificate and its key encrypted
//     (AES-256-GCM) under a key sealed to the instance's vTPM endorsement
//     key (a go-tpm-tools ImportBlob, which only that vTPM can open). The
//     guest agent writes /run/google-mds-mtls/root.crt and client.key.
//
// The guest's relay carries port 443 of the metadata server's addresses to
// the Compute port, first naming the instance (mdsHello), and the Compute
// port completes the TLS handshake with that instance's certificates,
// requiring a client certificate its root issued, then serves the
// instance's metadata as on port 80.

// GoogleEFIGUID is the vendor GUID of the guest's Google UEFI variables.
const GoogleEFIGUID = "a2858e46-a37f-456a-8c79-0c1fe48b65ff"

// mdsHello begins a connection the guest's relay carries from port 443:
// a NUL (never the first byte of an HTTP request), "CBMDS ", the
// instance's guest token and a newline, then the guest's TLS bytes.
const mdsHello = "\x00CBMDS "

const (
	mdsRootLife   = 50 * 365 * 24 * time.Hour
	mdsClientLife = 7 * 24 * time.Hour
)

// mdsPKI is an instance's HTTPS metadata server certificates, kept in its
// _mds field (PEM).
type mdsPKI struct {
	RootKey    string `json:"rootKey"`
	RootCert   string `json:"rootCert"`
	ServerKey  string `json:"serverKey"`
	ServerCert string `json:"serverCert"`
}

func pemKey(k *ecdsa.PrivateKey) string {
	b, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b}))
}

func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func certSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// newMDSPKI makes an instance's root and server certificates.
func newMDSPKI(instanceID string, now time.Time) (mdsPKI, error) {
	rk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return mdsPKI{}, err
	}
	root := &x509.Certificate{SerialNumber: certSerial(),
		Subject:   pkix.Name{CommonName: "google.internal", Organization: []string{"Google Compute Internal"}, OrganizationalUnit: []string{instanceID}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(mdsRootLife), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rk.PublicKey, rk)
	if err != nil {
		return mdsPKI{}, err
	}
	rootCert, _ := x509.ParseCertificate(rootDER)
	sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return mdsPKI{}, err
	}
	srv := &x509.Certificate{SerialNumber: certSerial(), Subject: pkix.Name{CommonName: "metadata.google.internal"},
		DNSNames:    []string{"metadata.google.internal", "metadata"},
		IPAddresses: []net.IP{net.ParseIP("169.254.169.254"), net.ParseIP("fd20:ce::254")},
		NotBefore:   now.Add(-time.Hour), NotAfter: now.Add(mdsRootLife),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	srvDER, err := x509.CreateCertificate(rand.Reader, srv, rootCert, &sk.PublicKey, rk)
	if err != nil {
		return mdsPKI{}, err
	}
	return mdsPKI{RootKey: pemKey(rk), RootCert: pemCert(rootDER), ServerKey: pemKey(sk), ServerCert: pemCert(srvDER)}, nil
}

func pkiOf(o obj) (mdsPKI, bool) {
	m := objOf(o["_mds"])
	if m == nil {
		return mdsPKI{}, false
	}
	p := mdsPKI{RootKey: str(m, "rootKey"), RootCert: str(m, "rootCert"), ServerKey: str(m, "serverKey"), ServerCert: str(m, "serverCert")}
	return p, p.RootCert != ""
}

func (p mdsPKI) obj() obj {
	return obj{"rootKey": p.RootKey, "rootCert": p.RootCert, "serverKey": p.ServerKey, "serverCert": p.ServerCert}
}

func parseECKey(s string) (*ecdsa.PrivateKey, error) {
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return nil, errors.New("no PEM key")
	}
	return x509.ParseECPrivateKey(b.Bytes)
}

func parseCertPEM(s string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// clientCredentials is a new client identity certificate and its key, the
// certificate first, as the guest agent writes them to client.key.
func (p mdsPKI) clientCredentials(instanceID string, now time.Time) ([]byte, error) {
	rk, err := parseECKey(p.RootKey)
	if err != nil {
		return nil, err
	}
	root, err := parseCertPEM(p.RootCert)
	if err != nil {
		return nil, err
	}
	ck, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: certSerial(), Subject: pkix.Name{CommonName: instanceID},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(mdsClientLife),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root, &ck.PublicKey, rk)
	if err != nil {
		return nil, err
	}
	return []byte(pemCert(der) + pemKey(ck)), nil
}

func (p mdsPKI) tlsConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(p.ServerCert), []byte(p.ServerKey))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(p.RootCert)) {
		return nil, errors.New("bad root certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
		MinVersion: tls.VersionTLS12}, nil
}

// sealedCredentials is the GuestCredentialsResponse (protojson) for plain,
// encrypted under a fresh AES-256 key that is sealed to ekPub.
func sealedCredentials(ekPub crypto.PublicKey, plain []byte) ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// Tink's AES-GCM (the guest agent's): a 12-byte IV, then the
	// ciphertext and tag.
	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	enc := append(iv, gcm.Seal(nil, iv, plain, nil)...)
	blob, err := createImportBlob(ekPub, dek)
	if err != nil {
		return nil, err
	}
	b64 := base64.StdEncoding.EncodeToString
	return json.Marshal(map[string]any{"encryptedCredentials": b64(enc), "keyImportBlob": map[string]any{
		"duplicate": b64(blob.duplicate), "encryptedSeed": b64(blob.encryptedSeed), "publicArea": b64(blob.publicArea)}})
}

// importBlob is go-tpm-tools' proto/tpm ImportBlob without PCRs.
type importBlob struct {
	duplicate, encryptedSeed, publicArea []byte
}

// createImportBlob seals sensitive to the TPM whose endorsement key is
// ekPub, as go-tpm-tools' server.CreateImportBlob does (Apache-2.0,
// Copyright Google LLC; ported here without PCR policy): a keyed-hash
// object duplicated to the EK, which the guest imports, loads and unseals
// (client.Key.Import).
func createImportBlob(ekPub crypto.PublicKey, sensitive []byte) (importBlob, error) {
	ek, err := ekPublicArea(ekPub)
	if err != nil {
		return importBlob{}, err
	}
	nameAlg := tpm2.AlgSHA256
	seedValue := make([]byte, crypto.SHA256.Size())
	if _, err := io.ReadFull(rand.Reader, seedValue); err != nil {
		return importBlob{}, err
	}
	private := tpm2.Private{Type: tpm2.AlgKeyedHash, SeedValue: seedValue, Sensitive: sensitive}
	h := crypto.SHA256.New()
	h.Write(private.SeedValue)
	h.Write(private.Sensitive)
	public := tpm2.Public{Type: tpm2.AlgKeyedHash, NameAlg: nameAlg, Attributes: tpm2.FlagUserWithAuth,
		KeyedHashParameters: &tpm2.KeyedHashParams{Alg: tpm2.AlgNull, Unique: h.Sum(nil)}}
	var seed, encSeed []byte
	var symBits int
	switch ek.Type {
	case tpm2.AlgECC:
		symBits = int(ek.ECCParameters.Symmetric.KeyBits)
		curve := elliptic.P256()
		priv, x, y, err := elliptic.GenerateKey(curve, rand.Reader)
		if err != nil {
			return importBlob{}, err
		}
		pt := ek.ECCParameters.Point
		z, _ := curve.ScalarMult(pt.X(), pt.Y(), priv)
		seed, err = tpm2.KDFe(ek.NameAlg, eccBytes(curve, z), "DUPLICATE", eccBytes(curve, x), eccBytes(curve, pt.X()), crypto.SHA256.Size()*8)
		if err != nil {
			return importBlob{}, err
		}
		encSeed, err = tpmutil.Pack(tpmutil.U16Bytes(eccBytes(curve, x)), tpmutil.U16Bytes(eccBytes(curve, y)))
		if err != nil {
			return importBlob{}, err
		}
	default:
		return importBlob{}, fmt.Errorf("unsupported EK type %v", ek.Type)
	}
	name, err := public.Name()
	if err != nil {
		return importBlob{}, err
	}
	nameEnc, err := name.Digest.Encode()
	if err != nil {
		return importBlob{}, err
	}
	secret, err := private.Encode()
	if err != nil {
		return importBlob{}, err
	}
	packed, err := tpmutil.Pack(tpmutil.U16Bytes(secret))
	if err != nil {
		return importBlob{}, err
	}
	symKey, err := tpm2.KDFa(ek.NameAlg, seed, "STORAGE", nameEnc, nil, symBits)
	if err != nil {
		return importBlob{}, err
	}
	c, err := aes.NewCipher(symKey)
	if err != nil {
		return importBlob{}, err
	}
	encSecret := make([]byte, len(packed))
	cipher.NewCFBEncrypter(c, make([]byte, c.BlockSize())).XORKeyStream(encSecret, packed)
	macKey, err := tpm2.KDFa(ek.NameAlg, seed, "INTEGRITY", nil, nil, crypto.SHA256.Size()*8)
	if err != nil {
		return importBlob{}, err
	}
	mac := hmac.New(func() hash.Hash { return crypto.SHA256.New() }, macKey)
	mac.Write(encSecret)
	mac.Write(nameEnc)
	dup, err := tpmutil.Pack(tpm2.IDObject{IntegrityHMAC: mac.Sum(nil), EncIdentity: encSecret})
	if err != nil {
		return importBlob{}, err
	}
	pubEnc, err := public.Encode()
	if err != nil {
		return importBlob{}, err
	}
	return importBlob{duplicate: dup, encryptedSeed: encSeed, publicArea: pubEnc}, nil
}

func eccBytes(curve elliptic.Curve, i *big.Int) []byte {
	b := i.Bytes()
	n := (curve.Params().BitSize + 7) / 8
	return append(make([]byte, n-len(b)), b...)
}

// ekPublicArea is the default ECC EK template with ekPub's point.
func ekPublicArea(k crypto.PublicKey) (tpm2.Public, error) {
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return tpm2.Public{}, fmt.Errorf("the EK is not an ECC P-256 key")
	}
	p := ekTemplateECC()
	p.ECCParameters.Point = tpm2.ECPoint{XRaw: eccBytes(ec.Curve, ec.X), YRaw: eccBytes(ec.Curve, ec.Y)}
	return p, nil
}

// serveMDSCredentials answers instance/credentials/mds-client-certificate.
func (s *Server) serveMDSCredentials(w http.ResponseWriter, r *http.Request, rel string) {
	// A first boot's vTPM is provisioned while the guest boots: the
	// answer waits for it, as the guest's own vTPM is there from power on.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s.mu.Lock()
		inst, ok := s.res[rel]
		var pki mdsPKI
		var have, shielded bool
		var ek, id string
		if ok {
			pki, have = pkiOf(inst)
			shielded = vtpmOn(inst)
			ek = str(objOf(inst["_vtpm"]), "ekEccPub")
			id = str(inst, "id")
		}
		s.mu.Unlock()
		if !ok || !shielded || !have {
			guestErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if ek == "" {
			if time.Now().After(deadline) {
				guestErr(w, http.StatusServiceUnavailable, "The instance's vTPM is not ready.")
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		pub, err := parsePublicPEM(ek)
		if err != nil {
			guestErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		plain, err := pki.clientCredentials(id, s.opts.Now())
		if err != nil {
			guestErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		body, err := sealedCredentials(pub, plain)
		if err != nil {
			guestErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etagOf(string(body)))
		_, _ = w.Write(body)
		return
	}
}

func parsePublicPEM(s string) (crypto.PublicKey, error) {
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return nil, errors.New("no PEM public key")
	}
	return x509.ParsePKIXPublicKey(b.Bytes)
}

// mdsListener sorts the Compute port's connections: those the guests'
// relays bring from port 443 (mdsHello) are served over TLS as their
// instance's HTTPS metadata server; every other one goes to the API.
type mdsListener struct {
	net.Listener
	s     *Server
	conns chan net.Conn
	errc  chan error
	once  sync.Once
	https *http.Server
	tlsc  chan net.Conn
	done  chan struct{}
}

type mdsConnKey struct{}

// mdsConn is a guest's port 443 connection, its instance named.
type mdsConn struct {
	net.Conn
	r   *bufio.Reader
	rel string
}

func (c *mdsConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// peekedConn gives back what was read to sort it.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func newMDSListener(s *Server, ln net.Listener) *mdsListener {
	m := &mdsListener{Listener: ln, s: s, conns: make(chan net.Conn), errc: make(chan error, 1), tlsc: make(chan net.Conn), done: make(chan struct{})}
	m.https = &http.Server{ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rel, _ := r.Context().Value(mdsConnKey{}).(string)
			s.serveInstanceMetadata(w, r, rel, strings.TrimPrefix(r.URL.Path, "/"))
		}),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tls.Conn); ok {
				if mc, ok := tc.NetConn().(*mdsConn); ok {
					return context.WithValue(ctx, mdsConnKey{}, mc.rel)
				}
			}
			return ctx
		}}
	go m.loop()
	go func() { _ = m.https.Serve(&chanListener{c: m.tlsc, addr: ln.Addr(), done: m.done}) }()
	return m
}

func (m *mdsListener) loop() {
	for {
		c, err := m.Listener.Accept()
		if err != nil {
			m.errc <- err
			return
		}
		go m.sort(c)
	}
}

func (m *mdsListener) sort(c net.Conn) {
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	b, err := br.Peek(1)
	if err != nil {
		_ = c.Close()
		return
	}
	if b[0] != 0 {
		_ = c.SetReadDeadline(time.Time{})
		select {
		case m.conns <- &peekedConn{Conn: c, r: br}:
		case <-m.done:
			_ = c.Close()
		}
		return
	}
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, mdsHello) {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	token := strings.TrimSpace(strings.TrimPrefix(line, mdsHello))
	rel, cfg := m.s.mdsTLSFor(token)
	if cfg == nil {
		// Not a Shielded VM with a vTPM: no HTTPS endpoint.
		_ = c.Close()
		return
	}
	tc := tls.Server(&mdsConn{Conn: c, r: br, rel: rel}, cfg)
	select {
	case m.tlsc <- tc:
	case <-m.done:
		_ = c.Close()
	}
}

func (m *mdsListener) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case err := <-m.errc:
		m.errc <- err
		return nil, err
	}
}

func (m *mdsListener) Close() error {
	m.once.Do(func() { close(m.done); _ = m.https.Close() })
	return m.Listener.Close()
}

type chanListener struct {
	c    chan net.Conn
	addr net.Addr
	done chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.c:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// mdsTLSFor is the TLS configuration of the instance whose guest token is
// token, nil when it has no HTTPS metadata server.
func (s *Server) mdsTLSFor(token string) (string, *tls.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		return "", nil
	}
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") || str(o, "_guestToken") != token {
			continue
		}
		p, ok := pkiOf(o)
		if !ok || !vtpmOn(o) {
			return "", nil
		}
		cfg, err := p.tlsConfig()
		if err != nil {
			return "", nil
		}
		return k, cfg
	}
	return "", nil
}

// rotateMDSRootLocked makes the instance's HTTPS metadata server root and
// server certificates again: at every start.
func (s *Server) rotateMDSRootLocked(o obj) error {
	if !vtpmOn(o) {
		delete(o, "_mds")
		return nil
	}
	p, err := newMDSPKI(str(o, "id"), s.opts.Now())
	if err != nil {
		return err
	}
	o["_mds"] = p.obj()
	return nil
}

// serveFirmware answers the guest firmware unit's read of a Google UEFI
// variable (GuestPrefix TOKEN/cloudburrow/firmware/NAME): the instance's
// root certificate, InstanceRootCACertificate. It waits, as
// serveMDSCredentials does, for a first boot's vTPM to be provisioned:
// on Compute Engine the firmware and the vTPM are there from power on, so
// Google's guest agent, which starts after the firmware unit and asks for
// its credentials once, finds the credentials endpoint ready.
func (s *Server) serveFirmware(w http.ResponseWriter, r *http.Request, rel, name string) {
	if name != "InstanceRootCACertificate" {
		http.NotFound(w, nil)
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s.mu.Lock()
		inst, ok := s.res[rel]
		var p mdsPKI
		var have bool
		var ek string
		if ok {
			p, have = pkiOf(inst)
			ek = str(objOf(inst["_vtpm"]), "ekEccPub")
		}
		s.mu.Unlock()
		if !have {
			http.NotFound(w, nil)
			return
		}
		if ek != "" || time.Now().After(deadline) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			_, _ = w.Write([]byte(p.RootCert))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}
