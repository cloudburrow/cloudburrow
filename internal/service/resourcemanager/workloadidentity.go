package resourcemanager

// Workload Identity Federation (#1941): the IAM API's
// projects.locations.workloadIdentityPools, their providers and the
// providers' keys, served as JSON on the IAM endpoint (this port) and kept
// in the registry's store. Behaviour follows Google's pages
// (docs.cloud.google.com/iam/docs/workload-identity-federation,
// .../manage-workload-pools-providers and the IAM v1 REST reference, read
// 2026-11-19):
//
//   - a pool or provider ID is 4 to 32 characters of [a-z0-9-]; the prefix
//     gcp- is Google's; a key ID is 4 to 32 characters of [a-z0-9-];
//   - the only location is global; names are under the project's number;
//   - a display name is at most 32 characters, a description at most 256;
//   - create, update, delete and undelete answer a done Operation;
//   - delete is soft: the resource is DELETED, with an expireTime 30 days
//     on, listed only with showDeleted, still read by get, and undeleted
//     until then; its ID cannot be reused before it is purged;
//   - a provider is exactly one of oidc, aws, saml and x509; an OIDC or
//     SAML provider needs an attribute mapping with google.subject; AWS
//     and X.509 have Google's default mappings; mappings and conditions
//     are CEL (internal/wif);
//   - OIDC's issuerUri must be https://, except a loopback http:// issuer,
//     which CloudBurrow accepts so a local identity provider needs no TLS
//     certificate (Google has no such case: its issuers are public);
//   - a disabled or deleted pool exchanges no token, and its tokens grant
//     nothing until it is enabled again; a disabled provider exchanges no
//     token, and the tokens it issued still grant.
//
// The Security Token Service (internal/metadata's sts.go) reads providers
// through FederationProvider, and IAM sees a federated token's principal://
// and principalSet:// identifiers through Principals.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

const (
	wifPoolPrefix      = "iam/wifpools/"
	wifProviderPrefix  = "iam/wifproviders/"
	wifKeyPrefix       = "iam/wifkeys/"
	wifPrincipalPrefix = "iam/wifprincipals/"
	wifUndeletePeriod  = 30 * 24 * time.Hour
	wifMaxDisplayName  = 32
	wifMaxDescription  = 256
	wifMaxAudiences    = 10
	wifMaxAudience     = 256
	wifMaxSAMLMetadata = 128 * 1024
	wifPoolPageDefault = 50
	wifPoolPageMax     = 1000
	wifProvPageDefault = 50
	wifProvPageMax     = 100
	wifKeyPageMax      = 10
	wifPoolType        = "type.googleapis.com/google.iam.admin.v1.WorkloadIdentityPool"
	wifProviderType    = "type.googleapis.com/google.iam.admin.v1.WorkloadIdentityPoolProvider"
	wifKeyType         = "type.googleapis.com/google.iam.admin.v1.WorkloadIdentityPoolProviderKey"
)

var wifIDPattern = regexp.MustCompile(`^[a-z0-9-]{4,32}$`)

// WorkloadIdentityPool is a pool, as the API renders it.
type WorkloadIdentityPool struct {
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	ExpireTime  string `json:"expireTime,omitempty"`
	Mode        string `json:"mode,omitempty"`
	// Not modelled: managed workload identity (TRUST_DOMAIN mode).
	InlineCertificateIssuanceConfig json.RawMessage `json:"inlineCertificateIssuanceConfig,omitempty"`
	InlineTrustConfig               json.RawMessage `json:"inlineTrustConfig,omitempty"`
}

// WIFOidc is an OIDC provider's configuration.
type WIFOidc struct {
	IssuerURI        string   `json:"issuerUri,omitempty"`
	AllowedAudiences []string `json:"allowedAudiences,omitempty"`
	JwksJSON         string   `json:"jwksJson,omitempty"`
	DiscoveryURI     string   `json:"discoveryUri,omitempty"`
}

// WIFAws is an AWS provider's configuration.
type WIFAws struct {
	AccountID string `json:"accountId,omitempty"`
}

// WIFSaml is a SAML provider's configuration.
type WIFSaml struct {
	IdpMetadataXML string `json:"idpMetadataXml,omitempty"`
}

// WIFPem is one PEM certificate of a trust store.
type WIFPem struct {
	PemCertificate string `json:"pemCertificate,omitempty"`
}

// WIFTrustStore is an X.509 provider's trust store.
type WIFTrustStore struct {
	TrustAnchors         []WIFPem `json:"trustAnchors,omitempty"`
	IntermediateCas      []WIFPem `json:"intermediateCas,omitempty"`
	TrustDefaultSharedCa bool     `json:"trustDefaultSharedCa,omitempty"`
}

// WIFX509 is an X.509 provider's configuration.
type WIFX509 struct {
	TrustStore *WIFTrustStore `json:"trustStore,omitempty"`
}

// WorkloadIdentityPoolProvider is a provider, as the API renders it.
type WorkloadIdentityPoolProvider struct {
	Name               string            `json:"name,omitempty"`
	DisplayName        string            `json:"displayName,omitempty"`
	Description        string            `json:"description,omitempty"`
	State              string            `json:"state,omitempty"`
	Disabled           bool              `json:"disabled,omitempty"`
	ExpireTime         string            `json:"expireTime,omitempty"`
	AttributeMapping   map[string]string `json:"attributeMapping,omitempty"`
	AttributeCondition string            `json:"attributeCondition,omitempty"`
	ActorProviders     []string          `json:"actorProviders,omitempty"`
	Oidc               *WIFOidc          `json:"oidc,omitempty"`
	Aws                *WIFAws           `json:"aws,omitempty"`
	Saml               *WIFSaml          `json:"saml,omitempty"`
	X509               *WIFX509          `json:"x509,omitempty"`
}

// WIFKeyData is a provider key's public half.
type WIFKeyData struct {
	Format        string `json:"format,omitempty"`
	NotBeforeTime string `json:"notBeforeTime,omitempty"`
	NotAfterTime  string `json:"notAfterTime,omitempty"`
	Key           string `json:"key,omitempty"`
	KeySpec       string `json:"keySpec,omitempty"`
}

