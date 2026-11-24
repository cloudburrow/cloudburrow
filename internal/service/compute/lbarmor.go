package compute

import (
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Google Cloud Armor security policies (#2160), as
// https://docs.cloud.google.com/armor/docs/security-policy-overview,
// .../armor/docs/configure-security-policies, .../rules-language-reference,
// .../rate-limiting-overview, .../waf-rules, .../armor/quotas and the
// securityPolicies, regionSecurityPolicies and networkEdgeSecurityServices
// REST references describe them (read 2026-11-22):
//
//   - a backend security policy (type CLOUD_ARMOR) on a backend service
//     (securityPolicy) filters the requests that reach its backends, after
//     Cloud CDN's cache (a hit is not filtered);
//   - an edge security policy (CLOUD_ARMOR_EDGE) on a backend service or
//     bucket (edgeSecurityPolicy) filters every request, before the cache,
//     with allow and deny only and the edge attributes only;
//   - a network edge security policy (CLOUD_ARMOR_NETWORK, regional),
//     through the region's network edge security service or on a target
//     pool or instance, filters the packets of external passthrough
//     Network Load Balancers by their network match;
//
// each by its rules in priority order (the lowest number first): the first
// rule whose match holds decides (allow, deny(STATUS), redirect, throttle
// or rate_based_ban), and a rule in preview is evaluated but not enforced.
// The data plane (lbarmoreval.go) enforces them on every request through
// the local proxy.

func init() {
	registerLB(&lbColl{coll: "securityPolicies", kind: "compute#securityPolicy", global: true, regional: true, patch: true,
		fingerprint: true, build: buildSecurityPolicy, noun: "securityPolicy",
		post: lbPost{"addRule": addSecurityRule, "patchRule": patchSecurityRule, "removeRule": removeSecurityRule,
			"setLabels": setSecurityPolicyLabels},
		get: map[string]func(*Server, lbCtx, obj, call) (obj, error){"getRule": getSecurityRule},
		collGet: map[string]func(*Server, lbCtx, call) (obj, error){
			"listPreconfiguredExpressionSets": func(*Server, lbCtx, call) (obj, error) { return preconfiguredExpressionSets(), nil }}})
	registerLB(&lbColl{coll: "networkEdgeSecurityServices", kind: "compute#networkEdgeSecurityService", regional: true, patch: true,
		fingerprint: true, build: buildNetworkEdgeSecurityService, noun: "networkEdgeSecurityService"})
}

// Rule priorities: the default rule's is the largest.
const (
	defaultRulePriority = 2147483647
	maxSrcIPRanges      = 10
	maxExprLen          = 2048
	maxSubexprLen       = 1024
	maxSubexprs         = 10
	maxNetworkEdgeRules = 100
)

var policyTypes = []string{"CLOUD_ARMOR", "CLOUD_ARMOR_EDGE", "CLOUD_ARMOR_NETWORK"}

// The documented rate limiting values (rate-limiting-overview).
var (
	rateIntervals = map[int]bool{10: true, 30: true, 60: true, 120: true, 180: true, 240: true, 300: true, 600: true,
		900: true, 1200: true, 1800: true, 2700: true, 3600: true}
	banDurations = map[int]bool{60: true, 120: true, 180: true, 240: true, 300: true, 600: true, 900: true, 1200: true,
		1800: true, 2700: true, 3600: true}
	enforceKeys = []string{"ALL", "IP", "HTTP_HEADER", "XFF_IP", "HTTP_COOKIE", "HTTP_PATH", "SNI", "REGION_CODE",
		"TLS_JA3_FINGERPRINT", "TLS_JA4_FINGERPRINT", "USER_IP"}
)

func buildSecurityPolicy(s *Server, x lbCtx, b, old obj) (obj, error) {
	typ, err := oneOf("resource.type", str(b, "type"), "CLOUD_ARMOR", policyTypes...)
	if err != nil {
		return nil, err
	}
	if old != nil && str(old, "type") != typ {
		return nil, invalid("Invalid value for field 'resource.type': '%s'. The type of a security policy cannot be changed.", typ)
	}
	if typ == "CLOUD_ARMOR_NETWORK" && x.region == "" {
		return nil, invalid("Invalid value for field 'resource.type': 'CLOUD_ARMOR_NETWORK'. Network edge security policies are regional.")
	}
	if typ == "CLOUD_ARMOR_EDGE" && x.region != "" {
		return nil, invalid("Invalid value for field 'resource.type': 'CLOUD_ARMOR_EDGE'. Edge security policies are global.")
	}
	o := obj{"type": typ}
	copyFields(o, b, "adaptiveProtectionConfig", "recaptchaOptionsConfig", "userDefinedFields", "labels")
	if typ != "CLOUD_ARMOR" && b["adaptiveProtectionConfig"] != nil {
		return nil, invalid("Invalid value for field 'resource.adaptiveProtectionConfig': ''. Adaptive Protection is available only for backend security policies.")
	}
	if ao := objOf(b["advancedOptionsConfig"]); ao != nil || typ == "CLOUD_ARMOR" {
		adv := obj{}
		jp, err := oneOf("resource.advancedOptionsConfig.jsonParsing", str(ao, "jsonParsing"), "DISABLED", "DISABLED", "STANDARD", "STANDARD_WITH_GRAPHQL")
		if err != nil {
			return nil, err
		}
		ll, err := oneOf("resource.advancedOptionsConfig.logLevel", str(ao, "logLevel"), "NORMAL", "NORMAL", "VERBOSE")
		if err != nil {
			return nil, err
		}
		adv["jsonParsing"], adv["logLevel"] = jp, ll
		copyFields(adv, ao, "jsonCustomConfig", "userIpRequestHeaders")
		if v := str(ao, "requestBodyInspectionSize"); v != "" {
			if _, ok := bodyInspectionSizes[v]; !ok {
				return nil, invalid("Invalid value for field 'resource.advancedOptionsConfig.requestBodyInspectionSize': '%s'. Must be one of [8KB, 16KB, 32KB, 48KB, 64KB].", v)
			}
			adv["requestBodyInspectionSize"] = v
		}
		o["advancedOptionsConfig"] = adv
	}
	if dd := objOf(b["ddosProtectionConfig"]); dd != nil {
		if typ != "CLOUD_ARMOR_NETWORK" {
			return nil, invalid("Invalid value for field 'resource.ddosProtectionConfig': ''. Advanced network DDoS protection is set on network edge security policies only.")
		}
		v, err := oneOf("resource.ddosProtectionConfig.ddosProtection", str(dd, "ddosProtection"), "STANDARD", "STANDARD", "ADVANCED", "ADVANCED_PREVIEW")
		if err != nil {
			return nil, err
		}
		o["ddosProtectionConfig"] = obj{"ddosProtection": v}
	}
	var rules []obj
	seen := map[int]bool{}
	for i, e := range listOf(b["rules"]) {
		r, err := buildSecurityRule(s, typ, objOf(e), "resource.rules["+strconv.Itoa(i)+"]", old == nil)
		if err != nil {
			return nil, err
		}
		if err := s.checkAddressGroupUse(x, typ, r, "resource.rules["+strconv.Itoa(i)+"]"); err != nil {
			return nil, err
		}
		p := rulePriority(r)
		if seen[p] {
			return nil, invalid("Invalid value for field 'resource.rules[%d].priority': '%d'. Cannot have rules with the same priorities.", i, p)
		}
		seen[p] = true
		rules = append(rules, r)
	}
	if !seen[defaultRulePriority] {
		rules = append(rules, defaultSecurityRule(typ))
	}
	if typ == "CLOUD_ARMOR_NETWORK" && len(rules) > maxNetworkEdgeRules {
		return nil, invalid("Invalid value for field 'resource.rules': ''. A network edge security policy can have at most %d rules.", maxNetworkEdgeRules)
	}
	o["rules"] = sortedRules(rules)
	o["labelFingerprint"] = labelFingerprint(o["labels"])
	return o, nil
}

var bodyInspectionSizes = map[string]int{"8KB": 8 << 10, "16KB": 16 << 10, "32KB": 32 << 10, "48KB": 48 << 10, "64KB": 64 << 10}

// defaultSecurityRule is the rule Google adds to a policy made without
// one: allow everything, at the lowest priority.
func defaultSecurityRule(typ string) obj {
	r := obj{"kind": "compute#securityPolicyRule", "description": "default rule", "priority": float64(defaultRulePriority),
		"action": "allow", "preview": false}
	if typ == "CLOUD_ARMOR_NETWORK" {
		r["match"] = nil
		delete(r, "match")
		r["networkMatch"] = obj{"srcIpRanges": []any{"*"}}
		return r
	}
	r["match"] = obj{"versionedExpr": "SRC_IPS_V1", "config": obj{"srcIpRanges": []any{"*"}}}
	return r
}

func rulePriority(r obj) int { return int(numOr(r, "priority", 0)) }

func sortedRules(rules []obj) []any {
	sort.SliceStable(rules, func(i, j int) bool { return rulePriority(rules[i]) < rulePriority(rules[j]) })
	out := make([]any, len(rules))
	for i, r := range rules {
		out[i] = r
	}
	return out
}

var denyRE = regexp.MustCompile(`^deny(\((\d+)\))?$`)

// buildSecurityRule checks one rule of a policy of type typ.
func buildSecurityRule(s *Server, typ string, b obj, field string, _ bool) (obj, error) {
	if b == nil {
		return nil, required(field)
	}
	r := obj{"kind": "compute#securityPolicyRule"}
	p, ok := num(b, "priority")
	if !ok {
		if v, err := strconv.Atoi(str(b, "priority")); err == nil {
			p, ok = float64(v), true
		}
	}
	if !ok {
		p = 0
	}
	if p < 0 || p > defaultRulePriority || p != float64(int64(p)) {
		return nil, invalid("Invalid value for field '%s.priority': '%v'. Must be an integer from 0 to 2147483647.", field, p)
	}
	r["priority"] = p
	action := str(b, "action")
	if action == "" {
		return nil, required(field + ".action")
	}
	switch {
	case action == "allow", action == "redirect", action == "throttle", action == "rate_based_ban":
	case denyRE.MatchString(action):
		m := denyRE.FindStringSubmatch(action)
		if m[2] == "" {
			action = "deny(403)"
		} else if m[2] != "403" && m[2] != "404" && m[2] != "502" {
			return nil, invalid("Invalid value for field '%s.action': '%s'. A deny action's status must be 403, 404 or 502.", field, action)
		}
	default:
		return nil, invalid("Invalid value for field '%s.action': '%s'. Must be one of [allow, deny(403), deny(404), deny(502), redirect, throttle, rate_based_ban].", field, action)
	}
	if typ != "CLOUD_ARMOR" && action != "allow" && !strings.HasPrefix(action, "deny") {
		return nil, invalid("Invalid value for field '%s.action': '%s'. %s security policies support only the allow and deny actions.", field, action, policyNoun(typ))
	}
	r["action"] = action
	r["preview"] = b["preview"] == true
	if d := str(b, "description"); d != "" {
		r["description"] = d
	}
	if typ == "CLOUD_ARMOR_NETWORK" {
		nm := objOf(b["networkMatch"])
		if nm == nil {
			return nil, required(field + ".networkMatch")
		}
		if b["match"] != nil {
			return nil, invalid("Invalid value for field '%s.match': ''. A network edge security policy's rules match with networkMatch.", field)
		}
		for _, k := range []string{"srcIpRanges", "destIpRanges"} {
			l := strs(nm, k)
			if len(l) > maxSrcIPRanges {
				return nil, invalid("Invalid value for field '%s.networkMatch.%s': ''. At most %d IP ranges per rule.", field, k, maxSrcIPRanges)
			}
			for _, c := range l {
				if c != "*" && !validCIDROrIP(c) {
					return nil, invalid("Invalid value for field '%s.networkMatch.%s': '%s'. Must be an IP address or range.", field, k, c)
				}
			}
		}
		r["networkMatch"] = deepCopy(nm)
	} else {
		m, err := buildRuleMatch(typ, objOf(b["match"]), field+".match")
		if err != nil {
			return nil, err
		}
		r["match"] = m
	}
	if pw := objOf(b["preconfiguredWafConfig"]); pw != nil {
		r["preconfiguredWafConfig"] = deepCopy(pw)
	}
	if ha := objOf(b["headerAction"]); ha != nil {
		for i, e := range listOf(ha["requestHeadersToAdds"]) {
			if str(objOf(e), "headerName") == "" {
				return nil, required(field + ".headerAction.requestHeadersToAdds[" + strconv.Itoa(i) + "].headerName")
			}
		}
		r["headerAction"] = deepCopy(ha)
	}
	if action == "redirect" {
		ro, err := buildRedirectOptions(objOf(b["redirectOptions"]), field+".redirectOptions")
		if err != nil {
			return nil, err
		}
		r["redirectOptions"] = ro
	} else if b["redirectOptions"] != nil {
		return nil, invalid("Invalid value for field '%s.redirectOptions': ''. redirectOptions is set only with the redirect action.", field)
	}
	rl := objOf(b["rateLimitOptions"])
	switch {
	case action == "throttle" || action == "rate_based_ban":
		o, err := buildRateLimit(action, rl, field+".rateLimitOptions")
		if err != nil {
			return nil, err
		}
		r["rateLimitOptions"] = o
	case rl != nil:
		return nil, invalid("Invalid value for field '%s.rateLimitOptions': ''. rateLimitOptions is set only with the throttle and rate_based_ban actions.", field)
	}
	_ = s
	return r, nil
}

func policyNoun(typ string) string {
	if typ == "CLOUD_ARMOR_NETWORK" {
		return "Network edge"
	}
	return "Edge"
}

func validCIDROrIP(c string) bool {
	if _, _, err := net.ParseCIDR(c); err == nil {
		return true
	}
	return net.ParseIP(c) != nil
}

// buildRuleMatch checks a rule's match: a basic one (versionedExpr
// SRC_IPS_V1 and config.srcIpRanges) or an advanced one (expr, in the
// custom rules language).
func buildRuleMatch(typ string, m obj, field string) (obj, error) {
	if m == nil {
		return nil, required(field)
	}
	ve, ex := str(m, "versionedExpr"), objOf(m["expr"])
	switch {
	case ve != "" && ex != nil:
		return nil, invalid("Invalid value for field '%s': ''. A match has either versionedExpr or expr, not both.", field)
	case ve != "":
		if ve != "SRC_IPS_V1" {
			return nil, invalid("Invalid value for field '%s.versionedExpr': '%s'. Must be one of [SRC_IPS_V1].", field, ve)
		}
		ranges := strs(objOf(m["config"]), "srcIpRanges")
		if len(ranges) == 0 {
			return nil, required(field + ".config.srcIpRanges")
		}
		if len(ranges) > maxSrcIPRanges {
			return nil, invalid("Invalid value for field '%s.config.srcIpRanges': ''. A rule can match at most %d IP addresses or ranges.", field, maxSrcIPRanges)
		}
		for i, c := range ranges {
			if c != "*" && !validCIDROrIP(c) {
				return nil, invalid("Invalid value for field '%s.config.srcIpRanges[%d]': '%s'. Must be an IP address or a CIDR range.", field, i, c)
			}
		}
		return obj{"versionedExpr": ve, "config": obj{"srcIpRanges": toAny(ranges)}}, nil
	case ex != nil:
		e := str(ex, "expression")
		if e == "" {
			return nil, required(field + ".expr.expression")
		}
		if err := checkArmorExpr(typ, e, field+".expr.expression"); err != nil {
			return nil, err
		}
		out := obj{"expr": obj{"expression": e}}
		if t := str(ex, "title"); t != "" {
			out["expr"].(obj)["title"] = t
		}
		if eo := objOf(m["exprOptions"]); eo != nil {
			out["exprOptions"] = deepCopy(eo)
		}
		return out, nil
	}
	return nil, required(field + ".versionedExpr")
}

// checkArmorExpr checks an advanced match condition against the
// documented limits, and compiles it.
func checkArmorExpr(typ, e, field string) error {
	if len(e) > maxExprLen {
		return invalid("Invalid value for field '%s': ''. An expression can have at most %d characters.", field, maxExprLen)
	}
	subs := splitSubexprs(e)
	if len(subs) > maxSubexprs {
		return invalid("Invalid value for field '%s': ''. An expression can have at most %d subexpressions.", field, maxSubexprs)
	}
	for _, sub := range subs {
		if len(sub) > maxSubexprLen {
			return invalid("Invalid value for field '%s': ''. A subexpression can have at most %d characters.", field, maxSubexprLen)
		}
	}
	if m := unservedArmorFunc.FindString(e); m != "" {
		return unimplemented("%s is not served here: it reads Google Threat Intelligence's IP feeds, Adaptive Protection's models or Google's managed protection rules, which Google keeps and a local proxy has no copy of (#2160)", strings.TrimSuffix(m, "("))
	}
	if typ == "CLOUD_ARMOR_EDGE" {
		if m := edgeDisallowed.FindString(e); m != "" {
			return invalid("Invalid value for field '%s': '%s'. Attribute %s is not supported in edge security policies.", field, e, m)
		}
	}
	for _, m := range wafCallRE.FindAllStringSubmatch(e, -1) {
		if wafSet(m[2]) == nil {
			return invalid("Invalid value for field '%s': '%s'. Unknown preconfigured WAF rule set '%s'.", field, e, m[2])
		}
	}
	if _, err := armorProgram(e); err != nil {
		return invalid("Invalid value for field '%s': '%s'. Error parsing the expression: %v", field, e, err)
	}
	return nil
}

var (
	unservedArmorFunc = regexp.MustCompile(`\b(evaluateThreatIntelligence|evaluateAdaptiveProtection|evaluateAdaptiveProtectionAutoDeploy|evaluateManagedRules)\(`)
	edgeDisallowed    = regexp.MustCompile(`\b(request\.body|request\.params|origin\.user_ip|origin\.tls_ja3_fingerprint|origin\.tls_ja4_fingerprint|token\.[a-z_.]+)`)
	wafCallRE         = regexp.MustCompile(`evaluatePreconfigured(Waf|Expr)\(\s*['"]([^'"]+)['"]`)
)

// splitSubexprs splits an expression at its top-level && and ||, outside
// strings and parentheses.
func splitSubexprs(e string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(e); i++ {
		c := e[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case (c == '&' || c == '|') && i+1 < len(e) && e[i+1] == c:
			out = append(out, strings.TrimSpace(e[start:i]))
			start = i + 2
			i++
		}
	}
	out = append(out, strings.TrimSpace(e[start:]))
	// Parenthesized groups count their inner subexpressions.
	var flat []string
	for _, s := range out {
		t := strings.TrimSpace(s)
		for strings.HasPrefix(t, "!") {
			t = strings.TrimSpace(t[1:])
		}
		if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") && balanced(t[1:len(t)-1]) {
			if inner := splitSubexprs(t[1 : len(t)-1]); len(inner) > 1 {
				flat = append(flat, inner...)
				continue
			}
		}
		flat = append(flat, s)
	}
	return flat
}

func balanced(s string) bool {
	d := 0
	for _, c := range s {
		switch c {
		case '(':
			d++
		case ')':
			d--
			if d < 0 {
				return false
			}
		}
	}
	return d == 0
}

func buildRedirectOptions(ro obj, field string) (obj, error) {
	if ro == nil {
		return nil, required(field)
	}
	t, err := oneOf(field+".type", str(ro, "type"), "", "GOOGLE_RECAPTCHA", "EXTERNAL_302")
	if err != nil {
		return nil, err
	}
	if t == "" {
		return nil, required(field + ".type")
	}
	out := obj{"type": t}
	switch t {
	case "EXTERNAL_302":
		if str(ro, "target") == "" {
			return nil, required(field + ".target")
		}
		out["target"] = str(ro, "target")
	default:
		if str(ro, "target") != "" {
			return nil, invalid("Invalid value for field '%s.target': '%s'. A target is set only for EXTERNAL_302 redirects.", field, str(ro, "target"))
		}
	}
	return out, nil
}

func buildRateLimit(action string, rl obj, field string) (obj, error) {
	if rl == nil {
		return nil, required(field)
	}
	out := obj{}
	th := objOf(rl["rateLimitThreshold"])
	if th == nil {
		return nil, required(field + ".rateLimitThreshold")
	}
	maxCount := 1000000.0
	if action == "rate_based_ban" {
		maxCount = 10000
	}
	thr, err := buildThreshold(th, field+".rateLimitThreshold", maxCount)
	if err != nil {
		return nil, err
	}
	out["rateLimitThreshold"] = thr
	if c := str(rl, "conformAction"); c != "allow" {
		if c == "" {
			return nil, required(field + ".conformAction")
		}
		return nil, invalid("Invalid value for field '%s.conformAction': '%s'. Must be allow.", field, c)
	}
	out["conformAction"] = "allow"
	ex := str(rl, "exceedAction")
	switch {
	case ex == "":
		return nil, required(field + ".exceedAction")
	case ex == "redirect":
		ro, err := buildRedirectOptions(objOf(rl["exceedRedirectOptions"]), field+".exceedRedirectOptions")
		if err != nil {
			return nil, err
		}
		out["exceedRedirectOptions"] = ro
	case denyRE.MatchString(ex):
		m := denyRE.FindStringSubmatch(ex)
		if m[2] != "403" && m[2] != "404" && m[2] != "429" && m[2] != "502" {
			return nil, invalid("Invalid value for field '%s.exceedAction': '%s'. A deny action's status must be 403, 404, 429 or 502.", field, ex)
		}
	default:
		return nil, invalid("Invalid value for field '%s.exceedAction': '%s'. Must be deny(STATUS) or redirect.", field, ex)
	}
	out["exceedAction"] = ex
	keys := listOf(rl["enforceOnKeyConfigs"])
	if len(keys) > 0 && str(rl, "enforceOnKey") != "" {
		return nil, invalid("Invalid value for field '%s.enforceOnKeyConfigs': ''. Set either enforceOnKey or enforceOnKeyConfigs.", field)
	}
	if len(keys) > 3 {
		return nil, invalid("Invalid value for field '%s.enforceOnKeyConfigs': ''. At most 3 keys can be combined.", field)
	}
	if len(keys) == 0 {
		k, err := checkEnforceKey(str(rl, "enforceOnKey"), str(rl, "enforceOnKeyName"), field+".enforceOnKey")
		if err != nil {
			return nil, err
		}
		out["enforceOnKey"] = k
		if n := str(rl, "enforceOnKeyName"); n != "" {
			out["enforceOnKeyName"] = n
		}
	} else {
		seen := map[string]bool{}
		var cfgs []any
		for i, e := range keys {
			kc := objOf(e)
			f := field + ".enforceOnKeyConfigs[" + strconv.Itoa(i) + "]"
			k, err := checkEnforceKey(str(kc, "enforceOnKeyType"), str(kc, "enforceOnKeyName"), f+".enforceOnKeyType")
			if err != nil {
				return nil, err
			}
			if k != "HTTP_HEADER" && k != "HTTP_COOKIE" && seen[k] {
				return nil, invalid("Invalid value for field '%s.enforceOnKeyType': '%s'. Only HTTP_HEADER and HTTP_COOKIE keys can repeat.", f, k)
			}
			seen[k] = true
			c := obj{"enforceOnKeyType": k}
			if n := str(kc, "enforceOnKeyName"); n != "" {
				c["enforceOnKeyName"] = n
			}
			cfgs = append(cfgs, c)
		}
		out["enforceOnKeyConfigs"] = cfgs
	}
	if action == "rate_based_ban" {
		if v, ok := num(rl, "banDurationSec"); ok {
			if !banDurations[int(v)] {
				return nil, invalid("Invalid value for field '%s.banDurationSec': '%v'. Must be one of 60, 120, 180, 240, 300, 600, 900, 1200, 1800, 2700 or 3600.", field, v)
			}
			out["banDurationSec"] = v
		}
		if bt := objOf(rl["banThreshold"]); bt != nil {
			b, err := buildThreshold(bt, field+".banThreshold", 10000)
			if err != nil {
				return nil, err
			}
			out["banThreshold"] = b
			if _, ok := out["banDurationSec"]; !ok {
				return nil, required(field + ".banDurationSec")
			}
		}
	} else if rl["banThreshold"] != nil || rl["banDurationSec"] != nil {
		return nil, invalid("Invalid value for field '%s.banThreshold': ''. A ban is set only with the rate_based_ban action.", field)
	}
	return out, nil
}

func buildThreshold(th obj, field string, maxCount float64) (obj, error) {
	c, ok := num(th, "count")
	if !ok || c < 1 || c > maxCount {
		return nil, invalid("Invalid value for field '%s.count': '%v'. Must be from 1 to %d.", field, th["count"], int(maxCount))
	}
	iv, ok := num(th, "intervalSec")
	if !ok || !rateIntervals[int(iv)] {
		return nil, invalid("Invalid value for field '%s.intervalSec': '%v'. Must be one of 10, 30, 60, 120, 180, 240, 300, 600, 900, 1200, 1800, 2700 or 3600.", field, th["intervalSec"])
	}
	return obj{"count": c, "intervalSec": iv}, nil
}

func checkEnforceKey(k, name, field string) (string, error) {
	k, err := oneOf(field, k, "ALL", enforceKeys...)
	if err != nil {
		return "", err
	}
	if (k == "HTTP_HEADER" || k == "HTTP_COOKIE") && name == "" {
		return "", invalid("Invalid value for field '%s': '%s'. An enforce-on-key name is required for %s.", field, k, k)
	}
	return k, nil
}

// ruleAt is the index of a policy's rule of a priority, -1 when none.
func ruleAt(o obj, p int) int {
	for i, e := range listOf(o["rules"]) {
		if rulePriority(objOf(e)) == p {
			return i
		}
	}
	return -1
}

func queryPriority(b obj) (int, error) {
	v := str(b, "_priority")
	if v == "" {
		return 0, nil
	}
	p, err := strconv.Atoi(v)
	if err != nil || p < 0 || p > defaultRulePriority {
		return 0, invalid("Invalid value for field 'priority': '%s'. Must be an integer from 0 to 2147483647.", v)
	}
	return p, nil
}

func stripInternal(b obj) obj {
	out := obj{}
	for k, v := range b {
		if !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

func addSecurityRule(s *Server, x lbCtx, o, b obj) (obj, error) {
	r, err := buildSecurityRule(s, str(o, "type"), stripInternal(b), "resource", true)
	if err != nil {
		return nil, err
	}
	if err := s.checkAddressGroupUse(x, str(o, "type"), r, "resource"); err != nil {
		return nil, err
	}
	if ruleAt(o, rulePriority(r)) >= 0 {
		return nil, invalid("Invalid value for field 'resource.priority': '%d'. Cannot have rules with the same priorities.", rulePriority(r))
	}
	if str(b, "_validateOnly") == "true" {
		return s.done(x.project, x.scope, "addSecurityPolicyRule", x.rel, o["id"])
	}
	return s.lbSet(x, o, "addSecurityPolicyRule", func(nw obj) error {
		rules := []obj{r}
		for _, e := range listOf(nw["rules"]) {
			rules = append(rules, objOf(e))
		}
		nw["rules"] = sortedRules(rules)
		return nil
	})
}

func getSecurityRule(s *Server, x lbCtx, o obj, c call) (obj, error) {
	p, err := queryPriority(obj{"_priority": first(c.query, "priority")})
	if err != nil {
		return nil, err
	}
	i := ruleAt(o, p)
	if i < 0 {
		return nil, invalid("Invalid value for field 'priority': '%d'. The rule at that priority does not exist.", p)
	}
	return deepCopy(listOf(o["rules"])[i]).(obj), nil
}

func patchSecurityRule(s *Server, x lbCtx, o, b obj) (obj, error) {
	p, err := queryPriority(b)
	if err != nil {
		return nil, err
	}
	i := ruleAt(o, p)
	if i < 0 {
		return nil, invalid("Invalid value for field 'priority': '%d'. The rule at that priority does not exist.", p)
	}
	merged := deepCopy(listOf(o["rules"])[i]).(obj)
	mergePatch(merged, stripInternal(b))
	if _, ok := stripInternal(b)["priority"]; !ok {
		merged["priority"] = float64(p)
	}
	r, err := buildSecurityRule(s, str(o, "type"), merged, "resource", false)
	if err != nil {
		return nil, err
	}
	if err := s.checkAddressGroupUse(x, str(o, "type"), r, "resource"); err != nil {
		return nil, err
	}
	if p == defaultRulePriority && rulePriority(r) != p {
		return nil, invalid("Invalid value for field 'resource.priority': '%d'. The default rule's priority cannot be changed.", rulePriority(r))
	}
	if j := ruleAt(o, rulePriority(r)); j >= 0 && j != i {
		return nil, invalid("Invalid value for field 'resource.priority': '%d'. Cannot have rules with the same priorities.", rulePriority(r))
	}
	if str(b, "_validateOnly") == "true" {
		return s.done(x.project, x.scope, "patchSecurityPolicyRule", x.rel, o["id"])
	}
	return s.lbSet(x, o, "patchSecurityPolicyRule", func(nw obj) error {
		var rules []obj
		for k, e := range listOf(nw["rules"]) {
			if k != i {
				rules = append(rules, objOf(e))
			}
		}
		nw["rules"] = sortedRules(append(rules, r))
		return nil
	})
}

func removeSecurityRule(s *Server, x lbCtx, o, b obj) (obj, error) {
	p, err := queryPriority(b)
	if err != nil {
		return nil, err
	}
	if p == defaultRulePriority {
		return nil, invalid("Invalid value for field 'priority': '%d'. The default rule cannot be removed.", p)
	}
	i := ruleAt(o, p)
	if i < 0 {
		return nil, invalid("Invalid value for field 'priority': '%d'. The rule at that priority does not exist.", p)
	}
	return s.lbSet(x, o, "removeSecurityPolicyRule", func(nw obj) error {
		l := listOf(nw["rules"])
		nw["rules"] = append(append([]any{}, l[:i]...), l[i+1:]...)
		return nil
	})
}

func setSecurityPolicyLabels(s *Server, x lbCtx, o, b obj) (obj, error) {
	if fp := str(b, "labelFingerprint"); fp != str(o, "labelFingerprint") {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed")
	}
	return s.lbSet(x, o, "setLabels", func(nw obj) error {
		if l := objOf(b["labels"]); len(l) > 0 {
			nw["labels"] = l
		} else {
			delete(nw, "labels")
		}
		nw["labelFingerprint"] = labelFingerprint(nw["labels"])
		return nil
	})
}

// buildNetworkEdgeSecurityService is a region's network edge security
// service: at most one per region, naming a CLOUD_ARMOR_NETWORK policy of
// its region, which then filters every external passthrough Network Load
// Balancer of the region.
func buildNetworkEdgeSecurityService(s *Server, x lbCtx, b, old obj) (obj, error) {
	if old == nil {
		prefix := "projects/" + x.project + "/" + x.scope + "/networkEdgeSecurityServices/"
		for k := range s.res {
			if strings.HasPrefix(k, prefix) {
				return nil, errf(http.StatusForbidden, "quotaExceeded", "Quota 'NETWORK_EDGE_SECURITY_SERVICES' exceeded. Limit: 1.0 in region %s.", x.region)
			}
		}
	}
	o := obj{}
	if sp := str(b, "securityPolicy"); sp != "" {
		rel, p, err := s.lbRef(x, "resource.securityPolicy", sp, "securityPolicies")
		if err != nil {
			return nil, err
		}
		if str(p, "type") != "CLOUD_ARMOR_NETWORK" || scopeOf(rel) != x.scope {
			return nil, invalid("Invalid value for field 'resource.securityPolicy': '%s'. A network edge security service takes a CLOUD_ARMOR_NETWORK policy of its region.", sp)
		}
		o["securityPolicy"] = rel
	}
	return o, nil
}

// policyRef resolves a policy to attach: of the given type, in the
// target's scope (a global backend service's policies are global).
func (s *Server) policyRef(x lbCtx, field, ref string, types ...string) (string, error) {
	rel, p, err := s.lbRef(x, field, ref, "securityPolicies")
	if err != nil {
		return "", err
	}
	if !hasStr(types, str(p, "type")) {
		return "", invalid("Invalid value for field '%s': '%s'. The security policy's type %s cannot be attached here; it must be %s.", field, ref,
			str(p, "type"), strings.Join(types, " or "))
	}
	if scopeOf(rel) != regionScope(x) {
		return "", invalid("Invalid value for field '%s': '%s'. The security policy must be in the target's scope (%s).", field, ref, regionScope(x))
	}
	return rel, nil
}

// regionScope is x's scope, a zone's being its region's.
func regionScope(x lbCtx) string {
	if x.zone != "" {
		return "regions/" + x.region
	}
	return x.scope
}

// setSecurityPolicyOn is setSecurityPolicy (field securityPolicy) or
// setEdgeSecurityPolicy (edgeSecurityPolicy): an empty policy detaches.
func setSecurityPolicyOn(field string, types ...string) func(*Server, lbCtx, obj, obj) (obj, error) {
	return func(s *Server, x lbCtx, o, b obj) (obj, error) {
		ref := str(b, "securityPolicy")
		var rel string
		if ref != "" {
			var err error
			if rel, err = s.policyRef(x, "securityPolicy", ref, types...); err != nil {
				return nil, err
			}
			if err := securityPolicyFits(o, field); err != nil {
				return nil, err
			}
		}
		op := "setSecurityPolicy"
		if field == "edgeSecurityPolicy" {
			op = "setEdgeSecurityPolicy"
		}
		return s.lbSet(x, o, op, func(nw obj) error {
			if rel == "" {
				delete(nw, field)
			} else {
				nw[field] = rel
			}
			return nil
		})
	}
}

// securityPolicyFits refuses a policy on a backend service that Cloud
// Armor does not filter: an internal passthrough one, or Traffic
// Director's.
func securityPolicyFits(o obj, field string) error {
	if str(o, "kind") != "compute#backendService" {
		return nil
	}
	switch str(o, "loadBalancingScheme") {
	case "INTERNAL", "INTERNAL_SELF_MANAGED":
		return invalid("Invalid value for field '%s': ''. Google Cloud Armor security policies are not supported for backend services with the %s load balancing scheme.", field, str(o, "loadBalancingScheme"))
	}
	return nil
}

// effectiveSecurityPolicies is backendServices.getEffectiveSecurityPolicies:
// the policies that apply to the service, its backend and edge ones (no
// hierarchical policies are served).
func effectiveSecurityPolicies(s *Server, x lbCtx, o obj, _ call) (obj, error) {
	var out []any
	for _, f := range []string{"securityPolicy", "edgeSecurityPolicy"} {
		if p := s.res[str(o, f)]; p != nil {
			e := deepCopy(p).(obj)
			e["shortName"] = str(p, "name")
			out = append(out, e)
		}
	}
	return obj{"securityPolicies": out}, nil
}

// buildPolicyFields checks a backend service's or bucket's securityPolicy
// and edgeSecurityPolicy on insert, patch and update, and copies them.
func (s *Server) buildPolicyFields(x lbCtx, b, o obj, backend bool) error {
	if backend {
		if ref := str(b, "securityPolicy"); ref != "" {
			rel, err := s.policyRef(x, "resource.securityPolicy", ref, "CLOUD_ARMOR")
			if err != nil {
				return err
			}
			if err := securityPolicyFits(b, "resource.securityPolicy"); err != nil {
				return err
			}
			o["securityPolicy"] = rel
		}
	}
	if ref := str(b, "edgeSecurityPolicy"); ref != "" {
		rel, err := s.policyRef(x, "resource.edgeSecurityPolicy", ref, "CLOUD_ARMOR_EDGE")
		if err != nil {
			return err
		}
		o["edgeSecurityPolicy"] = rel
	}
	return nil
}
