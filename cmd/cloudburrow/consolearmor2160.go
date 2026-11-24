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

// Cloud Armor's pages (#2160), over Compute Engine's securityPolicies and
// regionSecurityPolicies, as Google's console has them
// (armor/docs/create-security-policies, .../manage-security-policies,
// .../create-security-policy-rules and .../manage-security-policy-rules,
// read 2026-11-22): Network Security › Cloud Armor policies, the list of
// policies (a row's menu has Edit, Add rule and Apply policy to target;
// Delete); Create policy, whose steps are Configure policy (name,
// description, policy type, scope, default rule action and deny status),
// Add more rules and Apply policy to targets; a policy's Policy details
// page with its Rules tab (Add rule: description, Basic mode's IP ranges
// or Advanced mode's expression, action, response code, preview,
// priority; a rule's Edit and Delete) and Targets tab (Apply policy to new
// target; Remove). The create page's step and field labels other than
// those the docs quote are UNVERIFIED (re-read 2026-11-22: the docs give
// no Console steps for Create policy).

type computeArmorProvider struct{ computeScreen }

func (computeArmorProvider) ID() string    { return "netsecurity-armor" }
func (computeArmorProvider) Title() string { return "Cloud Armor policies" }

const armorNote = "Cloud Armor policies are enforced by the load balancers' proxy on this machine: each request through a backend service or bucket is matched against its policy's rules in priority order " +
	"(IP ranges, CEL expressions, the preconfigured WAF rules, rate limits), and is allowed, denied, throttled, banned or redirected as the first match says; rules in preview are logged only."

// armorDefaultPriority is the default rule's priority, the lowest.
const armorDefaultPriority = 2147483647

func armorTypeText(t string) string {
	switch t {
	case "CLOUD_ARMOR_EDGE":
		return "Edge security policy"
	case "CLOUD_ARMOR_NETWORK":
		return "Network edge security policy"
	}
	return "Backend security policy"
}

// armorPolicies are the project's policies, global and regional.
func (p computeArmorProvider) armorPolicies(project string) []map[string]any {
	return p.lbAll(project, "securityPolicies", true, true)
}

// armorRelOf is a policy's path after projects/P/.
func armorRelOf(path []string) string {
	return lbScopeColl(path[0], "securityPolicies") + "/" + path[1]
}

// armorTargets are the backend services and buckets a policy is attached
// to, as [kind, scope, name] with the field that names it.
func (p computeArmorProvider) armorTargets(project string, policy map[string]any) [][]string {
	self := lbRel(vpcStr(policy, "selfLink"))
	var out [][]string
	for _, o := range append(p.lbAll(project, "backendServices", true, true), p.lbAll(project, "backendBuckets", true, false)...) {
		coll := "backendServices"
		if vpcStr(o, "kind") == "compute#backendBucket" {
			coll = "backendBuckets"
		}
		for _, f := range []string{"securityPolicy", "edgeSecurityPolicy"} {
			if lbRel(vpcStr(o, f)) == self {
				sp := lbScopePath(o)
				out = append(out, []string{coll, sp[0], sp[1], f})
			}
		}
	}
	if vpcStr(policy, "type") == "CLOUD_ARMOR_NETWORK" {
		for _, t := range p.armorPassthroughTargets(project) {
			if lbRel(vpcStr(t.rec, "securityPolicy")) == self {
				out = append(out, []string{t.coll, t.scope, t.name, "securityPolicy"})
			}
		}
	}
	return out
}

