package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Network Security's authorization policies and client TLS policies
// (#2160), so every networksecurity.googleapis.com resource this instance
// serves is reachable from the console (the UI parity rule):
//
//   - Authorization policies (authzPolicies, the Application Load
//     Balancers' policies of load-balancing/docs/auth-policy/
//     auth-policy-overview: a target of forwarding rules, an action ALLOW,
//     DENY or CUSTOM, and HTTP rules), enforced by this instance's proxy;
//   - Service mesh authorization policies (authorizationPolicies, the
//     classic policies of Traffic Director) and Client TLS policies
//     (clientTlsPolicies) on the Authentication Configuration page.
//
// Google's docs give these as gcloud and REST steps (import from YAML);
// the console pages' layout, tabs and columns are UNVERIFIED (re-read 2026-11-22: load-balancing/docs/auth-policy/set-up-auth-policy) (docs re-read
// 2026-11-22: load-balancing/docs/auth-policy/set-up-auth-policy,
// service-mesh/docs/security/...). Their create and edit forms take the
// resource's rules as the JSON that gcloud's import YAML holds.

type netsecKind struct {
	id, title, coll, idParam, noun, nounPlural, note string
	// columns of the list, and the field each shows.
	columns []string
	column  func(o map[string]any) map[string]string
	// rulesField is the JSON field the form edits; template its example.
	fields   []string
	template string
	// form are fields the form asks for beside the JSON, and set puts
	// their values in the body (authzPolicies' action and custom
	// provider).
	form func(o map[string]any) []console.Field
	set  func(v map[string]string, body map[string]any) error
}

// authzForm is an authorization policy's action, profile and custom
// provider, as the fields of gcloud's import YAML (action, policyProfile,
// customProvider.cloudIap, customProvider.authzExtension.resources).
func authzForm(o map[string]any) []console.Field {
	action, profile, provider, ext := "ALLOW", "REQUEST_AUTHZ", "", ""
	if o != nil {
		action, profile = vpcStr(o, "action"), vpcStr(o, "policyProfile")
		cp := vmMap(o, "customProvider")
		if vmMap(cp, "cloudIap") != nil || cp["cloudIap"] != nil {
			provider = "cloudIap"
		}
		if ae := vmMap(cp, "authzExtension"); ae != nil {
			provider, ext = "authzExtension", strings.Join(listStrings(ae["resources"]), ",")
		}
	}
	return []console.Field{
		{Name: "action", Label: "Action", Type: "select", Required: true, Default: action, Options: []string{"ALLOW", "DENY", "CUSTOM"},
			Help: "CUSTOM delegates the decision to Cloud IAP or an authorization extension; the CUSTOM policies are evaluated first, then DENY, then ALLOW."},
		{Name: "policyProfile", Label: "Policy profile", Type: "select", Default: profile, Options: []string{"REQUEST_AUTHZ", "CONTENT_AUTHZ"},
			OptionLabels: map[string]string{"REQUEST_AUTHZ": "Request authorization (headers)", "CONTENT_AUTHZ": "Content authorization (headers and body, CUSTOM only)"},
			Immutable:    o != nil},
		{Name: "customProvider", Label: "Custom provider", Type: "select", Default: provider, Options: []string{"", "cloudIap", "authzExtension"},
			OptionLabels: map[string]string{"": "None", "cloudIap": "Cloud IAP", "authzExtension": "Authorization extension"},
			VisibleWhen:  map[string]string{"action": "CUSTOM"}},
		{Name: "authzExtension", Label: "Authorization extension", Type: "text", Default: ext, VisibleWhen: map[string]string{"customProvider": "authzExtension"},
			Help: "With an authorization extension provider: projects/PROJECT/locations/LOCATION/authzExtensions/EXTENSION, in the policy's location."},
	}
}

func authzSet(v map[string]string, body map[string]any) error {
	if a := strings.TrimSpace(v["action"]); a != "" {
		body["action"] = a
	}
	if pp := strings.TrimSpace(v["policyProfile"]); pp != "" {
		body["policyProfile"] = pp
	}
	provider := v["customProvider"]
	if body["action"] != "CUSTOM" {
		provider = "" // the field shows for CUSTOM only
	}
	switch provider {
	case "cloudIap":
		body["customProvider"] = map[string]any{"cloudIap": map[string]any{}}
	case "authzExtension":
		ext := strings.TrimSpace(v["authzExtension"])
		if ext == "" {
			return errors.New("Authorization extension: name the extension")
		}
		body["customProvider"] = map[string]any{"authzExtension": map[string]any{"resources": []any{ext}}}
	case "":
		if _, ok := body["customProvider"]; !ok && body["action"] == "CUSTOM" {
			return errors.New("Custom provider: the CUSTOM action needs Cloud IAP or an authorization extension")
		}
	}
	return nil
}

