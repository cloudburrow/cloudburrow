package compute

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VM instances, disks and snapshots (#1963). Each mutation answers a zonal
// (or, for snapshots, global) operation that is RUNNING while the backend
// does the work and DONE, with its error when it failed, once it is done:
// zoneOperations.wait waits for it, as on Google.

// DefaultMinFreeBytes is the free disk space below which a new disk or
// instance is refused with RESOURCE_EXHAUSTED, as Kubernetes Engine refuses
// a new node: every disk is a directory of this machine's disk.
const DefaultMinFreeBytes = 20 << 30

// opWaitMax is how long zoneOperations.wait waits, as Google's wait returns
// after at most two minutes.
const opWaitMax = 2 * time.Minute

// opTimeout bounds one operation's backend work.
const opTimeout = 15 * time.Minute

var diskTypes = map[string]struct{ min, max int64 }{
	"pd-standard": {10, 65536}, "pd-balanced": {10, 65536}, "pd-ssd": {10, 65536},
	"pd-extreme": {500, 65536}, "hyperdisk-balanced": {4, 65536},
}

func noVMs() error {
	return unimplemented("VM instances and disks need the instance's Kubernetes cluster, which runs each VM as a systemd container (#1963); start CloudBurrow with the cluster to serve them.")
}

func zoneRel(project, zone, coll, name string) string {
	return "projects/" + project + "/zones/" + zone + "/" + coll + "/" + name
}

// vmKey is the backend name of a resource: its kind and ID, DNS-label safe.
func vmKey(prefix string, o obj) string { return prefix + "-" + str(o, "id") }

func (s *Server) minFree() uint64 {
	if s.opts.MinFreeBytes > 0 {
		return s.opts.MinFreeBytes
	}
	return DefaultMinFreeBytes
}

// checkDiskBudget refuses new storage below the free-space threshold.
func (s *Server) checkDiskBudget() error {
	free, err := s.opts.VMs.FreeBytes()
	if err != nil {
		return errf(http.StatusInternalServerError, "backendError", "Cannot read this machine's free disk space: %v", err)
	}
	if free < s.minFree() {
		return errf(http.StatusTooManyRequests, "resourceExhausted",
			"RESOURCE_EXHAUSTED: Not enough free disk space on this machine for another disk: %d GiB free, %d GiB required. Delete instances, disks or snapshots, or free disk space, then retry.",
			free>>30, s.minFree()>>30)
	}
	return nil
}

// runOpLocked records a RUNNING operation on target and runs work in the
// background, once target's earlier operations are done; finish, called
// with s.mu held, records the outcome on the resources. It returns the
// operation.
func (s *Server) runOpLocked(project, scope, opType, target string, work func(context.Context) error, finish func(error)) (obj, error) {
	op := s.newOpLocked(project, scope, opType, target)
	op["status"], op["progress"] = "RUNNING", 0.0
	delete(op, "endTime")
	rel := str(op, "selfLink")
	done := make(chan struct{})
	s.waiters[rel] = done
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	prev := s.queue[target]
	s.queue[target] = done
	s.queued[target]++
	out := copyObj(op)
	go func() {
		if prev != nil {
			<-prev
		}
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		err := work(ctx)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.queued[target]--; s.queued[target] == 0 {
			delete(s.queued, target)
		}
		if s.queue[target] == done {
			delete(s.queue, target)
		}
		if finish != nil {
			finish(err)
		}
		if o, ok := s.ops[rel]; ok {
			o["status"], o["progress"], o["endTime"] = "DONE", 100.0, s.stamp()
			if err != nil {
				code, msg := "RESOURCE_OPERATION_FAILED", err.Error()
				if ae, ok := err.(*apiError); ok {
					code = strings.ToUpper(ae.reason)
					o["httpErrorStatusCode"] = float64(ae.code)
				} else {
					o["httpErrorStatusCode"] = 400.0
				}
				o["httpErrorMessage"] = "BAD REQUEST"
				o["error"] = obj{"errors": []any{obj{"code": code, "message": msg}}}
			}
		}
		delete(s.waiters, rel)
		close(done)
		_ = s.saveLocked()
	}()
	return out, nil
}

func copyObj(o obj) obj {
	out := make(obj, len(o))
	for k, v := range o {
		out[k] = v
	}
	return out
}

// waitOp blocks, without the lock, until the operation rel is done or
// opWaitMax passes.
func (s *Server) waitOp(ctx context.Context, rel string) {
	s.mu.Lock()
	ch := s.waiters[rel]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	t := time.NewTimer(opWaitMax)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
}

// metadataItems is a metadata object's items as a map.
func metadataItems(m any) map[string]string {
	out := map[string]string{}
	mo, _ := m.(map[string]any)
	l, _ := mo["items"].([]any)
	for _, e := range l {
		if it, ok := e.(map[string]any); ok {
			v, _ := it["value"].(string)
			out[str(it, "key")] = v
		}
	}
	return out
}

func metadataFingerprint(items any) string { return labelFingerprint(obj{"items": fmt.Sprint(items)}) }

// checkMetadata validates metadata items as Google does: unique keys of
// letters, digits, - and _, at most 128 bytes; values at most 256 KB, 512
// KB in all.
func checkMetadata(field string, m any) (obj, error) {
	mo, _ := m.(map[string]any)
	l, _ := mo["items"].([]any)
	seen := map[string]bool{}
	total := 0
	items := []any{}
	for i, e := range l {
		it, _ := e.(map[string]any)
		k := str(it, "key")
		v, _ := it["value"].(string)
		if k == "" || len(k) > 128 || strings.Trim(k, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			return nil, invalid("Invalid value for field '%s.items[%d].key': '%s'. Must be a match of regex '[a-zA-Z0-9-_]{1,128}'", field, i, k)
		}
		if seen[k] {
			return nil, invalid("Invalid value for field '%s.items[%d].key': '%s'. Duplicate keys are not allowed.", field, i, k)
		}
		seen[k] = true
		if len(v) > 256<<10 {
			return nil, invalid("Invalid value for field '%s.items[%d].value': Value is too long: the limit is 262144 bytes.", field, i)
		}
		total += len(k) + len(v)
		items = append(items, obj{"key": k, "value": v})
	}
	if total > 512<<10 {
		return nil, invalid("Invalid value for field '%s': The total size of all metadata is limited to 512 KB.", field)
	}
	out := obj{"kind": "compute#metadata", "items": items}
	out["fingerprint"] = metadataFingerprint(items)
	if len(items) == 0 {
		delete(out, "items")
	}
	return out, nil
}

// zonal serves zones/Z/{instances,disks,machineTypes,diskTypes}.
func (s *Server) zonal(c call, zone string) (obj, error) {
	switch c.parts[2] {
	case "machineTypes":
		return s.machineTypes(c, zone)
	case "diskTypes":
		return s.diskTypesCall(c, zone)
	}
	if s.opts.VMs == nil {
		return nil, noVMs()
	}
	switch c.parts[2] {
	case "instances":
		return s.instances(c, zone)
	case "disks":
		return s.disks(c, zone)
	case "instanceGroups":
		return s.instanceGroups(c, zoneLoc(c.project, zone))
	case "instanceGroupManagers":
		return s.instanceGroupManagers(c, zoneLoc(c.project, zone))
	case "autoscalers":
		return s.autoscalers(c, zoneLoc(c.project, zone))
	}
	return nil, unimplemented("zones/%s/%s is not served (#1963)", zone, c.parts[2])
}

func diskTypeObj(project, zone, name string) obj {
	d := diskTypes[name]
	return obj{"kind": "compute#diskType", "id": stableID(name), "name": name, "zone": "projects/" + project + "/zones/" + zone,
		"description": name, "validDiskSize": fmt.Sprintf("%dGB-%dGB", d.min, d.max), "defaultDiskSizeGb": "100",
		"selfLink": zoneRel(project, zone, "diskTypes", name), "creationTimestamp": "1969-12-31T16:00:00.000-08:00"}
}

func (s *Server) diskTypesCall(c call, zone string) (obj, error) {
	if c.method != http.MethodGet {
		return nil, unimplemented("%s on diskTypes", c.method)
	}
	if len(c.parts) == 3 {
		names := make([]string, 0, len(diskTypes))
		for n := range diskTypes {
			names = append(names, n)
		}
		sort.Strings(names)
		return s.listFixed("compute#diskTypeList", c, names, func(p, n string) obj { return diskTypeObj(p, zone, n) })
	}
	if _, ok := diskTypes[c.parts[3]]; !ok || len(c.parts) != 4 {
		return nil, notFound(zoneRel(c.project, zone, "diskTypes", c.parts[3]))
	}
	return diskTypeObj(c.project, zone, c.parts[3]), nil
}

// sizeGb is a sizeGb field, a string or a number.
func sizeGb(v any) (int64, bool, error) {
	switch t := v.(type) {
	case nil:
		return 0, false, nil
	case float64:
		return int64(t), true, nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, false, invalid("Invalid value for field 'sizeGb': '%s'.", t)
		}
		return n, true, nil
	}
	return 0, false, invalid("Invalid value for field 'sizeGb'.")
}