// armorPassthroughTargets are the project's target pools and target
// instances, which a network edge security policy (CLOUD_ARMOR_NETWORK)
// is attached to directly.
func (p computeArmorProvider) armorPassthroughTargets(project string) []armorPassTarget {
	var out []armorPassTarget
	for _, o := range p.lbAll(project, "targetPools", false, true) {
		sp := lbScopePath(o)
		out = append(out, armorPassTarget{"targetPools", sp[0], sp[1], o})
	}
	if m, err := p.call("GET", project, "aggregated/targetInstances", nil, nil); err == nil {
		for _, o := range aggregatedItems(m, "targetInstances") {
			out = append(out, armorPassTarget{"targetInstances", vpcLast(vpcStr(o, "zone")), vpcStr(o, "name"), o})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// armorPassTarget is a target pool (scope its region) or target instance
// (scope its zone).
type armorPassTarget struct {
	coll, scope, name string
	rec               map[string]any
}

// armorTargetColl is a target's collection path: global/C, regions/R/C or,
// for target instances, zones/Z/C.
func armorTargetColl(coll, scope string) string {
	if coll == "targetInstances" {
		return "zones/" + scope + "/" + coll
	}
	return lbScopeColl(scope, coll)
}

func (p computeArmorProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Type", "Scope", "Rules", "Targets"}, NameColumn: "Name", Noun: "security policies", Note: armorNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Security policies are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, o := range p.armorPolicies(project) {
		path := lbScopePath(o)
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields: map[string]string{"Type": armorTypeText(vpcStr(o, "type")), "Scope": lbScopeText(path[0]),
				"Rules": strconv.Itoa(len(vmMaps(o, "rules"))), "Targets": strconv.Itoa(len(p.armorTargets(project, o)))},
			Actions: append(p.policyActions(project, path, o), armorDeletePolicy(path))})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func armorDeletePolicy(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete security policy " + path[1] + "? It must not be attached to a target."}
}

func (computeArmorProvider) CreateOnPage() bool  { return true }
func (computeArmorProvider) CreateTitle() string { return "Create policy" }

func (p computeArmorProvider) CreateForm() (string, []console.Field) {
	return "Create policy", armorCreateFields(nil)
}

func (p computeArmorProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	var targets []string
	for _, o := range append(p.lbAll(project, "backendServices", true, true), p.lbAll(project, "backendBuckets", true, false)...) {
		sp := lbScopePath(o)
		coll := "backendServices"
		if vpcStr(o, "kind") == "compute#backendBucket" {
			coll = "backendBuckets"
		}
		targets = append(targets, lbScopeColl(sp[0], coll)+"/"+sp[1])
	}
	sort.Strings(targets)
	return armorCreateFields(targets)
}

// armorRuleFields are a rule's inputs, in Add rule's order, prefilled from
// r (nil for a new rule).
func armorRuleFields(section string, r map[string]any, withPriority bool) []console.Field {
	match := vmMap(r, "match")
	mode, ranges, expr := "basic", "", ""
	if e := vmMap(match, "expr"); len(e) > 0 {
		mode, expr = "advanced", vpcStr(e, "expression")
	} else {
		ranges = strings.Join(vpcStrs(vmMap(match, "config"), "srcIpRanges"), ", ")
	}
	action, code := armorSplitAction(vpcOr(vpcStr(r, "action"), "deny(403)"))
	rl := vmMap(r, "rateLimitOptions")
	thr := vmMap(rl, "rateLimitThreshold")
	redir := vmMap(r, "redirectOptions")
	if r == nil {
		action = "allow"
	}
	adv := map[string]string{"mode": "advanced"}
	basic := map[string]string{"mode": "basic"}
	rate := "action=throttle|rate_based_ban"
	fields := []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: vpcStr(r, "description"), Section: section, Help: "Optional. Enter a description of the rule."},
		{Name: "mode", Label: "Mode", Type: "radio", Default: mode, Section: section, Options: []string{"basic", "advanced"},
			OptionLabels: map[string]string{"basic": "Basic mode (IP addresses/ranges only)", "advanced": "Advanced mode"}},
		{Name: "srcIpRanges", Label: "Match", Type: "text", Default: ranges, Section: section, VisibleWhen: basic,
			Help: "IP addresses or ranges, comma separated (at most 10), such as 192.0.2.0/24; * matches every address."},
		{Name: "expression", Label: "Match", Type: "textarea", Default: expr, Section: section, VisibleWhen: adv,
			Help: "A Common Expression Language expression, such as request.path.matches('/admin') or evaluatePreconfiguredWaf('sqli-v33-stable')."},
		{Name: "action", Label: "Action", Type: "select", Default: action, Section: section,
			Options:      []string{"allow", "deny", "throttle", "rate_based_ban", "redirect"},
			OptionLabels: map[string]string{"allow": "Allow", "deny": "Deny", "throttle": "Throttle", "rate_based_ban": "Rate-based ban", "redirect": "Redirect"}},
		{Name: "responseCode", Label: "Response code", Type: "select", Default: vpcOr(code, "403"), Section: section, VisibleWhen: map[string]string{"action": "deny"},
			Options: []string{"403", "404", "502"}, OptionLabels: map[string]string{"403": "403 (Forbidden)", "404": "404 (Not Found)", "502": "502 (Bad Gateway)"}},
		{Name: "rateCount", Label: "Threshold: request count", Type: "text", Default: vpcOr(num2s(thr, "count"), "100"), Section: section, ShowWhen: rate, Pattern: `^[0-9]*$`},
		{Name: "rateInterval", Label: "Threshold: interval (seconds)", Type: "select", Default: vpcOr(num2s(thr, "intervalSec"), "60"), Section: section, ShowWhen: rate,
			Options: []string{"10", "30", "60", "120", "180", "240", "300", "600", "900", "1200", "1800", "2700", "3600"}},
		{Name: "enforceOnKey", Label: "Enforce on key", Type: "select", Default: vpcOr(vpcStr(rl, "enforceOnKey"), "IP"), Section: section, ShowWhen: rate,
			Options: []string{"ALL", "IP", "HTTP_HEADER", "XFF_IP", "HTTP_COOKIE", "HTTP_PATH", "SNI", "REGION_CODE"}},
		{Name: "enforceOnKeyName", Label: "Key name", Type: "text", Default: vpcStr(rl, "enforceOnKeyName"), Section: section, ShowWhen: "enforceOnKey=HTTP_HEADER|HTTP_COOKIE",
			Help: "The header or cookie whose value keys the rate limit."},
		{Name: "exceedAction", Label: "Exceed action", Type: "select", Default: vpcOr(vpcStr(rl, "exceedAction"), "deny(429)"), Section: section, ShowWhen: rate,
			Options: []string{"deny(403)", "deny(404)", "deny(429)", "deny(502)"}},
		{Name: "banDurationSec", Label: "Ban duration (seconds)", Type: "text", Default: vpcOr(num2s(rl, "banDurationSec"), "600"), Section: section,
			VisibleWhen: map[string]string{"action": "rate_based_ban"}, Pattern: `^[0-9]*$`},
		{Name: "redirectType", Label: "Redirect type", Type: "select", Default: vpcOr(vpcStr(redir, "type"), "EXTERNAL_302"), Section: section,
			VisibleWhen: map[string]string{"action": "redirect"}, Options: []string{"EXTERNAL_302", "GOOGLE_RECAPTCHA"}},
		{Name: "redirectTarget", Label: "Target", Type: "text", Default: vpcStr(redir, "target"), Section: section, VisibleWhen: map[string]string{"action": "redirect"},
			Help: "The URL an EXTERNAL_302 redirect sends the client to."},
		{Name: "preview", Label: "Preview", Type: "checkbox", Default: strconv.FormatBool(r["preview"] == true), Section: section,
			Help: "Enable: the rule's action is logged, not taken."},
	}
	if withPriority {
		p := ""
		if v, ok := r["priority"].(float64); ok {
			p = strconv.FormatFloat(v, 'f', -1, 64)
		}
		fields = append(fields, console.Field{Name: "priority", Label: "Priority", Type: "text", Default: p, Section: section, Required: r == nil,
			Pattern: `^[0-9]{1,10}$`, Help: "An integer from 0 to 2,147,483,646; the lowest number is evaluated first."})
	}
	return fields
}

