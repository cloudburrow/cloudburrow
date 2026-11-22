package compute

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// Cloud Armor's enforcement (#2160): each request through a target HTTP or
// HTTPS proxy is checked against its backend's edge security policy (before
// Cloud CDN), then its backend security policy (on a cache miss), by the
// rules language of
// https://docs.cloud.google.com/armor/docs/rules-language-reference (read
// 2026-11-22), compiled with cel-go: the origin and request attributes,
// the string methods (lower, upper, base64Decode, urlDecode,
// urlDecodeUni, utf8ToUnicode), inIpRange, has() on a map key, and the
// preconfigured WAF rules (lbwaf.go). A local client has no region or
// autonomous system: origin.region_code is "" and origin.asn 0; no
// reCAPTCHA token can be valid, so token.* attributes are false.

// armorEnv is the rules language's environment.
var armorEnv = func() *cel.Env {
	str1 := func(name string, f func(string) string) cel.EnvOption {
		return cel.Function(name, cel.MemberOverload("armor_"+name+"_string", []*cel.Type{cel.StringType}, cel.StringType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				s, ok := v.(types.String)
				if !ok {
					return types.NewErr("%s: not a string", name)
				}
				return types.String(f(string(s)))
			})))
	}
	env, err := cel.NewEnv(
		cel.Variable("origin", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("token", cel.MapType(cel.StringType, cel.DynType)),
		str1("lower", strings.ToLower),
		str1("upper", strings.ToUpper),
		str1("base64Decode", armorBase64Decode),
		str1("urlDecode", armorURLDecode),
		str1("urlDecodeUni", armorURLDecodeUni),
		str1("utf8ToUnicode", armorUTF8ToUnicode),
		cel.Function("inIpRange", cel.Overload("armor_inIpRange", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
			cel.BinaryBinding(func(a, b ref.Val) ref.Val {
				ip, _ := a.(types.String)
				cidr, _ := b.(types.String)
				return types.Bool(inIPRange(string(ip), string(cidr)))
			}))),
		cel.Function("evaluatePreconfiguredWaf",
			cel.Overload("armor_waf_string", []*cel.Type{cel.StringType}, cel.BoolType, cel.LateFunctionBinding()),
			cel.Overload("armor_waf_string_map", []*cel.Type{cel.StringType, cel.MapType(cel.StringType, cel.DynType)}, cel.BoolType, cel.LateFunctionBinding()),
			cel.Overload("armor_waf_string_list", []*cel.Type{cel.StringType, cel.ListType(cel.StringType)}, cel.BoolType, cel.LateFunctionBinding())),
		cel.Function("evaluatePreconfiguredExpr",
			cel.Overload("armor_expr_string", []*cel.Type{cel.StringType}, cel.BoolType, cel.LateFunctionBinding()),
			cel.Overload("armor_expr_string_list", []*cel.Type{cel.StringType, cel.ListType(cel.StringType)}, cel.BoolType, cel.LateFunctionBinding())),
	)
	if err != nil {
		panic(err)
	}
	return env
}()

// hasIndexRE is has(m['k']), which the rules language allows and CEL's
// has() macro does not: it is evaluated as 'k' in m.
var hasIndexRE = regexp.MustCompile(`has\(\s*([A-Za-z_][\w.]*)\s*\[\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")\s*\]\s*\)`)

var (
	armorProgMu sync.Mutex
	armorProgs  = map[string]cel.Program{}
)

// armorProgram compiles an expression once.
func armorProgram(e string) (cel.Program, error) {
	armorProgMu.Lock()
	defer armorProgMu.Unlock()
	if p, ok := armorProgs[e]; ok {
		return p, nil
	}
	src := hasIndexRE.ReplaceAllString(e, "($2 in $1)")
	ast, iss := armorEnv.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("the expression must be a boolean, not %s", ast.OutputType())
	}
	p, err := armorEnv.Program(ast)
	if err != nil {
		return nil, err
	}
	if len(armorProgs) > 4096 {
		armorProgs = map[string]cel.Program{}
	}
	armorProgs[e] = p
	return p, nil
}

func inIPRange(ip, cidr string) bool {
	a := net.ParseIP(strings.TrimSpace(ip))
	if a == nil {
		return false
	}
	if cidr == "*" {
		return true
	}
	if !strings.Contains(cidr, "/") {
		b := net.ParseIP(cidr)
		return b != nil && a.Equal(b)
	}
	_, n, err := net.ParseCIDR(cidr)
	return err == nil && n.Contains(a)
}

