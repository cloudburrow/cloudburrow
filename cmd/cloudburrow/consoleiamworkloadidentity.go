package main

// IAM & Admin > Workload Identity Federation (#1941): the selected
// project's workload identity pools, as Google's console lists them (Name,
// Pool ID, Status, Description), with Create pool in Google's steps (the
// pool, an optional first provider, its attributes), a pool's page with
// its providers, Add provider (OIDC, AWS, SAML or X.509, with the attribute
// mapping and condition), a provider's page with Edit provider, Disable,
// Enable, Delete and Undelete for pools and providers, over the registry the
// IAM API serves, so a pool made here is one gcloud iam
// workload-identity-pools and Terraform see, and the API's refusals are the
// forms'.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamWorkloadIdentityProvider struct {
	registry *resourcemanager.Registry
}

func (iamWorkloadIdentityProvider) ID() string    { return "iam-workload-identity" }
func (iamWorkloadIdentityProvider) Title() string { return "Workload Identity Federation" }

const iamWorkloadIdentityNote = "Workload identity pools of this project. A workload exchanges its identity provider's token at this instance's " +
	"Security Token Service for a federated token, then reaches resources directly as a principal:// or principalSet:// member, " +
	"or impersonates a service account. A deleted pool or provider can be restored for 30 days."

const wifIDPattern = `^[a-z0-9\-]{4,32}$`

// Provider types, as Google's Select a provider names them.
const (
	wifTypeOIDC = "OpenID Connect (OIDC)"
	wifTypeAWS  = "AWS"
	wifTypeSAML = "SAML"
	wifTypeX509 = "X.509"
)

func wifStatus(state string, disabled bool) string {
	switch {
	case state == "DELETED":
		return "DELETED"
	case disabled:
		return "DISABLED"
	}
	return "ENABLED"
}

func needProject(project string) error {
	if project == "" {
		return errors.New("choose a project in the toolbar")
	}
	return nil
}

func (p iamWorkloadIdentityProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Pool ID", Columns: []string{"Name", "Description"}, Noun: "workload identity pools", Note: iamWorkloadIdentityNote}
	if project == "" {
		l.Unavailable = "choose a project in the toolbar"
		return l, nil
	}
	pools, err := p.registry.WorkloadIdentityPools(project, true)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, pool := range pools {
		row := console.Resource{Name: lastSegment(pool.Name), Status: wifStatus(pool.State, pool.Disabled),
			Fields: map[string]string{"Name": pool.DisplayName, "Description": pool.Description}}
		row.Actions = p.Actions(row)
		l.Items = append(l.Items, row)
	}
	l.Total = len(l.Items)
	return l, nil
}

// providerFields are Add provider's fields; prefix names their section.
func providerFields(section, attrs string, required bool) []console.Field {
	show := func(t string) string { return "providerType=" + t }
	return []console.Field{
		{Name: "providerType", Label: "Select a provider", Type: "select", Section: section, Required: required,
			Options: []string{wifTypeOIDC, wifTypeAWS, wifTypeSAML, wifTypeX509}},
		{Name: "providerDisplayName", Label: "Provider name", Type: "text", Section: section, Help: "At most 32 characters."},
		{Name: "providerId", Label: "Provider ID", Type: "text", Section: section, Pattern: wifIDPattern, DeriveFrom: "providerDisplayName", Required: required,
			Help: "4 to 32 lowercase letters, digits and dashes; the prefix gcp- is Google's. It cannot be changed."},
		{Name: "issuerUri", Label: "Issuer (URL)", Type: "text", Section: section, ShowWhen: show(wifTypeOIDC),
			Help: "The OIDC issuer, https://… (a loopback http:// issuer is accepted locally). Its token's iss must match."},
		{Name: "allowedAudiences", Label: "Allowed audiences", Type: "textarea", Section: section, ShowWhen: show(wifTypeOIDC),
			Help: "One per line, at most 10. Empty: the default audience, the provider's full resource name."},
		{Name: "jwksJson", Label: "JWK file (JSON)", Type: "textarea", Section: section, ShowWhen: show(wifTypeOIDC), Upload: "Upload JWK file",
			Help: "Optional: the issuer's public keys as a JSON Web Key Set. Without it, the keys come from the issuer's discovery document."},
		{Name: "accountId", Label: "AWS account ID", Type: "text", Section: section, ShowWhen: show(wifTypeAWS), Pattern: `^[0-9]{12}$`,
			Help: "The 12-digit AWS account ID."},
		{Name: "idpMetadataXml", Label: "IdP metadata file (XML)", Type: "textarea", Section: section, ShowWhen: show(wifTypeSAML), Upload: "Upload metadata file",
			Help: "The SAML 2.0 identity provider's metadata, with its entity ID and signing certificates."},
		{Name: "trustAnchors", Label: "Trust anchors (PEM)", Type: "textarea", Section: section, ShowWhen: show(wifTypeX509),
			Help: "The root certificates client certificates must chain to, PEM-encoded, one after another."},
		{Name: "attributeMapping", Label: "Attribute mapping", Type: "map", Section: attrs,
			KeyPattern: `^(google\.(subject|groups|display_name|posix_username)|attribute\.[a-z0-9_]{1,100})$`,
			KeyHelp:    "google.subject, google.groups, google.display_name, google.posix_username or attribute.NAME",
			Help:       "Each key is a Google attribute, each value a CEL expression over assertion, such as google.subject = assertion.sub. OIDC and SAML need google.subject; AWS and X.509 have Google's defaults."},
		{Name: "attributeCondition", Label: "Attribute condition", Type: "textarea", Section: attrs,
			Help: "Optional CEL condition over assertion, google and attribute that a credential must meet, such as 'admins' in google.groups."},
	}
}

