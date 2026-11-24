package compute

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// The instance statuses (https://docs.cloud.google.com/compute/docs/instances/instance-life-cycle).
const (
	stProvisioning = "PROVISIONING"
	stRunning      = "RUNNING"
	stStopping     = "STOPPING"
	stTerminated   = "TERMINATED"
	stSuspending   = "SUSPENDING"
	stSuspended    = "SUSPENDED"
)

// vmSpecLocked is a stored instance as the backend sees it.
func (s *Server) vmSpecLocked(o obj) VMSpec {
	zone := lastSeg(str(o, "zone"))
	project := strings.Split(str(o, "selfLink"), "/")[1]
	mt, _ := LookupMachineType(lastSeg(str(o, "machineType")))
	sp := VMSpec{Key: vmKey("vm", o), Project: project, Zone: zone, Name: str(o, "name"), CPUs: mt.CPUs,
		MemoryMb: mt.MemoryMb, GuestToken: str(o, "_guestToken"), Hostname: str(o, "name")}
	if h := str(o, "hostname"); h != "" {
		sp.Hostname = h
	}
	if nics, _ := o["networkInterfaces"].([]any); len(nics) > 0 {
		nic, _ := nics[0].(map[string]any)
		sp.InternalIP = str(nic, "networkIP")
	}
	if sas, _ := o["serviceAccounts"].([]any); len(sas) > 0 {
		sa, _ := sas[0].(map[string]any)
		sp.ServiceAccount = str(sa, "email")
	}
	disks, _ := o["disks"].([]any)
	for _, e := range disks {
		ad, _ := e.(map[string]any)
		d, ok := s.res[str(ad, "source")]
		if !ok {
			continue
		}
		ds := s.diskSpecLocked(d)
		if b, _ := ad["boot"].(bool); b {
			sp.BootDisk = ds
			if ds.Image != nil {
				sp.Image = *ds.Image
			}
			continue
		}
		// A disk force-attached to another instance is fenced here: "Compute
		// Engine prevents the original instance from writing to the
		// regional disk" (repd-failover).
		fenced, _ := ad[fencedKey].(bool)
		sp.Disks = append(sp.Disks, AttachedDisk{Disk: ds, DeviceName: str(ad, "deviceName"), ReadOnly: str(ad, "mode") == "READ_ONLY" || fenced})
	}
	if sp.Image.Name == "" && sp.BootDisk.SourceSnapshot != "" {
		// A boot disk made from a snapshot runs the snapshot's image.
		for _, e := range disks {
			ad, _ := e.(map[string]any)
			if b, _ := ad["boot"].(bool); b {
				if d, ok := s.res[str(ad, "source")]; ok {
					if snap, ok := s.res[str(d, "sourceSnapshot")]; ok {
						if img, err := LookupImage(str(snap, "_sourceImage")); err == nil {
							sp.Image = img
							sp.BootDisk.Image = &img
						}
					}
				}
			}
		}
	}
	return sp
}

var deviceNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// attachedDiskLocked resolves one disks[] entry of an instance insert or
// attachDisk: an existing disk (source), or a new one (initializeParams),
// returned unstored with created true.
func (s *Server) attachedDiskLocked(project, zone, instance, machineType string, e map[string]any, index int, field string) (ad obj, newDisk obj, err error) {
	boot, _ := e["boot"].(bool)
	mode := str(e, "mode")
	if mode == "" {
		mode = "READ_WRITE"
	}
	if mode != "READ_WRITE" && mode != "READ_ONLY" {
		return nil, nil, invalid("Invalid value for field '%s.mode': '%s'.", field, mode)
	}
	if t := str(e, "type"); t != "" && t != "PERSISTENT" {
		return nil, nil, unimplemented("%s disks are not served: a VM's disks are persistent disks (#1963)", t)
	}
	var d obj
	switch {
	case str(e, "source") != "":
		rel := Rel(str(e, "source"), project)
		if !strings.Contains(rel, "/") {
			rel = zoneRel(project, zone, "disks", rel)
		}
		var ok bool
		if d, ok = s.res[rel]; !ok {
			return nil, nil, notFound(rel)
		}
		if isRegional(d) {
			if !hasStr(replicaZones(d), zone) {
				return nil, nil, invalid("Invalid value for field '%s.source': '%s'. You can attach regional disks only to instances that are in the same zones as the disk's replicas (%s).",
					field, rel, strings.Join(replicaZones(d), ", "))
			}
			if err := regionalMachineOK(d, machineType, field); err != nil {
				return nil, nil, err
			}
		} else if lastSeg(str(d, "zone")) != zone {
			return nil, nil, invalid("Invalid value for field '%s.source': '%s'. Disk must be in the same zone as the instance.", field, rel)
		}
		if str(d, "status") != "READY" {
			return nil, nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", rel)
		}
		if s.syncSecondaryLocked(d) != nil {
			return nil, nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The disk resource '%s' is a secondary disk in the process of replication: secondary disks can't be attached until replication stops.", rel)
		}
		force, _ := e["forceAttach"].(bool)
		if force && !isRegional(d) {
			return nil, nil, invalid("Invalid value for field '%s.forceAttach': 'true'. Force-attach applies to regional disks only: '%s' is a zonal disk.", field, rel)
		}
		users := strs(d, "users")
		if err := s.haAttachOK(d, rel, machineType, e, mode, boot, users, field); err != nil {
			return nil, nil, err
		}
		switch {
		case len(users) > 0 && mode == "READ_WRITE" && str(d, "accessMode") == rwMany && !force:
			// Multi-writer: each instance keeps write access.
		case len(users) > 0 && mode == "READ_WRITE" && force:
			for _, u := range users {
				if s.isBootOfLocked(u, rel) {
					return nil, nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
						"The disk resource '%s' is the boot disk of '%s': you can't simultaneously attach one boot disk to two instances.", rel, u)
				}
			}
		case len(users) > 0 && mode == "READ_WRITE":
			return nil, nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The disk resource '%s' is already being used by '%s'", rel, users[0])
		case isRegional(d) && mode == "READ_ONLY" && len(users) >= 10:
			return nil, nil, invalid("Invalid value for field '%s.source': '%s'. In read-only mode a regional disk can be attached to at most 10 instances.", field, rel)
		}
	case e["initializeParams"] != nil:
		ip, _ := e["initializeParams"].(map[string]any)
		name := str(ip, "diskName")
		if name == "" {
			name = instance
			if !boot {
				name = instance + "-" + strconv.Itoa(index)
			}
		}
		b := obj{"name": name, "sourceImage": str(ip, "sourceImage"), "sourceSnapshot": str(ip, "sourceSnapshot"),
			"type": str(ip, "diskType"), "sizeGb": ip["diskSizeGb"], "labels": ip["labels"], "description": str(ip, "description"),
			"provisionedIops": ip["provisionedIops"], "provisionedThroughput": ip["provisionedThroughput"]}
		if boot && str(ip, "sourceImage") == "" && str(ip, "sourceSnapshot") == "" {
			return nil, nil, required(field + ".initializeParams.sourceImage")
		}
		if rz := strs(ip, "replicaZones"); len(rz) > 0 {
			// A regional disk made with the instance.
			if t := str(ip, "diskType"); t != "" {
				b["type"] = lastSeg(t)
			}
			b["replicaZones"] = toAny(rz)
			if d, err = s.newRegionDiskLocked(project, regionOfZone(zone), b, field+".initializeParams"); err != nil {
				return nil, nil, err
			}
			if !hasStr(replicaZones(d), zone) {
				return nil, nil, invalid("Invalid value for field '%s.initializeParams.replicaZones': the instance's zone %s must be one of the replica zones.", field, zone)
			}
			if err := regionalMachineOK(d, machineType, field); err != nil {
				return nil, nil, err
			}
			if str(ip, "accessMode") == rwMany {
				return nil, nil, invalid("Invalid value for field '%s.initializeParams.accessMode': '%s'. A Hyperdisk volume in multi-writer mode can't be created during instance creation: create it, then attach it.", field, rwMany)
			}
			if err := s.haAttachOK(d, str(d, "selfLink"), machineType, e, mode, boot, nil, field); err != nil {
				return nil, nil, err
			}
		} else if d, err = s.newDiskLocked(project, zone, b, field+".initializeParams"); err != nil {
			return nil, nil, err
		}
		newDisk = d
	default:
		return nil, nil, required(field + ".source")
	}
	dev := str(e, "deviceName")
	if dev == "" {
		dev = str(d, "name")
	}
	if !deviceNameRE.MatchString(dev) || len(dev) > 63 {
		return nil, nil, invalid("Invalid value for field '%s.deviceName': '%s'.", field, dev)
	}
	autoDelete, _ := e["autoDelete"].(bool)
	if _, set := e["autoDelete"]; !set && newDisk != nil {
		autoDelete = true
	}
	ad = obj{"kind": "compute#attachedDisk", "type": "PERSISTENT", "mode": mode, "source": str(d, "selfLink"),
		"deviceName": dev, "index": float64(index), "boot": boot, "autoDelete": autoDelete, "interface": "SCSI",
		"diskSizeGb": d["sizeGb"], "architecture": arch()}
	if l, ok := d["licenses"]; ok {
		ad["licenses"] = l
	}
	if boot {
		ad["guestOsFeatures"] = []any{obj{"type": "UEFI_COMPATIBLE"}, obj{"type": "VIRTIO_SCSI_MULTIQUEUE"}, obj{"type": "GVNIC"}}
	}
	return ad, newDisk, nil
}

