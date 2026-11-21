package resourcemanager

// Workforce Identity Federation (#2056): the IAM API's
// locations.workforcePools, their providers (OIDC and SAML), the
// providers' keys and the pools' subjects, served as JSON on the IAM
// endpoint (this port) beside the workload identity pools
// (workloadidentity.go), and kept in the registry's store. Behaviour
// follows Google's pages (docs.cloud.google.com/iam/docs/workforce-identity-federation,
// .../iam/docs/manage-workforce-identity-pools-providers,
// .../iam/docs/principal-identifiers and the IAM v1
// locations.workforcePools reference, read 2026-11-20):
//
//   - a pool belongs to an organization (parent organizations/N, which
//     must be this instance's); its name is locations/global/workforcePools/ID
//     and does not show the organization; the only location is global;
//   - a pool ID is 6 to 63 lowercase letters, digits or hyphens, starts
//     with a letter, does not end with a hyphen, and is globally unique;
//     a provider or key ID is 4 to 32 characters of [a-z0-9-]; gcp- is
//     Google's prefix;
//   - a display name is at most 32 characters, a description at most 256;
//     sessionDuration is 900s to 43200s, 3600s when unset;
//   - create, update, delete and undelete answer a done Operation; delete
//     is soft (DELETED, an expireTime 30 days on, listed with showDeleted,
//     undeleted until then, its ID not reusable);
//   - a provider is exactly one of oidc and saml, with an attribute
//     mapping that maps google.subject; OIDC needs issuerUri (https://, or
//     a loopback http:// issuer, as for workload pools), clientId (the
//     tokens' aud) and webSsoConfig; a client secret is input only;
//   - subjects are made by the first token exchange of a google.subject
//     and cannot be made otherwise; a deleted subject exchanges no token
//     for 30 days and its tokens grant nothing; revokeSessions invalidates
//     every token issued to the subject before it;
//   - a disabled or deleted pool exchanges no token and its tokens grant
//     nothing; a disabled or deleted provider exchanges no token, and its
//     tokens still grant; accessRestrictions.disableProgrammaticSignin
//     refuses every exchange at the Security Token Service.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/quotalimit"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

// WorkforcePoolQuotaID is IAM's "Number of workforce identity pools"
// quota in the Cloud Quotas API, an organization-level quota (#2102):
// Google's IAM quotas page documents 100 per organization, adjustable.
const (
	WorkforcePoolQuotaID = "WORKFORCE-POOL-COUNT-per-organization"
	maxWorkforcePools    = 100
)

const (
	wfPoolPrefix       = "iam/wfpools/"
	wfProviderPrefix   = "iam/wfproviders/"
	wfKeyPrefix        = "iam/wfkeys/"
	wfSubjectPrefix    = "iam/wfsubjects/"
	wfPoolPageDefault  = 50
	wfPoolPageMax      = 100
	wfMinSession       = 900 * time.Second
	wfMaxSession       = 43200 * time.Second
	wfDefaultSession   = time.Hour
	wfMaxScopes        = 10
	wfMaxScope         = 256
	wfPoolType         = "type.googleapis.com/google.iam.admin.v1.WorkforcePool"
	wfProviderType     = "type.googleapis.com/google.iam.admin.v1.WorkforcePoolProvider"
	wfKeyType          = "type.googleapis.com/google.iam.admin.v1.WorkforcePoolProviderKey"
	wfSubjectType      = "type.googleapis.com/google.iam.admin.v1.WorkforcePoolSubject"
	wfRevokeType       = "type.googleapis.com/google.iam.admin.v1.RevokeWorkforcePoolSubjectSessionsResponse"
	wfSubjectUndelType = "type.googleapis.com/google.iam.admin.v1.UndeleteWorkforcePoolSubjectResponse"
)

var wfPoolIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,61}[a-z0-9]$`)

// ErrSubjectDeleted is wif's: an exchange for a deleted subject.
var ErrSubjectDeleted = wif.ErrSubjectDeleted

// WFServiceConfig is a service web sign-in is allowed to.
type WFServiceConfig struct {
	Domain string `json:"domain,omitempty"`
}

// WFAccessRestrictions are a pool's access restrictions.
type WFAccessRestrictions struct {
	AllowedServices           []WFServiceConfig `json:"allowedServices,omitempty"`
	DisableProgrammaticSignin bool              `json:"disableProgrammaticSignin,omitempty"`
}

// WorkforcePool is a pool, as the API renders it.
type WorkforcePool struct {
	Name               string                `json:"name,omitempty"`
	Parent             string                `json:"parent,omitempty"`
	DisplayName        string                `json:"displayName,omitempty"`
	Description        string                `json:"description,omitempty"`
	State              string                `json:"state,omitempty"`
	Disabled           bool                  `json:"disabled,omitempty"`
	SessionDuration    string                `json:"sessionDuration,omitempty"`
	ExpireTime         string                `json:"expireTime,omitempty"`
	AccessRestrictions *WFAccessRestrictions `json:"accessRestrictions,omitempty"`
}

// WFClientSecretValue is a client secret: its plain text is input only.
type WFClientSecretValue struct {
	PlainText  string `json:"plainText,omitempty"`
	Thumbprint string `json:"thumbprint,omitempty"`
}

// WFClientSecret is an OIDC provider's client secret.
type WFClientSecret struct {
	Value *WFClientSecretValue `json:"value,omitempty"`
}

// WFWebSsoConfig is an OIDC provider's web sign-in configuration.
type WFWebSsoConfig struct {
	ResponseType            string   `json:"responseType,omitempty"`
	AssertionClaimsBehavior string   `json:"assertionClaimsBehavior,omitempty"`
	AdditionalScopes        []string `json:"additionalScopes,omitempty"`
}

// WFOidc is a workforce OIDC provider's configuration.
type WFOidc struct {
	IssuerURI    string          `json:"issuerUri,omitempty"`
	ClientID     string          `json:"clientId,omitempty"`
	ClientSecret *WFClientSecret `json:"clientSecret,omitempty"`
	JwksJSON     string          `json:"jwksJson,omitempty"`
	WebSsoConfig *WFWebSsoConfig `json:"webSsoConfig,omitempty"`
}

// WorkforcePoolProvider is a provider, as the API renders it.
type WorkforcePoolProvider struct {
	Name                  string            `json:"name,omitempty"`
	DisplayName           string            `json:"displayName,omitempty"`
	Description           string            `json:"description,omitempty"`
	State                 string            `json:"state,omitempty"`
	Disabled              bool              `json:"disabled,omitempty"`
	ExpireTime            string            `json:"expireTime,omitempty"`
	AttributeMapping      map[string]string `json:"attributeMapping,omitempty"`
	AttributeCondition    string            `json:"attributeCondition,omitempty"`
	DetailedAuditLogging  bool              `json:"detailedAuditLogging,omitempty"`
	AttributeSyncInterval string            `json:"attributeSyncInterval,omitempty"`
	VendorType            string            `json:"vendorType,omitempty"`
	Oidc                  *WFOidc           `json:"oidc,omitempty"`
	Saml                  *WIFSaml          `json:"saml,omitempty"`
	// Not modelled: they call the identity provider's own APIs, which a
	// local instance never reaches, or SCIM (Gemini Enterprise, Looker).
	ExtraAttributesOauth2Client    json.RawMessage `json:"extraAttributesOauth2Client,omitempty"`
	ExtendedAttributesOauth2Client json.RawMessage `json:"extendedAttributesOauth2Client,omitempty"`
	ScimUsage                      string          `json:"scimUsage,omitempty"`
}

// WorkforcePoolSubject is a pool's subject, as the console lists it (the
// API has no get or list of subjects).
type WorkforcePoolSubject struct {
	Name       string `json:"name"`
	Subject    string `json:"subject"`
	State      string `json:"state"`
	CreateTime string `json:"createTime,omitempty"`
	RevokeTime string `json:"revokeTime,omitempty"`
	ExpireTime string `json:"expireTime,omitempty"`
}

// wfRecord is what the store keeps of a workforce pool, provider, key or
// subject.
type wfRecord struct {
	Pool       string                           `json:"pool"`
	Provider   string                           `json:"provider,omitempty"`
	Key        string                           `json:"key,omitempty"`
	Subject    string                           `json:"subject,omitempty"`
	Deleted    bool                             `json:"deleted,omitempty"`
	DeleteTime time.Time                        `json:"deleteTime,omitempty"`
	CreateTime time.Time                        `json:"createTime,omitempty"`
	RevokeTime time.Time                        `json:"revokeTime,omitempty"`
	PoolV      *WorkforcePool                   `json:"poolValue,omitempty"`
	ProviderV  *WorkforcePoolProvider           `json:"providerValue,omitempty"`
	KeyV       *WorkloadIdentityPoolProviderKey `json:"keyValue,omitempty"`
	// PrivateKey is a key's PKCS #8 private key (#2065), never rendered.
	PrivateKey []byte `json:"privateKey,omitempty"`
}

func wfPoolKey(pool string) string           { return wfPoolPrefix + pool }
func wfProviderKey(pool, prov string) string { return wfProviderPrefix + pool + "/" + prov }
func wfKeyKey(pool, prov, key string) string { return wfKeyPrefix + pool + "/" + prov + "/" + key }
func wfSubjectKey(pool, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return wfSubjectPrefix + pool + "/" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func (r *Registry) wfLoad(key string) *wfRecord {
	b, err := r.st.Get(key)
	if err != nil {
		return nil
	}
	var rec wfRecord
	if json.Unmarshal(b, &rec) != nil {
		return nil
	}
	if rec.Deleted && r.now().Sub(rec.DeleteTime) >= wifUndeletePeriod {
		_ = r.st.Delete(key)
		return nil
	}
	return &rec
}

func (r *Registry) wfSave(key string, rec *wfRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return apierror.Internal(err, "encode workforce identity record")
	}
	if err := r.st.Put(key, b); err != nil {
		return apierror.Internal(err, "store workforce identity record")
	}
	return nil
}

func (r *Registry) wfStamp(rec *wfRecord) {
	expire, state := "", "ACTIVE"
	if rec.Deleted {
		state = "DELETED"
		expire = rec.DeleteTime.Add(wifUndeletePeriod).UTC().Format(time.RFC3339Nano)
	}
	pool := wif.WorkforcePoolName(rec.Pool)
	switch {
	case rec.KeyV != nil:
		rec.KeyV.Name, rec.KeyV.State, rec.KeyV.ExpireTime = pool+"/providers/"+rec.Provider+"/keys/"+rec.Key, state, expire
	case rec.ProviderV != nil:
		rec.ProviderV.Name, rec.ProviderV.State, rec.ProviderV.ExpireTime = pool+"/providers/"+rec.Provider, state, expire
	case rec.PoolV != nil:
		rec.PoolV.Name, rec.PoolV.State, rec.PoolV.ExpireTime = pool, state, expire
		if rec.PoolV.SessionDuration == "" {
			rec.PoolV.SessionDuration = "3600s"
		}
	}
}

// parseWFDuration reads a google.protobuf.Duration's JSON ("3600s").
func parseWFDuration(s string) (time.Duration, bool) {
	if !strings.HasSuffix(s, "s") {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}

func checkWFPool(p *WorkforcePool) error {
	if err := checkWIFText(p.DisplayName, p.Description); err != nil {
		return err
	}
	if p.SessionDuration != "" {
		d, ok := parseWFDuration(p.SessionDuration)
		if !ok || d < wfMinSession || d > wfMaxSession {
			return apierror.InvalidArgument("session_duration must be between 15 minutes (900s) and 12 hours (43200s); got %q.", p.SessionDuration)
		}
	}
	if a := p.AccessRestrictions; a != nil {
		for _, s := range a.AllowedServices {
			if s.Domain == "" {
				return apierror.InvalidArgument("access_restrictions.allowed_services[].domain is required.")
			}
		}
	}
	return nil
}

// ---- pools

// CreateWorkforcePool creates a pool under the organization in.Parent.
func (r *Registry) CreateWorkforcePool(id string, in WorkforcePool) (WorkforcePool, error) {
	if !wfPoolIDPattern.MatchString(id) {
		return in, apierror.InvalidArgument("The workforce pool ID must be 6 to 63 lowercase letters, digits, or hyphens, start with a letter and not end with a hyphen; got %q.", id)
	}
	if strings.HasPrefix(id, "gcp-") {
		return in, apierror.InvalidArgument("The prefix `gcp-` is reserved for use by Google, and may not be specified; got %q.", id)
	}
	if in.Parent == "" {
		return in, apierror.InvalidArgument("parent is required: organizations/{org-id}.")
	}
	if err := checkOrg(in.Parent); err != nil {
		return in, err
	}
	if err := checkWFPool(&in); err != nil {
		return in, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	if r.wfLoad(wfPoolKey(id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	// The organization's Cloud Quotas preference for the pool count, when
	// one is granted (#2102), else Google's default of 100.
	live, err := r.liveWorkforcePoolsLocked(in.Parent)
	if err != nil {
		return in, err
	}
	if limit := quotalimit.Limit(in.Parent, ServiceAccountQuotaService, WorkforcePoolQuotaID, nil, maxWorkforcePools); live >= limit {
		return in, &apierror.Error{Code: codes.ResourceExhausted, Reason: "rateLimitExceeded",
			Message: fmt.Sprintf("Quota exceeded for quota metric 'Number of workforce identity pools' of service 'iam.googleapis.com' for consumer '%s': the limit is %d.", in.Parent, limit)}
	}
	v := WorkforcePool{Parent: in.Parent, DisplayName: in.DisplayName, Description: in.Description, Disabled: in.Disabled,
		SessionDuration: in.SessionDuration, AccessRestrictions: in.AccessRestrictions}
	rec := &wfRecord{Pool: id, PoolV: &v, CreateTime: r.now().UTC()}
	if err := r.wfSave(wfPoolKey(id), rec); err != nil {
		return in, err
	}
	r.wfStamp(rec)
	return *rec.PoolV, nil
}

// LiveWorkforcePools counts an organization's pools that are not deleted:
// the usage of its workforce pool quota (#2102).
func (r *Registry) LiveWorkforcePools(parent string) (int64, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	return r.liveWorkforcePoolsLocked(parent)
}

func (r *Registry) liveWorkforcePoolsLocked(parent string) (int64, error) {
	keys, err := r.st.List(wfPoolPrefix)
	if err != nil {
		return 0, apierror.Internal(err, "list pools")
	}
	var n int64
	for _, k := range keys {
		if rec := r.wfLoad(k); rec != nil && rec.PoolV != nil && rec.PoolV.Parent == parent && !rec.Deleted {
			n++
		}
	}
	return n, nil
}

func (r *Registry) wfPool(id string) (*wfRecord, error) {
	rec := r.wfLoad(wfPoolKey(id))
	if rec == nil || rec.PoolV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// WorkforcePool reads a pool, deleted or not.
func (r *Registry) WorkforcePool(id string) (WorkforcePool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfPool(id)
	if err != nil {
		return WorkforcePool{}, err
	}
	r.wfStamp(rec)
	return *rec.PoolV, nil
}

// WorkforcePoolParent is the organization a pool belongs to, "" when there
// is no such pool: IAM checks a pool's methods on its organization.
func (r *Registry) WorkforcePoolParent(id string) string {
	wifMu.Lock()
	defer wifMu.Unlock()
	if rec, err := r.wfPool(id); err == nil {
		return rec.PoolV.Parent
	}
	return ""
}

// WorkforcePools lists an organization's pools, by ID.
func (r *Registry) WorkforcePools(parent string, showDeleted bool) ([]WorkforcePool, error) {
	if parent == "" {
		return nil, apierror.InvalidArgument("parent is required: organizations/{org-id}.")
	}
	if err := checkOrg(parent); err != nil {
		return nil, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	keys, err := r.st.List(wfPoolPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list pools")
	}
	sort.Strings(keys)
	var out []WorkforcePool
	for _, k := range keys {
		rec := r.wfLoad(k)
		if rec == nil || rec.PoolV == nil || rec.PoolV.Parent != parent || (rec.Deleted && !showDeleted) {
			continue
		}
		r.wfStamp(rec)
		out = append(out, *rec.PoolV)
	}
	return out, nil
}

// UpdateWorkforcePool applies mask's fields of in.
func (r *Registry) UpdateWorkforcePool(id string, in WorkforcePool, mask []string) (WorkforcePool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfPool(id)
	if err != nil {
		return in, err
	}
	if rec.Deleted {
		return in, apierror.FailedPrecondition("The workforce pool is deleted; undelete it first.")
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
		case "sessionDuration", "session_duration":
			v.SessionDuration = in.SessionDuration
		case "accessRestrictions", "access_restrictions", "accessRestrictions.disableProgrammaticSignin", "access_restrictions.disable_programmatic_signin":
			cur := WFAccessRestrictions{}
			if v.AccessRestrictions != nil {
				cur = *v.AccessRestrictions
			}
			next := WFAccessRestrictions{AllowedServices: cur.AllowedServices}
			if in.AccessRestrictions != nil {
				next.DisableProgrammaticSignin = in.AccessRestrictions.DisableProgrammaticSignin
				if !strings.Contains(f, ".") && !sameServices(cur.AllowedServices, in.AccessRestrictions.AllowedServices) {
					return in, apierror.InvalidArgument("access_restrictions.allowed_services is immutable.")
				}
			}
			v.AccessRestrictions = &next
		case "parent":
			return in, apierror.InvalidArgument("parent is immutable.")
		default:
			return in, apierror.InvalidArgument("Invalid update_mask path %q: the pool fields that can be updated are display_name, description, disabled, session_duration and access_restrictions.", f)
		}
	}
	if err := checkWFPool(&v); err != nil {
		return in, err
	}
	rec.PoolV = &v
	if err := r.wfSave(wfPoolKey(id), rec); err != nil {
		return in, err
	}
	r.wfStamp(rec)
	return *rec.PoolV, nil
}

func sameServices(a, b []WFServiceConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *Registry) setWFPoolDeleted(id string, deleted bool) (WorkforcePool, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfPool(id)
	if err != nil {
		return WorkforcePool{}, err
	}
	if rec.Deleted == deleted {
		if deleted {
			return WorkforcePool{}, apierror.FailedPrecondition("The workforce pool is already deleted.")
		}
		return WorkforcePool{}, apierror.FailedPrecondition("The workforce pool is not deleted.")
	}
	rec.Deleted, rec.DeleteTime = deleted, time.Time{}
	if deleted {
		rec.DeleteTime = r.now().UTC()
	}
	if err := r.wfSave(wfPoolKey(id), rec); err != nil {
		return WorkforcePool{}, err
	}
	r.wfStamp(rec)
	return *rec.PoolV, nil
}

// DeleteWorkforcePool soft-deletes a pool.
func (r *Registry) DeleteWorkforcePool(id string) (WorkforcePool, error) {
	return r.setWFPoolDeleted(id, true)
}

// UndeleteWorkforcePool restores a pool deleted less than 30 days ago.
func (r *Registry) UndeleteWorkforcePool(id string) (WorkforcePool, error) {
	return r.setWFPoolDeleted(id, false)
}

// ---- providers

var (
	wfResponseTypes = map[string]bool{"CODE": true, "ID_TOKEN": true}
	wfClaimsModes   = map[string]bool{"MERGE_USER_INFO_OVER_ID_TOKEN_CLAIMS": true, "ONLY_ID_TOKEN_CLAIMS": true}
)

// checkWFProvider validates a workforce provider's configuration.
func checkWFProvider(p *WorkforcePoolProvider) error {
	if err := checkWIFText(p.DisplayName, p.Description); err != nil {
		return err
	}
	if (p.Oidc == nil) == (p.Saml == nil) {
		return apierror.InvalidArgument("A provider must specify exactly one of oidc and saml.")
	}
	if len(p.ExtraAttributesOauth2Client) > 0 || len(p.ExtendedAttributesOauth2Client) > 0 {
		return apierror.Unimplemented("extraAttributesOauth2Client and extendedAttributesOauth2Client call the identity provider's own APIs, which CloudBurrow never reaches: they are not supported.")
	}
	switch p.ScimUsage {
	case "", "SCIM_USAGE_UNSPECIFIED", scimUsageGroups, scimUsageUsersGroups:
		// SCIM-managed groups, or users and groups (#2068,
		// workforce_scimauthz.go).
	default:
		return apierror.InvalidArgument("Invalid value for scim_usage: %q.", p.ScimUsage)
	}
	if p.AttributeSyncInterval != "" {
		d, ok := parseWFDuration(p.AttributeSyncInterval)
		if !ok || d < 30*time.Minute || d > 12*time.Hour {
			return apierror.InvalidArgument("attribute_sync_interval must be between 30 minutes (1800s) and 12 hours (43200s).")
		}
	}
	if o := p.Oidc; o != nil {
		u, err := url.Parse(o.IssuerURI)
		if o.IssuerURI == "" || err != nil || u.Host == "" {
			return apierror.InvalidArgument("oidc.issuer_uri is required and must be an HTTPS URL.")
		}
		if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackURL(u)) {
			return apierror.InvalidArgument("oidc.issuer_uri must be a valid URI using the https scheme; got %q.", o.IssuerURI)
		}
		if o.ClientID == "" {
			return apierror.InvalidArgument("oidc.client_id is required.")
		}
		if o.JwksJSON != "" {
			if _, err := wif.ParseJWKS([]byte(o.JwksJSON)); err != nil {
				return apierror.InvalidArgument("oidc.jwks_json: %v", err)
			}
		}
		w := o.WebSsoConfig
		if w == nil {
			return apierror.InvalidArgument("oidc.web_sso_config is required.")
		}
		if !wfResponseTypes[w.ResponseType] {
			return apierror.InvalidArgument("oidc.web_sso_config.response_type must be CODE or ID_TOKEN.")
		}
		if !wfClaimsModes[w.AssertionClaimsBehavior] {
			return apierror.InvalidArgument("oidc.web_sso_config.assertion_claims_behavior must be MERGE_USER_INFO_OVER_ID_TOKEN_CLAIMS or ONLY_ID_TOKEN_CLAIMS.")
		}
		if w.ResponseType == "ID_TOKEN" && w.AssertionClaimsBehavior != "ONLY_ID_TOKEN_CLAIMS" {
			return apierror.InvalidArgument("With the ID_TOKEN response type, oidc.web_sso_config.assertion_claims_behavior must be ONLY_ID_TOKEN_CLAIMS.")
		}
		if w.ResponseType == "CODE" && (o.ClientSecret == nil || o.ClientSecret.Value == nil || (o.ClientSecret.Value.PlainText == "" && o.ClientSecret.Value.Thumbprint == "")) {
			return apierror.InvalidArgument("The CODE response type (the authorization code flow) requires oidc.client_secret.")
		}
		if len(w.AdditionalScopes) > wfMaxScopes {
			return apierror.InvalidArgument("A maximum of %d additional scopes may be configured.", wfMaxScopes)
		}
		for _, s := range w.AdditionalScopes {
			if s == "" || len(s) > wfMaxScope {
				return apierror.InvalidArgument("Each additional scope must be 1 to %d characters.", wfMaxScope)
			}
		}
	}
	if p.Saml != nil {
		if err := checkSAMLMetadata(p.Saml.IdpMetadataXML); err != nil {
			return err
		}
	}
	if err := wif.ValidateWorkforceMapping(p.AttributeMapping); err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	if err := wif.ValidateCondition(p.AttributeCondition); err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	for _, g := range []string{"google.profile_photo", "google.display_name", "google.posix_username"} {
		if strings.Contains(p.AttributeCondition, g) {
			return apierror.InvalidArgument("The attribute condition cannot reference %s.", g)
		}
	}
	return nil
}

// sealSecret keeps a client secret's thumbprint only: the plain text is
// input only and never stored or returned.
func sealSecret(o *WFOidc) {
	if o == nil || o.ClientSecret == nil || o.ClientSecret.Value == nil || o.ClientSecret.Value.PlainText == "" {
		return
	}
	sum := sha256.Sum256([]byte(o.ClientSecret.Value.PlainText))
	o.ClientSecret = &WFClientSecret{Value: &WFClientSecretValue{Thumbprint: base64.RawURLEncoding.EncodeToString(sum[:])}}
}

func (r *Registry) wfProvider(pool, id string) (*wfRecord, error) {
	rec := r.wfLoad(wfProviderKey(pool, id))
	if rec == nil || rec.ProviderV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// CreateWorkforcePoolProvider creates a provider in a live pool.
func (r *Registry) CreateWorkforcePoolProvider(pool, id string, in WorkforcePoolProvider) (WorkforcePoolProvider, error) {
	if err := checkWIFID("provider", id, true); err != nil {
		return in, err
	}
	in.Name, in.State, in.ExpireTime = "", "", ""
	if err := checkWFProvider(&in); err != nil {
		return in, err
	}
	sealSecret(in.Oidc)
	wifMu.Lock()
	defer wifMu.Unlock()
	pr, err := r.wfPool(pool)
	if err != nil {
		return in, err
	}
	if pr.Deleted {
		return in, apierror.FailedPrecondition("The workforce pool is deleted.")
	}
	if r.wfLoad(wfProviderKey(pool, id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	if err := r.checkWFScimUsage(pool, id, &in); err != nil {
		return in, err
	}
	rec := &wfRecord{Pool: pool, Provider: id, ProviderV: &in, CreateTime: r.now().UTC()}
	if err := r.wfSave(wfProviderKey(pool, id), rec); err != nil {
		return in, err
	}
	r.wfStamp(rec)
	return *rec.ProviderV, nil
}

// WorkforcePoolProvider reads a provider, deleted or not.
func (r *Registry) WorkforcePoolProvider(pool, id string) (WorkforcePoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfProvider(pool, id)
	if err != nil {
		return WorkforcePoolProvider{}, err
	}
	r.wfStamp(rec)
	return *rec.ProviderV, nil
}

// WorkforcePoolProviders lists a pool's providers, by ID.
func (r *Registry) WorkforcePoolProviders(pool string, showDeleted bool) ([]WorkforcePoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wfPool(pool); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wfProviderPrefix + pool + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list providers")
	}
	sort.Strings(keys)
	var out []WorkforcePoolProvider
	for _, k := range keys {
		rec := r.wfLoad(k)
		if rec == nil || rec.ProviderV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		r.wfStamp(rec)
		out = append(out, *rec.ProviderV)
	}
	return out, nil
}

// UpdateWorkforcePoolProvider applies mask's fields of in.
func (r *Registry) UpdateWorkforcePoolProvider(pool, id string, in WorkforcePoolProvider, mask []string) (WorkforcePoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfProvider(pool, id)
	if err != nil {
		return in, err
	}
	if rec.Deleted {
		return in, apierror.FailedPrecondition("The workforce pool provider is deleted; undelete it first.")
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
		case "detailedAuditLogging", "detailed_audit_logging":
			v.DetailedAuditLogging = in.DetailedAuditLogging
		case "attributeSyncInterval", "attribute_sync_interval":
			v.AttributeSyncInterval = in.AttributeSyncInterval
		case "vendorType", "vendor_type":
			v.VendorType = in.VendorType
		case "scimUsage", "scim_usage":
			v.ScimUsage = in.ScimUsage
		case "oidc":
			if v.Oidc == nil || in.Oidc == nil {
				return in, apierror.InvalidArgument("A provider's type cannot change.")
			}
			o := *in.Oidc
			if o.ClientSecret == nil {
				o.ClientSecret = v.Oidc.ClientSecret
			}
			v.Oidc = &o
		case "saml", "saml.idpMetadataXml", "saml.idp_metadata_xml":
			if v.Saml == nil || in.Saml == nil {
				return in, apierror.InvalidArgument("A provider's type cannot change.")
			}
			v.Saml = &WIFSaml{IdpMetadataXML: in.Saml.IdpMetadataXML}
		default:
			sub, field, ok := strings.Cut(f, ".")
			if !ok || sub != "oidc" || v.Oidc == nil || in.Oidc == nil {
				return in, apierror.InvalidArgument("Invalid update_mask path %q.", f)
			}
			o := *v.Oidc
			switch field {
			case "issuerUri", "issuer_uri":
				o.IssuerURI = in.Oidc.IssuerURI
			case "clientId", "client_id":
				o.ClientID = in.Oidc.ClientID
			case "jwksJson", "jwks_json":
				o.JwksJSON = in.Oidc.JwksJSON
			case "clientSecret", "client_secret", "clientSecret.value.plainText", "client_secret.value.plain_text":
				o.ClientSecret = in.Oidc.ClientSecret
			case "webSsoConfig", "web_sso_config":
				o.WebSsoConfig = in.Oidc.WebSsoConfig
			default:
				if strings.HasPrefix(field, "webSsoConfig.") || strings.HasPrefix(field, "web_sso_config.") {
					w := WFWebSsoConfig{}
					if o.WebSsoConfig != nil {
						w = *o.WebSsoConfig
					}
					src := in.Oidc.WebSsoConfig
					if src == nil {
						src = &WFWebSsoConfig{}
					}
					switch field[strings.Index(field, ".")+1:] {
					case "responseType", "response_type":
						w.ResponseType = src.ResponseType
					case "assertionClaimsBehavior", "assertion_claims_behavior":
						w.AssertionClaimsBehavior = src.AssertionClaimsBehavior
					case "additionalScopes", "additional_scopes":
						w.AdditionalScopes = src.AdditionalScopes
					default:
						return in, apierror.InvalidArgument("Invalid update_mask path %q.", f)
					}
					o.WebSsoConfig = &w
					break
				}
				return in, apierror.InvalidArgument("Invalid update_mask path %q.", f)
			}
			v.Oidc = &o
		}
	}
	if err := checkWFProvider(&v); err != nil {
		return in, err
	}
	if err := r.checkWFScimUsage(pool, id, &v); err != nil {
		return in, err
	}
	sealSecret(v.Oidc)
	rec.ProviderV = &v
	if err := r.wfSave(wfProviderKey(pool, id), rec); err != nil {
		return in, err
	}
	r.wfStamp(rec)
	return *rec.ProviderV, nil
}

func (r *Registry) setWFProviderDeleted(pool, id string, deleted bool) (WorkforcePoolProvider, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfProvider(pool, id)
	if err != nil {
		return WorkforcePoolProvider{}, err
	}
	if rec.Deleted == deleted {
		if deleted {
			return WorkforcePoolProvider{}, apierror.FailedPrecondition("The workforce pool provider is already deleted.")
		}
		return WorkforcePoolProvider{}, apierror.FailedPrecondition("The workforce pool provider is not deleted.")
	}
	if !deleted {
		if pr, err := r.wfPool(pool); err != nil || pr.Deleted {
			return WorkforcePoolProvider{}, apierror.FailedPrecondition("The workforce pool is deleted; undelete it first.")
		}
	}
	rec.Deleted, rec.DeleteTime = deleted, time.Time{}
	if deleted {
		rec.DeleteTime = r.now().UTC()
	}
	if err := r.wfSave(wfProviderKey(pool, id), rec); err != nil {
		return WorkforcePoolProvider{}, err
	}
	r.wfStamp(rec)
	return *rec.ProviderV, nil
}

// DeleteWorkforcePoolProvider soft-deletes a provider.
func (r *Registry) DeleteWorkforcePoolProvider(pool, id string) (WorkforcePoolProvider, error) {
	return r.setWFProviderDeleted(pool, id, true)
}

// UndeleteWorkforcePoolProvider restores a provider.
func (r *Registry) UndeleteWorkforcePoolProvider(pool, id string) (WorkforcePoolProvider, error) {
	return r.setWFProviderDeleted(pool, id, false)
}

// ---- provider keys

// CreateWorkforcePoolProviderKey makes an encryption key pair for a
// provider; the API returns its public certificate.
func (r *Registry) CreateWorkforcePoolProviderKey(pool, prov, id string, in WorkloadIdentityPoolProviderKey) (WorkloadIdentityPoolProviderKey, error) {
	if err := checkWIFID("key", id, false); err != nil {
		return in, err
	}
	wifMu.Lock()
	pr, err := r.wfProvider(pool, prov)
	if err == nil && pr.Deleted {
		err = apierror.FailedPrecondition("The workforce pool provider is deleted.")
	}
	if err == nil && r.wfLoad(wfKeyKey(pool, prov, id)) != nil {
		err = apierror.AlreadyExists("Requested entity already exists")
	}
	wifMu.Unlock()
	if err != nil {
		return in, err
	}
	v, priv, err := newProviderKey(r.now(), id, in)
	if err != nil {
		return in, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	rec := &wfRecord{Pool: pool, Provider: prov, Key: id, KeyV: &v, PrivateKey: priv, CreateTime: r.now().UTC()}
	if err := r.wfSave(wfKeyKey(pool, prov, id), rec); err != nil {
		return in, err
	}
	r.wfStamp(rec)
	return *rec.KeyV, nil
}

func (r *Registry) wfKey(pool, prov, id string) (*wfRecord, error) {
	rec := r.wfLoad(wfKeyKey(pool, prov, id))
	if rec == nil || rec.KeyV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// WorkforcePoolProviderKeys lists a provider's keys.
func (r *Registry) WorkforcePoolProviderKeys(pool, prov string, showDeleted bool) ([]WorkloadIdentityPoolProviderKey, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wfProvider(pool, prov); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wfKeyPrefix + pool + "/" + prov + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list keys")
	}
	sort.Strings(keys)
	var out []WorkloadIdentityPoolProviderKey
	for _, k := range keys {
		rec := r.wfLoad(k)
		if rec == nil || rec.KeyV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		r.wfStamp(rec)
		out = append(out, *rec.KeyV)
	}
	return out, nil
}

func (r *Registry) wfKeyGet(pool, prov, id string) (WorkloadIdentityPoolProviderKey, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfKey(pool, prov, id)
	if err != nil {
		return WorkloadIdentityPoolProviderKey{}, err
	}
	r.wfStamp(rec)
	return *rec.KeyV, nil
}

func (r *Registry) setWFKeyDeleted(pool, prov, id string, deleted bool) (WorkloadIdentityPoolProviderKey, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfKey(pool, prov, id)
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
	if err := r.wfSave(wfKeyKey(pool, prov, id), rec); err != nil {
		return WorkloadIdentityPoolProviderKey{}, err
	}
	r.wfStamp(rec)
	return *rec.KeyV, nil
}

// ---- subjects

func (r *Registry) wfSubject(pool, subject string) *wfRecord {
	rec := r.wfLoad(wfSubjectKey(pool, subject))
	if rec == nil || rec.Subject == "" {
		return nil
	}
	return rec
}

func wfSubjectView(rec *wfRecord) WorkforcePoolSubject {
	s := WorkforcePoolSubject{Name: wif.WorkforcePoolName(rec.Pool) + "/subjects/" + rec.Subject, Subject: rec.Subject, State: "ACTIVE",
		CreateTime: rec.CreateTime.UTC().Format(time.RFC3339)}
	if rec.Deleted {
		s.State = "DELETED"
		s.ExpireTime = rec.DeleteTime.Add(wifUndeletePeriod).UTC().Format(time.RFC3339)
	}
	if !rec.RevokeTime.IsZero() {
		s.RevokeTime = rec.RevokeTime.UTC().Format(time.RFC3339)
	}
	return s
}

// WorkforcePoolSubjects are a pool's subjects, for the console.
func (r *Registry) WorkforcePoolSubjects(pool string) ([]WorkforcePoolSubject, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wfPool(pool); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wfSubjectPrefix + pool + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list subjects")
	}
	var out []WorkforcePoolSubject
	for _, k := range keys {
		if rec := r.wfLoad(k); rec != nil && rec.Subject != "" {
			out = append(out, wfSubjectView(rec))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out, nil
}

// DeleteWorkforcePoolSubject soft-deletes a subject.
func (r *Registry) DeleteWorkforcePoolSubject(pool, subject string) (WorkforcePoolSubject, error) {
	return r.changeWFSubject(pool, subject, func(rec *wfRecord) error {
		if rec.Deleted {
			return apierror.FailedPrecondition("The workforce pool subject is already deleted.")
		}
		rec.Deleted, rec.DeleteTime = true, r.now().UTC()
		return nil
	})
}

// UndeleteWorkforcePoolSubject restores a subject deleted less than 30
// days ago.
func (r *Registry) UndeleteWorkforcePoolSubject(pool, subject string) (WorkforcePoolSubject, error) {
	return r.changeWFSubject(pool, subject, func(rec *wfRecord) error {
		if !rec.Deleted {
			return apierror.FailedPrecondition("The workforce pool subject is not deleted.")
		}
		rec.Deleted, rec.DeleteTime = false, time.Time{}
		return nil
	})
}

// RevokeWorkforcePoolSubjectSessions invalidates every token the subject
// was issued until now.
func (r *Registry) RevokeWorkforcePoolSubjectSessions(pool, subject string) (WorkforcePoolSubject, error) {
	return r.changeWFSubject(pool, subject, func(rec *wfRecord) error {
		if rec.Deleted {
			return apierror.FailedPrecondition("The workforce pool subject is deleted.")
		}
		rec.RevokeTime = r.now().UTC()
		return nil
	})
}

func (r *Registry) changeWFSubject(pool, subject string, f func(*wfRecord) error) (WorkforcePoolSubject, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wfPool(pool); err != nil {
		return WorkforcePoolSubject{}, err
	}
	rec := r.wfSubject(pool, subject)
	if rec == nil {
		return WorkforcePoolSubject{}, apierror.NotFound("Requested entity was not found.")
	}
	if err := f(rec); err != nil {
		return WorkforcePoolSubject{}, err
	}
	if err := r.wfSave(wfSubjectKey(pool, subject), rec); err != nil {
		return WorkforcePoolSubject{}, err
	}
	return wfSubjectView(rec), nil
}

// ---- federation: what the Security Token Service and IAM read

// workforceProvider is the live provider a workforce audience names
// ("//iam.googleapis.com/locations/global/workforcePools/P/providers/R").
func (r *Registry) workforceProvider(audience string) (wif.Provider, bool) {
	rest := strings.TrimPrefix(strings.TrimPrefix(audience, "https:"), "//iam.googleapis.com/")
	parts := strings.Split(rest, "/")
	if len(parts) != 6 || parts[0] != "locations" || parts[1] != "global" || parts[2] != "workforcePools" || parts[4] != "providers" {
		return wif.Provider{}, false
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	pool, err := r.wfPool(parts[3])
	if err != nil || pool.Deleted || pool.PoolV.Disabled {
		return wif.Provider{}, false
	}
	pr, err := r.wfProvider(parts[3], parts[5])
	if err != nil || pr.Deleted || pr.ProviderV.Disabled {
		return wif.Provider{}, false
	}
	ttl := wfDefaultSession
	if d, ok := parseWFDuration(pool.PoolV.SessionDuration); ok && d > 0 {
		ttl = d
	}
	v := pr.ProviderV
	out := wif.Provider{Pool: parts[3], ID: parts[5], Workforce: true, Mapping: v.AttributeMapping, Condition: v.AttributeCondition, SessionTTL: ttl,
		ProgrammaticDisabled: pool.PoolV.AccessRestrictions != nil && pool.PoolV.AccessRestrictions.DisableProgrammaticSignin}
	switch {
	case v.Oidc != nil:
		out.Kind = wif.KindOIDC
		out.IssuerURI, out.ClientID, out.JWKSJSON = v.Oidc.IssuerURI, v.Oidc.ClientID, v.Oidc.JwksJSON
		out.AllowedAudiences = []string{v.Oidc.ClientID}
	case v.Saml != nil:
		out.Kind = wif.KindSAML
		out.SAMLMetadata = v.Saml.IdpMetadataXML
		out.DecryptionKeys = r.liveDecryptionKeys(wfKeyPrefix + parts[3] + "/" + parts[5] + "/")
	}
	return out, true
}

// recordWorkforceSubject makes the subject on its first exchange; a
// deleted one is ErrSubjectDeleted.
func (r *Registry) recordWorkforceSubject(pool, subject string) error {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec := r.wfSubject(pool, subject)
	if rec != nil {
		if rec.Deleted {
			return ErrSubjectDeleted
		}
		return nil
	}
	return r.wfSave(wfSubjectKey(pool, subject), &wfRecord{Pool: pool, Subject: subject, CreateTime: r.now().UTC()})
}

// workforcePrincipalActive reports whether a workforce principal:// or
// principalSet:// identifier's pool is live and, for a subject, the
// subject is not deleted.
func (r *Registry) workforcePrincipalActive(principal string) bool {
	pool, subject, ok := wif.WorkforcePoolOfPrincipal(principal)
	if !ok {
		return false
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfPool(pool)
	if err != nil || rec.Deleted || rec.PoolV.Disabled {
		return false
	}
	if subject != "" {
		if s := r.wfSubject(pool, subject); s != nil && s.Deleted {
			return false
		}
	}
	return true
}

// FederatedSessionLive reports whether a token issued at issued to a
// federated principal still grants: a workforce subject's tokens issued
// before its sessions were revoked do not.
func (r *Registry) FederatedSessionLive(principal string, issued time.Time) bool {
	pool, subject, ok := wif.WorkforcePoolOfPrincipal(principal)
	if !ok || subject == "" {
		return true
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	s := r.wfSubject(pool, subject)
	return s == nil || s.RevokeTime.IsZero() || (!issued.IsZero() && issued.After(s.RevokeTime))
}

// FederationUserProject reports whether an STS exchange's
// options.userProject (an ID or number) names a live project.
func (r *Registry) FederationUserProject(p string) bool {
	_, err := r.wifProject(p)
	return err == nil
}

// ---- REST

// isWorkforcePath is a workforce pools path of the IAM API.
func isWorkforcePath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/locations/")
	if !ok {
		return false
	}
	_, sub, _ := strings.Cut(rest, "/")
	return sub == "workforcePools" || strings.HasPrefix(sub, "workforcePools/") || strings.HasPrefix(sub, "workforcePools:")
}

// ServeWorkforceIdentity serves the IAM API's workforcePools collection,
// its providers, keys, subjects and operations.
func (r *Registry) ServeWorkforceIdentity(w http.ResponseWriter, req *http.Request) {
	if err := r.serveWF(w, req); err != nil {
		rest.WriteError(w, err)
	}
}

func (r *Registry) serveWF(w http.ResponseWriter, req *http.Request) error {
	req.Body = http.MaxBytesReader(w, req.Body, rest.MaxRequestBytes)
	p := strings.TrimPrefix(req.URL.Path, "/v1/")
	verb := ""
	if i := strings.LastIndex(p, ":"); i > strings.LastIndex(p, "/") {
		p, verb = p[:i], p[i+1:]
	}
	seg := strings.Split(p, "/")
	// locations/{l}/workforcePools[/{pool}[/providers[/{prov}[/keys[/{key}]]]|/subjects/{s}]]
	// or .../operations/{op} under any of them.
	if len(seg) < 3 || seg[0] != "locations" || seg[2] != "workforcePools" {
		return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
	}
	if seg[1] != "global" {
		return apierror.InvalidArgument("The only supported location is global; got %q.", seg[1])
	}
	if n := len(seg); n >= 2 && seg[n-2] == "operations" && req.Method == http.MethodGet {
		wifOpsMu.Lock()
		op, ok := wifOps[p]
		wifOpsMu.Unlock()
		if !ok {
			return apierror.NotFound("Requested entity was not found.")
		}
		return rest.WriteJSON(w, http.StatusOK, op)
	}
	if len(seg) >= 5 && seg[4] != "providers" && seg[4] != "subjects" {
		return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
	}
	if len(seg) >= 7 && seg[4] == "providers" && seg[6] == "scimTenants" {
		// SCIM tenants and their tokens (#2065, workforce_scim.go).
		return r.serveWFScim(w, req, seg, verb)
	}
	q := req.URL.Query()
	showDeleted := q.Get("showDeleted") == "true"
	mask := splitWIFMask(q.Get("updateMask"))
	switch len(seg) {
	case 3: // the pools collection
		switch req.Method {
		case http.MethodGet:
			list, err := r.WorkforcePools(q.Get("parent"), showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wfPoolPageDefault, wfPoolPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workforcePools": page, "nextPageToken": next})
		case http.MethodPost:
			var in WorkforcePool
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkforcePool(q.Get("workforcePoolId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wfPoolType, out))
		}
	case 4: // one pool
		pool := seg[3]
		var out WorkforcePool
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.WorkforcePool(pool)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		case req.Method == http.MethodPatch && verb == "":
			var in WorkforcePool
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err = r.UpdateWorkforcePool(pool, in, mask)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.DeleteWorkforcePool(pool)
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.UndeleteWorkforcePool(pool)
		case req.Method == http.MethodPost && isIAMVerb(verb):
			// The pool's own IAM policy (#2065, workforce_iam.go).
			return r.serveWFIAM(w, req, wfResourcePrefix+pool, verb)
		case req.Method == http.MethodPost:
			return apierror.Unimplemented("workforcePools.%s is not implemented by CloudBurrow.", verb)
		default:
			return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wfPoolType, out))
	case 5: // providers collection
		pool := seg[3]
		if seg[4] != "providers" {
			break
		}
		switch req.Method {
		case http.MethodGet:
			list, err := r.WorkforcePoolProviders(pool, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wifProvPageDefault, wifProvPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workforcePoolProviders": page, "nextPageToken": next})
		case http.MethodPost:
			var in WorkforcePoolProvider
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkforcePoolProvider(pool, q.Get("workforcePoolProviderId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wfProviderType, out))
		}
	case 6: // one provider, or one subject
		pool := seg[3]
		if seg[4] == "subjects" {
			subject, err := url.PathUnescape(seg[5])
			if err != nil {
				return apierror.InvalidArgument("invalid subject %q", seg[5])
			}
			var out WorkforcePoolSubject
			typ := wfSubjectType
			switch {
			case req.Method == http.MethodDelete && verb == "":
				out, err = r.DeleteWorkforcePoolSubject(pool, subject)
			case req.Method == http.MethodPost && verb == "undelete":
				out, err = r.UndeleteWorkforcePoolSubject(pool, subject)
				typ = wfSubjectUndelType
			case req.Method == http.MethodPost && verb == "revokeSessions":
				out, err = r.RevokeWorkforcePoolSubjectSessions(pool, subject)
				typ = wfRevokeType
			default:
				return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
			}
			if err != nil {
				return err
			}
			var resp any = map[string]any{}
			if typ == wfSubjectType {
				resp = map[string]any{"name": out.Name}
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, typ, resp))
		}
		prov := seg[5]
		var out WorkforcePoolProvider
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.WorkforcePoolProvider(pool, prov)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		case req.Method == http.MethodPatch && verb == "":
			var in WorkforcePoolProvider
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err = r.UpdateWorkforcePoolProvider(pool, prov, in, mask)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.DeleteWorkforcePoolProvider(pool, prov)
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.UndeleteWorkforcePoolProvider(pool, prov)
		default:
			return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wfProviderType, out))
	case 7: // keys collection
		if seg[4] != "providers" || seg[6] != "keys" {
			break
		}
		pool, prov := seg[3], seg[5]
		switch req.Method {
		case http.MethodGet:
			list, err := r.WorkforcePoolProviderKeys(pool, prov, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wifKeyPageMax, wifKeyPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workforcePoolProviderKeys": page, "nextPageToken": next})
		case http.MethodPost:
			var in WorkloadIdentityPoolProviderKey
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkforcePoolProviderKey(pool, prov, q.Get("workforcePoolProviderKeyId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wfKeyType, out))
		}
	case 8: // one key
		if seg[4] != "providers" || seg[6] != "keys" {
			break
		}
		pool, prov, key := seg[3], seg[5], seg[7]
		var out WorkloadIdentityPoolProviderKey
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.wfKeyGet(pool, prov, key)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.setWFKeyDeleted(pool, prov, key, true)
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.setWFKeyDeleted(pool, prov, key, false)
		default:
			return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, wifOperation(out.Name, wfKeyType, out))
	}
	return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
}
