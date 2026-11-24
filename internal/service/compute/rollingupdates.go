package compute

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Rolling updates of managed instance groups (#2139):
// instanceGroupManagers.patch and regionInstanceGroupManagers.patch with
// versions and updatePolicy, and applyUpdatesToInstances, with Google's
// rules (https://docs.cloud.google.com/compute/docs/instance-groups/
// rolling-out-updates-to-managed-instance-groups, updating-selected-
// instances and the REST reference's InstanceGroupManagerUpdatePolicy,
// read 2026-11-22):
//
//   - versions: one or two, each an instance template; with two (a canary)
//     exactly one has no targetSize and takes the rest of the group's VMs,
//     the other a fixed count or a percent of the target size;
//   - updatePolicy.type PROACTIVE (the console's Automatic) brings every VM
//     to its version at once, within maxSurge (VMs made above the target
//     size) and maxUnavailable (VMs not running), fixed or percent, by
//     default 1 each in a zonal group and the number of zones in a regional
//     one, a regional group's fixed values 0 or at least its number of
//     zones; OPPORTUNISTIC (Selective) leaves the VMs as they are until
//     they are recreated or applyUpdatesToInstances is called on them;
//   - the action on a VM is the more disruptive of minimalAction and what
//     the update requires: REFRESH when only the template's metadata
//     differs (applied in place), REPLACE otherwise; a VM needing more than
//     mostDisruptiveAllowedAction is left as it is; RESTART restarts it;
//   - replacementMethod SUBSTITUTE replaces a VM with a new one of a new
//     name, RECREATE keeps its name and needs maxSurge 0;
//   - minimalAction no more disruptive than mostDisruptiveAllowedAction.
//
// The update is carried out by rollOnce, run after the patch and then every
// UpdateEvery by the server's loop: one step each time the group's VMs are
// settled (no operation running, none deleting, all RUNNING).

var actionRank = map[string]int{"NONE": 0, "REFRESH": 1, "RESTART": 2, "REPLACE": 3}

// fixedOrPercent reads a FixedOrPercent field: its calculated value of n.
func fixedOrPercent(field string, v any, n int) (obj, int, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, 0, invalid("Invalid value for field '%s': it must be an object with fixed or percent.", field)
	}
	f, hasF := m["fixed"].(float64)
	pc, hasP := m["percent"].(float64)
	switch {
	case hasF && hasP:
		return nil, 0, invalid("Invalid value for field '%s': only one of fixed and percent may be set.", field)
	case hasF:
		if f < 0 || f != math.Trunc(f) {
			return nil, 0, invalid("Invalid value for field '%s.fixed': %v. Must be a non-negative integer.", field, f)
		}
		return obj{"fixed": f, "calculated": f}, int(f), nil
	case hasP:
		if pc < 0 || pc > 100 || pc != math.Trunc(pc) {
			return nil, 0, invalid("Invalid value for field '%s.percent': %v. Must be an integer from 0 to 100.", field, pc)
		}
		c := int(math.Ceil(pc * float64(n) / 100))
		return obj{"percent": pc, "calculated": float64(c)}, c, nil
	}
	return nil, 0, invalid("Invalid value for field '%s': set fixed or percent.", field)
}

// versionsOf are a manager's versions, the first the one without a target
// size when there are two.
func versionsOf(m obj) []obj {
	var out []obj
	for _, e := range listOf(m["versions"]) {
		if v, ok := e.(map[string]any); ok {
			out = append(out, obj(v))
		}
	}
	return out
}

