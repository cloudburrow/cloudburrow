package compute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Prefix is the path every Compute Engine v1 call is under.
const Prefix = "/compute/v1/"

// call is one request, as the router sees it.
type call struct {
	method  string
	project string
	parts   []string // the path after projects/P/
	query   map[string][]string
	body    obj
}

// ServeHTTP serves the API, and the batch endpoint gcloud sends to.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/batch/compute/v1" || r.URL.Path == "/batch" {
		s.serveBatch(w, r)
		return
	}
	if r.URL.Path == CSEKCertPath && r.Method == http.MethodGet {
		s.serveCSEKCert(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, GuestPrefix) {
		s.serveGuest(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, Prefix) {
		writeError(w, unimplemented("CloudBurrow's Compute Engine API serves %s only (%s %s).", Prefix, r.Method, r.URL.Path))
		return
	}
	if s.opts.Extra != nil && s.opts.Extra(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, Prefix), "/"), "/")
	if len(parts) < 2 || parts[0] != "projects" {
		writeError(w, unimplemented("%s %s is not a Compute Engine method served here", r.Method, r.URL.Path))
		return
	}
	c := call{method: r.Method, project: parts[1], parts: parts[2:], query: r.URL.Query()}
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeError(w, invalid("The request body could not be read: %v", err))
			return
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &c.body); err != nil {
				writeError(w, errf(http.StatusBadRequest, "parseError", "Invalid JSON payload received. %v", err))
				return
			}
		}
	}
	if c.body == nil {
		c.body = obj{}
	}
	if rel := opWaitTarget(c); rel != "" {
		s.waitOp(r.Context(), rel)
	}
	out, err := s.Do(c)
	if err != nil {
		writeError(w, err)
		return
	}
	if out == nil {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write([]byte("{}\n"))
		return
	}
	writeJSON(w, renderAny(out, base(r)))
}

// renderAny renders an object, or a list's items.
func renderAny(v obj, b string) obj {
	out := render(v, b)
	switch items := v["items"].(type) {
	case []obj:
		l := make([]any, len(items))
		for i, it := range items {
			l[i] = render(it, b)
		}
		out["items"] = l
	case map[string]any:
		agg := map[string]any{}
		for scope, sv := range items {
			m := map[string]any{}
			for k, lv := range sv.(map[string]any) {
				if objs, ok := lv.([]obj); ok {
					l := make([]any, len(objs))
					for i, it := range objs {
						l[i] = render(it, b)
					}
					m[k] = l
				}
			}
			agg[scope] = m
		}
		out["items"] = agg
	}
	return out
}

// Call is one request made in process, as the console makes them: method,
// the path after /compute/v1/projects/P/, and its body; the result is
// rendered with relative links.
func (s *Server) Call(method, project, path string, query map[string][]string, body obj) (obj, error) {
	if body == nil {
		body = obj{}
	}
	var parts []string
	if p := strings.Trim(path, "/"); p != "" {
		// An empty path is the project itself (projects.get).
		parts = strings.Split(p, "/")
	}
	c := call{method: method, project: project, parts: parts, query: query, body: body}
	if rel := opWaitTarget(c); rel != "" {
		s.waitOp(context.Background(), rel)
	}
	out, err := s.Do(c)
	if err != nil || out == nil {
		return out, err
	}
	// A JSON round trip, so the console reads plain maps.
	b, _ := json.Marshal(renderAny(out, ""))
	var m obj
	_ = json.Unmarshal(b, &m)
	return m, nil
}

// Do runs one call.
func (s *Server) Do(c call) (obj, error) {
	if c.project == "" {
		return nil, required("project")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.doLocked(c)
	// What a call returns can be the stored resource itself, which an
	// operation's goroutine goes on changing under the lock (vms.go): the
	// caller renders it after the lock is released, so it gets its own copy
	// ("concurrent map iteration and map write" in TestConsoleComputeVMs,
	// Landing 157).
	return deepCopy(out).(obj), err
}

// deepCopy copies maps and lists all the way down, keeping their types
// ([]obj stays []obj, as renderAny tells lists from objects by it).
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return obj(nil)
		}
		out := make(obj, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []map[string]any:
		if t == nil {
			return t
		}
		out := make([]obj, len(t))
		for i, e := range t {
			out[i] = deepCopy(e).(obj)
		}
		return out
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	case []string:
		return append([]string(nil), t...)
	}
	return v
}

