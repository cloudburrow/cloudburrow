package compute

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// URL maps (#2153), as https://docs.cloud.google.com/load-balancing/docs/url-map-concepts,
// .../l7-internal/traffic-management and the urlMaps REST reference
// describe them (read 2026-11-22): host rules choose a path matcher, whose
// route rules (by priority) or path rules (the longest path wins) choose a
// backend service or bucket, a weighted set of backend services, or a
// redirect, with the route action's rewrites, retries, fault injection,
// mirroring and CORS, and the header actions of every level. The data
// plane (lbplane.go) routes each request by lbMatch; urlMaps.validate and
// a map's tests run the same match.

func init() {
	registerLB(&lbColl{coll: "urlMaps", kind: "compute#urlMap", global: true, regional: true, patch: true, update: true,
		fingerprint: true, testIam: true, build: buildURLMap, decorate: nil, noun: "urlMap",
		post: map[string]func(*Server, lbCtx, obj, obj) (obj, error){"validate": validateURLMap, "invalidateCache": invalidateCacheOn}})
}

var redirectCodes = map[string]int{"": 301, "MOVED_PERMANENTLY_DEFAULT": 301, "FOUND": 302, "SEE_OTHER": 303,
	"TEMPORARY_REDIRECT": 307, "PERMANENT_REDIRECT": 308}

// urlMapErr is a URL map's first problem, as a field error.
func buildURLMap(s *Server, x lbCtx, b, old obj) (obj, error) {
	o := obj{}
	copyFields(o, b, "defaultService", "defaultUrlRedirect", "defaultRouteAction", "headerAction", "hostRules", "pathMatchers",
		"tests", "defaultCustomErrorResponsePolicy")
	o = norm(o)
	schemes := map[string]bool{}
	if err := s.checkURLMapLocked(x, o, schemes); err != nil {
		return nil, err
	}
	if _, failures := s.runURLMapTestsLocked(x, o); len(failures) > 0 {
		f := failures[0]
		return nil, invalid("Invalid value for field 'resource.tests[%v]': ''. Test failure: %s", f["_index"], f["_why"])
	}
	return o, nil
}