// armorSplitAction splits "deny(403)" into deny and 403.
func armorSplitAction(a string) (string, string) {
	if i := strings.Index(a, "("); i > 0 && strings.HasSuffix(a, ")") {
		return a[:i], a[i+1 : len(a)-1]
	}
	return a, ""
}

func armorCreateFields(targets []string) []console.Field {
	const s1, s2, s3 = "Configure policy", "Add more rules (optional)", "Apply policy to targets (optional)"
	regional := map[string]string{"scope": "regional"}
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp, Section: s1},
		{Name: "description", Label: "Description", Type: "text", Section: s1},
		{Name: "type", Label: "Policy type", Type: "radio", Default: "CLOUD_ARMOR", Section: s1, Options: []string{"CLOUD_ARMOR", "CLOUD_ARMOR_EDGE", "CLOUD_ARMOR_NETWORK"},
			OptionLabels: map[string]string{"CLOUD_ARMOR": "Backend security policy", "CLOUD_ARMOR_EDGE": "Edge security policy", "CLOUD_ARMOR_NETWORK": "Network edge security policy"},
			Help: "An edge security policy filters requests before Cloud CDN's cache, by IP ranges and region codes only; it is global. " +
				"A network edge security policy is regional: it filters the packets of the region's external passthrough Network Load Balancers, through the region's network edge security service or on a target pool or instance."},
		{Name: "scope", Label: "Scope", Type: "radio", Default: "global", Section: s1, Options: []string{"global", "regional"},
			OptionLabels: map[string]string{"global": "Global", "regional": "Regional"}, ShowWhen: "type=CLOUD_ARMOR|CLOUD_ARMOR_NETWORK"},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, Section: s1, VisibleWhen: regional},
		{Name: "defaultAction", Label: "Default rule action", Type: "radio", Default: "allow", Section: s1, Options: []string{"allow", "deny"},
			OptionLabels: map[string]string{"allow": "Allow", "deny": "Deny"}},
		{Name: "denyStatus", Label: "Deny status", Type: "select", Default: "403", Section: s1, VisibleWhen: map[string]string{"defaultAction": "deny"},
			Options: []string{"403", "404", "502"}, OptionLabels: map[string]string{"403": "403 (Forbidden)", "404": "404 (Not Found)", "502": "502 (Bad Gateway)"}},
	}
	rule := armorRuleFields(s2, nil, true)
	for i := range rule {
		rule[i].Name = "rule_" + rule[i].Name
		rule[i].Required = false
		if rule[i].VisibleWhen != nil {
			vw := map[string]string{}
			for k, v := range rule[i].VisibleWhen {
				vw["rule_"+k] = v
			}
			rule[i].VisibleWhen = vw
		}
		if rule[i].ShowWhen != "" {
			rule[i].ShowWhen = strings.NewReplacer("action=", "rule_action=", "enforceOnKey=", "rule_enforceOnKey=").Replace(rule[i].ShowWhen)
		}
	}
	rule[len(rule)-1].Help = "Optional: give a priority to add this rule with the policy."
	fields = append(fields, rule...)
	tf := console.Field{Name: "target", Label: "Target", Type: "text", Section: s3,
		Help: "A backend service (global/backendServices/NAME) or, for an edge policy, a backend bucket; empty applies the policy to nothing yet."}
	if targets != nil {
		tf.Type, tf.Options = "select", append([]string{""}, targets...)
	}
	return append(fields, tf)
}

