package resourcemanager

// SCIM-provisioned users and groups in IAM (#2068). When a workforce
// pool's SCIM tenant sits on a provider whose scimUsage is set, Google
// "treats the synced data as the source of truth for attributes and group
// memberships in IAM policy evaluation" ("SCIM provisioning for Workforce
// Identity Federation", read 2026-11-20):
//
//   - ENABLED_FOR_GROUPS: a subject's groups (principalSet://.../group/G)
//     are the google.group of every SCIM group it is a member of, read at
//     each check; the sign-in token's google.groups are not used. Other
//     attributes still come from the token.
//   - ENABLED_FOR_USERS_GROUPS: as above, and the subject's custom
//     attributes are the tenant's attribute.KEY mappings evaluated on its
//     SCIM user; a subject with no active SCIM user signs in to nothing
//     (the exchange is invalid_grant) and its tokens grant nothing.
//
// A provider with scimUsage set but no live tenant on it fails sign-in, as
// Google documents.

import (
	"errors"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

const (
	scimUsageGroups      = "ENABLED_FOR_GROUPS"
	scimUsageUsersGroups = "ENABLED_FOR_USERS_GROUPS"
)

// scimUsageOf is a provider's scimUsage, "" when SCIM data is not used.
func scimUsageOf(p *WorkforcePoolProvider) string {
	if p == nil {
		return ""
	}
	switch p.ScimUsage {
	case scimUsageGroups, scimUsageUsersGroups:
		return p.ScimUsage
	}
	return ""
}

// checkWFScimUsage refuses scimUsage on a provider when the pool's SCIM
// tenant is under another provider: one provider per pool uses SCIM. The
// caller holds wifMu.
func (r *Registry) checkWFScimUsage(pool, prov string, p *WorkforcePoolProvider) error {
	if scimUsageOf(p) == "" {
		return nil
	}
	if t := r.scimPoolTenant(pool); t != nil && t.Provider != prov {
		return apierror.InvalidArgument("scim_usage cannot be enabled on the provider %s: the pool's SCIM tenant %s is under the provider %s.", prov,
			wfScimTenantName(t.Pool, t.Provider, t.Tenant), t.Provider)
	}
	return nil
}

// scimPoolTenant is a pool's live SCIM tenant, nil for none. The caller
// holds wifMu.
func (r *Registry) scimPoolTenant(pool string) *wfScimRecord {
	keys, err := r.st.List(wfScimTenantPrefix + pool + "/")
	if err != nil {
		return nil
	}
	for _, k := range keys {
		if rec := r.wfScimLoad(k); rec != nil && rec.TenantV != nil && !rec.Deleted {
			return rec
		}
	}
	return nil
}

// scimSubjectView is what SCIM says of a pool's subject.
type scimSubjectView struct {
	usage  string // "" when the pool's SCIM data is not used
	active bool   // an active SCIM user has the subject
	groups []string
	attrs  map[string][]string
}

// scimSubject reads a subject's SCIM user and groups. The caller holds
// wifMu.
func (r *Registry) scimSubject(pool, subject string) scimSubjectView {
	t := r.scimPoolTenant(pool)
	if t == nil {
		return scimSubjectView{}
	}
	pr, err := r.wfProvider(pool, t.Provider)
	if err != nil || pr.Deleted {
		return scimSubjectView{}
	}
	v := scimSubjectView{usage: scimUsageOf(pr.ProviderV)}
	if v.usage == "" {
		return v
	}
	var user *scimResource
	for _, u := range r.scimAll(t.UID, true) {
		if u.Key == subject {
			user = u
		}
	}
	if user == nil || user.Res["active"] == false {
		return v
	}
	v.active = true
	id := scimString(user.Res["id"])
	for _, g := range r.scimAll(t.UID, false) {
		list, _ := g.Res["members"].([]any)
		for _, m := range list {
			if mm, _ := m.(map[string]any); scimString(mm["value"]) == id {
				v.groups = append(v.groups, g.Key)
				break
			}
		}
	}
	sort.Strings(v.groups)
	if v.usage == scimUsageUsersGroups {
		v.attrs = map[string][]string{}
		for k, expr := range t.TenantV.ClaimMapping {
			name, ok := strings.CutPrefix(k, "attribute.")
			if !ok {
				continue
			}
			if val, err := scimEval(expr, user.Res, true); err == nil && val != "" {
				v.attrs[name] = []string{val}
			}
		}
	}
	return v
}

// scimAdjustSets replaces a workforce subject's token-given groups (and,
// for ENABLED_FOR_USERS_GROUPS, custom attributes) with SCIM's; ok is
// false when SCIM says the subject has no access at all.
func (r *Registry) scimAdjustSets(principal string, sets []string) ([]string, bool) {
	pool, subject, isWF := wif.WorkforcePoolOfPrincipal(principal)
	if !isWF || subject == "" {
		return sets, true
	}
	wifMu.Lock()
	v := r.scimSubject(pool, subject)
	wifMu.Unlock()
	if v.usage == "" {
		return sets, true
	}
	if v.usage == scimUsageUsersGroups && !v.active {
		return nil, false
	}
	base := "principalSet://iam.googleapis.com/" + wif.WorkforcePoolName(pool) + "/"
	out := make([]string, 0, len(sets)+len(v.groups))
	for _, s := range sets {
		if strings.HasPrefix(s, base+"group/") {
			continue
		}
		if v.usage == scimUsageUsersGroups && strings.HasPrefix(s, base+"attribute.") {
			continue
		}
		out = append(out, s)
	}
	for _, g := range v.groups {
		out = append(out, base+"group/"+g)
	}
	if v.usage == scimUsageUsersGroups {
		names := make([]string, 0, len(v.attrs))
		for n := range v.attrs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, val := range v.attrs[n] {
				out = append(out, base+"attribute."+n+"/"+val)
			}
		}
	}
	return out, true
}

// WorkforceScimSignIn checks a sign-in through a provider whose scimUsage
// is set: the provider must hold the pool's SCIM tenant, and with
// ENABLED_FOR_USERS_GROUPS the subject must be an active SCIM user. The
// Security Token Service calls it after mapping the token.
func (r *Registry) WorkforceScimSignIn(pool, prov, subject string) error {
	wifMu.Lock()
	defer wifMu.Unlock()
	pr, err := r.wfProvider(pool, prov)
	if err != nil {
		return nil
	}
	usage := scimUsageOf(pr.ProviderV)
	if usage == "" {
		return nil
	}
	t := r.scimPoolTenant(pool)
	if t == nil || t.Provider != prov {
		return errors.New("The provider " + prov + " uses SCIM (scimUsage " + usage + ") but no SCIM tenant is attached to it: sign-in fails.")
	}
	if usage == scimUsageUsersGroups && !r.scimSubject(pool, subject).active {
		return errors.New("The subject " + subject + " is not an active user of the SCIM tenant " + wfScimTenantName(t.Pool, t.Provider, t.Tenant) + ".")
	}
	return nil
}