// checkURLMapLocked checks every link and rule of a URL map, normalizing
// its service links to relative names.
func (s *Server) checkURLMapLocked(x lbCtx, m obj, schemes map[string]bool) error {
	svcColls := []string{"backendServices", "backendBuckets"}
	if x.region != "" {
		svcColls = []string{"backendServices"}
	}
	checkSvc := func(field string, holder obj, key string) error {
		ref := str(holder, key)
		if ref == "" {
			return nil
		}
		rel, o, err := s.lbRef(x, field, ref, svcColls...)
		if err != nil {
			return err
		}
		if scopeOf(rel) != x.scope {
			return invalid("Invalid value for field '%s': '%s'. The service must be in the URL map's scope (%s).", field, ref, x.scope)
		}
		holder[key] = rel
		sc := str(o, "loadBalancingScheme")
		if str(o, "kind") == "compute#backendBucket" {
			sc = ""
		}
		if sc != "" {
			schemes[sc] = true
			if len(schemes) > 1 {
				return invalid("Invalid value for field '%s': '%s'. Every backend service of a URL map must have the same load balancing scheme.", field, ref)
			}
			if sc == "INTERNAL" {
				return invalid("Invalid value for field '%s': '%s'. A backend service of scheme INTERNAL serves a passthrough load balancer, not a URL map.", field, ref)
			}
			if p := str(o, "protocol"); !isHTTPProtocol(p) {
				return invalid("Invalid value for field '%s': '%s'. A URL map's backend services must use HTTP, HTTPS, HTTP2, H2C or gRPC; this one uses %s.", field, ref, p)
			}
		}
		return nil
	}
	checkAction := func(field string, ra obj) error {
		if ra == nil {
			return nil
		}
		for i, e := range listOf(ra["weightedBackendServices"]) {
			w := objOf(e)
			f := field + ".weightedBackendServices[" + strconv.Itoa(i) + "]"
			if err := checkSvc(f+".backendService", w, "backendService"); err != nil {
				return err
			}
			if v := numOr(w, "weight", 0); v < 0 || v > 1000 {
				return invalid("Invalid value for field '%s.weight': '%v'. Must be between 0 and 1000.", f, v)
			}
		}
		if mp := objOf(ra["requestMirrorPolicy"]); mp != nil {
			if err := checkSvc(field+".requestMirrorPolicy.backendService", mp, "backendService"); err != nil {
				return err
			}
		}
		if ur := objOf(ra["urlRewrite"]); ur != nil {
			if p := str(ur, "pathPrefixRewrite"); p != "" && p[0] != '/' {
				return invalid("Invalid value for field '%s.urlRewrite.pathPrefixRewrite': '%s'. Must start with '/'.", field, p)
			}
		}
		return nil
	}
	checkRedirect := func(field string, r obj) error {
		if r == nil {
			return nil
		}
		if _, ok := redirectCodes[str(r, "redirectResponseCode")]; !ok {
			return invalid("Invalid value for field '%s.redirectResponseCode': '%s'. Must be one of [FOUND, MOVED_PERMANENTLY_DEFAULT, PERMANENT_REDIRECT, SEE_OTHER, TEMPORARY_REDIRECT]", field, str(r, "redirectResponseCode"))
		}
		if str(r, "pathRedirect") != "" && str(r, "prefixRedirect") != "" {
			return invalid("Invalid value for field '%s': ''. Only one of pathRedirect and prefixRedirect can be set.", field)
		}
		return nil
	}
	// exactlyOne checks a level's service, redirect and route action.
	exactlyOne := func(field string, holder obj, svcKey, redirKey, actionKey string, need bool) error {
		svc, redir, ra := str(holder, svcKey) != "", objOf(holder[redirKey]) != nil, objOf(holder[actionKey])
		weighted := ra != nil && len(listOf(ra["weightedBackendServices"])) > 0
		n := 0
		for _, v := range []bool{svc, redir, weighted} {
			if v {
				n++
			}
		}
		if n > 1 {
			return invalid("Invalid value for field '%s': ''. Only one of %s, %s or %s.weightedBackendServices can be set.", field, svcKey, redirKey, actionKey)
		}
		if redir && ra != nil {
			return invalid("Invalid value for field '%s.%s': ''. A redirect cannot have a route action.", field, actionKey)
		}
		if need && n == 0 {
			return invalid("Invalid value for field '%s': ''. One of %s, %s or %s.weightedBackendServices must be set.", field, svcKey, redirKey, actionKey)
		}
		if err := checkSvc(field+"."+svcKey, holder, svcKey); err != nil {
			return err
		}
		if err := checkRedirect(field+"."+redirKey, objOf(holder[redirKey])); err != nil {
			return err
		}
		return checkAction(field+"."+actionKey, ra)
	}
	if err := exactlyOne("resource", m, "defaultService", "defaultUrlRedirect", "defaultRouteAction", true); err != nil {
		return err
	}
	names := map[string]bool{}
	for i, e := range listOf(m["pathMatchers"]) {
		pm := objOf(e)
		field := "resource.pathMatchers[" + strconv.Itoa(i) + "]"
		name := str(pm, "name")
		if err := checkName(field+".name", name); err != nil {
			return err
		}
		if names[name] {
			return invalid("Invalid value for field '%s.name': '%s'. Duplicate path matcher name.", field, name)
		}
		names[name] = true
		if err := exactlyOne(field, pm, "defaultService", "defaultUrlRedirect", "defaultRouteAction", false); err != nil {
			return err
		}
		prs, rrs := listOf(pm["pathRules"]), listOf(pm["routeRules"])
		if len(prs) > 0 && len(rrs) > 0 {
			return invalid("Invalid value for field '%s': ''. A path matcher can have path rules or route rules, not both.", field)
		}
		paths := map[string]bool{}
		for j, pe := range prs {
			pr := objOf(pe)
			f := field + ".pathRules[" + strconv.Itoa(j) + "]"
			ps := strs(pr, "paths")
			if len(ps) == 0 {
				return invalid("Invalid value for field '%s.paths': ''. A path rule needs at least one path.", f)
			}
			for _, p := range ps {
				if !validPathPattern(p) {
					return invalid("Invalid value for field '%s.paths': '%s'. A path must start with '/', and '*' is allowed only at the end, after '/'.", f, p)
				}
				if paths[p] {
					return invalid("Invalid value for field '%s.paths': '%s'. Duplicate path in the path matcher.", f, p)
				}
				paths[p] = true
			}
			if err := exactlyOne(f, pr, "service", "urlRedirect", "routeAction", true); err != nil {
				return err
			}
		}
		prios := map[float64]bool{}
		for j, re := range rrs {
			rr := objOf(re)
			f := field + ".routeRules[" + strconv.Itoa(j) + "]"
			p, ok := num(rr, "priority")
			if !ok {
				return required(f + ".priority")
			}
			if p < 0 || p > 2147483647 {
				return invalid("Invalid value for field '%s.priority': '%v'. Must be between 0 and 2147483647.", f, p)
			}
			if prios[p] {
				return invalid("Invalid value for field '%s.priority': '%v'. Route rule priorities must be unique within a path matcher.", f, p)
			}
			prios[p] = true
			for k, me := range listOf(rr["matchRules"]) {
				if err := checkMatchRule(f+".matchRules["+strconv.Itoa(k)+"]", objOf(me)); err != nil {
					return err
				}
			}
			if err := exactlyOne(f, rr, "service", "urlRedirect", "routeAction", true); err != nil {
				return err
			}
		}
	}
	hosts := map[string]bool{}
	for i, e := range listOf(m["hostRules"]) {
		hr := objOf(e)
		field := "resource.hostRules[" + strconv.Itoa(i) + "]"
		if !names[str(hr, "pathMatcher")] {
			return invalid("Invalid value for field '%s.pathMatcher': '%s'. No path matcher of that name.", field, str(hr, "pathMatcher"))
		}
		hs := strs(hr, "hosts")
		if len(hs) == 0 {
			return invalid("Invalid value for field '%s.hosts': ''. A host rule needs at least one host.", field)
		}
		for _, h := range hs {
			if !validHostPattern(h) {
				return invalid("Invalid value for field '%s.hosts': '%s'. '*' is allowed only first, followed by '-' or '.'.", field, h)
			}
			if hosts[strings.ToLower(h)] {
				return invalid("Invalid value for field '%s.hosts': '%s'. Duplicate host pattern.", field, h)
			}
			hosts[strings.ToLower(h)] = true
		}
	}
	// The classic Application Load Balancer's URL maps have none of the
	// advanced traffic management (load-balancing/docs/features, "Routing
	// and traffic management").
	if schemes["EXTERNAL"] && x.region == "" {
		if f := advancedFeature(m); f != "" {
			return invalid("Invalid value for field 'resource.%s': ''. %s is not supported by the classic Application Load Balancer (load balancing scheme EXTERNAL).", f, f)
		}
	}
	for i, e := range listOf(m["tests"]) {
		t := objOf(e)
		if str(t, "path") == "" {
			return required("resource.tests[" + strconv.Itoa(i) + "].path")
		}
		if sv := str(t, "service"); sv != "" {
			rel, _, err := s.lbRef(x, "resource.tests["+strconv.Itoa(i)+"].service", sv, svcColls...)
			if err != nil {
				return err
			}
			t["service"] = rel
		}
	}
	return nil
}

