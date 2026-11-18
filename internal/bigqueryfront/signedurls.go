package bigqueryfront

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EXTERNAL_OBJECT_TRANSFORM(TABLE object_table, ['SIGNED_URL']) (#1786),
// as Google documents it
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/table-functions-built-in,
// read 2026-11-18): "a transformed object table with the original
// columns plus" signed_url, "read-only signed URLs for the objects" that
// are "valid for 6 hours".
//
// Each URL is a V4 signed URL
// (https://docs.cloud.google.com/storage/docs/access-control/signed-urls):
// GET, X-Goog-Algorithm GOOG4-RSA-SHA256, signed as the object table's
// connection's service account (bqcx-…@gcp-sa-bigquery-condel), with
// X-Goog-Expires 21600 and host the only signed header. The key is that
// service agent's Google-managed key, which on Google BigQuery holds and
// Cloud Storage verifies against the account's public certificates
// (www.googleapis.com/service_accounts/v1/metadata/x509/EMAIL). Here the
// front holds one RSA key per service agent (kept in its state
// directory), and serves the agents' certificates at that same path; the
// instance's Cloud Storage reads them there to verify the URL
// (storage-server --service-account-certs) and refuses it otherwise.
//
// The URL's host is the address clients reach Cloud Storage at (the
// front's --storage-public, which `up` sets to STORAGE_EMULATOR_HOST's
// address), path-style, as Google's are on storage.googleapis.com.
//
// UNVERIFIED (docs re-read 2026-11-18, the pages above): the region in
// the credential scope ("auto", as Google's own V4 signers write), the
// column's place (last), and that a row whose object is gone still gets
// a URL (it is 404 when fetched).

const (
	signedURLExpiry  = 6 * time.Hour
	serviceAgentKeys = "serviceagentkeys.json"
	// agentCertPrefix is Google's public certificate path, as Cloud
	// Storage reads it.
	agentCertPrefix = "/service_accounts/v1/metadata/x509/"
	agentDomain     = "@gcp-sa-bigquery-condel.iam.gserviceaccount.com"
)

// agentKey is one service agent's key.
type agentKey struct {
	ID      string `json:"id"`
	Private string `json:"private"` // PKCS#1 PEM
	Cert    string `json:"cert"`    // X.509 PEM
	key     *rsa.PrivateKey
}

// agentKeys are the connections' service agents' keys, by email.
type agentKeys struct {
	mu   sync.Mutex
	m    map[string]*agentKey
	path string
	logf func(string, ...any)
}

func (a *agentKeys) keep(dir string, logf func(string, ...any)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.path, a.logf = filepath.Join(dir, serviceAgentKeys), logf
	raw, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	var m map[string]*agentKey
	if json.Unmarshal(raw, &m) != nil {
		logf("bigquery front: %s is not readable; service agents get new keys", a.path)
		return
	}
	for email, k := range m {
		if b, _ := pem.Decode([]byte(k.Private)); b != nil {
			if pk, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
				k.key = pk
				if a.m == nil {
					a.m = map[string]*agentKey{}
				}
				a.m[email] = k
			}
		}
	}
}

// get is email's key, made the first time it is asked for.
func (a *agentKeys) get(email string) (*agentKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k, ok := a.m[email]; ok {
		return k, nil
	}
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 20)
	_, _ = rand.Read(id)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: email},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &pk.PublicKey, pk)
	if err != nil {
		return nil, err
	}
	k := &agentKey{ID: hex.EncodeToString(id), key: pk,
		Private: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(pk)})),
		Cert:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	if a.m == nil {
		a.m = map[string]*agentKey{}
	}
	a.m[email] = k
	if a.path != "" {
		raw, _ := json.Marshal(a.m)
		if err := writeFileAtomic(a.path, raw); err != nil && a.logf != nil {
			a.logf("bigquery front: keep the service agents' keys: %v", err)
		}
	}
	return k, nil
}

// serveCerts answers GET /service_accounts/v1/metadata/x509/EMAIL for a
// service agent the front holds a key of: {key ID: PEM certificate}, as
// Google's endpoint answers. It reports whether r was that path.
func (a *agentKeys) serveCerts(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, agentCertPrefix) {
		return false
	}
	email := strings.TrimPrefix(r.URL.Path, agentCertPrefix)
	a.mu.Lock()
	k, ok := a.m[email]
	a.mu.Unlock()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "badRequest", "only GET is served")
		return true
	}
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", "Service account "+email+" not found.")
		return true
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(map[string]string{k.ID: k.Cert})
	return true
}

