package main

// Third-party SSO (#1959): the customer's SSO profiles and their
// assignments, as Google's Admin console has them under Security >
// Authentication > SSO with third-party IdP (Third-party SSO profiles:
// Add SAML profile, Add OIDC profile, a SAML profile's SP details and IdP
// certificates; Manage SSO profile assignments, by group with its
// priority and by organizational unit,
// https://knowledge.workspace.google.com/admin/apps/setting-up-sso), over
// the registry Cloud Identity's inboundSamlSsoProfiles,
// inboundOidcSsoProfiles and inboundSsoAssignments serve. No sign-in
// happens locally: the profiles are stored configuration.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

const ssoNote = "Third-party SSO profiles of the organization's customer (" + resourcemanager.CustomerName + "), as Cloud Identity's " +
	"inboundSamlSsoProfiles and inboundOidcSsoProfiles serve them. No sign-in happens locally: a profile and its assignments are stored " +
	"configuration, which the API and this console read; the instance has no Google sign-in page to send users to an IdP."

type iamSSOProfilesProvider struct {
	registry *resourcemanager.Registry
}

func (iamSSOProfilesProvider) ID() string    { return "iam-sso-profiles" }
func (iamSSOProfilesProvider) Title() string { return "SSO profiles" }

func samlName(id string) string { return "inboundSamlSsoProfiles/" + id }
func oidcName(id string) string { return "inboundOidcSsoProfiles/" + id }