func armorBase64Decode(s string) string {
	s = strings.NewReplacer("_", "/", "-", "+").Replace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b)
		}
	}
	return ""
}

func armorURLDecode(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}

var percentU = regexp.MustCompile(`%[uU]([0-9a-fA-F]{4})`)

func armorURLDecodeUni(s string) string {
	s = percentU.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.ParseUint(m[2:], 16, 32)
		return string(rune(n))
	})
	return armorURLDecode(s)
}

func armorUTF8ToUnicode(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "%%u%04x", r)
	}
	return strings.ToLower(b.String())
}

// armorReq is what a policy reads from one request.
type armorReq struct {
	r        *http.Request
	clientIP string
	userIP   string
	scheme   string
	body     []byte
	params   map[string]string
	vars     map[string]any
}

// newArmorReq reads r's attributes; the body (up to the policy's
// inspection size, 8 KB by default) is read only when a rule needs it,
// and put back.
func newArmorReq(r *http.Request, scheme string) *armorReq {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return &armorReq{r: r, clientIP: ip, userIP: ip, scheme: scheme}
}

func (a *armorReq) readBody(limit int) {
	if a.body != nil || a.r.Body == nil || a.r.Body == http.NoBody {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(a.r.Body, int64(limit)))
	a.body = b
	a.r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), a.r.Body))
}

// activation is the request's attributes as the rules language names them.
func (a *armorReq) activation(policy obj, needBody bool) map[string]any {
	if a.vars != nil && (!needBody || a.body != nil) {
		return a.vars
	}
	adv := objOf(policy["advancedOptionsConfig"])
	for _, h := range strs(adv, "userIpRequestHeaders") {
		if v := strings.TrimSpace(strings.Split(a.r.Header.Get(h), ",")[0]); net.ParseIP(v) != nil {
			a.userIP = v
			break
		}
	}
	headers := map[string]string{}
	for k, v := range a.r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	if a.r.Host != "" {
		headers["host"] = a.r.Host
	}
	params := map[string]string{}
	for k, v := range a.r.URL.Query() {
		params[k] = strings.Join(v, ",")
	}
	if needBody {
		limit := 8 << 10
		if n, ok := bodyInspectionSizes[str(adv, "requestBodyInspectionSize")]; ok {
			limit = n
		}
		a.readBody(limit)
		ct := a.r.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
			if q, err := url.ParseQuery(string(a.body)); err == nil {
				for k, v := range q {
					params[k] = strings.Join(v, ",")
				}
			}
		case strings.HasPrefix(ct, "application/json") && str(adv, "jsonParsing") != "DISABLED" && str(adv, "jsonParsing") != "":
			var v any
			if json.Unmarshal(a.body, &v) == nil {
				flattenJSON("", v, params)
			}
		}
	}
	a.params = params
	body := ""
	if a.body != nil {
		body = string(a.body)
	}
	a.vars = map[string]any{
		"origin": map[string]any{"ip": a.clientIP, "user_ip": a.userIP, "region_code": "", "asn": int64(0),
			"tls_ja3_fingerprint": "", "tls_ja4_fingerprint": ""},
		"request": map[string]any{"headers": headers, "method": a.r.Method, "path": a.r.URL.EscapedPath(), "scheme": a.scheme,
			"query": a.r.URL.RawQuery, "body": body, "params": params},
		"token": map[string]any{
			"recaptcha_exemption": map[string]any{"valid": false},
			"recaptcha_action":    map[string]any{"valid": false, "score": 0.0, "captcha_status": "NONE", "action": ""},
			"recaptcha_session":   map[string]any{"valid": false, "score": 0.0},
		},
	}
	return a.vars
}

func flattenJSON(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenJSON(p, e, out)
		}
	case []any:
		for _, e := range t {
			flattenJSON(prefix, e, out)
		}
	default:
		if prefix != "" {
			out[prefix] = fmt.Sprint(t)
		}
	}
}