// armorRule builds a rule from Add rule's values (keys with prefix).
func armorRule(v map[string]string, prefix string) (map[string]any, error) {
	g := func(k string) string { return strings.TrimSpace(v[prefix+k]) }
	r := map[string]any{"description": g("description"), "preview": g("preview") == "true"}
	if g("mode") == "advanced" {
		if g("expression") == "" {
			return nil, errors.New("Match: give an expression")
		}
		r["match"] = map[string]any{"expr": map[string]any{"expression": g("expression")}}
	} else {
		ranges := vpcList(g("srcIpRanges"))
		if len(ranges) == 0 {
			return nil, errors.New("Match: give at least one IP address or range")
		}
		r["match"] = map[string]any{"versionedExpr": "SRC_IPS_V1", "config": map[string]any{"srcIpRanges": ranges}}
	}
	switch a := vpcOr(g("action"), "allow"); a {
	case "deny":
		r["action"] = "deny(" + vpcOr(g("responseCode"), "403") + ")"
	case "throttle", "rate_based_ban":
		r["action"] = a
		count, err := strconv.Atoi(vpcOr(g("rateCount"), "100"))
		if err != nil || count < 1 {
			return nil, errors.New("Threshold: a request count of at least 1")
		}
		interval, _ := strconv.Atoi(vpcOr(g("rateInterval"), "60"))
		rl := map[string]any{"conformAction": "allow", "exceedAction": vpcOr(g("exceedAction"), "deny(429)"),
			"rateLimitThreshold": map[string]any{"count": float64(count), "intervalSec": float64(interval)},
			"enforceOnKey":       vpcOr(g("enforceOnKey"), "IP")}
		if n := g("enforceOnKeyName"); n != "" {
			rl["enforceOnKeyName"] = n
		}
		if a == "rate_based_ban" {
			ban, _ := strconv.Atoi(vpcOr(g("banDurationSec"), "600"))
			rl["banDurationSec"] = float64(ban)
		}
		r["rateLimitOptions"] = rl
	case "redirect":
		r["action"] = "redirect"
		ro := map[string]any{"type": vpcOr(g("redirectType"), "EXTERNAL_302")}
		if t := g("redirectTarget"); t != "" {
			ro["target"] = t
		}
		r["redirectOptions"] = ro
	default:
		r["action"] = a
	}
	if ps := g("priority"); ps != "" {
		n, err := strconv.ParseInt(ps, 10, 64)
		if err != nil || n < 0 || n >= armorDefaultPriority {
			return nil, errors.New("Priority: an integer from 0 to 2,147,483,646")
		}
		r["priority"] = float64(n)
	}
	return r, nil
}

func (p computeArmorProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	if name == "" {
		return "", errors.New("Name: give the policy a name")
	}
	typ := vpcOr(v["type"], "CLOUD_ARMOR")
	scope := "global"
	if typ != "CLOUD_ARMOR_EDGE" && v["scope"] == "regional" {
		scope = vpcOr(v["region"], "us-central1")
	}
	if typ == "CLOUD_ARMOR_NETWORK" && scope == "global" {
		return "", errors.New("Scope: a network edge security policy is regional; choose Regional and its region")
	}
	def := "allow"
	if v["defaultAction"] == "deny" {
		def = "deny(" + vpcOr(v["denyStatus"], "403") + ")"
	}
	network := typ == "CLOUD_ARMOR_NETWORK"
	defRule := map[string]any{"priority": float64(armorDefaultPriority), "action": def, "description": "Default rule, higher priority overrides it",
		"match": map[string]any{"versionedExpr": "SRC_IPS_V1", "config": map[string]any{"srcIpRanges": []any{"*"}}}}
	if network {
		delete(defRule, "match")
		defRule["networkMatch"] = map[string]any{"srcIpRanges": []any{"*"}}
	}
	rules := []any{defRule}
	if strings.TrimSpace(v["rule_priority"]) != "" {
		r, err := armorRuleFor(v, "rule_", network)
		if err != nil {
			return "", err
		}
		rules = append([]any{r}, rules...)
	}
	body := map[string]any{"name": name, "type": typ, "rules": rules}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if _, err := p.call("POST", project, lbScopeColl(scope, "securityPolicies"), nil, body); err != nil {
		return "", err
	}
	if t := strings.TrimSpace(v["target"]); t != "" {
		if err := p.attach(project, []string{scope, name}, typ, t); err != nil {
			return name, err
		}
	}
	return name, nil
}