func (p iamSSOProfilesProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Profile ID", Columns: []string{"Name", "Type", "IdP entity ID / issuer", "IdP certificates"}, Noun: "SSO profiles", Note: ssoNote}
	saml, err := p.registry.ListSAMLProfiles()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	oidc, err := p.registry.ListOIDCProfiles()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, s := range saml {
		l.Items = append(l.Items, console.Resource{Name: strings.TrimPrefix(s.Profile.Name, "inboundSamlSsoProfiles/"), Status: "ACTIVE",
			Fields: map[string]string{"Name": orDash(s.Profile.DisplayName), "Type": "SAML", "IdP entity ID / issuer": s.Profile.IdpConfig.EntityId,
				"IdP certificates": strconv.Itoa(len(s.Credentials))}})
	}
	for _, o := range oidc {
		l.Items = append(l.Items, console.Resource{Name: strings.TrimPrefix(o.Profile.Name, "inboundOidcSsoProfiles/"), Status: "ACTIVE",
			Fields: map[string]string{"Name": orDash(o.Profile.DisplayName), "Type": "OIDC", "IdP entity ID / issuer": o.Profile.IdpConfig.IssuerUri,
				"IdP certificates": "—"}})
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	l.Total = len(l.Items)
	return l, nil
}

func (iamSSOProfilesProvider) CreateTitle() string { return "Add SSO profile" }

func (iamSSOProfilesProvider) CreateForm() (string, []console.Field) {
	return "Add", []console.Field{
		{Name: "type", Label: "Profile type", Type: "select", Options: []string{"SAML", "OIDC"}, Default: "SAML",
			OptionLabels: map[string]string{"SAML": "SAML profile", "OIDC": "OIDC profile"}},
		{Name: "displayName", Label: "Name", Type: "text", Help: "A name for the profile."},
		{Name: "entityId", Label: "IDP entity ID", Type: "text", ShowWhen: "type=SAML", Help: "Required for SAML: the IdP's SAML entity ID."},
		{Name: "ssoUrl", Label: "Sign-in page URL", Type: "text", ShowWhen: "type=SAML", Pattern: `https://\S+`,
			Help: "Required for SAML: where AuthnRequests are sent (HTTP-Redirect binding). Must use HTTPS."},
		{Name: "logoutUrl", Label: "Sign-out page URL", Type: "text", ShowWhen: "type=SAML", Pattern: `(https://\S+)?`,
			Help: "Optional. Must use HTTPS. No SAML logout request is sent."},
		{Name: "issuerUri", Label: "Issuer URL", Type: "text", ShowWhen: "type=OIDC", Pattern: `(https://\S+)?`,
			Help: "Required for OIDC: the IdP's issuer identifier, from which its discovery document is found. Must use HTTPS."},
		{Name: "clientId", Label: "Client ID", Type: "text", ShowWhen: "type=OIDC", Help: "The OAuth 2.0 client ID at the IdP."},
		{Name: "clientSecret", Label: "Client secret", Type: "password", ShowWhen: "type=OIDC",
			Help: "The OAuth 2.0 client secret: stored, never shown again."},
		{Name: "changePasswordUrl", Label: "Change password URL", Type: "text", Pattern: `(https://\S+)?`,
			Help: "Optional: where users change their password. Must use HTTPS."},
	}
}

func (p iamSSOProfilesProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	if v["type"] == "OIDC" {
		o, err := p.registry.CreateOIDCProfile(ci.InboundOidcSsoProfile{Customer: resourcemanager.CustomerName, DisplayName: v["displayName"],
			IdpConfig: &ci.OidcIdpConfig{IssuerUri: strings.TrimSpace(v["issuerUri"]), ChangePasswordUri: strings.TrimSpace(v["changePasswordUrl"])},
			RpConfig:  &ci.OidcRpConfig{ClientId: strings.TrimSpace(v["clientId"]), ClientSecret: v["clientSecret"]}})
		if err != nil {
			return "", err
		}
		return strings.TrimPrefix(o.Profile.Name, "inboundOidcSsoProfiles/"), nil
	}
	s, err := p.registry.CreateSAMLProfile(ci.InboundSamlSsoProfile{Customer: resourcemanager.CustomerName, DisplayName: v["displayName"],
		IdpConfig: &ci.SamlIdpConfig{EntityId: strings.TrimSpace(v["entityId"]), SingleSignOnServiceUri: strings.TrimSpace(v["ssoUrl"]),
			LogoutRedirectUri: strings.TrimSpace(v["logoutUrl"]), ChangePasswordUri: strings.TrimSpace(v["changePasswordUrl"])}})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(s.Profile.Name, "inboundSamlSsoProfiles/"), nil
}

func (p iamSSOProfilesProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if s, err := p.registry.GetSAMLProfile(samlName(path[0])); err == nil {
		return p.samlDetail(s), nil
	}
	o, err := p.registry.GetOIDCProfile(oidcName(path[0]))
	if err != nil {
		return console.Detail{Unavailable: "cannot read the SSO profile: " + consoleErr(err)}, nil
	}
	c := o.Profile.IdpConfig
	props := []console.Property{
		{Label: "Resource name", Value: o.Profile.Name},
		{Label: "Name", Value: orDash(o.Profile.DisplayName)},
		{Label: "Type", Value: "OIDC"},
		{Label: "Issuer URL", Value: c.IssuerUri},
		{Label: "Change password URL", Value: orDash(c.ChangePasswordUri)},
	}
	rp := []console.Property{{Label: "Client ID", Value: orDash(o.Profile.RpConfig.ClientId)},
		{Label: "Client secret", Value: map[bool]string{true: "Set (never shown)", false: "Not set"}[o.ClientSecret != ""]},
		{Label: "Redirect URI", Value: strings.Join(o.Profile.RpConfig.RedirectUris, ", ")}}
	return console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "IdP details", Properties: props}, {Heading: "Relying party details", Properties: rp}}},
	}, Edit: &console.EditForm{Label: "Edit profile", Fields: []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Default: o.Profile.DisplayName},
		{Name: "issuerUri", Label: "Issuer URL", Type: "text", Required: true, Default: c.IssuerUri, Pattern: `https://\S+`},
		{Name: "changePasswordUrl", Label: "Change password URL", Type: "text", Default: c.ChangePasswordUri, Pattern: `(https://\S+)?`},
		{Name: "clientId", Label: "Client ID", Type: "text", Default: o.Profile.RpConfig.ClientId},
		{Name: "clientSecret", Label: "Client secret", Type: "password", Help: "Leave empty to keep the stored secret."},
	}}}, nil
}

