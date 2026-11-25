package compute

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Instance groups (#2139): unmanaged instanceGroups
// (https://docs.cloud.google.com/compute/docs/instance-groups/creating-groups-of-unmanaged-instances),
// zonal managed instanceGroupManagers made from an instance template
// (https://docs.cloud.google.com/compute/docs/instance-groups/create-zonal-mig,
// read 2026-11-22), and regional ones, regionInstanceGroupManagers and
// regionInstanceGroups, whose VMs are zonal instances spread over the
// zones of their distributionPolicy
// (compute/docs/instance-groups/regional-mig-distribution-shape). A
// managed group's VMs are ordinary instances named BASE-XXXX, with the
// created-by and instance-template metadata Google gives them; resize adds
// or deletes VMs, recreateInstances re-creates a VM from the group's
// current template (its boot disk made again when the template is the
// one it was made from), and an autoscaler (autoscalers.go) sets the
// target size. Kubernetes Engine's node-pool groups are answered beside
// these (Options.Extra) by name.

// igLoc is where an instance group or manager lives: a zone, or a region.
type igLoc struct {
	project string
	// scope is "zones/Z" or "regions/R".
	scope        string
	zone, region string
}

func zoneLoc(project, zone string) igLoc {
	return igLoc{project: project, scope: "zones/" + zone, zone: zone, region: regionOfZone(zone)}
}

func regionLoc(project, region string) igLoc {
	return igLoc{project: project, scope: "regions/" + region, region: region}
}

func (l igLoc) regional() bool { return l.zone == "" }

func (l igLoc) coll(coll string) string { return "projects/" + l.project + "/" + l.scope + "/" + coll }

// field is the resource's location field: zone or region.
func (l igLoc) field() (string, string) {
	if l.regional() {
		return "region", "projects/" + l.project + "/regions/" + l.region
	}
	return "zone", "projects/" + l.project + "/zones/" + l.zone
}

func groupRel(project, zone, coll, name string) string {
	return zoneRel(project, zone, coll, name)
}

// groupMembersLocked are the instances an unmanaged group lists.
func groupMembersLocked(g obj) []string { return strs(g, "_members") }

// namedPortRE is a named port's name rule (RFC 1035, 1-63 characters).
var namedPortRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// checkNamedPorts are namedPorts[] as instanceGroups.setNamedPorts and
// inserts take them: each a name (RFC 1035) and a port in 1-65535.
func checkNamedPorts(field string, v any) ([]any, error) {
	out := []any{}
	for i, e := range listOf(v) {
		np, _ := e.(map[string]any)
		name := str(np, "name")
		if !namedPortRE.MatchString(name) {
			return nil, invalid("Invalid value for field '%s[%d].name': '%s'. Must be a match of regex '[a-z]([-a-z0-9]{0,61}[a-z0-9])?'", field, i, name)
		}
		var port float64
		switch p := np["port"].(type) {
		case float64:
			port = p
		case string:
			port, _ = strconv.ParseFloat(p, 64)
		}
		if port < 1 || port > 65535 || port != float64(int(port)) {
			return nil, invalid("Invalid value for field '%s[%d].port': '%v'. Must be greater than or equal to 1 and less than or equal to 65535.", field, i, np["port"])
		}
		out = append(out, map[string]any{"name": name, "port": port})
	}
	return out, nil
}

func (s *Server) instanceGroups(c call, l igLoc) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := l.coll("instanceGroups")
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 3 && get:
		return s.listOut("compute#"+map[bool]string{false: "instanceGroupList", true: "regionInstanceGroupList"}[l.regional()], coll, s.res, c.query)
	case n == 3 && post && !l.regional():
		return s.insertGroup(c, l.zone)
	}
	verbs := servedGroupVerbs
	if l.regional() {
		// regionInstanceGroups are the regional managers' groups: get,
		// list, listInstances and setNamedPorts.
		verbs = map[string]bool{"listInstances": true, "setNamedPorts": true}
	}
	if n < 4 || n > 5 || (n == 4 && !get && !(del && !l.regional())) || (n == 5 && (!post || !verbs[p[4]])) {
		return nil, unimplemented("%s on instanceGroups/%s is not served", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	g, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	verb := ""
	if n == 5 {
		verb = p[4]
	}
	switch {
	case n == 4 && get:
		return g, nil
	case n == 4 && del:
		if m := str(g, "_manager"); m != "" {
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The instance_group resource '%s' is already being used by '%s'", rel, m)
		}
		delete(s.res, rel)
		return s.done(c.project, l.scope, "delete", rel, g["id"])
	case post && verb == "listInstances":
		var items []obj
		state := strings.ToUpper(str(c.body, "instanceState"))
		for _, m := range s.groupInstancesLocked(g) {
			vm := s.res[m]
			if state == "RUNNING" && str(vm, "status") != stRunning {
				continue
			}
			it := obj{"instance": m, "status": str(vm, "status")}
			if np := listOf(g["namedPorts"]); len(np) > 0 {
				it["namedPorts"] = np
			}
			items = append(items, it)
		}
		if items == nil {
			items = []obj{}
		}
		return obj{"kind": "compute#" + map[bool]string{false: "instanceGroupsListInstances", true: "regionInstanceGroupsListInstances"}[l.regional()],
			"id": rel, "items": items, "selfLink": rel}, nil
	case post && (verb == "addInstances" || verb == "removeInstances"):
		if str(g, "_manager") != "" {
			return nil, invalid("Invalid value for field 'instanceGroup': '%s'. A managed instance group's instances are added and removed by its manager.", rel)
		}
		members := groupMembersLocked(g)
		var removed []string
		for i, e := range listOf(c.body["instances"]) {
			im, _ := e.(map[string]any)
			ir := Rel(str(im, "instance"), c.project)
			vm, ok := s.res[ir]
			if !ok || !strings.Contains(ir, "/instances/") {
				return nil, notFound(ir)
			}
			if verb == "addInstances" {
				if lastSeg(str(vm, "zone")) != l.zone {
					return nil, invalid("Invalid value for field 'resource.instances[%d].instance': '%s'. The instance must be in zone '%s'.", i, str(im, "instance"), l.zone)
				}
				if nw := str(g, "network"); nw != "" {
					if v, ok := vmNetOf(ir, vm); ok && v.network != nw {
						return nil, invalid("Invalid value for field 'resource.instances[%d].instance': '%s'. The instance must be in the group's network '%s'.", i, str(im, "instance"), lastSeg(nw))
					}
				}
				if hasStr(members, ir) {
					return nil, errf(http.StatusBadRequest, "memberAlreadyExists", "The resource '%s' is already a member of '%s'", ir, rel)
				}
				members = append(members, ir)
				continue
			}
			if hasStr(members, ir) {
				removed = append(removed, ir)
			}
		}
		if len(removed) > 0 {
			// Connection draining (#2226): a running member drains,
			// staying a member until its timeout passes.
			now := s.drainMembersLocked(rel, g, removed)
			var keep []string
			for _, m := range members {
				if !hasStr(now, m) {
					keep = append(keep, m)
				}
			}
			members = keep
		}
		if verb == "addInstances" && str(g, "network") == "" && len(members) > 0 {
			if v, ok := vmNetOf(members[0], s.res[members[0]]); ok {
				g["network"] = v.network
			}
		}
		g["_members"] = toAny(members)
		g["size"] = float64(len(members))
		g["fingerprint"] = newFingerprint()
		return s.done(c.project, l.scope, verb, rel, g["id"])
	case post && verb == "setNamedPorts":
		// The fingerprint, when given, must be the group's: a stale one
		// is refused, as Google refuses it, so two writers cannot each
		// overwrite the other's ports.
		if fp := str(c.body, "fingerprint"); fp != "" && fp != str(g, "fingerprint") {
			return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
				"Supplied fingerprint does not match current fingerprint.")
		}
		np, err := checkNamedPorts("namedPorts", c.body["namedPorts"])
		if err != nil {
			return nil, err
		}
		g["namedPorts"] = np
		g["fingerprint"] = newFingerprint()
		if m, ok := s.res[str(g, "_manager")]; ok {
			m["namedPorts"] = np
		}
		return s.done(c.project, l.scope, "setNamedPorts", rel, g["id"])
	}
	return nil, unimplemented("%s on instanceGroups/%s is not served", c.method, strings.Join(p[3:], "/"))
}

