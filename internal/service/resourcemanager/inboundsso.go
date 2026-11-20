package resourcemanager

import (
	"crypto/dsa" //nolint:staticcheck // an IdP certificate may carry a DSA key: Google accepts "DSA or RSA".
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Identity's inbound SSO (#1959): the customer's SAML and OIDC SSO
// profiles, a SAML profile's IdP credentials, and the SSO assignments
// that give a group or an org unit a profile (or SSO_OFF), stored as
// cloudidentity.googleapis.com/v1 documents them
// (https://docs.cloud.google.com/identity/docs/reference/rest/v1/inboundSamlSsoProfiles,
// .../inboundOidcSsoProfiles, .../inboundSsoAssignments, read
// 2026-11-20). No sign-in happens locally: the instance has no Google
// sign-in page to redirect to an IdP, so a profile is configuration that
// the API, the console and IAM's readers see, and nothing more. The
// service provider details (spConfig, rpConfig.redirectUris) carry the
// shapes Google's Admin console shows (an entity ID and ACS URL "unique to
// this profile", https://knowledge.workspace.google.com/admin/apps/setting-up-sso).

const (
	samlProfilePrefix = "resourcemanager/cloudidentity/inboundSamlSsoProfiles/"
	oidcProfilePrefix = "resourcemanager/cloudidentity/inboundOidcSsoProfiles/"
	ssoAssignPrefix   = "resourcemanager/cloudidentity/inboundSsoAssignments/"
	// maxSAMLProfiles: "You can create up to 1,000 profiles in your
	// organization" (Setting up SSO).
	maxSAMLProfiles = 1000
	// maxIdPCredentials: "Adds an IdpCredential. Up to 2 credentials are
	// allowed."
	maxIdPCredentials = 2
	// ssoMaxPage: "The maximum allowed value is 100. Requests with
	// page_size greater than that will be silently interpreted as 100."
	ssoMaxPage     = 100
	ssoDefaultPage = 50
)

// Inbound SSO modes (InboundSsoAssignment.ssoMode).
const (
	SSOModeOff         = "SSO_OFF"
	SSOModeSAML        = "SAML_SSO"
	SSOModeOIDC        = "OIDC_SSO"
	SSOModeDomainWide  = "DOMAIN_WIDE_SAML_IF_ENABLED"
	redirectNever      = "NEVER"
	redirectUnspecifed = "REDIRECT_CONDITION_UNSPECIFIED"
)

// SAMLProfile is a stored InboundSamlSsoProfile with its IdP credentials.
type SAMLProfile struct {
	Profile     ci.InboundSamlSsoProfile `json:"profile"`
	Credentials []IdPCredential          `json:"credentials,omitempty"`
}

// IdPCredential is one IdP signing certificate of a SAML profile.
type IdPCredential struct {
	ID         string    `json:"id"`
	PEM        string    `json:"pem"`
	KeyType    string    `json:"keyType"` // RSA or DSA
	KeySize    int64     `json:"keySize"`
	UpdateTime time.Time `json:"updateTime"`
}

// JSON is the credential as the API answers it.
func (c IdPCredential) JSON(profile string) *ci.IdpCredential {
	out := &ci.IdpCredential{Name: profile + "/idpCredentials/" + c.ID, UpdateTime: ciTime(c.UpdateTime)}
	if c.KeyType == "DSA" {
		out.DsaKeyInfo = &ci.DsaPublicKeyInfo{KeySize: c.KeySize}
	} else {
		out.RsaKeyInfo = &ci.RsaPublicKeyInfo{KeySize: c.KeySize}
	}
	return out
}

// OIDCProfile is a stored InboundOidcSsoProfile; its client secret is
// input only, kept and never answered.
type OIDCProfile struct {
	Profile      ci.InboundOidcSsoProfile `json:"profile"`
	ClientSecret string                   `json:"clientSecret,omitempty"`
}

// ssoCustomer reads a customer of an SSO resource: customers/my_customer
// is the caller's, the instance's one customer; another is not visible.
func ssoCustomer(c string, required bool) (string, error) {
	switch c = strings.TrimSpace(c); c {
	case "":
		if required {
			return "", apierror.InvalidArgument("Request contains an invalid argument: customer is required, customers/{customer_id}.")
		}
		return CustomerName, nil
	case CustomerName, "customers/my_customer":
		return CustomerName, nil
	}
	if !strings.HasPrefix(c, "customers/") || strings.Count(c, "/") != 1 {
		return "", apierror.InvalidArgument("Invalid customer %q: it must be customers/{customer_id}.", c)
	}
	return "", permissionDenied(c)
}

// ssoFilter reads a list's filter, "The only supported filter is
// filtering by customer": customer=="customers/C0123abc", omitted or
// customers/my_customer for the caller's own.
func ssoFilter(f string) error {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil
	}
	v, ok := strings.CutPrefix(strings.ReplaceAll(f, " ", ""), "customer==")
	if !ok {
		return apierror.InvalidArgument("Invalid filter %q: the only supported filter is customer==\"customers/{customer_id}\".", f)
	}
	v = strings.Trim(v, `"'`)
	_, err := ssoCustomer(v, true)
	return err
}