// checkVersionsLocked validates a patch's versions, returning them with
// their calculated sizes for targetSize.
func (s *Server) checkVersionsLocked(project, region string, raw any, size int) ([]any, error) {
	list := listOf(raw)
	if len(list) == 0 || len(list) > 2 {
		return nil, invalid("Invalid value for field 'resource.versions': a managed instance group has one or two versions, not %d.", len(list))
	}
	var out []any
	unsized, sized := 0, 0
	names := map[string]bool{}
	for i, e := range list {
		v, _ := e.(map[string]any)
		tref := str(v, "instanceTemplate")
		if tref == "" {
			return nil, required("resource.versions[" + itoa(i) + "].instanceTemplate")
		}
		t := Rel(tref, project)
		if _, ok := s.res[t]; !ok || !strings.Contains(t, "/instanceTemplates/") {
			return nil, notFound(t)
		}
		if r := lastSeg(str(s.res[t], "region")); r != "" && r != region {
			return nil, invalid("Invalid value for field 'resource.versions[%d].instanceTemplate': '%s'. A regional instance template makes VMs of its region only.", i, tref)
		}
		nv := obj{"instanceTemplate": t}
		if n := str(v, "name"); n != "" {
			if names[n] {
				return nil, invalid("Invalid value for field 'resource.versions[%d].name': '%s'. Version names must be unique.", i, n)
			}
			names[n] = true
			nv["name"] = n
		}
		if ts, ok := v["targetSize"]; ok && ts != nil {
			if len(list) == 1 {
				return nil, invalid("Invalid value for field 'resource.versions[0].targetSize': a group's only version takes all its VMs and has no target size.")
			}
			fp, c, err := fixedOrPercent("resource.versions["+itoa(i)+"].targetSize", ts, size)
			if err != nil {
				return nil, err
			}
			if c > size {
				c = size
				fp["calculated"] = float64(c)
			}
			nv["targetSize"] = fp
			sized += c
		} else {
			unsized++
		}
		out = append(out, nv)
	}
	if unsized != 1 {
		return nil, invalid("Invalid value for field 'resource.versions': exactly one version must leave targetSize unset, to take the rest of the group's VMs.")
	}
	for _, e := range out {
		if v := e.(obj); v["targetSize"] == nil {
			v["targetSize"] = obj{"calculated": float64(size - sized)}
		}
	}
	return out, nil
}

// checkUpdatePolicy merges a patch's updatePolicy into cur.
func checkUpdatePolicy(cur obj, raw any, zones, size int, regional bool, shape string) (obj, error) {
	up := obj{}
	for k, v := range cur {
		up[k] = v
	}
	in, _ := raw.(map[string]any)
	for k, v := range in {
		up[k] = v
	}
	enum := func(field string, allowed ...string) error {
		v := str(up, field)
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return invalid("Invalid value for field 'resource.updatePolicy.%s': '%s'. Must be one of %s.", field, v, strings.Join(allowed, ", "))
	}
	if str(up, "type") == "" {
		up["type"] = "OPPORTUNISTIC"
	}
	if str(up, "minimalAction") == "" {
		up["minimalAction"] = "REPLACE"
	}
	if str(up, "mostDisruptiveAllowedAction") == "" {
		up["mostDisruptiveAllowedAction"] = "REPLACE"
	}
	if str(up, "replacementMethod") == "" {
		up["replacementMethod"] = "SUBSTITUTE"
	}
	for _, c := range []struct {
		f string
		a []string
	}{{"type", []string{"PROACTIVE", "OPPORTUNISTIC"}}, {"minimalAction", []string{"NONE", "REFRESH", "RESTART", "REPLACE"}},
		{"mostDisruptiveAllowedAction", []string{"NONE", "REFRESH", "RESTART", "REPLACE"}}, {"replacementMethod", []string{"SUBSTITUTE", "RECREATE"}}} {
		if err := enum(c.f, c.a...); err != nil {
			return nil, err
		}
	}
	if actionRank[str(up, "minimalAction")] > actionRank[str(up, "mostDisruptiveAllowedAction")] {
		return nil, invalid("Invalid value for field 'resource.updatePolicy.minimalAction': '%s'. It cannot be more disruptive than mostDisruptiveAllowedAction '%s'.",
			str(up, "minimalAction"), str(up, "mostDisruptiveAllowedAction"))
	}
	recreate := str(up, "replacementMethod") == "RECREATE"
	defSurge := float64(zones)
	if recreate {
		defSurge = 0
	}
	if _, ok := up["maxSurge"]; !ok {
		up["maxSurge"] = obj{"fixed": defSurge}
	}
	if _, ok := up["maxUnavailable"]; !ok {
		up["maxUnavailable"] = obj{"fixed": float64(zones)}
	}
	surgeObj, surge, err := fixedOrPercent("resource.updatePolicy.maxSurge", up["maxSurge"], size)
	if err != nil {
		return nil, err
	}
	unavObj, unav, err := fixedOrPercent("resource.updatePolicy.maxUnavailable", up["maxUnavailable"], size)
	if err != nil {
		return nil, err
	}
	up["maxSurge"], up["maxUnavailable"] = surgeObj, unavObj
	if recreate && surge != 0 {
		return nil, invalid("Invalid value for field 'resource.updatePolicy.maxSurge': %d. maxSurge must be 0 when replacementMethod is RECREATE.", surge)
	}
	if str(up, "type") == "PROACTIVE" && surge == 0 && unav == 0 {
		return nil, invalid("Invalid value for field 'resource.updatePolicy.maxUnavailable': 0. maxSurge and maxUnavailable cannot both be 0 for a PROACTIVE update.")
	}
	if regional {
		for f, o := range map[string]obj{"maxSurge": surgeObj, "maxUnavailable": unavObj} {
			if fx, ok := o["fixed"].(float64); ok && fx != 0 && int(fx) < zones {
				return nil, invalid("Invalid value for field 'resource.updatePolicy.%s.fixed': %v. A regional group's fixed %s must be 0 or at least its number of zones, %d.", f, fx, f, zones)
			}
		}
		if r := str(up, "instanceRedistributionType"); r != "" && r != "PROACTIVE" && r != "NONE" {
			return nil, invalid("Invalid value for field 'resource.updatePolicy.instanceRedistributionType': '%s'. Must be PROACTIVE or NONE.", r)
		} else if r == "PROACTIVE" && shape != "EVEN" {
			return nil, invalid("Invalid value for field 'resource.updatePolicy.instanceRedistributionType': 'PROACTIVE'. Proactive instance redistribution is supported only with the EVEN target distribution shape: set it to NONE.")
		}
	} else {
		delete(up, "instanceRedistributionType")
	}
	return up, nil
}