// providerFromForm is the provider the form's values describe.
func providerFromForm(v map[string]string) (resourcemanager.WorkloadIdentityPoolProvider, error) {
	out := resourcemanager.WorkloadIdentityPoolProvider{DisplayName: v["providerDisplayName"], Description: v["providerDescription"],
		AttributeCondition: strings.TrimSpace(v["attributeCondition"])}
	m, err := console.ParseMap(v["attributeMapping"])
	if err != nil {
		return out, err
	}
	if len(m) > 0 {
		out.AttributeMapping = m
	}
	switch v["providerType"] {
	case wifTypeOIDC, "":
		var auds []string
		for _, a := range strings.Split(v["allowedAudiences"], "\n") {
			if a = strings.TrimSpace(a); a != "" {
				auds = append(auds, a)
			}
		}
		out.Oidc = &resourcemanager.WIFOidc{IssuerURI: strings.TrimSpace(v["issuerUri"]), AllowedAudiences: auds, JwksJSON: strings.TrimSpace(v["jwksJson"])}
	case wifTypeAWS:
		out.Aws = &resourcemanager.WIFAws{AccountID: strings.TrimSpace(v["accountId"])}
	case wifTypeSAML:
		out.Saml = &resourcemanager.WIFSaml{IdpMetadataXML: strings.TrimSpace(v["idpMetadataXml"])}
	case wifTypeX509:
		ts := &resourcemanager.WIFTrustStore{}
		for _, c := range splitPEM(v["trustAnchors"]) {
			ts.TrustAnchors = append(ts.TrustAnchors, resourcemanager.WIFPem{PemCertificate: c})
		}
		out.X509 = &resourcemanager.WIFX509{TrustStore: ts}
	default:
		return out, fmt.Errorf("unknown provider type %q", v["providerType"])
	}
	return out, nil
}

// splitPEM splits concatenated PEM certificates.
func splitPEM(s string) []string {
	var out []string
	const end = "-----END CERTIFICATE-----"
	for {
		i := strings.Index(s, end)
		if i < 0 {
			break
		}
		if c := strings.TrimSpace(s[:i+len(end)]); c != "" {
			out = append(out, c+"\n")
		}
		s = s[i+len(end):]
	}
	return out
}

// CreateForm is Google's New workload provider and pool, in its steps:
// the pool, a provider (optional here, Google's Continue), and the
// provider's attributes.
func (iamWorkloadIdentityProvider) CreateForm() (string, []console.Field) {
	const pool = "Create an identity pool"
	const prov = "Add a provider to pool (optional)"
	const attrs = "Configure provider attributes"
	fields := []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Section: pool, Help: "At most 32 characters."},
		{Name: "poolId", Label: "Pool ID", Type: "text", Required: true, Pattern: wifIDPattern, Section: pool, DeriveFrom: "displayName",
			Help: "4 to 32 lowercase letters, digits and dashes; the prefix gcp- is Google's. It cannot be changed, nor reused for 30 days after a delete."},
		{Name: "description", Label: "Description", Type: "text", Section: pool, Help: "At most 256 characters."},
	}
	return "New workload provider and pool", append(fields, providerFields(prov, attrs, false)...)
}

func (iamWorkloadIdentityProvider) CreateTitle() string { return "Create pool" }

func (iamWorkloadIdentityProvider) CreateOnPage() bool { return true }