// attach sets policy (at path) on a target (global/backendServices/NAME,
// regions/R/backendServices/NAME or global/backendBuckets/NAME).
func (p computeArmorProvider) attach(project string, path []string, typ, target string) error {
	link := "projects/" + project + "/" + armorRelOf(path)
	verb := "setSecurityPolicy"
	if typ == "CLOUD_ARMOR_EDGE" || strings.Contains(target, "/backendBuckets/") {
		verb = "setEdgeSecurityPolicy"
	}
	if path[1] == "" {
		link = ""
	}
	_, err := p.call("POST", project, target+"/"+verb, nil, map[string]any{"securityPolicy": link})
	return err
}

func (p computeArmorProvider) policy(project string, path []string) (map[string]any, error) {
	if len(path) < 2 {
		return nil, fmt.Errorf("a security policy is addressed by its scope and name, not %v", path)
	}
	return p.call("GET", project, armorRelOf(path), nil, nil)
}

func armorMatchText(r map[string]any) (string, string) {
	m := vmMap(r, "match")
	if e := vmMap(m, "expr"); len(e) > 0 {
		return "Advanced", vpcStr(e, "expression")
	}
	return "Basic", strings.Join(vpcStrs(vmMap(m, "config"), "srcIpRanges"), ", ")
}

func armorActionText(a string) string {
	act, code := armorSplitAction(a)
	switch act {
	case "deny":
		return "Deny (" + code + ")"
	case "rate_based_ban":
		return "Rate-based ban"
	}
	return vpcTitle(act)
}

func (p computeArmorProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) == 4 && path[2] == "rule" {
		return p.ruleDetail(project, path)
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a security policy is addressed by its scope and name"}, nil
	}
	o, err := p.policy(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	def := ""
	rules := console.Listing{Columns: []string{"Action", "Type", "Match", "Description", "Priority", "Preview"}, NameColumn: "Rule", Noun: "rules", Items: []console.Resource{}}
	rs := vmMaps(o, "rules")
	sort.SliceStable(rs, func(i, j int) bool {
		a, _ := rs[i]["priority"].(float64)
		b, _ := rs[j]["priority"].(float64)
		return a < b
	})
	for _, r := range rs {
		pri := num2s(r, "priority")
		typ, match := armorMatchText(r)
		if pri == strconv.Itoa(armorDefaultPriority) {
			def = armorActionText(vpcStr(r, "action"))
			typ = "Default"
		}
		row := console.Resource{Name: "Priority " + pri, Target: []string{path[0], path[1], "rule", pri}, Opens: []string{path[0], path[1], "rule", pri},
			Fields: map[string]string{"Action": armorActionText(vpcStr(r, "action")), "Type": typ, "Match": vpcDash(match),
				"Description": vpcDash(vpcStr(r, "description")), "Priority": pri, "Preview": map[bool]string{true: "Enabled", false: "—"}[r["preview"] == true]}}
		row.Actions = p.ruleActions(path, r, armorIsNetwork(o))
		rules.Items = append(rules.Items, row)
	}
	rules.Total = len(rules.Items)
	targets := console.Listing{Columns: []string{"Type", "Scope", "Policy field", "Effective policies"}, NameColumn: "Target", Noun: "targets", Items: []console.Resource{},
		Note: "Apply policy to new target attaches the policy to a backend service, an edge policy to a backend service or bucket, and a network edge security policy to a target pool or target instance."}
	for _, t := range p.armorTargets(project, o) {
		kind := map[string]string{"backendServices": "Backend service", "backendBuckets": "Backend bucket", "targetPools": "Target pool", "targetInstances": "Target instance"}[t[0]]
		targets.Items = append(targets.Items, console.Resource{Name: t[2], Target: []string{path[0], path[1], "target", t[0], t[1], t[2]},
			Fields: map[string]string{"Type": kind, "Scope": lbScopeText(t[1]), "Policy field": t[3], "Effective policies": p.effectivePolicies(project, t)},
			Actions: []console.Action{{ID: "removetarget", Label: "Remove", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Remove target",
				Confirm: "Remove the policy from " + t[2] + "? Its requests are then no longer filtered by it."}}})
	}
	targets.Total = len(targets.Items)
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Policy type", Value: armorTypeText(vpcStr(o, "type"))}, {Label: "Scope", Value: lbScopeText(path[0])},
		{Label: "Default rule action", Value: vpcDash(def)}, {Label: "Labels", Value: vpcDash(labelsText(armorLabels(o)))}}
	return console.Detail{Title: path[1], Heading: "Policy details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "rules", Label: "Rules", Listing: rules, Actions: []console.Action{p.addRuleAction(armorIsNetwork(o))}, Note: armorNote},
			{ID: "targets", Label: "Targets", Listing: targets, Actions: []console.Action{p.applyTargetAction(project, o)}}},
		OpenTab: "rules"}, nil
}

func (p computeArmorProvider) addRuleAction(network bool) console.Action {
	return console.Action{ID: "addrule", Label: "Add rule", Title: "Add rule", Submit: "Add", Fields: armorRuleFieldsFor(nil, true, network)}
}

func armorIsNetwork(o map[string]any) bool { return vpcStr(o, "type") == "CLOUD_ARMOR_NETWORK" }