// nicLocked resolves one networkInterfaces[] entry: its subnet, and an
// internal IP reserved in it.
func (s *Server) nicLocked(project, zone string, e map[string]any, i int) (obj, error) {
	region := regionOfZone(zone)
	field := "resource.networkInterfaces[" + strconv.Itoa(i) + "]"
	sub := str(e, "subnetwork")
	if sub == "" {
		nw := str(e, "network")
		if nw == "" {
			nw = "default"
		}
		rel, ok := NetworkName(nw, project)
		if !ok {
			return nil, invalid("Invalid value for field '%s.network': '%s'.", field, nw)
		}
		no, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		if a, _ := no["autoCreateSubnetworks"].(bool); !a {
			return nil, invalid("Invalid value for field '%s.subnetwork': ''. Subnetwork should be specified for custom subnetmode network", field)
		}
		sub = subnetRel(project, region, lastSeg(rel))
	}
	tmp := obj{}
	if err := s.regionalInternal(project, region, obj{"subnetwork": sub}, tmp, "", str(e, "networkIP")); err != nil {
		return nil, err
	}
	nic := obj{"kind": "compute#networkInterface", "name": "nic" + strconv.Itoa(i), "network": tmp["network"],
		"subnetwork": tmp["subnetwork"], "networkIP": tmp["address"], "stackType": "IPV4_ONLY",
		"fingerprint": newFingerprint()}
	if acs, _ := e["accessConfigs"].([]any); len(acs) > 0 {
		var out []any
		for _, a := range acs {
			ac, _ := a.(map[string]any)
			name := str(ac, "name")
			if name == "" {
				name = "External NAT"
			}
			tier := str(ac, "networkTier")
			if tier == "" {
				tier = "PREMIUM"
			}
			// The instance's external address is this machine's loopback:
			// its SSH port is forwarded to a port on it (#1963).
			out = append(out, obj{"kind": "compute#accessConfig", "type": "ONE_TO_ONE_NAT", "name": name,
				"natIP": "127.0.0.1", "networkTier": tier})
		}
		nic["accessConfigs"] = out
	}
	return nic, nil
}