func listStrings(v any) []string {
	var out []string
	for _, x := range listAny(v) {
		out = append(out, strAny(x))
	}
	return out
}

var netsecKinds = map[string]netsecKind{
	"netsecurity-authz": {id: "netsecurity-authz", title: "Authorization policies", coll: "authzPolicies", idParam: "authzPolicyId",
		noun: "Authorization policy", nounPlural: "authorization policies",
		note:    "An authorization policy allows, denies or delegates the requests through the load balancers whose forwarding rules it targets, by their source (IP ranges, client certificates, and on internal load balancers the source VM's service account or secure tags) and HTTP request; this instance's proxy enforces it: CUSTOM policies first (Cloud IAP, or an authorization extension the load balancer calls), then DENY, then ALLOW (a denied request is answered 403).",
		columns: []string{"Location", "Action", "Custom provider", "Load balancing scheme", "Targets", "Rules"},
		column: func(o map[string]any) map[string]string {
			t := vmMap(o, "target")
			cp := vmMap(o, "customProvider")
			prov := "—"
			if _, ok := cp["cloudIap"]; ok {
				prov = "Cloud IAP"
			} else if ae := vmMap(cp, "authzExtension"); ae != nil {
				prov = "Authorization extension " + strings.Join(lastAllAny(listAny(ae["resources"])), ", ")
			}
			return map[string]string{"Action": vpcDash(vpcStr(o, "action")), "Custom provider": prov, "Load balancing scheme": vpcDash(vpcStr(t, "loadBalancingScheme")),
				"Targets": vpcDash(strings.Join(lastAllAny(listAny(t["resources"])), ", ")), "Rules": fmt.Sprint(len(listAny(o["httpRules"])))}
		},
		fields:   []string{"target", "httpRules"},
		template: `{"target": {"loadBalancingScheme": "INTERNAL_MANAGED", "resources": ["projects/PROJECT/regions/REGION/forwardingRules/RULE"]}, "httpRules": [{"from": {"sources": [{"resources": [{"iamServiceAccount": {"exact": "web@PROJECT.iam.gserviceaccount.com"}}, {"tagValueIdSet": {"ids": ["TAG_VALUE_ID"]}}]}]}, "to": {"operations": [{"paths": [{"prefix": "/admin"}]}]}}]}`,
		form:     authzForm, set: authzSet},
	"netsecurity-meshauthz": {id: "netsecurity-meshauthz", title: "Authentication Configuration", coll: "authorizationPolicies", idParam: "authorizationPolicyId",
		noun: "Service mesh authorization policy", nounPlural: "service mesh authorization policies",
		note:    "Cloud Service Mesh's (Traffic Director's) authorization policies, which its Envoy and proxyless gRPC clients enforce.",
		columns: []string{"Location", "Action", "Rules"},
		column: func(o map[string]any) map[string]string {
			return map[string]string{"Action": vpcDash(vpcStr(o, "action")), "Rules": fmt.Sprint(len(listAny(o["rules"])))}
		},
		fields:   []string{"action", "rules"},
		template: `{"action": "ALLOW", "rules": [{"sources": [{"ipBlocks": ["10.0.0.0/8"]}], "destinations": [{"hosts": ["*"], "ports": [80], "methods": ["GET"]}]}]}`},
	"netsecurity-clienttls": {id: "netsecurity-clienttls", title: "Authentication Configuration", coll: "clientTlsPolicies", idParam: "clientTlsPolicyId",
		noun: "Client TLS policy", nounPlural: "client TLS policies",
		note:    "A client TLS policy is how a Cloud Service Mesh client authenticates to its backends: the SNI, its certificate and the CAs that validate the server's.",
		columns: []string{"Location", "SNI", "Client certificate", "Server validation CAs"},
		column: func(o map[string]any) map[string]string {
			cert := "None"
			if vmMap(o, "clientCertificate") != nil {
				cert = "Set"
			}
			return map[string]string{"SNI": vpcDash(vpcStr(o, "sni")), "Client certificate": cert, "Server validation CAs": fmt.Sprint(len(listAny(o["serverValidationCa"])))}
		},
		fields:   []string{"sni", "clientCertificate", "serverValidationCa"},
		template: `{"sni": "backend.example.com", "serverValidationCa": [{"certificateProviderInstance": {"pluginInstance": "google_cloud_private_spiffe"}}]}`},
}

func lastAllAny(l []any) []string {
	var out []string
	for _, v := range l {
		out = append(out, vpcLast(strAny(v)))
	}
	return out
}