// armorRuleFieldsFor are a rule's inputs; a network edge security
// policy's rules match packets (networkMatch): source and destination IP
// ranges, IP protocols and ports, and allow or deny.
func armorRuleFieldsFor(r map[string]any, withPriority, network bool) []console.Field {
	fields := armorRuleFields("", r, withPriority)
	if !network {
		return fields
	}
	nm := vmMap(r, "networkMatch")
	var out []console.Field
	for _, f := range fields {
		switch f.Name {
		case "mode", "expression", "rateCount", "rateInterval", "enforceOnKey", "enforceOnKeyName", "exceedAction", "banDurationSec", "redirectType", "redirectTarget", "responseCode":
			continue
		case "srcIpRanges":
			f.Label, f.VisibleWhen, f.Default = "Source IP ranges", nil, strings.Join(vpcStrs(nm, "srcIpRanges"), ", ")
			out = append(out, f,
				console.Field{Name: "destIpRanges", Label: "Destination IP ranges", Type: "text", Default: strings.Join(vpcStrs(nm, "destIpRanges"), ", "),
					Help: "Optional: the load balancers' or VMs' addresses, comma separated."},
				console.Field{Name: "ipProtocols", Label: "IP protocols", Type: "text", Default: strings.Join(vpcStrs(nm, "ipProtocols"), ", "),
					Help: "Optional: such as tcp, udp or icmp, comma separated."},
				console.Field{Name: "destPorts", Label: "Destination ports", Type: "text", Default: strings.Join(vpcStrs(nm, "destPorts"), ", "),
					Help: "Optional: ports or ranges, such as 80 or 8000-8080, comma separated."})
			continue
		case "action":
			act, _ := armorSplitAction(vpcOr(vpcStr(r, "action"), "allow"))
			f.Options, f.Default = []string{"allow", "deny"}, act
		}
		out = append(out, f)
	}
	return out
}

// armorRuleFor is armorRule, or for a network edge security policy a
// rule of networkMatch.
func armorRuleFor(v map[string]string, prefix string, network bool) (map[string]any, error) {
	if !network {
		return armorRule(v, prefix)
	}
	g := func(k string) string { return strings.TrimSpace(v[prefix+k]) }
	nm := map[string]any{}
	for _, k := range []string{"srcIpRanges", "destIpRanges", "ipProtocols", "destPorts"} {
		if l := vpcList(g(k)); len(l) > 0 {
			nm[k] = l
		}
	}
	if len(nm) == 0 {
		return nil, errors.New("Match: give at least a source IP range, destination, protocol or port")
	}
	a := "allow"
	if g("action") == "deny" {
		a = "deny"
	}
	r := map[string]any{"description": g("description"), "preview": g("preview") == "true", "action": a, "networkMatch": nm}
	if ps := g("priority"); ps != "" {
		n, err := strconv.ParseInt(ps, 10, 64)
		if err != nil || n < 0 || n >= armorDefaultPriority {
			return nil, errors.New("Priority: an integer from 0 to 2,147,483,646")
		}
		r["priority"] = float64(n)
	}
	return r, nil
}

func (p computeArmorProvider) applyTargetAction(project string, o map[string]any) console.Action {
	var opts []string
	edge := vpcStr(o, "type") == "CLOUD_ARMOR_EDGE"
	region := vpcLast(vpcStr(o, "region"))
	if vpcStr(o, "type") == "CLOUD_ARMOR_NETWORK" {
		for _, t := range p.armorPassthroughTargets(project) {
			if t.scope == region || strings.HasPrefix(t.scope, region+"-") {
				opts = append(opts, armorTargetColl(t.coll, t.scope)+"/"+t.name)
			}
		}
		f := console.Field{Name: "target", Label: "Target", Type: "select", Required: true, Options: opts,
			Help: "The target pool or target instance of the policy's region whose packets the policy filters."}
		if len(opts) == 0 {
			f.Type, f.Options = "text", nil
		}
		return console.Action{ID: "applytarget", Label: "Apply policy to new target", Title: "Add target", Submit: "Add", Fields: []console.Field{f}}
	}
	for _, b := range append(p.lbAll(project, "backendServices", true, true), p.lbAll(project, "backendBuckets", true, false)...) {
		sp := lbScopePath(b)
		bucket := vpcStr(b, "kind") == "compute#backendBucket"
		if (bucket && !edge) || (region == "" && sp[0] != "global") || (region != "" && sp[0] != region) {
			continue
		}
		coll := map[bool]string{true: "backendBuckets", false: "backendServices"}[bucket]
		opts = append(opts, lbScopeColl(sp[0], coll)+"/"+sp[1])
	}
	sort.Strings(opts)
	f := console.Field{Name: "target", Label: "Target", Type: "select", Required: true, Options: opts,
		Help: "The backend service (or, for an edge policy, the backend bucket) to protect."}
	if len(opts) == 0 {
		f.Type, f.Options = "text", nil
	}
	return console.Action{ID: "applytarget", Label: "Apply policy to new target", Title: "Add target", Submit: "Add", Fields: []console.Field{f}}
}