func (p iamSSOProfilesProvider) samlDetail(s *resourcemanager.SAMLProfile) console.Detail {
	c := s.Profile.IdpConfig
	props := []console.Property{
		{Label: "Resource name", Value: s.Profile.Name},
		{Label: "Name", Value: orDash(s.Profile.DisplayName)},
		{Label: "Type", Value: "SAML"},
		{Label: "IDP entity ID", Value: c.EntityId},
		{Label: "Sign-in page URL", Value: c.SingleSignOnServiceUri},
		{Label: "Sign-out page URL", Value: orDash(c.LogoutRedirectUri)},
		{Label: "Change password URL", Value: orDash(c.ChangePasswordUri)},
	}
	sp := []console.Property{{Label: "Entity ID", Value: s.Profile.SpConfig.EntityId},
		{Label: "ACS URL", Value: s.Profile.SpConfig.AssertionConsumerServiceUri}}
	certs := console.Listing{NameColumn: "Certificate ID", Columns: []string{"Key", "Updated"}, Noun: "IdP certificates"}
	for _, x := range s.Credentials {
		certs.Items = append(certs.Items, console.Resource{Name: x.ID, Fields: map[string]string{
			"Key": fmt.Sprintf("%s %d-bit", x.KeyType, x.KeySize), "Updated": x.UpdateTime.UTC().Format("2006-01-02 15:04:05 UTC")}})
	}
	certs.Total = len(certs.Items)
	return console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "IdP details", Properties: props}, {Heading: "SP details", Properties: sp}}},
		{ID: "certificates", Label: "IdP certificates", Listing: certs},
	}, Edit: &console.EditForm{Label: "Edit profile", Fields: []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Default: s.Profile.DisplayName},
		{Name: "entityId", Label: "IDP entity ID", Type: "text", Required: true, Default: c.EntityId},
		{Name: "ssoUrl", Label: "Sign-in page URL", Type: "text", Required: true, Default: c.SingleSignOnServiceUri, Pattern: `https://\S+`},
		{Name: "logoutUrl", Label: "Sign-out page URL", Type: "text", Default: c.LogoutRedirectUri, Pattern: `(https://\S+)?`},
		{Name: "changePasswordUrl", Label: "Change password URL", Type: "text", Default: c.ChangePasswordUri, Pattern: `(https://\S+)?`},
		{Name: "certificate", Label: "Upload IdP certificate", Type: "textarea",
			Help: "Optional: a PEM encoded X.509 certificate (-----BEGIN CERTIFICATE-----) with an RSA or DSA public key. Up to 2 per profile."},
		{Name: "removeCertificate", Label: "Delete IdP certificate", Type: "text", Help: "Optional: the ID of a certificate to delete."},
	}, Note: "Upload a second certificate before deleting the first to rotate them."}}
}

