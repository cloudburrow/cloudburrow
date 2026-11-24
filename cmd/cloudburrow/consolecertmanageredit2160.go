package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Certificate Manager's edits (#2160): each resource's page has Edit, the
// fields Google's Update methods change (certificate-manager/docs/certificates,
// .../maps, .../dns-authorizations and .../issuance-configs, "Update"
// sections, read 2026-11-22, give gcloud's update commands: a
// certificate's description, labels and, for a self-managed one, its
// certificate and private key; a map's description and labels; a map
// entry's description, labels and certificates; a DNS authorization's and
// an issuance config's description and labels). The Edit forms' layout is
// UNVERIFIED (re-read 2026-11-22: certificate-manager/docs; the docs give no Console steps for them).

// ccmPatch is Update*: PATCH name?updateMask=... with body.
func (c ccmScreen) ccmPatch(ctx context.Context, name string, mask []string, body map[string]any) error {
	if len(mask) == 0 {
		return nil
	}
	_, err := c.v1(ctx, "PATCH", "/v1/"+name+"?updateMask="+url.QueryEscape(strings.Join(mask, ",")), body)
	return err
}

func ccmLabels(o map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range vmMap(o, "labels") {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// ccmEditFields are Description and Labels, prefilled from o.
func ccmEditFields(o map[string]any) []console.Field {
	return []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."},
		arLabelsField(console.FormatMap(ccmLabels(o))),
	}
}

// ccmEditBody is the mask and body of an Edit's description and labels,
// with only what changed from o.
func ccmEditBody(o map[string]any, v map[string]string) ([]string, map[string]any, error) {
	var mask []string
	body := map[string]any{}
	if d := strings.TrimSpace(v["description"]); d != vpcStr(o, "description") {
		mask, body["description"] = append(mask, "description"), d
	}
	if lv, ok := v["labels"]; ok {
		labels, err := console.ParseMap(lv)
		if err != nil {
			return nil, nil, fmt.Errorf("Labels: %v", err)
		}
		if labelsText(labels) != labelsText(ccmLabels(o)) {
			l := map[string]any{}
			for k, x := range labels {
				l[k] = x
			}
			mask, body["labels"] = append(mask, "labels"), l
		}
	}
	return mask, body, nil
}

func ccmLabelsProperty(o map[string]any) console.Property {
	return console.Property{Label: "Labels", Value: vpcDash(labelsText(ccmLabels(o)))}
}

// ccmEdit is the Edit form of a resource page.
func ccmEdit(title string, fields []console.Field) *console.EditForm {
	return &console.EditForm{Label: "Edit", Title: title, Submit: "Save", Fields: fields}
}

// certEditForm is a certificate's Edit: a self-managed one also takes a
// new certificate and key.
func certEditForm(o map[string]any) *console.EditForm {
	fields := ccmEditFields(o)
	if len(vmMap(o, "managed")) == 0 {
		fields = append(fields,
			console.Field{Name: "certificate", Label: "Certificate", Type: "textarea", Help: "Optional: a new PEM certificate, then its chain, to replace the certificate."},
			console.Field{Name: "privateKey", Label: "Private key", Type: "textarea", Help: "The new certificate's PEM private key."})
	}
	return ccmEdit("Edit certificate", fields)
}

func (p ccmCertsProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a certificate")
	}
	name := ccmName(project, path, "certificates")
	o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
	if err != nil {
		return err
	}
	mask, body, err := ccmEditBody(o, v)
	if err != nil {
		return err
	}
	if c := strings.TrimSpace(v["certificate"]); c != "" {
		if strings.TrimSpace(v["privateKey"]) == "" {
			return errors.New("Private key: give the new certificate's key")
		}
		mask, body["selfManaged"] = append(mask, "selfManaged"), map[string]any{"pemCertificate": v["certificate"], "pemPrivateKey": v["privateKey"]}
	}
	return p.ccmPatch(ctx, name, mask, body)
}

func (p ccmMapsProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	switch {
	case len(path) == 2:
		name := ccmName(project, path, "certificateMaps")
		o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
		if err != nil {
			return err
		}
		mask, body, err := ccmEditBody(o, v)
		if err != nil {
			return err
		}
		return p.ccmPatch(ctx, name, mask, body)
	case len(path) == 4 && path[2] == "entry":
		name := ccmName(project, path, "certificateMaps") + "/certificateMapEntries/" + path[3]
		o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
		if err != nil {
			return err
		}
		mask, body, err := ccmEditBody(o, v)
		if err != nil {
			return err
		}
		if cv, ok := v["certificates"]; ok {
			var certs []any
			for _, c := range vpcList(cv) {
				certs = append(certs, "projects/"+project+"/locations/global/certificates/"+fmt.Sprint(c))
			}
			if strings.Join(lastAll(vpcStrs(o, "certificates")), ",") != strings.Join(lastAll(anyStrings(certs)), ",") {
				if len(certs) == 0 {
					return errors.New("Certificates: give at least one")
				}
				mask, body["certificates"] = append(mask, "certificates"), certs
			}
		}
		return p.ccmPatch(ctx, name, mask, body)
	}
	return errors.New("Edit applies to a certificate map or an entry")
}