func (s *Server) instances(c call, zone string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/zones/" + zone + "/instances"
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 3 && get:
		out, err := s.listOut("compute#instanceList", coll, s.res, c.query)
		if err == nil {
			s.decorateList(out)
		}
		return out, err
	case n == 3 && post:
		return s.insertInstance(c, zone)
	}
	if n < 4 {
		return nil, unimplemented("%s on instances", c.method)
	}
	if (n == 4 && !get && !del) || (n == 5 && !servedInstanceVerbs[p[4]]) || n > 5 {
		return nil, unimplemented("%s on instances/%s is not served (#1963)", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	scope := "zones/" + zone
	verb := ""
	if n == 5 {
		verb = p[4]
	}
	status := str(o, "status")
	switch {
	case n == 4 && get:
		return s.decorate(o), nil
	case n == 4 && del:
		return s.deleteInstance(c.project, scope, rel, o)
	case get && verb == "serialPort":
		return s.serialPort(c, rel, o)
	case post && verb == "start":
		if status == stRunning {
			return s.done(c.project, scope, "start", rel, o["id"])
		}
		if status != stTerminated {
			return nil, notReady(rel, status)
		}
		return s.transition(c.project, scope, "start", rel, o, stProvisioning, stRunning, stTerminated, func(ctx context.Context, vm VMSpec) error {
			return s.opts.VMs.Start(ctx, vm)
		})
	case post && verb == "stop":
		if status == stTerminated {
			return s.done(c.project, scope, "stop", rel, o["id"])
		}
		if status != stRunning && status != stSuspended {
			return nil, notReady(rel, status)
		}
		return s.transition(c.project, scope, "stop", rel, o, stStopping, stTerminated, status, func(ctx context.Context, vm VMSpec) error {
			return s.opts.VMs.Stop(ctx, vm)
		})
	case post && verb == "reset":
		if status != stRunning {
			return nil, notReady(rel, status)
		}
		return s.transition(c.project, scope, "reset", rel, o, stRunning, stRunning, stRunning, func(ctx context.Context, vm VMSpec) error {
			if err := s.opts.VMs.Stop(ctx, vm); err != nil {
				return err
			}
			return s.opts.VMs.Start(ctx, vm)
		})
	case post && verb == "suspend":
		if status != stRunning {
			return nil, notReady(rel, status)
		}
		return s.transition(c.project, scope, "suspend", rel, o, stSuspending, stSuspended, stRunning, func(ctx context.Context, vm VMSpec) error {
			return s.opts.VMs.Suspend(ctx, vm)
		})
	case post && verb == "resume":
		if status != stSuspended {
			return nil, notReady(rel, status)
		}
		return s.transition(c.project, scope, "resume", rel, o, "RESUMING", stRunning, stSuspended, func(ctx context.Context, vm VMSpec) error {
			return s.opts.VMs.Resume(ctx, vm)
		})
	case post && verb == "setMetadata":
		cur, _ := o["metadata"].(map[string]any)
		if fp := str(c.body, "fingerprint"); fp != str(cur, "fingerprint") {
			return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
				"Supplied fingerprint does not match current metadata fingerprint.")
		}
		md, err := checkMetadata("metadata", c.body)
		if err != nil {
			return nil, err
		}
		o["metadata"] = norm(md)
		o["fingerprint"] = newFingerprint()
		return s.done(c.project, scope, "setMetadata", rel, o["id"])
	case post && verb == "setLabels":
		if _, ok := o["labelFingerprint"]; !ok {
			o["labelFingerprint"] = labelFingerprint(nil)
		}
		return s.setLabels(c.project, scope, rel, c.body)
	case post && verb == "setTags":
		cur, _ := o["tags"].(map[string]any)
		if fp := str(c.body, "fingerprint"); fp != str(cur, "fingerprint") {
			return nil, errf(http.StatusPreconditionFailed, "conditionNotMet", "Supplied fingerprint does not match current tags fingerprint.")
		}
		o["tags"] = obj{"items": c.body["items"], "fingerprint": newFingerprint()}
		return s.done(c.project, scope, "setTags", rel, o["id"])
	case post && verb == "setDeletionProtection":
		o["deletionProtection"] = first(c.query, "deletionProtection") != "false"
		return s.done(c.project, scope, "setDeletionProtection", rel, o["id"])
	case post && verb == "setMachineType":
		if status != stTerminated {
			return nil, errf(http.StatusBadRequest, "resourceNotReady",
				"The resource '%s' is not ready: the instance must be stopped (TERMINATED) to change its machine type, it is %s.", rel, status)
		}
		mt := lastSeg(str(c.body, "machineType"))
		if _, ok := LookupMachineType(mt); !ok {
			return nil, invalid("Invalid value for field 'machineType': '%s'. Machine type with name '%s' does not exist in zone '%s'.", str(c.body, "machineType"), mt, zone)
		}
		o["machineType"] = zoneRel(c.project, zone, "machineTypes", mt)
		return s.done(c.project, scope, "setMachineType", rel, o["id"])
	case post && verb == "attachDisk":
		return s.attachDisk(c, scope, rel, o)
	case post && verb == "detachDisk":
		return s.detachDisk(c, scope, rel, o)
	}
	return nil, unimplemented("%s on instances/%s is not served (#1963)", c.method, strings.Join(p[3:], "/"))
}

// servedInstanceVerbs are the instance methods served under instances/NAME/.
var servedInstanceVerbs = map[string]bool{"serialPort": true, "start": true, "stop": true, "reset": true, "suspend": true,
	"resume": true, "setMetadata": true, "setLabels": true, "setTags": true, "setDeletionProtection": true,
	"setMachineType": true, "attachDisk": true, "detachDisk": true}

func notReady(rel, status string) error {
	return errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready (it is %s)", rel, status)
}

