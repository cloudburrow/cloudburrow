package compute

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Instance groups (#2139): unmanaged instanceGroups
// (https://docs.cloud.google.com/compute/docs/instance-groups/creating-groups-of-unmanaged-instances)
// and zonal managed instanceGroupManagers made from an instance template
// (https://docs.cloud.google.com/compute/docs/instance-groups/create-zonal-mig,
// read 2026-11-22). A managed group's VMs are ordinary instances named
// BASE-XXXX, with the created-by and instance-template metadata Google
// gives them; resize adds or deletes VMs, recreateInstances re-creates a
// VM's boot disk from the template and boots it again. Kubernetes Engine's
// node-pool groups are answered beside these (Options.Extra) by name.

func groupRel(project, zone, coll, name string) string {
	return zoneRel(project, zone, coll, name)
}

// groupMembersLocked are the instances an unmanaged group lists.
func groupMembersLocked(g obj) []string { return strs(g, "_members") }

func (s *Server) instanceGroups(c call, zone string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/zones/" + zone + "/instanceGroups"
	scope := "zones/" + zone
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 3 && get:
		out, err := s.listOut("compute#instanceGroupList", coll, s.res, c.query)
		return out, err
	case n == 3 && post:
		return s.insertGroup(c, zone)
	}
	if n < 4 || n > 5 || (n == 4 && !get && !del) || (n == 5 && (!post || !servedGroupVerbs[p[4]])) {
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
		return s.done(c.project, scope, "delete", rel, g["id"])
	case post && verb == "listInstances":
		var items []obj
		for _, m := range s.groupInstancesLocked(g) {
			vm := s.res[m]
			items = append(items, obj{"instance": m, "status": str(vm, "status")})
		}
		if items == nil {
			items = []obj{}
		}
		return obj{"kind": "compute#instanceGroupsListInstances", "id": rel, "items": items, "selfLink": rel}, nil
	case post && (verb == "addInstances" || verb == "removeInstances"):
		if str(g, "_manager") != "" {
			return nil, invalid("Invalid value for field 'instanceGroup': '%s'. A managed instance group's instances are added and removed by its manager.", rel)
		}
		members := groupMembersLocked(g)
		for i, e := range listOf(c.body["instances"]) {
			im, _ := e.(map[string]any)
			ir := Rel(str(im, "instance"), c.project)
			vm, ok := s.res[ir]
			if !ok || !strings.Contains(ir, "/instances/") {
				return nil, notFound(ir)
			}
			if verb == "addInstances" {
				if lastSeg(str(vm, "zone")) != zone {
					return nil, invalid("Invalid value for field 'resource.instances[%d].instance': '%s'. The instance must be in zone '%s'.", i, str(im, "instance"), zone)
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
			var keep []string
			for _, m := range members {
				if m != ir {
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
		return s.done(c.project, scope, verb, rel, g["id"])
	case post && verb == "setNamedPorts":
		g["namedPorts"] = c.body["namedPorts"]
		g["fingerprint"] = newFingerprint()
		return s.done(c.project, scope, "setNamedPorts", rel, g["id"])
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
	if np, ok := c.body["namedPorts"]; ok {
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

// managedLocked are the VMs a manager made, by name.
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

func vmMetadataItems(o obj) any {
	md, _ := o["metadata"].(map[string]any)
	return md["items"]
}

// --- managed instance groups ----------------------------------------------------

func (s *Server) instanceGroupManagers(c call, zone string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/zones/" + zone + "/instanceGroupManagers"
	scope := "zones/" + zone
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 3 && get:
		out, err := s.listOut("compute#instanceGroupManagerList", coll, s.res, c.query)
		if err == nil {
			items := out["items"].([]obj)
			for _, e := range items {
				s.refreshManagerLocked(e)
			}
			if s.opts.ExtraGroups != nil && first(c.query, "filter") == "" {
				// Kubernetes Engine's node-pool groups (#1913).
				extra := s.opts.ExtraGroups(c.project, zone)
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
		return s.insertManager(c, zone)
	}
	if n < 4 || n > 5 || (n == 4 && !get && !del) || (n == 5 && (!post || !servedManagerVerbs[p[4]])) {
		return nil, unimplemented("%s on instanceGroupManagers/%s is not served", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	m, ok := s.res[rel]
	if !ok {
		if s.opts.ExtraGroups != nil {
			if g := s.opts.ExtraGroups(c.project, zone)[p[3]]; g != nil && n == 4 && get {
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
	case n == 4 && del:
		for _, vm := range s.managedLocked(rel) {
			if o := s.res[vm]; o["deletionProtection"] == true {
				return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
					"Invalid resource usage: 'Resource cannot be deleted if it's protected against deletion.'.")
			}
		}
		for _, vm := range s.managedLocked(rel) {
			if _, err := s.deleteInstance(c.project, scope, vm, s.res[vm]); err != nil {
				return nil, err
			}
		}
		delete(s.res, str(m, "instanceGroup"))
		delete(s.res, rel)
		return s.done(c.project, scope, "delete", rel, m["id"])
	case post && verb == "resize":
		size, err := strconv.Atoi(first(c.query, "size"))
		if err != nil || size < 0 {
			return nil, invalid("Invalid value for field 'size': '%s'. Must be a non-negative integer.", first(c.query, "size"))
		}
		m["targetSize"] = float64(size)
		return s.reconcileManagerLocked(c.project, zone, rel, m, "resize")
	case post && verb == "setInstanceTemplate":
		t := Rel(str(c.body, "instanceTemplate"), c.project)
		if _, ok := s.res[t]; !ok || !strings.Contains(t, "/instanceTemplates/") {
			return nil, notFound(t)
		}
		m["instanceTemplate"] = t
		m["versions"] = []any{obj{"instanceTemplate": t, "targetSize": obj{"calculated": m["targetSize"]}}}
		return s.done(c.project, scope, "setInstanceTemplate", rel, m["id"])
	case post && verb == "listManagedInstances":
		var items []obj
		for _, vm := range s.managedLocked(rel) {
			o := s.res[vm]
			action := "NONE"
			switch str(o, "status") {
			case stProvisioning:
				action = "CREATING"
			case stStopping:
				action = "DELETING"
			}
			if s.queued[vm] > 0 && action == "NONE" {
				action = "RECREATING"
			}
			items = append(items, obj{"instance": vm, "id": o["id"], "instanceStatus": str(o, "status"), "currentAction": action,
				"version": obj{"instanceTemplate": instanceTemplateOf(o)}})
		}
		if items == nil {
			items = []obj{}
		}
		return obj{"managedInstances": items}, nil
	case post && (verb == "startInstances" || verb == "stopInstances" || verb == "suspendInstances" || verb == "resumeInstances"):
		iverb := strings.TrimSuffix(verb, "Instances")
		for _, e := range strsAny(c.body["instances"]) {
			ir := Rel(e, c.project)
			if !hasStr(s.managedLocked(rel), ir) {
				return nil, invalid("Invalid value for field 'instances': '%s'. The instance is not managed by '%s'.", e, rel)
			}
			parts := strings.Split(ir, "/")
			if _, err := s.instances(call{method: http.MethodPost, project: c.project, parts: []string{"zones", zone, "instances", parts[len(parts)-1], iverb}, body: obj{}}, zone); err != nil {
				return nil, err
			}
		}
		return s.done(c.project, scope, verb, rel, m["id"])
	case post && (verb == "recreateInstances" || verb == "deleteInstances"):
		var targets []string
		for _, e := range strsAny(c.body["instances"]) {
			ir := Rel(e, c.project)
			if !hasStr(s.managedLocked(rel), ir) {
				return nil, invalid("Invalid value for field 'instances': '%s'. The instance is not managed by '%s'.", e, rel)
			}
			targets = append(targets, ir)
		}
		if len(targets) == 0 {
			return nil, required("instances")
		}
		for _, vm := range targets {
			o := s.res[vm]
			if verb == "deleteInstances" {
				if _, err := s.deleteInstance(c.project, scope, vm, o); err != nil {
					return nil, err
				}
				continue
			}
			if _, err := s.recreateLocked(c.project, scope, vm, o); err != nil {
				return nil, err
			}
		}
		if verb == "deleteInstances" {
			size, _ := m["targetSize"].(float64)
			m["targetSize"] = max(0, size-float64(len(targets)))
		}
		return s.done(c.project, scope, verb, rel, m["id"])
	}
	return nil, unimplemented("%s on instanceGroupManagers/%s is not served", c.method, strings.Join(p[3:], "/"))
}

var servedGroupVerbs = map[string]bool{"addInstances": true, "removeInstances": true, "listInstances": true, "setNamedPorts": true}

var servedManagerVerbs = map[string]bool{"resize": true, "setInstanceTemplate": true, "listManagedInstances": true,
	"recreateInstances": true, "deleteInstances": true, "startInstances": true, "stopInstances": true,
	"suspendInstances": true, "resumeInstances": true}

func instanceTemplateOf(o obj) string {
	for _, it := range listOf(vmMetadataItems(o)) {
		im, _ := it.(map[string]any)
		if str(im, "key") == "instance-template" {
			return str(im, "value")
		}
	}
	return ""
}

func (s *Server) insertManager(c call, zone string) (obj, error) {
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := groupRel(c.project, zone, "instanceGroupManagers", name)
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
	if r := lastSeg(str(s.res[t], "region")); r != "" && r != regionOfZone(zone) {
		return nil, invalid("Invalid value for field 'resource.instanceTemplate': '%s'. A regional instance template makes VMs of its region only.", tref)
	}
	if _, ok := b["autoHealingPolicies"]; ok && len(listOf(b["autoHealingPolicies"])) > 0 {
		return nil, unimplemented("autohealing is not served: health checks are not")
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
	group := groupRel(c.project, zone, "instanceGroups", name)
	if _, ok := s.res[group]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", group)
	}
	m := obj{"kind": "compute#instanceGroupManager", "id": newID(), "name": name, "zone": "projects/" + c.project + "/zones/" + zone,
		"selfLink": rel, "creationTimestamp": s.stamp(), "baseInstanceName": base, "instanceTemplate": t, "instanceGroup": group,
		"targetSize": size, "fingerprint": newFingerprint(), "listManagedInstancesResults": "PAGELESS",
		"versions":     []any{obj{"instanceTemplate": t, "targetSize": obj{"calculated": size}}},
		"updatePolicy": obj{"type": "OPPORTUNISTIC", "minimalAction": "REPLACE", "replacementMethod": "SUBSTITUTE"}}
	for _, k := range []string{"description", "namedPorts"} {
		if v, ok := b[k]; ok {
			m[k] = v
		}
	}
	g := obj{"kind": "compute#instanceGroup", "id": newID(), "name": name, "zone": m["zone"], "selfLink": group,
		"creationTimestamp": s.stamp(), "size": size, "fingerprint": newFingerprint(), "_manager": rel}
	if np, ok := b["namedPorts"]; ok {
		g["namedPorts"] = np
	}
	s.res[rel] = norm(m)
	s.res[group] = norm(g)
	return s.reconcileManagerLocked(c.project, zone, rel, s.res[rel], "insert")
}

// reconcileManagerLocked adds VMs up to the target size, or deletes the
// newest beyond it.
func (s *Server) reconcileManagerLocked(project, zone, rel string, m obj, opType string) (obj, error) {
	have := s.managedLocked(rel)
	want := int(m["targetSize"].(float64))
	tpl := str(m, "instanceTemplate")
	for len(have) < want {
		name := str(m, "baseInstanceName") + "-" + randLower(4)
		vmRel := zoneRel(project, zone, "instances", name)
		if _, taken := s.res[vmRel]; taken {
			continue
		}
		props, _ := s.res[tpl]["properties"].(map[string]any)
		md, _ := deepCopy(props["metadata"]).(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		items := listOf(md["items"])
		items = append(items, map[string]any{"key": "created-by", "value": rel}, map[string]any{"key": "instance-template", "value": tpl})
		md["items"] = items
		if _, err := s.insertInstance(call{method: http.MethodPost, project: project, parts: []string{"zones", zone, "instances"},
			query: map[string][]string{"sourceInstanceTemplate": {tpl}}, body: obj{"name": name, "metadata": md}}, zone); err != nil {
			return nil, err
		}
		have = append(have, vmRel)
	}
	if len(have) > want {
		// The newest go first, as Google's resize down does.
		sort.Slice(have, func(i, j int) bool {
			return str(s.res[have[i]], "creationTimestamp") > str(s.res[have[j]], "creationTimestamp")
		})
		for _, vm := range have[:len(have)-want] {
			if _, err := s.deleteInstance(project, "zones/"+zone, vm, s.res[vm]); err != nil {
				return nil, err
			}
		}
	}
	if g, ok := s.res[str(m, "instanceGroup")]; ok {
		g["size"] = float64(want)
	}
	return s.done(project, "zones/"+zone, opType, rel, m["id"])
}

// refreshManagerLocked sets a manager's currentActions and status from its
// VMs.
func (s *Server) refreshManagerLocked(m obj) {
	acts := obj{"none": 0.0, "creating": 0.0, "deleting": 0.0, "recreating": 0.0, "restarting": 0.0, "verifying": 0.0,
		"abandoning": 0.0, "creatingWithoutRetries": 0.0, "refreshing": 0.0, "resuming": 0.0, "starting": 0.0,
		"stopping": 0.0, "suspending": 0.0}
	stable := true
	vms := s.managedLocked(str(m, "selfLink"))
	for _, vm := range vms {
		o := s.res[vm]
		switch {
		case str(o, "status") == stProvisioning:
			acts["creating"] = acts["creating"].(float64) + 1
			stable = false
		case str(o, "status") == stStopping:
			acts["deleting"] = acts["deleting"].(float64) + 1
			stable = false
		case s.queued[vm] > 0:
			acts["recreating"] = acts["recreating"].(float64) + 1
			stable = false
		default:
			acts["none"] = acts["none"].(float64) + 1
		}
	}
	if float64(len(vms)) != m["targetSize"] {
		stable = false
	}
	m["currentActions"] = acts
	m["status"] = obj{"isStable": stable, "versionTarget": obj{"isReached": stable}, "allInstancesConfig": obj{"effective": true}}
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
