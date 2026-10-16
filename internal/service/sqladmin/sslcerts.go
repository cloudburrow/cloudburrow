package sqladmin

// Client certificates (sslCerts, #1380): each is signed by the instance's
// server CA, the one the connector path already holds, and kept beside it
// in the instance's directory, so they survive `up` as the CA does.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// clientCertLifetime is how long an sslCerts.insert certificate is valid:
// Cloud SQL's client certificates last ten years.
const clientCertLifetime = 10 * 365 * 24 * time.Hour

type sslCertStore struct {
	dir string
	mu  sync.Mutex
}

type storedCert struct {
	Project string `json:"project"`
	PEM     string `json:"cert"`
}

func (c *sslCertStore) path(instance string) string {
	return filepath.Join(c.dir, "cloudsql-"+instance+"-client-certs.json")
}

func (c *sslCertStore) load(instance string) ([]storedCert, error) {
	b, err := os.ReadFile(c.path(instance))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []storedCert
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *sslCertStore) save(instance string, certs []storedCert) error {
	if len(certs) == 0 {
		err := os.Remove(c.path(instance))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return err
	}
	b, _ := json.MarshalIndent(certs, "", "  ")
	tmp := c.path(instance) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path(instance))
}

func parseCertPEM(s string) *x509.Certificate {
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return nil
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil
	}
	return c
}

func sslCertSelfLink(base, project, instance, fp string) string {
	return base + "projects/" + project + "/instances/" + instance + "/sslCerts/" + fp
}

func (s *Server) sslCertJSONs(base, project, instance string) ([]map[string]any, []storedCert, error) {
	all, err := s.certs.load(instance)
	if err != nil {
		return nil, nil, err
	}
	var out []map[string]any
	for _, sc := range all {
		if sc.Project != project {
			continue
		}
		if c := parseCertPEM(sc.PEM); c != nil {
			j := sslCertJSON(project, instance, c, sc.PEM)
			j["selfLink"] = sslCertSelfLink(base, project, instance, j["sha1Fingerprint"].(string))
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["commonName"].(string) < out[j]["commonName"].(string) })
	return out, all, nil
}

func (s *Server) sslCertsReady(r *http.Request, e Engine) (*serverCA, error) {
	if s.certs == nil || s.conn == nil {
		return nil, unimplemented("sslCerts", "this server was started without its connector, which holds the instances' server CAs")
	}
	ca := s.conn.ca(e.Instance())
	if ca == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "instance %s has no server CA yet", e.Instance())
	}
	return ca, nil
}

func (s *Server) sslCerts(r *http.Request, base, project string, e Engine, rest []string) (string, any, error) {
	ca, err := s.sslCertsReady(r, e)
	if err != nil {
		return "sql.sslCerts", nil, err
	}
	inst := e.Instance()
	s.certs.mu.Lock()
	defer s.certs.mu.Unlock()
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			items, _, err := s.sslCertJSONs(base, project, inst)
			if err != nil {
				return "sql.sslCerts.list", nil, err
			}
			list := []any{}
			for _, i := range items {
				list = append(list, i)
			}
			return "sql.sslCerts.list", map[string]any{"kind": "sql#sslCertsList", "items": list}, nil
		case http.MethodPost:
			out, err := s.insertSSLCert(r, base, project, e, ca)
			return "sql.sslCerts.insert", out, err
		}
		return "sql.sslCerts", nil, Errorf(http.StatusNotFound, "method %s is not served on sslCerts", r.Method)
	}
	if len(rest) != 1 {
		return "sql.sslCerts", nil, Errorf(http.StatusNotFound, "%s is not a Cloud SQL Admin API path", r.URL.Path)
	}
	fp := rest[0]
	items, all, err := s.sslCertJSONs(base, project, inst)
	if err != nil {
		return "sql.sslCerts", nil, err
	}
	var found map[string]any
	for _, i := range items {
		if i["sha1Fingerprint"] == fp {
			found = i
		}
	}
	method := "sql.sslCerts.get"
	if r.Method == http.MethodDelete {
		method = "sql.sslCerts.delete"
	}
	if found == nil {
		return method, nil, Errorf(http.StatusNotFound, "The SSL certificate %s does not exist for instance %s.", fp, inst)
	}
	switch r.Method {
	case http.MethodGet:
		return method, found, nil
	case http.MethodDelete:
		var keep []storedCert
		for _, sc := range all {
			if c := parseCertPEM(sc.PEM); c != nil && sc.Project == project &&
				sslCertJSON(project, inst, c, sc.PEM)["sha1Fingerprint"] == fp {
				continue
			}
			keep = append(keep, sc)
		}
		if err := s.certs.save(inst, keep); err != nil {
			return method, nil, err
		}
		return method, s.done(base, project, e, "UPDATE", inst), nil
	}
	return "sql.sslCerts", nil, Errorf(http.StatusNotFound, "method %s is not served on an sslCert", r.Method)
}

func (s *Server) insertSSLCert(r *http.Request, base, project string, e Engine, ca *serverCA) (any, error) {
	var req struct {
		CommonName string `json:"commonName"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.CommonName == "" {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: commonName.")
	}
	inst := e.Instance()
	items, all, err := s.sslCertJSONs(base, project, inst)
	if err != nil {
		return nil, err
	}
	for _, i := range items {
		if i["commonName"] == req.CommonName {
			return nil, Errorf(http.StatusConflict, "A client certificate named %s already exists for instance %s.", req.CommonName, inst)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: req.CommonName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(clientCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err := s.certs.save(inst, append(all, storedCert{Project: project, PEM: certPEM})); err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	info := sslCertJSON(project, inst, cert, certPEM)
	info["selfLink"] = sslCertSelfLink(base, project, inst, info["sha1Fingerprint"].(string))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return map[string]any{
		"kind":         "sql#sslCertsInsert",
		"operation":    s.done(base, project, e, "UPDATE", inst),
		"serverCaCert": sslCertJSON(project, inst, ca.cert, ca.certPEM),
		"clientCert":   map[string]any{"certInfo": info, "certPrivateKey": keyPEM},
	}, nil
}

// resetSslConfig is instances.resetSslConfig: it deletes every client
// certificate and makes the connector issue new server certificates.
func (s *Server) resetSslConfig(base, project string, e Engine) (any, error) {
	if _, err := s.sslCertsReady(nil, e); err != nil {
		return nil, err
	}
	inst := e.Instance()
	s.certs.mu.Lock()
	all, err := s.certs.load(inst)
	if err == nil {
		var keep []storedCert
		for _, sc := range all {
			if sc.Project != project {
				keep = append(keep, sc)
			}
		}
		err = s.certs.save(inst, keep)
	}
	s.certs.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.conn.mu.Lock()
	delete(s.conn.certs, dnsName(project, inst))
	s.conn.mu.Unlock()
	return s.done(base, project, e, "UPDATE", inst), nil
}