// advancedFeature names an advanced traffic management feature m uses,
// "" when none.
func advancedFeature(m obj) string {
	var walk func(v any, path string) string
	walk = func(v any, path string) string {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range []string{"headerMatches", "queryParameterMatches", "regexMatch", "pathTemplateMatch", "retryPolicy",
				"faultInjectionPolicy", "requestMirrorPolicy", "corsPolicy", "timeout"} {
				if t[k] != nil {
					return path + k
				}
			}
			if l := listOf(t["weightedBackendServices"]); len(l) > 1 {
				return path + "weightedBackendServices"
			}
			for k, e := range t {
				if k == "tests" {
					continue
				}
				if f := walk(e, path+k+"."); f != "" {
					return f
				}
			}
		case []any:
			for _, e := range t {
				if f := walk(e, path); f != "" {
					return f
				}
			}
		}
		return ""
	}
	return strings.TrimSuffix(walk(m, ""), ".")
}

func checkMatchRule(field string, mr obj) error {
	n := 0
	for _, k := range []string{"prefixMatch", "fullPathMatch", "regexMatch", "pathTemplateMatch"} {
		if str(mr, k) != "" {
			n++
		}
	}
	if n > 1 {
		return invalid("Invalid value for field '%s': ''. Only one of prefixMatch, fullPathMatch, regexMatch or pathTemplateMatch can be set.", field)
	}
	if n == 0 {
		return invalid("Invalid value for field '%s': ''. One of prefixMatch, fullPathMatch, regexMatch or pathTemplateMatch must be set.", field)
	}
	for _, k := range []string{"prefixMatch", "fullPathMatch"} {
		if v := str(mr, k); v != "" && v[0] != '/' {
			return invalid("Invalid value for field '%s.%s': '%s'. Must start with '/'.", field, k, v)
		}
	}
	if re := str(mr, "regexMatch"); re != "" {
		if _, err := regexp.Compile(re); err != nil {
			return invalid("Invalid value for field '%s.regexMatch': '%s'. Not a valid RE2 regular expression.", field, re)
		}
	}
	if pt := str(mr, "pathTemplateMatch"); pt != "" {
		if _, _, err := compilePathTemplate(pt); err != nil {
			return invalid("Invalid value for field '%s.pathTemplateMatch': '%s'. %v", field, pt, err)
		}
	}
	for i, e := range listOf(mr["headerMatches"]) {
		hm := objOf(e)
		if str(hm, "headerName") == "" {
			return required(field + ".headerMatches[" + strconv.Itoa(i) + "].headerName")
		}
		if re := str(hm, "regexMatch"); re != "" {
			if _, err := regexp.Compile(re); err != nil {
				return invalid("Invalid value for field '%s.headerMatches[%d].regexMatch': '%s'. Not a valid RE2 regular expression.", field, i, re)
			}
		}
	}
	for i, e := range listOf(mr["queryParameterMatches"]) {
		if str(objOf(e), "name") == "" {
			return required(field + ".queryParameterMatches[" + strconv.Itoa(i) + "].name")
		}
	}
	return nil
}

