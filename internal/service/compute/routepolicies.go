package compute

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Cloud Router route policies and named sets, and Private NAT (#2140), as
// the REST references of routers.patchRoutePolicy, updateRoutePolicy,
// getRoutePolicy, listRoutePolicies, deleteRoutePolicy and the named-set
// methods, network-connectivity/docs/router/how-to/use-route-policies,
// nat/docs/private-nat and nat/docs/set-up-private-nat (read 2026-11-22)
// describe them:
//
//   - a route policy is a router's named, typed (ROUTE_POLICY_TYPE_IMPORT
//     or ROUTE_POLICY_TYPE_EXPORT) list of terms; each term has a priority
//     from 0 to 2^31-1, unique in the policy, an optional CEL match and CEL
//     actions; its fingerprint changes on every change and, given, must be
//     the current one. A BGP peer's importPolicies and exportPolicies name
//     policies of that type. Policies are not part of the router resource;
//     they are read through getRoutePolicy and listRoutePolicies;
//   - a named set is a router's named list of prefix or community
//     expressions (NAMED_SET_TYPE_PREFIX, NAMED_SET_TYPE_COMMUNITY) that
//     policies refer to;
//   - a Private NAT gateway (nats[].type PRIVATE) translates its source
//     subnets to ranges of subnets of purpose PRIVATE_NAT, chosen by rules
//     whose match names the next hop (nexthop.hub == "//networkconnectivity
//     .googleapis.com/projects/P/locations/global/hubs/H", nexthop.is_hybrid,
//     or both), every rule of a gateway with the same next hop; it has no
//     NAT IPs and no Endpoint-Independent Mapping, and uses dynamic port
//     allocation by default.
//
// What is recorded and what is carried: no BGP session comes up, so no
// policy is evaluated against a route; the CEL expressions are checked for
// their syntax (balanced brackets and quotes, known operators) and kept as
// given. Network Connectivity Center is not served, so a hub a Private NAT
// rule names is checked for its form only, and no packet is translated
// (docs/gaps.md).

// routePolicyMethods are the router custom methods of this file, by the
// HTTP method they take.
var routePolicyMethods = map[string]string{
	"getRoutePolicy": http.MethodGet, "listRoutePolicies": http.MethodGet,
	"patchRoutePolicy": http.MethodPost, "updateRoutePolicy": http.MethodPost, "deleteRoutePolicy": http.MethodPost,
	"getNamedSet": http.MethodGet, "listNamedSets": http.MethodGet,
	"patchNamedSet": http.MethodPost, "updateNamedSet": http.MethodPost, "deleteNamedSet": http.MethodPost,
}

// policyKind is what a route policy or named set method works on.
type policyKind struct {
	hidden, param, typeField, what string
	types                          []string
	listKind                       string
}

var (
	routePolicyKind = policyKind{hidden: "_routePolicies", param: "policy", what: "Route policy",
		types: []string{"ROUTE_POLICY_TYPE_EXPORT", "ROUTE_POLICY_TYPE_IMPORT"}, listKind: "compute#routersListRoutePolicies"}
	namedSetKind = policyKind{hidden: "_namedSets", param: "namedSet", what: "Named set",
		types: []string{"NAMED_SET_TYPE_COMMUNITY", "NAMED_SET_TYPE_PREFIX"}, listKind: "compute#routersListNamedSets"}
)

func (s *Server) routePolicies(c call, region, router, method string) (obj, error) {
	if c.method != routePolicyMethods[method] {
		return nil, errf(http.StatusMethodNotAllowed, "methodNotAllowed", "%s takes %s", method, routePolicyMethods[method])
	}
	k := routePolicyKind
	if strings.HasSuffix(method, "NamedSet") || strings.HasSuffix(method, "NamedSets") {
		k = namedSetKind
	}
	r, rel, err := s.routerOf(c.project, region, router)
	if err != nil {
		return nil, err
	}
	all, _ := r[k.hidden].(map[string]any)
	switch {
	case strings.HasPrefix(method, "get"):
		name := first(c.query, k.param)
		if name == "" {
			return nil, required(k.param)
		}
		p, ok := all[name]
		if !ok {
			return nil, errf(http.StatusNotFound, "notFound", "%s '%s' not found in router '%s'.", k.what, name, rel)
		}
		return obj{"resource": deepCopy(p)}, nil
	case strings.HasPrefix(method, "list"):
		names := make([]string, 0, len(all))
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		res := make([]any, 0, len(names))
		for _, n := range names {
			res = append(res, deepCopy(all[n]))
		}
		return obj{"kind": k.listKind, "id": rel, "result": res, "selfLink": rel + "/" + method}, nil
	case strings.HasPrefix(method, "delete"):
		name := first(c.query, k.param)
		if name == "" {
			return nil, required(k.param)
		}
		if _, ok := all[name]; !ok {
			return nil, errf(http.StatusNotFound, "notFound", "%s '%s' not found in router '%s'.", k.what, name, rel)
		}
		if k.hidden == routePolicyKind.hidden {
			for _, e := range sliceOf(r["bgpPeers"]) {
				pm, _ := e.(map[string]any)
				if hasStr(strsAny(pm["importPolicies"]), name) || hasStr(strsAny(pm["exportPolicies"]), name) {
					return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
						"Route policy '%s' is used by BGP peer '%s' of router '%s'.", name, str(pm, "name"), rel)
				}
			}
		}
		delete(all, name)
		return s.done(c.project, "regions/"+region, method, rel, r["id"])
	}
	// patch or update
	name := str(c.body, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	old, _ := all[name].(map[string]any)
	if fp := str(c.body, "fingerprint"); fp != "" && old != nil && fp != str(old, "fingerprint") {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
			"Supplied fingerprint does not match current fingerprint of %s '%s'.", strings.ToLower(k.what), name)
	}
	merged := obj{}
	if strings.HasPrefix(method, "patch") && old != nil {
		for kk, v := range deepCopy(old).(map[string]any) {
			merged[kk] = v
		}
	}
	for kk, v := range c.body {
		merged[kk] = v
	}
	var o obj
	if k.hidden == routePolicyKind.hidden {
		o, err = checkRoutePolicy(merged)
	} else {
		o, err = checkNamedSet(merged)
	}
	if err != nil {
		return nil, err
	}
	if old != nil && str(old, "type") != str(o, "type") {
		return nil, invalid("Invalid value for field 'resource.type': '%s'. The type of %s '%s' cannot be changed.", str(o, "type"), strings.ToLower(k.what), name)
	}
	o["fingerprint"] = newFingerprint()
	if all == nil {
		all = map[string]any{}
		r[k.hidden] = all
	}
	all[name] = o
	return s.done(c.project, "regions/"+region, method, rel, r["id"])
}

