package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Certificate Manager's Trust configs tab and Network Security's
// Authentication Configuration page (#2160), as
// https://docs.cloud.google.com/certificate-manager/docs/trust-configs and
// .../load-balancing/docs/https/setting-up-mtls-ccm give their Console
// steps (read 2026-11-22):
//
//   - Certificate Manager › Trust configs: Add Trust Config opens Create
//     Trust Config (Name, Description, Labels, Location Global or
//     Regional, the Trust store section's trust anchors and intermediate
//     CAs, Allowlisted certificates, each uploaded as a PEM file or
//     pasted), Create; a trust config's page is Trust Config details,
//     with Edit (Save) and Delete;
//   - Network Security › Authentication Configuration › Client
//     Authentication: Create (a name, Location, Client Authentication
//     mode Load balancing, Client Validation Mode Reject invalid or Allow
//     invalid or missing client certificate, the trust config). Google's
//     third mode, Allow validation relaxations, is not in the v1 API this
//     instance serves (docs/gaps.md).
//
// The lists' columns are UNVERIFIED (the docs name none).

const trustNote = "Trust configs validate client certificates for frontend mutual TLS: a Client Authentication resource (Network Security) names one, and a target HTTPS proxy's serverTlsPolicy names that, so this instance's load balancers check each client's certificate against it."

// --- Trust configs ------------------------------------------------------------

type ccmTrustProvider struct{ ccmScreen }

func (ccmTrustProvider) ID() string    { return "ccm-trustconfigs" }
func (ccmTrustProvider) Title() string { return "Certificate Manager" }

// trustCounts are a trust config's anchors, intermediates and allowlisted
// certificates.
func trustCounts(o map[string]any) (anchors, inter, allow int) {
	for _, ts := range vmMaps(o, "trustStores") {
		anchors += len(vmMaps(ts, "trustAnchors"))
		inter += len(vmMaps(ts, "intermediateCas"))
	}
	return anchors, inter, len(vmMaps(o, "allowlistedCertificates"))
}

func (p ccmTrustProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Trust anchors", "Intermediate CAs", "Allowlisted certificates", "Labels"}, NameColumn: "Name",
		Noun: "trust configs", Note: trustNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Trust configs are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "trustConfigs")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		a, i, w := trustCounts(o)
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "Trust anchors": fmt.Sprint(a), "Intermediate CAs": fmt.Sprint(i),
				"Allowlisted certificates": fmt.Sprint(w), "Labels": vpcDash(vmLabelsText(o))},
			Actions: []console.Action{ccmDelete("trust config", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (ccmTrustProvider) CreateOnPage() bool   { return true }
func (ccmTrustProvider) CreateTitle() string  { return "Create Trust Config" }
func (ccmTrustProvider) CreateLayout() string { return console.CreateLayoutPage }

// trustPEMFields are the Trust store section and the allowlist, with
// their defaults.
func trustPEMFields(anchors, inter, allow string) []console.Field {
	const pemHelp = "One or more PEM-encoded certificates (-----BEGIN CERTIFICATE-----), uploaded as a file or pasted."
	return []console.Field{
		{Name: "trustAnchors", Label: "Trust anchors", Type: "textarea", Section: "Trust store", Upload: "Upload", Default: anchors,
			Help: "Add trust anchor: the root certificates client certificates must chain to. " + pemHelp},
		{Name: "intermediateCas", Label: "Intermediate CAs", Type: "textarea", Section: "Trust store", Upload: "Upload", Default: inter,
			Help: "Add intermediate CA (optional): certificates that may complete a client's chain. " + pemHelp},
		{Name: "allowlisted", Label: "Allowlisted certificates", Type: "textarea", Section: "Allowlisted certificates", Upload: "Upload", Default: allow,
			Help: "Add certificate (optional): certificates that are always considered valid, such as self-signed ones. " + pemHelp},
	}
}

func (ccmTrustProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
		arLabelsField(""),
	}
	fields = append(fields, ccmLocationFields("")...)
	return "Create", append(fields, trustPEMFields("", "", "")...)
}

// trustSplitPEM is each certificate of a PEM text, re-encoded; an error names
// the field whose text is not PEM certificates.
func trustSplitPEM(label, text string) ([]string, error) {
	rest := []byte(strings.TrimSpace(text))
	var out []string
	for len(strings.TrimSpace(string(rest))) > 0 {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil || b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%s: not a PEM-encoded certificate", label)
		}
		out = append(out, string(pem.EncodeToMemory(b)))
	}
	return out, nil
}