func (p iamWorkloadIdentityProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	if err := needProject(project); err != nil {
		return "", err
	}
	id := strings.TrimSpace(values["poolId"])
	var prov resourcemanager.WorkloadIdentityPoolProvider
	provID := strings.TrimSpace(values["providerId"])
	if provID != "" {
		var err error
		if prov, err = providerFromForm(values); err != nil {
			return "", err
		}
	}
	if _, err := p.registry.CreateWorkloadIdentityPool(project, id, resourcemanager.WorkloadIdentityPool{
		DisplayName: values["displayName"], Description: values["description"]}); err != nil {
		return "", err
	}
	if provID != "" {
		if _, err := p.registry.CreateWorkloadIdentityPoolProvider(project, id, provID, prov); err != nil {
			// As on Google, the pool is made first: it stays, and Add
			// provider on its page tries again.
			return id, fmt.Errorf("created pool %s, but not its provider: %w", id, err)
		}
	}
	return id, nil
}

func (p iamWorkloadIdentityProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	if err := needProject(project); err != nil {
		return console.Detail{Prompt: err.Error()}, nil
	}
	pool, err := p.registry.WorkloadIdentityPool(project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the pool: " + consoleErr(err)}, nil
	}
	if len(path) == 2 {
		return p.providerDetail(project, pool, path[1])
	}
	props := []console.Property{
		{Label: "Pool ID", Value: path[0]},
		{Label: "Name", Value: orDash(pool.DisplayName)},
		{Label: "Status", Value: wifStatus(pool.State, pool.Disabled)},
		{Label: "Description", Value: orDash(pool.Description)},
		{Label: "IAM principal", Value: "principalSet://iam.googleapis.com/" + pool.Name + "/*"},
		{Label: "Resource name", Value: pool.Name},
	}
	if pool.ExpireTime != "" {
		props = append(props, console.Property{Label: "Purged after", Value: pool.ExpireTime})
	}
	provs := console.Listing{NameColumn: "Provider ID", Columns: []string{"Name", "Type", "Issuer"}, Noun: "providers"}
	list, err := p.registry.WorkloadIdentityPoolProviders(project, path[0], true)
	if err != nil {
		provs.Unavailable = consoleErr(err)
	}
	for _, v := range list {
		typ, issuer := providerType(v)
		r := console.Resource{Name: lastSegment(v.Name), Status: wifStatus(v.State, v.Disabled),
			Fields: map[string]string{"Name": v.DisplayName, "Type": typ, "Issuer": issuer}}
		r.Actions = p.DetailActions(ctx, project, []string{path[0], r.Name})
		provs.Items = append(provs.Items, r)
	}
	provs.Total = len(provs.Items)
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Pool", Properties: props}}},
		{ID: "providers", Label: "Providers", Listing: provs},
	}}
	if pool.State != "DELETED" {
		d.Edit = &console.EditForm{Label: "Edit pool", Note: "The pool ID cannot change, as on Google.", Fields: []console.Field{
			{Name: "displayName", Label: "Name", Type: "text", Default: pool.DisplayName, Help: "At most 32 characters."},
			{Name: "description", Label: "Description", Type: "text", Default: pool.Description, Help: "At most 256 characters."},
		}}
	}
	return d, nil
}

func providerType(v resourcemanager.WorkloadIdentityPoolProvider) (typ, issuer string) {
	switch {
	case v.Oidc != nil:
		return wifTypeOIDC, v.Oidc.IssuerURI
	case v.Aws != nil:
		return wifTypeAWS, v.Aws.AccountID
	case v.Saml != nil:
		return wifTypeSAML, ""
	case v.X509 != nil:
		return wifTypeX509, ""
	}
	return "", ""
}