func (p iamSSOProfilesProvider) Edit(_ context.Context, _ string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a profile can be edited")
	}
	if _, err := p.registry.GetSAMLProfile(samlName(path[0])); err != nil {
		_, err := p.registry.UpdateOIDCProfile(oidcName(path[0]), ci.InboundOidcSsoProfile{DisplayName: v["displayName"],
			IdpConfig: &ci.OidcIdpConfig{IssuerUri: strings.TrimSpace(v["issuerUri"]), ChangePasswordUri: strings.TrimSpace(v["changePasswordUrl"])},
			RpConfig:  &ci.OidcRpConfig{ClientId: strings.TrimSpace(v["clientId"]), ClientSecret: v["clientSecret"]}},
			append([]string{"display_name", "idp_config", "rp_config.client_id"}, map[bool][]string{true: {"rp_config.client_secret"}}[v["clientSecret"] != ""]...))
		return err
	}
	name := samlName(path[0])
	if _, err := p.registry.UpdateSAMLProfile(name, ci.InboundSamlSsoProfile{DisplayName: v["displayName"],
		IdpConfig: &ci.SamlIdpConfig{EntityId: strings.TrimSpace(v["entityId"]), SingleSignOnServiceUri: strings.TrimSpace(v["ssoUrl"]),
			LogoutRedirectUri: strings.TrimSpace(v["logoutUrl"]), ChangePasswordUri: strings.TrimSpace(v["changePasswordUrl"])}},
		[]string{"display_name", "idp_config"}); err != nil {
		return err
	}
	if id := strings.TrimSpace(v["removeCertificate"]); id != "" {
		if err := p.registry.DeleteIdPCredential(name + "/idpCredentials/" + id); err != nil {
			return err
		}
	}
	if pemData := strings.TrimSpace(v["certificate"]); pemData != "" {
		if _, _, err := p.registry.AddIdPCredential(name, pemData); err != nil {
			return err
		}
	}
	return nil
}

func (p iamSSOProfilesProvider) Delete(_ context.Context, _ string, row string) error {
	if _, err := p.registry.GetSAMLProfile(samlName(row)); err == nil {
		return p.registry.DeleteSAMLProfile(samlName(row))
	}
	return p.registry.DeleteOIDCProfile(oidcName(row))
}

var _ console.Creator = iamSSOProfilesProvider{}
var _ console.Deleter = iamSSOProfilesProvider{}
var _ console.Driller = iamSSOProfilesProvider{}
var _ console.Editor = iamSSOProfilesProvider{}

// iamSSOAssignmentsProvider is Manage SSO profile assignments: a group's
// or an organizational unit's profile (or None), the groups in priority
// order.
type iamSSOAssignmentsProvider struct {
	registry *resourcemanager.Registry
}

func (iamSSOAssignmentsProvider) ID() string    { return "iam-sso-assignments" }
func (iamSSOAssignmentsProvider) Title() string { return "SSO profile assignments" }

var ssoModeLabels = map[string]string{
	resourcemanager.SSOModeOff: "None (SSO off)", resourcemanager.SSOModeSAML: "SAML profile", resourcemanager.SSOModeOIDC: "OIDC profile",
	resourcemanager.SSOModeDomainWide: "Legacy SSO profile (domain-wide SAML, if enabled)",
}

// ssoTargetLabel names an assignment's target: a group by its email.
func (p iamSSOAssignmentsProvider) ssoTargetLabel(a *ci.InboundSsoAssignment) string {
	if a.TargetGroup != "" {
		if g, err := p.registry.GetGroup(a.TargetGroup); err == nil {
			return "Group " + g.Email
		}
		return a.TargetGroup
	}
	return "Org unit " + a.TargetOrgUnit
}

func ssoProfileOf(a *ci.InboundSsoAssignment) string {
	switch {
	case a.SamlSsoInfo != nil:
		return strings.TrimPrefix(a.SamlSsoInfo.InboundSamlSsoProfile, "inboundSamlSsoProfiles/")
	case a.OidcSsoInfo != nil:
		return strings.TrimPrefix(a.OidcSsoInfo.InboundOidcSsoProfile, "inboundOidcSsoProfiles/")
	}
	return ""
}

func redirectLabel(a *ci.InboundSsoAssignment) string {
	if a.SignInBehavior != nil && a.SignInBehavior.RedirectCondition == "NEVER" {
		return "Never redirect"
	}
	return "Redirect"
}

