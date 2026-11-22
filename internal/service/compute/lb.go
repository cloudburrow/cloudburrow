package compute

import (
	"net/http"
	"sort"
	"strings"
)

// Cloud Load Balancing (#2153), served through the Compute Engine API as
// https://docs.cloud.google.com/load-balancing/docs and the REST
// references of each collection describe it (read 2026-11-22): health
// checks (legacy and new), backend services and backend buckets, URL
// maps, target HTTP, HTTPS, TCP and SSL proxies, target pools and target
// instances, SSL certificates and SSL policies, network endpoint groups,
// and the load-balancing forwarding rules (psc.go keeps the Private
// Service Connect ones). Each collection is described by an lbColl and
// served by the generic insert, get, list, patch, update and delete here,
// with its own validation (build) and its own methods (post, get).
//
// The records are what the data plane (lbplane.go) routes by: a
// forwarding rule is a listener on this machine, its target's URL map
// chooses a backend service, whose health-checked backends serve.

// lbCtx is where one call's resource lives.
type lbCtx struct {
	project string
	// scope is "global", "regions/R" or "zones/Z".
	scope        string
	region, zone string
	// coll and rel are the collection and, on item calls, the resource.
	coll, rel string
}

func (x lbCtx) collRel() string { return "projects/" + x.project + "/" + x.scope + "/" + x.coll }

// opScope is the operation scope of x's mutations.
func (x lbCtx) opScope() string { return x.scope }

// lbColl is one load-balancing collection.
type lbColl struct {
	coll, kind string
	// global, regional and zonal are the scopes it is served in.
	global, regional, zonal bool
	// build validates a request's body (the merged record on patch) and
	// returns the record to keep, without its name, id, selfLink,
	// creationTimestamp, region or zone, which are set here; old is the
	// current record on patch and update, nil on insert.
	build func(s *Server, x lbCtx, b, old obj) (obj, error)
	// patch and update are whether PATCH (JSON merge patch) and PUT are
	// served; fingerprint whether the record carries one.
	patch, update, fingerprint bool
	// post are the collection's POST methods on an item (setUrlMap,
	// addInstance, ...), get its GET ones (getHealth); each returns what
	// the method answers.
	post map[string]func(s *Server, x lbCtx, o, b obj) (obj, error)
	get  map[string]func(s *Server, x lbCtx, o obj, c call) (obj, error)
	// collPost are POST methods on the collection (urlMaps.validate is on
	// an item; listAvailableFeatures is a GET on the collection).
	collGet map[string]func(s *Server, x lbCtx, c call) (obj, error)
	// iam is whether getIamPolicy and setIamPolicy are served; every
	// collection with testIamPermissions serves it.
	iam, testIam bool
	// decorate adds a record's output-only fields when it is read.
	decorate func(s *Server, o obj) obj
	// deleted runs as a record is deleted (an address released).
	deleted func(s *Server, o obj)
	// noun is the resource's name in an in-use refusal.
	noun string
}

// lbUnservedVerbs answer 501 whether or not their record exists, as the
// coverage table's unserved methods do.
var lbUnservedVerbs = map[string]bool{"setSecurityPolicy": true, "setEdgeSecurityPolicy": true, "addSignedUrlKey": true,
	"deleteSignedUrlKey": true, "setCertificateMap": true, "getEffectiveSecurityPolicies": true}

// lbColls are the collections, by their path segment.
var lbColls = map[string]*lbColl{}

func registerLB(c *lbColl) { lbColls[c.coll] = c }

// lbKinds are the kinds of every load-balancing record, and the forwarding
// rules', which in-use checks scan.
func isLBRecord(o obj) bool {
	k := str(o, "kind")
	if k == "compute#forwardingRule" || k == "compute#serviceAttachment" {
		return true
	}
	for _, c := range lbColls {
		if c.kind == k {
			return true
		}
	}
	return false
}

func init() {
	// The load-balancing records' link fields, made absolute when they
	// are returned (server.go's render).
	for _, k := range []string{"healthChecks", "backendService", "defaultService", "service", "urlMap", "sslCertificates",
		"sslPolicy", "target", "group", "instances", "instance", "backupPool", "reference", "forwardingRule",
		"producerForwardingRule", "targetService", "natSubnets", "baseForwardingRule", "healthCheck"} {
		linkKeys[k] = true
	}
}

