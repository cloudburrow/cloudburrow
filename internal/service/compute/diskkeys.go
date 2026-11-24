package compute

// Customer-supplied encryption keys (CSEK) on disks and snapshots (#2206),
// as https://docs.cloud.google.com/compute/docs/disks/customer-supplied-encryption
// documents them (read 2026-11-24; Google marks CSEK deprecated in favour
// of CMEK, and still serves it):
//
//   - diskEncryptionKey.rawKey is "a 256-bit string encoded in RFC 4648
//     standard base64"; rsaEncryptedKey is that key RSA-wrapped (OAEP,
//     SHA-1, as the page's example does) with the ingress certificate:
//     Google's is its own, so CloudBurrow serves one of its own at
//     CSEKCertPath, made once per instance and kept in the state file;
//   - the disk answers only diskEncryptionKey.sha256, "the RFC 4648 base64
//     encoded SHA-256 hash of the customer-supplied encryption key", and
//     the key protects the disk's data key (wrapped with AES-256-GCM), as
//     Google's does: "Compute Engine uses your key to protect the
//     Google-generated keys used to encrypt and decrypt your data";
//   - every use names the key again: attaching the disk (the attached
//     disk's diskEncryptionKey), starting its instance
//     (instances.startWithEncryptionKey), a snapshot
//     (sourceDiskEncryptionKey) and a disk from a CSEK snapshot
//     (sourceSnapshotEncryptionKey); a snapshot without
//     snapshotEncryptionKey is encrypted with a key CloudBurrow keeps, as
//     Google's then is ("used without requiring a key");
//   - "You can't suspend instances that have CSEK-protected disks
//     attached", and updateKmsKey refuses a CSEK disk
//     (KMS_UPDATE_NOT_SUPPORTED_FOR_CSEK_RESOURCE).

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"time"
)

// CSEKCertPath is where the Compute port serves the certificate
// rsaEncryptedKey is wrapped with: CloudBurrow's own, in place of
// https://cloud-certs.storage.googleapis.com/google-cloud-csek-ingress.pem.
const CSEKCertPath = "/cloudburrow/compute/csek-ingress.pem"

const (
	// csekKey marks a disk or snapshot whose data key the customer's key
	// wraps.
	csekKey = "_csek"
	// googleKeyKey is the data key of a snapshot of a CSEK disk made
	// without snapshotEncryptionKey: the key the service keeps.
	googleKeyKey = "_googleKey"
)

func isCSEK(o obj) bool { b, _ := o[csekKey].(bool); return b }

func csekSHA256(k []byte) string {
	sum := sha256.Sum256(k)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// rememberKeyLocked keeps an unwrapped data key in memory, by its wrapped
// form, for sealing the disk again later.
func (s *Server) rememberKeyLocked(wrapped string, dek []byte) {
	if s.openKeys == nil {
		s.openKeys = map[string][]byte{}
	}
	s.openKeys[wrapped] = dek
}

// specKeyLocked is the data key a DiskSpec carries for o: the remembered
// one, the service's own (googleKeyKey), or a Cloud KMS one unwrapped
// now; nil when o has none or it can't be unwrapped (every use that needs
// it checks the key first, and answers that error).
func (s *Server) specKeyLocked(o obj) []byte {
	if gk := str(o, googleKeyKey); gk != "" {
		k, _ := base64.StdEncoding.DecodeString(gk)
		return k
	}
	w := str(o, wrappedKeyKey)
	if w == "" {
		return nil
	}
	if k, ok := s.openKeys[w]; ok {
		return k
	}
	if isCSEK(o) {
		return nil
	}
	k, _ := s.diskKeyLocked(o, nil, "")
	return k
}

// customerKeyLocked decodes a CustomerEncryptionKey's rawKey or
// rsaEncryptedKey into its 32 bytes.
func (s *Server) customerKeyLocked(ek map[string]any, field string) ([]byte, error) {
	if raw := str(ek, "rawKey"); raw != "" {
		k, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(k) != 32 {
			return nil, invalid("Invalid value for field '%s.rawKey': the key must be a 256-bit key encoded in RFC 4648 standard base64.", field)
		}
		return k, nil
	}
	if wr := str(ek, "rsaEncryptedKey"); wr != "" {
		ct, err := base64.StdEncoding.DecodeString(wr)
		if err != nil {
			return nil, invalid("Invalid value for field '%s.rsaEncryptedKey': the key must be encoded in RFC 4648 standard base64.", field)
		}
		priv, err := s.csekIngressLocked()
		if err != nil {
			return nil, err
		}
		k, err := rsa.DecryptOAEP(sha1.New(), nil, priv, ct, nil)
		if err != nil || len(k) != 32 {
			return nil, invalid("Invalid value for field '%s.rsaEncryptedKey': the key must be a 256-bit key wrapped with RSA-OAEP by the certificate at %s.", field, CSEKCertPath)
		}
		return k, nil
	}
	return nil, nil
}

// wrapCustomer wraps a data key with the customer's key (AES-256-GCM).
func wrapCustomer(ck, dek []byte) (string, error) {
	b, err := aes.NewCipher(ck)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, dek, nil)), nil
}

