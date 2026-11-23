package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Identity-Aware Proxy's page (#2160), as Google's console has it
// (iap/docs/enabling-compute-howto, read 2026-11-22): Security ›
// Identity-Aware Proxy, its HTTPS resources (the load balancers' backend
// services) each with its IAP switch — Turn on IAP asks to confirm with
// Turn On — and, for the resource whose checkbox is selected, the side
// panel's principals, where Add principal grants IAP-secured Web App User
// (roles/iap.httpsResourceAccessor) and Save keeps it; and a resource's
// settings (iap.googleapis.com's IapSettings). The table's columns are
// UNVERIFIED (re-read 2026-11-22: the docs name none). The switch patches
// the backend service's iap.enabled through the Compute Engine API; the
// policy and settings are IAP's v1 API, served on the Compute port.

type iapProvider struct{ computeScreen }

func (iapProvider) ID() string    { return "security-iap" }
func (iapProvider) Title() string { return "Identity-Aware Proxy" }

const iapNote = "IAP is enforced by the load balancers' proxy on this machine: a request through a backend service with IAP on needs an identity token this instance issued " +
	"(gcloud auth print-identity-token, the metadata server, or IAM Credentials' generateIdToken) in Authorization or Proxy-Authorization, of a principal with IAP-secured Web App User on the resource; " +
	"the backend then gets Google's x-goog-iap-jwt-assertion and x-goog-authenticated-user-email headers. " +
	"A browser without IAP's session is sent to this instance's sign-in page (Google's is accounts.google.com), never to Google."

// iapResource is a backend service's IAP resource name: projects/P/iap_web/
// compute/services/S, or compute-REGION for a regional one.
func iapResource(project string, path []string) string {
	kind := "compute"
	if path[0] != "global" {
		kind = "compute-" + path[0]
	}
	return "projects/" + project + "/iap_web/" + kind + "/services/" + path[1]
}

// iapCandidates are the backend services an Application Load Balancer can
// put behind IAP: HTTP, HTTPS or HTTP/2, external or internal managed.
func (p iapProvider) iapCandidates(project string) []map[string]any {
	var out []map[string]any
	for _, o := range p.lbAll(project, "backendServices", true, true) {
		switch vpcOr(vpcStr(o, "protocol"), "HTTP") {
		case "HTTP", "HTTPS", "HTTP2", "H2C":
		default:
			continue
		}
		if s := vpcStr(o, "loadBalancingScheme"); s == "INTERNAL" || s == "INTERNAL_SELF_MANAGED" {
			continue
		}
		out = append(out, o)
	}
	return out
}

func iapOn(o map[string]any) bool { return vmMap(o, "iap")["enabled"] == true }