func validPathPattern(p string) bool {
	if p == "" || p[0] != '/' || strings.ContainsAny(p, "?#") {
		return false
	}
	if i := strings.Index(p, "*"); i >= 0 {
		return i == len(p)-1 && p[i-1] == '/'
	}
	return true
}

func validHostPattern(h string) bool {
	if h == "" {
		return false
	}
	if i := strings.Index(h, "*"); i >= 0 {
		if i != 0 || strings.Count(h, "*") > 1 {
			return false
		}
		return len(h) == 1 || h[1] == '-' || h[1] == '.'
	}
	return true
}

// lbDecision is where a URL map sends one request.
type lbDecision struct {
	// service is the backend service or bucket; weighted, when set, are
	// the weighted backend services to choose from instead.
	service  string
	weighted []obj
	redirect obj
	action   obj
	// headers are the header actions that apply, outermost first.
	headers []obj
	// prefix is the matched path prefix (a path rule's, before its '*',
	// or a route rule's prefixMatch), for prefixRedirect and
	// pathPrefixRewrite; vars are a path template's captured variables.
	prefix string
	vars   map[string]string
}

// lbMatch routes a request (its host, path without query, headers and
// query) through URL map m.
func lbMatch(m obj, host, path string, h http.Header, q url.Values) lbDecision {
	d := lbDecision{}
	addHeaders := func(o obj) {
		if ha := objOf(o["headerAction"]); ha != nil {
			d.headers = append(d.headers, ha)
		}
	}
	addHeaders(m)
	level := func(holder obj, svc, redir, action string) {
		d.service = str(holder, svc)
		d.redirect = objOf(holder[redir])
		d.action = objOf(holder[action])
		if d.action != nil {
			d.weighted = nil
			for _, e := range listOf(d.action["weightedBackendServices"]) {
				d.weighted = append(d.weighted, objOf(e))
			}
		}
	}
	pm := pathMatcherFor(m, host)
	if pm == nil {
		level(m, "defaultService", "defaultUrlRedirect", "defaultRouteAction")
		return d
	}
	addHeaders(pm)
	if rrs := listOf(pm["routeRules"]); len(rrs) > 0 {
		rules := make([]obj, 0, len(rrs))
		for _, e := range rrs {
			rules = append(rules, objOf(e))
		}
		sort.SliceStable(rules, func(i, j int) bool { return numOr(rules[i], "priority", 0) < numOr(rules[j], "priority", 0) })
		for _, rr := range rules {
			for _, me := range listOf(rr["matchRules"]) {
				if ok, prefix, vars := matchRule(objOf(me), path, h, q); ok {
					addHeaders(rr)
					level(rr, "service", "urlRedirect", "routeAction")
					d.prefix, d.vars = prefix, vars
					return d
				}
			}
		}
	}
	best, bestLen := obj(nil), -1
	for _, e := range listOf(pm["pathRules"]) {
		pr := objOf(e)
		for _, p := range strs(pr, "paths") {
			if pathMatches(p, path) && len(p) > bestLen {
				best, bestLen = pr, len(p)
				d.prefix = strings.TrimSuffix(p, "*")
			}
		}
	}
	if best != nil {
		level(best, "service", "urlRedirect", "routeAction")
		return d
	}
	d.prefix = ""
	level(pm, "defaultService", "defaultUrlRedirect", "defaultRouteAction")
	if d.service == "" && d.redirect == nil && len(d.weighted) == 0 {
		level(m, "defaultService", "defaultUrlRedirect", "defaultRouteAction")
	}
	return d
}