func (p iamSSOAssignmentsProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Assignment ID", Columns: []string{"Target", "Priority", "SSO profile", "Profile ID", "Sign-in"},
		Noun: "SSO profile assignments", Note: ssoNote + " A group's assignment of the lowest priority number applies first; otherwise its organizational unit's."}
	as, err := p.registry.ListSSOAssignments()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, a := range as {
		prio := "—"
		if a.TargetGroup != "" {
			prio = strconv.FormatInt(a.Rank, 10)
		}
		l.Items = append(l.Items, console.Resource{Name: strings.TrimPrefix(a.Name, "inboundSsoAssignments/"), Status: "ACTIVE",
			Fields: map[string]string{"Target": p.ssoTargetLabel(a), "Priority": prio, "SSO profile": ssoModeLabels[a.SsoMode],
				"Profile ID": orDash(ssoProfileOf(a)), "Sign-in": redirectLabel(a)}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (iamSSOAssignmentsProvider) CreateTitle() string { return "Assign SSO profile" }

func ssoModeField(def string) console.Field {
	return console.Field{Name: "mode", Label: "SSO profile", Type: "select", Default: def,
		Options:      []string{resourcemanager.SSOModeSAML, resourcemanager.SSOModeOIDC, resourcemanager.SSOModeOff, resourcemanager.SSOModeDomainWide},
		OptionLabels: ssoModeLabels}
}

func ssoRedirectField(def string) console.Field {
	return console.Field{Name: "redirect", Label: "Redirect users to the IdP", Type: "select", Default: def, Options: []string{"Redirect", "NEVER"},
		OptionLabels: map[string]string{"Redirect": "Yes (recommended)", "NEVER": "No: Google asks for the user's identity"}}
}

func (iamSSOAssignmentsProvider) CreateForm() (string, []console.Field) {
	return "Assign", []console.Field{
		{Name: "targetType", Label: "Assign to", Type: "select", Options: []string{"group", "orgUnit"}, Default: "group",
			OptionLabels: map[string]string{"group": "Group", "orgUnit": "Organizational unit"}},
		{Name: "group", Label: "Group email", Type: "text", ShowWhen: "targetType=group", Help: "A group of " + resourcemanager.OrganizationDomain + "."},
		{Name: "rank", Label: "Priority", Type: "text", ShowWhen: "targetType=group", Default: "1", Pattern: `[1-9][0-9]*`,
			Help: "1 is the highest; the other groups' assignments move down."},
		{Name: "orgUnit", Label: "Organizational unit ID", Type: "text", ShowWhen: "targetType=orgUnit", Help: "orgUnits/{id}, or the ID alone."},
		ssoModeField(resourcemanager.SSOModeSAML),
		{Name: "profile", Label: "Profile ID", Type: "text", Help: "The SAML or OIDC profile's ID, from SSO profiles; empty for None and the legacy profile."},
		ssoRedirectField("Redirect"),
	}
}

func ssoAssignmentInput(v map[string]string, base *ci.InboundSsoAssignment) (*ci.InboundSsoAssignment, error) {
	a := base
	a.SsoMode = v["mode"]
	a.SamlSsoInfo, a.OidcSsoInfo = nil, nil
	prof := strings.TrimSpace(v["profile"])
	switch a.SsoMode {
	case resourcemanager.SSOModeSAML:
		a.SamlSsoInfo = &ci.SamlSsoInfo{InboundSamlSsoProfile: "inboundSamlSsoProfiles/" + strings.TrimPrefix(prof, "inboundSamlSsoProfiles/")}
	case resourcemanager.SSOModeOIDC:
		a.OidcSsoInfo = &ci.OidcSsoInfo{InboundOidcSsoProfile: "inboundOidcSsoProfiles/" + strings.TrimPrefix(prof, "inboundOidcSsoProfiles/")}
	}
	if (a.SamlSsoInfo != nil || a.OidcSsoInfo != nil) && prof == "" {
		return nil, errors.New("give the profile's ID")
	}
	a.SignInBehavior = nil
	if v["redirect"] == "NEVER" {
		a.SignInBehavior = &ci.SignInBehavior{RedirectCondition: "NEVER"}
	}
	return a, nil
}

func (p iamSSOAssignmentsProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	a, err := ssoAssignmentInput(v, &ci.InboundSsoAssignment{Customer: resourcemanager.CustomerName})
	if err != nil {
		return "", err
	}
	if v["targetType"] == "orgUnit" {
		a.TargetOrgUnit = "orgUnits/" + strings.TrimPrefix(strings.TrimSpace(v["orgUnit"]), "orgUnits/")
	} else {
		name, err := p.registry.LookupGroup(strings.TrimSpace(v["group"]), "")
		if err != nil {
			return "", err
		}
		a.TargetGroup = name
		if a.Rank, err = strconv.ParseInt(strings.TrimSpace(v["rank"]), 10, 64); err != nil {
			return "", fmt.Errorf("priority %q is not a number", v["rank"])
		}
	}
	out, err := p.registry.CreateSSOAssignment(*a)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(out.Name, "inboundSsoAssignments/"), nil
}

func (p iamSSOAssignmentsProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	a, err := p.registry.GetSSOAssignment("inboundSsoAssignments/" + path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the SSO profile assignment: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "Resource name", Value: a.Name},
		{Label: "Target", Value: p.ssoTargetLabel(a)},
		{Label: "SSO profile", Value: ssoModeLabels[a.SsoMode]},
		{Label: "Profile ID", Value: orDash(ssoProfileOf(a))},
		{Label: "Sign-in", Value: redirectLabel(a)},
	}
	fields := []console.Field{ssoModeField(a.SsoMode),
		{Name: "profile", Label: "Profile ID", Type: "text", Default: ssoProfileOf(a)},
		ssoRedirectField(map[bool]string{true: "NEVER", false: "Redirect"}[redirectLabel(a) == "Never redirect"])}
	if a.TargetGroup != "" {
		props = append(props, console.Property{Label: "Priority", Value: strconv.FormatInt(a.Rank, 10)})
		fields = append(fields, console.Field{Name: "rank", Label: "Priority", Type: "text", Default: strconv.FormatInt(a.Rank, 10), Pattern: `[1-9][0-9]*`,
			Help: "1 is the highest; the other groups' assignments shift."})
	}
	return console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Assignment", Properties: props}}},
	}, Edit: &console.EditForm{Label: "Edit assignment", Fields: fields}}, nil
}