func checkPolicyType(k policyKind, b obj) (string, error) {
	t := str(b, "type")
	if t == "" {
		return "", required("resource.type")
	}
	if !hasStr(k.types, t) {
		return "", invalid("Invalid value for field 'resource.type': '%s'. Must be one of [%s]", t, strings.Join(k.types, ", "))
	}
	return t, nil
}

// checkExpr is a CEL Expr object (expression, title, description,
// location) checked at field f.
func checkExpr(f string, v any) (obj, error) {
	m, _ := v.(map[string]any)
	e := strings.TrimSpace(str(m, "expression"))
	if e == "" {
		return nil, required(f + ".expression")
	}
	if err := celSyntax(e); err != nil {
		return nil, invalid("Invalid value for field '%s.expression': '%s'. %s", f, e, err)
	}
	o := obj{"expression": e}
	for _, k := range []string{"title", "description", "location"} {
		if x := str(m, k); x != "" {
			o[k] = x
		}
	}
	return o, nil
}

// celSyntax checks what can be checked of a CEL expression without its
// environment: balanced brackets, closed string literals, and no
// assignment or statement separators (CEL has neither).
func celSyntax(e string) error {
	var stack []rune
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	for i := 0; i < len(e); i++ {
		ch := rune(e[i])
		switch ch {
		case '\'', '"':
			j := i + 1
			for j < len(e) && rune(e[j]) != ch {
				if e[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(e) {
				return fmt.Errorf("Syntax error: unterminated string literal at position %d.", i)
			}
			i = j
		case '(', '[', '{':
			stack = append(stack, ch)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[ch] {
				return fmt.Errorf("Syntax error: mismatched '%c' at position %d.", ch, i)
			}
			stack = stack[:len(stack)-1]
		case ';':
			return fmt.Errorf("Syntax error: unexpected ';' at position %d.", i)
		case '=':
			prev, next := byte(0), byte(0)
			if i > 0 {
				prev = e[i-1]
			}
			if i+1 < len(e) {
				next = e[i+1]
			}
			if next != '=' && prev != '=' && prev != '!' && prev != '<' && prev != '>' {
				return fmt.Errorf("Syntax error: unexpected '=' at position %d.", i)
			}
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("Syntax error: missing closing bracket for '%c'.", stack[len(stack)-1])
	}
	return nil
}

func checkRoutePolicy(b obj) (obj, error) {
	t, err := checkPolicyType(routePolicyKind, b)
	if err != nil {
		return nil, err
	}
	o := obj{"name": str(b, "name"), "type": t}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	seen := map[int64]bool{}
	terms := []any{}
	for i, e := range sliceOf(b["terms"]) {
		m, _ := e.(map[string]any)
		f := "resource.terms[" + strconv.Itoa(i) + "]"
		pr, ok := m["priority"].(float64)
		if !ok {
			return nil, required(f + ".priority")
		}
		if pr < 0 || pr >= 1<<31 || pr != float64(int64(pr)) {
			return nil, invalid("Invalid value for field '%s.priority': '%v'. Must be from 0 (inclusive) to 2^31 (exclusive).", f, pr)
		}
		if seen[int64(pr)] {
			return nil, invalid("Invalid value for field '%s.priority': '%d'. Term priorities must be unique within a route policy.", f, int64(pr))
		}
		seen[int64(pr)] = true
		to := obj{"priority": pr}
		if mv, ok := m["match"]; ok && mv != nil {
			x, err := checkExpr(f+".match", mv)
			if err != nil {
				return nil, err
			}
			to["match"] = x
		}
		var acts []any
		for j, a := range sliceOf(m["actions"]) {
			x, err := checkExpr(fmt.Sprintf("%s.actions[%d]", f, j), a)
			if err != nil {
				return nil, err
			}
			acts = append(acts, x)
		}
		if len(acts) > 0 {
			to["actions"] = acts
		}
		terms = append(terms, to)
	}
	sort.SliceStable(terms, func(i, j int) bool {
		return terms[i].(obj)["priority"].(float64) < terms[j].(obj)["priority"].(float64)
	})
	if len(terms) > 0 {
		o["terms"] = terms
	}
	return o, nil
}

func checkNamedSet(b obj) (obj, error) {
	t, err := checkPolicyType(namedSetKind, b)
	if err != nil {
		return nil, err
	}
	o := obj{"name": str(b, "name"), "type": t}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	var els []any
	for i, e := range sliceOf(b["elements"]) {
		x, err := checkExpr("resource.elements["+strconv.Itoa(i)+"]", e)
		if err != nil {
			return nil, err
		}
		els = append(els, x)
	}
	if len(els) > 0 {
		o["elements"] = els
	}
	return o, nil
}

// peerPolicies checks a BGP peer's importPolicies and exportPolicies
// against the router r's route policies: each must exist and be of the
// list's type (routers reference, bgpPeers[].importPolicies).
func peerPolicies(f string, m, o, r obj) error {
	all, _ := r["_routePolicies"].(map[string]any)
	for field, want := range map[string]string{"importPolicies": "ROUTE_POLICY_TYPE_IMPORT", "exportPolicies": "ROUTE_POLICY_TYPE_EXPORT"} {
		names := strsAny(m[field])
		for j, n := range names {
			p, ok := all[n].(map[string]any)
			if !ok {
				return invalid("Invalid value for field '%s.%s[%d]': '%s'. The route policy does not exist in the router.", f, field, j, n)
			}
			if str(p, "type") != want {
				return invalid("Invalid value for field '%s.%s[%d]': '%s'. The route policy must be of type %s.", f, field, j, n, want)
			}
		}
		if len(names) > 0 {
			o[field] = toAny(names)
		}
	}
	return nil
}

// natTypeOf is a stored gateway's type.
func natTypeOf(m map[string]any) string {
	if str(m, "type") == natTypePrivate {
		return natTypePrivate
	}
	return natTypePublic
}

// checkPrivateNat checks what a Private NAT gateway must not have: NAT
// IPs (it translates to its rules' PRIVATE_NAT subnets) and
// Endpoint-Independent Mapping; it defaults dynamic port allocation on
// (nat/docs/set-up-private-nat).
func checkPrivateNat(f string, m map[string]any) error {
	for _, k := range []string{"natIpAllocateOption", "autoNetworkTier"} {
		if v := str(m, k); v != "" {
			return invalid("Invalid value for field '%s.%s': '%s'. A Private NAT gateway has no NAT IP allocation; its rules' sourceNatActiveRanges give its ranges.", f, k, v)
		}
	}
	for _, k := range []string{"natIps", "drainNatIps"} {
		if len(sliceOf(m[k])) > 0 {
			return invalid("Invalid value for field '%s.%s': a Private NAT gateway has no NAT IPs.", f, k)
		}
	}
	if m["enableEndpointIndependentMapping"] == true {
		return invalid("Invalid value for field '%s.enableEndpointIndependentMapping': Private NAT does not support Endpoint-Independent Mapping.", f)
	}
	if _, ok := m["enableDynamicPortAllocation"]; !ok {
		m["enableDynamicPortAllocation"] = true
	}
	return nil
}

var (
	natHubMatch    = regexp.MustCompile(`^nexthop\.hub\s*==\s*["']//networkconnectivity\.googleapis\.com/projects/[a-z0-9-]+/locations/global/hubs/[a-z]([-a-z0-9]*[a-z0-9])?["']$`)
	natSourceMatch = regexp.MustCompile(`^(source\.ip\s*==\s*["'][0-9a-fA-F.:]+["']|inIpRange\(source\.ip,\s*["'][0-9a-fA-F.:/]+["']\))$`)
)

// privateNatNextHop is the next hop of a Private NAT rule's match: the
// hub, "hybrid", or both, after optional source conditions joined by &&.
func privateNatNextHop(match string) (string, bool) {
	parts := strings.Split(match, "&&")
	hop := strings.TrimSpace(parts[len(parts)-1])
	for _, p := range parts[:len(parts)-1] {
		if !natSourceMatch.MatchString(stripParens(p)) {
			return "", false
		}
	}
	hop = stripParens(hop)
	var hops []string
	for _, h := range strings.Split(hop, "||") {
		h = strings.TrimSpace(h)
		switch {
		case h == "nexthop.is_hybrid":
			hops = append(hops, "hybrid")
		case natHubMatch.MatchString(h):
			hops = append(hops, strings.Trim(strings.TrimSpace(h[strings.Index(h, "==")+2:]), `"'`))
		default:
			return "", false
		}
	}
	sort.Strings(hops)
	return strings.Join(hops, "|"), true
}

// stripParens is e trimmed, without parentheses that wrap all of it.
func stripParens(e string) string {
	e = strings.TrimSpace(e)
	for strings.HasPrefix(e, "(") && strings.HasSuffix(e, ")") && celSyntax(e[1:len(e)-1]) == nil {
		e = strings.TrimSpace(e[1 : len(e)-1])
	}
	return e
}

// checkPrivateNatRules checks a Private NAT gateway's rules: a next-hop
// match, the same next hop in every rule, and active (and drain) ranges
// that are PRIVATE_NAT subnets of the router's network and region.
func (s *Server) checkPrivateNatRules(project, region, network, f string, l []any) ([]any, error) {
	var out []any
	seen := map[int]bool{}
	hop := ""
	for i, e := range l {
		m, _ := e.(map[string]any)
		rf := f + ".rules[" + strconv.Itoa(i) + "]"
		num, ok := m["ruleNumber"].(float64)
		if !ok {
			return nil, required(rf + ".ruleNumber")
		}
		if num < 0 || num > 65000 {
			return nil, invalid("Invalid value for field '%s.ruleNumber': '%d'. Must be between 0 and 65000.", rf, int64(num))
		}
		if seen[int(num)] {
			return nil, invalid("Invalid value for field '%s.ruleNumber': '%d'. Rule numbers are unique in a NAT gateway.", rf, int64(num))
		}
		seen[int(num)] = true
		match := strings.TrimSpace(str(m, "match"))
		if match == "" {
			return nil, required(rf + ".match")
		}
		h, ok := privateNatNextHop(match)
		if !ok {
			return nil, invalid("Invalid value for field '%s.match': '%s'. A Private NAT rule matches on its next hop: nexthop.hub == '//networkconnectivity.googleapis.com/projects/PROJECT_ID/locations/global/hubs/HUB', nexthop.is_hybrid, or both joined by ||, optionally after source.ip conditions joined by &&.", rf, match)
		}
		if hop != "" && h != hop {
			return nil, invalid("Invalid value for field '%s.match': '%s'. All NAT rules of a gateway must be configured with the same next hop.", rf, match)
		}
		hop = h
		o := obj{"ruleNumber": num, "match": match}
		if d := str(m, "description"); d != "" {
			o["description"] = d
		}
		act, _ := m["action"].(map[string]any)
		for _, k := range []string{"sourceNatActiveIps", "sourceNatDrainIps"} {
			if len(sliceOf(act[k])) > 0 {
				return nil, invalid("Invalid value for field '%s.action.%s': a Private NAT rule translates to subnet ranges (sourceNatActiveRanges), not IP addresses.", rf, k)
			}
		}
		oa := obj{}
		used := map[string]bool{}
		for _, k := range []string{"sourceNatActiveRanges", "sourceNatDrainRanges"} {
			var rels []any
			for j, ref := range strsAny(act[k]) {
				sf := fmt.Sprintf("%s.action.%s[%d]", rf, k, j)
				sr := Rel(ref, project)
				if !strings.Contains(sr, "/") {
					sr = subnetRel(project, region, sr)
				}
				so, ok := s.res[sr]
				if !ok || !strings.Contains(sr, "/subnetworks/") {
					return nil, notFound(sr)
				}
				if str(so, "purpose") != "PRIVATE_NAT" {
					return nil, invalid("Invalid value for field '%s': '%s'. A Private NAT range must be a subnetwork of purpose PRIVATE_NAT.", sf, ref)
				}
				if str(so, "network") != network || lastSeg(str(so, "region")) != region {
					return nil, invalid("Invalid value for field '%s': '%s'. The subnetwork must be in the router's network and region.", sf, ref)
				}
				if used[sr] {
					return nil, invalid("Invalid value for field '%s': '%s'. The subnetwork is given twice.", sf, ref)
				}
				used[sr] = true
				rels = append(rels, sr)
			}
			if len(rels) > 0 {
				oa[k] = rels
			}
		}
		if oa["sourceNatActiveRanges"] == nil {
			return nil, required(rf + ".action.sourceNatActiveRanges")
		}
		o["action"] = oa
		out = append(out, o)
	}
	return out, nil
}