func (p iamWorkloadIdentityProvider) providerDetail(project string, pool resourcemanager.WorkloadIdentityPool, id string) (console.Detail, error) {
	v, err := p.registry.WorkloadIdentityPoolProvider(project, lastSegment(pool.Name), id)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the provider: " + consoleErr(err)}, nil
	}
	typ, _ := providerType(v)
	props := []console.Property{
		{Label: "Provider ID", Value: id},
		{Label: "Name", Value: orDash(v.DisplayName)},
		{Label: "Status", Value: wifStatus(v.State, v.Disabled)},
		{Label: "Type", Value: typ},
		{Label: "Description", Value: orDash(v.Description)},
		{Label: "Audience", Value: "//iam.googleapis.com/" + v.Name},
	}
	var cfg []console.Property
	switch {
	case v.Oidc != nil:
		cfg = append(cfg, console.Property{Label: "Issuer (URL)", Value: v.Oidc.IssuerURI},
			console.Property{Label: "Allowed audiences", Value: orDash(strings.Join(v.Oidc.AllowedAudiences, ", "))},
			console.Property{Label: "JWK file", Value: map[bool]string{true: "Uploaded", false: "From the issuer's discovery document"}[v.Oidc.JwksJSON != ""]})
	case v.Aws != nil:
		cfg = append(cfg, console.Property{Label: "AWS account ID", Value: v.Aws.AccountID})
	case v.Saml != nil:
		cfg = append(cfg, console.Property{Label: "IdP metadata", Value: strconv.Itoa(len(v.Saml.IdpMetadataXML)) + " characters"})
	case v.X509 != nil && v.X509.TrustStore != nil:
		cfg = append(cfg, console.Property{Label: "Trust anchors", Value: strconv.Itoa(len(v.X509.TrustStore.TrustAnchors))})
	}
	var attrs []console.Property
	for _, k := range mappingKeys(v.AttributeMapping) {
		attrs = append(attrs, console.Property{Label: k, Value: v.AttributeMapping[k]})
	}
	attrs = append(attrs, console.Property{Label: "Attribute condition", Value: orDash(v.AttributeCondition)})
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Provider", Properties: props}, {Heading: "Provider details", Properties: cfg}, {Heading: "Attribute mapping", Properties: attrs}}},
	}}
	if v.State != "DELETED" {
		fields := []console.Field{
			{Name: "providerDisplayName", Label: "Provider name", Type: "text", Default: v.DisplayName, Help: "At most 32 characters."},
			{Name: "providerDescription", Label: "Description", Type: "text", Default: v.Description, Help: "At most 256 characters."},
		}
		if v.Oidc != nil {
			fields = append(fields,
				console.Field{Name: "issuerUri", Label: "Issuer (URL)", Type: "text", Default: v.Oidc.IssuerURI},
				console.Field{Name: "allowedAudiences", Label: "Allowed audiences", Type: "textarea", Default: strings.Join(v.Oidc.AllowedAudiences, "\n")},
				console.Field{Name: "jwksJson", Label: "JWK file (JSON)", Type: "textarea", Default: v.Oidc.JwksJSON})
		}
		fields = append(fields,
			console.Field{Name: "attributeMapping", Label: "Attribute mapping", Type: "map", Default: console.FormatMap(v.AttributeMapping)},
			console.Field{Name: "attributeCondition", Label: "Attribute condition", Type: "textarea", Default: v.AttributeCondition})
		d.Edit = &console.EditForm{Label: "Edit provider", Fields: fields, Note: "The provider ID and type cannot change, as on Google."}
	}
	return d, nil
}

func mappingKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// attribute.* sorts before google.*; Google's form lists google.* first.
	sort.Slice(out, func(i, j int) bool {
		gi, gj := strings.HasPrefix(out[i], "google."), strings.HasPrefix(out[j], "google.")
		if gi != gj {
			return gi
		}
		return out[i] < out[j]
	})
	return out
}

func (p iamWorkloadIdentityProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	if err := needProject(project); err != nil {
		return err
	}
	switch len(path) {
	case 1:
		_, err := p.registry.UpdateWorkloadIdentityPool(project, path[0], resourcemanager.WorkloadIdentityPool{
			DisplayName: values["displayName"], Description: values["description"]}, []string{"displayName", "description"})
		return err
	case 2:
		cur, err := p.registry.WorkloadIdentityPoolProvider(project, path[0], path[1])
		if err != nil {
			return err
		}
		in := resourcemanager.WorkloadIdentityPoolProvider{DisplayName: values["providerDisplayName"], Description: values["providerDescription"],
			AttributeCondition: strings.TrimSpace(values["attributeCondition"])}
		if in.AttributeMapping, err = console.ParseMap(values["attributeMapping"]); err != nil {
			return err
		}
		mask := []string{"displayName", "description", "attributeMapping", "attributeCondition"}
		if cur.Oidc != nil {
			o, _ := providerFromForm(map[string]string{"providerType": wifTypeOIDC, "issuerUri": values["issuerUri"],
				"allowedAudiences": values["allowedAudiences"], "jwksJson": values["jwksJson"]})
			in.Oidc = o.Oidc
			mask = append(mask, "oidc")
		}
		_, err = p.registry.UpdateWorkloadIdentityPoolProvider(project, path[0], path[1], in, mask)
		return err
	}
	return fmt.Errorf("only a pool or a provider can be edited")
}

