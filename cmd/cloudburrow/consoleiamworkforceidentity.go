package main

// IAM & Admin > Workforce Identity Federation (#2056): the organization's
// workforce pools, as Google's console lists them (Display name, Pool ID,
// Status, Description), with Create pool (the pool, its session duration,
// an optional first provider and its attributes), a pool's page with its
// providers and subjects, Add provider (OIDC with issuer, client ID,
// client secret, web sign-in response type and assertion claims, JWK
// file; SAML IdP metadata), a provider's page with Edit provider, Disable,
// Enable, Delete and Restore for pools and providers, and a subject's
// Revoke sessions, Delete and Restore, over the registry the IAM API
// serves, so a pool made here is one gcloud iam workforce-pools sees, and
// the API's refusals are the forms'. Workforce pools are the
// organization's, not a project's: the page shows the instance's
// organization whatever project the toolbar has.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamWorkforceIdentityProvider struct {
	registry *resourcemanager.Registry
}

func (iamWorkforceIdentityProvider) ID() string    { return "iam-workforce-identity" }
func (iamWorkforceIdentityProvider) Title() string { return "Workforce Identity Federation" }

const iamWorkforceIdentityNote = "Workforce pools of the organization " + resourcemanager.OrganizationName + ". A user signs in with the pool's " +
	"identity provider and exchanges its token at this instance's Security Token Service; IAM then sees the user as a " +
	"principal://iam.googleapis.com/locations/global/workforcePools/POOL/subject/SUBJECT member, and its groups and attributes as principalSet:// members. " +
	"A deleted pool or provider can be restored for 30 days."

const (
	wfPoolIDPattern = `^[a-z][a-z0-9\-]{4,61}[a-z0-9]$`
	wfTypeOIDC      = "OpenID Connect (OIDC)"
	wfTypeSAML      = "SAML"
	wfSubjectRow    = "s:"
)

func (p iamWorkforceIdentityProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Pool ID", Columns: []string{"Display name", "Description", "Session duration"}, Noun: "workforce pools", Note: iamWorkforceIdentityNote}
	pools, err := p.registry.WorkforcePools(resourcemanager.OrganizationName, true)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, pool := range pools {
		row := console.Resource{Name: lastSegment(pool.Name), Status: wifStatus(pool.State, pool.Disabled),
			Fields: map[string]string{"Display name": pool.DisplayName, "Description": pool.Description, "Session duration": pool.SessionDuration}}
		row.Actions = p.Actions(row)
		l.Items = append(l.Items, row)
	}
	l.Total = len(l.Items)
	return l, nil
}