// httpsURI checks a URI field that "Must use HTTPS".
func httpsURI(field, v string, required bool) error {
	if v == "" {
		if required {
			return apierror.InvalidArgument("Request contains an invalid argument: %s is required.", field)
		}
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return apierror.InvalidArgument("Invalid %s %q: it must be an https URL.", field, v)
	}
	return nil
}

func checkSAMLIdp(c *ci.SamlIdpConfig) error {
	if c == nil {
		return apierror.InvalidArgument("Request contains an invalid argument: idpConfig is required (entityId and singleSignOnServiceUri).")
	}
	if strings.TrimSpace(c.EntityId) == "" {
		return apierror.InvalidArgument("Request contains an invalid argument: idpConfig.entityId is required.")
	}
	if err := httpsURI("idpConfig.singleSignOnServiceUri", c.SingleSignOnServiceUri, true); err != nil {
		return err
	}
	if err := httpsURI("idpConfig.logoutRedirectUri", c.LogoutRedirectUri, false); err != nil {
		return err
	}
	return httpsURI("idpConfig.changePasswordUri", c.ChangePasswordUri, false)
}

func checkOIDCIdp(c *ci.OidcIdpConfig) error {
	if c == nil {
		return apierror.InvalidArgument("Request contains an invalid argument: idpConfig is required (issuerUri).")
	}
	if err := httpsURI("idpConfig.issuerUri", c.IssuerUri, true); err != nil {
		return err
	}
	return httpsURI("idpConfig.changePasswordUri", c.ChangePasswordUri, false)
}

// spConfig is a SAML profile's service provider details: its entity ID,
// and the ACS URL beneath it.
func spConfig(id string) *ci.SamlSpConfig {
	e := "https://accounts.google.com/samlrp/" + id
	return &ci.SamlSpConfig{EntityId: e, AssertionConsumerServiceUri: e + "/acs"}
}

func oidcRedirectURIs(id string) []string {
	return []string{"https://accounts.google.com/samlrp/" + id + "/oidc"}
}

// loadJSONLocked reads one stored document into v; false when absent.
func (r *Registry) loadJSONLocked(key string, v any) (bool, error) {
	b, err := r.st.Get(key)
	if err != nil {
		return false, nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, apierror.Internal(err, "decode %s", key)
	}
	return true, nil
}

func (r *Registry) putJSONLocked(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encode %s", key)
	}
	if err := r.st.Put(key, b); err != nil {
		return apierror.Internal(err, "store %s", key)
	}
	return nil
}

// ssoID reads the id of a name prefix/{id}.
func ssoID(name, prefix string) (string, error) {
	id, ok := strings.CutPrefix(name, prefix+"/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", apierror.InvalidArgument("Invalid name %q: it must be %s/{id}.", name, prefix)
	}
	return id, nil
}

func ssoNotFound(name string) error {
	return &apierror.Error{Code: codes.NotFound, Reason: "notFound", Message: fmt.Sprintf("Requested entity was not found: %s.", name)}
}

// --- SAML profiles

