package compute

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Regional disks (#2206): regionDisks and regionDiskTypes. A regional disk
// is replicated synchronously between the two replica zones of its region;
// here it is one volume of the instance's cluster, so a VM in either
// replica zone that attaches it reads what the other wrote, as after a
// failover on Google
// (https://docs.cloud.google.com/compute/docs/disks/regional-persistent-disk).
// It is attached read-write to one VM at a time; attachDisk with
// forceAttach moves it to a VM in the other replica zone, the documented
// failover, the disk taken from the VM it was attached to.
//
// Asynchronous replication (async-pd) copies a primary disk to a secondary
// disk in the paired region, made with asyncPrimaryDisk; the secondary
// cannot be used while replication runs, so the copy is made when
// replication starts, at every point the secondary is read (a snapshot or
// clone of it) and when replication stops: the secondary then holds all
// that the primary had, which is within Google's documented RPO.
// Consistency groups are resource policies with a
// diskConsistencyGroupPolicy (resourcepolicies.go).

// regionDiskTypes are the disk types a regional disk can have, with their
// documented size limits in GB.
var regionDiskTypes = map[string]struct{ min, max int64 }{
	"pd-standard": {200, 65536}, "pd-balanced": {10, 65536}, "pd-ssd": {10, 65536},
	"hyperdisk-balanced-high-availability": {4, 65536},
}

// asyncTypes are the disk types Asynchronous Replication supports ("pd-
// standard" and the other Hyperdisk types are not).
var asyncTypes = map[string]bool{"pd-balanced": true, "pd-ssd": true, "hyperdisk-balanced": true,
	"hyperdisk-balanced-high-availability": true, "hyperdisk-extreme": true}

// continent is the continent Asynchronous Replication pairs a region in:
// "You can replicate a primary disk in a given region to a secondary disk
// in any available region within the same continent", for "Asia, except
// Indonesia", Europe, North America, Oceania and South America
// (async-pd/about, read 2026-11-24); "" for a region it does not serve.
func continent(region string) string {
	switch {
	case region == "asia-southeast2": // Jakarta, Indonesia
		return ""
	case strings.HasPrefix(region, "asia-"):
		return "Asia"
	case strings.HasPrefix(region, "europe-"):
		return "Europe"
	case strings.HasPrefix(region, "us-"), strings.HasPrefix(region, "northamerica-"):
		return "North America"
	case strings.HasPrefix(region, "australia-"):
		return "Oceania"
	case strings.HasPrefix(region, "southamerica-"):
		return "South America"
	}
	return ""
}

func asyncPair(a, b string) bool { return continent(a) != "" && continent(a) == continent(b) }

func regionRel(project, region, coll, name string) string {
	return "projects/" + project + "/regions/" + region + "/" + coll + "/" + name
}

func regionDiskTypeObj(project, region, name string) obj {
	d := regionDiskTypes[name]
	def := "100"
	if name == "pd-standard" {
		def = "500"
	}
	return obj{"kind": "compute#diskType", "id": stableID("r-" + name), "name": name, "region": "projects/" + project + "/regions/" + region,
		"description": "Regional " + name, "validDiskSize": fmt.Sprintf("%dGB-%dGB", d.min, d.max), "defaultDiskSizeGb": def,
		"selfLink": regionRel(project, region, "diskTypes", name), "creationTimestamp": "1969-12-31T16:00:00.000-08:00"}
}

// regionDiskTypesCall serves regionDiskTypes.list and get.
func (s *Server) regionDiskTypesCall(c call, region string) (obj, error) {
	if c.method != http.MethodGet || len(c.parts) > 4 {
		return nil, unimplemented("%s on regionDiskTypes", c.method)
	}
	if len(c.parts) == 3 {
		names := make([]string, 0, len(regionDiskTypes))
		for n := range regionDiskTypes {
			names = append(names, n)
		}
		sort.Strings(names)
		out, err := s.listFixed("compute#regionDiskTypeList", c, names, func(p, n string) obj { return regionDiskTypeObj(p, region, n) })
		if err == nil {
			out["selfLink"] = "projects/" + c.project + "/regions/" + region + "/diskTypes"
		}
		return out, err
	}
	if _, ok := regionDiskTypes[c.parts[3]]; !ok {
		return nil, notFound(regionRel(c.project, region, "diskTypes", c.parts[3]))
	}
	return regionDiskTypeObj(c.project, region, c.parts[3]), nil
}

// diskScope is the operation scope of a disk: zones/Z or regions/R.
func diskScope(d obj) string {
	if r := str(d, "region"); r != "" {
		return "regions/" + lastSeg(r)
	}
	return "zones/" + lastSeg(str(d, "zone"))
}

// diskRegion is the region a disk (zonal or regional) is in.
func diskRegion(d obj) string {
	if r := str(d, "region"); r != "" {
		return lastSeg(r)
	}
	return regionOfZone(lastSeg(str(d, "zone")))
}

// isRegional is whether d is a regional disk.
func isRegional(d obj) bool { return str(d, "region") != "" }

// replicaZones are a regional disk's replica zones, by name.
func replicaZones(d obj) []string {
	var out []string
	for _, z := range strs(d, "replicaZones") {
		out = append(out, lastSeg(z))
	}
	return out
}

// checkReplicaZones validates a regional disk's replicaZones: two
// distinct zones of its region.
func checkReplicaZones(project, region string, l []string, field string) ([]any, error) {
	if len(l) == 0 {
		return nil, required(field)
	}
	if len(l) != 2 {
		return nil, invalid("Invalid value for field '%s': %d zones. A regional disk must have exactly two replica zones.", field, len(l))
	}
	var out []any
	for i, z := range l {
		zn := lastSeg(Rel(z, project))
		if !IsZone(zn) || regionOfZone(zn) != region {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. Replica zones must be zones of the disk's region, %s.", field, i, z, region)
		}
		out = append(out, "projects/"+project+"/zones/"+zn)
	}
	if out[0] == out[1] {
		return nil, invalid("Invalid value for field '%s': the two replica zones must be distinct, got '%s' twice.", field, lastSeg(out[0].(string)))
	}
	return out, nil
}

// resolveDisk is the relative name of a disk reference (a URL, projects/...,
// regions/... or zones/...), and whether it names a disk.
func resolveDisk(ref, project string) (string, bool) {
	rel := Rel(ref, project)
	p := strings.Split(rel, "/")
	if len(p) != 6 || p[0] != "projects" || (p[2] != "zones" && p[2] != "regions") || p[4] != "disks" {
		return rel, false
	}
	return rel, true
}

// newRegionDiskLocked validates a regional disk's fields and returns it,
// not stored.
func (s *Server) newRegionDiskLocked(project, region string, b obj, field string) (obj, error) {
	name := str(b, "name")
	if err := checkName(field+".name", name); err != nil {
		return nil, err
	}
	rel := regionRel(project, region, "disks", name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	typ := "pd-balanced"
	if t := str(b, "type"); t != "" {
		typ = lastSeg(t)
		if strings.Contains(t, "/zones/") || strings.HasPrefix(t, "zones/") {
			return nil, invalid("Invalid value for field '%s.type': '%s'. A regional disk's type is a regional disk type (regions/%s/diskTypes/TYPE).", field, t, region)
		}
	}
	lim, ok := regionDiskTypes[typ]
	if !ok {
		return nil, invalid("Invalid value for field '%s.type': '%s'. The disk type is not valid for a regional disk: it is one of pd-standard, pd-balanced, pd-ssd or hyperdisk-balanced-high-availability.", field, str(b, "type"))
	}
	zones, err := checkReplicaZones(project, region, strs(b, "replicaZones"), field+".replicaZones")
	if err != nil {
		return nil, err
	}
	size, set, err := sizeGb(b["sizeGb"])
	if err != nil {
		return nil, err
	}
	d := obj{"kind": "compute#disk", "id": newID(), "name": name, "region": "projects/" + project + "/regions/" + region,
		"replicaZones": zones, "type": regionRel(project, region, "diskTypes", typ), "status": "CREATING", "selfLink": rel,
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
	sources := 0
	for _, k := range []string{"sourceImage", "sourceSnapshot", "sourceDisk"} {
		if str(b, k) != "" {
			sources++
		}
	}
	if sources > 1 {
		return nil, invalid("Invalid value for field '%s': give at most one of sourceImage, sourceSnapshot and sourceDisk.", field)
	}
	if ap, ok := b["asyncPrimaryDisk"].(map[string]any); ok && str(ap, "disk") != "" && sources > 0 {
		// "Secondary disks must be blank when created. They can't be
		// created from images, snapshots, or other disks."
		return nil, invalid("Invalid value for field '%s.asyncPrimaryDisk': secondary disks must be blank when created: they can't be created from images, snapshots, or other disks.", field)
	}
	switch {
	case str(b, "sourceImage") != "":
		img, err := LookupImage(str(b, "sourceImage"))
		if err != nil {
			return nil, err
		}
		d["sourceImage"], d["sourceImageId"] = imageRel(img), stableID(imageRel(img))
		d["licenses"] = []any{"projects/" + img.Project + "/global/licenses/" + img.Family}
		if !set {
			size = max(int64(img.DiskSizeGb), lim.min)
		}
		if size < int64(img.DiskSizeGb) {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Requested disk size cannot be smaller than the image size (%d GB)", field, size, img.DiskSizeGb)
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
		if l, ok := so["licenses"]; ok {
			d["licenses"] = l
		}
		ssz, _, _ := sizeGb(so["diskSizeGb"])
		if !set {
			size = max(ssz, lim.min)
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
		if diskRegion(so) != region {
			return nil, invalid("Invalid value for field '%s.sourceDisk': '%s'. A disk can be cloned only in its own region, %s.", field, src, diskRegion(so))
		}
		if str(so, "status") != "READY" {
			return nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", src)
		}
		if !isRegional(so) && !hasStr(replicaZones(d), lastSeg(str(so, "zone"))) {
			return nil, invalid("Invalid value for field '%s.replicaZones': a regional disk cloned from zonal disk '%s' must have the zone %s as one of its replica zones.",
				field, src, lastSeg(str(so, "zone")))
		}
		d["sourceDisk"], d["sourceDiskId"] = src, so["id"]
		if l, ok := so["licenses"]; ok {
			d["licenses"] = l
		}
		ssz, _, _ := sizeGb(so["sizeGb"])
		if !set {
			size = max(ssz, lim.min)
		}
		if size < ssz {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Requested disk size cannot be smaller than the source disk's size (%d GB)", field, size, ssz)
		}
	}
	if ap, ok := b["asyncPrimaryDisk"].(map[string]any); ok && str(ap, "disk") != "" {
		prel, pd, err := s.asyncPrimaryLocked(project, str(ap, "disk"), region, typ, field)
		if err != nil {
			return nil, err
		}
		psz, _, _ := sizeGb(pd["sizeGb"])
		if !set {
			size = psz
		}
		if size != psz {
			return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. The primary and secondary disk must have the same size: the primary disk is %d GB.", field, size, psz)
		}
		d["asyncPrimaryDisk"] = obj{"disk": prel, "diskId": pd["id"]}
		// "Secondary disk is created and is waiting for replication to
		// start" (AsyncReplicationStatus CREATED).
		d["resourceStatus"] = obj{"asyncPrimaryDisk": obj{"state": "CREATED"}}
	}
	if !set && size == 0 {
		size = 100
		if typ == "pd-standard" {
			size = 500
		}
	}
	if size < minSize || size > lim.max {
		return nil, invalid("Invalid value for field '%s.sizeGb': '%d'. Disk size must be between %d GB and %d GB for regional %s.", field, size, minSize, lim.max, typ)
	}
	d["sizeGb"] = strconv.FormatInt(size, 10)
	if pols := strs(b, "resourcePolicies"); len(pols) > 0 {
		l, err := s.checkDiskPoliciesLocked(project, d, pols, field+".resourcePolicies")
		if err != nil {
			return nil, err
		}
		d["resourcePolicies"] = l
	}
	return d, nil
}

// asyncPrimaryLocked resolves a secondary disk's asyncPrimaryDisk: an
// existing disk of a supported type, in a region paired with the
// secondary's, of the same type and scope.
func (s *Server) asyncPrimaryLocked(project, ref, region, typ, field string) (string, obj, error) {
	prel, ok := resolveDisk(ref, project)
	if !ok {
		return "", nil, invalid("Invalid value for field '%s.asyncPrimaryDisk.disk': '%s'.", field, ref)
	}
	pd, ok := s.res[prel]
	if !ok {
		return "", nil, notFound(prel)
	}
	ptyp := lastSeg(str(pd, "type"))
	if !asyncTypes[ptyp] {
		return "", nil, invalid("Invalid value for field '%s.asyncPrimaryDisk.disk': '%s'. Asynchronous Replication does not support %s disks.", field, prel, ptyp)
	}
	if !asyncTypes[typ] {
		return "", nil, invalid("Invalid value for field '%s.type': '%s'. Asynchronous Replication does not support %s disks.", field, typ, typ)
	}
	if pr := diskRegion(pd); !asyncPair(pr, region) {
		return "", nil, invalid("Invalid value for field '%s.asyncPrimaryDisk.disk': '%s'. A secondary disk must be in a region of its primary disk's continent: Asynchronous Replication does not pair %s and %s.", field, prel, pr, region)
	}
	if pd["asyncPrimaryDisk"] != nil {
		return "", nil, invalid("Invalid value for field '%s.asyncPrimaryDisk.disk': '%s'. That disk is itself a secondary disk.", field, prel)
	}
	return prel, pd, nil
}

// regionDisks serves regions/R/disks.
func (s *Server) regionDisks(c call, region string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/regions/" + region + "/disks"
	scope := "regions/" + region
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 3 && get:
		return s.listOut("compute#diskList", coll, s.res, c.query)
	case n == 3 && post:
		if err := s.checkDiskBudget(); err != nil {
			return nil, err
		}
		d, err := s.newRegionDiskLocked(c.project, region, c.body, "resource")
		if err != nil {
			return nil, err
		}
		return s.insertDiskLocked(c.project, scope, d)
	case n == 4 && post && p[3] == "bulkInsert":
		return s.bulkInsertDisks(c.project, scope, region, c.body)
	case n == 4 && post && p[3] == "stopGroupAsyncReplication":
		return s.stopGroupAsyncReplication(c.project, scope, region, c.body)
	}
	if n < 4 || (n == 4 && !get && !del) || (n == 5 && !(post && regionDiskVerbs[p[4]])) || n > 5 {
		return nil, unimplemented("%s on regionDisks/%s is not served (#2206)", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	d, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if n == 4 && get {
		return d, nil
	}
	if n == 4 && del {
		return s.deleteDiskLocked(c.project, scope, rel, d)
	}
	return s.diskVerbLocked(c, scope, rel, d)
}

var regionDiskVerbs = map[string]bool{"resize": true, "setLabels": true, "createSnapshot": true, "addResourcePolicies": true,
	"removeResourcePolicies": true, "startAsyncReplication": true, "stopAsyncReplication": true}

// insertDiskLocked stores a new disk and makes its storage: blank, from a
// snapshot, or a copy of its sourceDisk.
func (s *Server) insertDiskLocked(project, scope string, d obj) (obj, error) {
	rel := str(d, "selfLink")
	s.res[rel] = norm(d)
	spec := s.diskSpecLocked(s.res[rel])
	var src *DiskSpec
	var sync func(context.Context) error
	if sd := str(d, "sourceDisk"); sd != "" {
		if so, ok := s.res[sd]; ok {
			sp := s.diskSpecLocked(so)
			src = &sp
			sync = s.syncSecondaryLocked(so)
		}
	}
	return s.runOpLocked(project, scope, "insert", rel, func(ctx context.Context) error {
		if err := s.opts.VMs.CreateDisk(ctx, spec); err != nil {
			return err
		}
		if sync != nil {
			// A replicating secondary is cloned as replication has it.
			if err := sync(ctx); err != nil {
				return err
			}
		}
		if src != nil {
			return s.opts.VMs.CopyDisk(ctx, *src, spec)
		}
		return nil
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

// deleteDiskLocked deletes a disk that no VM uses and that is not
// replicating.
func (s *Server) deleteDiskLocked(project, scope, rel string, d obj) (obj, error) {
	if users := strs(d, "users"); len(users) > 0 {
		return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
			"The disk resource '%s' is already being used by '%s'", rel, users[0])
	}
	if peer := s.replicatingPeerLocked(d); peer != "" {
		return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
			"The disk resource '%s' is already being used by '%s': stop its asynchronous replication first.", rel, peer)
	}
	spec := s.diskSpecLocked(d)
	d["status"] = "DELETING"
	return s.runOpLocked(project, scope, "delete", rel, func(ctx context.Context) error {
		return s.opts.VMs.DeleteDisk(ctx, spec)
	}, func(err error) {
		if err == nil {
			delete(s.res, rel)
			s.forgetSecondaryLocked(rel)
		} else if o, ok := s.res[rel]; ok {
			o["status"] = "READY"
		}
	})
}

// resizeDiskLocked grows a disk, within its type's limit.
func (s *Server) resizeDiskLocked(project, scope, rel string, d, b obj) (obj, error) {
	size, set, err := sizeGb(b["sizeGb"])
	if err != nil {
		return nil, err
	}
	if !set {
		return nil, required("sizeGb")
	}
	cur, _, _ := sizeGb(d["sizeGb"])
	if size <= cur {
		return nil, invalid("Invalid value for field 'sizeGb': '%d'. New disk size '%d' GB must be larger than existing size '%d' GB.", size, size, cur)
	}
	lim := diskTypes[lastSeg(str(d, "type"))]
	if isRegional(d) {
		lim = regionDiskTypes[lastSeg(str(d, "type"))]
	}
	if size > lim.max {
		return nil, invalid("Invalid value for field 'sizeGb': '%d'. Disk size must be at most %d GB.", size, lim.max)
	}
	d["sizeGb"] = strconv.FormatInt(size, 10)
	specs := []DiskSpec{s.diskSpecLocked(d)}
	// "Resizing the primary disk automatically resizes the secondary disk"
	// (async-pd/manage-async-disks).
	if d["asyncPrimaryDisk"] == nil {
		if peer := s.replicatingPeerLocked(d); peer != "" {
			if sd, ok := s.res[peer]; ok {
				sd["sizeGb"] = d["sizeGb"]
				specs = append(specs, s.diskSpecLocked(sd))
			}
		}
	}
	return s.runOpLocked(project, scope, "resizeDisk", rel, func(ctx context.Context) error {
		for _, sp := range specs {
			if err := s.opts.VMs.ResizeDisk(ctx, sp); err != nil {
				return err
			}
		}
		return nil
	}, nil)
}