func (s *Server) insertGroup(c call, zone string) (obj, error) {
	name := str(c.body, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := groupRel(c.project, zone, "instanceGroups", name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	g := obj{"kind": "compute#instanceGroup", "id": newID(), "name": name, "zone": "projects/" + c.project + "/zones/" + zone,
		"selfLink": rel, "creationTimestamp": s.stamp(), "size": 0.0, "fingerprint": newFingerprint()}
	if d := str(c.body, "description"); d != "" {
		g["description"] = d
	}
	if _, ok := c.body["namedPorts"]; ok {
		np, err := checkNamedPorts("resource.namedPorts", c.body["namedPorts"])
		if err != nil {
			return nil, err
		}
		g["namedPorts"] = np
	}
	if nw := str(c.body, "network"); nw != "" {
		nr, ok := NetworkName(nw, c.project)
		if _, exists := s.res[nr]; !ok || !exists {
			return nil, notFound(nr)
		}
		g["network"] = nr
	}
	s.res[rel] = norm(g)
	return s.done(c.project, "zones/"+zone, "insert", rel, g["id"])
}

// groupInstancesLocked are an instance group's instances: an unmanaged
// group's members, or its manager's VMs.
func (s *Server) groupInstancesLocked(g obj) []string {
	if m := str(g, "_manager"); m != "" {
		return s.managedLocked(m)
	}
	var out []string
	for _, m := range groupMembersLocked(g) {
		if _, ok := s.res[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// managedLocked are the VMs a manager made, by name, those it is deleting
// included.
func (s *Server) managedLocked(mgr string) []string {
	var out []string
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		for _, it := range listOf(vmMetadataItems(o)) {
			im, _ := it.(map[string]any)
			if str(im, "key") == "created-by" && str(im, "value") == mgr {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// liveManagedLocked are a manager's VMs less those it is deleting.
func (s *Server) liveManagedLocked(mgr string) []string {
	var out []string
	for _, vm := range s.managedLocked(mgr) {
		if s.res[vm]["_deleting"] != true {
			out = append(out, vm)
		}
	}
	return out
}

func vmMetadataItems(o obj) any {
	md, _ := o["metadata"].(map[string]any)
	return md["items"]
}

// --- managed instance groups ----------------------------------------------------

// managerZones are the zones a manager makes VMs in: its zone, or its
// distributionPolicy's.
func managerZones(m obj) []string {
	if z := str(m, "zone"); z != "" {
		return []string{lastSeg(z)}
	}
	dp, _ := m["distributionPolicy"].(map[string]any)
	var out []string
	for _, e := range listOf(dp["zones"]) {
		zm, _ := e.(map[string]any)
		out = append(out, lastSeg(str(zm, "zone")))
	}
	return out
}

func targetShapeOf(m obj) string {
	dp, _ := m["distributionPolicy"].(map[string]any)
	if t := str(dp, "targetShape"); t != "" {
		return t
	}
	return "EVEN"
}

// mgrAction marks a managed VM with the action its manager is taking on
// it, shown as listManagedInstances' currentAction until the VM's
// operations are done.
func mgrAction(o obj, action string) { o["_mgrAction"] = action }

func (s *Server) instanceGroupManagers(c call, l igLoc) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := l.coll("instanceGroupManagers")
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 3 && get:
		out, err := s.listOut("compute#"+map[bool]string{false: "instanceGroupManagerList", true: "regionInstanceGroupManagerList"}[l.regional()], coll, s.res, c.query)
		if err == nil {
			items := out["items"].([]obj)
			for _, e := range items {
				s.refreshManagerLocked(e)
			}
			if s.opts.ExtraGroups != nil && first(c.query, "filter") == "" && !l.regional() {
				// Kubernetes Engine's node-pool groups (#1913).
				extra := s.opts.ExtraGroups(c.project, l.zone)
				names := make([]string, 0, len(extra))
				for n := range extra {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					items = append(items, obj(extra[n]))
				}
				out["items"] = items
			}
		}
		return out, err
	case n == 3 && post:
		return s.insertManager(c, l)
	}
	if n < 4 || n > 5 || (n == 4 && !get && !del && c.method != http.MethodPatch) ||
		(n == 5 && p[4] == "listErrors" && !get) || (n == 5 && p[4] != "listErrors" && (!post || !servedManagerVerbs[p[4]])) {
		return nil, unimplemented("%s on instanceGroupManagers/%s is not served", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	m, ok := s.res[rel]
	if !ok {
		if s.opts.ExtraGroups != nil && !l.regional() {
			if g := s.opts.ExtraGroups(c.project, l.zone)[p[3]]; g != nil && n == 4 && get {
				return obj(g), nil
			}
		}
		return nil, notFound(rel)
	}
	verb := ""
	if n == 5 {
		verb = p[4]
	}
	switch {
	case n == 4 && get:
		s.refreshManagerLocked(m)
		return m, nil
	case n == 4 && c.method == http.MethodPatch:
		// Versions and updatePolicy: rolling updates (rollingupdates.go).
		return s.patchManagerLocked(c, l, rel, m)
	case get && verb == "listErrors":
		return s.listErrorsLocked(c, m)
	case post && verb == "applyUpdatesToInstances":
		return s.applyUpdatesLocked(c, l, rel, m)
	case n == 4 && del:
		for _, vm := range s.managedLocked(rel) {
			if o := s.res[vm]; o["deletionProtection"] == true {
				return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
					"Invalid resource usage: 'Resource cannot be deleted if it's protected against deletion.'.")
			}
		}
		for _, vm := range s.liveManagedLocked(rel) {
			if err := s.deleteManagedNowLocked(c.project, vm); err != nil {
				return nil, err
			}
		}
		delete(s.res, str(m, "instanceGroup"))
		delete(s.res, rel)
		return s.done(c.project, l.scope, "delete", rel, m["id"])
	case post && verb == "resize":
		size, err := strconv.Atoi(first(c.query, "size"))
		if err != nil || size < 0 {
			return nil, invalid("Invalid value for field 'size': '%s'. Must be a non-negative integer.", first(c.query, "size"))
		}
		if a := s.autoscalerOfLocked(rel); a != nil && l.regional() && autoscalerMode(a) != "OFF" {
			return nil, invalid("Resizing of autoscaled regional managed instance groups is not allowed.")
		}
		// A zonal group's autoscaler sets its size again at its next
		// pass, as Google's does.
		if workloadPolicyOf(m) != "" {
			if err := s.checkWorkloadTemplatesLocked(l.project, l.region, managerTemplates(m), size); err != nil {
				return nil, err
			}
		}
		m["targetSize"] = float64(size)
		return s.reconcileManagerLocked(l, rel, m, "resize")
	case post && verb == "setInstanceTemplate":
		// The template new VMs are made from; the group's VMs keep theirs
		// until they are recreated (or an update replaces them), as
		// Google's setInstanceTemplate does.
		t := Rel(str(c.body, "instanceTemplate"), c.project)
		if _, ok := s.res[t]; !ok || !strings.Contains(t, "/instanceTemplates/") {
			return nil, notFound(t)
		}
		if r := lastSeg(str(s.res[t], "region")); r != "" && r != l.region {
			return nil, invalid("Invalid value for field 'resource.instanceTemplate': '%s'. A regional instance template makes VMs of its region only.", str(c.body, "instanceTemplate"))
		}
		if workloadPolicyOf(m) != "" {
			if err := s.checkWorkloadTemplatesLocked(l.project, l.region, []string{t}, int(m["targetSize"].(float64))); err != nil {
				return nil, err
			}
		}
		m["instanceTemplate"] = t
		m["versions"] = []any{obj{"instanceTemplate": t, "targetSize": obj{"calculated": m["targetSize"]}}}
		m["fingerprint"] = newFingerprint()
		return s.done(c.project, l.scope, "setInstanceTemplate", rel, m["id"])
	case post && verb == "listManagedInstances":
		var items []any
		for _, vm := range s.managedLocked(rel) {
			o := s.res[vm]
			mi := obj{"instance": vm, "id": o["id"], "name": o["name"], "instanceStatus": str(o, "status"),
				"currentAction": s.currentActionLocked(vm, o),
				"version":       obj{"instanceTemplate": instanceTemplateOf(o)}}
			if la := lastAttemptOf(pendingEntry(m, vm)); la != nil {
				mi["lastAttempt"] = la
			}
			if ih := s.instanceHealthLocked(m, vm); ih != nil {
				mi["instanceHealth"] = ih
			}
			items = append(items, mi)
		}
		// The VMs the group has yet to make: no id or status while they
		// do not exist, their last attempt's errors (#2220).
		var pending []string
		for vm := range pendingOf(m) {
			if _, made := s.res[vm]; !made {
				pending = append(pending, vm)
			}
		}
		sort.Strings(pending)
		for _, vm := range pending {
			items = append(items, pendingManagedInstance(vm, pendingEntry(m, vm)))
		}
		if items == nil {
			items = []any{}
		}
		return obj{"managedInstances": items}, nil
	case post && (verb == "startInstances" || verb == "stopInstances" || verb == "suspendInstances" || verb == "resumeInstances"):
		targets, err := s.managedTargetsLocked(c, rel)
		if err != nil {
			return nil, err
		}
		iverb := strings.TrimSuffix(verb, "Instances")
		action := map[string]string{"start": "STARTING", "stop": "STOPPING", "suspend": "SUSPENDING", "resume": "RESUMING"}[iverb]
		for _, ir := range targets {
			z := lastSeg(str(s.res[ir], "zone"))
			if _, err := s.instances(call{method: http.MethodPost, project: c.project, parts: []string{"zones", z, "instances", lastSeg(ir), iverb}, body: obj{}}, z); err != nil {
				return nil, err
			}
			mgrAction(s.res[ir], action)
		}
		return s.done(c.project, l.scope, verb, rel, m["id"])
	case post && (verb == "recreateInstances" || verb == "deleteInstances"):
		targets, err := s.managedTargetsLocked(c, rel)
		if verb == "deleteInstances" {
			// A VM the group is still making can be deleted too: its
			// creation stops (#2220).
			targets, err = s.memberTargetsLocked(c, rel, m)
		}
		if err != nil {
			return nil, err
		}
		for _, vm := range targets {
			o := s.res[vm]
			if verb == "deleteInstances" {
				if err := s.removeMemberLocked(c.project, m, vm); err != nil {
					return nil, err
				}
				continue
			}
			if err := s.recreateManagedLocked(c.project, rel, m, vm, o); err != nil {
				return nil, err
			}
		}
		if verb == "deleteInstances" {
			size, _ := m["targetSize"].(float64)
			m["targetSize"] = max(0, size-float64(len(targets)))
			if g, ok := s.res[str(m, "instanceGroup")]; ok {
				g["size"] = m["targetSize"]
			}
		}
		return s.done(c.project, l.scope, verb, rel, m["id"])
	}
	return nil, unimplemented("%s on instanceGroupManagers/%s is not served", c.method, strings.Join(p[3:], "/"))
}

// managedTargetsLocked are a manager verb's instances[], each a VM of the
// manager's.
func (s *Server) managedTargetsLocked(c call, rel string) ([]string, error) {
	return s.targetsInLocked(c, rel, s.liveManagedLocked(rel))
}

// memberTargetsLocked are a manager verb's instances[], each a VM of the
// manager's or one it is still making.
func (s *Server) memberTargetsLocked(c call, rel string, m obj) ([]string, error) {
	return s.targetsInLocked(c, rel, s.membersLocked(rel, m))
}

func (s *Server) targetsInLocked(c call, rel string, live []string) ([]string, error) {
	var targets []string
	for _, e := range strsAny(c.body["instances"]) {
		ir := Rel(e, c.project)
		if !hasStr(live, ir) {
			return nil, invalid("Invalid value for field 'instances': '%s'. The instance is not managed by '%s'.", e, rel)
		}
		targets = append(targets, ir)
	}
	if len(targets) == 0 {
		return nil, required("instances")
	}
	return targets, nil
}

// currentActionLocked is a managed VM's currentAction: VERIFYING during
// its autohealing initial delay (#2225).
func (s *Server) currentActionLocked(vm string, o obj) string {
	a := s.baseActionLocked(vm, o)
	if a == "NONE" && o["_ahVerifying"] == true {
		return "VERIFYING"
	}
	return a
}

// baseActionLocked is a managed VM's currentAction, autohealing's
// verification aside.
func (s *Server) baseActionLocked(vm string, o obj) string {
	if s.queued[vm] == 0 && s.res[vm] != nil {
		delete(o, "_mgrAction")
	}
	switch {
	case o["_drain"] != nil:
		// Draining before its manager deletes or recreates it (#2226).
		return str(drainOf(o), "action")
	case o["_deleting"] == true:
		return "DELETING"
	case str(o, "_mgrAction") != "":
		return str(o, "_mgrAction")
	case str(o, "status") == stProvisioning || str(o, "status") == "STAGING":
		return "CREATING"
	}
	return "NONE"
}

var servedGroupVerbs = map[string]bool{"addInstances": true, "removeInstances": true, "listInstances": true, "setNamedPorts": true}

var servedManagerVerbs = map[string]bool{"resize": true, "setInstanceTemplate": true, "listManagedInstances": true,
	"recreateInstances": true, "deleteInstances": true, "startInstances": true, "stopInstances": true,
	"suspendInstances": true, "resumeInstances": true, "applyUpdatesToInstances": true}

func instanceTemplateOf(o obj) string {
	for _, it := range listOf(vmMetadataItems(o)) {
		im, _ := it.(map[string]any)
		if str(im, "key") == "instance-template" {
			return str(im, "value")
		}
	}
	return ""
}

// targetShapes are distributionPolicy.targetShape's values.
var targetShapes = map[string]bool{"EVEN": true, "BALANCED": true, "ANY": true, "ANY_SINGLE_ZONE": true}

func (s *Server) insertManager(c call, l igLoc) (obj, error) {
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := l.coll("instanceGroupManagers") + "/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	tref := str(b, "instanceTemplate")
	if versions := listOf(b["versions"]); tref == "" && len(versions) > 0 {
		v, _ := versions[0].(map[string]any)
		tref = str(v, "instanceTemplate")
	}
	if tref == "" {
		return nil, required("resource.instanceTemplate")
	}
	t := Rel(tref, c.project)
	if _, ok := s.res[t]; !ok || !strings.Contains(t, "/instanceTemplates/") {
		return nil, notFound(t)
	}
	if r := lastSeg(str(s.res[t], "region")); r != "" && r != l.region {
		return nil, invalid("Invalid value for field 'resource.instanceTemplate': '%s'. A regional instance template makes VMs of its region only.", tref)
	}
	healing, err := s.checkAutoHealingPoliciesLocked(c.project, l.region, b["autoHealingPolicies"])
	if err != nil {
		return nil, err
	}
	base := str(b, "baseInstanceName")
	if base == "" {
		base = name
	}
	if len(base) > 58 {
		return nil, invalid("Invalid value for field 'resource.baseInstanceName': '%s'. Must be at most 58 characters.", base)
	}
	size, _ := b["targetSize"].(float64)
	if size < 0 {
		return nil, invalid("Invalid value for field 'resource.targetSize': '%v'. Must be non-negative.", size)
	}
	group := l.coll("instanceGroups") + "/" + name
	if _, ok := s.res[group]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", group)
	}
	locKey, locVal := l.field()
	m := obj{"kind": "compute#instanceGroupManager", "id": newID(), "name": name, locKey: locVal,
		"selfLink": rel, "creationTimestamp": s.stamp(), "baseInstanceName": base, "instanceTemplate": t, "instanceGroup": group,
		"targetSize": size, "fingerprint": newFingerprint(), "listManagedInstancesResults": "PAGELESS",
		"versions":     []any{obj{"instanceTemplate": t, "targetSize": obj{"calculated": size}}},
		"updatePolicy": obj{"type": "OPPORTUNISTIC", "minimalAction": "REPLACE", "replacementMethod": "SUBSTITUTE"}}
	if l.regional() {
		dp, err := checkDistributionPolicy(b, l.region)
		if err != nil {
			return nil, err
		}
		m["distributionPolicy"] = dp
		redist := "PROACTIVE"
		if str(dp, "targetShape") != "EVEN" {
			redist = "NONE"
		}
		up, _ := b["updatePolicy"].(map[string]any)
		if r := str(up, "instanceRedistributionType"); r != "" {
			if r != "PROACTIVE" && r != "NONE" {
				return nil, invalid("Invalid value for field 'resource.updatePolicy.instanceRedistributionType': '%s'. Must be PROACTIVE or NONE.", r)
			}
			if r == "PROACTIVE" && str(dp, "targetShape") != "EVEN" {
				return nil, invalid("Invalid value for field 'resource.updatePolicy.instanceRedistributionType': 'PROACTIVE'. Proactive instance redistribution is supported only with the EVEN target distribution shape: set it to NONE.")
			}
			redist = r
		}
		m["updatePolicy"].(obj)["instanceRedistributionType"] = redist
	}
	if d := str(b, "description"); d != "" {
		m["description"] = d
	}
	if wp, ok, err := s.checkManagerWorkloadLocked(l, b, m, false, 0); err != nil {
		return nil, err
	} else if ok {
		if err := s.checkWorkloadTemplatesLocked(l.project, l.region, managerTemplates(m), int(size)); err != nil {
			return nil, err
		}
		m["resourcePolicies"] = obj{"workloadPolicy": wp}
	}
	if len(healing) > 0 {
		m["autoHealingPolicies"] = healing
	}
	g := obj{"kind": "compute#instanceGroup", "id": newID(), "name": name, locKey: locVal, "selfLink": group,
		"creationTimestamp": s.stamp(), "size": size, "fingerprint": newFingerprint(), "_manager": rel}
	if _, ok := b["namedPorts"]; ok {
		np, err := checkNamedPorts("resource.namedPorts", b["namedPorts"])
		if err != nil {
			return nil, err
		}
		m["namedPorts"], g["namedPorts"] = np, np
	}
	s.res[rel] = norm(m)
	s.res[group] = norm(g)
	return s.reconcileManagerLocked(l, rel, s.res[rel], "insert")
}

// checkDistributionPolicy is a regional manager's distributionPolicy:
// zones of its region (three of them when none are named, as Google
// picks), and the target shape, EVEN by default.
func checkDistributionPolicy(b obj, region string) (obj, error) {
	dp, _ := b["distributionPolicy"].(map[string]any)
	var zones []any
	seen := map[string]bool{}
	for i, e := range listOf(dp["zones"]) {
		zm, _ := e.(map[string]any)
		z := lastSeg(str(zm, "zone"))
		if !IsZone(z) || regionOfZone(z) != region {
			return nil, invalid("Invalid value for field 'resource.distributionPolicy.zones[%d].zone': '%s'. Zone must be in region '%s'.", i, str(zm, "zone"), region)
		}
		if seen[z] {
			return nil, invalid("Invalid value for field 'resource.distributionPolicy.zones[%d].zone': '%s'. Zones must be unique.", i, str(zm, "zone"))
		}
		seen[z] = true
		zones = append(zones, obj{"zone": "zones/" + z})
	}
	if len(zones) == 0 {
		all := Zones(region)
		for _, z := range all[:min(3, len(all))] {
			zones = append(zones, obj{"zone": "zones/" + z})
		}
	}
	shape := str(dp, "targetShape")
	if shape == "" {
		shape = "EVEN"
	}
	if !targetShapes[shape] {
		return nil, invalid("Invalid value for field 'resource.distributionPolicy.targetShape': '%s'. Must be one of EVEN, BALANCED, ANY or ANY_SINGLE_ZONE.", shape)
	}
	return obj{"zones": zones, "targetShape": shape}, nil
}

// nextZoneLocked is the zone a manager's next VM is made in: for a
// regional group the zone of its distribution with the fewest of its VMs
// (EVEN, BALANCED and ANY spread alike here, every zone having the
// capacity), or, for ANY_SINGLE_ZONE, the zone its VMs are already in.
func (s *Server) nextZoneLocked(m obj, have []string) string {
	zones := managerZones(m)
	count := map[string]int{}
	for _, vm := range have {
		count[zoneOfRel(vm)]++
	}
	if targetShapeOf(m) == "ANY_SINGLE_ZONE" {
		for _, z := range zones {
			if count[z] > 0 {
				return z
			}
		}
		return zones[0]
	}
	best := zones[0]
	for _, z := range zones[1:] {
		if count[z] < count[best] {
			best = z
		}
	}
	return best
}

// victimLocked is the VM a resize down deletes first: one the group has
// not yet made, else the newest, of the zone with the most of the
// group's VMs for a regional group.
func (s *Server) victimLocked(m obj, have []string) string {
	for _, vm := range have {
		if _, made := s.res[vm]; !made {
			return vm
		}
	}
	count := map[string]int{}
	for _, vm := range have {
		count[zoneOfRel(vm)]++
	}
	sorted := append([]string(nil), have...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := s.res[sorted[i]], s.res[sorted[j]]
		if ca, cb := count[zoneOfRel(sorted[i])], count[zoneOfRel(sorted[j])]; ca != cb && str(m, "region") != "" {
			return ca > cb
		}
		if ta, tb := str(a, "creationTimestamp"), str(b, "creationTimestamp"); ta != tb {
			return ta > tb
		}
		return sorted[i] > sorted[j]
	})
	return sorted[0]
}

// insertManagedVMFromLocked starts making one of a manager's VMs from
// template tpl; it returns the insert operation's URL. Callers go through
// createManagedLocked, which retries a failed creation (#2220).
func (s *Server) insertManagedVMFromLocked(project, zone, rel string, m obj, name, tpl string) (string, error) {
	props, _ := s.res[tpl]["properties"].(map[string]any)
	md, _ := deepCopy(props["metadata"]).(map[string]any)
	if md == nil {
		md = map[string]any{}
	}
	items := listOf(md["items"])
	items = append(items, map[string]any{"key": "created-by", "value": rel}, map[string]any{"key": "instance-template", "value": tpl})
	md["items"] = items
	op, err := s.insertInstance(call{method: http.MethodPost, project: project, parts: []string{"zones", zone, "instances"},
		query: map[string][]string{"sourceInstanceTemplate": {tpl}}, body: obj{"name": name, "metadata": md}}, zone)
	if err != nil {
		return "", err
	}
	if o := s.res[zoneRel(project, zone, "instances", name)]; o != nil {
		mgrAction(o, "CREATING")
	}
	return str(op, "selfLink"), nil
}

// removeMemberLocked deletes one of a manager's members: its VM, or, for
// one not yet made, its pending creation.
func (s *Server) removeMemberLocked(project string, m obj, vm string) error {
	dropPendingLocked(m, vm)
	if _, ok := s.res[vm]; !ok {
		return nil
	}
	return s.deleteManagedLocked(project, vm)
}

// deleteManagedLocked deletes one of a manager's VMs, once it has drained
// (#2226): DELETING meanwhile.
func (s *Server) deleteManagedLocked(project, vm string) error {
	o := s.res[vm]
	if s.beginDrainLocked(vm, o, "DELETING", obj{"do": "delete"}) {
		o["_deleting"] = true
		return nil
	}
	return s.deleteManagedNowLocked(project, vm)
}

// deleteManagedNowLocked deletes one of a manager's VMs at once.
func (s *Server) deleteManagedNowLocked(project, vm string) error {
	o := s.res[vm]
	if _, err := s.deleteInstance(project, "zones/"+lastSeg(str(o, "zone")), vm, o); err != nil {
		return err
	}
	o["_deleting"] = true
	return nil
}

// reconcileManagerLocked adds VMs up to the target size, or deletes the
// newest beyond it.
func (s *Server) reconcileManagerLocked(l igLoc, rel string, m obj, opType string) (obj, error) {
	if err := s.resizeToLocked(l.project, rel, m); err != nil {
		return nil, err
	}
	return s.done(l.project, l.scope, opType, rel, m["id"])
}

// resizeToLocked makes or deletes a manager's VMs until it has its
// targetSize.
func (s *Server) resizeToLocked(project, rel string, m obj) error {
	have := s.membersLocked(rel, m)
	want := int(m["targetSize"].(float64))
	for len(have) < want {
		name := str(m, "baseInstanceName") + "-" + randLower(4)
		zone := s.nextZoneLocked(m, have)
		vmRel := zoneRel(project, zone, "instances", name)
		if _, taken := s.res[vmRel]; taken || pendingEntry(m, vmRel) != nil {
			continue
		}
		// A creation that fails is retried (#2220), so the resize
		// itself succeeds, as Google's does.
		s.createManagedLocked(project, rel, m, zone, name, s.newVMTemplateLocked(rel, m), "CREATING")
		have = append(have, vmRel)
	}
	for len(have) > want {
		vm := s.victimLocked(m, have)
		if err := s.removeMemberLocked(project, m, vm); err != nil {
			return err
		}
		var keep []string
		for _, h := range have {
			if h != vm {
				keep = append(keep, h)
			}
		}
		have = keep
	}
	if g, ok := s.res[str(m, "instanceGroup")]; ok {
		g["size"] = float64(want)
	}
	return nil
}

// refreshManagerLocked sets a manager's currentActions and status from its
// VMs.
func (s *Server) refreshManagerLocked(m obj) {
	acts := obj{"none": 0.0, "creating": 0.0, "deleting": 0.0, "recreating": 0.0, "restarting": 0.0, "verifying": 0.0,
		"abandoning": 0.0, "creatingWithoutRetries": 0.0, "refreshing": 0.0, "resuming": 0.0, "starting": 0.0,
		"stopping": 0.0, "suspending": 0.0}
	key := map[string]string{"NONE": "none", "CREATING": "creating", "DELETING": "deleting", "RECREATING": "recreating",
		"STARTING": "starting", "STOPPING": "stopping", "SUSPENDING": "suspending", "RESUMING": "resuming",
		"VERIFYING": "verifying", "RESTARTING": "restarting", "REFRESHING": "refreshing", "ABANDONING": "abandoning"}
	stable := true
	rel := str(m, "selfLink")
	vms := s.managedLocked(rel)
	live := 0
	for _, vm := range vms {
		o := s.res[vm]
		a := s.currentActionLocked(vm, o)
		if a != "NONE" {
			stable = false
		}
		if a != "DELETING" {
			live++
		}
		if k := key[a]; k != "" {
			acts[k] = acts[k].(float64) + 1
		}
	}
	// The VMs it is still making, or re-making (#2220).
	for vm, v := range pendingOf(m) {
		if _, made := s.res[vm]; made {
			continue
		}
		e, _ := v.(map[string]any)
		k := key[str(e, "action")]
		if k == "" {
			k = "creating"
		}
		acts[k] = acts[k].(float64) + 1
		stable = false
		live++
	}
	if float64(live) != m["targetSize"] {
		stable = false
	}
	m["currentActions"] = acts
	up, _ := m["updatePolicy"].(map[string]any)
	reached := stable
	if out, _, _ := s.outdatedLocked(rel, m, "NONE", "REPLACE"); len(out) > 0 {
		reached = false
		if str(up, "type") == "PROACTIVE" {
			stable = false
		}
	}
	st := obj{"isStable": stable, "versionTarget": obj{"isReached": reached}, "allInstancesConfig": obj{"effective": true}}
	if a := s.autoscalerOfLocked(rel); a != nil {
		st["autoscaler"] = str(a, "selfLink")
	}
	m["status"] = st
}

// recreateManagedLocked re-creates a managed VM from its group's current
// template: a VM made from that template has its boot disk made again and
// boots again, its name and address kept; one made from another template
// is deleted and made again, by the same name, from the current one.
func (s *Server) recreateManagedLocked(project, mgr string, m obj, vm string, o obj) error {
	tpl := s.desiredTemplatesLocked(m, s.liveManagedLocked(mgr))[vm]
	if tpl == "" {
		tpl = str(m, "instanceTemplate")
	}
	return s.recreateManagedFromLocked(project, mgr, m, vm, o, tpl)
}

// recreateManagedFromLocked re-creates a managed VM from template tpl, by
// the same name.
func (s *Server) recreateManagedFromLocked(project, mgr string, m obj, vm string, o obj, tpl string) error {
	if s.beginDrainLocked(vm, o, "RECREATING", obj{"do": "recreate", "template": tpl}) {
		return nil // once it has drained (#2226)
	}
	return s.recreateManagedFromNowLocked(project, mgr, m, vm, o, tpl)
}

// recreateManagedFromNowLocked re-creates a managed VM from template tpl
// at once.
func (s *Server) recreateManagedFromNowLocked(project, mgr string, m obj, vm string, o obj, tpl string) error {
	if instanceTemplateOf(o) == tpl {
		if _, err := s.recreateLocked(project, "zones/"+lastSeg(str(o, "zone")), vm, o); err != nil {
			return err
		}
		mgrAction(o, "RECREATING")
		return nil
	}
	return s.remakeManagedNowLocked(project, mgr, m, vm, o, tpl)
}

// remakeManagedLocked deletes a managed VM and makes it again, by the same
// name, from template tpl: a pending member, listed RECREATING, until it is
// made (its creation retried while it fails, #2220). Autohealing recreates
// VMs this way (#2225).
func (s *Server) remakeManagedLocked(project, mgr string, m obj, vm string, o obj, tpl string) error {
	if s.beginDrainLocked(vm, o, "RECREATING", obj{"do": "remake", "template": tpl}) {
		return nil // once it has drained (#2226)
	}
	return s.remakeManagedNowLocked(project, mgr, m, vm, o, tpl)
}

// remakeManagedNowLocked is remakeManagedLocked at once.
func (s *Server) remakeManagedNowLocked(project, mgr string, m obj, vm string, o obj, tpl string) error {
	zone, name := lastSeg(str(o, "zone")), str(o, "name")
	op, err := s.deleteInstance(project, "zones/"+zone, vm, o)
	if err != nil {
		return err
	}
	mgrAction(o, "RECREATING")
	opRel := str(op, "selfLink")
	// The group is re-making it: a pending member until it is made
	// (#2220), listed RECREATING.
	if pendingEntry(m, vm) == nil {
		if m["_pending"] == nil {
			m["_pending"] = map[string]any{}
		}
		pendingOf(m)[vm] = map[string]any{"zone": zone, "name": name, "action": "RECREATING", "template": tpl, "inFlight": true}
	}
	// Until it is made again, the VM is pending (queued under its own
	// name), so no loop takes the group for settled and short of a VM
	// in between and makes one of a new name.
	s.queued[vm]++
	go func() {
		s.waitOp(context.Background(), opRel)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer func() {
			if s.queued[vm]--; s.queued[vm] <= 0 {
				delete(s.queued, vm)
			}
		}()
		cur, ok := s.res[mgr]
		if !ok || pendingEntry(cur, vm) == nil {
			return
		}
		if _, taken := s.res[vm]; taken {
			// Its deletion failed: the VM stays, and is no longer pending.
			dropPendingLocked(cur, vm)
			return
		}
		s.createManagedLocked(project, mgr, cur, zone, name, tpl, "RECREATING")
		_ = s.saveLocked()
	}()
	return nil
}

// recreateLocked re-creates a managed VM: its boot disk is made again from
// its image, and it boots again, its name and address kept.
func (s *Server) recreateLocked(project, scope, rel string, o obj) (obj, error) {
	vm := s.vmSpecLocked(o)
	boot := vm.BootDisk
	o["status"] = stStopping
	return s.runOpLocked(project, scope, "recreate", rel, func(ctx context.Context) error {
		if err := s.opts.VMs.Stop(ctx, vm); err != nil {
			return err
		}
		if err := s.opts.VMs.DeleteDisk(ctx, boot); err != nil {
			return err
		}
		fresh := boot
		fresh.SourceSnapshot = ""
		if err := s.opts.VMs.CreateDisk(ctx, fresh); err != nil {
			return err
		}
		return s.opts.VMs.Start(ctx, vm)
	}, func(err error) {
		if cur, ok := s.res[rel]; ok {
			if err != nil {
				cur["status"] = stTerminated
				return
			}
			cur["status"], cur["lastStartTimestamp"] = stRunning, s.stamp()
		}
	})
}

func randLower(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	h := randHex(n)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		v, _ := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		out[i] = alpha[int(v)%len(alpha)]
	}
	return string(out)
}