// diskSpecLocked is a stored disk as the backend sees it.
func (s *Server) diskSpecLocked(d obj) DiskSpec {
	n, _, _ := sizeGb(d["sizeGb"])
	sp := DiskSpec{Key: vmKey("disk", d), SizeGb: n, Type: lastSeg(str(d, "type")), rel: str(d, "selfLink"), DataKey: s.specKeyLocked(d)}
	if si := str(d, "sourceImage"); si != "" {
		if img, err := LookupImage(si); err == nil {
			sp.Image = &img
		}
	}
	if ss := str(d, "sourceSnapshot"); ss != "" {
		if so, ok := s.res[ss]; ok {
			sp.SourceSnapshot = vmKey("snap", so)
			sp.SourceDataKey = s.specKeyLocked(so)
		}
	}
	// A disk made from a custom image, a machine image or another disk
	// (#2139): a copy of that storage, running the public image it came
	// from.
	if k := str(d, "_seedKey"); k != "" {
		sp.SourceSnapshot = k
	}
	if si := str(d, "_seedImage"); si != "" && sp.Image == nil {
		if img, err := LookupImage(si); err == nil {
			sp.Image = &img
		}
	}
	return sp
}

// newDiskLocked validates a disk's fields and returns it, not stored.
func (s *Server) newDiskLocked(project, zone string, b obj, field string) (obj, error) {
	name := str(b, "name")
	if err := checkName(field+".name", name); err != nil {
		return nil, err
	}
	rel := zoneRel(project, zone, "disks", name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	typ := "pd-balanced"
	if t := str(b, "type"); t != "" {
		typ = lastSeg(t)
	}
	lim, ok := diskTypes[typ]
	if !ok {
		return nil, invalid("Invalid value for field '%s.type': '%s'. The disk type is not valid.", field, str(b, "type"))
	}
	size, set, err := sizeGb(b["sizeGb"])
	if err != nil {
		return nil, err
	}
	d := obj{"kind": "compute#disk", "id": newID(), "name": name, "zone": "projects/" + project + "/zones/" + zone,
		"type": zoneRel(project, zone, "diskTypes", typ), "status": "CREATING", "selfLink": rel,
		"creationTimestamp": s.stamp(), "physicalBlockSizeBytes": "4096", "architecture": arch(),
		"labelFingerprint": labelFingerprint(nil)}
	if desc := str(b, "description"); desc != "" {
		d["description"] = desc
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		d["labels"] = l
		d["labelFingerprint"] = labelFingerprint(l)
	}
	minSize := lim.min
	if ap, ok := b["asyncPrimaryDisk"].(map[string]any); ok && str(ap, "disk") != "" &&
		(str(b, "sourceImage") != "" || str(b, "sourceSnapshot") != "" || str(b, "sourceDisk") != "") {
		return nil, invalid("Invalid value for field '%s.asyncPrimaryDisk': secondary disks must be blank when created: they can't be created from images, snapshots, or other disks.", field)
	}
	switch {
	case str(b, "sourceImage") != "":
		img, custom, err := s.imageSeedLocked(str(b, "sourceImage"), project)
		if err != nil {
			return nil, err
		}
		imgSize := int64(img.DiskSizeGb)
		d["sourceImage"], d["sourceImageId"] = imageRel(img), stableID(imageRel(img))
		if custom != nil {
			d["sourceImage"], d["sourceImageId"], d["_seedImage"] = str(custom, "selfLink"), custom["id"], imageRel(img)
			if k := str(custom, "_storageKey"); k != "" {
				d["_seedKey"] = k
			}
			imgSize, _, _ = sizeGb(custom["diskSizeGb"])
		}
		d["licenses"] = []any{"projects/" + img.Project + "/global/licenses/" + img.Family}
		if imgSize > minSize {
			minSize = imgSize
		}
		if !set {
			size = imgSize
		}
		if size < imgSize {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Requested disk size cannot be smaller than the image size (%d GB)", field, size, imgSize)
		}
	case str(b, "sourceSnapshot") != "":
		ss := Rel(str(b, "sourceSnapshot"), project)
		if !strings.Contains(ss, "/") {
			ss = "projects/" + project + "/global/snapshots/" + ss
		}
		so, ok := s.res[ss]
		if !ok {
			return nil, notFound(ss)
		}
		if str(so, "status") != "READY" {
			return nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", ss)
		}
		d["sourceSnapshot"], d["sourceSnapshotId"] = ss, so["id"]
		ssz, _, _ := sizeGb(so["diskSizeGb"])
		if !set {
			size = ssz
		}
		if size < ssz {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Requested disk size cannot be smaller than the snapshot size (%d GB)", field, size, ssz)
		}
	case str(b, "sourceDisk") != "":
		src, ok := resolveDisk(str(b, "sourceDisk"), project)
		if !ok {
			return nil, invalid("Invalid value for field '%s.sourceDisk': '%s'.", field, str(b, "sourceDisk"))
		}
		so, ok := s.res[src]
		if !ok {
			return nil, notFound(src)
		}
		// A zonal disk is cloned in its zone; a regional disk into one of
		// its replica zones.
		if (isRegional(so) && !hasStr(replicaZones(so), zone)) || (!isRegional(so) && lastSeg(str(so, "zone")) != zone) {
			return nil, invalid("Invalid value for field '%s.sourceDisk': '%s'. A zonal disk can be cloned only from a disk in its own zone (or a regional disk replicated in it).", field, src)
		}
		if str(so, "status") != "READY" {
			return nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", src)
		}
		d["sourceDisk"], d["sourceDiskId"] = src, so["id"]
		// The clone runs the public image its source came from; its
		// storage is a copy of the source's (insertDiskLocked).
		if si := s.diskImageLocked(so); si != "" {
			d["_seedImage"] = si
		}
		if l, ok := so["licenses"]; ok {
			d["licenses"] = l
		}
		ssz, _, _ := sizeGb(so["sizeGb"])
		if !set {
			size = ssz
		}
		if size < ssz {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Requested disk size cannot be smaller than the source disk's size (%d GB)", field, size, ssz)
		}
	}
	if ap, ok := b["asyncPrimaryDisk"].(map[string]any); ok && str(ap, "disk") != "" {
		prel, pd, err := s.asyncPrimaryLocked(project, str(ap, "disk"), regionOfZone(zone), typ, field)
		if err != nil {
			return nil, err
		}
		psz, _, _ := sizeGb(pd["sizeGb"])
		if !set {
			size, set = psz, true
		}
		if size != psz {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. The primary and secondary disk must have the same size: the primary disk is %d GB.", field, size, psz)
		}
		d["asyncPrimaryDisk"] = obj{"disk": prel, "diskId": pd["id"]}
		d["resourceStatus"] = obj{"asyncPrimaryDisk": obj{"state": "CREATED"}}
	}
	if k := str(b, "_seedKey"); k != "" && d["sourceImage"] == nil && d["sourceSnapshot"] == nil && d["sourceDisk"] == nil {
		// A machine image's saved disk (templates.go), copied.
		d["_seedKey"] = k
		if si := str(b, "_seedImage"); si != "" {
			d["_seedImage"] = si
			if img, err := LookupImage(si); err == nil {
				d["licenses"] = []any{"projects/" + img.Project + "/global/licenses/" + img.Family}
				if !set && size < int64(img.DiskSizeGb) {
					size = int64(img.DiskSizeGb)
				}
			}
		}
	}
	if !set && size == 0 {
		size = 100
		if typ == "pd-standard" {
			size = 500
		}
	}
	if size < minSize || size > lim.max {
		return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Disk size must be between %d GB and %d GB for %s.", field, size, minSize, lim.max, typ)
	}
	d["sizeGb"] = strconv.FormatInt(size, 10)
	if err := s.finishNewDiskLocked(d, b, field); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Server) disks(c call, zone string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/zones/" + zone + "/disks"
	scope := "zones/" + zone
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	patch := c.method == http.MethodPatch
	switch {
	case n == 3 && get:
		return s.listOut("compute#diskList", coll, s.res, c.query)
	case n == 3 && post:
		if err := s.checkDiskBudget(); err != nil {
			return nil, err
		}
		if t := str(c.body, "sourceImage"); t == "" {
			if si := first(c.query, "sourceImage"); si != "" {
				c.body["sourceImage"] = si
			}
		}
		d, err := s.newDiskLocked(c.project, zone, c.body, "resource")
		if err != nil {
			return nil, err
		}
		if pols := strs(c.body, "resourcePolicies"); len(pols) > 0 {
			l, err := s.checkDiskPoliciesLocked(c.project, d, pols, "resource.resourcePolicies")
			if err != nil {
				return nil, err
			}
			d["resourcePolicies"] = l
		}
		return s.insertDiskLocked(c.project, scope, d)
	case n == 4 && post && p[3] == "bulkInsert":
		return s.bulkInsertDisks(c.project, scope, regionOfZone(zone), c.body)
	case n == 4 && post && p[3] == "stopGroupAsyncReplication":
		return s.stopGroupAsyncReplication(c.project, scope, regionOfZone(zone), c.body)
	}
	if n < 4 || (n == 4 && !get && !del && !patch) || (n == 5 && !(post && regionDiskVerbs[p[4]])) || n > 5 {
		return nil, unimplemented("%s on disks/%s is not served (#1963)", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	d, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	switch {
	case n == 4 && get:
		return d, nil
	case n == 4 && del:
		return s.deleteDiskLocked(c.project, scope, rel, d)
	case n == 4 && patch:
		return s.updateDiskLocked(c, scope, rel, d)
	}
	return s.diskVerbLocked(c, scope, rel, d)
}

// diskVerbLocked serves a POST verb on a zonal or regional disk.
func (s *Server) diskVerbLocked(c call, scope, rel string, d obj) (obj, error) {
	switch c.parts[4] {
	case "resize":
		return s.resizeDiskLocked(c.project, scope, rel, d, c.body)
	case "setLabels":
		return s.setLabels(c.project, scope, rel, c.body)
	case "createSnapshot":
		return s.createSnapshot(c.project, rel, c.body, scope)
	case "addResourcePolicies", "removeResourcePolicies":
		return s.diskPolicies(c.project, scope, rel, d, c.body, c.parts[4] == "addResourcePolicies")
	case "startAsyncReplication":
		return s.startAsyncReplication(c.project, scope, rel, d, c.body)
	case "stopAsyncReplication":
		return s.stopAsyncReplication(c.project, scope, rel, d)
	case "updateKmsKey":
		return s.updateDiskKmsKeyLocked(c.project, scope, rel, d, c.body)
	}
	return nil, unimplemented("%s on %s/%s is not served (#2206)", c.method, rel, c.parts[4])
}

var servedDiskVerbs = map[string]bool{"resize": true, "setLabels": true, "createSnapshot": true,
	"addResourcePolicies": true, "removeResourcePolicies": true}

// setLabelsAsync is setLabels answered with a zonal operation.
func (s *Server) setLabelsAsync(project, scope, rel string, b obj) (obj, error) {
	return s.setLabels(project, scope, rel, b)
}

func (s *Server) createSnapshot(project, diskRel string, b obj, scope string) (obj, error) {
	if err := s.checkDiskBudget(); err != nil {
		return nil, err
	}
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := "projects/" + project + "/global/snapshots/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	d := s.res[diskRel]
	if str(d, "status") != "READY" {
		return nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", diskRel)
	}
	if s.syncSecondaryLocked(d) != nil {
		// "Secondary disks can't be attached, deleted, or snapshotted
		// while they are in the process of replication" (async-pd/about).
		return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
			"The disk resource '%s' is a secondary disk in the process of replication: it can't be snapshotted until replication stops.", diskRel)
	}
	// "You can't ... create a snapshot from the disk" with a revoked key;
	// a customer-supplied key is given as sourceDiskEncryptionKey.
	sk, _ := b["sourceDiskEncryptionKey"].(map[string]any)
	dek, err := s.diskKeyLocked(d, sk, "resource.sourceDiskEncryptionKey")
	if err != nil {
		return nil, err
	}
	snap := obj{"kind": "compute#snapshot", "id": newID(), "name": name, "status": "CREATING", "selfLink": rel,
		"sourceDisk": diskRel, "sourceDiskId": d["id"], "diskSizeGb": d["sizeGb"], "storageBytes": "0",
		"storageBytesStatus": "UP_TO_DATE", "storageLocations": []any{diskRegion(d)},
		"creationTimestamp": s.stamp(), "architecture": arch(), "snapshotType": "STANDARD",
		"labelFingerprint": labelFingerprint(nil)}
	// Snapshot type and location (#2139, compute/docs/disks/create-snapshots):
	// an archive snapshot is kept as a standard one is.
	switch st := str(b, "snapshotType"); st {
	case "", "STANDARD", "ARCHIVE":
		if st != "" {
			snap["snapshotType"] = st
		}
	default:
		return nil, invalid("Invalid value for field 'resource.snapshotType': '%s'. Must be STANDARD or ARCHIVE.", st)
	}
	if locs := strsAny(b["storageLocations"]); len(locs) > 0 {
		if len(locs) > 1 {
			return nil, invalid("Invalid value for field 'resource.storageLocations': a snapshot has one storage location.")
		}
		snap["storageLocations"] = toAny(locs)
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		snap["labels"], snap["labelFingerprint"] = l, labelFingerprint(l)
	}
	if desc := str(b, "description"); desc != "" {
		snap["description"] = desc
	}
	if l, ok := b["licenses"]; ok {
		snap["licenses"] = l
	} else if l, ok := d["licenses"]; ok {
		snap["licenses"] = l
	}
	if si := str(d, "sourceImage"); si != "" {
		snap["_sourceImage"] = si
	}
	// A CMEK disk's snapshot is "encrypted with the same key" (#2206); a
	// CSEK disk's with snapshotEncryptionKey, or the service's own key.
	if err := s.snapshotKeyLocked(snap, d, b, dek); err != nil {
		return nil, err
	}
	if v, _ := b[VaultSnapshot].(bool); v {
		// A Backup and DR vault's (#2075): kept out of the project's own
		// snapshots, as a vaulted backup is not one of them.
		snap[VaultSnapshot] = true
	}
	s.res[rel] = norm(snap)
	spec := s.diskSpecLocked(d)
	key := vmKey("snap", s.res[rel])
	return s.runOpLocked(project, scope, "createSnapshot", diskRel, func(ctx context.Context) error {
		return s.opts.VMs.Snapshot(ctx, spec, key)
	}, func(err error) {
		if err != nil {
			delete(s.res, rel)
			return
		}
		if o, ok := s.res[rel]; ok {
			o["status"] = "READY"
		}
	})
}

// VaultSnapshot marks a snapshot a Backup and DR vault holds (#2075): set
// in an in-process createSnapshot's body, and the query parameter that
// reaches it with snapshots.get and delete.
const VaultSnapshot = "_backupdr"

func regionOfZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// snapshots serves global/snapshots: list, get, delete, insert (from a
// sourceDisk) and setLabels.
func (s *Server) snapshots(c call) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/global/snapshots"
	switch {
	case n == 2 && c.method == http.MethodGet:
		own := map[string]obj{}
		for k, o := range s.res {
			if v, _ := o[VaultSnapshot].(bool); !v && strings.HasPrefix(k, coll+"/") {
				own[k] = o
			}
		}
		return s.listOut("compute#snapshotList", coll, own, c.query)
	case n == 2 && c.method == http.MethodPost:
		if s.opts.VMs == nil {
			return nil, noVMs()
		}
		src := Rel(str(c.body, "sourceDisk"), c.project)
		if src == "" {
			return nil, required("resource.sourceDisk")
		}
		if _, ok := s.res[src]; !ok {
			return nil, notFound(src)
		}
		return s.createSnapshot(c.project, src, c.body, "global")
	}
	if n < 3 || (n == 3 && c.method != http.MethodGet && c.method != http.MethodDelete) || (n == 4 && p[3] != "setLabels") || n > 4 {
		return nil, unimplemented("%s on snapshots/%s is not served (#1963)", c.method, strings.Join(p[2:], "/"))
	}
	rel := coll + "/" + p[2]
	o, ok := s.res[rel]
	if v, _ := o[VaultSnapshot].(bool); v && first(c.query, VaultSnapshot) == "" {
		ok = false // only the vault reaches its snapshots
	}
	if !ok {
		return nil, notFound(rel)
	}
	switch {
	case n == 3 && c.method == http.MethodGet:
		return o, nil
	case n == 3 && c.method == http.MethodDelete:
		if s.opts.VMs == nil {
			return nil, noVMs()
		}
		key := vmKey("snap", o)
		o["status"] = "DELETING"
		return s.runOpLocked(c.project, "global", "delete", rel, func(ctx context.Context) error {
			return s.opts.VMs.DeleteSnapshot(ctx, key)
		}, func(err error) {
			if err == nil {
				delete(s.res, rel)
			} else if o, ok := s.res[rel]; ok {
				o["status"] = "READY"
			}
		})
	case n == 4 && c.method == http.MethodPost && p[3] == "setLabels":
		return s.setLabels(c.project, "global", rel, c.body)
	}
	return nil, unimplemented("%s on snapshots/%s is not served (#1963)", c.method, strings.Join(p[2:], "/"))
}