func (p computeArmorProvider) ruleActions(path []string, r map[string]any, network bool) []console.Action {
	if num2s(r, "priority") == strconv.Itoa(armorDefaultPriority) {
		def, code := armorSplitAction(vpcStr(r, "action"))
		fields := []console.Field{
			{Name: "action", Label: "Action", Type: "radio", Default: def, Options: []string{"allow", "deny"}, OptionLabels: map[string]string{"allow": "Allow", "deny": "Deny"}},
			{Name: "responseCode", Label: "Response code", Type: "select", Default: vpcOr(code, "403"), Options: []string{"403", "404", "502"},
				VisibleWhen: map[string]string{"action": "deny"}},
		}
		if network {
			fields = fields[:1]
		}
		return []console.Action{{ID: "editrule", Label: "Edit", Title: "Edit rule", Submit: "Update", Fields: fields}}
	}
	return []console.Action{{ID: "editrule", Label: "Edit", Title: "Edit rule", Submit: "Update", Fields: armorRuleFieldsFor(r, false, network)},
		{ID: "deleterule", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Delete rule",
			Confirm: "Delete the rule of priority " + num2s(r, "priority") + "?"}}
}

// policyActions are a policy row's menu items and its page's.
func (p computeArmorProvider) policyActions(project string, path []string, o map[string]any) []console.Action {
	return []console.Action{p.editPolicyAction(o), p.addRuleAction(armorIsNetwork(o)), p.applyTargetAction(project, o)}
}

func (p computeArmorProvider) editPolicyAction(o map[string]any) console.Action {
	return console.Action{ID: "editpolicy", Label: "Edit", Title: "Edit policy", Submit: "Update", Fields: []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description")},
		arLabelsField(console.FormatMap(armorLabels(o)))}}
}