func (r *Registry) samlLocked(name string) (*SAMLProfile, error) {
	id, err := ssoID(name, "inboundSamlSsoProfiles")
	if err != nil {
		return nil, err
	}
	var p SAMLProfile
	ok, err := r.loadJSONLocked(samlProfilePrefix+id, &p)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ssoNotFound(name)
	}
	return &p, nil
}

// CreateSAMLProfile stores a new SAML SSO profile.
func (r *Registry) CreateSAMLProfile(in ci.InboundSamlSsoProfile) (*SAMLProfile, error) {
	cust, err := ssoCustomer(in.Customer, true)
	if err != nil {
		return nil, err
	}
	if err := checkSAMLIdp(in.IdpConfig); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	keys, err := r.st.List(samlProfilePrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list SAML profiles")
	}
	if len(keys) >= maxSAMLProfiles {
		return nil, apierror.FailedPrecondition("The customer has %d SAML SSO profiles: you can create up to %d profiles in your organization.", len(keys), maxSAMLProfiles)
	}
	id := newIdentityID()
	p := &SAMLProfile{Profile: ci.InboundSamlSsoProfile{Name: "inboundSamlSsoProfiles/" + id, Customer: cust,
		DisplayName: in.DisplayName, IdpConfig: in.IdpConfig, SpConfig: spConfig(id)}}
	return p, r.putJSONLocked(samlProfilePrefix+id, p)
}

// GetSAMLProfile reads inboundSamlSsoProfiles/{id}.
func (r *Registry) GetSAMLProfile(name string) (*SAMLProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.samlLocked(name)
}

// ListSAMLProfiles is every SAML profile, by name.
func (r *Registry) ListSAMLProfiles() ([]*SAMLProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys, err := r.st.List(samlProfilePrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list SAML profiles")
	}
	sort.Strings(keys)
	var out []*SAMLProfile
	for _, k := range keys {
		var p SAMLProfile
		if ok, err := r.loadJSONLocked(k, &p); err != nil {
			return nil, err
		} else if ok {
			out = append(out, &p)
		}
	}
	return out, nil
}

// UpdateSAMLProfile applies mask's fields of in to a SAML profile.
func (r *Registry) UpdateSAMLProfile(name string, in ci.InboundSamlSsoProfile, mask []string) (*SAMLProfile, error) {
	if len(mask) == 0 {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: updateMask is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.samlLocked(name)
	if err != nil {
		return nil, err
	}
	cfg := *p.Profile.IdpConfig
	src := in.IdpConfig
	if src == nil {
		src = &ci.SamlIdpConfig{}
	}
	for _, f := range mask {
		switch f {
		case "display_name", "displayName":
			p.Profile.DisplayName = in.DisplayName
		case "idp_config", "idpConfig":
			cfg = *src
		case "idp_config.entity_id", "idpConfig.entityId":
			cfg.EntityId = src.EntityId
		case "idp_config.single_sign_on_service_uri", "idpConfig.singleSignOnServiceUri":
			cfg.SingleSignOnServiceUri = src.SingleSignOnServiceUri
		case "idp_config.logout_redirect_uri", "idpConfig.logoutRedirectUri":
			cfg.LogoutRedirectUri = src.LogoutRedirectUri
		case "idp_config.change_password_uri", "idpConfig.changePasswordUri":
			cfg.ChangePasswordUri = src.ChangePasswordUri
		case "customer":
			return nil, apierror.InvalidArgument("Invalid updateMask field %q: customer is immutable.", f)
		default:
			return nil, apierror.InvalidArgument("Invalid updateMask field %q: it may contain display_name and idp_config (or its fields).", f)
		}
	}
	if err := checkSAMLIdp(&cfg); err != nil {
		return nil, err
	}
	p.Profile.IdpConfig = &cfg
	return p, r.putJSONLocked(samlProfilePrefix+strings.TrimPrefix(p.Profile.Name, "inboundSamlSsoProfiles/"), p)
}

// profileInUseLocked refuses deleting a profile an assignment uses.
func (r *Registry) profileInUseLocked(name string) error {
	as, err := r.assignmentsLocked()
	if err != nil {
		return err
	}
	for _, a := range as {
		if (a.SamlSsoInfo != nil && a.SamlSsoInfo.InboundSamlSsoProfile == name) || (a.OidcSsoInfo != nil && a.OidcSsoInfo.InboundOidcSsoProfile == name) {
			return apierror.FailedPrecondition("The SSO profile %s is used by the SSO assignment %s: change or delete the assignment first.", name, a.Name)
		}
	}
	return nil
}

// DeleteSAMLProfile deletes a SAML profile and its credentials.
func (r *Registry) DeleteSAMLProfile(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.samlLocked(name)
	if err != nil {
		return err
	}
	if err := r.profileInUseLocked(p.Profile.Name); err != nil {
		return err
	}
	if err := r.st.Delete(samlProfilePrefix + strings.TrimPrefix(p.Profile.Name, "inboundSamlSsoProfiles/")); err != nil {
		return apierror.Internal(err, "delete SAML profile")
	}
	return nil
}

// parseIdPCertificate reads pemData, a "PEM encoded x509 certificate
// containing the public key for verifying IdP signatures", whose key is
// RSA or DSA ("PEM or DER X.509 with an embedded public key, using DSA or
// RSA", Setting up SSO).
func parseIdPCertificate(pemData string) (keyType string, size int64, _ error) {
	blk, _ := pem.Decode([]byte(strings.TrimSpace(pemData)))
	if blk == nil || blk.Type != "CERTIFICATE" {
		return "", 0, apierror.InvalidArgument("Invalid pemData: it must be a PEM encoded x509 certificate (-----BEGIN CERTIFICATE-----).")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return "", 0, apierror.InvalidArgument("Invalid pemData: the certificate cannot be parsed: %v.", err)
	}
	switch k := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return "RSA", int64(k.N.BitLen()), nil
	case *dsa.PublicKey:
		return "DSA", int64(k.P.BitLen()), nil
	}
	return "", 0, apierror.InvalidArgument("Invalid pemData: the certificate's public key is %s; an IdP certificate's key must be RSA or DSA.", cert.PublicKeyAlgorithm)
}