// transition sets o's status to during, runs work, and sets after (or
// failed when work fails), unless another operation on the instance is
// queued behind this one: the status is then the later operation's to set.
func (s *Server) transition(project, scope, opType, rel string, o obj, during, after, failed string, work func(context.Context, VMSpec) error) (obj, error) {
	o["status"] = during
	vm := s.vmSpecLocked(o)
	return s.runOpLocked(project, scope, opType, rel, func(ctx context.Context) error { return work(ctx, vm) }, func(err error) {
		cur, ok := s.res[rel]
		if !ok || s.queued[rel] > 0 {
			return
		}
		if err != nil {
			cur["status"] = failed
			return
		}
		cur["status"] = after
		switch after {
		case stRunning:
			if opType != "resume" {
				cur["lastStartTimestamp"] = s.stamp()
			}
		case stTerminated:
			cur["lastStopTimestamp"] = s.stamp()
		case stSuspended:
			cur["lastSuspendedTimestamp"] = s.stamp()
		}
	})
}

func (s *Server) insertInstance(c call, zone string) (obj, error) {
	b := c.body
	if first(c.query, "sourceInstanceTemplate") != "" || str(b, "sourceMachineImage") != "" {
		return nil, unimplemented("instances made from instance templates or machine images are not served (#1963)")
	}
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := zoneRel(c.project, zone, "instances", name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	mtRef := str(b, "machineType")
	if mtRef == "" {
		return nil, required("resource.machineType")
	}
	mt := lastSeg(mtRef)
	if _, ok := LookupMachineType(mt); !ok {
		if strings.HasPrefix(mt, "custom-") {
			return nil, unimplemented("custom machine types are not served (#1963); use a predefined type")
		}
		return nil, invalid("Invalid value for field 'resource.machineType': '%s'. Machine type with name '%s' does not exist in zone '%s'.", mtRef, mt, zone)
	}
	for _, k := range []string{"guestAccelerators"} {
		if l, _ := b[k].([]any); len(l) > 0 {
			return nil, unimplemented("GPUs (guestAccelerators) are not served: a VM here is a container without a GPU (#1963)")
		}
	}
	if err := s.checkDiskBudget(); err != nil {
		return nil, err
	}
	in, _ := b["disks"].([]any)
	if len(in) == 0 {
		return nil, invalid("Invalid value for field 'resource.disks': ''. At least one disk, the boot disk, must be specified.")
	}
	var disks []any
	var newDisks []obj
	boots := 0
	names := map[string]bool{}
	for i, e := range in {
		em, _ := e.(map[string]any)
		if bt, _ := em["boot"].(bool); bt || (i == 0 && em["boot"] == nil) {
			em["boot"] = true
			boots++
		}
		ad, nd, err := s.attachedDiskLocked(c.project, zone, name, mt, em, i, "resource.disks["+strconv.Itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		if nd != nil {
			if names[str(nd, "name")] {
				return nil, invalid("Invalid value for field 'resource.disks[%d].initializeParams.diskName': '%s'. Disk names must be unique.", i, str(nd, "name"))
			}
			names[str(nd, "name")] = true
			newDisks = append(newDisks, nd)
		}
		disks = append(disks, ad)
	}
	if boots != 1 {
		return nil, invalid("Invalid value for field 'resource.disks': exactly one disk must be the boot disk.")
	}
	if ad := disks[0].(obj); !ad["boot"].(bool) {
		return nil, invalid("Invalid value for field 'resource.disks[0].boot': the boot disk must be the first disk.")
	}
	nicsIn, _ := b["networkInterfaces"].([]any)
	if len(nicsIn) == 0 {
		return nil, required("resource.networkInterfaces")
	}
	if len(nicsIn) > 1 {
		return nil, unimplemented("instances with more than one network interface are not served (#1963)")
	}
	var nics []any
	for i, e := range nicsIn {
		em, _ := e.(map[string]any)
		nic, err := s.nicLocked(c.project, zone, em, i)
		if err != nil {
			return nil, err
		}
		nics = append(nics, nic)
	}
	md, err := checkMetadata("resource.metadata", b["metadata"])
	if err != nil {
		return nil, err
	}
	o := obj{"kind": "compute#instance", "id": newID(), "name": name, "zone": "projects/" + c.project + "/zones/" + zone,
		"machineType": zoneRel(c.project, zone, "machineTypes", mt), "status": stProvisioning, "selfLink": rel,
		"creationTimestamp": s.stamp(), "disks": disks, "networkInterfaces": nics, "metadata": md,
		"fingerprint": newFingerprint(), "labelFingerprint": labelFingerprint(nil), "canIpForward": b["canIpForward"] == true,
		"deletionProtection": b["deletionProtection"] == true, "startRestricted": false, "cpuPlatform": "Unknown CPU Platform",
		"tags":        obj{"fingerprint": newFingerprint()},
		"scheduling":  obj{"onHostMaintenance": "MIGRATE", "automaticRestart": true, "preemptible": false, "provisioningModel": "STANDARD"},
		"_guestToken": randHex(16)}
	for _, k := range []string{"description", "hostname"} {
		if v := str(b, k); v != "" {
			o[k] = v
		}
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"], o["labelFingerprint"] = l, labelFingerprint(l)
	}
	if t, ok := b["tags"].(map[string]any); ok {
		o["tags"] = obj{"items": t["items"], "fingerprint": newFingerprint()}
	}
	if sc, ok := b["scheduling"].(map[string]any); ok {
		sched := o["scheduling"].(obj)
		for k, v := range sc {
			sched[k] = v
		}
	}
	if sas, ok := b["serviceAccounts"].([]any); ok {
		var out []any
		for _, e := range sas {
			sa, _ := e.(map[string]any)
			email := str(sa, "email")
			if email == "" || email == "default" {
				email = c.project + "-compute@developer.gserviceaccount.com"
			}
			out = append(out, obj{"email": email, "scopes": sa["scopes"]})
		}
		o["serviceAccounts"] = out
	}
	// Store the new disks and the instance, then make the disks and boot.
	var created []string
	for _, d := range newDisks {
		d["status"] = "READY"
		d["users"] = []any{rel}
		d["lastAttachTimestamp"] = s.stamp()
		s.res[str(d, "selfLink")] = norm(d)
		created = append(created, str(d, "selfLink"))
	}
	var fenced []string
	for i, e := range disks {
		src := str(e.(obj), "source")
		if d, ok := s.res[src]; ok && !hasStr(created, src) {
			if em, _ := in[i].(map[string]any); em["forceAttach"] == true && str(e.(obj), "mode") == "READ_WRITE" {
				fenced = append(fenced, s.fenceLocked(src)...)
			}
			d["users"] = append(toAny(strs(d, "users")), rel)
			d["lastAttachTimestamp"] = s.stamp()
		}
	}
	if err := s.remountFencedLocked(c.project, fenced); err != nil {
		return nil, err
	}
	s.res[rel] = norm(o)
	vm := s.vmSpecLocked(s.res[rel])
	var specs []DiskSpec
	for _, d := range created {
		specs = append(specs, s.diskSpecLocked(s.res[d]))
	}
	return s.runOpLocked(c.project, "zones/"+zone, "insert", rel, func(ctx context.Context) error {
		for i, d := range specs {
			if err := s.opts.VMs.CreateDisk(ctx, d); err != nil {
				for _, m := range specs[:i] {
					_ = s.opts.VMs.DeleteDisk(context.Background(), m)
				}
				return err
			}
		}
		if err := s.opts.VMs.Start(ctx, vm); err != nil {
			_ = s.opts.VMs.Stop(context.Background(), vm)
			for _, m := range specs {
				_ = s.opts.VMs.DeleteDisk(context.Background(), m)
			}
			return err
		}
		return nil
	}, func(err error) {
		if err != nil {
			delete(s.res, rel)
			for _, d := range created {
				delete(s.res, d)
			}
			s.releaseDisksLocked(rel)
			return
		}
		if cur, ok := s.res[rel]; ok {
			cur["status"], cur["lastStartTimestamp"] = stRunning, s.stamp()
		}
	})
}

func hasStr(l []string, v string) bool {
	for _, e := range l {
		if e == v {
			return true
		}
	}
	return false
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, v := range l {
		out[i] = v
	}
	return out
}

// releaseDisksLocked removes instance from every disk's users.
func (s *Server) releaseDisksLocked(instance string) {
	for k, d := range s.res {
		if !strings.Contains(k, "/disks/") {
			continue
		}
		users := strs(d, "users")
		if !hasStr(users, instance) {
			continue
		}
		var keep []any
		for _, u := range users {
			if u != instance {
				keep = append(keep, u)
			}
		}
		if len(keep) == 0 {
			delete(d, "users")
			d["lastDetachTimestamp"] = s.stamp()
		} else {
			d["users"] = keep
		}
	}
}

func (s *Server) deleteInstance(project, scope, rel string, o obj) (obj, error) {
	if dp, _ := o["deletionProtection"].(bool); dp {
		return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
			"Invalid resource usage: 'Resource cannot be deleted if it's protected against deletion.'.")
	}
	vm := s.vmSpecLocked(o)
	var autoDel []string
	var specs []DiskSpec
	disks, _ := o["disks"].([]any)
	for _, e := range disks {
		ad, _ := e.(map[string]any)
		if a, _ := ad["autoDelete"].(bool); a {
			if d, ok := s.res[str(ad, "source")]; ok && len(strs(d, "users")) <= 1 {
				autoDel = append(autoDel, str(ad, "source"))
				specs = append(specs, s.diskSpecLocked(d))
			}
		}
	}
	o["status"] = stStopping
	return s.runOpLocked(project, scope, "delete", rel, func(ctx context.Context) error {
		if err := s.opts.VMs.Stop(ctx, vm); err != nil {
			return err
		}
		for _, d := range specs {
			if err := s.opts.VMs.DeleteDisk(ctx, d); err != nil {
				return err
			}
		}
		return nil
	}, func(err error) {
		if err != nil {
			if cur, ok := s.res[rel]; ok {
				cur["status"] = stTerminated
			}
			return
		}
		delete(s.res, rel)
		s.releaseDisksLocked(rel)
		for _, d := range autoDel {
			delete(s.res, d)
		}
	})
}

// attachDisk adds a disk. A running instance's container is restarted with
// the disk mounted (a reboot: a container's mounts are fixed when it
// starts); a stopped instance has it at its next start.
func (s *Server) attachDisk(c call, scope, rel string, o obj) (obj, error) {
	disks, _ := o["disks"].([]any)
	if bt, _ := c.body["boot"].(bool); bt {
		return nil, invalid("Invalid value for field 'resource.boot': 'true'. An instance has one boot disk, attached at creation.")
	}
	if strings.EqualFold(first(c.query, "forceAttach"), "true") {
		c.body["forceAttach"] = true
	}
	ad, nd, err := s.attachedDiskLocked(c.project, lastSeg(str(o, "zone")), str(o, "name"), lastSeg(str(o, "machineType")), c.body, len(disks), "resource")
	if err != nil {
		return nil, err
	}
	if nd != nil {
		return nil, unimplemented("attachDisk with initializeParams is not served; create the disk with disks.insert, then attach it")
	}
	for _, e := range disks {
		if str(e.(map[string]any), "deviceName") == str(ad, "deviceName") {
			return nil, invalid("Invalid value for field 'resource.deviceName': '%s'. A disk with that device name is already attached.", str(ad, "deviceName"))
		}
	}
	if len(disks) >= 128 {
		return nil, invalid("Invalid value for field 'resource.disks': at most 128 disks may be attached.")
	}
	o["disks"] = append(disks, norm(ad))
	d := s.res[str(ad, "source")]
	var fenced []string
	if f, _ := c.body["forceAttach"].(bool); f && str(ad, "mode") == "READ_WRITE" {
		fenced = s.fenceLocked(str(ad, "source"))
	}
	d["users"] = append(toAny(strs(d, "users")), rel)
	d["lastAttachTimestamp"] = s.stamp()
	if err := s.remountFencedLocked(c.project, fenced); err != nil {
		return nil, err
	}
	return s.remount(c.project, scope, "attachDisk", rel, o)
}

func (s *Server) detachDisk(c call, scope, rel string, o obj) (obj, error) {
	dev := first(c.query, "deviceName")
	if dev == "" {
		return nil, required("deviceName")
	}
	disks, _ := o["disks"].([]any)
	var keep []any
	var gone string
	for _, e := range disks {
		ad := e.(map[string]any)
		if str(ad, "deviceName") == dev {
			if bt, _ := ad["boot"].(bool); bt {
				return nil, invalid("Invalid value for field 'deviceName': '%s'. The boot disk cannot be detached.", dev)
			}
			gone = str(ad, "source")
			continue
		}
		keep = append(keep, e)
	}
	if gone == "" {
		return nil, invalid("Invalid value for field 'deviceName': '%s'. No attached disk found with device name '%s'", dev, dev)
	}
	o["disks"] = keep
	if d, ok := s.res[gone]; ok {
		var users []any
		for _, u := range strs(d, "users") {
			if u != rel {
				users = append(users, u)
			}
		}
		if len(users) == 0 {
			delete(d, "users")
		} else {
			d["users"] = users
		}
		d["lastDetachTimestamp"] = s.stamp()
	}
	return s.remount(c.project, scope, "detachDisk", rel, o)
}

// remount restarts a running instance so its container has its disks.
func (s *Server) remount(project, scope, opType, rel string, o obj) (obj, error) {
	if str(o, "status") != stRunning {
		return s.done(project, scope, opType, rel, o["id"])
	}
	return s.transition(project, scope, opType, rel, o, stRunning, stRunning, stRunning, func(ctx context.Context, vm VMSpec) error {
		if err := s.opts.VMs.Stop(ctx, vm); err != nil {
			return err
		}
		return s.opts.VMs.Start(ctx, vm)
	})
}

// serialPort is getSerialPortOutput: port 1, the container's console.
func (s *Server) serialPort(c call, rel string, o obj) (obj, error) {
	port := first(c.query, "port")
	if port != "" && port != "1" {
		return nil, unimplemented("serial port %s is not served: a VM here has serial port 1, its console (#1963)", port)
	}
	text := ""
	if st := str(o, "status"); st == stRunning || st == stSuspended {
		vm := s.vmSpecLocked(o)
		s.mu.Unlock()
		out, err := s.opts.VMs.SerialOutput(context.Background(), vm)
		s.mu.Lock()
		if err != nil {
			return nil, errf(http.StatusInternalServerError, "backendError", "Cannot read the serial port output: %v", err)
		}
		text = out
	}
	start := int64(0)
	if v := first(c.query, "start"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, invalid("Invalid value for field 'start': '%s'.", v)
		}
		start = n
	}
	if start < 0 {
		start = 0
	}
	if start > int64(len(text)) {
		start = int64(len(text))
	}
	return obj{"kind": "compute#serialPortOutput", "contents": text[start:], "start": strconv.FormatInt(start, 10),
		"next": strconv.Itoa(len(text)), "selfLink": rel + "/serialPort"}, nil
}