// lbRoute serves c when it names a load-balancing collection; ok is false
// otherwise, and the router goes on.
func (s *Server) lbRoute(c call) (out obj, err error, ok bool) {
	p := c.parts
	n := len(p)
	if n == 3 && (p[0] == "targetHttpProxies" || p[0] == "targetHttpsProxies") {
		// setUrlMap and setSslCertificates of the global proxies are
		// under projects/P/, not projects/P/global/ (the discovery
		// document's paths).
		p = append([]string{"global"}, p...)
		n++
	}
	if n == 2 && p[0] == "aggregated" && c.method == http.MethodGet {
		if lc := lbColls[p[1]]; lc != nil {
			out, err = s.lbAggregated(c, lc)
			return out, err, true
		}
		return nil, nil, false
	}
	x := lbCtx{project: c.project}
	var rest []string
	switch {
	case n >= 2 && p[0] == "global":
		x.scope, rest = "global", p[1:]
	case n >= 3 && p[0] == "regions" && IsRegion(p[1]):
		x.scope, x.region, rest = "regions/"+p[1], p[1], p[2:]
	case n >= 3 && p[0] == "zones" && IsZone(p[1]):
		x.scope, x.zone, x.region, rest = "zones/"+p[1], p[1], regionOfZone(p[1]), p[2:]
	default:
		return nil, nil, false
	}
	x.coll = rest[0]
	if x.coll == "forwardingRules" && x.zone == "" {
		return s.lbForwardingRules(c, x, rest)
	}
	lc := lbColls[x.coll]
	if lc == nil || !lc.in(x) {
		return nil, nil, false
	}
	out, err = s.lbServe(c, lc, x, rest)
	return out, err, true
}

func (lc *lbColl) in(x lbCtx) bool {
	switch {
	case x.zone != "":
		return lc.zonal
	case x.region != "":
		return lc.regional
	}
	return lc.global
}

// lbServe serves one call on a collection.
func (s *Server) lbServe(c call, lc *lbColl, x lbCtx, rest []string) (obj, error) {
	get, post := c.method == http.MethodGet, c.method == http.MethodPost
	coll := x.collRel()
	switch len(rest) {
	case 1:
		switch {
		case get:
			out, err := s.listOut(lc.kind+"List", coll, s.res, c.query)
			if err == nil && lc.decorate != nil {
				items := out["items"].([]obj)
				for i, it := range items {
					items[i] = lc.decorate(s, deepCopy(it).(obj))
				}
			}
			return out, err
		case post:
			return s.lbInsert(lc, x, c.body)
		}
	case 2:
		if f := lc.collGet[rest[1]]; f != nil && get {
			return f(s, x, c)
		}
		x.rel = coll + "/" + rest[1]
		switch c.method {
		case http.MethodGet:
			o, err := s.getRes(x.rel)
			if err == nil && lc.decorate != nil {
				o = lc.decorate(s, deepCopy(o).(obj))
			}
			return o, err
		case http.MethodDelete:
			return s.lbDelete(lc, x)
		case http.MethodPatch:
			if lc.patch {
				return s.lbPatch(lc, x, c.body, false)
			}
		case http.MethodPut:
			if lc.update {
				return s.lbPatch(lc, x, c.body, true)
			}
		}
	case 3:
		x.rel = coll + "/" + rest[1]
		verb := rest[2]
		if iamVerbs[verb] && (lc.iam || (lc.testIam && verb == "testIamPermissions")) {
			return s.iamCall(c, x.rel)
		}
		o, ok := s.res[x.rel]
		if (post && lc.post[verb] != nil) || (get && lc.get[verb] != nil) {
			// validate checks the map in its body, which need not exist yet
			// (gcloud's url-maps validate --source runs before an import).
			if !ok && !lbUnservedVerbs[verb] && verb != "validate" {
				return nil, notFound(x.rel)
			}
			if post {
				b := c.body
				if v := first(c.query, "failoverRatio"); v != "" && verb == "setBackup" {
					// setBackup's failoverRatio is a query parameter.
					b = deepCopy(b).(obj)
					b["_failoverRatio"] = v
				}
				return lc.post[verb](s, x, o, b)
			}
			return lc.get[verb](s, x, o, c)
		}
	}
	return nil, unimplemented("%s %s/%s is not served (#2153)", c.method, coll, strings.Join(rest[1:], "/"))
}

// lbScopeFields sets a record's region or zone.
func lbScopeFields(o obj, x lbCtx) {
	switch {
	case x.zone != "":
		o["zone"] = "projects/" + x.project + "/zones/" + x.zone
	case x.region != "":
		o["region"] = "projects/" + x.project + "/regions/" + x.region
	}
}