// wfProviderFields are Add provider's fields; section and attrs name
// their steps.
func wfProviderFields(section, attrs string, required bool) []console.Field {
	show := func(t string) string { return "providerType=" + t }
	return []console.Field{
		{Name: "providerType", Label: "Select a provider", Type: "select", Section: section, Required: required, Options: []string{wfTypeOIDC, wfTypeSAML}},
		{Name: "providerDisplayName", Label: "Name", Type: "text", Section: section, Help: "At most 32 characters."},
		{Name: "providerId", Label: "Provider ID", Type: "text", Section: section, Pattern: wifIDPattern, DeriveFrom: "providerDisplayName", Required: required,
			Help: "4 to 32 lowercase letters, digits and dashes; the prefix gcp- is Google's. It cannot be changed."},
		{Name: "providerDescription", Label: "Description", Type: "text", Section: section, Help: "At most 256 characters."},
		{Name: "issuerUri", Label: "Issuer (URL)", Type: "text", Section: section, ShowWhen: show(wfTypeOIDC),
			Help: "The OIDC issuer, https://… (a loopback http:// issuer is accepted locally). Its tokens' iss must match."},
		{Name: "clientId", Label: "Client ID", Type: "text", Section: section, ShowWhen: show(wfTypeOIDC),
			Help: "The client ID registered with the identity provider: the tokens' aud must be it."},
		{Name: "clientSecret", Label: "Client secret", Type: "password", Section: section, ShowWhen: show(wfTypeOIDC),
			Help: "Required for the Code flow. It is never shown again, only its thumbprint."},
		{Name: "responseType", Label: "Response type", Type: "select", Section: section, ShowWhen: show(wfTypeOIDC), Options: []string{"Code", "ID token"},
			Help: "The web sign-in flow. With ID token, only the ID token's claims are used."},
		{Name: "assertionClaims", Label: "Assertion claims behavior", Type: "select", Section: section, ShowWhen: show(wfTypeOIDC),
			Options: []string{"Only ID token claims", "User info and ID token"}},
		{Name: "jwksJson", Label: "JWK file (JSON)", Type: "textarea", Section: section, ShowWhen: show(wfTypeOIDC), Upload: "Upload JWK file",
			Help: "Optional: the issuer's public keys as a JSON Web Key Set. Without it, the keys come from the issuer's discovery document."},
		{Name: "idpMetadataXml", Label: "IdP metadata file (XML)", Type: "textarea", Section: section, ShowWhen: show(wfTypeSAML), Upload: "Upload metadata file",
			Help: "The SAML 2.0 identity provider's metadata, with its entity ID and signing certificates."},
		{Name: "attributeMapping", Label: "Attribute mapping", Type: "map", Section: attrs, Required: required,
			KeyPattern: `^(google\.(subject|groups|display_name|profile_photo|posix_username)|attribute\.[a-z0-9_]{1,100})$`,
			KeyHelp:    "google.subject, google.groups, google.display_name, google.profile_photo, google.posix_username or attribute.NAME",
			Help:       "Each key is a Google attribute, each value a CEL expression over assertion, such as google.subject = assertion.sub. google.subject is required."},
		{Name: "attributeCondition", Label: "Attribute condition", Type: "textarea", Section: attrs,
			Help: "Optional CEL condition over assertion, google and attribute that a credential must meet, such as 'admins' in google.groups."},
	}
}

func wfWebSso(v map[string]string) *resourcemanager.WFWebSsoConfig {
	w := &resourcemanager.WFWebSsoConfig{ResponseType: "ID_TOKEN", AssertionClaimsBehavior: "ONLY_ID_TOKEN_CLAIMS"}
	if v["responseType"] == "Code" {
		w.ResponseType = "CODE"
	}
	if v["assertionClaims"] == "User info and ID token" {
		w.AssertionClaimsBehavior = "MERGE_USER_INFO_OVER_ID_TOKEN_CLAIMS"
	}
	return w
}

// wfProviderFromForm is the provider the form's values describe.
func wfProviderFromForm(v map[string]string) (resourcemanager.WorkforcePoolProvider, error) {
	out := resourcemanager.WorkforcePoolProvider{DisplayName: v["providerDisplayName"], Description: v["providerDescription"],
		AttributeCondition: strings.TrimSpace(v["attributeCondition"])}
	m, err := console.ParseMap(v["attributeMapping"])
	if err != nil {
		return out, err
	}
	if len(m) > 0 {
		out.AttributeMapping = m
	}
	switch v["providerType"] {
	case wfTypeOIDC, "":
		out.Oidc = &resourcemanager.WFOidc{IssuerURI: strings.TrimSpace(v["issuerUri"]), ClientID: strings.TrimSpace(v["clientId"]),
			JwksJSON: strings.TrimSpace(v["jwksJson"]), WebSsoConfig: wfWebSso(v)}
		if s := v["clientSecret"]; s != "" {
			out.Oidc.ClientSecret = &resourcemanager.WFClientSecret{Value: &resourcemanager.WFClientSecretValue{PlainText: s}}
		}
	case wfTypeSAML:
		out.Saml = &resourcemanager.WIFSaml{IdpMetadataXML: strings.TrimSpace(v["idpMetadataXml"])}
	default:
		return out, fmt.Errorf("unknown provider type %q", v["providerType"])
	}
	return out, nil
}