// WorkloadIdentityPoolProviderKey is a SAML provider's encryption key.
type WorkloadIdentityPoolProviderKey struct {
	Name       string      `json:"name,omitempty"`
	State      string      `json:"state,omitempty"`
	KeyData    *WIFKeyData `json:"keyData,omitempty"`
	ExpireTime string      `json:"expireTime,omitempty"`
	Use        string      `json:"use,omitempty"`
}

// wifRecord is what the store keeps of a pool, provider or key: its
// project ID (names render its number), its IDs, and when it was deleted.
type wifRecord struct {
	Project    string                           `json:"project"`
	Pool       string                           `json:"pool"`
	Provider   string                           `json:"provider,omitempty"`
	Key        string                           `json:"key,omitempty"`
	Deleted    bool                             `json:"deleted,omitempty"`
	DeleteTime time.Time                        `json:"deleteTime,omitempty"`
	PoolV      *WorkloadIdentityPool            `json:"poolValue,omitempty"`
	ProviderV  *WorkloadIdentityPoolProvider    `json:"providerValue,omitempty"`
	KeyV       *WorkloadIdentityPoolProviderKey `json:"keyValue,omitempty"`
}

var wifMu sync.Mutex

func wifPoolKey(project, pool string) string { return wifPoolPrefix + project + "/" + pool }
func wifProviderKey(project, pool, prov string) string {
	return wifProviderPrefix + project + "/" + pool + "/" + prov
}
func wifKeyKey(project, pool, prov, key string) string {
	return wifKeyPrefix + project + "/" + pool + "/" + prov + "/" + key
}

func (r *Registry) wifNumber(project string) string {
	return strconv.FormatInt(r.projectNumber(project), 10)
}

// wifLoad reads a record; nil when there is none, or it was deleted longer
// than the undelete period ago (it is then purged).
func (r *Registry) wifLoad(key string) *wifRecord {
	b, err := r.st.Get(key)
	if err != nil {
		return nil
	}
	var rec wifRecord
	if json.Unmarshal(b, &rec) != nil {
		return nil
	}
	if rec.Deleted && r.now().Sub(rec.DeleteTime) >= wifUndeletePeriod {
		_ = r.st.Delete(key)
		return nil
	}
	return &rec
}

func (r *Registry) wifSave(key string, rec *wifRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return apierror.Internal(err, "encode workload identity record")
	}
	if err := r.st.Put(key, b); err != nil {
		return apierror.Internal(err, "store workload identity record")
	}
	return nil
}

// wifProject resolves a projects/{project} path segment (ID or number) to
// the project's ID.
func (r *Registry) wifProject(seg string) (string, error) {
	r.mu.Lock()
	p, err := r.resolveProjectLocked("projects/" + seg)
	r.unlock()
	if err != nil || p.State == "DELETE_REQUESTED" {
		return "", apierror.NotFound("Requested entity was not found.")
	}
	return p.ProjectID, nil
}

func (r *Registry) wifStamp(rec *wifRecord) {
	expire := ""
	state := "ACTIVE"
	if rec.Deleted {
		state = "DELETED"
		expire = rec.DeleteTime.Add(wifUndeletePeriod).UTC().Format(time.RFC3339Nano)
	}
	pool := wif.PoolName(r.wifNumber(rec.Project), rec.Pool)
	switch {
	case rec.KeyV != nil:
		rec.KeyV.Name, rec.KeyV.State, rec.KeyV.ExpireTime = pool+"/providers/"+rec.Provider+"/keys/"+rec.Key, state, expire
	case rec.ProviderV != nil:
		rec.ProviderV.Name, rec.ProviderV.State, rec.ProviderV.ExpireTime = pool+"/providers/"+rec.Provider, state, expire
	case rec.PoolV != nil:
		rec.PoolV.Name, rec.PoolV.State, rec.PoolV.ExpireTime = pool, state, expire
		if rec.PoolV.Mode == "" {
			rec.PoolV.Mode = "FEDERATION_ONLY"
		}
	}
}

// wifOperation is a done google.longrunning.Operation, kept so that
// operations.get answers it.
var (
	wifOpsMu sync.Mutex
	wifOps   = map[string]map[string]any{}
)

func wifOperation(parent, typ string, v any) map[string]any {
	b, _ := json.Marshal(v)
	var resp map[string]any
	_ = json.Unmarshal(b, &resp)
	resp["@type"] = typ
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	op := map[string]any{"name": parent + "/operations/" + hex.EncodeToString(id), "done": true, "response": resp}
	wifOpsMu.Lock()
	if len(wifOps) > 10000 {
		wifOps = map[string]map[string]any{}
	}
	wifOps[op["name"].(string)] = op
	wifOpsMu.Unlock()
	return op
}

func checkWIFID(what, id string, reservedPrefix bool) error {
	if !wifIDPattern.MatchString(id) {
		return apierror.InvalidArgument("The %s ID must be 4 to 32 characters, and may contain the characters [a-z0-9-]; got %q.", what, id)
	}
	if reservedPrefix && strings.HasPrefix(id, "gcp-") {
		return apierror.InvalidArgument("The prefix `gcp-` is reserved for use by Google, and may not be specified; got %q.", id)
	}
	return nil
}

func checkWIFText(displayName, description string) error {
	if len([]rune(displayName)) > wifMaxDisplayName {
		return apierror.InvalidArgument("The display name cannot exceed %d characters.", wifMaxDisplayName)
	}
	if len([]rune(description)) > wifMaxDescription {
		return apierror.InvalidArgument("The description cannot exceed %d characters.", wifMaxDescription)
	}
	return nil
}

// ---- pools