// trustBody is a trust config's trustStores and allowlistedCertificates
// from the form.
func trustBody(v map[string]string) (map[string]any, error) {
	anchors, err := trustSplitPEM("Trust anchors", v["trustAnchors"])
	if err != nil {
		return nil, err
	}
	inter, err := trustSplitPEM("Intermediate CAs", v["intermediateCas"])
	if err != nil {
		return nil, err
	}
	allow, err := trustSplitPEM("Allowlisted certificates", v["allowlisted"])
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 && len(allow) == 0 {
		return nil, errors.New("Trust store: add a trust anchor or an allowlisted certificate")
	}
	body := map[string]any{}
	if len(anchors)+len(inter) > 0 {
		ts := map[string]any{}
		var a, i []any
		for _, c := range anchors {
			a = append(a, map[string]any{"pemCertificate": c})
		}
		for _, c := range inter {
			i = append(i, map[string]any{"pemCertificate": c})
		}
		if a != nil {
			ts["trustAnchors"] = a
		}
		if i != nil {
			ts["intermediateCas"] = i
		}
		body["trustStores"] = []any{ts}
	} else {
		body["trustStores"] = []any{}
	}
	var w []any
	for _, c := range allow {
		w = append(w, map[string]any{"pemCertificate": c})
	}
	body["allowlistedCertificates"] = w
	if w == nil {
		body["allowlistedCertificates"] = []any{}
	}
	return body, nil
}

func (p ccmTrustProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	body, err := trustBody(v)
	if err != nil {
		return "", err
	}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	labels, err := console.ParseMap(v["labels"])
	if err != nil {
		return "", err
	}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	if err := p.ccmCreate(ctx, project, ccmLocation(v), "trustConfigs", "trustConfigId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

// pemsOf joins a list's pemCertificate values.
func pemsOf(l []map[string]any) string {
	var out []string
	for _, c := range l {
		out = append(out, strings.TrimSpace(vpcStr(c, "pemCertificate")))
	}
	return strings.Join(out, "\n")
}

func (p ccmTrustProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a trust config is addressed by its location and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "trustConfigs"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	a, i, w := trustCounts(o)
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])}, {Label: "Labels", Value: vpcDash(vmLabelsText(o))},
		{Label: "Trust anchors", Value: fmt.Sprint(a)}, {Label: "Intermediate CAs", Value: fmt.Sprint(i)},
		{Label: "Allowlisted certificates", Value: fmt.Sprint(w)},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}, {Label: "Last updated", Value: vpcDash(vpcStr(o, "updateTime"))}}
	var anchors, inter []map[string]any
	for _, ts := range vmMaps(o, "trustStores") {
		anchors = append(anchors, vmMaps(ts, "trustAnchors")...)
		inter = append(inter, vmMaps(ts, "intermediateCas")...)
	}
	allow := vmMaps(o, "allowlistedCertificates")
	certs := console.Listing{Columns: []string{"Type", "Subject", "Expires"}, NameColumn: "Certificate", Noun: "certificates", Items: []console.Resource{}}
	add := func(kind string, l []map[string]any) {
		for n, c := range l {
			subj, exp := pemSummary(vpcStr(c, "pemCertificate"))
			certs.Items = append(certs.Items, console.Resource{Name: fmt.Sprintf("%s %d", kind, n+1),
				Fields: map[string]string{"Type": kind, "Subject": vpcDash(subj), "Expires": vpcDash(exp)}})
		}
	}
	add("Trust anchor", anchors)
	add("Intermediate CA", inter)
	add("Allowlisted certificate", allow)
	certs.Total = len(certs.Items)
	labels := vmLabels(o)
	return console.Detail{Title: path[1], Heading: "Trust Config details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "certificates", Label: "Certificates", Listing: certs}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit Trust Config", Submit: "Save", Page: true,
			Fields: append([]console.Field{{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."},
				arLabelsField(console.FormatMap(labels))}, trustPEMFields(pemsOf(anchors), pemsOf(inter), pemsOf(allow))...)}}, nil
}

