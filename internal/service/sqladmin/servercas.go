package sqladmin

// Server CA rotation (#1380): addServerCa makes an upcoming CA beside the
// active one, rotateServerCa makes it the one the connector path signs
// with, and listServerCas lists them. The CA rotated out stays trusted for
// the client certificates it signed, as Cloud SQL keeps it.

import (
	"crypto/sha1" //nolint:gosec // Cloud SQL names certificates by SHA-1 fingerprint
	"encoding/hex"
	"errors"
	"net/http"
	"os"
)

const (
	upcomingSuffix = ".upcoming"
	previousSuffix = ".previous"
)

// loadInstanceCA is the instance's active CA, made when it has none, whose
// pool also trusts the CA rotated out, if any.
func loadInstanceCA(dir, instance string) (*serverCA, error) {
	ca, err := loadOrCreateCA(caPath(dir, instance), instance)
	if err != nil {
		return nil, err
	}
	if prev := readCA(caPath(dir, instance) + previousSuffix); prev != nil {
		ca.pool.AddCert(prev.cert)
	}
	return ca, nil
}

// readCA is the CA kept at path, nil when there is none.
func readCA(path string) *serverCA {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	ca, err := loadOrCreateCA(path, "")
	if err != nil {
		return nil
	}
	return ca
}

func fingerprint(ca *serverCA) string {
	fp := sha1.Sum(ca.cert.Raw) //nolint:gosec
	return hex.EncodeToString(fp[:])
}

func (s *Server) serverCAsReady(e Engine) (*serverCA, error) {
	if s.conn == nil || s.conn.cas == nil {
		return nil, unimplemented("server CAs", "this server was started without its connector, which holds the instances' server CAs")
	}
	ca := s.conn.ca(e.Instance())
	if ca == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "instance %s has no server CA yet", e.Instance())
	}
	return ca, nil
}

// listServerCas is instances.listServerCas: the active CA, the upcoming one
// addServerCa made, and the one rotated out.
func (s *Server) listServerCas(project string, e Engine) (any, error) {
	ca, err := s.serverCAsReady(e)
	if err != nil {
		return nil, err
	}
	inst := e.Instance()
	certs := []any{sslCertJSON(project, inst, ca.cert, ca.certPEM)}
	for _, suf := range []string{upcomingSuffix, previousSuffix} {
		if c := readCA(caPath(s.conn.dir, inst) + suf); c != nil {
			certs = append(certs, sslCertJSON(project, inst, c.cert, c.certPEM))
		}
	}
	return map[string]any{"kind": "sql#instancesListServerCas", "certs": certs, "activeVersion": fingerprint(ca)}, nil
}

// addServerCa is instances.addServerCa: a new upcoming CA, replacing one
// added before and never rotated in.
func (s *Server) addServerCa(base, project string, e Engine) (any, error) {
	if _, err := s.serverCAsReady(e); err != nil {
		return nil, err
	}
	path := caPath(s.conn.dir, e.Instance()) + upcomingSuffix
	s.conn.camu.Lock()
	_ = os.Remove(path)
	_, err := loadOrCreateCA(path, e.Instance())
	s.conn.camu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.done(base, project, e, "UPDATE", e.Instance()), nil
}

// rotateServerCa is instances.rotateServerCa: the upcoming CA becomes the
// active one, and the connector issues new server certificates with it.
func (s *Server) rotateServerCa(r *http.Request, base, project string, e Engine) (any, error) {
	if _, err := s.serverCAsReady(e); err != nil {
		return nil, err
	}
	var req struct {
		RotateServerCaContext *struct {
			NextVersion string `json:"nextVersion"`
		} `json:"rotateServerCaContext"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	inst := e.Instance()
	active := caPath(s.conn.dir, inst)
	up := readCA(active + upcomingSuffix)
	if up == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance %s has no upcoming server CA; add one with addServerCa first.", inst)
	}
	if c := req.RotateServerCaContext; c != nil && c.NextVersion != "" && c.NextVersion != fingerprint(up) {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: nextVersion %s is not the upcoming server CA, %s.", c.NextVersion, fingerprint(up))
	}
	s.conn.camu.Lock()
	err := os.Rename(active, active+previousSuffix)
	if err == nil {
		err = os.Rename(active+upcomingSuffix, active)
	}
	var ca *serverCA
	if err == nil {
		ca, err = loadInstanceCA(s.conn.dir, inst)
	}
	if err == nil {
		s.conn.cas[inst] = ca
	}
	s.conn.camu.Unlock()
	if err != nil {
		return nil, errors.Join(errors.New("rotate the server CA"), err)
	}
	s.conn.mu.Lock()
	for name := range s.conn.certs {
		if _, in, ok := parseDNSName(name); ok && in == inst {
			delete(s.conn.certs, name)
		}
	}
	s.conn.mu.Unlock()
	return s.done(base, project, e, "UPDATE", inst), nil
}
