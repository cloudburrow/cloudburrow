package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Network Security's address groups and backend authentication configs
// (#2160), so both are reachable from the console (the UI parity rule):
//
//   - Address groups (/net-security/addressgroups), as
//     firewall/docs/use-address-groups-firewall-policies' console steps
//     have them (read 2026-11-22): the groups listed in the Address groups
//     section; click a name for its details, whose In use by field names
//     each policy as POLICY_NAME(rule priority:PRIORITY_NUMBER); Edit,
//     ending in Save, which adds and removes items; select a group and
//     Delete. The Create form's fields follow the API (name, description,
//     location, type, purpose, capacity, items): the docs give no console
//     steps for it, so its order is UNVERIFIED (re-read 2026-11-22: firewall/docs/use-address-groups-firewall-policies). The list's Cloud Armor
//     tier action is projects.setCloudArmorTier, which address groups for
//     Cloud Armor need (armor/docs/address-groups-overview).
//   - Backend Authentication, the Authentication Configuration page's tab
//     (load-balancing/docs/backend-authenticated-tls-setup, read
//     2026-11-22): Create, then a name, Location Global or Regional,
//     optionally the public roots of trust, and the trust config; the
//     client certificate for backend mTLS (backend-mtls-setup).

type netsecAddressGroupsProvider struct{ ccmScreen }

func (netsecAddressGroupsProvider) ID() string    { return "netsecurity-addressgroups" }
func (netsecAddressGroupsProvider) Title() string { return "Address groups" }

var (
	addressGroupTypeLabels    = map[string]string{"IPV4": "IPv4", "IPV6": "IPv6"}
	addressGroupPurposeLabels = map[string]string{"DEFAULT": "Default", "CLOUD_ARMOR": "Cloud Armor"}
)

func addressGroupPurposes(o map[string]any) string {
	var out []string
	for _, p := range vpcStrs(o, "purpose") {
		out = append(out, vpcOr(addressGroupPurposeLabels[p], p))
	}
	if len(out) == 0 {
		return "Default"
	}
	return strings.Join(out, ", ")
}

// inUseBy are a group's references as the console names them.
func (p netsecAddressGroupsProvider) inUseBy(ctx context.Context, name string) string {
	m, err := p.v1(ctx, "GET", "/v1/"+name+":listReferences", nil)
	if err != nil {
		return ""
	}
	var out []string
	for _, r := range vmMaps(m, "addressGroupReferences") {
		pol := vpcLast(vpcStr(r, "securityPolicy"))
		if pol == "" {
			pol = vpcLast(vpcStr(r, "firewallPolicy"))
		}
		out = append(out, fmt.Sprintf("%s(rule priority:%s)", pol, num2s(r, "rulePriority")))
	}
	return strings.Join(out, ", ")
}