type netsecProvider struct {
	ccmScreen
	k netsecKind
}

func newNetsecProvider(s computeScreen, id string) netsecProvider {
	return netsecProvider{ccmScreen{s}, netsecKinds[id]}
}

func (p netsecProvider) ID() string    { return p.k.id }
func (p netsecProvider) Title() string { return p.k.title }

func (p netsecProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: p.k.columns, NameColumn: "Name", Noun: p.k.nounPlural, Note: p.k.note}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "These are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, p.k.coll)
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		f := p.k.column(o)
		f["Location"] = ccmLocationText(path[0])
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path, Fields: f,
			Actions: []console.Action{ccmDelete(p.k.noun, path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p netsecProvider) CreateOnPage() bool { return true }
func (p netsecProvider) CreateTitle() string {
	return "Create " + strings.ToLower(p.k.noun[:1]) + p.k.noun[1:]
}
func (p netsecProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p netsecProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z]([\-a-z0-9]{0,61}[a-z0-9])?$`,
			Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
	fields = append(fields, ccmLocationFields("")...)
	if p.k.form != nil {
		fields = append(fields, p.k.form(nil)...)
	}
	return "Create", append(fields, console.Field{Name: "spec", Label: "Policy", Type: "textarea", Required: true, Default: p.k.template,
		Help: "The policy's " + strings.Join(p.k.fields, ", ") + " as JSON, as gcloud's import file holds them."})
}

// netsecSpec is the form's JSON, with only the kind's fields.
func (p netsecProvider) netsecSpec(v map[string]string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(v["spec"])), &m); err != nil {
		return nil, fmt.Errorf("Policy: not a JSON object: %v", err)
	}
	body := map[string]any{}
	for _, f := range p.k.fields {
		if x, ok := m[f]; ok {
			body[f] = x
		}
	}
	for k := range m {
		if !hasString(p.k.fields, k) {
			return nil, fmt.Errorf("Policy: %q is not a field of a %s (%s)", k, strings.ToLower(p.k.noun), strings.Join(p.k.fields, ", "))
		}
	}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if p.k.set != nil {
		if err := p.k.set(v, body); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func (p netsecProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	body, err := p.netsecSpec(v)
	if err != nil {
		return "", err
	}
	if err := p.ccmCreate(ctx, project, ccmLocation(v), p.k.coll, p.k.idParam, name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p netsecProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a " + strings.ToLower(p.k.noun) + " is addressed by its location and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, p.k.coll), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])}}
	f := p.k.column(o)
	for _, c := range p.k.columns {
		if c != "Location" {
			props = append(props, console.Property{Label: c, Value: f[c]})
		}
	}
	props = append(props, console.Property{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))},
		console.Property{Label: "Updated", Value: vpcDash(vpcStr(o, "updateTime"))})
	spec := map[string]any{}
	for _, k := range p.k.fields {
		if x, ok := o[k]; ok {
			spec[k] = x
		}
	}
	js, _ := json.MarshalIndent(spec, "", "  ")
	return console.Detail{Title: path[1], Heading: p.k.noun + " details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props},
			{Heading: "Policy", Properties: []console.Property{{Label: "JSON", Value: string(js)}}}}}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit " + strings.ToLower(p.k.noun), Submit: "Save", Page: true, Fields: p.editFields(o, string(js))}}, nil
}

func (p netsecProvider) editFields(o map[string]any, js string) []console.Field {
	f := []console.Field{{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."}}
	if p.k.form != nil {
		for _, x := range p.k.form(o) {
			if x.Name != "policyProfile" { // immutable
				f = append(f, x)
			}
		}
	}
	return append(f, console.Field{Name: "spec", Label: "Policy", Type: "textarea", Required: true, Default: js})
}

func (p netsecProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete(p.k.noun, path)
	a.Leaves = true
	return []console.Action{a}
}

// Edit implements console.Editor: Save replaces the description and the
// policy's fields.
func (p netsecProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a " + strings.ToLower(p.k.noun))
	}
	body, err := p.netsecSpec(v)
	if err != nil {
		return err
	}
	body["description"] = strings.TrimSpace(v["description"])
	delete(body, "policyProfile")
	mask := append([]string{"description"}, p.k.fields...)
	if p.k.set != nil {
		mask = append(mask, "action", "customProvider")
		if _, ok := body["customProvider"]; !ok {
			body["customProvider"] = nil
		}
	}
	_, err = p.v1(ctx, "PATCH", "/v1/"+ccmName(project, path, p.k.coll)+"?updateMask="+url.QueryEscape(strings.Join(mask, ",")), body)
	return err
}

func (p netsecProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, p.k.coll), nil)
	return err
}