// pathMatcherFor is the path matcher of the host rule that best matches
// host: an exact host first, then the longest wildcard, then "*".
func pathMatcherFor(m obj, host string) obj {
	host = strings.ToLower(host)
	bare := host
	if h, _, err := splitHostPortLB(host); err == nil {
		bare = h
	}
	best, score := "", -1
	for _, e := range listOf(m["hostRules"]) {
		hr := objOf(e)
		for _, pat := range strs(hr, "hosts") {
			pat = strings.ToLower(pat)
			sc := -1
			switch {
			case pat == host || pat == bare:
				sc = 1 << 20
			case pat == "*":
				sc = 0
			case strings.HasPrefix(pat, "*"):
				if strings.HasSuffix(bare, pat[1:]) && len(bare) > len(pat)-1 {
					sc = len(pat)
				}
			}
			if sc > score {
				best, score = str(hr, "pathMatcher"), sc
			}
		}
	}
	if best == "" {
		return nil
	}
	for _, e := range listOf(m["pathMatchers"]) {
		if str(objOf(e), "name") == best {
			return objOf(e)
		}
	}
	return nil
}

func splitHostPortLB(h string) (string, string, error) {
	i := strings.LastIndex(h, ":")
	if i < 0 || strings.HasSuffix(h, "]") {
		return h, "", errNoPort
	}
	return h[:i], h[i+1:], nil
}

