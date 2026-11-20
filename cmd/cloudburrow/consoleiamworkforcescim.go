package main

// IAM & Admin > Workforce Identity Federation (#2067): a pool's
// Permissions tab (its own IAM policy: Grant access, Remove principal,
// Test permissions, through the shared console Permissions tab), and a
// provider's SCIM tab: its SCIM tenant (Create SCIM tenant with the claim
// mapping, Edit, Delete, Restore, Delete permanently), the tenant's page
// with its tokens (Create token, its security token shown once in the
// answer and never again; Delete) and its own Permissions tab, over the
// registry the IAM API serves (internal/service/resourcemanager's
// workforce_scim.go and workforce_iam.go).

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

const (
	wfScimRow  = "scim:"
	wfTokenRow = "tok:"
	// wfScimUsageNone and the others are the provider's SCIM usage, as
	// the Edit provider form offers it.
	wfScimUsageNone        = "Not used"
	wfScimUsageGroups      = "Groups (ENABLED_FOR_GROUPS)"
	wfScimUsageUsersGroups = "Users and groups (ENABLED_FOR_USERS_GROUPS)"
)

func wfScimUsageLabel(v string) string {
	switch v {
	case "ENABLED_FOR_GROUPS":
		return wfScimUsageGroups
	case "ENABLED_FOR_USERS_GROUPS":
		return wfScimUsageUsersGroups
	}
	return wfScimUsageNone
}

func wfScimUsageValue(label string) string {
	switch label {
	case wfScimUsageGroups:
		return "ENABLED_FOR_GROUPS"
	case wfScimUsageUsersGroups:
		return "ENABLED_FOR_USERS_GROUPS"
	}
	return "SCIM_USAGE_UNSPECIFIED"
}

const wfScimNote = "SCIM provisioning (Gemini Enterprise and Looker on Google): the identity provider pushes users and groups to the tenant's " +
	"SCIM 2.0 endpoint, authenticated with one of its tokens. A pool has one SCIM tenant. When the provider's SCIM usage is set, " +
	"IAM reads group memberships (and, for users and groups, custom attributes) from SCIM instead of the sign-in token. " +
	"Locally the endpoint is served on the IAM API's port at /v1alpha1/tenants/UID."