func (p iapProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Type", "Scope", "IAP", "Status"}, NameColumn: "Resource", Noun: "HTTPS resources", Note: iapNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "HTTPS resources are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, o := range p.iapCandidates(project) {
		path := lbScopePath(o)
		on := iapOn(o)
		status := "OK"
		if len(vpcStrs(o, "usedBy")) == 0 {
			status = "No load balancer uses this backend service"
		}
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path, Selectable: true,
			Fields:  map[string]string{"Type": "Backend service", "Scope": lbScopeText(path[0]), "IAP": map[bool]string{true: "On", false: "Off"}[on], "Status": status},
			Actions: p.rowActions(o), Info: []console.InfoTab{{ID: "permissions", Label: "Principals", Permissions: true}}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p iapProvider) rowActions(o map[string]any) []console.Action {
	toggle := console.Action{ID: "turnon", Label: "Turn on IAP", Title: "Turn on IAP", Submit: "Turn On", Button: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Requests to " + vpcStr(o, "name") + " then need an identity of a principal with IAP-secured Web App User; others are refused."}
	if iapOn(o) {
		toggle = console.Action{ID: "turnoff", Label: "Turn off IAP", Title: "Turn off IAP", Submit: "Turn Off", Button: true, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "Anyone who can reach the load balancer can then reach " + vpcStr(o, "name") + "."}
	}
	return []console.Action{toggle, {ID: "settings", Label: "Settings", Title: "IAP settings", Submit: "Save", Fields: iapSettingsFields(nil)}}
}

func iapSettingsFields(s map[string]any) []console.Field {
	acc := vmMap(s, "accessSettings")
	cors := vmMap(acc, "corsSettings")
	dom := vmMap(acc, "allowedDomainsSettings")
	app := vmMap(s, "applicationSettings")
	return []console.Field{
		{Name: "allowHttpOptions", Label: "Allow HTTP OPTIONS requests (CORS preflight)", Type: "checkbox", Default: fmt.Sprint(cors["allowHttpOptions"] == true), Section: "Access settings"},
		{Name: "allowedDomainsEnable", Label: "Restrict to allowed domains", Type: "checkbox", Default: fmt.Sprint(dom["enable"] == true), Section: "Access settings"},
		{Name: "allowedDomains", Label: "Allowed domains", Type: "text", Default: strings.Join(vpcStrs(dom, "domains"), ", "), Section: "Access settings",
			VisibleWhen: map[string]string{"allowedDomainsEnable": "true"}, Help: "Comma separated; a principal's email must be in one."},
		{Name: "cookieDomain", Label: "Cookie domain", Type: "text", Default: vpcStr(app, "cookieDomain"), Section: "Application settings"},
	}
}

func (p iapProvider) backend(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("an HTTPS resource is addressed by its scope and name, not %v", path)
	}
	return p.call("GET", project, lbScopeColl(path[0], "backendServices")+"/"+path[1], nil, nil)
}

func (p iapProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	o, err := p.backend(project, path)
	if err != nil {
		return nil
	}
	acts := p.rowActions(o)
	if s, err := p.v1(ctx, "GET", "/v1/"+iapResource(project, path)+":iapSettings", nil); err == nil {
		acts[1].Fields = iapSettingsFields(s)
	}
	return acts
}

func (p iapProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if _, err := p.backend(project, path); err != nil {
		return err
	}
	switch action {
	case "turnon", "turnoff":
		_, err := p.call("PATCH", project, lbScopeColl(path[0], "backendServices")+"/"+path[1], nil,
			map[string]any{"iap": map[string]any{"enabled": action == "turnon"}})
		return err
	case "settings":
		var domains []any
		for _, d := range vpcList(v["allowedDomains"]) {
			domains = append(domains, d)
		}
		body := map[string]any{"name": iapResource(project, path),
			"accessSettings": map[string]any{"corsSettings": map[string]any{"allowHttpOptions": v["allowHttpOptions"] == "true"},
				"allowedDomainsSettings": map[string]any{"enable": v["allowedDomainsEnable"] == "true", "domains": domains}},
			"applicationSettings": map[string]any{"cookieDomain": strings.TrimSpace(v["cookieDomain"])}}
		mask := "accessSettings.corsSettings.allowHttpOptions,accessSettings.allowedDomainsSettings,applicationSettings.cookieDomain"
		_, err := p.v1(ctx, "PATCH", "/v1/"+iapResource(project, path)+":iapSettings?updateMask="+url.QueryEscape(mask), body)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

// PolicyOn implements console.PolicyEditor: each HTTPS resource's IAP
// policy, which the proxy enforces.
func (p iapProvider) PolicyOn(ctx context.Context, path []string) *console.PolicyTarget {
	if len(path) != 2 {
		return nil
	}
	return &console.PolicyTarget{RoleHelp: "A role, such as roles/iap.httpsResourceAccessor (IAP-secured Web App User).", Conditions: true,
		Enforced: "IAP checks iap.webServiceVersions.accessViaIAP, which IAP-secured Web App User (roles/iap.httpsResourceAccessor) holds, on every request through the load balancer, " +
			"for the principal named by the request's identity token, with each binding's condition."}
}

func (p iapProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	return iapGetPolicy(ctx, p.computeScreen, iapResource(project, path))
}

func (p iapProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return iapSetPolicy(ctx, p.computeScreen, iapResource(project, path), pol)
}

func (p iapProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	return iapTestPermissions(ctx, p.computeScreen, iapResource(project, path), perms)
}

// iapGetPolicy is an IAP resource's policy, through IAP's v1 API.
func iapGetPolicy(ctx context.Context, p computeScreen, res string) (console.Policy, error) {
	m, err := p.v1(ctx, "POST", "/v1/"+res+":getIamPolicy", map[string]any{"options": map[string]any{"requestedPolicyVersion": 3}})
	if err != nil {
		return console.Policy{}, err
	}
	out := console.Policy{Etag: vpcStr(m, "etag"), Bindings: []console.Binding{}}
	for _, b := range vmMaps(m, "bindings") {
		cb := console.Binding{Role: vpcStr(b, "role"), Members: vpcStrs(b, "members")}
		if cond := vmMap(b, "condition"); len(cond) > 0 {
			cb.Conditional = &console.Condition{Title: vpcStr(cond, "title"), Description: vpcStr(cond, "description"), Expression: vpcStr(cond, "expression")}
			cb.Condition = cb.Conditional.Expression
			if cb.Conditional.Title != "" {
				cb.Condition = cb.Conditional.Title + ": " + cb.Condition
			}
		}
		out.Bindings = append(out.Bindings, cb)
	}
	return out, nil
}

// iapSetPolicy writes an IAP resource's policy.
func iapSetPolicy(ctx context.Context, p computeScreen, res string, pol console.Policy) error {
	version := 1
	bindings := []any{}
	for _, b := range pol.Bindings {
		pb := map[string]any{"role": b.Role, "members": append([]string(nil), b.Members...)}
		switch {
		case b.Conditional != nil:
			pb["condition"] = map[string]any{"title": b.Conditional.Title, "description": b.Conditional.Description, "expression": b.Conditional.Expression}
			version = 3
		case b.Condition != "":
			return fmt.Errorf("the binding for %s has a condition, which the console cannot write back", b.Role)
		}
		bindings = append(bindings, pb)
	}
	policy := map[string]any{"version": version, "bindings": bindings}
	if pol.Etag != "" {
		policy["etag"] = pol.Etag
	}
	_, err := p.v1(ctx, "POST", "/v1/"+res+":setIamPolicy", map[string]any{"policy": policy})
	return err
}

func iapTestPermissions(ctx context.Context, p computeScreen, res string, perms []string) ([]string, error) {
	m, err := p.v1(ctx, "POST", "/v1/"+res+":testIamPermissions", map[string]any{"permissions": perms})
	if err != nil {
		return nil, err
	}
	return vpcStrs(m, "permissions"), nil
}

// PolicyResource implements console.PolicyResourcer.
func (p iapProvider) PolicyResource(ctx context.Context, project string, path []string) string {
	if len(path) != 2 {
		return ""
	}
	return "//iap.googleapis.com/" + iapResource(project, path)
}