var errNoPort = invalid("no port")

// pathMatches is a path rule's pattern against a path.
func pathMatches(pat, path string) bool {
	if strings.HasSuffix(pat, "*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pat, "*"))
	}
	return pat == path
}

// matchRule is one route rule match: whether it matches, its matched
// prefix and its path template's variables.
func matchRule(mr obj, path string, h http.Header, q url.Values) (bool, string, map[string]string) {
	ic := mr["ignoreCase"] == true
	cmp := func(a string) string {
		if ic {
			return strings.ToLower(a)
		}
		return a
	}
	prefix := ""
	var vars map[string]string
	switch {
	case str(mr, "prefixMatch") != "":
		pm := str(mr, "prefixMatch")
		if !strings.HasPrefix(cmp(path), cmp(pm)) {
			return false, "", nil
		}
		prefix = path[:len(pm)]
	case str(mr, "fullPathMatch") != "":
		if cmp(path) != cmp(str(mr, "fullPathMatch")) {
			return false, "", nil
		}
		prefix = path
	case str(mr, "regexMatch") != "":
		re, err := regexp.Compile("^(?:" + str(mr, "regexMatch") + ")$")
		if err != nil || !re.MatchString(path) {
			return false, "", nil
		}
	case str(mr, "pathTemplateMatch") != "":
		re, names, err := compilePathTemplate(str(mr, "pathTemplateMatch"))
		if err != nil {
			return false, "", nil
		}
		sm := re.FindStringSubmatch(path)
		if sm == nil {
			return false, "", nil
		}
		vars = map[string]string{}
		for i, n := range names {
			vars[n] = sm[i+1]
		}
	}
	for _, e := range listOf(mr["headerMatches"]) {
		if !headerMatch(objOf(e), h) {
			return false, "", nil
		}
	}
	for _, e := range listOf(mr["queryParameterMatches"]) {
		qm := objOf(e)
		vals, present := q[str(qm, "name")]
		v := ""
		if len(vals) > 0 {
			v = vals[0]
		}
		switch {
		case qm["presentMatch"] == true:
			if !present {
				return false, "", nil
			}
		case qm["exactMatch"] != nil:
			if !present || v != str(qm, "exactMatch") {
				return false, "", nil
			}
		case str(qm, "regexMatch") != "":
			re, err := regexp.Compile("^(?:" + str(qm, "regexMatch") + ")$")
			if err != nil || !present || !re.MatchString(v) {
				return false, "", nil
			}
		}
	}
	return true, prefix, vars
}

func headerMatch(hm obj, h http.Header) bool {
	name := str(hm, "headerName")
	vals, present := h[http.CanonicalHeaderKey(name)]
	v := strings.Join(vals, ",")
	ok := true
	switch {
	case hm["presentMatch"] != nil:
		ok = present == (hm["presentMatch"] == true)
	case hm["exactMatch"] != nil:
		ok = present && v == str(hm, "exactMatch")
	case hm["prefixMatch"] != nil:
		ok = present && strings.HasPrefix(v, str(hm, "prefixMatch"))
	case hm["suffixMatch"] != nil:
		ok = present && strings.HasSuffix(v, str(hm, "suffixMatch"))
	case str(hm, "regexMatch") != "":
		re, err := regexp.Compile("^(?:" + str(hm, "regexMatch") + ")$")
		ok = err == nil && present && re.MatchString(v)
	case hm["rangeMatch"] != nil:
		rm := objOf(hm["rangeMatch"])
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		lo, _ := strconv.ParseInt(str(rm, "rangeStart"), 10, 64)
		hi, _ := strconv.ParseInt(str(rm, "rangeEnd"), 10, 64)
		if f, ok2 := rm["rangeStart"].(float64); ok2 {
			lo = int64(f)
		}
		if f, ok2 := rm["rangeEnd"].(float64); ok2 {
			hi = int64(f)
		}
		ok = present && err == nil && n >= lo && n < hi
	default:
		ok = present
	}
	if hm["invertMatch"] == true {
		return !ok
	}
	return ok
}

