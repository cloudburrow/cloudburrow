package storage

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signed URLs (#509), verified and failing closed, to
// docs.cloud.google.com/storage/docs/access-control/signed-urls and
// .../authentication/signatures and canonical-requests. A request to the
// XML API that carries a signature is served only when the signature
// checks out against a key CloudBurrow holds:
//
//   - V4 GOOG4-HMAC-SHA256 (and AWS4-HMAC-SHA256 with X-Amz-* parameters)
//     against an ACTIVE HMAC key (#505);
//   - V4 GOOG4-RSA-SHA256, and V2 (GoogleAccessId, Expires, Signature),
//     against a public key the user registered for that service account
//     (Options.SigningKeys; storage-server --signing-cert).
//
// Anything else (a bad signature, an unknown key, an expired URL) is 403
// with the XML API's error, never 200. The canonical request is the verb,
// the percent-encoded path, the query sorted by code point without the
// signature, the signed headers (host required) lowercased and sorted, and
// UNSIGNED-PAYLOAD unless x-goog-content-sha256 is signed. X-Goog-Expires
// is at most 604800 seconds, and expiry is judged on the server's clock.
//
// The host a client signs may carry the port or not (the Go client signs
// the host name, the Python client host:port), so both are tried.

const maxSignedURLExpiry = 604800

// ParseSigningKey reads a PEM certificate, PKIX public key or PKCS#1 RSA
// public key, as registered for a service account's signed URLs.
func ParseSigningKey(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	switch block.Type {
	case "CERTIFICATE":
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if k, ok := c.PublicKey.(*rsa.PublicKey); ok {
			return k, nil
		}
		return nil, errors.New("the certificate's key is not RSA")
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		if rk, ok := k.(*rsa.PublicKey); ok {
			return rk, nil
		}
		return nil, errors.New("the key is not RSA")
	case "RSA PUBLIC KEY":
		return x509.ParsePKCS1PublicKey(block.Bytes)
	}
	return nil, fmt.Errorf("a PEM %q block is not a certificate or public key", block.Type)
}

// isSigned reports whether a request carries a signed URL's parameters.
func isSigned(q url.Values) bool {
	return q.Has("X-Goog-Signature") || q.Has("X-Amz-Signature") || q.Has("Signature") && q.Has("GoogleAccessId")
}

// signatureError is a failed verification, with its XML API code.
type signatureError struct {
	status int
	code   string
	msg    string
}

func (e *signatureError) Error() string { return e.msg }

func denied(msg string, a ...any) error {
	return &signatureError{http.StatusForbidden, "AccessDenied", fmt.Sprintf(msg, a...)}
}

func mismatch() error {
	return &signatureError{http.StatusForbidden, "SignatureDoesNotMatch",
		"The request signature we calculated does not match the signature you provided."}
}

// verifySignedURL checks a signed request; nil means serve it.
func (s *Server) verifySignedURL(r *http.Request) error {
	q := r.URL.Query()
	if q.Has("X-Goog-Signature") || q.Has("X-Amz-Signature") {
		return s.verifyV4(r, q)
	}
	bucket, object := s.xmlTarget(r)
	return s.verifyV2(r, q, "/"+bucket+"/"+object)
}

func (s *Server) verifyV4(r *http.Request, q url.Values) error {
	p := "X-Goog-"
	if q.Has("X-Amz-Signature") {
		p = "X-Amz-"
	}
	alg, cred, date := q.Get(p+"Algorithm"), q.Get(p+"Credential"), q.Get(p+"Date")
	expires, signed, sig := q.Get(p+"Expires"), q.Get(p+"SignedHeaders"), q.Get(p+"Signature")
	parts := strings.Split(cred, "/")
	if alg == "" || len(parts) != 5 || date == "" || expires == "" || signed == "" || sig == "" {
		return &signatureError{http.StatusBadRequest, "InvalidArgument", "The signed URL is missing a required " + p + "* parameter."}
	}
	accessID, scopeDate, region, service, kind := parts[0], parts[1], parts[2], parts[3], parts[4]
	t, err := time.Parse("20060102T150405Z", date)
	if err != nil || !strings.HasPrefix(date, scopeDate) {
		return &signatureError{http.StatusBadRequest, "InvalidArgument", "The signed URL's " + p + "Date is not YYYYMMDD'T'HHMMSS'Z' in the credential's day."}
	}
	secs, err := strconv.Atoi(expires)
	if err != nil || secs < 1 || secs > maxSignedURLExpiry {
		// UNVERIFIED: the docs state the 604800-second maximum, not the code.
		return &signatureError{http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("%sExpires must be 1 to %d seconds.", p, maxSignedURLExpiry)}
	}
	if now := s.now(); !now.Before(t.Add(time.Duration(secs) * time.Second)) {
		return denied("Request has expired: %s", date)
	}
	names := strings.Split(signed, ";")
	if !contains(names, "host") {
		return denied("The signed headers must include host.")
	}
	scope := strings.Join(parts[1:], "/")
	for _, host := range hostForms(r.Host) {
		sts := alg + "\n" + date + "\n" + scope + "\n" + hexSHA256(canonicalRequestV4(r, q, p, names, host))
		ok, err := s.checkV4(alg, accessID, sts, sig, scopeDate, region, service, kind)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return mismatch()
}

// checkV4 verifies one string-to-sign; an unknown key is an error.
func (s *Server) checkV4(alg, accessID, sts, sig, date, region, service, kind string) (bool, error) {
	switch alg {
	case "GOOG4-HMAC-SHA256", "AWS4-HMAC-SHA256":
		secret, err := s.activeHMACSecret(accessID)
		if err != nil {
			return false, err
		}
		prefix := "GOOG4"
		if alg == "AWS4-HMAC-SHA256" {
			prefix = "AWS4"
		}
		k := hmacSHA256([]byte(prefix+secret), date)
		for _, v := range []string{region, service, kind} {
			k = hmacSHA256(k, v)
		}
		return hmac.Equal([]byte(hex.EncodeToString(hmacSHA256(k, sts))), []byte(strings.ToLower(sig))), nil
	case "GOOG4-RSA-SHA256":
		key, ok := s.signingKeys[accessID]
		if !ok {
			return false, denied("No signing certificate is registered for %s; register one (storage-server --signing-cert) to verify its signatures.", accessID)
		}
		raw, err := hex.DecodeString(sig)
		if err != nil {
			return false, nil
		}
		sum := sha256.Sum256([]byte(sts))
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], raw) == nil, nil
	}
	return false, &signatureError{http.StatusBadRequest, "InvalidArgument", "Unknown signing algorithm " + alg}
}