func (s *Server) lbInsert(lc *lbColl, x lbCtx, b obj) (obj, error) {
	b = norm(b)
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	x.rel = x.collRel() + "/" + name
	if _, ok := s.res[x.rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", x.rel)
	}
	o, err := lc.build(s, x, b, nil)
	if err != nil {
		return nil, err
	}
	o["kind"], o["name"], o["selfLink"] = lc.kind, name, x.rel
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	lbScopeFields(o, x)
	o["id"], o["creationTimestamp"] = newID(), s.stamp()
	if lc.fingerprint {
		o["fingerprint"] = newFingerprint()
	}
	s.res[x.rel] = norm(o)
	s.lbChangedLocked()
	return s.done(x.project, x.opScope(), "insert", x.rel, s.res[x.rel]["id"])
}

// lbFixed are the fields a patch or update never changes.
var lbFixed = map[string]bool{"kind": true, "id": true, "selfLink": true, "selfLinkWithId": true, "creationTimestamp": true,
	"region": true, "zone": true, "fingerprint": true, "usedBy": true}

// lbPatch is a PATCH (JSON merge patch, RFC 7396) or, replace, a PUT.
func (s *Server) lbPatch(lc *lbColl, x lbCtx, b obj, replace bool) (obj, error) {
	b = norm(b)
	old, ok := s.res[x.rel]
	if !ok {
		return nil, notFound(x.rel)
	}
	if fp := str(b, "fingerprint"); lc.fingerprint && fp != "" && fp != str(old, "fingerprint") {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
			"Supplied fingerprint does not match current metadata fingerprint.")
	}
	if nm := str(b, "name"); nm != "" && nm != str(old, "name") {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. The name of a resource cannot be changed.", nm)
	}
	var merged obj
	if replace {
		merged = obj{}
		for k, v := range b {
			merged[k] = v
		}
	} else {
		merged = obj{}
		for k, v := range old {
			if !strings.HasPrefix(k, "_") {
				merged[k] = deepCopy(v)
			}
		}
		mergePatch(merged, b)
	}
	for k := range lbFixed {
		delete(merged, k)
	}
	merged["name"] = str(old, "name")
	o, err := lc.build(s, x, merged, old)
	if err != nil {
		return nil, err
	}
	for k := range lbFixed {
		if v, ok := old[k]; ok && k != "fingerprint" {
			o[k] = v
		}
	}
	o["kind"], o["name"], o["selfLink"] = lc.kind, str(old, "name"), x.rel
	if d := str(merged, "description"); d != "" {
		o["description"] = d
	}
	if lc.fingerprint {
		o["fingerprint"] = newFingerprint()
	}
	s.res[x.rel] = norm(o)
	s.lbChangedLocked()
	op := "patch"
	if replace {
		op = "update"
	}
	return s.done(x.project, x.opScope(), op, x.rel, old["id"])
}

// mergePatch applies a JSON merge patch to dst: objects merge, a null
// removes, anything else replaces.
func mergePatch(dst, patch obj) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergePatch(dm, pm)
				continue
			}
		}
		dst[k] = v
	}
}