// patchManagerLocked is instanceGroupManagers.patch.
func (s *Server) patchManagerLocked(c call, l igLoc, rel string, m obj) (obj, error) {
	b := c.body
	if _, ok := b["fingerprint"]; ok {
		if err := s.checkFingerprint(m, b); err != nil {
			return nil, err
		}
	}
	if len(listOf(b["autoHealingPolicies"])) > 0 {
		return nil, unimplemented("autohealing is not served: health checks are not")
	}
	for _, f := range []string{"name", "zone", "region", "baseInstanceName", "instanceGroup"} {
		if v, ok := b[f]; ok && v != nil && str(b, f) != "" && lastSeg(str(b, f)) != lastSeg(str(m, f)) {
			return nil, invalid("Invalid value for field 'resource.%s': '%s'. The field cannot be changed.", f, str(b, f))
		}
	}
	size := int(m["targetSize"].(float64))
	if v, ok := b["targetSize"].(float64); ok {
		if v < 0 || v != math.Trunc(v) {
			return nil, invalid("Invalid value for field 'resource.targetSize': '%v'. Must be non-negative.", v)
		}
		if a := s.autoscalerOfLocked(rel); a != nil && l.regional() && autoscalerMode(a) != "OFF" && int(v) != size {
			return nil, invalid("Resizing of autoscaled regional managed instance groups is not allowed.")
		}
		size = int(v)
	}
	versions := m["versions"]
	if t := str(b, "instanceTemplate"); t != "" && b["versions"] == nil {
		versions = []any{obj{"instanceTemplate": t}}
	} else if b["versions"] != nil {
		versions = b["versions"]
	} else {
		// The current versions, sized again for the new target size.
		var vs []any
		for _, v := range versionsOf(m) {
			nv := obj{"instanceTemplate": v["instanceTemplate"]}
			if n := str(v, "name"); n != "" {
				nv["name"] = n
			}
			if ts, _ := v["targetSize"].(map[string]any); ts != nil && (ts["fixed"] != nil || ts["percent"] != nil) {
				nv["targetSize"] = obj{"fixed": ts["fixed"], "percent": ts["percent"]}
				if ts["fixed"] == nil {
					delete(nv["targetSize"].(obj), "fixed")
				} else {
					delete(nv["targetSize"].(obj), "percent")
				}
			}
			vs = append(vs, nv)
		}
		versions = vs
	}
	vs, err := s.checkVersionsLocked(c.project, l.region, versions, size)
	if err != nil {
		return nil, err
	}
	wp, setWP, err := s.checkManagerWorkloadLocked(l, b, m, true, len(s.liveManagedLocked(rel)))
	if err != nil {
		return nil, err
	}
	if setWP || workloadPolicyOf(m) != "" {
		var tpls []string
		for _, v := range vs {
			tpls = append(tpls, str(v.(obj), "instanceTemplate"))
		}
		if err := s.checkWorkloadTemplatesLocked(l.project, l.region, tpls, size); err != nil {
			return nil, err
		}
	}
	cur, _ := m["updatePolicy"].(map[string]any)
	up, err := checkUpdatePolicy(obj(cur), b["updatePolicy"], len(managerZones(m)), size, l.regional(), targetShapeOf(m))
	if err != nil {
		return nil, err
	}
	if _, ok := b["namedPorts"]; ok {
		np, err := checkNamedPorts("resource.namedPorts", b["namedPorts"])
		if err != nil {
			return nil, err
		}
		m["namedPorts"] = np
		if g, ok := s.res[str(m, "instanceGroup")]; ok {
			g["namedPorts"] = np
		}
	}
	if d, ok := b["description"].(string); ok {
		m["description"] = d
	}
	if setWP {
		m["resourcePolicies"] = obj{"workloadPolicy": wp}
	}
	m["targetSize"] = float64(size)
	m["versions"] = vs
	m["instanceTemplate"] = vs[0].(obj)["instanceTemplate"]
	if len(vs) > 1 {
		// With a canary the group's instanceTemplate is not one template.
		for _, v := range vs {
			if ts, _ := v.(obj)["targetSize"].(obj); ts["fixed"] == nil && ts["percent"] == nil {
				m["instanceTemplate"] = v.(obj)["instanceTemplate"]
			}
		}
	}
	m["updatePolicy"] = norm(up)
	m["fingerprint"] = newFingerprint()
	if _, err := s.rollManagerLocked(l.project, rel, m); err != nil {
		return nil, err
	}
	return s.done(c.project, l.scope, "patch", rel, m["id"])
}

