package compute

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// Instance templates and machine images (#2139):
// https://docs.cloud.google.com/compute/docs/instance-templates/create-instance-templates,
// get-list-delete-instance-templates and
// https://docs.cloud.google.com/compute/docs/machine-images/create-machine-images
// (read 2026-11-22). A template is a VM's properties kept to make VMs from
// (instances.insert ?sourceInstanceTemplate=, and a managed instance
// group's VMs); a machine image is a VM's properties and a copy of each of
// its disks (instances.insert with sourceMachineImage).

// instanceTemplates serves global and regional instanceTemplates: insert,
// get, list and delete (refused while a managed instance group uses it).
func (s *Server) instanceTemplates(c call, scope string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/" + scope + "/instanceTemplates"
	base := 2
	if scope != "global" {
		base = 3
	}
	switch {
	case n == base && c.method == http.MethodGet:
		return s.listOut("compute#instanceTemplateList", coll, s.res, c.query)
	case n == base && c.method == http.MethodPost:
		return s.insertTemplate(c, scope, coll)
	}
	if n != base+1 || (c.method != http.MethodGet && c.method != http.MethodDelete) {
		return nil, unimplemented("%s on instanceTemplates/%s is not served", c.method, strings.Join(p[base:], "/"))
	}
	rel := coll + "/" + p[base]
	o, ok := s.res[rel]
	if !ok {
		// Compute Engine takes a template's numeric id in place of its
		// name, as the Terraform provider reads and deletes it by.
		if rel, o, ok = s.byIDLocked(coll, p[base]); !ok {
			return nil, notFound(coll + "/" + p[base])
		}
	}
	switch c.method {
	case http.MethodGet:
		return o, nil
	case http.MethodDelete:
		for k, m := range s.res {
			if strings.Contains(k, "/instanceGroupManagers/") && str(m, "instanceTemplate") == rel {
				return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
					"The instance_template resource '%s' is already being used by '%s'", rel, k)
			}
		}
		delete(s.res, rel)
		return s.done(c.project, scope, "delete", rel, o["id"])
	}
	return nil, unimplemented("%s on instanceTemplates/%s is not served", c.method, p[base])
}

// byIDLocked is the resource of coll whose numeric id is seg.
func (s *Server) byIDLocked(coll, seg string) (string, obj, bool) {
	if seg == "" || strings.Trim(seg, "0123456789") != "" {
		return "", nil, false
	}
	for k, o := range s.res {
		if strings.HasPrefix(k, coll+"/") && !strings.Contains(k[len(coll)+1:], "/") && str(o, "id") == seg {
			return k, o, true
		}
	}
	return "", nil, false
}