func unwrapCustomer(ck []byte, w string) ([]byte, error) {
	ct, err := base64.StdEncoding.DecodeString(w)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(ck)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	if len(ct) < g.NonceSize() {
		return nil, ErrSealed
	}
	return g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], nil)
}

// openCustomerLocked unwraps a CSEK resource's data key with the key the
// request gave.
func (s *Server) openCustomerLocked(o obj, ck map[string]any, field string) ([]byte, error) {
	rel := str(o, "selfLink")
	k, err := s.customerKeyLocked(ck, field)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, errf(http.StatusBadRequest, "resourceIsEncryptedWithCustomerEncryptionKey",
			"The resource '%s' is encrypted with a customer-supplied encryption key, and the key was not provided.", rel)
	}
	ek, _ := o["diskEncryptionKey"].(map[string]any)
	if ek == nil {
		ek, _ = o["snapshotEncryptionKey"].(map[string]any)
	}
	if subtle.ConstantTimeCompare([]byte(csekSHA256(k)), []byte(str(ek, "sha256"))) != 1 {
		return nil, errf(http.StatusBadRequest, "customerEncryptionKeyIsIncorrect",
			"The customer-supplied encryption key provided for '%s' is incorrect.", rel)
	}
	w := str(o, wrappedKeyKey)
	dek, err := unwrapCustomer(k, w)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "customerEncryptionKeyIsIncorrect",
			"The customer-supplied encryption key provided for '%s' is incorrect.", rel)
	}
	s.rememberKeyLocked(w, dek)
	return dek, nil
}

// csekIngressLocked is the instance's RSA key for rsaEncryptedKey, made
// on first use and kept in the state file.
func (s *Server) csekIngressLocked() (*rsa.PrivateKey, error) {
	if s.csekPriv != nil {
		return s.csekPriv, nil
	}
	if s.csekPEM != "" {
		if blk, _ := pem.Decode([]byte(s.csekPEM)); blk != nil {
			if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
				if rk, ok := k.(*rsa.PrivateKey); ok {
					s.csekPriv = rk
					return rk, nil
				}
			}
		}
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	s.csekPriv = k
	s.csekPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return k, nil
}

// serveCSEKCert answers CSEKCertPath: the ingress key's self-signed
// certificate, PEM.
func (s *Server) serveCSEKCert(w http.ResponseWriter) {
	s.mu.Lock()
	k, err := s.csekIngressLocked()
	if err == nil {
		err = s.saveLocked()
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CloudBurrow CSEK ingress"},
		NotBefore: time.Unix(0, 0).UTC(), NotAfter: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage: x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// snapshotKeyLocked gives a new snapshot of d its key: a CMEK disk's
// snapshot shares its wrapped data key ("encrypted with the same key"); a
// CSEK disk's is wrapped by snapshotEncryptionKey's customer key when b
// gives one, and kept by the service otherwise. dek is d's data key.
func (s *Server) snapshotKeyLocked(snap, d, b obj, dek []byte) error {
	if dek == nil {
		return nil
	}
	if !isCSEK(d) {
		snap["snapshotEncryptionKey"] = d["diskEncryptionKey"]
		snap[wrappedKeyKey] = d[wrappedKeyKey]
		return nil
	}
	sk, _ := b["snapshotEncryptionKey"].(map[string]any)
	ck, err := s.customerKeyLocked(sk, "resource.snapshotEncryptionKey")
	if err != nil {
		return err
	}
	if ck == nil {
		snap[googleKeyKey] = base64.StdEncoding.EncodeToString(dek)
		return nil
	}
	w, err := wrapCustomer(ck, dek)
	if err != nil {
		return err
	}
	snap["snapshotEncryptionKey"] = obj{"sha256": csekSHA256(ck)}
	snap[wrappedKeyKey] = w
	snap[csekKey] = true
	return nil
}
