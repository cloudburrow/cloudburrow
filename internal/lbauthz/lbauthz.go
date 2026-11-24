// Package lbauthz holds the types Cloud Load Balancing (internal/service/compute)
// and Network Security (internal/service/networksecurity) share for
// authorization policies (#2160): the request a policy matches, a client
// certificate's check, and the policies' verdict. Neither service imports
// the other; cmd/cloudburrow wires Network Security in as the load
// balancers' policy source.
package lbauthz

import (
	"crypto/x509"
	"net/http"
)

// ClientCert is the outcome of a request's client certificate check, as
// the mTLS custom header variables report it.
type ClientCert struct {
	Present, Verified bool
	// Error is client_cert_error's value, "" when there is none.
	Error string
	Leaf  *x509.Certificate
	Chain []*x509.Certificate
}

// Request is what an authorization policy matches a request on.
type Request struct {
	// Rule is the forwarding rule's relative name.
	Rule                 string
	Host, Path, Method   string
	Scheme, SNI          string
	SourceIP, DestIP     string
	SourcePort, DestPort int
	Header               http.Header
	Cert                 ClientCert
	// SourceVM is the instance (projects/P/zones/Z/instances/I) whose
	// internal address the request comes from, "" for none; its first
	// service account, and the permanent IDs of its secure tags' values
	// (its effective tags), are what a rule's source resources match.
	SourceVM             string
	SourceServiceAccount string
	SourceTagValues      []int64
}

// Verdict is the authorization policies' decision on a request.
type Verdict struct {
	// Custom are the CUSTOM policies whose rules the request matches, in
	// evaluation order; each provider must allow it.
	Custom []Custom
	// Allowed is the DENY and ALLOW policies' decision, applied once every
	// custom provider has allowed the request.
	Allowed bool
	// Content are the CUSTOM policies of the CONTENT_AUTHZ profile, asked
	// last, with the request's body.
	Content []Custom
}

// Custom is one CUSTOM policy's provider.
type Custom struct {
	// Policy is the authorization policy's name.
	Policy string
	// IAP delegates the request to Cloud IAP.
	IAP bool
	// Extensions are the authorization extensions' names
	// (projects/P/locations/L/authzExtensions/E).
	Extensions []string
}