// compilePathTemplate turns a path template (/{a=*}/x/{b=**}, a '*' one
// segment, '**' the rest) into a regular expression and its variables.
func compilePathTemplate(t string) (*regexp.Regexp, []string, error) {
	if t == "" || t[0] != '/' {
		return nil, nil, invalid("A path template must start with '/'.")
	}
	var b strings.Builder
	var names []string
	b.WriteString("^")
	segs := strings.Split(t[1:], "/")
	for i, seg := range segs {
		b.WriteString("/")
		last := i == len(segs)-1
		switch {
		case seg == "*":
			b.WriteString("[^/]+")
		case seg == "**":
			if !last {
				return nil, nil, invalid("'**' is allowed only in the last segment.")
			}
			b.WriteString(".*")
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			in := seg[1 : len(seg)-1]
			name, pat := in, "*"
			if j := strings.Index(in, "="); j >= 0 {
				name, pat = in[:j], in[j+1:]
			}
			if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`).MatchString(name) {
				return nil, nil, invalid("Invalid variable name '%s'.", name)
			}
			names = append(names, name)
			switch pat {
			case "*":
				b.WriteString("([^/]+)")
			case "**":
				if !last {
					return nil, nil, invalid("'**' is allowed only in the last segment.")
				}
				b.WriteString("(.*)")
			default:
				return nil, nil, invalid("A variable matches '*' or '**'.")
			}
		default:
			if strings.ContainsAny(seg, "*{}") {
				return nil, nil, invalid("Invalid segment '%s'.", seg)
			}
			b.WriteString(regexp.QuoteMeta(seg))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return re, names, err
}

// rewritten is the request's path and host after the route action's URL
// rewrite.
func (d lbDecision) rewritten(host, path string) (string, string) {
	ur := objOf(d.action["urlRewrite"])
	if ur == nil {
		return host, path
	}
	if h := str(ur, "hostRewrite"); h != "" {
		host = h
	}
	if p := str(ur, "pathPrefixRewrite"); p != "" && d.prefix != "" && strings.HasPrefix(path, d.prefix) {
		path = p + path[len(d.prefix):]
	}
	if t := str(ur, "pathTemplateRewrite"); t != "" && d.vars != nil {
		path = regexp.MustCompile(`\{([a-zA-Z][a-zA-Z0-9_]*)\}`).ReplaceAllStringFunc(t, func(v string) string {
			return d.vars[v[1:len(v)-1]]
		})
	}
	return host, path
}

// redirectURL is where a redirect sends the request, and its status.
func (d lbDecision) redirectURL(scheme, host, path, rawQuery string) (string, int) {
	r := d.redirect
	if r["httpsRedirect"] == true {
		scheme = "https"
	}
	if h := str(r, "hostRedirect"); h != "" {
		host = h
	}
	switch {
	case str(r, "pathRedirect") != "":
		path = str(r, "pathRedirect")
	case str(r, "prefixRedirect") != "":
		path = str(r, "prefixRedirect") + strings.TrimPrefix(path, d.prefix)
	}
	u := scheme + "://" + host + path
	if rawQuery != "" && r["stripQuery"] != true {
		u += "?" + rawQuery
	}
	return u, redirectCodes[str(r, "redirectResponseCode")]
}

// runURLMapTestsLocked runs a URL map's tests: how many passed, and each
// failure as a TestFailure (with _index and _why).
func (s *Server) runURLMapTestsLocked(x lbCtx, m obj) (int, []obj) {
	passed := 0
	var failures []obj
	for i, e := range listOf(m["tests"]) {
		t := objOf(e)
		host := str(t, "host")
		u, _ := url.Parse(str(t, "path"))
		path, rawQuery := str(t, "path"), ""
		if u != nil {
			path, rawQuery = u.Path, u.RawQuery
		}
		h := http.Header{}
		for _, he := range listOf(t["headers"]) {
			hh := objOf(he)
			h.Add(str(hh, "name"), str(hh, "value"))
		}
		q, _ := url.ParseQuery(rawQuery)
		d := lbMatch(m, host, path, h, q)
		f := obj{"host": host, "path": str(t, "path")}
		why := ""
		if d.redirect != nil {
			loc, code := d.redirectURL("http", host, path, rawQuery)
			f["actualOutputUrl"], f["actualRedirectResponseCode"] = loc, code
			if want := str(t, "expectedOutputUrl"); want != "" && want != loc {
				why = "expected output URL '" + want + "' but got '" + loc + "'"
			}
			if want, ok := num(t, "expectedRedirectResponseCode"); ok && int(want) != code {
				why = "expected redirect response code " + strconv.Itoa(int(want)) + " but got " + strconv.Itoa(code)
			}
			if str(t, "service") != "" {
				why = "expected service '" + str(t, "service") + "' but the URL is redirected"
			}
		} else {
			svc := d.service
			if svc == "" && len(d.weighted) > 0 {
				svc = str(d.weighted[0], "backendService")
			}
			f["actualService"] = svc
			if want := str(t, "service"); want != "" && want != svc {
				why = "expected service '" + want + "' but got '" + svc + "'"
			}
			nh, np := d.rewritten(host, path)
			out := "http://" + nh + np
			if rawQuery != "" {
				out += "?" + rawQuery
			}
			if want := str(t, "expectedOutputUrl"); want != "" && strings.TrimPrefix(strings.TrimPrefix(want, "http://"), "https://") != strings.TrimPrefix(out, "http://") {
				why = "expected output URL '" + want + "' but got '" + out + "'"
			}
			f["actualOutputUrl"] = out
			if _, ok := num(t, "expectedRedirectResponseCode"); ok {
				why = "expected a redirect but the URL is not redirected"
			}
		}
		if why == "" {
			passed++
			continue
		}
		f["expectedService"], f["expectedOutputUrl"] = t["service"], t["expectedOutputUrl"]
		f["expectedRedirectResponseCode"] = t["expectedRedirectResponseCode"]
		f["_index"], f["_why"] = i, why
		failures = append(failures, f)
	}
	return passed, failures
}

// validateURLMap is urlMaps.validate: the given map, checked and its tests
// run, without changing anything.
func validateURLMap(s *Server, x lbCtx, o, b obj) (obj, error) {
	m := norm(objOf(b["resource"]))
	if m == nil {
		return nil, required("resource")
	}
	res := obj{}
	if err := s.checkURLMapLocked(x, m, map[string]bool{}); err != nil {
		res["loadSucceeded"], res["loadErrors"] = false, []any{err.Error()}
		return obj{"result": res}, nil
	}
	res["loadSucceeded"] = true
	_, failures := s.runURLMapTestsLocked(x, m)
	res["testPassed"] = len(failures) == 0
	if len(failures) > 0 {
		var l []any
		for _, f := range failures {
			delete(f, "_index")
			delete(f, "_why")
			l = append(l, f)
		}
		res["testFailures"] = l
	}
	return obj{"result": res}, nil
}

// urlMapServicesLocked are every backend service and bucket a URL map
// names, sorted.
func urlMapServices(m obj) []string {
	set := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, e := range t {
				if k == "tests" {
					continue
				}
				if s, ok := e.(string); ok && (k == "defaultService" || k == "service" || k == "backendService") {
					set[s] = true
					continue
				}
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(m)
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
