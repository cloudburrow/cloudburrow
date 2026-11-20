package sqladmin

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIAMDBName (#1944): the database user Google names for an IAM
// principal, on PostgreSQL and MySQL.
func TestIAMDBName(t *testing.T) {
	for _, c := range []struct {
		typ, email string
		mysql      bool
		want       string
		ok         bool
	}{
		{TypeIAMUser, "Alice@Example.com", false, "alice@example.com", true},
		{TypeIAMUser, "alice@example.com", true, "alice", true},
		{TypeIAMServiceAccount, "sa@p.iam.gserviceaccount.com", false, "sa@p.iam", true},
		{TypeIAMServiceAccount, "sa@p.iam.gserviceaccount.com", true, "sa", true},
		{TypeIAMServiceAccount, "alice@example.com", false, "", false},
		{TypeIAMUser, "sa@p.iam.gserviceaccount.com", false, "", false},
		{TypeIAMUser, "not an email", false, "", false},
		{TypeIAMUser, strings.Repeat("a", 33) + "@example.com", true, "", false},
	} {
		got, err := iamDBName(c.typ, c.email, c.mysql)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("iamDBName(%s, %s, mysql=%v) = %q, %v; want %q", c.typ, c.email, c.mysql, got, err, c.want)
		}
	}
}

// TestSCRAMClientMatchesRFC7677: the client proof of RFC 7677's example.
func TestSCRAMClientMatchesRFC7677(t *testing.T) {
	c := &scramClient{password: "pencil", cnonce: "rOprNGfwEbeRWgbNEkqO", firstBare: "n=user,r=rOprNGfwEbeRWgbNEkqO"}
	got, err := c.final("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if got != want {
		t.Errorf("client-final = %s, want %s", got, want)
	}
}

// TestMySQLResponseRoundTrip: a HandshakeResponse41 rewritten with other
// auth data keeps its user, database, plugin and attributes.
func TestMySQLResponseRoundTrip(t *testing.T) {
	r := myResponse{caps: myClientProtocol41 | myClientSecureConn | myClientPluginAuth | myClientPluginAuthLenEnc | myClientConnectWithDB,
		head: make([]byte, 32), user: "sa", db: "app", attrs: []byte{3, 'a', 'b', 'c'}}
	r.head[0], r.head[1], r.head[2], r.head[3] = byte(r.caps), byte(r.caps>>8), byte(r.caps>>16), byte(r.caps>>24)
	b := r.build(myCachingSHA2Password, []byte("12345"))
	got, ok := parseMyResponse(b)
	if !ok || got.user != "sa" || got.db != "app" || got.plugin != myCachingSHA2Password || string(got.attrs) != string(r.attrs) {
		t.Errorf("round trip: %+v, %v", got, ok)
	}
}

// TestEphemeralCertCarriesTheToken (#1944): generateEphemeralCert with an
// access token this instance minted signs a certificate carrying its
// SHA-256 and expiring with it.
func TestEphemeralCertCarriesTheToken(t *testing.T) {
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(20 * time.Minute)
	srv.UseIAM(IAMSource{
		Token: func(hash string, _ time.Time) (string, time.Time, bool) {
			if hash == tokenHash("minted") {
				return "sa@p.iam.gserviceaccount.com", exp, true
			}
			return "", time.Time{}, false
		},
		Allowed: func(string, string, string, string) bool { return true },
	})
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pk := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	body, _ := json.Marshal(map[string]string{"public_key": pk, "access_token": "minted"})
	req, _ := http.NewRequest(http.MethodPost, "/v1/projects/p/instances/pg:generateEphemeralCert", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generateEphemeralCert: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		EphemeralCert struct{ Cert string } `json:"ephemeralCert"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	b, _ := pem.Decode([]byte(out.EphemeralCert.Cert))
	if b == nil {
		t.Fatal("no certificate")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != tokenURNPrefix+tokenHash("minted") {
		t.Errorf("URIs = %v", cert.URIs)
	}
	if cert.NotAfter.After(exp) {
		t.Errorf("NotAfter %v is after the token's expiry %v", cert.NotAfter, exp)
	}
}