// CreateWorkloadIdentityPool creates a pool in project (an ID).
func (r *Registry) CreateWorkloadIdentityPool(project, id string, in WorkloadIdentityPool) (WorkloadIdentityPool, error) {
	if err := checkWIFID("pool", id, true); err != nil {
		return in, err
	}
	if err := checkWIFText(in.DisplayName, in.Description); err != nil {
		return in, err
	}
	switch in.Mode {
	case "", "MODE_UNSPECIFIED", "FEDERATION_ONLY":
	default:
		return in, apierror.Unimplemented("Workload identity pool mode %s (managed workload identity) is not supported by CloudBurrow; use FEDERATION_ONLY.", in.Mode)
	}
	if len(in.InlineCertificateIssuanceConfig) > 0 || len(in.InlineTrustConfig) > 0 {
		return in, apierror.Unimplemented("inlineCertificateIssuanceConfig and inlineTrustConfig (managed workload identity) are not supported by CloudBurrow.")
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	if r.wifLoad(wifPoolKey(project, id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	v := WorkloadIdentityPool{DisplayName: in.DisplayName, Description: in.Description, Disabled: in.Disabled, Mode: "FEDERATION_ONLY"}
	rec := &wifRecord{Project: project, Pool: id, PoolV: &v}
	if err := r.wifSave(wifPoolKey(project, id), rec); err != nil {
		return in, err
	}
	r.wifStamp(rec)
	return *rec.PoolV, nil
}

func (r *Registry) wifPool(project, id string) (*wifRecord, error) {
	rec := r.wifLoad(wifPoolKey(project, id))
	if rec == nil || rec.PoolV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// WorkloadIdentityPool reads a pool, deleted or not.
func (r *Registry) WorkloadIdentityPool(project, id string) (WorkloadIdentityPool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifPool(project, id)
	if err != nil {
		return WorkloadIdentityPool{}, err
	}
	r.wifStamp(rec)
	return *rec.PoolV, nil
}

// WorkloadIdentityPools lists a project's pools, by ID.
func (r *Registry) WorkloadIdentityPools(project string, showDeleted bool) ([]WorkloadIdentityPool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	keys, err := r.st.List(wifPoolPrefix + project + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list pools")
	}
	sort.Strings(keys)
	var out []WorkloadIdentityPool
	for _, k := range keys {
		rec := r.wifLoad(k)
		if rec == nil || rec.PoolV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		r.wifStamp(rec)
		out = append(out, *rec.PoolV)
	}
	return out, nil
}

// UpdateWorkloadIdentityPool applies mask's fields of in.
func (r *Registry) UpdateWorkloadIdentityPool(project, id string, in WorkloadIdentityPool, mask []string) (WorkloadIdentityPool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifPool(project, id)
	if err != nil {
		return in, err
	}
	if rec.Deleted {
		return in, apierror.FailedPrecondition("The workload identity pool is deleted; undelete it first.")
	}
	if len(mask) == 0 {
		return in, apierror.InvalidArgument("update_mask is required.")
	}
	v := *rec.PoolV
	for _, f := range mask {
		switch f {
		case "displayName", "display_name":
			v.DisplayName = in.DisplayName
		case "description":
			v.Description = in.Description
		case "disabled":
			v.Disabled = in.Disabled
		default:
			return in, apierror.InvalidArgument("Invalid update_mask path %q: the pool fields that can be updated are display_name, description and disabled.", f)
		}
	}
	if err := checkWIFText(v.DisplayName, v.Description); err != nil {
		return in, err
	}
	rec.PoolV = &v
	if err := r.wifSave(wifPoolKey(project, id), rec); err != nil {
		return in, err
	}
	r.wifStamp(rec)
	return *rec.PoolV, nil
}

// DeleteWorkloadIdentityPool soft-deletes a pool.
func (r *Registry) DeleteWorkloadIdentityPool(project, id string) (WorkloadIdentityPool, error) {
	return r.setPoolDeleted(project, id, true)
}

// UndeleteWorkloadIdentityPool restores a pool deleted less than 30 days ago.
func (r *Registry) UndeleteWorkloadIdentityPool(project, id string) (WorkloadIdentityPool, error) {
	return r.setPoolDeleted(project, id, false)
}

func (r *Registry) setPoolDeleted(project, id string, deleted bool) (WorkloadIdentityPool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifPool(project, id)
	if err != nil {
		return WorkloadIdentityPool{}, err
	}
	if rec.Deleted == deleted {
		if deleted {
			return WorkloadIdentityPool{}, apierror.FailedPrecondition("The workload identity pool is already deleted.")
		}
		return WorkloadIdentityPool{}, apierror.FailedPrecondition("The workload identity pool is not deleted.")
	}
	rec.Deleted, rec.DeleteTime = deleted, time.Time{}
	if deleted {
		rec.DeleteTime = r.now().UTC()
	}
	if err := r.wifSave(wifPoolKey(project, id), rec); err != nil {
		return WorkloadIdentityPool{}, err
	}
	r.wifStamp(rec)
	return *rec.PoolV, nil
}

// ---- providers

// awsDefaultMapping is Google's default for an AWS provider.
var awsDefaultMapping = map[string]string{
	"google.subject": "assertion.arn",
	"attribute.aws_role": "assertion.arn.contains('assumed-role')" +
		" ? assertion.arn.extract('{account_arn}assumed-role/')" +
		" + 'assumed-role/'" +
		" + assertion.arn.extract('assumed-role/{role_name}/')" +
		" : assertion.arn",
}

// x509DefaultMapping is Google's default for an X.509 provider.
var x509DefaultMapping = map[string]string{"google.subject": "assertion.subject.dn.cn"}

func isLoopbackURL(u *url.URL) bool {
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// checkProvider validates a provider's configuration and fills Google's
// defaults.
func checkProvider(p *WorkloadIdentityPoolProvider) error {
	if err := checkWIFText(p.DisplayName, p.Description); err != nil {
		return err
	}
	kinds := 0
	for _, set := range []bool{p.Oidc != nil, p.Aws != nil, p.Saml != nil, p.X509 != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return apierror.InvalidArgument("A provider must specify exactly one of oidc, aws, saml and x509.")
	}
	if len(p.ActorProviders) > 3 {
		return apierror.InvalidArgument("At most 3 actor providers can be specified.")
	}
	if len(p.ActorProviders) > 0 {
		return apierror.Unimplemented("actorProviders (delegated actor tokens) are not supported by CloudBurrow.")
	}
	requireSubject := false
	switch {
	case p.Oidc != nil:
		o := p.Oidc
		u, err := url.Parse(o.IssuerURI)
		if o.IssuerURI == "" || err != nil || u.Host == "" {
			return apierror.InvalidArgument("oidc.issuer_uri is required and must be an HTTPS URL.")
		}
		if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackURL(u)) {
			return apierror.InvalidArgument("oidc.issuer_uri must be an HTTPS endpoint; got %q.", o.IssuerURI)
		}
		if o.DiscoveryURI != "" {
			d, err := url.Parse(o.DiscoveryURI)
			if err != nil || d.Scheme != u.Scheme || d.Host != u.Host || !strings.Contains(d.Path, "/.well-known/") || len(o.DiscoveryURI) > 2000 {
				return apierror.InvalidArgument("oidc.discovery_uri must be on the issuer's scheme, host and port, contain /.well-known/ and not exceed 2000 characters.")
			}
		}
		if len(o.AllowedAudiences) > wifMaxAudiences {
			return apierror.InvalidArgument("A maximum of %d audiences may be configured.", wifMaxAudiences)
		}
		for _, a := range o.AllowedAudiences {
			if a == "" || len(a) > wifMaxAudience {
				return apierror.InvalidArgument("Each allowed audience must be 1 to %d characters.", wifMaxAudience)
			}
		}
		if o.JwksJSON != "" {
			if _, err := wif.ParseJWKS([]byte(o.JwksJSON)); err != nil {
				return apierror.InvalidArgument("oidc.jwks_json: %v", err)
			}
		}
		requireSubject = true
	case p.Aws != nil:
		if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(p.Aws.AccountID) {
			return apierror.InvalidArgument("aws.account_id is required and must be a 12-digit AWS account ID.")
		}
		if len(p.AttributeMapping) == 0 {
			p.AttributeMapping = map[string]string{}
			for k, v := range awsDefaultMapping {
				p.AttributeMapping[k] = v
			}
		}
	case p.Saml != nil:
		if err := checkSAMLMetadata(p.Saml.IdpMetadataXML); err != nil {
			return err
		}
		requireSubject = true
	case p.X509 != nil:
		if p.X509.TrustStore == nil || len(p.X509.TrustStore.TrustAnchors) == 0 {
			return apierror.InvalidArgument("x509.trust_store.trust_anchors must contain at least one certificate.")
		}
		for _, list := range [][]WIFPem{p.X509.TrustStore.TrustAnchors, p.X509.TrustStore.IntermediateCas} {
			for _, c := range list {
				if _, err := parsePEMCert(c.PemCertificate); err != nil {
					return apierror.InvalidArgument("x509.trust_store: %v", err)
				}
			}
		}
		if len(p.AttributeMapping) == 0 {
			p.AttributeMapping = map[string]string{}
			for k, v := range x509DefaultMapping {
				p.AttributeMapping[k] = v
			}
		}
	}
	if err := wif.ValidateMapping(p.AttributeMapping, requireSubject); err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	if err := wif.ValidateCondition(p.AttributeCondition); err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	return nil
}

func parsePEMCert(s string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(s))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("pem_certificate must be a PEM-encoded X.509 certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// checkSAMLMetadata applies Google's constraints on idpMetadataXml: an IdP
// entity ID and one to three signing certificates, at least one not
// expired.
func checkSAMLMetadata(doc string) error {
	if doc == "" {
		return apierror.InvalidArgument("saml.idp_metadata_xml is required.")
	}
	if len(doc) > wifMaxSAMLMetadata {
		return apierror.InvalidArgument("saml.idp_metadata_xml exceeds %d characters.", wifMaxSAMLMetadata)
	}
	var md struct {
		EntityID string `xml:"entityID,attr"`
		IDP      struct {
			Keys []struct {
				Use  string `xml:"use,attr"`
				Cert string `xml:"KeyInfo>X509Data>X509Certificate"`
			} `xml:"KeyDescriptor"`
		} `xml:"IDPSSODescriptor"`
	}
	if err := xml.Unmarshal([]byte(doc), &md); err != nil {
		return apierror.InvalidArgument("saml.idp_metadata_xml is not a valid SAML 2.0 metadata document: %v", err)
	}
	if md.EntityID == "" {
		return apierror.InvalidArgument("saml.idp_metadata_xml must contain an IdP Entity ID.")
	}
	signing, live := 0, 0
	for _, k := range md.IDP.Keys {
		if k.Use != "" && k.Use != "signing" {
			continue
		}
		signing++
		der, err := decodeB64Loose(k.Cert)
		if err != nil {
			continue
		}
		c, err := x509.ParseCertificate(der)
		if err == nil && time.Now().Before(c.NotAfter) {
			live++
		}
	}
	if signing > 3 {
		return apierror.InvalidArgument("Up to three IdP signing keys are allowed.")
	}
	if live == 0 {
		return apierror.InvalidArgument("saml.idp_metadata_xml must contain at least one non-expired signing certificate.")
	}
	return nil
}

func decodeB64Loose(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	b, _ := pem.Decode([]byte("-----BEGIN CERTIFICATE-----\n" + s + "\n-----END CERTIFICATE-----\n"))
	if b == nil {
		return nil, fmt.Errorf("invalid base64 certificate")
	}
	return b.Bytes, nil
}

func (r *Registry) wifProvider(project, pool, id string) (*wifRecord, error) {
	rec := r.wifLoad(wifProviderKey(project, pool, id))
	if rec == nil || rec.ProviderV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// CreateWorkloadIdentityPoolProvider creates a provider in a live pool.
func (r *Registry) CreateWorkloadIdentityPoolProvider(project, pool, id string, in WorkloadIdentityPoolProvider) (WorkloadIdentityPoolProvider, error) {
	if err := checkWIFID("provider", id, true); err != nil {
		return in, err
	}
	in.Name, in.State, in.ExpireTime = "", "", ""
	if err := checkProvider(&in); err != nil {
		return in, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	pr, err := r.wifPool(project, pool)
	if err != nil {
		return in, err
	}
	if pr.Deleted {
		return in, apierror.FailedPrecondition("The workload identity pool is deleted.")
	}
	if r.wifLoad(wifProviderKey(project, pool, id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	rec := &wifRecord{Project: project, Pool: pool, Provider: id, ProviderV: &in}
	if err := r.wifSave(wifProviderKey(project, pool, id), rec); err != nil {
		return in, err
	}
	r.wifStamp(rec)
	return *rec.ProviderV, nil
}

// WorkloadIdentityPoolProvider reads a provider, deleted or not.
func (r *Registry) WorkloadIdentityPoolProvider(project, pool, id string) (WorkloadIdentityPoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifProvider(project, pool, id)
	if err != nil {
		return WorkloadIdentityPoolProvider{}, err
	}
	r.wifStamp(rec)
	return *rec.ProviderV, nil
}

// WorkloadIdentityPoolProviders lists a pool's providers, by ID.
func (r *Registry) WorkloadIdentityPoolProviders(project, pool string, showDeleted bool) ([]WorkloadIdentityPoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wifPool(project, pool); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wifProviderPrefix + project + "/" + pool + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list providers")
	}
	sort.Strings(keys)
	var out []WorkloadIdentityPoolProvider
	for _, k := range keys {
		rec := r.wifLoad(k)
		if rec == nil || rec.ProviderV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		r.wifStamp(rec)
		out = append(out, *rec.ProviderV)
	}
	return out, nil
}

// UpdateWorkloadIdentityPoolProvider applies mask's fields of in.
func (r *Registry) UpdateWorkloadIdentityPoolProvider(project, pool, id string, in WorkloadIdentityPoolProvider, mask []string) (WorkloadIdentityPoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifProvider(project, pool, id)
	if err != nil {
		return in, err
	}
	if rec.Deleted {
		return in, apierror.FailedPrecondition("The workload identity pool provider is deleted; undelete it first.")
	}
	if len(mask) == 0 {
		return in, apierror.InvalidArgument("update_mask is required.")
	}
	v := *rec.ProviderV
	for _, f := range mask {
		switch f {
		case "displayName", "display_name":
			v.DisplayName = in.DisplayName
		case "description":
			v.Description = in.Description
		case "disabled":
			v.Disabled = in.Disabled
		case "attributeMapping", "attribute_mapping":
			v.AttributeMapping = in.AttributeMapping
		case "attributeCondition", "attribute_condition":
			v.AttributeCondition = in.AttributeCondition
		case "oidc", "aws", "saml", "x509":
			v.Oidc, v.Aws, v.Saml, v.X509 = mergeKind(f, v, in)
		default:
			sub, field, ok := strings.Cut(f, ".")
			if !ok || sub != "oidc" || v.Oidc == nil || in.Oidc == nil {
				return in, apierror.InvalidArgument("Invalid update_mask path %q.", f)
			}
			o := *v.Oidc
			switch field {
			case "issuerUri", "issuer_uri":
				o.IssuerURI = in.Oidc.IssuerURI
			case "allowedAudiences", "allowed_audiences":
				o.AllowedAudiences = in.Oidc.AllowedAudiences
			case "jwksJson", "jwks_json":
				o.JwksJSON = in.Oidc.JwksJSON
			case "discoveryUri", "discovery_uri":
				o.DiscoveryURI = in.Oidc.DiscoveryURI
			default:
				return in, apierror.InvalidArgument("Invalid update_mask path %q.", f)
			}
			v.Oidc = &o
		}
	}
	if err := checkProvider(&v); err != nil {
		return in, err
	}
	rec.ProviderV = &v
	if err := r.wifSave(wifProviderKey(project, pool, id), rec); err != nil {
		return in, err
	}
	r.wifStamp(rec)
	return *rec.ProviderV, nil
}

// mergeKind replaces the provider's kind-specific block: a provider's type
// cannot change.
func mergeKind(f string, cur, in WorkloadIdentityPoolProvider) (*WIFOidc, *WIFAws, *WIFSaml, *WIFX509) {
	switch f {
	case "oidc":
		if cur.Oidc != nil {
			return in.Oidc, nil, nil, nil
		}
	case "aws":
		if cur.Aws != nil {
			return nil, in.Aws, nil, nil
		}
	case "saml":
		if cur.Saml != nil {
			return nil, nil, in.Saml, nil
		}
	case "x509":
		if cur.X509 != nil {
			return nil, nil, nil, in.X509
		}
	}
	return cur.Oidc, cur.Aws, cur.Saml, cur.X509
}

// DeleteWorkloadIdentityPoolProvider soft-deletes a provider.
func (r *Registry) DeleteWorkloadIdentityPoolProvider(project, pool, id string) (WorkloadIdentityPoolProvider, error) {
	return r.setProviderDeleted(project, pool, id, true)
}

// UndeleteWorkloadIdentityPoolProvider restores a provider.
func (r *Registry) UndeleteWorkloadIdentityPoolProvider(project, pool, id string) (WorkloadIdentityPoolProvider, error) {
	return r.setProviderDeleted(project, pool, id, false)
}

func (r *Registry) setProviderDeleted(project, pool, id string, deleted bool) (WorkloadIdentityPoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifProvider(project, pool, id)
	if err != nil {
		return WorkloadIdentityPoolProvider{}, err
	}
	if rec.Deleted == deleted {
		if deleted {
			return WorkloadIdentityPoolProvider{}, apierror.FailedPrecondition("The workload identity pool provider is already deleted.")
		}
		return WorkloadIdentityPoolProvider{}, apierror.FailedPrecondition("The workload identity pool provider is not deleted.")
	}
	if !deleted {
		if pr, err := r.wifPool(project, pool); err != nil || pr.Deleted {
			return WorkloadIdentityPoolProvider{}, apierror.FailedPrecondition("The workload identity pool is deleted; undelete it first.")
		}
	}
	rec.Deleted, rec.DeleteTime = deleted, time.Time{}
	if deleted {
		rec.DeleteTime = r.now().UTC()
	}
	if err := r.wifSave(wifProviderKey(project, pool, id), rec); err != nil {
		return WorkloadIdentityPoolProvider{}, err
	}
	r.wifStamp(rec)
	return *rec.ProviderV, nil
}

// ---- provider keys

// CreateWorkloadIdentityPoolProviderKey makes an encryption key pair for a
// SAML provider; the API returns its public certificate.
func (r *Registry) CreateWorkloadIdentityPoolProviderKey(project, pool, prov, id string, in WorkloadIdentityPoolProviderKey) (WorkloadIdentityPoolProviderKey, error) {
	if err := checkWIFID("key", id, false); err != nil {
		return in, err
	}
	if _, err := newProviderKeySpec(in); err != nil {
		return in, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	pr, err := r.wifProvider(project, pool, prov)
	if err != nil {
		return in, err
	}
	if pr.Deleted {
		return in, apierror.FailedPrecondition("The workload identity pool provider is deleted.")
	}
	if r.wifLoad(wifKeyKey(project, pool, prov, id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	v, err := newProviderKey(r.now(), id, in)
	if err != nil {
		return in, err
	}
	rec := &wifRecord{Project: project, Pool: pool, Provider: prov, Key: id, KeyV: &v}
	if err := r.wifSave(wifKeyKey(project, pool, prov, id), rec); err != nil {
		return in, err
	}
	r.wifStamp(rec)
	return *rec.KeyV, nil
}

// newProviderKeySpec checks a key request: use ENCRYPTION and an RSA key
// spec; the answer is the key's size in bits.
func newProviderKeySpec(in WorkloadIdentityPoolProviderKey) (int, error) {
	if in.Use != "ENCRYPTION" {
		return 0, apierror.InvalidArgument("use must be ENCRYPTION.")
	}
	bits := 0
	spec := ""
	if in.KeyData != nil {
		spec = in.KeyData.KeySpec
	}
	switch spec {
	case "RSA_2048":
		bits = 2048
	case "RSA_3072":
		bits = 3072
	case "RSA_4096":
		bits = 4096
	default:
		return 0, apierror.InvalidArgument("key_data.key_spec must be RSA_2048, RSA_3072 or RSA_4096.")
	}
	return bits, nil
}

// newProviderKey makes a provider's encryption key pair, certified for
// ten years; the record keeps its public certificate.
func newProviderKey(at time.Time, id string, in WorkloadIdentityPoolProviderKey) (WorkloadIdentityPoolProviderKey, error) {
	bits, err := newProviderKeySpec(in)
	if err != nil {
		return in, err
	}
	spec := in.KeyData.KeySpec
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return in, apierror.Internal(err, "generate key")
	}
	now := at.UTC().Truncate(time.Second)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: id},
		NotBefore: now, NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return in, apierror.Internal(err, "certify key")
	}
	v := WorkloadIdentityPoolProviderKey{Use: "ENCRYPTION", KeyData: &WIFKeyData{Format: "RSA_X509_PEM", KeySpec: spec,
		NotBeforeTime: tmpl.NotBefore.Format(time.RFC3339), NotAfterTime: tmpl.NotAfter.Format(time.RFC3339),
		Key: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}}
	return v, nil
}

func (r *Registry) wifKey(project, pool, prov, id string) (*wifRecord, error) {
	rec := r.wifLoad(wifKeyKey(project, pool, prov, id))
	if rec == nil || rec.KeyV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// WorkloadIdentityPoolProviderKeys lists a provider's keys.
func (r *Registry) WorkloadIdentityPoolProviderKeys(project, pool, prov string, showDeleted bool) ([]WorkloadIdentityPoolProviderKey, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wifProvider(project, pool, prov); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wifKeyPrefix + project + "/" + pool + "/" + prov + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list keys")
	}
	sort.Strings(keys)
	var out []WorkloadIdentityPoolProviderKey
	for _, k := range keys {
		rec := r.wifLoad(k)
		if rec == nil || rec.KeyV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		r.wifStamp(rec)
		out = append(out, *rec.KeyV)
	}
	return out, nil
}

func (r *Registry) wifKeyGet(project, pool, prov, id string) (WorkloadIdentityPoolProviderKey, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifKey(project, pool, prov, id)
	if err != nil {
		return WorkloadIdentityPoolProviderKey{}, err
	}
	r.wifStamp(rec)
	return *rec.KeyV, nil
}

func (r *Registry) setKeyDeleted(project, pool, prov, id string, deleted bool) (WorkloadIdentityPoolProviderKey, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifKey(project, pool, prov, id)
	if err != nil {
		return WorkloadIdentityPoolProviderKey{}, err
	}
	if rec.Deleted == deleted {
		return WorkloadIdentityPoolProviderKey{}, apierror.FailedPrecondition("The key is already in that state.")
	}
	rec.Deleted, rec.DeleteTime = deleted, time.Time{}
	if deleted {
		rec.DeleteTime = r.now().UTC()
	}
	if err := r.wifSave(wifKeyKey(project, pool, prov, id), rec); err != nil {
		return WorkloadIdentityPoolProviderKey{}, err
	}
	r.wifStamp(rec)
	return *rec.KeyV, nil
}

// ---- federation: what the Security Token Service and IAM read

// FederationProvider is the live provider an STS audience names
// ("//iam.googleapis.com/projects/N/locations/global/workloadIdentityPools/P/providers/R",
// with or without an https: scheme). ok is false when the audience names
// no provider, or its pool or itself is deleted or disabled: Google's
// invalid_target.
func (r *Registry) FederationProvider(audience string) (wif.Provider, bool) {
	if strings.Contains(audience, "/workforcePools/") {
		// A workforce pool's provider (#2056, workforce.go).
		return r.workforceProvider(audience)
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(audience, "https:"), "//iam.googleapis.com/")
	parts := strings.Split(rest, "/")
	if len(parts) != 8 || parts[0] != "projects" || parts[2] != "locations" || parts[3] != "global" ||
		parts[4] != "workloadIdentityPools" || parts[6] != "providers" {
		return wif.Provider{}, false
	}
	project, err := r.wifProject(parts[1])
	if err != nil {
		return wif.Provider{}, false
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	pool, err := r.wifPool(project, parts[5])
	if err != nil || pool.Deleted || pool.PoolV.Disabled {
		return wif.Provider{}, false
	}
	pr, err := r.wifProvider(project, parts[5], parts[7])
	if err != nil || pr.Deleted || pr.ProviderV.Disabled {
		return wif.Provider{}, false
	}
	v := pr.ProviderV
	out := wif.Provider{ProjectNumber: r.wifNumber(project), Pool: parts[5], ID: parts[7],
		Mapping: v.AttributeMapping, Condition: v.AttributeCondition}
	switch {
	case v.Oidc != nil:
		out.Kind = wif.KindOIDC
		out.IssuerURI, out.AllowedAudiences, out.JWKSJSON, out.DiscoveryURI = v.Oidc.IssuerURI, v.Oidc.AllowedAudiences, v.Oidc.JwksJSON, v.Oidc.DiscoveryURI
	case v.Aws != nil:
		out.Kind = wif.KindAWS
	case v.Saml != nil:
		out.Kind = wif.KindSAML
	case v.X509 != nil:
		out.Kind = wif.KindX509
	}
	return out, true
}

type wifPrincipalRecord struct {
	Principal string   `json:"principal"`
	Sets      []string `json:"sets"`
}

func wifPrincipalKey(principal string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(principal)))
	return wifPrincipalPrefix + hex.EncodeToString(sum[:])
}

// RecordFederatedPrincipal keeps the principalSet:// identifiers a
// federated subject's latest exchange gave it, which Principals reports.
func (r *Registry) RecordFederatedPrincipal(principal string, sets []string) error {
	if pool, subject, ok := wif.WorkforcePoolOfPrincipal(principal); ok && subject != "" {
		// A workforce pool's subject (#2056) is made by its first
		// exchange; a deleted one exchanges nothing.
		if err := r.recordWorkforceSubject(pool, subject); err != nil {
			return err
		}
	}
	b, err := json.Marshal(wifPrincipalRecord{Principal: principal, Sets: sets})
	if err != nil {
		return err
	}
	return r.st.Put(wifPrincipalKey(principal), b)
}

// federatedPrincipals are a principal:// identifier's members: itself and
// its sets, nil when its pool is gone or disabled (its tokens then grant
// nothing).
func (r *Registry) federatedPrincipals(principal string) []string {
	if !r.FederatedPrincipalActive(principal) {
		return nil
	}
	out := []string{principal}
	if b, err := r.st.Get(wifPrincipalKey(principal)); err == nil {
		var rec wifPrincipalRecord
		if json.Unmarshal(b, &rec) == nil {
			out = append(out, rec.Sets...)
		}
	}
	return out
}

// FederatedPrincipalActive reports whether a principal:// identifier's
// pool exists and is neither deleted nor disabled.
func (r *Registry) FederatedPrincipalActive(principal string) bool {
	if _, _, ok := wif.WorkforcePoolOfPrincipal(principal); ok {
		return r.workforcePrincipalActive(principal)
	}
	number, pool, ok := wif.PoolOfPrincipal(principal)
	if !ok {
		return false
	}
	project, err := r.wifProject(number)
	if err != nil {
		return false
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wifPool(project, pool)
	return err == nil && !rec.Deleted && !rec.PoolV.Disabled
}

// ---- REST

// isWIFPath is a workload identity pools path of the IAM API.
func isWIFPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/projects/")
	if !ok {
		return false
	}
	_, sub, _ := strings.Cut(rest, "/")
	return strings.HasPrefix(sub, "locations/") && strings.Contains(sub, "/workloadIdentityPools")
}

// ServeWorkloadIdentity serves the IAM API's workloadIdentityPools
// collection, its providers, keys and operations.
func (r *Registry) ServeWorkloadIdentity(w http.ResponseWriter, req *http.Request) {
	if err := r.serveWIF(w, req); err != nil {
		rest.WriteError(w, err)
	}
}

func (r *Registry) serveWIF(w http.ResponseWriter, req *http.Request) error {
	req.Body = http.MaxBytesReader(w, req.Body, rest.MaxRequestBytes)
	p := strings.TrimPrefix(req.URL.Path, "/v1/")
	verb := ""
	if i := strings.LastIndex(p, ":"); i > strings.LastIndex(p, "/") {
		p, verb = p[:i], p[i+1:]
	}
	seg := strings.Split(p, "/")
	// projects/{p}/locations/{l}/workloadIdentityPools[/{pool}[/providers[/{prov}[/keys[/{key}]]]]]
	// or .../operations/{op} under any of them.
	if len(seg) < 5 || seg[0] != "projects" || seg[2] != "locations" || seg[4] != "workloadIdentityPools" {
		return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
	}
	if len(seg) >= 7 && seg[6] != "providers" && seg[6] != "operations" {
		return apierror.Unimplemented("workloadIdentityPools.%s (managed workload identity) is not implemented by CloudBurrow.", seg[6])
	}
	if seg[3] != "global" {
		return apierror.InvalidArgument("The only supported location is global; got %q.", seg[3])
	}
	if n := len(seg); n >= 2 && seg[n-2] == "operations" && req.Method == http.MethodGet {
		wifOpsMu.Lock()
		op, ok := wifOps[strings.Join(seg[:2], "/")+"/"+strings.Join(seg[2:], "/")]
		wifOpsMu.Unlock()
		if !ok {
			// Names render the project number; look the operation up
			// under it too.
			for name, o := range wifOpsMuSnapshot() {
				if strings.HasSuffix(name, "/"+strings.Join(seg[2:], "/")) {
					op, ok = o, true
				}
			}
		}
		if !ok {
			return apierror.NotFound("Requested entity was not found.")
		}
		return rest.WriteJSON(w, http.StatusOK, op)
	}
	project, err := r.wifProject(seg[1])
	if err != nil {
		return err
	}
	q := req.URL.Query()
	showDeleted := q.Get("showDeleted") == "true"
	mask := splitWIFMask(q.Get("updateMask"))
	switch len(seg) {
	case 5: // the pools collection
		switch req.Method {
		case http.MethodGet:
			list, err := r.WorkloadIdentityPools(project, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wifPoolPageDefault, wifPoolPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workloadIdentityPools": page, "nextPageToken": next})
		case http.MethodPost:
			var in WorkloadIdentityPool
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkloadIdentityPool(project, q.Get("workloadIdentityPoolId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wifPoolType, out))
		}
	case 6: // one pool
		pool := seg[5]
		var out WorkloadIdentityPool
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.WorkloadIdentityPool(project, pool)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		case req.Method == http.MethodGet && verb == "listAttestationRules":
			// Attestation rules are managed workload identity's
			// (TRUST_DOMAIN pools); a federation pool has none, which
			// Terraform reads after every apply.
			if _, err := r.WorkloadIdentityPool(project, pool); err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{})
		case req.Method == http.MethodPatch && verb == "":
			var in WorkloadIdentityPool
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err = r.UpdateWorkloadIdentityPool(project, pool, in, mask)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.DeleteWorkloadIdentityPool(project, pool)
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.UndeleteWorkloadIdentityPool(project, pool)
		case req.Method == http.MethodPost:
			return apierror.Unimplemented("workloadIdentityPools.%s is not implemented by CloudBurrow.", verb)
		default:
			return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wifPoolType, out))
	case 7: // providers collection (or namespaces)
		pool := seg[5]
		if seg[6] != "providers" {
			return apierror.Unimplemented("workloadIdentityPools.%s (managed workload identity) is not implemented by CloudBurrow.", seg[6])
		}
		switch req.Method {
		case http.MethodGet:
			list, err := r.WorkloadIdentityPoolProviders(project, pool, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wifProvPageDefault, wifProvPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workloadIdentityPoolProviders": page, "nextPageToken": next})
		case http.MethodPost:
			var in WorkloadIdentityPoolProvider
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkloadIdentityPoolProvider(project, pool, q.Get("workloadIdentityPoolProviderId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wifProviderType, out))
		}
	case 8: // one provider
		if seg[6] != "providers" {
			return apierror.Unimplemented("workloadIdentityPools.%s (managed workload identity) is not implemented by CloudBurrow.", seg[6])
		}
		pool, prov := seg[5], seg[7]
		var out WorkloadIdentityPoolProvider
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.WorkloadIdentityPoolProvider(project, pool, prov)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		case req.Method == http.MethodPatch && verb == "":
			var in WorkloadIdentityPoolProvider
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err = r.UpdateWorkloadIdentityPoolProvider(project, pool, prov, in, mask)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.DeleteWorkloadIdentityPoolProvider(project, pool, prov)
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.UndeleteWorkloadIdentityPoolProvider(project, pool, prov)
		default:
			return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wifProviderType, out))
	case 9: // keys collection
		if seg[6] != "providers" || seg[8] != "keys" {
			break
		}
		pool, prov := seg[5], seg[7]
		switch req.Method {
		case http.MethodGet:
			list, err := r.WorkloadIdentityPoolProviderKeys(project, pool, prov, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wifKeyPageMax, wifKeyPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workloadIdentityPoolProviderKeys": page, "nextPageToken": next})
		case http.MethodPost:
			var in WorkloadIdentityPoolProviderKey
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkloadIdentityPoolProviderKey(project, pool, prov, q.Get("workloadIdentityPoolProviderKeyId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wifKeyType, out))
		}
	case 10: // one key
		if seg[6] != "providers" || seg[8] != "keys" {
			break
		}
		pool, prov, key := seg[5], seg[7], seg[9]
		var out WorkloadIdentityPoolProviderKey
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.wifKeyGet(project, pool, prov, key)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.setKeyDeleted(project, pool, prov, key, true)
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.setKeyDeleted(project, pool, prov, key, false)
		default:
			return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wifKeyType, out))
	}
	return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
}

func wifOpsMuSnapshot() map[string]map[string]any {
	wifOpsMu.Lock()
	defer wifOpsMu.Unlock()
	out := make(map[string]map[string]any, len(wifOps))
	for k, v := range wifOps {
		out[k] = v
	}
	return out
}

func splitWIFMask(m string) []string {
	var out []string
	for _, f := range strings.Split(m, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// wifPage is one page of list: pageToken is the offset, as text.
func wifPage[T any](list []T, q url.Values, def, max int) ([]T, string, error) {
	size := def
	if s := q.Get("pageSize"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, "", apierror.InvalidArgument("page_size must be a non-negative integer.")
		}
		if n > 0 {
			size = n
		}
	}
	if size > max {
		size = max
	}
	start := 0
	if t := q.Get("pageToken"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > len(list) {
			return nil, "", apierror.InvalidArgument("Invalid page_token.")
		}
		start = n
	}
	end := start + size
	if end >= len(list) {
		return list[start:], "", nil
	}
	return list[start:end], strconv.Itoa(end), nil
}

// decodeWIF reads a request body, ignoring fields this server does not
// model: gcloud and Terraform send fields newer than the discovery
// document (enableMeshCaCompatibility), which Google accepts.
func decodeWIF(req *http.Request, dst any) error {
	if req.Body == nil {
		return apierror.InvalidArgument("request body is required")
	}
	if err := json.NewDecoder(req.Body).Decode(dst); err != nil && err.Error() != "EOF" {
		return apierror.InvalidArgument("malformed request body: %v", err)
	}
	return nil
}