func (s *Server) insertTemplate(c call, scope, coll string) (obj, error) {
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := coll + "/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	if str(b, "sourceInstance") != "" {
		src := Rel(str(b, "sourceInstance"), c.project)
		vm, ok := s.res[src]
		if !ok {
			return nil, notFound(src)
		}
		b["properties"] = s.propertiesOfLocked(vm)
	}
	props, _ := b["properties"].(map[string]any)
	if props == nil {
		return nil, required("resource.properties")
	}
	mt := lastSeg(str(props, "machineType"))
	if mt == "" {
		return nil, required("resource.properties.machineType")
	}
	if _, ok := LookupMachineType(mt); !ok {
		return nil, invalid("Invalid value for field 'resource.properties.machineType': '%s'. Machine type with name '%s' does not exist.", str(props, "machineType"), mt)
	}
	disks, _ := props["disks"].([]any)
	if len(disks) == 0 {
		return nil, invalid("Invalid value for field 'resource.properties.disks': ''. At least one disk, the boot disk, must be specified.")
	}
	for i, e := range disks {
		d, _ := e.(map[string]any)
		ip, _ := d["initializeParams"].(map[string]any)
		if i == 0 && str(d, "source") == "" && str(ip, "sourceImage") == "" && str(ip, "sourceSnapshot") == "" {
			return nil, required("resource.properties.disks[0].initializeParams.sourceImage")
		}
		if si := str(ip, "sourceImage"); si != "" {
			if _, _, err := s.imageSeedLocked(si, c.project); err != nil {
				return nil, err
			}
		}
		stripPrivate(ip)
	}
	if nics, _ := props["networkInterfaces"].([]any); len(nics) == 0 {
		props["networkInterfaces"] = []any{map[string]any{"network": "projects/" + c.project + "/global/networks/default",
			"accessConfigs": []any{map[string]any{"name": "External NAT", "type": "ONE_TO_ONE_NAT"}}}}
	} else {
		for _, e := range nics {
			nic, _ := e.(map[string]any)
			nw := vpcOrStr(str(nic, "network"), "default")
			nr, ok := NetworkName(nw, c.project)
			if !ok {
				return nil, invalid("Invalid value for field 'resource.properties.networkInterfaces[0].network': '%s'.", nw)
			}
			if _, ok := s.res[nr]; !ok {
				return nil, notFound(nr)
			}
			nic["network"] = nr
		}
	}
	if sc, ok := props["scheduling"].(map[string]any); ok {
		next, err := checkScheduling(sc, obj{"onHostMaintenance": "MIGRATE", "automaticRestart": true, "preemptible": false, "provisioningModel": "STANDARD"})
		if err != nil {
			return nil, err
		}
		props["scheduling"] = next
	}
	props["machineType"] = mt
	o := obj{"kind": "compute#instanceTemplate", "id": newID(), "name": name, "selfLink": rel, "creationTimestamp": s.stamp(),
		"properties": props}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	if src := str(b, "sourceInstance"); src != "" {
		o["sourceInstance"] = Rel(src, c.project)
	}
	if scope != "global" {
		o["region"] = "projects/" + c.project + "/" + scope
	}
	s.res[rel] = norm(o)
	return s.done(c.project, scope, "insert", rel, o["id"])
}

// stripPrivate drops the fields only this package sets: a request cannot
// name another resource's storage.
func stripPrivate(m map[string]any) {
	for k := range m {
		if strings.HasPrefix(k, "_") {
			delete(m, k)
		}
	}
}

// propertiesOfLocked are an instance's properties as a template or a
// machine image keeps them: its disks as new disks of the same image.
func (s *Server) propertiesOfLocked(vm obj) obj {
	props := obj{"machineType": lastSeg(str(vm, "machineType")), "canIpForward": vm["canIpForward"] == true}
	for _, k := range []string{"description", "metadata", "tags", "labels", "scheduling", "serviceAccounts", "resourcePolicies"} {
		if v, ok := vm[k]; ok {
			props[k] = deepCopy(v)
		}
	}
	var disks []any
	for _, e := range listOf(vm["disks"]) {
		ad, _ := e.(map[string]any)
		d := s.res[str(ad, "source")]
		ip := obj{"diskType": lastSeg(str(d, "type")), "diskSizeGb": d["sizeGb"]}
		if si := s.diskImageLocked(d); si != "" {
			ip["sourceImage"] = si
		}
		disks = append(disks, obj{"boot": ad["boot"], "autoDelete": ad["autoDelete"], "deviceName": ad["deviceName"],
			"mode": ad["mode"], "type": "PERSISTENT", "initializeParams": ip})
	}
	props["disks"] = disks
	var nics []any
	for _, e := range listOf(vm["networkInterfaces"]) {
		n, _ := e.(map[string]any)
		nic := obj{"network": n["network"], "subnetwork": n["subnetwork"]}
		if acs := listOf(n["accessConfigs"]); len(acs) > 0 {
			nic["accessConfigs"] = []any{obj{"name": "External NAT", "type": "ONE_TO_ONE_NAT"}}
		}
		nics = append(nics, nic)
	}
	props["networkInterfaces"] = nics
	return props
}