// decorate is an instance with what is live: the SSH address its port 22
// is forwarded to.
func (s *Server) decorate(o obj) obj {
	if s.opts.VMs == nil || str(o, "status") != stRunning {
		return o
	}
	if a := s.opts.VMs.SSHAddress(s.vmSpecLocked(o)); a != "" {
		out := copyObj(o)
		out["cloudburrowSshAddress"] = a
		return out
	}
	return o
}

func (s *Server) decorateList(list obj) {
	items, _ := list["items"].([]obj)
	for i, it := range items {
		items[i] = s.decorate(it)
	}
}

// fencedKey marks an attached disk that was force-attached to another
// instance: the instance keeps it, read-only.
const fencedKey = "_fenced"

// regionalMachineOK applies the machine series a regional disk attaches
// to: "Regional Persistent Disk is supported on only E2, N1, N2, and N2D"
// (regional-persistent-disk, read 2026-11-24); f1-micro and g1-small are
// N1's shared-core types.
func regionalMachineOK(d obj, machineType, field string) error {
	if strings.HasPrefix(lastSeg(str(d, "type")), "hyperdisk-") || machineType == "" {
		return nil
	}
	series := machineType
	if i := strings.Index(series, "-"); i > 0 {
		series = series[:i]
	}
	switch series {
	case "e2", "n1", "n2", "n2d", "f1", "g1":
		return nil
	}
	return invalid("Invalid value for field '%s': machine type %s does not support Regional Persistent Disk, which is supported on only E2, N1, N2, and N2D machine types.", field, machineType)
}

