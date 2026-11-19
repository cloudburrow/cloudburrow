package sqladmin

// Automatic server certificate rotation during maintenance (#1524).
//
// settings.ipConfiguration.serverCertificateRotationMode
// AUTOMATIC_ROTATION_DURING_MAINTENANCE: Google "can enable automatic
// server certificate rotation to run during maintenance updates up to 180
// days before expiry", for shared CA (GOOGLE_MANAGED_CAS_CA) and
// customer-managed CA (CUSTOMER_MANAGED_CAS_CA) instances; per-instance
// CA instances have no automatic rotation (authorize-ssl and
// manage-ssl-instance, re-read 2026-11-18). "If maintenance is skipped,
// rotation doesn't happen automatically in that window."
//
// So a maintenance run (maintenance.go) on such an instance whose active
// server certificate expires within 180 days rotates it: to the upcoming
// version addServerCertificate added, if there is one, else to a version
// issued then, by the instance's issuer. The version rotated out stays
// listed as the previous one, as rotateServerCertificate leaves it.
//
// UNVERIFIED (re-read 2026-11-18, the pages above, which do not say):
// that the automatic rotation keeps the rotated-out version listed, and
// uses an upcoming version already added rather than a new one.

import (
	"log"
	"time"
)

// autoRotationWindow is how long before its expiry a server certificate
// is rotated during maintenance.
const autoRotationWindow = 180 * 24 * time.Hour

const autoRotation = "AUTOMATIC_ROTATION_DURING_MAINTENANCE"

// rotateDuringMaintenance rotates spec's server certificate, as a
// maintenance run at now does, when its mode asks for it and the active
// certificate expires within 180 days. It reports whether it rotated.
func (s *Server) rotateDuringMaintenance(spec Spec, now time.Time) (bool, error) {
	if m, _ := ipConfig(&spec)["serverCertificateRotationMode"].(string); m != autoRotation || !isCASMode(specCAMode(&spec)) ||
		s.conn == nil || s.conn.cas == nil {
		return false, nil
	}
	iss, err := s.serverIssuer(spec.Name)
	if err != nil {
		return false, err
	}
	s.conn.camu.Lock()
	defer s.conn.camu.Unlock()
	f, err := s.conn.serverCertsLocked(iss, spec.Project, spec.Name)
	if err != nil {
		return false, err
	}
	active, upcoming := -1, -1
	for i, v := range f.Versions {
		if fp, _ := certFingerprint(v.Cert); fp == f.Active {
			active = i
		}
	}
	if active < 0 {
		return false, nil
	}
	_, cert := certFingerprint(f.Versions[active].Cert)
	if cert == nil || cert.NotAfter.Sub(now) > autoRotationWindow {
		return false, nil
	}
	for i, v := range f.Versions {
		if i != active && !v.Used && v.Created > f.Versions[active].Created {
			upcoming = i
		}
	}
	var next serverCertVersion
	if upcoming >= 0 {
		next = f.Versions[upcoming]
	} else if next, err = iss.issue(spec.Project, spec.Name); err != nil {
		return false, err
	}
	next.Used = true
	f.Versions = []serverCertVersion{f.Versions[active], next}
	f.Active, _ = certFingerprint(next.Cert)
	if err := s.conn.writeServerCertsLocked(spec.Name, f); err != nil {
		return false, err
	}
	log.Printf("cloudsql: %s: maintenance rotated the server certificate, which expired %s", spec.Name, cert.NotAfter.UTC().Format(time.RFC3339))
	return true, nil
}