// CreateForm is Google's Create pool: the pool and its session, then an
// optional provider and its attributes.
func (iamWorkforceIdentityProvider) CreateForm() (string, []console.Field) {
	const pool = "Create a workforce pool"
	const prov = "Add a provider to pool (optional)"
	const attrs = "Configure provider attributes"
	fields := []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Section: pool, Help: "At most 32 characters."},
		{Name: "poolId", Label: "Pool ID", Type: "text", Required: true, Pattern: wfPoolIDPattern, Section: pool, DeriveFrom: "displayName",
			Help: "6 to 63 lowercase letters, digits and dashes, starting with a letter; the prefix gcp- is Google's. It cannot be changed, nor reused for 30 days after a delete."},
		{Name: "description", Label: "Description", Type: "text", Section: pool, Help: "At most 256 characters."},
		{Name: "sessionDuration", Label: "Session duration", Type: "text", Section: pool, Default: "3600s", Pattern: `^[0-9]+s$`,
			Help: "How long the pool's access tokens and sign-in sessions last: 900s to 43200s."},
	}
	return "Create pool", append(fields, wfProviderFields(prov, attrs, false)...)
}

func (iamWorkforceIdentityProvider) CreateTitle() string { return "Create pool" }

func (iamWorkforceIdentityProvider) CreateOnPage() bool { return true }

func (p iamWorkforceIdentityProvider) Create(_ context.Context, _ string, values map[string]string) (string, error) {
	id := strings.TrimSpace(values["poolId"])
	provID := strings.TrimSpace(values["providerId"])
	var prov resourcemanager.WorkforcePoolProvider
	if provID != "" {
		var err error
		if prov, err = wfProviderFromForm(values); err != nil {
			return "", err
		}
	}
	if _, err := p.registry.CreateWorkforcePool(id, resourcemanager.WorkforcePool{Parent: resourcemanager.OrganizationName,
		DisplayName: values["displayName"], Description: values["description"], SessionDuration: strings.TrimSpace(values["sessionDuration"])}); err != nil {
		return "", err
	}
	if provID != "" {
		if _, err := p.registry.CreateWorkforcePoolProvider(id, provID, prov); err != nil {
			// As on Google, the pool is made first: it stays, and Add
			// provider on its page tries again.
			return id, fmt.Errorf("created pool %s, but not its provider: %w", id, err)
		}
	}
	return id, nil
}