func (p iamSSOAssignmentsProvider) Edit(_ context.Context, _ string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("only an assignment can be edited")
	}
	name := "inboundSsoAssignments/" + path[0]
	cur, err := p.registry.GetSSOAssignment(name)
	if err != nil {
		return err
	}
	a, err := ssoAssignmentInput(v, &ci.InboundSsoAssignment{})
	if err != nil {
		return err
	}
	mask := []string{"sso_mode", "saml_sso_info", "oidc_sso_info", "sign_in_behavior"}
	if cur.TargetGroup != "" && strings.TrimSpace(v["rank"]) != "" {
		if a.Rank, err = strconv.ParseInt(strings.TrimSpace(v["rank"]), 10, 64); err != nil {
			return fmt.Errorf("priority %q is not a number", v["rank"])
		}
		if a.Rank != cur.Rank {
			mask = append(mask, "rank")
		}
	}
	_, err = p.registry.UpdateSSOAssignment(name, *a, mask)
	return err
}

func (p iamSSOAssignmentsProvider) Delete(_ context.Context, _ string, row string) error {
	return p.registry.DeleteSSOAssignment("inboundSsoAssignments/" + row)
}

var _ console.Creator = iamSSOAssignmentsProvider{}
var _ console.Deleter = iamSSOAssignmentsProvider{}
var _ console.Driller = iamSSOAssignmentsProvider{}
var _ console.Editor = iamSSOAssignmentsProvider{}