// bodyFromProperties is an instances.insert body made from template or
// machine image properties, in zone, the request's own fields winning.
func bodyFromProperties(props obj, zone string, req obj) obj {
	b := obj{}
	for k, v := range deepCopy(props).(obj) {
		b[k] = v
	}
	b["machineType"] = "zones/" + zone + "/machineTypes/" + lastSeg(str(props, "machineType"))
	for _, e := range listOf(b["networkInterfaces"]) {
		nic, _ := e.(map[string]any)
		if sub := str(nic, "subnetwork"); sub != "" && !strings.Contains(sub, "/regions/"+regionOfZone(zone)+"/") {
			delete(nic, "subnetwork") // a global template's subnet is of its region; the network picks the zone's
		}
	}
	for k, v := range req {
		if k == "disks" || k == "networkInterfaces" {
			if l, _ := v.([]any); len(l) == 0 {
				continue
			}
		}
		b[k] = v
	}
	return b
}

// fromSourceLocked fills an instances.insert body from its
// sourceInstanceTemplate or sourceMachineImage, and says which.
func (s *Server) fromSourceLocked(c call, zone string) (obj, string, string, error) {
	for _, e := range listOf(c.body["disks"]) {
		d, _ := e.(map[string]any)
		ip, _ := d["initializeParams"].(map[string]any)
		stripPrivate(ip)
	}
	if t := first(c.query, "sourceInstanceTemplate"); t != "" {
		rel := Rel(t, c.project)
		if !strings.HasPrefix(rel, "projects/") {
			rel = "projects/" + c.project + "/" + strings.TrimPrefix(rel, "/")
		}
		tpl, ok := s.res[rel]
		if !ok || !strings.Contains(rel, "/instanceTemplates/") {
			return nil, "", "", notFound(rel)
		}
		if r := lastSeg(str(tpl, "region")); r != "" && r != regionOfZone(zone) {
			return nil, "", "", invalid("Invalid value for field 'sourceInstanceTemplate': '%s'. A regional instance template makes VMs of its region only.", t)
		}
		props, _ := tpl["properties"].(map[string]any)
		return bodyFromProperties(props, zone, c.body), "sourceInstanceTemplate", rel, nil
	}
	if mi := str(c.body, "sourceMachineImage"); mi != "" {
		rel := Rel(mi, c.project)
		if !strings.Contains(rel, "/") {
			rel = "projects/" + c.project + "/global/machineImages/" + rel
		}
		img, ok := s.res[rel]
		if !ok || !strings.Contains(rel, "/machineImages/") {
			return nil, "", "", notFound(rel)
		}
		if str(img, "status") != "READY" {
			return nil, "", "", errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", rel)
		}
		props, _ := img["instanceProperties"].(map[string]any)
		req := obj{}
		for k, v := range c.body {
			if k != "sourceMachineImage" {
				req[k] = v
			}
		}
		delete(req, "disks") // a machine image's VM gets copies of its disks
		b := bodyFromProperties(props, zone, req)
		saved := listOf(img["savedDisks"])
		for i, e := range listOf(b["disks"]) {
			d, _ := e.(map[string]any)
			ip, _ := d["initializeParams"].(map[string]any)
			if ip == nil {
				ip = map[string]any{}
				d["initializeParams"] = ip
			}
			if i < len(saved) {
				sd, _ := saved[i].(map[string]any)
				ip["_seedKey"], ip["_seedImage"] = str(sd, "_storageKey"), str(sd, "_sourceImage")
				delete(ip, "sourceImage")
			}
			ip["diskName"] = str(c.body, "name")
			if i > 0 {
				ip["diskName"] = str(c.body, "name") + "-" + strconv.Itoa(i)
			}
		}
		return b, "sourceMachineImage", rel, nil
	}
	return c.body, "", "", nil
}

