package compute

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Regional disks (#2139, https://docs.cloud.google.com/compute/docs/disks/regional-persistent-disk,
// .../high-availability-regional-persistent-disk and the regionDisks REST
// reference, read 2026-11-22): regionDisks insert, get, list, delete,
// resize, setLabels, createSnapshot and add/removeResourcePolicies, and
// regionDiskTypes get and list. A regional disk has exactly two replica
// zones, both of its region; its type is pd-balanced, pd-ssd or
// pd-standard (pd-standard at least 200 GB, the others at least 10 GB); a
// VM of either replica zone attaches it (instances.insert and attachDisk
// with source regions/R/disks/D).
//
// Replication here: the region's zones are modelled on one cluster, so a
// regional disk is one storage volume (the backend's disk, a
// PersistentVolumeClaim) that the VMs of both replica zones mount. Every
// write is on the one copy, so the two replicas are always in sync, as
// Google's synchronous replication keeps them; a zone outage, and with it
// a replica's failover (regionDisks.stopAsyncReplication, force-attach),
// is not simulated.

// regionalDiskTypes are the disk types a regional disk takes, with their
// regional size limits.
var regionalDiskTypes = map[string]struct{ min, max int64 }{
	"pd-standard": {200, 65536}, "pd-balanced": {10, 65536}, "pd-ssd": {10, 65536},
}

func regionRel(project, region, coll, name string) string {
	return "projects/" + project + "/regions/" + region + "/" + coll + "/" + name
}

func regionDiskTypeObj(project, region, name string) obj {
	d := regionalDiskTypes[name]
	return obj{"kind": "compute#diskType", "id": stableID("regional-" + name), "name": name, "region": "projects/" + project + "/regions/" + region,
		"description": "Regional " + name, "validDiskSize": fmt.Sprintf("%dGB-%dGB", d.min, d.max), "defaultDiskSizeGb": "500",
		"selfLink": regionRel(project, region, "diskTypes", name), "creationTimestamp": "1969-12-31T16:00:00.000-08:00"}
}

// regionDiskTypes serves regions/R/diskTypes.
func (s *Server) regionDiskTypes(c call, region string) (obj, error) {
	if c.method != http.MethodGet || len(c.parts) > 4 {
		return nil, unimplemented("%s on regionDiskTypes", c.method)
	}
	if len(c.parts) == 3 {
		names := make([]string, 0, len(regionalDiskTypes))
		for n := range regionalDiskTypes {
			names = append(names, n)
		}
		sort.Strings(names)
		return s.listFixed("compute#regionDiskTypeList", c, names, func(p, n string) obj { return regionDiskTypeObj(p, region, n) })
	}
	if _, ok := regionalDiskTypes[c.parts[3]]; !ok {
		return nil, notFound(regionRel(c.project, region, "diskTypes", c.parts[3]))
	}
	return regionDiskTypeObj(c.project, region, c.parts[3]), nil
}

// replicaZonesOf checks a regional disk's replicaZones: two distinct zones
// of region.
func replicaZonesOf(project, region string, v any) ([]string, error) {
	in := strsAny(v)
	if len(in) != 2 {
		return nil, invalid("Invalid value for field 'resource.replicaZones': '%s'. A regional disk must have exactly two replica zones.", strings.Join(in, ","))
	}
	var zones []string
	for i, z := range in {
		zn := lastSeg(z)
		if !IsZone(zn) {
			return nil, invalid("Invalid value for field 'resource.replicaZones[%d]': '%s'. The zone does not exist.", i, z)
		}
		if regionOfZone(zn) != region {
			return nil, invalid("Invalid value for field 'resource.replicaZones[%d]': '%s'. Replica zones must be in the disk's region '%s'.", i, z, region)
		}
		zones = append(zones, zn)
	}
	if zones[0] == zones[1] {
		return nil, invalid("Invalid value for field 'resource.replicaZones': '%s'. The two replica zones must be different.", strings.Join(in, ","))
	}
	return zones, nil
}

// diskInZone is whether disk d (zonal or regional) may be attached to a VM
// of zone.
func diskInZone(d obj, zone string) bool {
	if z := str(d, "zone"); z != "" {
		return lastSeg(z) == zone
	}
	for _, rz := range strs(d, "replicaZones") {
		if lastSeg(rz) == zone {
			return true
		}
	}
	return false
}

// newRegionDiskLocked validates a regional disk and returns it, not stored.
func (s *Server) newRegionDiskLocked(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := regionRel(project, region, "disks", name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	zones, err := replicaZonesOf(project, region, b["replicaZones"])
	if err != nil {
		return nil, err
	}
	typ := "pd-balanced"
	if t := str(b, "type"); t != "" {
		typ = lastSeg(t)
	}
	lim, ok := regionalDiskTypes[typ]
	if !ok {
		return nil, invalid("Invalid value for field 'resource.type': '%s'. The disk type is not supported for regional disks: use pd-balanced, pd-ssd or pd-standard.", str(b, "type"))
	}
	// The zonal rules for the source, size and policies, applied in the
	// first replica zone (or the source disk's, when it is one of them),
	// under a name of its own so a zonal disk of the same name does not
	// collide.
	zone := zones[0]
	if sd := Rel(str(b, "sourceDisk"), project); sd != "" {
		if so, ok := s.res[sd]; ok && hasStr(zones, lastSeg(str(so, "zone"))) {
			zone = lastSeg(str(so, "zone"))
		}
	}
	zb := obj{}
	for k, v := range b {
		zb[k] = v
	}
	zb["name"] = "regional-" + randHex(8)
	zb["type"] = typ
	if pols := strsAny(b["resourcePolicies"]); len(pols) > 0 {
		zb["resourcePolicies"] = b["resourcePolicies"]
	}
	d, err := s.newDiskLocked(project, zone, zb, "resource")
	if err != nil {
		return nil, err
	}
	size, _, _ := sizeGb(d["sizeGb"])
	if size < lim.min || size > lim.max {
		return nil, invalid("Invalid value for field 'resource.sizeGb': '%d'. Disk size must be between %d GB and %d GB for a regional %s disk.", size, lim.min, lim.max, typ)
	}
	delete(d, "zone")
	d["name"], d["selfLink"] = name, rel
	d["region"] = "projects/" + project + "/regions/" + region
	d["type"] = regionRel(project, region, "diskTypes", typ)
	d["replicaZones"] = []any{"projects/" + project + "/zones/" + zones[0], "projects/" + project + "/zones/" + zones[1]}
	return d, nil
}

// regionDisks serves regions/R/disks.
func (s *Server) regionDisks(c call, region string) (obj, error) {
	if s.opts.VMs == nil {
		return nil, noVMs()
	}
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
		d, err := s.newRegionDiskLocked(c.project, region, c.body)
		if err != nil {
			return nil, err
		}
		rel := str(d, "selfLink")
		s.res[rel] = norm(d)
		s.markScheduledLocked(s.res[rel], strs(s.res[rel], "resourcePolicies"))
		spec := s.diskSpecLocked(s.res[rel])
		return s.runOpLocked(c.project, scope, "insert", rel, func(ctx context.Context) error {
			return s.opts.VMs.CreateDisk(ctx, spec)
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
	if n < 4 || (n == 4 && !get && !del) || (n == 5 && !servedRegionDiskVerbs[p[4]]) || n > 5 {
		return nil, unimplemented("%s on regionDisks/%s is not served", c.method, strings.Join(p[3:], "/"))
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
		if users := strs(d, "users"); len(users) > 0 {
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The disk resource '%s' is already being used by '%s'", rel, users[0])
		}
		spec := s.diskSpecLocked(d)
		d["status"] = "DELETING"
		return s.runOpLocked(c.project, scope, "delete", rel, func(ctx context.Context) error {
			return s.opts.VMs.DeleteDisk(ctx, spec)
		}, func(err error) {
			if err == nil {
				delete(s.res, rel)
			} else if o, ok := s.res[rel]; ok {
				o["status"] = "READY"
			}
		})
	case post && p[4] == "resize":
		size, set, err := sizeGb(c.body["sizeGb"])
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
		if lim := regionalDiskTypes[lastSeg(str(d, "type"))]; size > lim.max {
			return nil, invalid("Invalid value for field 'sizeGb': '%d'. Disk size must be at most %d GB.", size, lim.max)
		}
		d["sizeGb"] = strconv.FormatInt(size, 10)
		spec := s.diskSpecLocked(d)
		return s.runOpLocked(c.project, scope, "resizeDisk", rel, func(ctx context.Context) error {
			return s.opts.VMs.ResizeDisk(ctx, spec)
		}, nil)
	case post && p[4] == "setLabels":
		return s.setLabels(c.project, scope, rel, c.body)
	case post && p[4] == "createSnapshot":
		return s.createSnapshot(c.project, rel, c.body, scope)
	case post && (p[4] == "addResourcePolicies" || p[4] == "removeResourcePolicies"):
		return s.attachPolicies(c.project, scope, rel, d, c.body, p[4] == "addResourcePolicies", "snapshotSchedulePolicy")
	}
	return nil, unimplemented("%s on regionDisks/%s is not served", c.method, strings.Join(p[3:], "/"))
}

var servedRegionDiskVerbs = map[string]bool{"resize": true, "setLabels": true, "createSnapshot": true,
	"addResourcePolicies": true, "removeResourcePolicies": true}

// diskRegion is the region of a zonal or regional disk.
func diskRegion(d obj) string {
	if r := str(d, "region"); r != "" {
		return lastSeg(r)
	}
	return regionOfZone(lastSeg(str(d, "zone")))
}