// AddIdPCredential adds a certificate to a SAML profile.
func (r *Registry) AddIdPCredential(parent, pemData string) (*SAMLProfile, IdPCredential, error) {
	if strings.TrimSpace(pemData) == "" {
		return nil, IdPCredential{}, apierror.InvalidArgument("Request contains an invalid argument: pemData is required.")
	}
	kt, size, err := parseIdPCertificate(pemData)
	if err != nil {
		return nil, IdPCredential{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.samlLocked(parent)
	if err != nil {
		return nil, IdPCredential{}, err
	}
	if len(p.Credentials) >= maxIdPCredentials {
		return nil, IdPCredential{}, apierror.FailedPrecondition("The profile %s has %d IdP credentials: up to %d credentials are allowed. Delete one first.",
			parent, len(p.Credentials), maxIdPCredentials)
	}
	c := IdPCredential{ID: newIdentityID(), PEM: strings.TrimSpace(pemData), KeyType: kt, KeySize: size, UpdateTime: r.now().UTC()}
	p.Credentials = append(p.Credentials, c)
	return p, c, r.putJSONLocked(samlProfilePrefix+strings.TrimPrefix(p.Profile.Name, "inboundSamlSsoProfiles/"), p)
}

func (r *Registry) credentialLocked(name string) (*SAMLProfile, int, error) {
	pname, cid, ok := strings.Cut(name, "/idpCredentials/")
	if !ok || cid == "" || strings.Contains(cid, "/") {
		return nil, 0, apierror.InvalidArgument("Invalid name %q: it must be inboundSamlSsoProfiles/{id}/idpCredentials/{id}.", name)
	}
	p, err := r.samlLocked(pname)
	if err != nil {
		return nil, 0, err
	}
	for i, c := range p.Credentials {
		if c.ID == cid {
			return p, i, nil
		}
	}
	return nil, 0, ssoNotFound(name)
}

// GetIdPCredential reads one credential.
func (r *Registry) GetIdPCredential(name string) (*SAMLProfile, IdPCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, i, err := r.credentialLocked(name)
	if err != nil {
		return nil, IdPCredential{}, err
	}
	return p, p.Credentials[i], nil
}

// DeleteIdPCredential removes one credential.
func (r *Registry) DeleteIdPCredential(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, i, err := r.credentialLocked(name)
	if err != nil {
		return err
	}
	p.Credentials = slices.Delete(p.Credentials, i, i+1)
	return r.putJSONLocked(samlProfilePrefix+strings.TrimPrefix(p.Profile.Name, "inboundSamlSsoProfiles/"), p)
}

// --- OIDC profiles

func (r *Registry) oidcLocked(name string) (*OIDCProfile, error) {
	id, err := ssoID(name, "inboundOidcSsoProfiles")
	if err != nil {
		return nil, err
	}
	var p OIDCProfile
	ok, err := r.loadJSONLocked(oidcProfilePrefix+id, &p)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ssoNotFound(name)
	}
	return &p, nil
}

// CreateOIDCProfile stores a new OIDC SSO profile.
func (r *Registry) CreateOIDCProfile(in ci.InboundOidcSsoProfile) (*OIDCProfile, error) {
	cust, err := ssoCustomer(in.Customer, true)
	if err != nil {
		return nil, err
	}
	if err := checkOIDCIdp(in.IdpConfig); err != nil {
		return nil, err
	}
	id := newIdentityID()
	p := &OIDCProfile{Profile: ci.InboundOidcSsoProfile{Name: "inboundOidcSsoProfiles/" + id, Customer: cust, DisplayName: in.DisplayName,
		IdpConfig: in.IdpConfig, RpConfig: &ci.OidcRpConfig{RedirectUris: oidcRedirectURIs(id)}}}
	if in.RpConfig != nil {
		p.Profile.RpConfig.ClientId = in.RpConfig.ClientId
		p.ClientSecret = in.RpConfig.ClientSecret
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return p, r.putJSONLocked(oidcProfilePrefix+id, p)
}

// GetOIDCProfile reads inboundOidcSsoProfiles/{id}.
func (r *Registry) GetOIDCProfile(name string) (*OIDCProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.oidcLocked(name)
}

// ListOIDCProfiles is every OIDC profile, by name.
func (r *Registry) ListOIDCProfiles() ([]*OIDCProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys, err := r.st.List(oidcProfilePrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list OIDC profiles")
	}
	sort.Strings(keys)
	var out []*OIDCProfile
	for _, k := range keys {
		var p OIDCProfile
		if ok, err := r.loadJSONLocked(k, &p); err != nil {
			return nil, err
		} else if ok {
			out = append(out, &p)
		}
	}
	return out, nil
}

// UpdateOIDCProfile applies mask's fields of in to an OIDC profile.
func (r *Registry) UpdateOIDCProfile(name string, in ci.InboundOidcSsoProfile, mask []string) (*OIDCProfile, error) {
	if len(mask) == 0 {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: updateMask is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.oidcLocked(name)
	if err != nil {
		return nil, err
	}
	cfg := *p.Profile.IdpConfig
	src := in.IdpConfig
	if src == nil {
		src = &ci.OidcIdpConfig{}
	}
	rp := in.RpConfig
	if rp == nil {
		rp = &ci.OidcRpConfig{}
	}
	for _, f := range mask {
		switch f {
		case "display_name", "displayName":
			p.Profile.DisplayName = in.DisplayName
		case "idp_config", "idpConfig":
			cfg = *src
		case "idp_config.issuer_uri", "idpConfig.issuerUri":
			cfg.IssuerUri = src.IssuerUri
		case "idp_config.change_password_uri", "idpConfig.changePasswordUri":
			cfg.ChangePasswordUri = src.ChangePasswordUri
		case "rp_config", "rpConfig":
			p.Profile.RpConfig.ClientId, p.ClientSecret = rp.ClientId, rp.ClientSecret
		case "rp_config.client_id", "rpConfig.clientId":
			p.Profile.RpConfig.ClientId = rp.ClientId
		case "rp_config.client_secret", "rpConfig.clientSecret":
			p.ClientSecret = rp.ClientSecret
		case "customer":
			return nil, apierror.InvalidArgument("Invalid updateMask field %q: customer is immutable.", f)
		default:
			return nil, apierror.InvalidArgument("Invalid updateMask field %q: it may contain display_name, idp_config and rp_config (or their fields).", f)
		}
	}
	if err := checkOIDCIdp(&cfg); err != nil {
		return nil, err
	}
	p.Profile.IdpConfig = &cfg
	return p, r.putJSONLocked(oidcProfilePrefix+strings.TrimPrefix(p.Profile.Name, "inboundOidcSsoProfiles/"), p)
}

// DeleteOIDCProfile deletes an OIDC profile.
func (r *Registry) DeleteOIDCProfile(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.oidcLocked(name)
	if err != nil {
		return err
	}
	if err := r.profileInUseLocked(p.Profile.Name); err != nil {
		return err
	}
	if err := r.st.Delete(oidcProfilePrefix + strings.TrimPrefix(p.Profile.Name, "inboundOidcSsoProfiles/")); err != nil {
		return apierror.Internal(err, "delete OIDC profile")
	}
	return nil
}

// --- SSO assignments

func (r *Registry) assignmentsLocked() ([]*ci.InboundSsoAssignment, error) {
	keys, err := r.st.List(ssoAssignPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list SSO assignments")
	}
	var out []*ci.InboundSsoAssignment
	for _, k := range keys {
		var a ci.InboundSsoAssignment
		if ok, err := r.loadJSONLocked(k, &a); err != nil {
			return nil, err
		} else if ok {
			out = append(out, &a)
		}
	}
	// Group assignments by rank, then org unit assignments, by name.
	sort.Slice(out, func(i, j int) bool {
		gi, gj := out[i].TargetGroup != "", out[j].TargetGroup != ""
		if gi != gj {
			return gi
		}
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (r *Registry) putAssignmentLocked(a *ci.InboundSsoAssignment) error {
	return r.putJSONLocked(ssoAssignPrefix+strings.TrimPrefix(a.Name, "inboundSsoAssignments/"), a)
}

// checkAssignmentLocked checks an assignment's mode and the profile it
// names: "samlSsoInfo ... Must be set if and only if sso_mode is set to
// SAML_SSO", and the same of oidcSsoInfo and OIDC_SSO.
func (r *Registry) checkAssignmentLocked(a *ci.InboundSsoAssignment) error {
	switch a.SsoMode {
	case SSOModeOff, SSOModeSAML, SSOModeOIDC, SSOModeDomainWide:
	case "", "SSO_MODE_UNSPECIFIED":
		return apierror.InvalidArgument("Request contains an invalid argument: ssoMode is required (SSO_OFF, SAML_SSO, OIDC_SSO or DOMAIN_WIDE_SAML_IF_ENABLED).")
	default:
		return apierror.InvalidArgument("Invalid value at 'sso_mode', %q.", a.SsoMode)
	}
	saml := a.SamlSsoInfo != nil && a.SamlSsoInfo.InboundSamlSsoProfile != ""
	oidc := a.OidcSsoInfo != nil && a.OidcSsoInfo.InboundOidcSsoProfile != ""
	if saml != (a.SsoMode == SSOModeSAML) {
		return apierror.InvalidArgument("samlSsoInfo must be set if and only if ssoMode is SAML_SSO.")
	}
	if oidc != (a.SsoMode == SSOModeOIDC) {
		return apierror.InvalidArgument("oidcSsoInfo must be set if and only if ssoMode is OIDC_SSO.")
	}
	if saml {
		if _, err := r.samlLocked(a.SamlSsoInfo.InboundSamlSsoProfile); err != nil {
			return err
		}
	} else {
		a.SamlSsoInfo = nil
	}
	if oidc {
		if _, err := r.oidcLocked(a.OidcSsoInfo.InboundOidcSsoProfile); err != nil {
			return err
		}
	} else {
		a.OidcSsoInfo = nil
	}
	if b := a.SignInBehavior; b != nil {
		switch b.RedirectCondition {
		case "", redirectUnspecifed, redirectNever:
		default:
			return apierror.InvalidArgument("Invalid value at 'sign_in_behavior.redirect_condition', %q.", b.RedirectCondition)
		}
	}
	return nil
}

// rerankLocked writes the group assignments in order with ranks 1..n,
// placing moved (when set) at rank want (clamped to 1..n).
func (r *Registry) rerankLocked(moved *ci.InboundSsoAssignment, want int64) error {
	all, err := r.assignmentsLocked()
	if err != nil {
		return err
	}
	var order []*ci.InboundSsoAssignment
	for _, a := range all {
		if a.TargetGroup != "" && (moved == nil || a.Name != moved.Name) {
			order = append(order, a)
		}
	}
	if moved != nil && moved.TargetGroup != "" {
		i := int(want) - 1
		i = max(0, min(i, len(order)))
		order = slices.Insert(order, i, moved)
	}
	for i, a := range order {
		if a.Rank != int64(i+1) || (moved != nil && a.Name == moved.Name) {
			a.Rank = int64(i + 1)
			if err := r.putAssignmentLocked(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Registry) assignmentLocked(name string) (*ci.InboundSsoAssignment, error) {
	id, err := ssoID(name, "inboundSsoAssignments")
	if err != nil {
		return nil, err
	}
	var a ci.InboundSsoAssignment
	ok, err := r.loadJSONLocked(ssoAssignPrefix+id, &a)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ssoNotFound(name)
	}
	return &a, nil
}

// CreateSSOAssignment stores an assignment "for users and devices in a
// Customer under a given Group or OrgUnit".
func (r *Registry) CreateSSOAssignment(in ci.InboundSsoAssignment) (*ci.InboundSsoAssignment, error) {
	cust, err := ssoCustomer(in.Customer, true)
	if err != nil {
		return nil, err
	}
	a := in
	a.Customer = cust
	switch {
	case a.TargetGroup != "" && a.TargetOrgUnit != "":
		return nil, apierror.InvalidArgument("targetGroup and targetOrgUnit are mutually exclusive: set one.")
	case a.TargetGroup == "" && a.TargetOrgUnit == "":
		return nil, apierror.InvalidArgument("Request contains an invalid argument: an assignment targets a group (targetGroup) or an org unit (targetOrgUnit).")
	case a.TargetOrgUnit != "":
		if id, ok := strings.CutPrefix(a.TargetOrgUnit, "orgUnits/"); !ok || id == "" || strings.Contains(id, "/") {
			return nil, apierror.InvalidArgument("Invalid targetOrgUnit %q: it must be orgUnits/{org_unit}.", a.TargetOrgUnit)
		}
		if a.Rank != 0 {
			return nil, apierror.InvalidArgument("rank must be zero for an assignment with targetOrgUnit set, not %d.", a.Rank)
		}
	default:
		if a.Rank < 1 {
			return nil, apierror.InvalidArgument("rank must be greater than or equal to one for an assignment with targetGroup set, not %d.", a.Rank)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.TargetGroup != "" {
		if _, err := r.groupFromNameLocked(a.TargetGroup); err != nil {
			return nil, err
		}
	}
	if err := r.checkAssignmentLocked(&a); err != nil {
		return nil, err
	}
	all, err := r.assignmentsLocked()
	if err != nil {
		return nil, err
	}
	for _, o := range all {
		if (a.TargetGroup != "" && o.TargetGroup == a.TargetGroup) || (a.TargetOrgUnit != "" && o.TargetOrgUnit == a.TargetOrgUnit) {
			return nil, apierror.AlreadyExists("The target %s%s already has the SSO assignment %s.", a.TargetGroup, a.TargetOrgUnit, o.Name)
		}
	}
	a.Name = "inboundSsoAssignments/" + newIdentityID()
	if a.TargetGroup != "" {
		if err := r.rerankLocked(&a, a.Rank); err != nil {
			return nil, err
		}
	} else if err := r.putAssignmentLocked(&a); err != nil {
		return nil, err
	}
	return r.assignmentLocked(a.Name)
}

// GetSSOAssignment reads inboundSsoAssignments/{id}.
func (r *Registry) GetSSOAssignment(name string) (*ci.InboundSsoAssignment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assignmentLocked(name)
}

// ListSSOAssignments is every assignment: the group ones by rank, then
// the org unit ones.
func (r *Registry) ListSSOAssignments() ([]*ci.InboundSsoAssignment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assignmentsLocked()
}

// UpdateSSOAssignment applies mask's fields of in: "a PATCH ... with a
// body of { "rank": 1 } moves that (presumably group-targeted) SSO
// assignment to the highest priority and shifts any other group-targeted
// assignments down in priority".
func (r *Registry) UpdateSSOAssignment(name string, in ci.InboundSsoAssignment, mask []string) (*ci.InboundSsoAssignment, error) {
	if len(mask) == 0 {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: updateMask is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := r.assignmentLocked(name)
	if err != nil {
		return nil, err
	}
	rank, reranked := a.Rank, false
	infoSet := map[string]bool{}
	for _, f := range mask {
		switch f {
		case "rank":
			rank, reranked = in.Rank, true
		case "sso_mode", "ssoMode":
			a.SsoMode = in.SsoMode
		case "saml_sso_info", "samlSsoInfo", "saml_sso_info.inbound_saml_sso_profile", "samlSsoInfo.inboundSamlSsoProfile":
			a.SamlSsoInfo, infoSet["saml"] = in.SamlSsoInfo, true
		case "oidc_sso_info", "oidcSsoInfo", "oidc_sso_info.inbound_oidc_sso_profile", "oidcSsoInfo.inboundOidcSsoProfile":
			a.OidcSsoInfo, infoSet["oidc"] = in.OidcSsoInfo, true
		case "sign_in_behavior", "signInBehavior", "sign_in_behavior.redirect_condition", "signInBehavior.redirectCondition":
			a.SignInBehavior = in.SignInBehavior
		case "customer", "target_group", "targetGroup", "target_org_unit", "targetOrgUnit":
			return nil, apierror.InvalidArgument("Invalid updateMask field %q: it is immutable.", f)
		default:
			return nil, apierror.InvalidArgument("Invalid updateMask field %q: it may contain rank, sso_mode, saml_sso_info, oidc_sso_info and sign_in_behavior.", f)
		}
	}
	// A mode changed away from SAML or OIDC drops the profile it named,
	// unless the request set it.
	if a.SsoMode != SSOModeSAML && !infoSet["saml"] {
		a.SamlSsoInfo = nil
	}
	if a.SsoMode != SSOModeOIDC && !infoSet["oidc"] {
		a.OidcSsoInfo = nil
	}
	if err := r.checkAssignmentLocked(a); err != nil {
		return nil, err
	}
	if a.TargetOrgUnit != "" {
		if rank != 0 {
			return nil, apierror.InvalidArgument("rank must be zero for an assignment with targetOrgUnit set, not %d.", rank)
		}
		if err := r.putAssignmentLocked(a); err != nil {
			return nil, err
		}
		return a, nil
	}
	if rank < 1 {
		return nil, apierror.InvalidArgument("rank must be greater than or equal to one for an assignment with targetGroup set, not %d.", rank)
	}
	if !reranked {
		if err := r.putAssignmentLocked(a); err != nil {
			return nil, err
		}
		return a, nil
	}
	if err := r.rerankLocked(a, rank); err != nil {
		return nil, err
	}
	return r.assignmentLocked(a.Name)
}

// DeleteSSOAssignment deletes an assignment; the group ones after it move
// up.
func (r *Registry) DeleteSSOAssignment(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := r.assignmentLocked(name)
	if err != nil {
		return err
	}
	return r.deleteAssignmentLocked(a)
}

func (r *Registry) deleteAssignmentLocked(a *ci.InboundSsoAssignment) error {
	if err := r.st.Delete(ssoAssignPrefix + strings.TrimPrefix(a.Name, "inboundSsoAssignments/")); err != nil {
		return apierror.Internal(err, "delete SSO assignment")
	}
	if a.TargetGroup != "" {
		return r.rerankLocked(nil, 0)
	}
	return nil
}

// dropGroupAssignmentsLocked deletes the assignment of a deleted group.
func (r *Registry) dropGroupAssignmentsLocked(group string) error {
	all, err := r.assignmentsLocked()
	if err != nil {
		return err
	}
	for _, a := range all {
		if a.TargetGroup == group {
			return r.deleteAssignmentLocked(a)
		}
	}
	return nil
}