func anyStrings(l []any) []string {
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// entryDetail is a map entry's page (GetCertificateMapEntry), with Edit.
func (p ccmMapsProvider) entryDetail(ctx context.Context, project string, path []string) (console.Detail, error) {
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "certificateMaps")+"/certificateMapEntries/"+path[3], nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	host := vpcStr(o, "hostname")
	if vpcStr(o, "matcher") == "PRIMARY" {
		host = "Primary entry"
	}
	props := []console.Property{{Label: "Name", Value: path[3]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Certificate map", Value: path[1]}, {Label: "Hostname", Value: vpcDash(host)},
		{Label: "Certificates", Value: vpcDash(strings.Join(lastAll(vpcStrs(o, "certificates")), ", "))},
		{Label: "State", Value: vpcTitle(strings.ToLower(vpcOr(vpcStr(o, "state"), "ACTIVE")))}, ccmLabelsProperty(o),
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	fields := append(ccmEditFields(o), console.Field{Name: "certificates", Label: "Certificates", Type: "text", Required: true,
		Default: strings.Join(lastAll(vpcStrs(o, "certificates")), ", "), Help: "Certificates' names (global, Default scope), comma separated."})
	return console.Detail{Title: path[3], Heading: "Certificate map entry details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit:     ccmEdit("Edit certificate map entry", fields)}, nil
}

// --- DNS authorizations' and issuance configs' pages --------------------------

func (p ccmDNSAuthProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a DNS authorization is addressed by its location and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "dnsAuthorizations"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	rec := vmMap(o, "dnsResourceRecord")
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])}, {Label: "Domain", Value: vpcStr(o, "domain")},
		{Label: "Type", Value: vpcTitle(strings.ToLower(strings.ReplaceAll(vpcOr(vpcStr(o, "type"), "FIXED_RECORD"), "_", " ")))},
		{Label: "DNS record name", Value: vpcDash(vpcStr(rec, "name"))}, {Label: "DNS record type", Value: vpcDash(vpcStr(rec, "type"))},
		{Label: "DNS record data", Value: vpcDash(vpcStr(rec, "data"))}, ccmLabelsProperty(o), {Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "DNS authorization details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit:     ccmEdit("Edit DNS authorization", ccmEditFields(o))}, nil
}

func (p ccmDNSAuthProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("DNS authorization", path)
	a.Leaves = true
	return []console.Action{a}
}

func (p ccmDNSAuthProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	return p.ccmEditSimple(ctx, project, path, "dnsAuthorizations", v)
}

// ccmEditSimple is an Edit of description and labels only.
func (c ccmScreen) ccmEditSimple(ctx context.Context, project string, path []string, coll string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to one resource")
	}
	name := ccmName(project, path, coll)
	o, err := c.v1(ctx, "GET", "/v1/"+name, nil)
	if err != nil {
		return err
	}
	mask, body, err := ccmEditBody(o, v)
	if err != nil {
		return err
	}
	return c.ccmPatch(ctx, name, mask, body)
}

func (p ccmIssuanceProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a certificate issuance config is addressed by its location and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "certificateIssuanceConfigs"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	pool := vpcStr(vmMap(vmMap(o, "certificateAuthorityConfig"), "certificateAuthorityServiceConfig"), "caPool")
	rot := num2s(o, "rotationWindowPercentage")
	if rot != "" {
		rot += "%"
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])}, {Label: "CA pool", Value: vpcDash(pool)},
		{Label: "Lifetime", Value: vpcDash(vpcStr(o, "lifetime"))}, {Label: "Rotation window", Value: vpcDash(rot)},
		{Label: "Key algorithm", Value: vpcDash(vpcStr(o, "keyAlgorithm"))}, ccmLabelsProperty(o), {Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "Certificate issuance config details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit certificate issuance config", Submit: "Save", Fields: ccmEditFields(o),
			Note: "The CA pool, lifetime, rotation window and key algorithm cannot be changed: create a new issuance config."}}, nil
}

func (p ccmIssuanceProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("certificate issuance config", path)
	a.Leaves = true
	return []console.Action{a}
}

func (p ccmIssuanceProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	return p.ccmEditSimple(ctx, project, path, "certificateIssuanceConfigs", v)
}