// wfScimSection is a provider's SCIM tab: its tenants, deleted ones too.
func (p iamWorkforceIdentityProvider) wfScimSection(ctx context.Context, project, pool, prov string) console.Section {
	l := console.Listing{NameColumn: "Tenant ID", Columns: []string{"Display name", "Base URI", "Service agent"}, Noun: "SCIM tenants", Note: wfScimNote}
	list, err := p.registry.WorkforcePoolProviderScimTenants(pool, prov, true)
	if err != nil {
		l.Unavailable = consoleErr(err)
	}
	for _, t := range list {
		r := console.Resource{Name: wfScimRow + lastSegment(t.Name), Status: t.State,
			Fields: map[string]string{"Display name": orDash(t.DisplayName), "Base URI": t.BaseURI, "Service agent": t.ServiceAgent}}
		r.Actions = p.DetailActions(ctx, project, []string{pool, prov, r.Name})
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return console.Section{ID: "scim", Label: "SCIM", Listing: l}
}

// wfScimTenantFields are Create SCIM tenant's fields.
func wfScimTenantFields() []console.Field {
	return []console.Field{
		{Name: "tenantDisplayName", Label: "Display name", Type: "text", Help: "At most 32 characters."},
		{Name: "tenantId", Label: "SCIM tenant ID", Type: "text", Required: true, Pattern: `^[a-z0-9\-]{4,32}$`, DeriveFrom: "tenantDisplayName",
			Help: "4 to 32 lowercase letters, digits and dashes; the prefix gcp- is Google's. It cannot be changed."},
		{Name: "tenantDescription", Label: "Description", Type: "text", Help: "At most 256 characters."},
		{Name: "claimMapping", Label: "Claim mapping", Type: "map", Required: true, Default: "google.subject=user.externalId\ngoogle.group=group.displayName",
			KeyPattern: `^(google\.(subject|group|display_name|profile_photo|email|posix_username)|attribute\.[a-z_][a-z0-9_]{0,99})$`,
			KeyHelp:    "google.subject, google.group, google.display_name, google.profile_photo, google.email, google.posix_username or attribute.KEY",
			Help: "google.subject is user.externalId, user.userName or user.emails[0].value (the last two optionally .lowerAscii()), and must name the " +
				"identity provider attribute the provider's google.subject maps; google.group is group.externalId or group.displayName. " +
				"google.subject and google.group cannot be changed later."},
	}
}

func (p iamWorkforceIdentityProvider) scimTenantDetail(ctx context.Context, project, pool, prov, tenant string) (console.Detail, error) {
	t, err := p.registry.WorkforcePoolProviderScimTenant(pool, prov, tenant)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the SCIM tenant: " + consoleErr(err)}, nil
	}
	uid := strings.TrimPrefix(t.BaseURI, "https://iamscim.googleapis.com/v1alpha1/tenants/")
	props := []console.Property{
		{Label: "SCIM tenant ID", Value: tenant},
		{Label: "Display name", Value: orDash(t.DisplayName)},
		{Label: "Status", Value: t.State},
		{Label: "Description", Value: orDash(t.Description)},
		{Label: "Base URI", Value: t.BaseURI},
		{Label: "Local endpoint path", Value: "/v1alpha1/tenants/" + uid},
		{Label: "Service agent", Value: t.ServiceAgent},
		{Label: "Resource name", Value: t.Name},
	}
	if t.PurgeTime != "" {
		props = append(props, console.Property{Label: "Purged after", Value: t.PurgeTime})
	}
	var claims []console.Property
	for _, k := range mappingKeys(t.ClaimMapping) {
		claims = append(claims, console.Property{Label: k, Value: t.ClaimMapping[k]})
	}
	toks := console.Listing{NameColumn: "Token ID", Columns: []string{"Display name"}, Noun: "SCIM tokens",
		Note: "A tenant has at most two tokens, so one can be rotated without downtime: delete one before creating a third. A token's security token is shown once, when it is created."}
	tl, err := p.registry.WorkforcePoolProviderScimTokens(pool, prov, tenant, true)
	if err != nil {
		toks.Unavailable = consoleErr(err)
	}
	for _, x := range tl {
		r := console.Resource{Name: wfTokenRow + lastSegment(x.Name), Status: x.State, Fields: map[string]string{"Display name": orDash(x.DisplayName)}}
		r.Actions = p.DetailActions(ctx, project, []string{pool, prov, wfScimRow + tenant, r.Name})
		toks.Items = append(toks.Items, r)
	}
	toks.Total = len(toks.Items)
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "SCIM tenant", Properties: props}, {Heading: "Claim mapping", Properties: claims}}},
		{ID: "tokens", Label: "Tokens", Listing: toks},
		{ID: "permissions", Kind: console.KindPermissions},
	}}
	if t.State != "DELETED" {
		d.Edit = &console.EditForm{Label: "Edit SCIM tenant", Note: "The tenant ID and its claim mapping cannot change, as on Google: delete the tenant permanently and create it again.",
			Fields: []console.Field{
				{Name: "tenantDisplayName", Label: "Display name", Type: "text", Default: t.DisplayName, Help: "At most 32 characters."},
				{Name: "tenantDescription", Label: "Description", Type: "text", Default: t.Description, Help: "At most 256 characters."},
			}}
	}
	return d, nil
}

func (p iamWorkforceIdentityProvider) tokenDetail(pool, prov, tenant, token string) (console.Detail, error) {
	x, err := p.registry.WorkforcePoolProviderScimToken(pool, prov, tenant, token)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the SCIM token: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "Token ID", Value: token},
		{Label: "Display name", Value: orDash(x.DisplayName)},
		{Label: "Status", Value: x.State},
		{Label: "Security token", Value: "Shown once, when the token was created"},
		{Label: "Resource name", Value: x.Name},
	}
	return console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "SCIM token", Properties: props}}}}}, nil
}

// scimActions are a provider's Create SCIM tenant, a tenant's and a
// token's actions.
func (p iamWorkforceIdentityProvider) scimActions(path []string) []console.Action {
	pool, prov := path[0], path[1]
	tenant, ok := strings.CutPrefix(path[2], wfScimRow)
	if !ok {
		return nil
	}
	if len(path) == 4 {
		token, ok := strings.CutPrefix(path[3], wfTokenRow)
		if !ok {
			return nil
		}
		x, err := p.registry.WorkforcePoolProviderScimToken(pool, prov, tenant, token)
		if err != nil || x.State == "DELETED" {
			return nil
		}
		return []console.Action{{ID: "delete-token", Label: "Delete", Destructive: true}}
	}
	t, err := p.registry.WorkforcePoolProviderScimTenant(pool, prov, tenant)
	if err != nil {
		return nil
	}
	if t.State == "DELETED" {
		return []console.Action{{ID: "undelete-tenant", Label: "Restore"},
			{ID: "purge-tenant", Label: "Delete permanently", Destructive: true}}
	}
	return []console.Action{
		{ID: "create-token", Label: "Create token", Fields: []console.Field{
			{Name: "tokenDisplayName", Label: "Display name", Type: "text", Help: "At most 32 characters."},
			{Name: "tokenId", Label: "Token ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{2,30}[a-z0-9]$`, DeriveFrom: "tokenDisplayName",
				Help: "4 to 32 lowercase letters, digits and dashes, starting with a letter. The security token is shown once."},
		}},
		{ID: "delete-tenant", Label: "Delete", Destructive: true},
		{ID: "purge-tenant", Label: "Delete permanently", Destructive: true},
	}
}