func (p iamWorkloadIdentityProvider) Delete(_ context.Context, project, row string) error {
	if err := needProject(project); err != nil {
		return err
	}
	_, err := p.registry.DeleteWorkloadIdentityPool(project, row)
	return err
}

// Actions are a pool row's Disable or Enable, or a deleted one's Restore.
func (iamWorkloadIdentityProvider) Actions(r console.Resource) []console.Action {
	switch r.Status {
	case "DELETED":
		return []console.Action{{ID: "undelete", Label: "Restore"}}
	case "DISABLED":
		return []console.Action{{ID: "enable", Label: "Enable"}}
	}
	return []console.Action{{ID: "disable", Label: "Disable", Confirm: "the pool exchanges no token, and its federated tokens grant nothing until it is enabled again"}}
}

func (p iamWorkloadIdentityProvider) Act(ctx context.Context, project, row, action string) error {
	return p.ActAt(ctx, project, []string{row}, action, nil)
}

// DetailActions are a pool's Add provider and its row actions, and a
// provider's Disable or Enable, Delete, or Restore.
func (p iamWorkloadIdentityProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if project == "" {
		return nil
	}
	switch len(path) {
	case 1:
		pool, err := p.registry.WorkloadIdentityPool(project, path[0])
		if err != nil {
			return nil
		}
		st := wifStatus(pool.State, pool.Disabled)
		var out []console.Action
		if st != "DELETED" {
			out = append(out, console.Action{ID: "add-provider", Label: "Add provider", Fields: providerFields("Provider details", "Configure provider attributes", true)})
		}
		return append(out, p.Actions(console.Resource{Status: st})...)
	case 2:
		v, err := p.registry.WorkloadIdentityPoolProvider(project, path[0], path[1])
		if err != nil {
			return nil
		}
		switch wifStatus(v.State, v.Disabled) {
		case "DELETED":
			return []console.Action{{ID: "undelete-provider", Label: "Restore"}}
		case "DISABLED":
			return []console.Action{{ID: "enable-provider", Label: "Enable"}, {ID: "delete-provider", Label: "Delete", Destructive: true}}
		}
		return []console.Action{{ID: "disable-provider", Label: "Disable", Confirm: "the provider exchanges no token; the tokens it issued still grant"},
			{ID: "delete-provider", Label: "Delete", Destructive: true}}
	}
	return nil
}

func (p iamWorkloadIdentityProvider) ActAt(_ context.Context, project string, path []string, action string, values map[string]string) error {
	if err := needProject(project); err != nil {
		return err
	}
	if len(path) == 0 {
		return fmt.Errorf("unknown action %q", action)
	}
	pool := path[0]
	setPool := func(disabled bool) error {
		_, err := p.registry.UpdateWorkloadIdentityPool(project, pool, resourcemanager.WorkloadIdentityPool{Disabled: disabled}, []string{"disabled"})
		return err
	}
	if len(path) == 1 {
		switch action {
		case "disable":
			return setPool(true)
		case "enable":
			return setPool(false)
		case "undelete":
			_, err := p.registry.UndeleteWorkloadIdentityPool(project, pool)
			return err
		case "add-provider":
			v, err := providerFromForm(values)
			if err != nil {
				return err
			}
			_, err = p.registry.CreateWorkloadIdentityPoolProvider(project, pool, strings.TrimSpace(values["providerId"]), v)
			return err
		}
		return fmt.Errorf("unknown action %q", action)
	}
	prov := path[1]
	setProv := func(disabled bool) error {
		_, err := p.registry.UpdateWorkloadIdentityPoolProvider(project, pool, prov, resourcemanager.WorkloadIdentityPoolProvider{Disabled: disabled}, []string{"disabled"})
		return err
	}
	switch action {
	case "disable-provider":
		return setProv(true)
	case "enable-provider":
		return setProv(false)
	case "delete-provider":
		_, err := p.registry.DeleteWorkloadIdentityPoolProvider(project, pool, prov)
		return err
	case "undelete-provider":
		_, err := p.registry.UndeleteWorkloadIdentityPoolProvider(project, pool, prov)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var _ console.Creator = iamWorkloadIdentityProvider{}
var _ console.PageCreator = iamWorkloadIdentityProvider{}
var _ console.Deleter = iamWorkloadIdentityProvider{}
var _ console.Driller = iamWorkloadIdentityProvider{}
var _ console.Editor = iamWorkloadIdentityProvider{}
var _ console.Actor = iamWorkloadIdentityProvider{}