// desiredTemplatesLocked is the template each of a manager's live VMs is
// to run: with one version its template; with two, the sized version's
// count kept or filled from the other's VMs, sorted by name.
func (s *Server) desiredTemplatesLocked(m obj, live []string) map[string]string {
	vs := versionsOf(m)
	out := map[string]string{}
	if len(vs) == 0 {
		for _, vm := range live {
			out[vm] = str(m, "instanceTemplate")
		}
		return out
	}
	if len(vs) == 1 {
		for _, vm := range live {
			out[vm] = str(vs[0], "instanceTemplate")
		}
		return out
	}
	var sized, rest obj
	for _, v := range vs {
		ts, _ := v["targetSize"].(map[string]any)
		if ts["fixed"] != nil || ts["percent"] != nil {
			sized = v
		} else {
			rest = v
		}
	}
	want := 0
	if ts, _ := sized["targetSize"].(map[string]any); ts != nil {
		f, _ := ts["calculated"].(float64)
		want = int(f)
	}
	st := str(sized, "instanceTemplate")
	sorted := append([]string(nil), live...)
	sort.Strings(sorted)
	n := 0
	for _, vm := range sorted {
		if instanceTemplateOf(s.res[vm]) == st && n < want {
			out[vm] = st
			n++
		}
	}
	for _, vm := range sorted {
		if _, ok := out[vm]; ok {
			continue
		}
		if n < want && instanceTemplateOf(s.res[vm]) != str(rest, "instanceTemplate") {
			out[vm] = st
			n++
			continue
		}
		out[vm] = str(rest, "instanceTemplate")
	}
	for _, vm := range sorted {
		if n >= want {
			break
		}
		if out[vm] != st {
			out[vm] = st
			n++
		}
	}
	return out
}