// pemSummary is a PEM certificate's subject and expiry, for the details.
func pemSummary(p string) (string, string) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return "", ""
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "", ""
	}
	return c.Subject.String(), c.NotAfter.UTC().Format(time.RFC3339)
}

func (p ccmTrustProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("trust config", path)
	a.Leaves = true
	return []console.Action{a}
}

// Edit implements console.Editor: Save replaces the description, labels,
// trust store and allowlist.
func (p ccmTrustProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a trust config")
	}
	body, err := trustBody(v)
	if err != nil {
		return err
	}
	body["description"] = strings.TrimSpace(v["description"])
	labels, err := console.ParseMap(v["labels"])
	if err != nil {
		return err
	}
	body["labels"] = labels
	_, err = p.v1(ctx, "PATCH", "/v1/"+ccmName(project, path, "trustConfigs")+"?updateMask=description,labels,trustStores,allowlistedCertificates", body)
	return err
}

func (p ccmTrustProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "trustConfigs"), nil)
	return err
}

// --- Client Authentication (server TLS policies) ---------------------------------

type netsecClientAuthProvider struct{ ccmScreen }

func (netsecClientAuthProvider) ID() string    { return "netsecurity-clientauth" }
func (netsecClientAuthProvider) Title() string { return "Authentication Configuration" }

var clientValidationLabels = map[string]string{"REJECT_INVALID": "Reject invalid",
	"ALLOW_INVALID_OR_MISSING_CLIENT_CERT": "Allow invalid or missing client certificate"}

// netsecUsersOf are the target HTTPS proxies that name a policy.
func (p netsecClientAuthProvider) usersOf(project, name string) []string {
	out, err := p.call("GET", project, "aggregated/targetHttpsProxies", nil, nil)
	if err != nil {
		return nil
	}
	var users []string
	items, _ := out["items"].(map[string]any)
	for _, scoped := range items {
		sm, _ := scoped.(map[string]any)
		for _, it := range listAny(sm["targetHttpsProxies"]) {
			m, _ := it.(map[string]any)
			if strAny(m["serverTlsPolicy"]) == "//networksecurity.googleapis.com/"+name {
				users = append(users, strAny(m["name"]))
			}
		}
	}
	sort.Strings(users)
	return users
}