func (p netsecAddressGroupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Type", "Purpose", "Capacity", "Items", "In use by"}, NameColumn: "Name",
		Noun: "address groups", TableTitle: "Address groups",
		Note: "An address group is a named list of IP address ranges that Cloud Armor rules match with evaluateAddressGroup; this instance's load balancers apply them to every request."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Address groups are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "addressGroups")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "Type": vpcOr(addressGroupTypeLabels[vpcStr(o, "type")], vpcStr(o, "type")),
				"Purpose": addressGroupPurposes(o), "Capacity": num2s(o, "capacity"), "Items": strconv.Itoa(len(vpcStrs(o, "items"))),
				"In use by": vpcDash(p.inUseBy(ctx, vpcStr(o, "name")))},
			Actions: []console.Action{ccmDelete("address group", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (netsecAddressGroupsProvider) CreateOnPage() bool   { return true }
func (netsecAddressGroupsProvider) CreateTitle() string  { return "Create address group" }
func (netsecAddressGroupsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (netsecAddressGroupsProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[A-Za-z][A-Za-z0-9_\-]{0,62}$`,
			Help: "1 to 63 letters, digits, hyphens and underscores, starting with a letter."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
	fields = append(fields, ccmLocationFields("")...)
	return "Create", append(fields,
		console.Field{Name: "type", Label: "Type", Type: "radio", Default: "IPV4", Options: []string{"IPV4", "IPV6"}, OptionLabels: addressGroupTypeLabels,
			Help: "The type cannot be changed later."},
		console.Field{Name: "purposeDefault", Label: "Default", Type: "checkbox", Default: "false", Section: "Purpose",
			Help: "For Cloud Next Generation Firewall policies. Without Cloud Armor, the capacity is at most 1,000."},
		console.Field{Name: "purposeArmor", Label: "Cloud Armor", Type: "checkbox", Default: "true", Section: "Purpose",
			Help: "For Cloud Armor security policies; the project must be enrolled in Cloud Armor Enterprise."},
		console.Field{Name: "capacity", Label: "Capacity", Type: "text", Required: true, Default: "100", Pattern: `^[0-9]+$`,
			Help: "The most items the group holds, set at creation: at most 150,000 IPv4 or 50,000 IPv6 ranges for Cloud Armor."},
		console.Field{Name: "items", Label: "IP addresses", Type: "textarea",
			Help: "IP addresses or ranges in CIDR notation, one per line or comma separated."},
	)
}

func splitItems(s string) []any {
	var out []any
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\r' || r == '\t' }) {
		out = append(out, f)
	}
	return out
}

func (p netsecAddressGroupsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	if name == "" {
		return "", errors.New("Name: give one")
	}
	n, err := strconv.Atoi(strings.TrimSpace(v["capacity"]))
	if err != nil || n < 1 {
		return "", errors.New("Capacity: a whole number, at least 1")
	}
	var purpose []any
	if v["purposeDefault"] == "true" {
		purpose = append(purpose, "DEFAULT")
	}
	if v["purposeArmor"] == "true" {
		purpose = append(purpose, "CLOUD_ARMOR")
	}
	body := map[string]any{"type": vpcOr(v["type"], "IPV4"), "capacity": n}
	if len(purpose) > 0 {
		body["purpose"] = purpose
	}
	if items := splitItems(v["items"]); len(items) > 0 {
		body["items"] = items
	}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if err := p.ccmCreate(ctx, project, ccmLocation(v), "addressGroups", "addressGroupId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p netsecAddressGroupsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "an address group is addressed by its location and name"}, nil
	}
	name := ccmName(project, path, "addressGroups")
	o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	items := vpcStrs(o, "items")
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])},
		{Label: "Type", Value: vpcOr(addressGroupTypeLabels[vpcStr(o, "type")], vpcStr(o, "type"))},
		{Label: "Purpose", Value: addressGroupPurposes(o)},
		{Label: "Capacity", Value: num2s(o, "capacity")},
		{Label: "In use by", Value: vpcDash(p.inUseBy(ctx, name))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))},
		{Label: "Updated", Value: vpcDash(vpcStr(o, "updateTime"))}}
	var rows []console.Property
	for _, it := range items {
		rows = append(rows, console.Property{Label: "IP address", Value: it})
	}
	groups := []console.PropertyGroup{{Properties: props}}
	if len(rows) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "IP addresses", Properties: rows})
	}
	return console.Detail{Title: path[1], Heading: "Address group details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit address group", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."},
			{Name: "items", Label: "IP addresses", Type: "textarea", Default: strings.Join(items, "\n"),
				Help: "One per line. The type and the capacity cannot be changed."}}}}, nil
}

func (p netsecAddressGroupsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("address group", path)
	a.Leaves = true
	return []console.Action{a}
}

// Edit implements console.Editor: Save adds the new items and removes the
// dropped ones (addItems and removeItems), and sets the description.
func (p netsecAddressGroupsProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to an address group")
	}
	name := ccmName(project, path, "addressGroups")
	o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, it := range vpcStrs(o, "items") {
		have[it] = true
	}
	want := map[string]bool{}
	var add []any
	for _, it := range splitItems(v["items"]) {
		s := it.(string)
		want[s] = true
		if !have[s] {
			add = append(add, s)
		}
	}
	var rm []any
	for it := range have {
		if !want[it] {
			rm = append(rm, it)
		}
	}
	sort.Slice(rm, func(i, j int) bool { return rm[i].(string) < rm[j].(string) })
	if len(rm) > 0 {
		if _, err := p.v1(ctx, "POST", "/v1/"+name+":removeItems", map[string]any{"items": rm}); err != nil {
			return err
		}
	}
	if len(add) > 0 {
		if _, err := p.v1(ctx, "POST", "/v1/"+name+":addItems", map[string]any{"items": add}); err != nil {
			return err
		}
	}
	if d := strings.TrimSpace(v["description"]); d != vpcStr(o, "description") {
		_, err = p.v1(ctx, "PATCH", "/v1/"+name+"?updateMask=description", map[string]any{"description": d})
	}
	return err
}

// ListActions is the page's Cloud Armor tier action
// (projects.setCloudArmorTier).
func (p netsecAddressGroupsProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.api() == nil || project == "" {
		return nil
	}
	tier := "CA_STANDARD"
	if o, err := p.call("GET", project, "", nil, nil); err == nil {
		tier = vpcOr(vpcStr(o, "cloudArmorTier"), tier)
	}
	return []console.Action{{ID: "armor-tier", Label: "Cloud Armor tier", Title: "Cloud Armor tier", Fields: []console.Field{{
		Name: "tier", Label: "Tier", Type: "radio", Default: tier, Options: []string{"CA_STANDARD", "CA_ENTERPRISE_PAYGO", "CA_ENTERPRISE_ANNUAL"},
		OptionLabels: map[string]string{"CA_STANDARD": "Standard", "CA_ENTERPRISE_PAYGO": "Enterprise Paygo", "CA_ENTERPRISE_ANNUAL": "Enterprise Annual"},
		Help:         "Address groups for Cloud Armor need Cloud Armor Enterprise. Nothing is billed here."}}}}
}

func (p netsecAddressGroupsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	switch {
	case len(path) == 0 && action == "armor-tier":
		_, err := p.call("POST", project, "setCloudArmorTier", nil, map[string]any{"cloudArmorTier": v["tier"]})
		return err
	case len(path) == 2 && action == "delete":
		_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "addressGroups"), nil)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

// --- Backend Authentication (backend authentication configs) ---------------

type netsecBackendAuthProvider struct{ ccmScreen }

func (netsecBackendAuthProvider) ID() string    { return "netsecurity-backendauth" }
func (netsecBackendAuthProvider) Title() string { return "Authentication Configuration" }

// usersOf are the backend services whose tlsSettings name a config.
func (p netsecBackendAuthProvider) usersOf(project, name string) []string {
	out, err := p.call("GET", project, "aggregated/backendServices", nil, nil)
	if err != nil {
		return nil
	}
	var users []string
	items, _ := out["items"].(map[string]any)
	for _, scoped := range items {
		sm, _ := scoped.(map[string]any)
		for _, it := range listAny(sm["backendServices"]) {
			m, _ := it.(map[string]any)
			if strAny(vmMap(m, "tlsSettings")["authenticationConfig"]) == "//networksecurity.googleapis.com/"+name {
				users = append(users, strAny(m["name"]))
			}
		}
	}
	sort.Strings(users)
	return users
}

func publicRootsText(o map[string]any) string {
	if vpcStr(o, "wellKnownRoots") == "PUBLIC_ROOTS" {
		return "Enabled"
	}
	return "Disabled"
}

func (p netsecBackendAuthProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Trust config", "Public roots of trust", "Client certificate", "In use by"}, NameColumn: "Name",
		Noun: "Backend Authentication resources",
		Note: "A backend authentication config is how a load balancer authenticates its HTTPS and HTTP/2 backends (a trust config, or the public roots of trust) and, with a client certificate, itself to them (backend mTLS)."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Backend Authentication resources are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "backendAuthenticationConfigs")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "Trust config": vpcDash(vpcLast(vpcStr(o, "trustConfig"))),
				"Public roots of trust": publicRootsText(o), "Client certificate": vpcDash(vpcLast(vpcStr(o, "clientCertificate"))),
				"In use by": vpcDash(strings.Join(p.usersOf(project, vpcStr(o, "name")), ", "))},
			Actions: []console.Action{ccmDelete("Backend Authentication resource", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (netsecBackendAuthProvider) CreateOnPage() bool   { return true }
func (netsecBackendAuthProvider) CreateTitle() string  { return "Create Backend Authentication" }
func (netsecBackendAuthProvider) CreateLayout() string { return console.CreateLayoutPage }

func (netsecBackendAuthProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[A-Za-z_\-][A-Za-z0-9_\-]{0,62}$`,
			Help: "1 to 63 letters, digits, hyphens and underscores, not starting with a digit."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
	fields = append(fields, ccmLocationFields("")...)
	return "Create", append(fields,
		console.Field{Name: "publicRoots", Label: "Use public roots of trust", Type: "checkbox", Default: "false",
			Help: "Also trust backend certificates of public certificate authorities, when the backend service sets an SNI hostname."},
		console.Field{Name: "trustConfig", Label: "Trust config", Type: "text",
			Help: "The trust config's name, in the same location (Certificate Manager › Trust configs). Required without the public roots of trust."},
		console.Field{Name: "clientCertificate", Label: "Client certificate", Type: "text",
			Help: "Optional, for backend mTLS: a Certificate Manager certificate of scope client-auth in the same location. It cannot be added later."},
	)
}

func backendAuthBody(project, loc string, v map[string]string, create bool) map[string]any {
	body := map[string]any{"wellKnownRoots": "NONE"}
	if v["publicRoots"] == "true" {
		body["wellKnownRoots"] = "PUBLIC_ROOTS"
	}
	full := func(coll, s string) string {
		s = strings.TrimSpace(s)
		if s != "" && !strings.Contains(s, "/") {
			s = "projects/" + project + "/locations/" + loc + "/" + coll + "/" + s
		}
		return s
	}
	body["trustConfig"] = full("trustConfigs", v["trustConfig"])
	if create {
		if c := full("certificates", v["clientCertificate"]); c != "" {
			body["clientCertificate"] = c
		}
	}
	body["description"] = strings.TrimSpace(v["description"])
	return body
}

func (p netsecBackendAuthProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	if name == "" {
		return "", errors.New("Name: give one")
	}
	loc := ccmLocation(v)
	if err := p.ccmCreate(ctx, project, loc, "backendAuthenticationConfigs", "backendAuthenticationConfigId", name, backendAuthBody(project, loc, v, true)); err != nil {
		return "", err
	}
	return name, nil
}

func (p netsecBackendAuthProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a Backend Authentication resource is addressed by its location and name"}, nil
	}
	name := ccmName(project, path, "backendAuthenticationConfigs")
	o, err := p.v1(ctx, "GET", "/v1/"+name, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])},
		{Label: "Trust config", Value: vpcDash(vpcStr(o, "trustConfig"))},
		{Label: "Public roots of trust", Value: publicRootsText(o)},
		{Label: "Client certificate", Value: vpcDash(vpcStr(o, "clientCertificate"))},
		{Label: "In use by", Value: vpcDash(strings.Join(p.usersOf(project, name), ", "))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "Backend Authentication details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit Backend Authentication", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."},
			{Name: "publicRoots", Label: "Use public roots of trust", Type: "checkbox", Default: strconv.FormatBool(vpcStr(o, "wellKnownRoots") == "PUBLIC_ROOTS")},
			{Name: "trustConfig", Label: "Trust config", Type: "text", Default: vpcLast(vpcStr(o, "trustConfig"))}}}}, nil
}

func (p netsecBackendAuthProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("Backend Authentication resource", path)
	a.Leaves = true
	return []console.Action{a}
}

// Edit implements console.Editor; the client certificate cannot change.
func (p netsecBackendAuthProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a Backend Authentication resource")
	}
	_, err := p.v1(ctx, "PATCH", "/v1/"+ccmName(project, path, "backendAuthenticationConfigs")+"?updateMask="+url.QueryEscape("description,trustConfig,wellKnownRoots"),
		backendAuthBody(project, path[0], v, false))
	return err
}

func (p netsecBackendAuthProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "backendAuthenticationConfigs"), nil)
	return err
}