// newVMTemplateLocked is the template a manager's next new VM is made
// from: the version short of its count.
func (s *Server) newVMTemplateLocked(rel string, m obj) string {
	vs := versionsOf(m)
	if len(vs) < 2 {
		return str(m, "instanceTemplate")
	}
	counts := map[string]int{}
	for _, vm := range s.liveManagedLocked(rel) {
		counts[instanceTemplateOf(s.res[vm])]++
	}
	for _, v := range vs {
		ts, _ := v["targetSize"].(map[string]any)
		if ts["fixed"] == nil && ts["percent"] == nil {
			continue
		}
		want, _ := ts["calculated"].(float64)
		if counts[str(v, "instanceTemplate")] < int(want) {
			return str(v, "instanceTemplate")
		}
	}
	return str(m, "instanceTemplate")
}

// requiredAction is what bringing a VM made from have to want needs:
// NONE, REFRESH when only the templates' metadata differ, else REPLACE.
func (s *Server) requiredAction(have, want string) string {
	if have == want {
		return "NONE"
	}
	a, _ := s.res[have]["properties"].(map[string]any)
	b, _ := s.res[want]["properties"].(map[string]any)
	if a == nil || b == nil {
		return "REPLACE"
	}
	ac, _ := deepCopy(a).(map[string]any)
	bc, _ := deepCopy(b).(map[string]any)
	delete(ac, "metadata")
	delete(bc, "metadata")
	if equalJSON(ac, bc) {
		return "REFRESH"
	}
	return "REPLACE"
}

// refreshManagedLocked gives a managed VM the metadata of template tpl, in
// place, and records tpl as its template.
func (s *Server) refreshManagedLocked(mgr string, o obj, tpl string) {
	props, _ := s.res[tpl]["properties"].(map[string]any)
	md, _ := deepCopy(props["metadata"]).(map[string]any)
	if md == nil {
		md = map[string]any{}
	}
	items := listOf(md["items"])
	items = append(items, map[string]any{"key": "created-by", "value": mgr}, map[string]any{"key": "instance-template", "value": tpl})
	md["items"] = items
	md["fingerprint"] = newFingerprint()
	md["kind"] = "compute#metadata"
	o["metadata"] = norm(md)
}

// settledLocked is whether every VM of a manager is RUNNING with no
// operation of its own running.
func (s *Server) settledLocked(rel string) bool {
	prefix := rel[:strings.Index(rel, "/instanceGroupManagers/")]
	prefix = prefix[:strings.LastIndex(prefix, "/")] // projects/P/zones or projects/P/regions
	prefix = prefix[:strings.LastIndex(prefix, "/")] + "/zones/"
	for vm, n := range s.queued {
		// A VM being made again (recreateManagedFromLocked) is not yet
		// listed; its group is not settled until it is.
		if n > 0 && strings.HasPrefix(vm, prefix) && s.res[vm] == nil && strings.Contains(vm, "/instances/") {
			return false
		}
	}
	for _, vm := range s.managedLocked(rel) {
		o := s.res[vm]
		if o == nil || o["_deleting"] == true || s.queued[vm] > 0 || str(o, "status") != stRunning {
			return false
		}
	}
	return true
}

// outdatedLocked are a manager's live VMs whose template is not their
// version's, with the action each takes under minimal and most (VMs that
// would need more than most left out).
func (s *Server) outdatedLocked(rel string, m obj, minimal, most string) ([]string, map[string]string, map[string]string) {
	live := s.liveManagedLocked(rel)
	want := s.desiredTemplatesLocked(m, live)
	var out []string
	acts, tpls := map[string]string{}, map[string]string{}
	for _, vm := range live {
		have := instanceTemplateOf(s.res[vm])
		if have == want[vm] {
			continue
		}
		a := s.requiredAction(have, want[vm])
		if actionRank[minimal] > actionRank[a] {
			a = minimal
		}
		if actionRank[a] > actionRank[most] {
			continue
		}
		out = append(out, vm)
		acts[vm], tpls[vm] = a, want[vm]
	}
	sort.Strings(out)
	return out, acts, tpls
}

