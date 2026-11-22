package compute

import (
	"crypto/tls"
	"net/http"
	"regexp"
	"strings"
)

// Certificate Manager on target proxies (#2160), as
// https://docs.cloud.google.com/certificate-manager/docs/overview,
// .../certificate-manager/docs/maps and the targetHttpsProxies and
// targetSslProxies references (certificateMap, setCertificateMap; read
// 2026-11-22) describe it: a global target HTTPS or SSL proxy's
// certificateMap (//certificatemanager.googleapis.com/projects/P/locations/global/certificateMaps/M)
// chooses each handshake's certificate by SNI from the map's entries, and
// its sslCertificates are then ignored; a regional target HTTPS proxy's
// sslCertificates may name Certificate Manager certificates
// (//certificatemanager.googleapis.com/projects/P/locations/R/certificates/C)
// beside Compute Engine SSL certificates.

const cmPrefix = "//certificatemanager.googleapis.com/"

var (
	certMapRE = regexp.MustCompile(`^projects/[^/]+/locations/global/certificateMaps/[a-zA-Z0-9_-]{1,63}$`)
	cmCertRE  = regexp.MustCompile(`^projects/[^/]+/locations/[a-z0-9-]+/certificates/[a-zA-Z0-9_-]{1,63}$`)
)

// certMapRef checks a proxy's certificate map and returns it in Google's
// form.
func (s *Server) certMapRef(x lbCtx, field, ref string) (string, error) {
	if x.region != "" {
		return "", invalid("Invalid value for field '%s': '%s'. Certificate maps can be attached to global target proxies only.", field, ref)
	}
	name := strings.TrimPrefix(strings.TrimPrefix(ref, "https:"), cmPrefix)
	if !certMapRE.MatchString(name) {
		return "", invalid("Invalid value for field '%s': '%s'. Must be a certificate map URL of the form //certificatemanager.googleapis.com/projects/{project}/locations/global/certificateMaps/{name}.", field, ref)
	}
	if s.opts.CertMaps == nil || !s.opts.CertMaps.MapExists(name) {
		return "", errf(http.StatusNotFound, "notFound", "The resource '%s%s' was not found", cmPrefix, name)
	}
	return cmPrefix + name, nil
}

func setCertificateMap(s *Server, x lbCtx, o, b obj) (obj, error) {
	ref := str(b, "certificateMap")
	m := ""
	if ref != "" {
		var err error
		if m, err = s.certMapRef(x, "certificateMap", ref); err != nil {
			return nil, err
		}
	}
	return s.lbSet(x, o, "setCertificateMap", func(o obj) error {
		if m == "" {
			delete(o, "certificateMap")
			if len(strs(o, "sslCertificates")) == 0 {
				return invalid("Invalid value for field 'certificateMap': ''. A target proxy needs a certificate map or SSL certificates.")
			}
		} else {
			o["certificateMap"] = m
		}
		return nil
	})
}

// isCMCert is whether a proxy's sslCertificates entry names a Certificate
// Manager certificate.
func isCMCert(ref string) bool { return strings.HasPrefix(ref, cmPrefix) }

// cmCertRef checks a Certificate Manager certificate on a regional proxy.
func (s *Server) cmCertRef(x lbCtx, field, ref string) (string, error) {
	name := strings.TrimPrefix(ref, cmPrefix)
	if !cmCertRE.MatchString(name) {
		return "", invalid("Invalid value for field '%s': '%s'. The URL is malformed.", field, ref)
	}
	if x.region == "" {
		return "", invalid("Invalid value for field '%s': '%s'. A global target proxy takes Certificate Manager certificates through a certificate map.", field, ref)
	}
	if loc := strings.Split(name, "/")[3]; loc != x.region {
		return "", invalid("Invalid value for field '%s': '%s'. The certificate must be in region '%s'.", field, ref, x.region)
	}
	if s.opts.CertMaps == nil || len(s.opts.CertMaps.Certificates([]string{name})) == 0 && !s.cmCertExists(name) {
		return "", errf(http.StatusNotFound, "notFound", "The resource '%s' was not found", ref)
	}
	return cmPrefix + name, nil
}

// cmCertExists is whether a Certificate Manager certificate exists,
// ready or not, as the maps interface answers only ready ones.
func (s *Server) cmCertExists(name string) bool {
	type exister interface{ CertificateExists(string) bool }
	if e, ok := s.opts.CertMaps.(exister); ok {
		return e.CertificateExists(name)
	}
	return false
}

// proxyCertificates (s.mu not held) are the certificates a proxy's handshake for sni
// chooses from: its certificate map's, else its SSL certificates
// (Compute Engine's and, on a regional proxy, Certificate Manager's).
func (s *Server) proxyCertificates(proxy obj, sni string) []tls.Certificate {
	cm := s.opts.CertMaps
	if m := str(proxy, "certificateMap"); m != "" {
		if cm == nil {
			return nil
		}
		return cm.MapCertificates(strings.TrimPrefix(m, cmPrefix), sni)
	}
	var certs []tls.Certificate
	var cmNames []string
	for _, c := range strs(proxy, "sslCertificates") {
		if isCMCert(c) {
			cmNames = append(cmNames, strings.TrimPrefix(c, cmPrefix))
			continue
		}
		s.mu.Lock()
		pair, ok := certPair(s.res[c])
		s.mu.Unlock()
		if ok {
			certs = append(certs, pair)
		}
	}
	if len(cmNames) > 0 && cm != nil {
		certs = append(certs, cm.Certificates(cmNames)...)
	}
	return certs
}