func (p netsecClientAuthProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Client Authentication mode", "Client Validation Mode", "Trust config", "In use by"}, NameColumn: "Name",
		Noun: "Client Authentication resources", Note: trustNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Client Authentication resources are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "serverTlsPolicies")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		mt := vmMap(o, "mtlsPolicy")
		mode := "Traffic Director"
		if vpcStr(mt, "clientValidationMode") != "" {
			mode = "Load balancing"
		}
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "Client Authentication mode": mode,
				"Client Validation Mode": vpcDash(clientValidationLabels[vpcStr(mt, "clientValidationMode")]),
				"Trust config":           vpcDash(vpcLast(vpcStr(mt, "clientValidationTrustConfig"))),
				"In use by":              vpcDash(strings.Join(p.usersOf(project, vpcStr(o, "name")), ", "))},
			Actions: []console.Action{ccmDelete("Client Authentication resource", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (netsecClientAuthProvider) CreateOnPage() bool   { return true }
func (netsecClientAuthProvider) CreateTitle() string  { return "Create Client Authentication" }
func (netsecClientAuthProvider) CreateLayout() string { return console.CreateLayoutPage }

func (netsecClientAuthProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[A-Za-z_\-][A-Za-z0-9_\-]{0,62}$`,
			Help: "1 to 63 letters, digits, hyphens and underscores, not starting with a digit."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
	fields = append(fields, ccmLocationFields("")...)
	return "Create", append(fields,
		console.Field{Name: "authMode", Label: "Client Authentication mode", Type: "radio", Default: "lb", Options: []string{"lb"},
			OptionLabels: map[string]string{"lb": "Load balancing"}},
		console.Field{Name: "validation", Label: "Client Validation Mode", Type: "radio", Default: "REJECT_INVALID",
			Options: []string{"REJECT_INVALID", "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"}, OptionLabels: clientValidationLabels,
			Help: "Reject invalid ends the TLS handshake of a client without a valid certificate; Allow passes it, and the backend learns the outcome from the {client_cert_*} custom header variables."},
		console.Field{Name: "trustConfig", Label: "Trust config", Type: "text", Help: "The trust config's name, in the same location (Certificate Manager › Trust configs). Required with Reject invalid."},
	)
}

func (p netsecClientAuthProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	if name == "" {
		return "", errors.New("Name: give one")
	}
	loc := ccmLocation(v)
	mt := map[string]any{"clientValidationMode": vpcOr(v["validation"], "REJECT_INVALID")}
	if tc := strings.TrimSpace(v["trustConfig"]); tc != "" {
		if !strings.Contains(tc, "/") {
			tc = "projects/" + project + "/locations/" + loc + "/trustConfigs/" + tc
		}
		mt["clientValidationTrustConfig"] = tc
	}
	body := map[string]any{"mtlsPolicy": mt}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	_, err := p.v1(ctx, "POST", "/v1/projects/"+project+"/locations/"+loc+"/serverTlsPolicies?serverTlsPolicyId="+url.QueryEscape(name), body)
	if err != nil {
		return "", err
	}
	return name, nil
}

func (p netsecClientAuthProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a Client Authentication resource is addressed by its location and name"}, nil
	}
	name := "projects/" + project + "/locations/" + path[0] + "/serverTlsPolicies/" + path[1]
	o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	mt := vmMap(o, "mtlsPolicy")
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])},
		{Label: "Client Validation Mode", Value: vpcDash(clientValidationLabels[vpcStr(mt, "clientValidationMode")])},
		{Label: "Trust config", Value: vpcDash(vpcStr(mt, "clientValidationTrustConfig"))},
		{Label: "In use by", Value: vpcDash(strings.Join(p.usersOf(project, name), ", "))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "Client Authentication details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit Client Authentication", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."},
			{Name: "validation", Label: "Client Validation Mode", Type: "radio", Default: vpcOr(vpcStr(mt, "clientValidationMode"), "REJECT_INVALID"),
				Options: []string{"REJECT_INVALID", "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"}, OptionLabels: clientValidationLabels},
			{Name: "trustConfig", Label: "Trust config", Type: "text", Default: vpcLast(vpcStr(mt, "clientValidationTrustConfig"))}}}}, nil
}

func (p netsecClientAuthProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("Client Authentication resource", path)
	a.Leaves = true
	return []console.Action{a}
}

// Edit implements console.Editor.
func (p netsecClientAuthProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a Client Authentication resource")
	}
	mt := map[string]any{"clientValidationMode": vpcOr(v["validation"], "REJECT_INVALID")}
	if tc := strings.TrimSpace(v["trustConfig"]); tc != "" {
		if !strings.Contains(tc, "/") {
			tc = "projects/" + project + "/locations/" + path[0] + "/trustConfigs/" + tc
		}
		mt["clientValidationTrustConfig"] = tc
	}
	_, err := p.v1(ctx, "PATCH", "/v1/projects/"+project+"/locations/"+path[0]+"/serverTlsPolicies/"+path[1]+"?updateMask=description,mtlsPolicy",
		map[string]any{"description": strings.TrimSpace(v["description"]), "mtlsPolicy": mt})
	return err
}

func (p netsecClientAuthProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/projects/"+project+"/locations/"+path[0]+"/serverTlsPolicies/"+path[1], nil)
	return err
}