func (s *Server) activeHMACSecret(accessID string) (string, error) {
	secret := ""
	_ = s.meta.View(func(tx Tx) error {
		for _, key := range tx.List(hmacPrefix) {
			if !strings.HasSuffix(key, "/"+accessID) {
				continue
			}
			raw, _ := tx.Get(key)
			var k hmacKey
			if json.Unmarshal(raw, &k) == nil && k.AccessID == accessID && k.State == "ACTIVE" {
				secret = k.Secret
			}
		}
		return nil
	})
	if secret == "" {
		return "", denied("The HMAC key %s does not exist or is not ACTIVE.", accessID)
	}
	return secret, nil
}

// canonicalRequestV4 is the canonical request for one form of the host.
func canonicalRequestV4(r *http.Request, q url.Values, p string, names []string, host string) string {
	cq := url.Values{}
	for k, v := range q {
		if k != p+"Signature" {
			cq[k] = v
		}
	}
	query := strings.ReplaceAll(cq.Encode(), "+", "%20")
	var headers []string
	payload := "UNSIGNED-PAYLOAD"
	for _, n := range names {
		v := host
		if n != "host" {
			v = strings.Join(r.Header.Values(n), ",")
		}
		v = strings.Join(strings.Fields(v), " ")
		headers = append(headers, n+":"+v)
		if n == "x-goog-content-sha256" || n == "x-amz-content-sha256" {
			payload = v
		}
	}
	sort.Strings(headers)
	return r.Method + "\n" + encodePathV4(r.URL.Path) + "\n" + query + "\n" + strings.Join(headers, "\n") + "\n\n" +
		strings.Join(names, ";") + "\n" + payload
}

// encodePathV4 percent-encodes each path segment, keeping "/", as the
// canonical-requests docs define (and the Go client's pathEncodeV4 does).
func encodePathV4(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	return strings.Join(segs, "/")
}

// hostForms are the host as sent, and without its port.
func hostForms(h string) []string {
	if name, _, err := net.SplitHostPort(h); err == nil && name != h {
		return []string{h, name}
	}
	return []string{h}
}

// verifyV2 checks a V2 signature. resource is /{bucket}/{object}, which V2
// signs whatever the host style.
func (s *Server) verifyV2(r *http.Request, q url.Values, resource string) error {
	id, exp, sig := q.Get("GoogleAccessId"), q.Get("Expires"), q.Get("Signature")
	e, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return &signatureError{http.StatusBadRequest, "InvalidArgument", "Expires must be a Unix time."}
	}
	if !s.now().Before(time.Unix(e, 0)) {
		return denied("Request has expired: %s", exp)
	}
	key, ok := s.signingKeys[id]
	if !ok {
		return denied("No signing certificate is registered for %s; register one (storage-server --signing-cert) to verify its signatures.", id)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return mismatch()
	}
	var ext []string
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-goog-") && len(v) > 0 {
			ext = append(ext, lk+":"+strings.Join(v, ","))
		}
	}
	sort.Strings(ext)
	sts := r.Method + "\n" + r.Header.Get("Content-MD5") + "\n" + r.Header.Get("Content-Type") + "\n" + exp + "\n"
	if len(ext) > 0 {
		sts += strings.Join(ext, "\n") + "\n"
	}
	sts += (&url.URL{Path: resource}).String()
	sum := sha256.Sum256([]byte(sts))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], raw) != nil {
		return mismatch()
	}
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