func (p iamWorkforceIdentityProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 4 {
		return console.DeeperThan(4, path), nil
	}
	pool, err := p.registry.WorkforcePool(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the pool: " + consoleErr(err)}, nil
	}
	if len(path) >= 3 {
		// A provider's SCIM tenant and its tokens (#2067).
		tenant, ok := strings.CutPrefix(path[2], wfScimRow)
		if !ok {
			return console.DeeperThan(2, path), nil
		}
		if len(path) == 4 {
			token, ok := strings.CutPrefix(path[3], wfTokenRow)
			if !ok {
				return console.DeeperThan(3, path), nil
			}
			return p.tokenDetail(path[0], path[1], tenant, token)
		}
		return p.scimTenantDetail(ctx, project, path[0], path[1], tenant)
	}
	if len(path) == 2 {
		if s, ok := strings.CutPrefix(path[1], wfSubjectRow); ok {
			return p.subjectDetail(pool, s)
		}
		return p.providerDetail(ctx, project, pool, path[1])
	}
	props := []console.Property{
		{Label: "Pool ID", Value: path[0]},
		{Label: "Display name", Value: orDash(pool.DisplayName)},
		{Label: "Status", Value: wifStatus(pool.State, pool.Disabled)},
		{Label: "Description", Value: orDash(pool.Description)},
		{Label: "Organization", Value: pool.Parent},
		{Label: "Session duration", Value: pool.SessionDuration},
		{Label: "Programmatic sign-in", Value: map[bool]string{true: "Disabled", false: "Enabled"}[pool.AccessRestrictions != nil && pool.AccessRestrictions.DisableProgrammaticSignin]},
		{Label: "IAM principal", Value: "principalSet://iam.googleapis.com/" + pool.Name + "/*"},
		{Label: "Resource name", Value: pool.Name},
	}
	if pool.ExpireTime != "" {
		props = append(props, console.Property{Label: "Purged after", Value: pool.ExpireTime})
	}
	provs := console.Listing{NameColumn: "Provider ID", Columns: []string{"Name", "Type", "Issuer"}, Noun: "providers"}
	list, err := p.registry.WorkforcePoolProviders(path[0], true)
	if err != nil {
		provs.Unavailable = consoleErr(err)
	}
	for _, v := range list {
		typ, issuer := wfProviderType(v)
		r := console.Resource{Name: lastSegment(v.Name), Status: wifStatus(v.State, v.Disabled),
			Fields: map[string]string{"Name": v.DisplayName, "Type": typ, "Issuer": issuer}}
		r.Actions = p.DetailActions(ctx, project, []string{path[0], r.Name})
		provs.Items = append(provs.Items, r)
	}
	provs.Total = len(provs.Items)
	subs := console.Listing{NameColumn: "Subject", Columns: []string{"First sign-in", "Sessions revoked"}, Noun: "subjects",
		Note: "A subject is made by its first token exchange. Deleting one stops its google.subject from signing in for 30 days."}
	sl, err := p.registry.WorkforcePoolSubjects(path[0])
	if err != nil {
		subs.Unavailable = consoleErr(err)
	}
	for _, s := range sl {
		r := console.Resource{Name: wfSubjectRow + s.Subject, Status: s.State,
			Fields: map[string]string{"First sign-in": s.CreateTime, "Sessions revoked": orDash(s.RevokeTime)}}
		r.Actions = p.DetailActions(ctx, project, []string{path[0], r.Name})
		subs.Items = append(subs.Items, r)
	}
	subs.Total = len(subs.Items)
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Pool", Properties: props}}},
		{ID: "providers", Label: "Providers", Listing: provs},
		{ID: "subjects", Label: "Subjects", Listing: subs},
		// The pool's own IAM policy (#2067).
		{ID: "permissions", Kind: console.KindPermissions},
	}}
	if pool.State != "DELETED" {
		d.Edit = &console.EditForm{Label: "Edit pool", Note: "The pool ID and its organization cannot change, as on Google.", Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Default: pool.DisplayName, Help: "At most 32 characters."},
			{Name: "description", Label: "Description", Type: "text", Default: pool.Description, Help: "At most 256 characters."},
			{Name: "sessionDuration", Label: "Session duration", Type: "text", Default: pool.SessionDuration, Pattern: `^[0-9]+s$`, Help: "900s to 43200s."},
		}}
	}
	return d, nil
}

func wfProviderType(v resourcemanager.WorkforcePoolProvider) (typ, issuer string) {
	switch {
	case v.Oidc != nil:
		return wfTypeOIDC, v.Oidc.IssuerURI
	case v.Saml != nil:
		return wfTypeSAML, ""
	}
	return "", ""
}