// isBootOfLocked is whether disk is instance's boot disk.
func (s *Server) isBootOfLocked(instance, disk string) bool {
	o, ok := s.res[instance]
	if !ok {
		return false
	}
	l, _ := o["disks"].([]any)
	for _, e := range l {
		ad, _ := e.(map[string]any)
		if b, _ := ad["boot"].(bool); b && str(ad, "source") == disk {
			return true
		}
	}
	return false
}

// fenceLocked fences the regional disk on the instances it is attached to
// read-write, and returns them.
func (s *Server) fenceLocked(disk string) []string {
	d := s.res[disk]
	var out []string
	for _, u := range strs(d, "users") {
		o, ok := s.res[u]
		if !ok {
			continue
		}
		l, _ := o["disks"].([]any)
		for _, e := range l {
			ad, _ := e.(map[string]any)
			if str(ad, "source") == disk && str(ad, "mode") == "READ_WRITE" && ad[fencedKey] != true {
				ad[fencedKey] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// remountFencedLocked restarts the running fenced instances, so their
// guests have the disk read-only.
func (s *Server) remountFencedLocked(project string, instances []string) error {
	for _, u := range instances {
		o, ok := s.res[u]
		if !ok {
			continue
		}
		if _, err := s.remount(project, "zones/"+lastSeg(str(o, "zone")), "attachDisk", u, o); err != nil {
			return err
		}
	}
	return nil
}