// applyActionLocked brings one VM to tpl by action; a SUBSTITUTE
// replacement deletes it (the group's size is made up by new VMs).
func (s *Server) applyActionLocked(project, mgr string, m obj, vm, tpl, action string, substitute bool) error {
	o := s.res[vm]
	switch action {
	case "REFRESH":
		s.refreshManagedLocked(mgr, o, tpl)
		mgrAction(o, "REFRESHING")
		return nil
	case "RESTART":
		s.refreshManagedLocked(mgr, o, tpl)
		z := lastSeg(str(o, "zone"))
		if _, err := s.instances(call{method: http.MethodPost, project: project, parts: []string{"zones", z, "instances", str(o, "name"), "reset"}, body: obj{}}, z); err != nil {
			return err
		}
		mgrAction(o, "RESTARTING")
		return nil
	case "REPLACE":
		if substitute {
			return s.deleteManagedLocked(project, vm)
		}
		return s.recreateManagedFromLocked(project, mgr, m, vm, o, tpl)
	}
	return nil
}

// rollManagerLocked takes one step of a PROACTIVE update when the
// group's VMs are settled: whether it changed anything.
func (s *Server) rollManagerLocked(project, rel string, m obj) (bool, error) {
	up, _ := m["updatePolicy"].(map[string]any)
	if str(up, "type") != "PROACTIVE" || !s.settledLocked(rel) {
		return false, nil
	}
	size := int(m["targetSize"].(float64))
	live := s.liveManagedLocked(rel)
	out, acts, tpls := s.outdatedLocked(rel, obj(m), str(up, "minimalAction"), str(up, "mostDisruptiveAllowedAction"))
	if len(live) > size {
		// Surplus after a surge: the outdated ones go first.
		victims := append([]string(nil), out...)
		for _, vm := range live {
			if !hasStr(victims, vm) {
				victims = append(victims, vm)
			}
		}
		for _, vm := range victims[:len(live)-size] {
			if err := s.deleteManagedLocked(project, vm); err != nil {
				return true, err
			}
		}
		return true, s.syncGroupSizeLocked(m)
	}
	if len(live) < size {
		return true, s.resizeToLocked(project, rel, m)
	}
	if len(out) == 0 {
		return false, nil
	}
	_, surge, _ := fixedOrPercent("maxSurge", up["maxSurge"], size)
	_, unav, _ := fixedOrPercent("maxUnavailable", up["maxUnavailable"], size)
	substitute := str(up, "replacementMethod") != "RECREATE"
	var replace, restart []string
	for _, vm := range out {
		switch acts[vm] {
		case "REFRESH":
			// In place, nothing made unavailable: all at once.
			if err := s.applyActionLocked(project, rel, m, vm, tpls[vm], "REFRESH", substitute); err != nil {
				return true, err
			}
		case "RESTART":
			restart = append(restart, vm)
		case "REPLACE":
			replace = append(replace, vm)
		}
	}
	budget := unav
	for _, vm := range restart {
		if budget == 0 {
			break
		}
		if err := s.applyActionLocked(project, rel, m, vm, tpls[vm], "RESTART", substitute); err != nil {
			return true, err
		}
		budget--
	}
	if len(replace) == 0 {
		return true, nil
	}
	if substitute {
		// maxSurge new VMs first, and up to maxUnavailable old ones
		// deleted; the next steps delete the surplus or make up the size.
		made := 0
		for made < surge && made < len(replace) {
			if err := s.addManagedVMLocked(project, rel, m); err != nil {
				return true, err
			}
			made++
		}
		for i := 0; i < budget && i < len(replace); i++ {
			if err := s.deleteManagedLocked(project, replace[i]); err != nil {
				return true, err
			}
		}
		return true, s.syncGroupSizeLocked(m)
	}
	for i := 0; i < budget && i < len(replace); i++ {
		if err := s.applyActionLocked(project, rel, m, replace[i], tpls[replace[i]], "REPLACE", false); err != nil {
			return true, err
		}
	}
	return true, nil
}