func (p iamWorkforceIdentityProvider) subjectDetail(pool resourcemanager.WorkforcePool, subject string) (console.Detail, error) {
	list, err := p.registry.WorkforcePoolSubjects(lastSegment(pool.Name))
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	for _, s := range list {
		if s.Subject != subject {
			continue
		}
		props := []console.Property{
			{Label: "Subject", Value: s.Subject},
			{Label: "Status", Value: s.State},
			{Label: "IAM principal", Value: "principal://iam.googleapis.com/" + pool.Name + "/subject/" + s.Subject},
			{Label: "First sign-in", Value: s.CreateTime},
			{Label: "Sessions revoked", Value: orDash(s.RevokeTime)},
			{Label: "Resource name", Value: s.Name},
		}
		if s.ExpireTime != "" {
			props = append(props, console.Property{Label: "Purged after", Value: s.ExpireTime})
		}
		return console.Detail{Summary: props[:2], Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Subject", Properties: props}}}}}, nil
	}
	return console.Detail{Unavailable: "no subject " + subject + " in the pool"}, nil
}

func (p iamWorkforceIdentityProvider) providerDetail(ctx context.Context, project string, pool resourcemanager.WorkforcePool, id string) (console.Detail, error) {
	v, err := p.registry.WorkforcePoolProvider(lastSegment(pool.Name), id)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the provider: " + consoleErr(err)}, nil
	}
	typ, _ := wfProviderType(v)
	props := []console.Property{
		{Label: "Provider ID", Value: id},
		{Label: "Name", Value: orDash(v.DisplayName)},
		{Label: "Status", Value: wifStatus(v.State, v.Disabled)},
		{Label: "Type", Value: typ},
		{Label: "Description", Value: orDash(v.Description)},
		{Label: "Audience", Value: "//iam.googleapis.com/" + v.Name},
		{Label: "SCIM usage", Value: wfScimUsageLabel(v.ScimUsage)},
	}
	var cfg []console.Property
	switch {
	case v.Oidc != nil:
		secret := "None"
		if v.Oidc.ClientSecret != nil && v.Oidc.ClientSecret.Value != nil {
			secret = "Thumbprint " + v.Oidc.ClientSecret.Value.Thumbprint
		}
		resp, claims := "", ""
		if w := v.Oidc.WebSsoConfig; w != nil {
			resp, claims = w.ResponseType, w.AssertionClaimsBehavior
		}
		cfg = append(cfg, console.Property{Label: "Issuer (URL)", Value: v.Oidc.IssuerURI},
			console.Property{Label: "Client ID", Value: v.Oidc.ClientID},
			console.Property{Label: "Client secret", Value: secret},
			console.Property{Label: "Response type", Value: orDash(resp)},
			console.Property{Label: "Assertion claims behavior", Value: orDash(claims)},
			console.Property{Label: "JWK file", Value: map[bool]string{true: "Uploaded", false: "From the issuer's discovery document"}[v.Oidc.JwksJSON != ""]})
	case v.Saml != nil:
		cfg = append(cfg, console.Property{Label: "IdP metadata", Value: strconv.Itoa(len(v.Saml.IdpMetadataXML)) + " characters"})
	}
	var attrs []console.Property
	for _, k := range mappingKeys(v.AttributeMapping) {
		attrs = append(attrs, console.Property{Label: k, Value: v.AttributeMapping[k]})
	}
	attrs = append(attrs, console.Property{Label: "Attribute condition", Value: orDash(v.AttributeCondition)})
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Provider", Properties: props}, {Heading: "Provider details", Properties: cfg}, {Heading: "Attribute mapping", Properties: attrs}}},
		p.wfScimSection(ctx, project, lastSegment(pool.Name), id),
	}}
	if v.State != "DELETED" {
		fields := []console.Field{
			{Name: "providerDisplayName", Label: "Name", Type: "text", Default: v.DisplayName, Help: "At most 32 characters."},
			{Name: "providerDescription", Label: "Description", Type: "text", Default: v.Description, Help: "At most 256 characters."},
		}
		if v.Oidc != nil {
			fields = append(fields,
				console.Field{Name: "issuerUri", Label: "Issuer (URL)", Type: "text", Default: v.Oidc.IssuerURI},
				console.Field{Name: "clientId", Label: "Client ID", Type: "text", Default: v.Oidc.ClientID},
				console.Field{Name: "jwksJson", Label: "JWK file (JSON)", Type: "textarea", Default: v.Oidc.JwksJSON})
		}
		fields = append(fields,
			console.Field{Name: "attributeMapping", Label: "Attribute mapping", Type: "map", Default: console.FormatMap(v.AttributeMapping)},
			console.Field{Name: "attributeCondition", Label: "Attribute condition", Type: "textarea", Default: v.AttributeCondition},
			console.Field{Name: "scimUsage", Label: "SCIM usage", Type: "select", Default: wfScimUsageLabel(v.ScimUsage),
				Options: []string{wfScimUsageNone, wfScimUsageGroups, wfScimUsageUsersGroups},
				Help:    "Whether IAM reads groups, or users and groups, from the pool's SCIM tenant, which must be under this provider."})
		d.Edit = &console.EditForm{Label: "Edit provider", Fields: fields, Note: "The provider ID and type cannot change, as on Google."}
	}
	return d, nil
}