// ruleMatches evaluates one rule's match against a request.
func (a *armorReq) ruleMatches(policy, rule obj) bool {
	m := objOf(rule["match"])
	if m == nil {
		return false
	}
	if str(m, "versionedExpr") == "SRC_IPS_V1" {
		for _, c := range strs(objOf(m["config"]), "srcIpRanges") {
			if inIPRange(a.clientIP, c) {
				return true
			}
		}
		return false
	}
	e := str(objOf(m["expr"]), "expression")
	p, err := armorProgram(e)
	if err != nil {
		return false
	}
	needBody := strings.Contains(e, "request.body") || strings.Contains(e, "request.params") || strings.Contains(e, "evaluatePreconfigured")
	vars := a.activation(policy, needBody)
	waf := func(_ string, args ...ref.Val) ref.Val {
		name, _ := args[0].(types.String)
		opts := wafOptions{sensitivity: 4}
		if len(args) > 1 {
			switch t := args[1].(type) {
			case traits.Mapper:
				opts = wafOptionsOf(t)
			case traits.Lister:
				opts.optOut = celStrings(t)
			}
		}
		if pw := objOf(rule["preconfiguredWafConfig"]); pw != nil {
			opts.exclusions = listOf(pw["exclusions"])
		}
		return types.Bool(a.wafMatch(string(name), opts))
	}
	act, err := cel.FunctionVars(vars, map[string]cel.LateBoundFunction{"evaluatePreconfiguredWaf": waf, "evaluatePreconfiguredExpr": waf})
	if err != nil {
		return false
	}
	out, _, err := p.Eval(act)
	if err != nil {
		return false
	}
	b, ok := out.(types.Bool)
	return ok && bool(b)
}

func celStrings(l traits.Lister) []string {
	var out []string
	it := l.Iterator()
	for it.HasNext() == types.True {
		if s, ok := it.Next().(types.String); ok {
			out = append(out, string(s))
		}
	}
	return out
}

func wafOptionsOf(m traits.Mapper) wafOptions {
	o := wafOptions{sensitivity: 4}
	if v, ok := m.Find(types.String("sensitivity")); ok {
		if n, ok := v.(types.Int); ok {
			o.sensitivity = int(n)
		}
	}
	if v, ok := m.Find(types.String("opt_out_rule_ids")); ok {
		if l, ok := v.(traits.Lister); ok {
			o.optOut = celStrings(l)
		}
	}
	if v, ok := m.Find(types.String("opt_in_rule_ids")); ok {
		if l, ok := v.(traits.Lister); ok {
			o.optIn = celStrings(l)
		}
	}
	return o
}

// armorVerdict is a policy's decision on a request.
type armorVerdict struct {
	// status is the response to send (0: let the request through).
	status   int
	location string
	// headers are the matched allow rule's headers to add to the request.
	headers []obj
	rule    int
}

// evaluate applies a policy's rules, in priority order, to a request.
func (s *Server) armorEvaluate(policyRel string, policy obj, a *armorReq, sni string) armorVerdict {
	for _, e := range listOf(policy["rules"]) {
		rule := objOf(e)
		if !a.ruleMatches(policy, rule) {
			continue
		}
		v := s.armorAction(policyRel, policy, rule, a, sni)
		if rule["preview"] == true {
			continue
		}
		return v
	}
	return armorVerdict{}
}

func (s *Server) armorAction(policyRel string, policy, rule obj, a *armorReq, sni string) armorVerdict {
	action := str(rule, "action")
	v := armorVerdict{rule: rulePriority(rule)}
	if ha := objOf(rule["headerAction"]); ha != nil {
		for _, e := range listOf(ha["requestHeadersToAdds"]) {
			h := objOf(e)
			v.headers = append(v.headers, obj{"requestHeadersToAdd": []any{obj{"headerName": str(h, "headerName"),
				"headerValue": str(h, "headerValue"), "replace": true}}})
		}
	}
	switch {
	case action == "allow":
	case strings.HasPrefix(action, "deny"):
		v.status = denyStatus(action)
	case action == "redirect":
		v.status, v.location = redirectOf(objOf(rule["redirectOptions"]))
	case action == "throttle" || action == "rate_based_ban":
		rl := objOf(rule["rateLimitOptions"])
		key := policyRel + "|" + strconv.Itoa(rulePriority(rule)) + "|" + a.rateKey(rl, sni)
		if s.lb.rates.exceeded(key, action, rl, s.opts.Now()) {
			ex := str(rl, "exceedAction")
			if ex == "redirect" {
				v.status, v.location = redirectOf(objOf(rl["exceedRedirectOptions"]))
			} else {
				v.status = denyStatus(ex)
			}
		}
	}
	return v
}

func denyStatus(action string) int {
	if m := denyRE.FindStringSubmatch(action); m != nil && m[2] != "" {
		n, _ := strconv.Atoi(m[2])
		return n
	}
	return http.StatusForbidden
}

// redirectOf is a redirect action's response: EXTERNAL_302 to its
// target; GOOGLE_RECAPTCHA would send the client to reCAPTCHA's
// challenge page, which no local proxy can serve or verify, so it is
// refused (403) as a client failing the challenge is (docs/gaps.md).
func redirectOf(ro obj) (int, string) {
	if str(ro, "type") == "EXTERNAL_302" {
		return http.StatusFound, str(ro, "target")
	}
	return http.StatusForbidden, ""
}

