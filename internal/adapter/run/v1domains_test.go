package run

import (
	"testing"

	runv1 "google.golang.org/api/run/v1"
)

// A mapping's conditions (#1450): DomainRoutable follows the Knative
// DomainMapping's Ready, CertificateProvisioned is True for NONE and False
// (CertificateNotIssued) for AUTOMATIC, and Ready is True only when both
// are, so an AUTOMATIC mapping ends False instead of waiting for ever.
func TestV1DomainConditions(t *testing.T) {
	routed := kdomain{}
	routed.Status.Conditions = []ksvcCondition{{Type: "Ready", Status: "True"}}
	broken := kdomain{}
	broken.Status.Conditions = []ksvcCondition{{Type: "Ready", Status: "False", Reason: "RevisionMissing", Message: "service gone"}}
	for _, tc := range []struct {
		name                   string
		mode                   string
		kd                     kdomain
		found                  bool
		ready, cert, routable  string
		readyReason, certReasn string
	}{
		{"none, routed", "NONE", routed, true, "True", "True", "True", "", ""},
		{"none, pending", "NONE", kdomain{}, true, "Unknown", "True", "Unknown", "RoutePending", ""},
		{"none, route broken", "NONE", broken, true, "False", "True", "False", "RevisionMissing", ""},
		{"automatic, routed", "AUTOMATIC", routed, true, "False", "False", "True", "CertificateNotIssued", "CertificateNotIssued"},
		{"automatic, route broken", "AUTOMATIC", broken, true, "False", "False", "False", "RevisionMissing", "CertificateNotIssued"},
		{"no route", "NONE", kdomain{}, false, "False", "True", "False", "RouteNotFound", ""},
	} {
		dm := &runv1.DomainMapping{Spec: &runv1.DomainMappingSpec{CertificateMode: tc.mode}, Status: &runv1.DomainMappingStatus{}}
		domainConditions(dm, tc.kd, tc.found)
		got := map[string]*runv1.GoogleCloudRunV1Condition{}
		for _, c := range dm.Status.Conditions {
			got[c.Type] = c
		}
		if got["Ready"].Status != tc.ready || got["CertificateProvisioned"].Status != tc.cert || got["DomainRoutable"].Status != tc.routable {
			t.Errorf("%s: Ready %s, CertificateProvisioned %s, DomainRoutable %s; want %s %s %s", tc.name,
				got["Ready"].Status, got["CertificateProvisioned"].Status, got["DomainRoutable"].Status, tc.ready, tc.cert, tc.routable)
		}
		if got["Ready"].Reason != tc.readyReason || got["CertificateProvisioned"].Reason != tc.certReasn {
			t.Errorf("%s: reasons Ready %q, CertificateProvisioned %q; want %q, %q", tc.name,
				got["Ready"].Reason, got["CertificateProvisioned"].Reason, tc.readyReason, tc.certReasn)
		}
		if tc.ready != "True" && got["Ready"].Message == "" {
			t.Errorf("%s: Ready is %s with no message saying why", tc.name, tc.ready)
		}
	}
}