// actScim performs a provider's, a tenant's or a token's SCIM action.
func (p iamWorkforceIdentityProvider) actScim(path []string, action string, values map[string]string) (*console.Listing, error) {
	pool, prov := path[0], path[1]
	if len(path) == 2 {
		m, err := console.ParseMap(values["claimMapping"])
		if err != nil {
			return nil, err
		}
		_, err = p.registry.CreateWorkforcePoolProviderScimTenant(pool, prov, strings.TrimSpace(values["tenantId"]),
			resourcemanager.WorkforcePoolProviderScimTenant{DisplayName: values["tenantDisplayName"], Description: values["tenantDescription"], ClaimMapping: m})
		return nil, err
	}
	tenant, ok := strings.CutPrefix(path[2], wfScimRow)
	if !ok {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	if len(path) == 4 {
		token, ok := strings.CutPrefix(path[3], wfTokenRow)
		if !ok || action != "delete-token" {
			return nil, fmt.Errorf("unknown action %q", action)
		}
		_, err := p.registry.DeleteWorkforcePoolProviderScimToken(pool, prov, tenant, token)
		return nil, err
	}
	var err error
	switch action {
	case "create-token":
		x, cerr := p.registry.CreateWorkforcePoolProviderScimToken(pool, prov, tenant, strings.TrimSpace(values["tokenId"]),
			resourcemanager.WorkforcePoolProviderScimToken{DisplayName: values["tokenDisplayName"]})
		if cerr != nil {
			return nil, cerr
		}
		r := console.Resource{Name: lastSegment(x.Name), Status: x.State,
			Fields: map[string]string{"Display name": orDash(x.DisplayName), "Security token": x.SecurityToken}}
		return &console.Listing{NameColumn: "Token ID", Columns: []string{"Display name", "Security token"}, Noun: "SCIM tokens",
			Note: "Copy the security token now and give it to the identity provider's SCIM connector: it is not shown again.", Items: []console.Resource{r}, Total: 1}, nil
	case "delete-tenant":
		_, err = p.registry.DeleteWorkforcePoolProviderScimTenant(pool, prov, tenant, false)
	case "purge-tenant":
		_, err = p.registry.DeleteWorkforcePoolProviderScimTenant(pool, prov, tenant, true)
	case "undelete-tenant":
		_, err = p.registry.UndeleteWorkforcePoolProviderScimTenant(pool, prov, tenant)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	return nil, err
}

// ---- Permissions tabs: a pool's and a SCIM tenant's own IAM policy.

// wfPolicyResource is the IAM resource of a pool or tenant page, "" for
// a page without a policy.
func wfPolicyResource(path []string) string {
	switch len(path) {
	case 1:
		return "locations/global/workforcePools/" + path[0]
	case 3:
		if t, ok := strings.CutPrefix(path[2], wfScimRow); ok {
			return "locations/global/workforcePools/" + path[0] + "/providers/" + path[1] + "/scimTenants/" + t
		}
	}
	return ""
}

const wfPermissionsEnforced = "This policy is the IAM API's getIamPolicy and setIamPolicy of the resource. With `up --iam-enforcement=on` it is " +
	"enforced: the resource's methods are checked on its policy, then its pool's and the organization's. Without enforcement it is stored and grants nothing."

func (iamWorkforceIdentityProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if wfPolicyResource(path) == "" {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "workforce-identity-federation-2056", Enforced: wfPermissionsEnforced,
		RoleHelp: roleHelp("IAM", "roles/iam.workforcePoolViewer"),
		PrincipalHelp: "One or more principals, separated by commas or new lines: user:, group:, serviceAccount:, or a workforce pool's " +
			"principal://iam.googleapis.com/locations/global/workforcePools/POOL/subject/SUBJECT or principalSet://…/group/GROUP."}
}

func (p iamWorkforceIdentityProvider) GetPolicy(ctx context.Context, _ string, path []string) (console.Policy, error) {
	return getProtoPolicy(ctx, p.registry.WorkforceIAM(), wfPolicyResource(path))
}

func (p iamWorkforceIdentityProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	return setProtoPolicy(ctx, p.registry.WorkforceIAM(), wfPolicyResource(path), pol)
}

func (p iamWorkforceIdentityProvider) TestPermissions(ctx context.Context, _ string, path []string, perms []string) ([]string, error) {
	return testProtoPermissions(ctx, p.registry.WorkforceIAM(), wfPolicyResource(path), perms)
}

var _ console.PolicyEditor = iamWorkforceIdentityProvider{}
var _ console.ResultActor = iamWorkforceIdentityProvider{}
