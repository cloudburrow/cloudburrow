package compute

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A Shielded VM (#2179) defaults to a vTPM and integrity monitoring; its
// HTTPS metadata server's credentials are sealed to its EK, and the
// Compute port serves its metadata over TLS only to a client certificate
// of its own root; guest attributes are rate limited.
func TestShieldedVMMetadataServer2179(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}, Tokens: &fakeGuestTokens{}, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(t.Context()) })
	z := "zones/us-central1-a/"
	if _, err := vmCall(t, s, "POST", z+"instances", obj{"name": "vm1", "machineType": z + "machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}},
		"metadata":          obj{"items": []any{obj{"key": "enable-guest-attributes", "value": "TRUE"}}}}); err != nil {
		t.Fatal(err)
	}
	rel := "projects/p/" + z + "instances/vm1"
	s.mu.Lock()
	inst := s.res[rel]
	if sc := objOf(inst["shieldedInstanceConfig"]); sc["enableVtpm"] != true || sc["enableIntegrityMonitoring"] != true || sc["enableSecureBoot"] != false {
		t.Errorf("shieldedInstanceConfig: %v", sc)
	}
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ekPEM, _ := pemPublic(&ek.PublicKey)
	inst["_vtpm"] = obj{"ekEccPub": ekPEM}
	token := str(inst, "_guestToken")
	pki, _ := pkiOf(inst)
	s.mu.Unlock()

	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", GuestPrefix+token+"/"+path, nil)
		r.Header.Set("Metadata-Flavor", "Google")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := get("cloudburrow/firmware/InstanceRootCACertificate"); w.Code != 200 || w.Body.String() != pki.RootCert {
		t.Errorf("the root certificate: %d %s", w.Code, w.Body)
	}
	w := get("computeMetadata/v1/instance/credentials/mds-client-certificate")
	var resp struct {
		EncryptedCredentials string `json:"encryptedCredentials"`
		KeyImportBlob        struct {
			Duplicate, EncryptedSeed, PublicArea string
		} `json:"keyImportBlob"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.EncryptedCredentials == "" || resp.KeyImportBlob.Duplicate == "" ||
		resp.KeyImportBlob.EncryptedSeed == "" || resp.KeyImportBlob.PublicArea == "" {
		t.Fatalf("credentials: %d %s %v", w.Code, w.Body, err)
	}
	if b, _ := base64.StdEncoding.DecodeString(resp.EncryptedCredentials); len(b) < 12+16+100 {
		t.Errorf("encrypted credentials of %d bytes", len(b))
	}

	// TLS on the Compute port, after the relay's line.
	plain, err := pki.clientCredentials("1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(plain, plain)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(pki.RootCert))
	dial := func(certs []tls.Certificate) (string, error) {
		c, err := net.Dial("tcp", s.Addr())
		if err != nil {
			return "", err
		}
		defer c.Close()
		_, _ = io.WriteString(c, mdsHello+token+"\n")
		tc := tls.Client(c, &tls.Config{RootCAs: roots, ServerName: "metadata.google.internal", Certificates: certs})
		req, _ := http.NewRequest("GET", "https://metadata.google.internal/computeMetadata/v1/instance/name", nil)
		req.Header.Set("Metadata-Flavor", "Google")
		if err := req.Write(tc); err != nil {
			return "", err
		}
		res, err := http.ReadResponse(bufio.NewReader(tc), req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.Status + " " + string(b), nil
	}
	if out, err := dial([]tls.Certificate{cert}); err != nil || !strings.HasPrefix(out, "200") || !strings.Contains(out, "vm1") {
		t.Errorf("HTTPS with the client certificate: %q %v", out, err)
	}
	if out, err := dial(nil); err == nil {
		t.Errorf("HTTPS without a client certificate: %q", out)
	}
	// The API still answers on the same port.
	if res, err := http.Get("http://" + s.Addr() + "/compute/v1/projects/p/" + z + "instances/vm1"); err != nil || res.StatusCode != 200 {
		t.Errorf("the API beside it: %v %v", res, err)
	}

	// Guest attributes: three writes a second.
	put := func(k string) int {
		r := httptest.NewRequest("PUT", GuestPrefix+token+"/computeMetadata/v1/instance/guest-attributes/app/"+k, strings.NewReader("v"))
		r.Header.Set("Metadata-Flavor", "Google")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	var codes []int
	for _, k := range []string{"a", "b", "c", "d"} {
		codes = append(codes, put(k))
	}
	if codes[0] != 200 || codes[2] != 200 || codes[3] != http.StatusTooManyRequests {
		t.Errorf("a burst of guest attribute writes: %v", codes)
	}

	// The token carries the instance's scopes.
	if _, err := vmCall(t, s, "GET", z+"instances/vm1/getShieldedInstanceIdentity", nil); err != nil {
		t.Errorf("getShieldedInstanceIdentity: %v", err)
	}
	if _, err := s.Call("PATCH", "p", z+"instances/vm1/updateShieldedInstanceConfig", nil, obj{"enableSecureBoot": true}); err == nil {
		t.Errorf("updateShieldedInstanceConfig on a running VM succeeded")
	}
}