// doLocked is Do with s.mu held.
func (s *Server) doLocked(c call) (obj, error) {
	if len(c.parts) >= 2 && c.parts[0] == "global" && c.parts[1] == "images" {
		// Public image projects are not projects of the caller's: no
		// default network is made in them.
		return s.images(c)
	}
	if err := s.ensureProjectLocked(c.project); err != nil {
		return nil, err
	}
	if rel := iamTarget(c); rel != "" {
		return s.iamCall(c, rel)
	}
	p := c.parts
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	patch, put := c.method == http.MethodPatch, c.method == http.MethodPut
	n := len(p)
	switch {
	case n == 0 && get:
		return s.project(c)
	case n == 1 && post && p[0] == "setCommonInstanceMetadata":
		return s.setCommonInstanceMetadata(c)
	case n >= 2 && p[0] == "global":
		coll := p[1]
		rel := "projects/" + c.project + "/global/" + coll
		switch coll {
		case "networks":
			switch {
			case n == 2 && get:
				return s.listOut("compute#networkList", rel, s.res, c.query)
			case n == 2 && post:
				return s.insertNetwork(c.project, c.body)
			case n == 3 && get:
				return s.getRes(rel + "/" + p[2])
			case n == 3 && del:
				return s.deleteNetwork(c.project, p[2])
			case n == 3 && patch:
				return s.patchNetwork(c.project, p[2], c.body)
			case n == 4 && post && p[3] == "switchToCustomMode":
				return s.switchToCustomMode(c.project, p[2])
			}
		case "firewalls":
			switch {
			case n == 2 && get:
				return s.listOut("compute#firewallList", rel, s.res, c.query)
			case n == 2 && post:
				return s.insertFirewall(c.project, c.body)
			case n == 3 && get:
				return s.getRes(rel + "/" + p[2])
			case n == 3 && del:
				return s.deleteSimple(c.project, "global", "firewall", rel+"/"+p[2])
			case n == 3 && (patch || put):
				return s.updateFirewall(c.project, p[2], c.body, put)
			}
		case "routes":
			switch {
			case n == 2 && get:
				return s.listOut("compute#routeList", rel, s.res, c.query)
			case n == 2 && post:
				return s.insertRoute(c.project, c.body)
			case n == 3 && get:
				return s.getRes(rel + "/" + p[2])
			case n == 3 && del:
				return s.deleteRoute(c.project, p[2])
			}
		case "addresses":
			switch {
			case n == 2 && get:
				return s.listOut("compute#addressList", rel, s.res, c.query)
			case n == 2 && post:
				return s.insertAddress(c.project, "", c.body)
			case n == 3 && get:
				return s.getRes(rel + "/" + p[2])
			case n == 3 && del:
				return s.deleteSimple(c.project, "global", "address", rel+"/"+p[2])
			case n == 4 && post && p[3] == "setLabels":
				return s.setLabels(c.project, "global", rel+"/"+p[2], c.body)
			}
		case "operations":
			return s.operation(c, rel)
		case "snapshots":
			return s.snapshots(c)
		}
	case n >= 1 && p[0] == "regions":
		if n == 1 && get {
			return s.listFixed("compute#regionList", c, Regions(), regionObj)
		}
		region := p[1]
		if !IsRegion(region) {
			return nil, notFound("projects/" + c.project + "/regions/" + region)
		}
		if n == 2 && get {
			return regionObj(c.project, region), nil
		}
		if n < 3 {
			break
		}
		scope := "regions/" + region
		rel := "projects/" + c.project + "/" + scope + "/" + p[2]
		switch p[2] {
		case "subnetworks":
			switch {
			case n == 3 && get:
				return s.listOut("compute#subnetworkList", rel, s.res, c.query)
			case n == 3 && post:
				return s.insertSubnetwork(c.project, region, c.body)
			case n == 4 && get:
				return s.getRes(rel + "/" + p[3])
			case n == 4 && del:
				return s.deleteSubnetwork(c.project, region, p[3])
			case n == 4 && patch:
				return s.patchSubnetwork(c.project, region, p[3], c.body)
			case n == 5 && post && p[4] == "expandIpCidrRange":
				return s.expandSubnetwork(c.project, region, p[3], c.body)
			case n == 5 && post && p[4] == "setPrivateIpGoogleAccess":
				return s.setPrivateAccess(c.project, region, p[3], c.body)
			}
		case "addresses":
			switch {
			case n == 3 && get:
				return s.listOut("compute#addressList", rel, s.res, c.query)
			case n == 3 && post:
				return s.insertAddress(c.project, region, c.body)
			case n == 4 && get:
				return s.getRes(rel + "/" + p[3])
			case n == 4 && del:
				return s.deleteSimple(c.project, scope, "address", rel+"/"+p[3])
			case n == 5 && post && p[4] == "setLabels":
				return s.setLabels(c.project, scope, rel+"/"+p[3], c.body)
			}
		case "forwardingRules":
			return s.forwardingRulesLocked(c, region, p, get, post, del)
		case "diskTypes":
			return s.regionDiskTypesCall(c, region)
		case "disks":
			if s.opts.VMs == nil {
				return nil, noVMs()
			}
			return s.regionDisks(c, region)
		case "resourcePolicies":
			return s.resourcePolicies(c, region)
		case "operations":
			return s.operation(c, rel)
		}
	case n >= 1 && p[0] == "zones":
		if n == 1 && get {
			var zones []string
			for _, r := range Regions() {
				zones = append(zones, Zones(r)...)
			}
			return s.listFixed("compute#zoneList", c, zones, zoneObj)
		}
		zone := p[1]
		if !IsZone(zone) {
			return nil, notFound("projects/" + c.project + "/zones/" + zone)
		}
		if n == 2 && get {
			return zoneObj(c.project, zone), nil
		}
		if n >= 3 && p[2] == "operations" {
			return s.operation(c, "projects/"+c.project+"/zones/"+zone+"/operations")
		}
		if n >= 3 {
			return s.zonal(c, zone)
		}
	case n == 2 && p[0] == "aggregated" && get:
		return s.aggregated(c, p[1])
	}
	return nil, unimplemented("%s %sprojects/%s/%s is not served: CloudBurrow's Compute Engine API serves VPC networks, subnetworks, firewall rules, routes, addresses, operations, regions, zones, VM instances, disks, snapshots, machine types and public images (#1936, #1963).",
		c.method, Prefix, c.project, strings.Join(p, "/"))
}