// armorLabels are a policy's labels.
func armorLabels(o map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range vmMap(o, "labels") {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// effectivePolicies is a backend service's getEffectiveSecurityPolicies,
// the policies' names; "—" for other targets.
func (p computeArmorProvider) effectivePolicies(project string, t []string) string {
	if t[0] != "backendServices" {
		return "—"
	}
	m, err := p.call("GET", project, lbScopeColl(t[1], t[0])+"/"+t[2]+"/getEffectiveSecurityPolicies", nil, nil)
	if err != nil {
		return "—"
	}
	var names []string
	for _, e := range vmMaps(m, "securityPolicies") {
		names = append(names, vpcOr(vpcStr(e, "shortName"), vpcStr(e, "name")))
	}
	return vpcDash(strings.Join(names, ", "))
}

// ruleDetail is a rule's own page, by its priority (getRule): its match,
// action and options, and Edit rule, Google's page ending in Update.
func (p computeArmorProvider) ruleDetail(project string, path []string) (console.Detail, error) {
	r, err := p.call("GET", project, armorRelOf(path)+"/getRule", url.Values{"priority": {path[3]}}, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	typ, match := armorMatchText(r)
	isDefault := path[3] == strconv.Itoa(armorDefaultPriority)
	if isDefault {
		typ = "Default"
	}
	props := []console.Property{{Label: "Priority", Value: path[3]}, {Label: "Description", Value: vpcDash(vpcStr(r, "description"))},
		{Label: "Mode", Value: typ}, {Label: "Match", Value: vpcDash(match)}, {Label: "Action", Value: armorActionText(vpcStr(r, "action"))},
		{Label: "Preview", Value: map[bool]string{true: "Enabled", false: "Disabled"}[r["preview"] == true]}}
	if rl := vmMap(r, "rateLimitOptions"); len(rl) > 0 {
		thr := vmMap(rl, "rateLimitThreshold")
		props = append(props, console.Property{Label: "Threshold", Value: num2s(thr, "count") + " requests per " + num2s(thr, "intervalSec") + " seconds"},
			console.Property{Label: "Enforce on key", Value: vpcOr(vpcStr(rl, "enforceOnKey"), "ALL")},
			console.Property{Label: "Exceed action", Value: vpcDash(vpcStr(rl, "exceedAction"))})
		if b := num2s(rl, "banDurationSec"); b != "" {
			props = append(props, console.Property{Label: "Ban duration", Value: b + " seconds"})
		}
	}
	if ro := vmMap(r, "redirectOptions"); len(ro) > 0 {
		props = append(props, console.Property{Label: "Redirect type", Value: vpcStr(ro, "type")}, console.Property{Label: "Target", Value: vpcDash(vpcStr(ro, "target"))})
	}
	pol, _ := p.policy(project, path)
	network := armorIsNetwork(pol)
	if network {
		nm := vmMap(r, "networkMatch")
		var parts []string
		for _, k := range []string{"srcIpRanges", "destIpRanges", "ipProtocols", "destPorts"} {
			if l := vpcStrs(nm, k); len(l) > 0 {
				parts = append(parts, k+": "+strings.Join(l, ", "))
			}
		}
		props[2], props[3] = console.Property{Label: "Mode", Value: map[bool]string{true: "Default", false: "Network match"}[isDefault]}, console.Property{Label: "Match", Value: vpcDash(strings.Join(parts, "; "))}
	}
	fields := armorRuleFieldsFor(r, false, network)
	if isDefault {
		fields = p.ruleActions(path, r, network)[0].Fields
	}
	return console.Detail{Title: "Priority " + path[3], Heading: "Rule details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit:     &console.EditForm{Label: "Edit", Title: "Edit rule", Submit: "Update", Page: true, Fields: fields}}, nil
}

// Edit is the rule page's Edit rule (Update).
func (p computeArmorProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 4 || path[2] != "rule" {
		return errors.New("Edit applies to a rule")
	}
	return p.ActAt(ctx, project, path, "editrule", v)
}

func (p computeArmorProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) < 2 {
		return nil
	}
	o, err := p.policy(project, path)
	if err != nil {
		return nil
	}
	switch {
	case len(path) == 2:
		d := armorDeletePolicy(path)
		d.Leaves = true
		return append(p.policyActions(project, path, o), d)
	case len(path) == 4 && path[2] == "rule":
		for _, r := range vmMaps(o, "rules") {
			if num2s(r, "priority") == path[3] {
				return p.ruleActions(path, r, armorIsNetwork(o))
			}
		}
	case len(path) == 6 && path[2] == "target":
		return []console.Action{{ID: "removetarget", Label: "Remove", Destructive: true}}
	}
	return nil
}

func (p computeArmorProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) < 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := armorRelOf(path)
	switch action {
	case "delete":
		_, err := p.call("DELETE", project, base, nil, nil)
		return err
	case "editpolicy":
		o, err := p.policy(project, path)
		if err != nil {
			return err
		}
		if d := strings.TrimSpace(v["description"]); d != vpcStr(o, "description") {
			if _, err = p.call("PATCH", project, base, nil, map[string]any{"description": d, "fingerprint": vpcStr(o, "fingerprint")}); err != nil {
				return err
			}
		}
		if lv, ok := v["labels"]; ok {
			labels, err := console.ParseMap(lv)
			if err != nil {
				return fmt.Errorf("Labels: %v", err)
			}
			if labelsText(labels) != labelsText(armorLabels(o)) {
				cur, err := p.policy(project, path)
				if err != nil {
					return err
				}
				l := map[string]any{}
				for k, x := range labels {
					l[k] = x
				}
				_, err = p.call("POST", project, base+"/setLabels", nil, map[string]any{"labels": l, "labelFingerprint": vpcStr(cur, "labelFingerprint")})
				return err
			}
		}
		return nil
	case "addrule":
		o, err := p.policy(project, path)
		if err != nil {
			return err
		}
		r, err := armorRuleFor(v, "", armorIsNetwork(o))
		if err != nil {
			return err
		}
		if _, ok := r["priority"]; !ok {
			return errors.New("Priority: give the rule a priority")
		}
		_, err = p.call("POST", project, base+"/addRule", nil, r)
		return err
	case "editrule":
		if len(path) != 4 {
			return errors.New("Edit applies to a rule")
		}
		q := url.Values{"priority": {path[3]}}
		if path[3] == strconv.Itoa(armorDefaultPriority) {
			a := "allow"
			if v["action"] == "deny" {
				a = "deny(" + vpcOr(v["responseCode"], "403") + ")"
				if o, err := p.policy(project, path); err == nil && armorIsNetwork(o) {
					a = "deny"
				}
			}
			_, err := p.call("POST", project, base+"/patchRule", q, map[string]any{"action": a})
			return err
		}
		o, err := p.policy(project, path)
		if err != nil {
			return err
		}
		r, err := armorRuleFor(v, "", armorIsNetwork(o))
		if err != nil {
			return err
		}
		pri, _ := strconv.ParseFloat(path[3], 64)
		r["priority"] = pri
		_, err = p.call("POST", project, base+"/patchRule", q, r)
		return err
	case "deleterule":
		if len(path) != 4 {
			return errors.New("Delete applies to a rule")
		}
		_, err := p.call("POST", project, base+"/removeRule", url.Values{"priority": {path[3]}}, nil)
		return err
	case "applytarget":
		o, err := p.policy(project, path)
		if err != nil {
			return err
		}
		t := strings.TrimSpace(v["target"])
		if t == "" {
			return errors.New("Target: choose one")
		}
		return p.attach(project, path, vpcStr(o, "type"), t)
	case "removetarget":
		if len(path) != 6 {
			return errors.New("Remove applies to a target")
		}
		o, err := p.policy(project, path)
		if err != nil {
			return err
		}
		return p.attach(project, []string{path[0], ""}, vpcStr(o, "type"), armorTargetColl(path[3], path[4])+"/"+path[5])
	}
	return fmt.Errorf("%s is not available here", action)
}