// signURL is a V4 signed GET URL of gs://bucket/object at public
// (http[s]://host[:port]), signed as email with k at now.
func signURL(k *agentKey, email, public, bucket, object string, now time.Time) (string, error) {
	u, err := url.Parse(public)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the client-facing Cloud Storage address %q is not a URL", public)
	}
	now = now.UTC()
	day, stamp := now.Format("20060102"), now.Format("20060102T150405Z")
	scope := day + "/auto/storage/goog4_request"
	q := url.Values{
		"X-Goog-Algorithm":     {"GOOG4-RSA-SHA256"},
		"X-Goog-Credential":    {email + "/" + scope},
		"X-Goog-Date":          {stamp},
		"X-Goog-Expires":       {fmt.Sprint(int(signedURLExpiry.Seconds()))},
		"X-Goog-SignedHeaders": {"host"},
	}
	query := strings.ReplaceAll(q.Encode(), "+", "%20")
	path := encodePathV4("/" + bucket + "/" + object)
	canonical := "GET\n" + path + "\n" + query + "\nhost:" + u.Host + "\n\nhost\nUNSIGNED-PAYLOAD"
	sum := sha256.Sum256([]byte(canonical))
	sts := "GOOG4-RSA-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	digest := sha256.Sum256([]byte(sts))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host + path + "?" + query + "&X-Goog-Signature=" + hex.EncodeToString(sig), nil
}

// encodePathV4 percent-encodes each path segment, keeping "/", as the
// canonical-requests docs define and Cloud Storage checks.
func encodePathV4(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	return strings.Join(segs, "/")
}

// objectTransform is one EXTERNAL_OBJECT_TRANSFORM call in a query: the
// byte range it spans and the object table it names.
type objectTransform struct {
	pos, end       int
	dataset, table string
}

// rewriteObjectTransforms replaces each call in q with a subquery of the
// object table's rows and their signed_url: 0 and the new text, or a
// refusal. The tables' rows are refreshed first (objectTablesRead).
func (f front) rewriteObjectTransforms(r *http.Request, q queryOptions, calls []objectTransform) (string, int, string) {
	if f.objects.public == "" {
		return "", 501, "Not implemented here: EXTERNAL_OBJECT_TRANSFORM(TABLE ..., ['SIGNED_URL']) needs the address clients reach " +
			"the instance's Cloud Storage at, which this BigQuery was not given (bigquery-front --storage-public). Nothing was run."
	}
	if code, msg := f.objectTablesRead(r, q); code != 0 {
		return "", code, msg
	}
	project := projectOf(f.base)
	now := time.Now()
	if f.objects.signNow != nil {
		now = f.objects.signNow()
	}
	out := q.Query
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		ot, live := f.objectTable(r, c.dataset, c.table)
		if !live {
			return "", 404, "Not found: Table " + project + ":" + c.dataset + "." + c.table
		}
		parts := strings.Split(ot.Connection, ".")
		cn, ok := f.connections.lookup(ot.Number, parts[1], parts[2])
		if !ok {
			return "", 404, "Not found: Connection " + ot.Connection
		}
		sa := cn.serviceAccount()
		if !strings.HasSuffix(sa, agentDomain) {
			return "", 501, "Not implemented here: signing as " + sa + ", which is not a BigQuery connection's service agent. Nothing was run."
		}
		key, err := f.objects.agents.get(sa)
		if err != nil {
			return "", 500, "cloudburrow: could not make the service agent's key: " + err.Error()
		}
		uris, code, msg := f.objectURIs(r, ot)
		if code != 0 {
			return "", code, msg
		}
		var vals []string
		for _, u := range uris {
			bucket, name, _ := strings.Cut(strings.TrimPrefix(u, "gs://"), "/")
			s, err := signURL(key, sa, f.objects.public, bucket, name, now)
			if err != nil {
				return "", 500, "cloudburrow: " + err.Error()
			}
			vals = append(vals, "("+sqlString(u)+", "+sqlString(s)+")")
		}
		sub := "(SELECT _cb_o.*, _cb_s.signed_url FROM " + quotePath([]string{ot.Dataset, ot.Table}) + " AS _cb_o LEFT JOIN UNNEST(" +
			"ARRAY<STRUCT<uri STRING, signed_url STRING>>[" + strings.Join(vals, ", ") + "]) AS _cb_s ON _cb_o.uri = _cb_s.uri)"
		out = out[:c.pos] + sub + out[c.end:]
	}
	return out, 0, ""
}

// objectURIs are the uri column of ot's rows, as the engine holds them.
func (f front) objectURIs(r *http.Request, ot objectTable) ([]string, int, string) {
	legacy := false
	body, _ := json.Marshal(map[string]any{"query": "SELECT uri FROM " + quotePath([]string{ot.Dataset, ot.Table}),
		"useLegacySql": &legacy, "maxResults": 1000000})
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return nil, 500, "cloudburrow: could not read the object table's rows: " + errorMessage(got, status)
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
		PageToken string `json:"pageToken"`
	}
	if err := json.Unmarshal(got, &res); err != nil {
		return nil, 500, "cloudburrow: could not read the object table's rows: " + err.Error()
	}
	if res.PageToken != "" {
		return nil, 501, "Not implemented here: EXTERNAL_OBJECT_TRANSFORM over more rows than one page of the engine's answer. Nothing was run."
	}
	var out []string
	for _, row := range res.Rows {
		if len(row.F) == 1 && row.F[0].V != nil {
			out = append(out, *row.F[0].V)
		}
	}
	sort.Strings(out)
	return out, 0, ""
}