func (p iamWorkforceIdentityProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	switch len(path) {
	case 1:
		_, err := p.registry.UpdateWorkforcePool(path[0], resourcemanager.WorkforcePool{DisplayName: values["displayName"],
			Description: values["description"], SessionDuration: strings.TrimSpace(values["sessionDuration"])},
			[]string{"displayName", "description", "sessionDuration"})
		return err
	case 2:
		cur, err := p.registry.WorkforcePoolProvider(path[0], path[1])
		if err != nil {
			return err
		}
		in := resourcemanager.WorkforcePoolProvider{DisplayName: values["providerDisplayName"], Description: values["providerDescription"],
			AttributeCondition: strings.TrimSpace(values["attributeCondition"])}
		if in.AttributeMapping, err = console.ParseMap(values["attributeMapping"]); err != nil {
			return err
		}
		in.ScimUsage = wfScimUsageValue(values["scimUsage"])
		mask := []string{"displayName", "description", "attributeMapping", "attributeCondition", "scimUsage"}
		if cur.Oidc != nil {
			in.Oidc = &resourcemanager.WFOidc{IssuerURI: strings.TrimSpace(values["issuerUri"]), ClientID: strings.TrimSpace(values["clientId"]),
				JwksJSON: strings.TrimSpace(values["jwksJson"])}
			mask = append(mask, "oidc.issuerUri", "oidc.clientId", "oidc.jwksJson")
		}
		_, err = p.registry.UpdateWorkforcePoolProvider(path[0], path[1], in, mask)
		return err
	case 3:
		tenant, ok := strings.CutPrefix(path[2], wfScimRow)
		if !ok {
			break
		}
		_, err := p.registry.UpdateWorkforcePoolProviderScimTenant(path[0], path[1], tenant, resourcemanager.WorkforcePoolProviderScimTenant{
			DisplayName: values["tenantDisplayName"], Description: values["tenantDescription"]}, []string{"displayName", "description"})
		return err
	}
	return fmt.Errorf("only a pool, a provider or a SCIM tenant can be edited")
}

func (p iamWorkforceIdentityProvider) Delete(_ context.Context, _, row string) error {
	_, err := p.registry.DeleteWorkforcePool(row)
	return err
}

// Actions are a pool row's Disable or Enable, or a deleted one's Restore.
func (iamWorkforceIdentityProvider) Actions(r console.Resource) []console.Action {
	switch r.Status {
	case "DELETED":
		return []console.Action{{ID: "undelete", Label: "Restore"}}
	case "DISABLED":
		return []console.Action{{ID: "enable", Label: "Enable"}}
	}
	return []console.Action{{ID: "disable", Label: "Disable", Confirm: "the pool exchanges no token, and its users' tokens grant nothing until it is enabled again"}}
}

func (p iamWorkforceIdentityProvider) Act(ctx context.Context, project, row, action string) error {
	return p.ActAt(ctx, project, []string{row}, action, nil)
}