// machineImages serves global/machineImages: insert from a sourceInstance,
// get, list and delete.
func (s *Server) machineImages(c call) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/global/machineImages"
	switch {
	case n == 2 && c.method == http.MethodGet:
		return s.listOut("compute#machineImageList", coll, s.res, c.query)
	case n == 2 && c.method == http.MethodPost:
		return s.insertMachineImage(c, coll)
	}
	if n == 4 && c.method == http.MethodPost && p[3] == "setLabels" {
		return s.setLabels(c.project, "global", coll+"/"+p[2], c.body)
	}
	if n != 3 || (c.method != http.MethodGet && c.method != http.MethodDelete) {
		return nil, unimplemented("%s on machineImages/%s is not served", c.method, strings.Join(p[2:], "/"))
	}
	rel := coll + "/" + p[2]
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	switch c.method {
	case http.MethodGet:
		return o, nil
	case http.MethodDelete:
		if s.opts.VMs == nil {
			return nil, noVMs()
		}
		var keys []string
		for _, e := range listOf(o["savedDisks"]) {
			sd, _ := e.(map[string]any)
			keys = append(keys, str(sd, "_storageKey"))
		}
		o["status"] = "DELETING"
		return s.runOpLocked(c.project, "global", "delete", rel, func(ctx context.Context) error {
			for _, k := range keys {
				if err := s.opts.VMs.DeleteSnapshot(ctx, k); err != nil {
					return err
				}
			}
			return nil
		}, func(err error) {
			if err == nil {
				delete(s.res, rel)
			} else if cur, ok := s.res[rel]; ok {
				cur["status"] = "READY"
			}
		})
	}
	return nil, unimplemented("%s on machineImages/%s is not served", c.method, p[2])
}

func (s *Server) insertMachineImage(c call, coll string) (obj, error) {
	if s.opts.VMs == nil {
		return nil, noVMs()
	}
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := coll + "/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	srcRef := str(b, "sourceInstance")
	if srcRef == "" {
		return nil, required("resource.sourceInstance")
	}
	src := Rel(srcRef, c.project)
	vm, ok := s.res[src]
	if !ok || !strings.Contains(src, "/instances/") {
		return nil, notFound(src)
	}
	if err := s.checkDiskBudget(); err != nil {
		return nil, err
	}
	o := obj{"kind": "compute#machineImage", "id": newID(), "name": name, "selfLink": rel, "creationTimestamp": s.stamp(),
		"status": "CREATING", "sourceInstance": src, "instanceProperties": s.propertiesOfLocked(vm),
		"sourceInstanceProperties": s.propertiesOfLocked(vm), "guestFlush": b["guestFlush"] == true, "totalStorageBytes": "0",
		"labelFingerprint": labelFingerprint(nil)}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		if err := checkLabels("resource.labels", l); err != nil {
			return nil, err
		}
		o["labels"], o["labelFingerprint"] = l, labelFingerprint(l)
	}
	locs := strsAny(b["storageLocations"])
	if len(locs) == 0 {
		locs = []string{regionOfZone(lastSeg(str(vm, "zone")))}
	}
	o["storageLocations"] = toAny(locs)
	var specs []DiskSpec
	var keys []string
	var saved []any
	for i, e := range listOf(vm["disks"]) {
		ad, _ := e.(map[string]any)
		d, ok := s.res[str(ad, "source")]
		if !ok {
			continue
		}
		key := vmKey("mi", o) + "-" + strconv.Itoa(i)
		specs, keys = append(specs, s.diskSpecLocked(d)), append(keys, key)
		saved = append(saved, obj{"kind": "compute#savedDisk", "sourceDisk": str(d, "selfLink"), "architecture": arch(),
			"storageBytes": "0", "storageBytesStatus": "UP_TO_DATE", "_storageKey": key, "_sourceImage": s.diskImageLocked(d)})
	}
	o["savedDisks"] = saved
	s.res[rel] = norm(o)
	return s.runOpLocked(c.project, "global", "insert", rel, func(ctx context.Context) error {
		for i, sp := range specs {
			if err := s.opts.VMs.Snapshot(ctx, sp, keys[i]); err != nil {
				for _, k := range keys[:i] {
					_ = s.opts.VMs.DeleteSnapshot(context.Background(), k)
				}
				return err
			}
		}
		return nil
	}, func(err error) {
		if err != nil {
			delete(s.res, rel)
			return
		}
		if cur, ok := s.res[rel]; ok {
			cur["status"] = "READY"
		}
	})
}
