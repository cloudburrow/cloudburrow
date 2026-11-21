package resourcemanager

// SCIM tenants and their tokens (#2065): the IAM API's
// locations.workforcePools.providers.scimTenants and .scimTenants.tokens
// (Gemini Enterprise and Looker only on Google), served beside the
// workforce pools (workforce.go). Behaviour follows the IAM v1 reference
// (google.golang.org/api/iam/v1's discovery document, and
// docs.cloud.google.com/iam/docs/reference/rest/v1/locations.workforcePools.providers.scimTenants),
// "Configure SCIM in OIDC or SAML IdP" (.../iam/docs/configure-scim-oidc-saml)
// and "Configure SCIM in Okta" (.../iam/docs/configure-scim-okta), read
// 2026-11-20:
//
//   - a tenant ID is 4 to 32 characters of [a-z0-9-], not starting gcp- (gcloud's help); a pool has one SCIM
//     tenant, under one of its providers; claimMapping is required and
//     immutable, with google.subject (required) and google.group, each a
//     CEL expression over the SCIM `user` and `group`; display name at
//     most 32 characters, description 256;
//   - the tenant's baseUri is https://iamscim.googleapis.com/v1alpha1/tenants/UID
//     and its service agent o-ORGANIZATION_ID-UID@gcp-sa-iamscim.iam.gserviceaccount.com;
//     the SCIM protocol endpoint itself is served at /v1alpha1/tenants/UID
//     on the IAM API's port (#2068, workforce_scimapi.go);
//   - delete is soft (DELETED, purgeTime 30 days on; the ID and the pool's
//     one tenant not reusable until then), or immediate with hardDelete;
//     a deleted tenant is listed with showDeleted and cannot be updated;
//   - a token ID is 4 to 32 characters, ^[a-z][a-z0-9-]{2,30}[a-z0-9]$; a
//     tenant has at most two tokens; securityToken is answered by create
//     only; delete is soft (DELETED), a deleted token cannot be updated,
//     and tokens have no undelete method;
//   - every method answers the resource itself, not an Operation.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

const (
	wfScimTenantPrefix   = "iam/wfscimtenants/"
	wfScimTokenPrefix    = "iam/wfscimtokens/"
	wfScimTenantPageDef  = 50
	wfScimTenantPageMax  = 100
	wfScimTokenPageDef   = 2
	wfScimTokenPageMax   = 100
	wfScimTokensMax      = 2
	wfScimBaseURIPrefix  = "https://iamscim.googleapis.com/v1alpha1/tenants/"
	wfScimServiceAgentAt = "@gcp-sa-iamscim.iam.gserviceaccount.com"
)

var wfScimTokenIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,30}[a-z0-9]$`)

// WorkforcePoolProviderScimTenant is a SCIM tenant, as the API renders it.
type WorkforcePoolProviderScimTenant struct {
	Name         string            `json:"name,omitempty"`
	DisplayName  string            `json:"displayName,omitempty"`
	Description  string            `json:"description,omitempty"`
	State        string            `json:"state,omitempty"`
	BaseURI      string            `json:"baseUri,omitempty"`
	PurgeTime    string            `json:"purgeTime,omitempty"`
	ServiceAgent string            `json:"serviceAgent,omitempty"`
	ClaimMapping map[string]string `json:"claimMapping,omitempty"`
}

// WorkforcePoolProviderScimToken is a SCIM token, as the API renders it.
type WorkforcePoolProviderScimToken struct {
	Name          string `json:"name,omitempty"`
	DisplayName   string `json:"displayName,omitempty"`
	State         string `json:"state,omitempty"`
	SecurityToken string `json:"securityToken,omitempty"`
}

// wfScimRecord is what the store keeps of a tenant or a token.
type wfScimRecord struct {
	Pool       string                           `json:"pool"`
	Provider   string                           `json:"provider"`
	Tenant     string                           `json:"tenant"`
	Token      string                           `json:"token,omitempty"`
	UID        string                           `json:"uid,omitempty"`
	Deleted    bool                             `json:"deleted,omitempty"`
	DeleteTime time.Time                        `json:"deleteTime,omitempty"`
	TenantV    *WorkforcePoolProviderScimTenant `json:"tenantValue,omitempty"`
	TokenV     *WorkforcePoolProviderScimToken  `json:"tokenValue,omitempty"`
	// Thumbprint is the token's SHA-256: the security token itself is
	// never stored after create answers it.
	Thumbprint string `json:"thumbprint,omitempty"`
}

func wfScimTenantKey(pool, prov, tenant string) string {
	return wfScimTenantPrefix + pool + "/" + prov + "/" + tenant
}

func wfScimTokenKey(pool, prov, tenant, token string) string {
	return wfScimTokenPrefix + pool + "/" + prov + "/" + tenant + "/" + token
}

func wfScimTenantName(pool, prov, tenant string) string {
	return "locations/global/workforcePools/" + pool + "/providers/" + prov + "/scimTenants/" + tenant
}

func (r *Registry) wfScimLoad(key string) *wfScimRecord {
	b, err := r.st.Get(key)
	if err != nil {
		return nil
	}
	var rec wfScimRecord
	if json.Unmarshal(b, &rec) != nil {
		return nil
	}
	if rec.Deleted && r.now().Sub(rec.DeleteTime) >= wifUndeletePeriod {
		_ = r.st.Delete(key)
		return nil
	}
	return &rec
}

func (r *Registry) wfScimSave(key string, rec *wfScimRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return apierror.Internal(err, "encode the SCIM record")
	}
	if err := r.st.Put(key, b); err != nil {
		return apierror.Internal(err, "store the SCIM record")
	}
	return nil
}

// wfScimTenantView renders a tenant with its output-only fields.
func wfScimTenantView(rec *wfScimRecord) WorkforcePoolProviderScimTenant {
	v := *rec.TenantV
	v.Name = wfScimTenantName(rec.Pool, rec.Provider, rec.Tenant)
	v.BaseURI = wfScimBaseURIPrefix + rec.UID
	org := strings.TrimPrefix(OrganizationName, "organizations/")
	v.ServiceAgent = "o-" + org + "-" + rec.UID + wfScimServiceAgentAt
	v.State, v.PurgeTime = "ACTIVE", ""
	if rec.Deleted {
		v.State = "DELETED"
		v.PurgeTime = rec.DeleteTime.Add(wifUndeletePeriod).UTC().Format(time.RFC3339Nano)
	}
	return v
}

func wfScimTokenView(rec *wfScimRecord) WorkforcePoolProviderScimToken {
	v := *rec.TokenV
	v.Name = wfScimTenantName(rec.Pool, rec.Provider, rec.Tenant) + "/tokens/" + rec.Token
	v.SecurityToken = ""
	v.State = "ACTIVE"
	if rec.Deleted {
		v.State = "DELETED"
	}
	return v
}

var (
	scimEnvOnce sync.Once
	scimEnv     *cel.Env
	scimEnvErr  error
)

// The claim mapping's documented forms ("Configure SCIM in OIDC or SAML
// IdP", read 2026-11-20): google.subject is user.externalId, user.userName
// or user.emails[0].value, the last two optionally .lowerAscii() (the only
// case folding offered, and only on those two); google.group is
// group.externalId or group.displayName; google.display_name,
// google.profile_photo, google.email and google.posix_username are any
// expression over `user`; and at most 50 attribute.KEY rules of at most
// 256 characters each.
var (
	scimSubjectExpr = regexp.MustCompile(`^user\.(externalId|(userName|emails\[0\]\.value)(\.lowerAscii\(\))?)$`)
	scimGroupExpr   = regexp.MustCompile(`^group\.(externalId|displayName)$`)
	scimAttrKey     = regexp.MustCompile(`^attribute\.[a-z_][a-z0-9_]{0,99}$`)
)

const (
	scimMaxCustomRules = 50
	scimMaxRuleChars   = 256
)

func scimCELEnv() (*cel.Env, error) {
	scimEnvOnce.Do(func() {
		scimEnv, scimEnvErr = cel.NewEnv(cel.Variable("user", cel.DynType), cel.Variable("group", cel.DynType), ext.Strings())
	})
	return scimEnv, scimEnvErr
}

// checkScimClaimMapping validates a tenant's claim mapping: google.subject
// is required, every expression a CEL expression over `user` or `group` in
// the documented forms.
func checkScimClaimMapping(m map[string]string) error {
	if m["google.subject"] == "" {
		return apierror.InvalidArgument("claim_mapping is required and must map google.subject.")
	}
	env, err := scimCELEnv()
	if err != nil {
		return apierror.Internal(err, "SCIM CEL environment")
	}
	custom := 0
	for k, v := range m {
		switch {
		case k == "google.subject":
			if !scimSubjectExpr.MatchString(strings.TrimSpace(v)) {
				return apierror.InvalidArgument("claim_mapping[\"google.subject\"] must be user.externalId, user.userName or user.emails[0].value (the last two optionally with .lowerAscii()); got %q.", v)
			}
		case k == "google.group":
			if !scimGroupExpr.MatchString(strings.TrimSpace(v)) {
				return apierror.InvalidArgument("claim_mapping[\"google.group\"] must be group.externalId or group.displayName; got %q.", v)
			}
		case k == "google.display_name", k == "google.profile_photo", k == "google.email", k == "google.posix_username":
		case strings.HasPrefix(k, "attribute."):
			if !scimAttrKey.MatchString(k) {
				return apierror.InvalidArgument("claim_mapping key %q is not a valid custom attribute: attribute.KEY, KEY of [a-z0-9_] starting with a letter or _.", k)
			}
			custom++
		default:
			return apierror.InvalidArgument("claim_mapping key %q is not supported: the keys are google.subject, google.group, google.display_name, google.profile_photo, google.email, google.posix_username and attribute.KEY.", k)
		}
		if len(v) > scimMaxRuleChars {
			return apierror.InvalidArgument("claim_mapping[%q] is longer than %d characters.", k, scimMaxRuleChars)
		}
		if _, iss := env.Compile(v); iss.Err() != nil {
			return apierror.InvalidArgument("claim_mapping[%q] is not a valid CEL expression: %v", k, iss.Err())
		}
	}
	if custom > scimMaxCustomRules {
		return apierror.InvalidArgument("claim_mapping has %d custom attribute rules: at most %d are allowed.", custom, scimMaxCustomRules)
	}
	return nil
}

func scimThumbprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newScimUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// liveWFProvider checks that a pool and its provider exist and are not
// deleted. The caller holds wifMu.
func (r *Registry) liveWFProvider(pool, prov string) error {
	p, err := r.wfPool(pool)
	if err != nil {
		return err
	}
	if p.Deleted {
		return apierror.FailedPrecondition("The workforce pool is deleted.")
	}
	pr, err := r.wfProvider(pool, prov)
	if err != nil {
		return err
	}
	if pr.Deleted {
		return apierror.FailedPrecondition("The workforce pool provider is deleted.")
	}
	return nil
}

// CreateWorkforcePoolProviderScimTenant creates a pool's SCIM tenant.
func (r *Registry) CreateWorkforcePoolProviderScimTenant(pool, prov, id string, in WorkforcePoolProviderScimTenant) (WorkforcePoolProviderScimTenant, error) {
	if err := checkWIFID("SCIM tenant", id, true); err != nil {
		return in, err
	}
	if err := checkWIFText(in.DisplayName, in.Description); err != nil {
		return in, err
	}
	if err := checkScimClaimMapping(in.ClaimMapping); err != nil {
		return in, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	if err := r.liveWFProvider(pool, prov); err != nil {
		return in, err
	}
	if r.wfScimLoad(wfScimTenantKey(pool, prov, id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	// One SCIM tenant per pool, deleted ones included until purged.
	keys, err := r.st.List(wfScimTenantPrefix + pool + "/")
	if err != nil {
		return in, apierror.Internal(err, "list SCIM tenants")
	}
	for _, k := range keys {
		if rec := r.wfScimLoad(k); rec != nil && rec.TenantV != nil {
			return in, apierror.FailedPrecondition("The workforce pool %s already has a SCIM tenant (%s): a pool supports only one.", pool,
				wfScimTenantName(rec.Pool, rec.Provider, rec.Tenant))
		}
	}
	v := WorkforcePoolProviderScimTenant{DisplayName: in.DisplayName, Description: in.Description, ClaimMapping: in.ClaimMapping}
	rec := &wfScimRecord{Pool: pool, Provider: prov, Tenant: id, UID: newScimUID(), TenantV: &v}
	if err := r.wfScimSave(wfScimTenantKey(pool, prov, id), rec); err != nil {
		return in, err
	}
	return wfScimTenantView(rec), nil
}

func (r *Registry) wfScimTenant(pool, prov, id string) (*wfScimRecord, error) {
	rec := r.wfScimLoad(wfScimTenantKey(pool, prov, id))
	if rec == nil || rec.TenantV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// WorkforcePoolProviderScimTenant reads a tenant, deleted or not.
func (r *Registry) WorkforcePoolProviderScimTenant(pool, prov, id string) (WorkforcePoolProviderScimTenant, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfScimTenant(pool, prov, id)
	if err != nil {
		return WorkforcePoolProviderScimTenant{}, err
	}
	return wfScimTenantView(rec), nil
}

// WorkforcePoolProviderScimTenants lists a provider's tenants.
func (r *Registry) WorkforcePoolProviderScimTenants(pool, prov string, showDeleted bool) ([]WorkforcePoolProviderScimTenant, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wfProvider(pool, prov); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wfScimTenantPrefix + pool + "/" + prov + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list SCIM tenants")
	}
	sort.Strings(keys)
	var out []WorkforcePoolProviderScimTenant
	for _, k := range keys {
		rec := r.wfScimLoad(k)
		if rec == nil || rec.TenantV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		out = append(out, wfScimTenantView(rec))
	}
	return out, nil
}

// UpdateWorkforcePoolProviderScimTenant patches a live tenant's display
// name and description; claimMapping is immutable.
func (r *Registry) UpdateWorkforcePoolProviderScimTenant(pool, prov, id string, in WorkforcePoolProviderScimTenant, mask []string) (WorkforcePoolProviderScimTenant, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfScimTenant(pool, prov, id)
	if err != nil {
		return in, err
	}
	if rec.Deleted {
		return in, apierror.FailedPrecondition("The SCIM tenant is deleted and cannot be updated.")
	}
	if len(mask) == 0 {
		return in, apierror.InvalidArgument("update_mask is required.")
	}
	v := *rec.TenantV
	for _, f := range mask {
		switch f {
		case "displayName", "display_name":
			v.DisplayName = in.DisplayName
		case "description":
			v.Description = in.Description
		case "claimMapping", "claim_mapping":
			return in, apierror.InvalidArgument("claim_mapping is immutable: hard-delete the SCIM tenant and create a new one to change it.")
		default:
			return in, apierror.InvalidArgument("update_mask field %q is not supported: the fields are display_name and description.", f)
		}
	}
	if err := checkWIFText(v.DisplayName, v.Description); err != nil {
		return in, err
	}
	rec.TenantV = &v
	if err := r.wfScimSave(wfScimTenantKey(pool, prov, id), rec); err != nil {
		return in, err
	}
	return wfScimTenantView(rec), nil
}

// DeleteWorkforcePoolProviderScimTenant soft-deletes a tenant, or removes
// it and its tokens at once with hard.
func (r *Registry) DeleteWorkforcePoolProviderScimTenant(pool, prov, id string, hard bool) (WorkforcePoolProviderScimTenant, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfScimTenant(pool, prov, id)
	if err != nil {
		return WorkforcePoolProviderScimTenant{}, err
	}
	if rec.Deleted && !hard {
		return WorkforcePoolProviderScimTenant{}, apierror.FailedPrecondition("The SCIM tenant is already deleted.")
	}
	if !rec.Deleted {
		rec.Deleted, rec.DeleteTime = true, r.now().UTC()
	}
	view := wfScimTenantView(rec)
	if hard {
		tokens, err := r.st.List(wfScimTokenPrefix + pool + "/" + prov + "/" + id + "/")
		if err != nil {
			return view, apierror.Internal(err, "list SCIM tokens")
		}
		for _, k := range tokens {
			_ = r.st.Delete(k)
		}
		r.scimPurge(rec.UID)
		if err := r.st.Delete(wfScimTenantKey(pool, prov, id)); err != nil {
			return view, apierror.Internal(err, "delete the SCIM tenant")
		}
		return view, nil
	}
	if err := r.wfScimSave(wfScimTenantKey(pool, prov, id), rec); err != nil {
		return view, err
	}
	return view, nil
}

// UndeleteWorkforcePoolProviderScimTenant restores a tenant deleted fewer
// than 30 days ago.
func (r *Registry) UndeleteWorkforcePoolProviderScimTenant(pool, prov, id string) (WorkforcePoolProviderScimTenant, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfScimTenant(pool, prov, id)
	if err != nil {
		return WorkforcePoolProviderScimTenant{}, err
	}
	if !rec.Deleted {
		return WorkforcePoolProviderScimTenant{}, apierror.FailedPrecondition("The SCIM tenant is not deleted.")
	}
	rec.Deleted, rec.DeleteTime = false, time.Time{}
	if err := r.wfScimSave(wfScimTenantKey(pool, prov, id), rec); err != nil {
		return WorkforcePoolProviderScimTenant{}, err
	}
	return wfScimTenantView(rec), nil
}

// ---- tokens

// CreateWorkforcePoolProviderScimToken makes a token for a live tenant;
// only this answer carries its securityToken.
func (r *Registry) CreateWorkforcePoolProviderScimToken(pool, prov, tenant, id string, in WorkforcePoolProviderScimToken) (WorkforcePoolProviderScimToken, error) {
	if !wfScimTokenIDPattern.MatchString(id) {
		return in, apierror.InvalidArgument("The SCIM token ID must be 4 to 32 characters, start with a letter, end with a letter or digit, and contain only [a-z0-9-]; got %q.", id)
	}
	if err := checkWIFText(in.DisplayName, ""); err != nil {
		return in, err
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	t, err := r.wfScimTenant(pool, prov, tenant)
	if err != nil {
		return in, err
	}
	if t.Deleted {
		return in, apierror.FailedPrecondition("The SCIM tenant is deleted.")
	}
	if r.wfScimLoad(wfScimTokenKey(pool, prov, tenant, id)) != nil {
		return in, apierror.AlreadyExists("Requested entity already exists")
	}
	keys, err := r.st.List(wfScimTokenPrefix + pool + "/" + prov + "/" + tenant + "/")
	if err != nil {
		return in, apierror.Internal(err, "list SCIM tokens")
	}
	active := 0
	for _, k := range keys {
		if rec := r.wfScimLoad(k); rec != nil && rec.TokenV != nil && !rec.Deleted {
			active++
		}
	}
	if active >= wfScimTokensMax {
		return in, apierror.FailedPrecondition("A SCIM tenant has at most %d tokens: delete one before creating another.", wfScimTokensMax)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return in, apierror.Internal(err, "generate the security token")
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	v := WorkforcePoolProviderScimToken{DisplayName: in.DisplayName}
	rec := &wfScimRecord{Pool: pool, Provider: prov, Tenant: tenant, Token: id, TokenV: &v, Thumbprint: scimThumbprint(secret)}
	if err := r.wfScimSave(wfScimTokenKey(pool, prov, tenant, id), rec); err != nil {
		return in, err
	}
	out := wfScimTokenView(rec)
	out.SecurityToken = secret
	return out, nil
}

func (r *Registry) wfScimToken(pool, prov, tenant, id string) (*wfScimRecord, error) {
	rec := r.wfScimLoad(wfScimTokenKey(pool, prov, tenant, id))
	if rec == nil || rec.TokenV == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return rec, nil
}

// WorkforcePoolProviderScimToken reads a token, without its secret.
func (r *Registry) WorkforcePoolProviderScimToken(pool, prov, tenant, id string) (WorkforcePoolProviderScimToken, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfScimToken(pool, prov, tenant, id)
	if err != nil {
		return WorkforcePoolProviderScimToken{}, err
	}
	return wfScimTokenView(rec), nil
}

// WorkforcePoolProviderScimTokens lists a tenant's tokens.
func (r *Registry) WorkforcePoolProviderScimTokens(pool, prov, tenant string, showDeleted bool) ([]WorkforcePoolProviderScimToken, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if _, err := r.wfScimTenant(pool, prov, tenant); err != nil {
		return nil, err
	}
	keys, err := r.st.List(wfScimTokenPrefix + pool + "/" + prov + "/" + tenant + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list SCIM tokens")
	}
	sort.Strings(keys)
	var out []WorkforcePoolProviderScimToken
	for _, k := range keys {
		rec := r.wfScimLoad(k)
		if rec == nil || rec.TokenV == nil || (rec.Deleted && !showDeleted) {
			continue
		}
		out = append(out, wfScimTokenView(rec))
	}
	return out, nil
}

// UpdateWorkforcePoolProviderScimToken patches a live token's display name.
func (r *Registry) UpdateWorkforcePoolProviderScimToken(pool, prov, tenant, id string, in WorkforcePoolProviderScimToken, mask []string) (WorkforcePoolProviderScimToken, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	if t, err := r.wfScimTenant(pool, prov, tenant); err != nil {
		return in, err
	} else if t.Deleted {
		return in, apierror.FailedPrecondition("The SCIM tenant is deleted.")
	}
	rec, err := r.wfScimToken(pool, prov, tenant, id)
	if err != nil {
		return in, err
	}
	if rec.Deleted {
		return in, apierror.FailedPrecondition("The SCIM token is deleted and cannot be updated.")
	}
	if len(mask) == 0 {
		return in, apierror.InvalidArgument("update_mask is required.")
	}
	v := *rec.TokenV
	for _, f := range mask {
		switch f {
		case "displayName", "display_name":
			v.DisplayName = in.DisplayName
		default:
			return in, apierror.InvalidArgument("update_mask field %q is not supported: the field is display_name.", f)
		}
	}
	if err := checkWIFText(v.DisplayName, ""); err != nil {
		return in, err
	}
	rec.TokenV = &v
	if err := r.wfScimSave(wfScimTokenKey(pool, prov, tenant, id), rec); err != nil {
		return in, err
	}
	return wfScimTokenView(rec), nil
}

// DeleteWorkforcePoolProviderScimToken soft-deletes a token.
func (r *Registry) DeleteWorkforcePoolProviderScimToken(pool, prov, tenant, id string) (WorkforcePoolProviderScimToken, error) {
	wifMu.Lock()
	defer wifMu.Unlock()
	rec, err := r.wfScimToken(pool, prov, tenant, id)
	if err != nil {
		return WorkforcePoolProviderScimToken{}, err
	}
	if rec.Deleted {
		return WorkforcePoolProviderScimToken{}, apierror.FailedPrecondition("The SCIM token is already deleted.")
	}
	rec.Deleted, rec.DeleteTime = true, r.now().UTC()
	if err := r.wfScimSave(wfScimTokenKey(pool, prov, tenant, id), rec); err != nil {
		return WorkforcePoolProviderScimToken{}, err
	}
	return wfScimTokenView(rec), nil
}

// ScimTokenLive reports whether secret is a live token of a live tenant:
// what the SCIM endpoint checks (workforce_scimapi.go).
func (r *Registry) ScimTokenLive(pool, prov, tenant, secret string) bool {
	wifMu.Lock()
	defer wifMu.Unlock()
	t, err := r.wfScimTenant(pool, prov, tenant)
	if err != nil || t.Deleted {
		return false
	}
	keys, err := r.st.List(wfScimTokenPrefix + pool + "/" + prov + "/" + tenant + "/")
	if err != nil {
		return false
	}
	want := scimThumbprint(secret)
	for _, k := range keys {
		if rec := r.wfScimLoad(k); rec != nil && rec.TokenV != nil && !rec.Deleted && rec.Thumbprint == want {
			return true
		}
	}
	return false
}

// ---- REST

// serveWFScim serves .../providers/{prov}/scimTenants[/{t}[/tokens[/{tok}]]];
// seg is the path split at "/" with the :verb removed.
func (r *Registry) serveWFScim(w http.ResponseWriter, req *http.Request, seg []string, verb string) error {
	pool, prov := seg[3], seg[5]
	q := req.URL.Query()
	showDeleted := q.Get("showDeleted") == "true"
	mask := splitWIFMask(q.Get("updateMask"))
	notFound := apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
	switch len(seg) {
	case 7: // tenants collection
		switch {
		case req.Method == http.MethodGet && verb == "":
			list, err := r.WorkforcePoolProviderScimTenants(pool, prov, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wfScimTenantPageDef, wfScimTenantPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workforcePoolProviderScimTenants": page, "nextPageToken": next})
		case req.Method == http.MethodPost && verb == "":
			var in WorkforcePoolProviderScimTenant
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkforcePoolProviderScimTenant(pool, prov, q.Get("workforcePoolProviderScimTenantId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		}
	case 8: // one tenant
		tenant := seg[7]
		var out WorkforcePoolProviderScimTenant
		var err error
		switch {
		case req.Method == http.MethodPost && (verb == "getIamPolicy" || verb == "setIamPolicy" || verb == "testIamPermissions"):
			return r.serveWFIAM(w, req, wfScimTenantName(pool, prov, tenant), verb)
		case req.Method == http.MethodGet && verb == "":
			out, err = r.WorkforcePoolProviderScimTenant(pool, prov, tenant)
		case req.Method == http.MethodPatch && verb == "":
			var in WorkforcePoolProviderScimTenant
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err = r.UpdateWorkforcePoolProviderScimTenant(pool, prov, tenant, in, mask)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.DeleteWorkforcePoolProviderScimTenant(pool, prov, tenant, q.Get("hardDelete") == "true")
		case req.Method == http.MethodPost && verb == "undelete":
			out, err = r.UndeleteWorkforcePoolProviderScimTenant(pool, prov, tenant)
		default:
			return notFound
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, out)
	case 9: // tokens collection
		if seg[8] != "tokens" {
			break
		}
		tenant := seg[7]
		switch {
		case req.Method == http.MethodGet && verb == "":
			list, err := r.WorkforcePoolProviderScimTokens(pool, prov, tenant, showDeleted)
			if err != nil {
				return err
			}
			page, next, err := wifPage(list, q, wfScimTokenPageDef, wfScimTokenPageMax)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, map[string]any{"workforcePoolProviderScimTokens": page, "nextPageToken": next})
		case req.Method == http.MethodPost && verb == "":
			var in WorkforcePoolProviderScimToken
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err := r.CreateWorkforcePoolProviderScimToken(pool, prov, tenant, q.Get("workforcePoolProviderScimTokenId"), in)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, out)
		}
	case 10: // one token
		if seg[8] != "tokens" {
			break
		}
		tenant, token := seg[7], seg[9]
		var out WorkforcePoolProviderScimToken
		var err error
		switch {
		case req.Method == http.MethodGet && verb == "":
			out, err = r.WorkforcePoolProviderScimToken(pool, prov, tenant, token)
		case req.Method == http.MethodPatch && verb == "":
			var in WorkforcePoolProviderScimToken
			if err := decodeWIF(req, &in); err != nil {
				return err
			}
			out, err = r.UpdateWorkforcePoolProviderScimToken(pool, prov, tenant, token, in, mask)
		case req.Method == http.MethodDelete && verb == "":
			out, err = r.DeleteWorkforcePoolProviderScimToken(pool, prov, tenant, token)
		default:
			return notFound
		}
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, out)
	}
	return notFound
}