func (s *Server) getRes(rel string) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	return o, nil
}

func (s *Server) listOut(kind, rel string, m map[string]obj, q map[string][]string) (obj, error) {
	items, next, err := s.listLocked(m, rel+"/", q)
	if err != nil {
		return nil, err
	}
	out := obj{"kind": kind, "id": rel, "items": items, "selfLink": rel}
	if items == nil {
		out["items"] = []obj{}
	}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

func (s *Server) listFixed(kind string, c call, names []string, mk func(project, name string) obj) (obj, error) {
	f, err := parseFilter(first(c.query, "filter"))
	if err != nil {
		return nil, err
	}
	var all []obj
	for _, nm := range names {
		if o := mk(c.project, nm); f.match(o) {
			all = append(all, o)
		}
	}
	items, next, err := page(all, c.query)
	if err != nil {
		return nil, err
	}
	out := obj{"kind": kind, "id": "projects/" + c.project, "items": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

// operation serves get, list, delete and wait on a scope's operations.
func (s *Server) operation(c call, rel string) (obj, error) {
	p := c.parts
	n := len(p)
	last := p[n-1]
	switch {
	case last == "operations" && c.method == http.MethodGet:
		return s.listOut("compute#operationList", rel, s.ops, c.query)
	case c.method == http.MethodGet && n >= 2 && p[n-2] == "operations",
		c.method == http.MethodPost && last == "wait" && n >= 3 && p[n-3] == "operations":
		name := last
		if last == "wait" {
			name = p[n-2]
		}
		o, ok := s.ops[rel+"/"+name]
		if !ok {
			return nil, notFound(rel + "/" + name)
		}
		return o, nil
	case c.method == http.MethodDelete && n >= 2 && p[n-2] == "operations":
		if _, ok := s.ops[rel+"/"+last]; !ok {
			return nil, notFound(rel + "/" + last)
		}
		delete(s.ops, rel+"/"+last)
		return nil, s.saveLocked()
	}
	return nil, unimplemented("%s on %s", c.method, rel)
}

// aggregated is an aggregatedList of addresses, subnetworks or operations,
// by scope.
func (s *Server) aggregated(c call, coll string) (obj, error) {
	if coll == "diskTypes" {
		return s.aggregatedDiskTypes(c)
	}
	var m map[string]obj
	key, kind := coll, ""
	switch coll {
	case "addresses":
		m, kind = s.res, "compute#addressAggregatedList"
	case "subnetworks":
		m, kind = s.res, "compute#subnetworkAggregatedList"
	case "forwardingRules":
		m, kind = s.res, "compute#forwardingRuleAggregatedList"
		for k, o := range s.res {
			if strings.Contains(k, "/forwardingRules/") {
				s.pscStatusLocked(o)
			}
		}
	case "operations":
		m, kind = s.ops, "compute#operationAggregatedList"
	case "instances":
		m, kind = s.res, "compute#instanceAggregatedList"
	case "disks":
		m, kind = s.res, "compute#diskAggregatedList"
	case "resourcePolicies":
		m, kind = s.res, "compute#resourcePolicyAggregatedList"
	default:
		return nil, unimplemented("aggregated/%s is not served", coll)
	}
	f, err := parseFilter(first(c.query, "filter"))
	if err != nil {
		return nil, err
	}
	prefix := "projects/" + c.project + "/"
	scopes := map[string][]obj{}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.Split(k[len(prefix):], "/")
		var scope string
		switch {
		case len(rest) == 3 && rest[0] == "global" && rest[1] == coll:
			scope = "global"
		case len(rest) == 4 && (rest[0] == "regions" || rest[0] == "zones") && rest[2] == coll:
			scope = rest[0] + "/" + rest[1]
		default:
			continue
		}
		if f.match(m[k]) {
			o := m[k]
			if coll == "instances" {
				// As instances.get: with the SSH address a running
				// instance's port 22 is forwarded to.
				o = s.decorate(o)
			}
			scopes[scope] = append(scopes[scope], o)
		}
	}
	items := map[string]any{}
	for sc, l := range scopes {
		items[sc] = map[string]any{key: l}
	}
	return obj{"kind": kind, "id": "projects/" + c.project + "/aggregated/" + coll, "items": items}, nil
}

// newOpLocked records a finished operation on target in scope ("global",
// "regions/R" or "zones/Z") and returns it.
func (s *Server) newOpLocked(project, scope, opType, target string) obj {
	name := "operation-" + strconvU(uint64(s.opts.Now().UnixMilli())) + "-" + randHex(6) + "-" + randHex(4)
	rel := "projects/" + project + "/" + scope + "/operations/" + name
	now := s.stamp()
	op := obj{"kind": "compute#operation", "id": newID(), "name": name, "operationType": opType,
		"targetLink": target, "status": "DONE", "user": "cloudburrow@localhost", "progress": 0.0 + 100,
		"insertTime": now, "startTime": now, "endTime": now, "selfLink": rel}
	if t, ok := s.res[target]; ok {
		op["targetId"] = t["id"]
	}
	if strings.HasPrefix(scope, "regions/") {
		op["region"] = "projects/" + project + "/" + scope
	}
	if strings.HasPrefix(scope, "zones/") {
		op["zone"] = "projects/" + project + "/" + scope
	}
	s.ops[rel] = op
	return op
}

// done saves and returns the operation.
func (s *Server) done(project, scope, opType, target string, targetID any) (obj, error) {
	op := s.newOpLocked(project, scope, opType, target)
	if targetID != nil {
		op["targetId"] = targetID
	}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return op, nil
}

// norm is v as JSON decodes it (numbers float64, lists []any), so stored
// objects compare and filter alike whether made here or decoded.
func norm(v obj) obj {
	b, _ := json.Marshal(v)
	var out obj
	_ = json.Unmarshal(b, &out)
	return out
}

// str is o[k] as a string.
func str(o obj, k string) string {
	v, _ := o[k].(string)
	return v
}

// strs is o[k] as a list of strings.
func strs(o obj, k string) []string {
	l, _ := o[k].([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func lastSeg(rel string) string { return rel[strings.LastIndex(rel, "/")+1:] }

// opWaitTarget is the operation a zoneOperations.wait or
// regionOperations.wait call waits for, "" for any other call.
func opWaitTarget(c call) string {
	p := c.parts
	if c.method == http.MethodPost && len(p) == 5 && (p[0] == "zones" || p[0] == "regions") && p[2] == "operations" && p[4] == "wait" {
		return "projects/" + c.project + "/" + p[0] + "/" + p[1] + "/operations/" + p[3]
	}
	return ""
}