// lbUsersLocked are the load-balancing records (and forwarding rules)
// whose links name rel, sorted.
func (s *Server) lbUsersLocked(rel string) []string {
	var out []string
	for k, o := range s.res {
		if strings.Contains(k, "/serviceAttachments/") && str(o, "targetService") == rel {
			// A service attachment publishing the rule (#2140).
			out = append(out, k)
			continue
		}
		if k == rel || !isLBRecord(o) {
			continue
		}
		if linksTo(o, rel, true) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// linksTo reports whether any string of v (not its own selfLink, network,
// subnetwork, region or zone) names rel.
func linksTo(v any, rel string, top bool) bool {
	switch t := v.(type) {
	case string:
		return t == rel
	case []any:
		for _, e := range t {
			if linksTo(e, rel, false) {
				return true
			}
		}
	case map[string]any:
		for k, e := range t {
			if top && (k == "selfLink" || k == "network" || k == "subnetwork" || k == "region" || k == "zone" || k == "usedBy") {
				continue
			}
			if linksTo(e, rel, false) {
				return true
			}
		}
	}
	return false
}

func (s *Server) lbDelete(lc *lbColl, x lbCtx) (obj, error) {
	o, ok := s.res[x.rel]
	if !ok {
		return nil, notFound(x.rel)
	}
	if users := s.lbUsersLocked(x.rel); len(users) > 0 {
		return nil, inUse(lc.noun, x.rel, users[0])
	}
	if lc.deleted != nil {
		lc.deleted(s, o)
	}
	delete(s.res, x.rel)
	s.lbChangedLocked()
	return s.done(x.project, x.opScope(), "delete", x.rel, o["id"])
}

// lbAggregated is a collection's aggregatedList, by scope.
func (s *Server) lbAggregated(c call, lc *lbColl) (obj, error) {
	f, err := parseFilter(first(c.query, "filter"))
	if err != nil {
		return nil, err
	}
	prefix := "projects/" + c.project + "/"
	scopes := map[string][]obj{}
	var keys []string
	for k := range s.res {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		rest := strings.Split(k[len(prefix):], "/")
		var scope string
		switch {
		case len(rest) == 3 && rest[0] == "global" && rest[1] == lc.coll:
			scope = "global"
		case len(rest) == 4 && (rest[0] == "regions" || rest[0] == "zones") && rest[2] == lc.coll:
			scope = rest[0] + "/" + rest[1]
		default:
			continue
		}
		o := s.res[k]
		if !f.match(o) {
			continue
		}
		if lc.decorate != nil {
			o = lc.decorate(s, deepCopy(o).(obj))
		}
		scopes[scope] = append(scopes[scope], o)
	}
	items := map[string]any{}
	for sc, l := range scopes {
		items[sc] = map[string]any{lc.coll: l}
	}
	return obj{"kind": lc.kind + "AggregatedList", "id": "projects/" + c.project + "/aggregated/" + lc.coll, "items": items}, nil
}

// lbRef resolves a link field: a full URL, a relative name or a bare name
// (in x's scope), of one of colls, which must exist. It returns the
// relative name and the record.
func (s *Server) lbRef(x lbCtx, field, ref string, colls ...string) (string, obj, error) {
	if ref == "" {
		return "", nil, required(field)
	}
	rel := Rel(ref, x.project)
	if !strings.HasPrefix(rel, "projects/") {
		if strings.Contains(rel, "/") {
			return "", nil, invalid("Invalid value for field '%s': '%s'. The URL is malformed.", field, ref)
		}
		rel = "projects/" + x.project + "/" + x.scope + "/" + colls[0] + "/" + rel
	}
	parts := strings.Split(rel, "/")
	coll := ""
	if len(parts) >= 2 {
		coll = parts[len(parts)-2]
	}
	okColl := false
	for _, c := range colls {
		if c == coll {
			okColl = true
		}
	}
	if !okColl {
		return "", nil, invalid("Invalid value for field '%s': '%s'. The URL is malformed.", field, ref)
	}
	o, ok := s.res[rel]
	if !ok {
		return "", nil, errf(http.StatusNotFound, "notFound", "The resource '%s' was not found", rel)
	}
	return rel, o, nil
}

// scopeOf is a relative name's scope: "global", "regions/R" or "zones/Z".
func scopeOf(rel string) string {
	p := strings.Split(rel, "/")
	if len(p) >= 4 && (p[2] == "regions" || p[2] == "zones") {
		return p[2] + "/" + p[3]
	}
	return "global"
}

// regionOfScope is a scope's region ("" for global).
func regionOfScope(scope string) string {
	switch {
	case strings.HasPrefix(scope, "regions/"):
		return strings.TrimPrefix(scope, "regions/")
	case strings.HasPrefix(scope, "zones/"):
		return regionOfZone(strings.TrimPrefix(scope, "zones/"))
	}
	return ""
}

// num is o[k] as a float64, and whether it is set.
func num(o obj, k string) (float64, bool) {
	v, ok := o[k].(float64)
	return v, ok
}

// numOr is o[k], or def when unset.
func numOr(o obj, k string, def float64) float64 {
	if v, ok := num(o, k); ok {
		return v
	}
	return def
}

// oneOf checks an enum field, returning def when it is unset.
func oneOf(field, v, def string, allowed ...string) (string, error) {
	if v == "" {
		return def, nil
	}
	for _, a := range allowed {
		if a == v {
			return v, nil
		}
	}
	return "", invalid("Invalid value for field '%s': '%s'. Must be one of [%s]", field, v, strings.Join(allowed, ", "))
}

// copyFields copies the set fields of b named by keys into o.
func copyFields(o, b obj, keys ...string) {
	for _, k := range keys {
		if v, ok := b[k]; ok && v != nil {
			o[k] = deepCopy(v)
		}
	}
}

// objOf is v as an object, nil when it is not one.
func objOf(v any) obj {
	m, _ := v.(map[string]any)
	return m
}