// DetailActions are a pool's Add provider and its row actions; a
// provider's Disable or Enable, Delete, or Restore; a subject's Revoke
// sessions, Delete, or Restore.
func (p iamWorkforceIdentityProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	switch len(path) {
	case 1:
		pool, err := p.registry.WorkforcePool(path[0])
		if err != nil {
			return nil
		}
		st := wifStatus(pool.State, pool.Disabled)
		var out []console.Action
		if st != "DELETED" {
			out = append(out, console.Action{ID: "add-provider", Label: "Add provider", Fields: wfProviderFields("Provider details", "Configure provider attributes", true)})
		}
		return append(out, p.Actions(console.Resource{Status: st})...)
	case 2:
		if s, ok := strings.CutPrefix(path[1], wfSubjectRow); ok {
			list, err := p.registry.WorkforcePoolSubjects(path[0])
			if err != nil {
				return nil
			}
			for _, x := range list {
				if x.Subject == s {
					if x.State == "DELETED" {
						return []console.Action{{ID: "undelete-subject", Label: "Restore"}}
					}
					return []console.Action{{ID: "revoke-sessions", Label: "Revoke sessions", Confirm: "every token the subject holds stops granting; it must sign in again"},
						{ID: "delete-subject", Label: "Delete", Destructive: true}}
				}
			}
			return nil
		}
		v, err := p.registry.WorkforcePoolProvider(path[0], path[1])
		if err != nil {
			return nil
		}
		switch wifStatus(v.State, v.Disabled) {
		case "DELETED":
			return []console.Action{{ID: "undelete-provider", Label: "Restore"}}
		case "DISABLED":
			return []console.Action{{ID: "enable-provider", Label: "Enable"}, {ID: "delete-provider", Label: "Delete", Destructive: true}}
		}
		out := []console.Action{{ID: "disable-provider", Label: "Disable", Confirm: "the provider exchanges no token; the tokens it issued still grant"},
			{ID: "delete-provider", Label: "Delete", Destructive: true}}
		if ts, err := p.registry.WorkforcePoolProviderScimTenants(path[0], path[1], true); err == nil && len(ts) == 0 {
			out = append([]console.Action{{ID: "create-scim-tenant", Label: "Create SCIM tenant", Fields: wfScimTenantFields()}}, out...)
		}
		return out
	case 3, 4:
		return p.scimActions(path)
	}
	return nil
}

func (p iamWorkforceIdentityProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult performs an action; a SCIM token's Create answers its
// security token, to the response only, never to the operations ledger.
func (p iamWorkforceIdentityProvider) ActAtResult(_ context.Context, _ string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) >= 3 || (len(path) == 2 && action == "create-scim-tenant") {
		return p.actScim(path, action, values)
	}
	return nil, p.actAt(path, action, values)
}

func (p iamWorkforceIdentityProvider) actAt(path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return fmt.Errorf("unknown action %q", action)
	}
	pool := path[0]
	setPool := func(disabled bool) error {
		_, err := p.registry.UpdateWorkforcePool(pool, resourcemanager.WorkforcePool{Disabled: disabled}, []string{"disabled"})
		return err
	}
	if len(path) == 1 {
		switch action {
		case "disable":
			return setPool(true)
		case "enable":
			return setPool(false)
		case "undelete":
			_, err := p.registry.UndeleteWorkforcePool(pool)
			return err
		case "add-provider":
			v, err := wfProviderFromForm(values)
			if err != nil {
				return err
			}
			_, err = p.registry.CreateWorkforcePoolProvider(pool, strings.TrimSpace(values["providerId"]), v)
			return err
		}
		return fmt.Errorf("unknown action %q", action)
	}
	if s, ok := strings.CutPrefix(path[1], wfSubjectRow); ok {
		var err error
		switch action {
		case "revoke-sessions":
			_, err = p.registry.RevokeWorkforcePoolSubjectSessions(pool, s)
		case "delete-subject":
			_, err = p.registry.DeleteWorkforcePoolSubject(pool, s)
		case "undelete-subject":
			_, err = p.registry.UndeleteWorkforcePoolSubject(pool, s)
		default:
			err = fmt.Errorf("unknown action %q", action)
		}
		return err
	}
	prov := path[1]
	setProv := func(disabled bool) error {
		_, err := p.registry.UpdateWorkforcePoolProvider(pool, prov, resourcemanager.WorkforcePoolProvider{Disabled: disabled}, []string{"disabled"})
		return err
	}
	switch action {
	case "disable-provider":
		return setProv(true)
	case "enable-provider":
		return setProv(false)
	case "delete-provider":
		_, err := p.registry.DeleteWorkforcePoolProvider(pool, prov)
		return err
	case "undelete-provider":
		_, err := p.registry.UndeleteWorkforcePoolProvider(pool, prov)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var _ console.Creator = iamWorkforceIdentityProvider{}
var _ console.PageCreator = iamWorkforceIdentityProvider{}
var _ console.Deleter = iamWorkforceIdentityProvider{}
var _ console.Driller = iamWorkforceIdentityProvider{}
var _ console.Editor = iamWorkforceIdentityProvider{}
var _ console.Actor = iamWorkforceIdentityProvider{}