// rateKey is a rate limit's key for a request (rate-limiting-overview,
// "Enforce on key"), values truncated to 128 bytes.
func (a *armorReq) rateKey(rl obj, sni string) string {
	type kc struct{ typ, name string }
	var keys []kc
	if cfgs := listOf(rl["enforceOnKeyConfigs"]); len(cfgs) > 0 {
		for _, e := range cfgs {
			c := objOf(e)
			keys = append(keys, kc{str(c, "enforceOnKeyType"), str(c, "enforceOnKeyName")})
		}
	} else {
		keys = []kc{{str(rl, "enforceOnKey"), str(rl, "enforceOnKeyName")}}
	}
	var parts []string
	for _, k := range keys {
		v := ""
		switch k.typ {
		case "IP":
			v = a.clientIP
		case "XFF_IP":
			v = a.clientIP
			if x := strings.TrimSpace(strings.Split(a.r.Header.Get("X-Forwarded-For"), ",")[0]); net.ParseIP(x) != nil {
				v = x
			}
		case "USER_IP":
			v = a.userIP
		case "HTTP_HEADER":
			v = a.r.Header.Get(k.name)
		case "HTTP_COOKIE":
			if c, err := a.r.Cookie(k.name); err == nil {
				v = c.Value
			}
		case "HTTP_PATH":
			v = a.r.URL.Path
		case "SNI":
			v = sni
		case "REGION_CODE":
			v = ""
		}
		if len(v) > 128 {
			v = v[:128]
		}
		parts = append(parts, k.typ+"="+v)
	}
	return strings.Join(parts, "&")
}

// lbRates are the rate limits' counters: each key's requests in the
// current interval, and its ban.
type lbRates struct {
	mu sync.Mutex
	m  map[string]*rateState
}

type rateState struct {
	start, banStart time.Time
	count, banCount int
	bannedUntil     time.Time
}

// exceeded counts a request against its key, and is whether the exceed
// action applies: over the threshold in this interval, or banned.
func (r *lbRates) exceeded(key, action string, rl obj, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*rateState{}
	}
	st := r.m[key]
	if st == nil {
		st = &rateState{start: now, banStart: now}
		r.m[key] = st
	}
	th := objOf(rl["rateLimitThreshold"])
	iv := time.Duration(numOr(th, "intervalSec", 60)) * time.Second
	if now.Sub(st.start) >= iv {
		st.start, st.count = now, 0
	}
	st.count++
	if action == "rate_based_ban" {
		if now.Before(st.bannedUntil) {
			return true
		}
		ban := time.Duration(numOr(rl, "banDurationSec", 0)) * time.Second
		if bt := objOf(rl["banThreshold"]); bt != nil {
			biv := time.Duration(numOr(bt, "intervalSec", 60)) * time.Second
			if now.Sub(st.banStart) >= biv {
				st.banStart, st.banCount = now, 0
			}
			st.banCount++
			if st.banCount > int(numOr(bt, "count", 0)) {
				st.bannedUntil = now.Add(ban)
				return true
			}
		} else if st.count > int(numOr(th, "count", 0)) && ban > 0 {
			st.bannedUntil = now.Add(ban)
			return true
		}
	}
	return st.count > int(numOr(th, "count", 0))
}

// armorDenyPage is the body Google's front end answers a denied request
// with.
func armorDeny(w http.ResponseWriter, code int, location string) {
	if location != "" {
		w.Header().Set("Location", location)
		w.Header().Set("Via", "1.1 google")
		w.WriteHeader(code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Via", "1.1 google")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name=viewport content="width=device-width, initial-scale=1"><title>%d</title>%d %s`,
		code, code, http.StatusText(code))
}

// armorCheck applies the policy rel (if any) to a request; ok is false
// when the request was answered.
func (s *Server) armorCheck(w http.ResponseWriter, a *armorReq, rel string) ([]obj, bool) {
	if rel == "" {
		return nil, true
	}
	s.mu.Lock()
	p := s.res[rel]
	var policy obj
	if p != nil {
		policy = deepCopy(p).(obj)
	}
	s.mu.Unlock()
	if policy == nil {
		return nil, true
	}
	v := s.armorEvaluate(rel, policy, a, sniOf(a.r))
	if v.status != 0 {
		armorDeny(w, v.status, v.location)
		return nil, false
	}
	return v.headers, true
}