// addManagedVMLocked makes one more VM of a manager, beyond its size.
func (s *Server) addManagedVMLocked(project, rel string, m obj) error {
	have := s.liveManagedLocked(rel)
	for {
		name := str(m, "baseInstanceName") + "-" + randLower(4)
		zone := s.nextZoneLocked(m, have)
		if _, taken := s.res[zoneRel(project, zone, "instances", name)]; taken {
			continue
		}
		return s.insertManagedVMLocked(project, zone, rel, m, name)
	}
}

// syncGroupSizeLocked sets the instance group's size to the manager's.
func (s *Server) syncGroupSizeLocked(m obj) error {
	if g, ok := s.res[str(m, "instanceGroup")]; ok {
		g["size"] = m["targetSize"]
	}
	return nil
}

// applyUpdatesLocked is applyUpdatesToInstances: the chosen VMs (or all)
// brought to their version now, whatever the update type, each by the more
// disruptive of minimalAction (default NONE) and what it needs, none
// beyond mostDisruptiveAllowedAction (default REPLACE); a replacement
// keeps the VM's name.
func (s *Server) applyUpdatesLocked(c call, l igLoc, rel string, m obj) (obj, error) {
	minimal, most := str(c.body, "minimalAction"), str(c.body, "mostDisruptiveAllowedAction")
	if minimal == "" {
		minimal = "NONE"
	}
	if most == "" {
		most = "REPLACE"
	}
	for f, v := range map[string]string{"minimalAction": minimal, "mostDisruptiveAllowedAction": most} {
		if _, ok := actionRank[v]; !ok {
			return nil, invalid("Invalid value for field '%s': '%s'. Must be one of NONE, REFRESH, RESTART, REPLACE.", f, v)
		}
	}
	if actionRank[minimal] > actionRank[most] {
		return nil, invalid("Invalid value for field 'minimalAction': '%s'. It cannot be more disruptive than mostDisruptiveAllowedAction '%s'.", minimal, most)
	}
	live := s.liveManagedLocked(rel)
	var targets []string
	if c.body["allInstances"] == true {
		if len(listOf(c.body["instances"])) > 0 {
			return nil, invalid("Invalid value for field 'instances': with allInstances, no instances may be named.")
		}
		targets = live
	} else {
		t, err := s.managedTargetsLocked(c, rel)
		if err != nil {
			return nil, err
		}
		targets = t
	}
	want := s.desiredTemplatesLocked(m, live)
	for _, vm := range targets {
		have := instanceTemplateOf(s.res[vm])
		a := s.requiredAction(have, want[vm])
		if actionRank[minimal] > actionRank[a] {
			a = minimal
		}
		if a == "NONE" || actionRank[a] > actionRank[most] {
			continue
		}
		if err := s.applyActionLocked(c.project, rel, m, vm, want[vm], a, false); err != nil {
			return nil, err
		}
	}
	return s.done(c.project, l.scope, "applyUpdatesToInstances", rel, m["id"])
}

// rollOnce steps every PROACTIVE update.
func (s *Server) rollOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, m := range s.res {
		if !strings.Contains(k, "/instanceGroupManagers/") {
			continue
		}
		up, _ := m["updatePolicy"].(map[string]any)
		if str(up, "type") != "PROACTIVE" {
			continue
		}
		did, err := s.rollManagerLocked(projectOfRel(k), k, m)
		if err != nil {
			fmt.Printf("compute: updating %s: %v\n", k, err)
		}
		changed = changed || did
	}
	if changed {
		_ = s.saveLocked()
	}
}

// rollLoop runs rollOnce every UpdateEvery until ctx ends.
func (s *Server) rollLoop(ctx context.Context) {
	every := s.opts.UpdateEvery
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.rollOnce()
	}
}

func projectOfRel(rel string) string { return strings.Split(rel, "/")[1] }
