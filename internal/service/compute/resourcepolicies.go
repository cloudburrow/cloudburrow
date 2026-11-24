package compute

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

// Resource policies (#2206): the regional resourcePolicies that are disk
// consistency groups (diskConsistencyGroupPolicy), which Asynchronous
// Replication stops and clones as one
// (https://docs.cloud.google.com/compute/docs/disks/async-pd/manage-consistency-groups).
// A policy's other kinds (snapshot schedules, instance schedules,
// placement and workload policies) are not served.

// maxGroupDisks is a consistency group's documented "maximum of 128
// disks".
const maxGroupDisks = 128

// resourcePolicies serves regions/R/resourcePolicies: list, insert, get
// and delete.
func (s *Server) resourcePolicies(c call, region string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/regions/" + region + "/resourcePolicies"
	scope := "regions/" + region
	switch {
	case n == 3 && c.method == http.MethodGet:
		return s.listOut("compute#resourcePolicyList", coll, s.res, c.query)
	case n == 3 && c.method == http.MethodPost:
		return s.insertResourcePolicy(c.project, region, c.body)
	case n == 4 && c.method == http.MethodGet:
		return s.getRes(coll + "/" + p[3])
	case n == 4 && c.method == http.MethodPatch:
		rel := coll + "/" + p[3]
		po, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		return s.patchResourcePolicy(c, rel, po)
	case n == 4 && c.method == http.MethodDelete:
		rel := coll + "/" + p[3]
		if _, ok := s.res[rel]; !ok {
			return nil, notFound(rel)
		}
		if users := s.policyUsersLocked(rel); len(users) > 0 {
			return nil, inUse("resource policy", rel, users[0])
		}
		delete(s.res, rel)
		return s.done(c.project, scope, "delete", rel, nil)
	}
	return nil, unimplemented("%s on resourcePolicies/%s is not served (#2206): resource policies are served as disk consistency groups and snapshot schedules (insert, get, list, patch, delete)", c.method, strings.Join(p[3:], "/"))
}

func (s *Server) insertResourcePolicy(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := regionRel(project, region, "resourcePolicies", name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	var kinds []string
	for _, k := range []string{"snapshotSchedulePolicy", "groupPlacementPolicy", "instanceSchedulePolicy", "diskConsistencyGroupPolicy", "workloadPolicy"} {
		if b[k] != nil {
			kinds = append(kinds, k)
		}
	}
	switch {
	case len(kinds) == 0:
		return nil, invalid("Invalid value for field 'resource': a resource policy needs one of snapshotSchedulePolicy, groupPlacementPolicy, instanceSchedulePolicy, diskConsistencyGroupPolicy or workloadPolicy.")
	case len(kinds) > 1:
		return nil, invalid("Invalid value for field 'resource': at most one of %s may be set.", strings.Join(kinds, ", "))
	case kinds[0] != "diskConsistencyGroupPolicy" && kinds[0] != "snapshotSchedulePolicy":
		return nil, unimplemented("%s resource policies are not served (#2206): they apply to VM instances, whose addResourcePolicies is not served; resource policies are served as disk consistency groups (diskConsistencyGroupPolicy) and snapshot schedules (snapshotSchedulePolicy)", kinds[0])
	}
	o := obj{"kind": "compute#resourcePolicy", "id": newID(), "name": name, "region": "projects/" + project + "/regions/" + region,
		"selfLink": rel, "creationTimestamp": s.stamp(), "status": "READY"}
	if kinds[0] == "snapshotSchedulePolicy" {
		sp, err := s.newSnapshotSchedule(project, region, b)
		if err != nil {
			return nil, err
		}
		o["snapshotSchedulePolicy"] = sp
	} else {
		o["diskConsistencyGroupPolicy"] = obj{}
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"] = l
	}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	s.res[rel] = norm(o)
	return s.done(project, "regions/"+region, "insert", rel, o["id"])
}

// policyUsersLocked are the disks that have the policy rel, sorted.
func (s *Server) policyUsersLocked(rel string) []string {
	var out []string
	for k, o := range s.res {
		if strings.Contains(k, "/disks/") && hasStr(strs(o, "resourcePolicies"), rel) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// consistencyGroupLocked is the consistency group a disk is in, or "".
func (s *Server) consistencyGroupLocked(d obj) string {
	for _, p := range strs(d, "resourcePolicies") {
		if po, ok := s.res[p]; ok && po["diskConsistencyGroupPolicy"] != nil {
			return p
		}
	}
	return ""
}

// checkDiskPoliciesLocked validates resource policies added to d and
// returns d's policies with them: each in d's region, and at most one
// consistency group, whose disks are all primaries or all secondaries, in
// one zone (zonal disks) or one pair of zones (regional), at most 128.
func (s *Server) checkDiskPoliciesLocked(project string, d obj, add []string, field string) ([]any, error) {
	cur := strs(d, "resourcePolicies")
	out := toAny(cur)
	region := diskRegion(d)
	for i, ref := range add {
		rel := Rel(ref, project)
		if !strings.Contains(rel, "/") {
			rel = regionRel(project, region, "resourcePolicies", rel)
		}
		po, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		if hasStr(cur, rel) {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. The disk already has this resource policy.", field, i, rel)
		}
		if r := lastSeg(str(po, "region")); r != region {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. The resource policy must be in the disk's region, %s.", field, i, rel, region)
		}
		if !isSchedule(po) && po["diskConsistencyGroupPolicy"] == nil {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. Only snapshot schedules and consistency groups can be attached to a disk.", field, i, rel)
		}
		if isSchedule(po) {
			if err := s.checkScheduleAttachLocked(cur, rel, field, i); err != nil {
				return nil, err
			}
		}
		if po["diskConsistencyGroupPolicy"] != nil {
			if g := s.consistencyGroupLocked(obj{"resourcePolicies": out}); g != "" {
				return nil, invalid("Invalid value for field '%s[%d]': '%s'. A disk can belong to only one consistency group at a time: it is in '%s'.", field, i, rel, g)
			}
			if err := s.checkGroupMemberLocked(d, rel, field, i); err != nil {
				return nil, err
			}
		}
		cur = append(cur, rel)
		out = append(out, rel)
	}
	return out, nil
}

// checkGroupMemberLocked applies the consistency group rules to d joining
// the group pol.
func (s *Server) checkGroupMemberLocked(d obj, pol, field string, i int) error {
	members := s.policyUsersLocked(pol)
	if len(members) >= maxGroupDisks {
		return invalid("Invalid value for field '%s[%d]': '%s'. A consistency group can have a maximum of %d disks.", field, i, pol, maxGroupDisks)
	}
	secondary := d["asyncPrimaryDisk"] != nil
	if !secondary && s.replicatingPeerLocked(d) != "" {
		return invalid("Invalid value for field '%s[%d]': '%s'. You can't add a primary disk to a consistency group while the disk is replicating.", field, i, pol)
	}
	where := func(o obj) string {
		if isRegional(o) {
			z := replicaZones(o)
			sort.Strings(z)
			return strings.Join(z, ",")
		}
		return lastSeg(str(o, "zone"))
	}
	for _, m := range members {
		o := s.res[m]
		if (o["asyncPrimaryDisk"] != nil) != secondary {
			return invalid("Invalid value for field '%s[%d]': '%s'. A consistency group holds primary disks or secondary disks, but not both.", field, i, pol)
		}
		if where(o) != where(d) {
			return invalid("Invalid value for field '%s[%d]': '%s'. The disks of a consistency group must be in the same zone, for zonal disks, or in the same pair of zones, for regional disks: '%s' is in %s.", field, i, pol, m, where(o))
		}
	}
	return nil
}

// diskPolicies is addResourcePolicies or removeResourcePolicies.
func (s *Server) diskPolicies(project, scope, rel string, d, b obj, add bool) (obj, error) {
	refs := strs(b, "resourcePolicies")
	if len(refs) == 0 {
		return nil, required("resourcePolicies")
	}
	if add {
		l, err := s.checkDiskPoliciesLocked(project, d, refs, "resourcePolicies")
		if err != nil {
			return nil, err
		}
		d["resourcePolicies"] = l
		return s.done(project, scope, "addResourcePolicies", rel, d["id"])
	}
	cur := strs(d, "resourcePolicies")
	var keep []any
	for i, ref := range refs {
		pr := Rel(ref, project)
		if !strings.Contains(pr, "/") {
			pr = regionRel(project, diskRegion(d), "resourcePolicies", pr)
		}
		if !hasStr(cur, pr) {
			return nil, invalid("Invalid value for field 'resourcePolicies[%d]': '%s'. The disk does not have this resource policy.", i, ref)
		}
		if po := s.res[pr]; po["diskConsistencyGroupPolicy"] != nil && d["asyncPrimaryDisk"] == nil && s.replicatingPeerLocked(d) != "" {
			return nil, invalid("Invalid value for field 'resourcePolicies[%d]': '%s'. You can't remove a primary disk from a consistency group while the disk is replicating.", i, ref)
		}
		refs[i] = pr
	}
	for _, c := range cur {
		if !hasStr(refs, c) {
			keep = append(keep, c)
		}
	}
	if len(keep) == 0 {
		delete(d, "resourcePolicies")
	} else {
		d["resourcePolicies"] = keep
	}
	return s.done(project, scope, "removeResourcePolicies", rel, d["id"])
}

// bulkInsertDisks clones the secondary disks of a consistency group
// (BulkInsertDiskResource.sourceConsistencyGroupPolicy) into the scope's
// region or zone: each clone is named after its disk and records the
// group. "You can't clone primary disks from a consistency group."
func (s *Server) bulkInsertDisks(project, scope, region string, b obj) (obj, error) {
	if b["instantSnapshotGroupParameters"] != nil || b["snapshotGroupParameters"] != nil {
		return nil, unimplemented("bulkInsert from snapshot groups or instant snapshot groups is not served (#2206): clone a consistency group with sourceConsistencyGroupPolicy")
	}
	ref := str(b, "sourceConsistencyGroupPolicy")
	if ref == "" {
		return nil, required("sourceConsistencyGroupPolicy")
	}
	pol := Rel(ref, project)
	po, ok := s.res[pol]
	if !ok {
		return nil, notFound(pol)
	}
	if po["diskConsistencyGroupPolicy"] == nil {
		return nil, invalid("Invalid value for field 'sourceConsistencyGroupPolicy': '%s'. The resource policy is not a consistency group.", pol)
	}
	members := s.policyUsersLocked(pol)
	if len(members) == 0 {
		return nil, invalid("Invalid value for field 'sourceConsistencyGroupPolicy': '%s'. The consistency group has no disks.", pol)
	}
	if err := s.checkDiskBudget(); err != nil {
		return nil, err
	}
	zonal := strings.HasPrefix(scope, "zones/")
	var clones []obj
	for _, m := range members {
		src := s.res[m]
		if src["asyncPrimaryDisk"] == nil {
			return nil, invalid("Invalid value for field 'sourceConsistencyGroupPolicy': '%s'. You can't clone primary disks from a consistency group: '%s' is a primary disk.", pol, m)
		}
		if diskRegion(src) != region {
			return nil, invalid("Invalid value for field 'sourceConsistencyGroupPolicy': '%s'. Clones are made in the disks' own region, %s.", pol, diskRegion(src))
		}
		body := obj{"name": str(src, "name") + "-clone", "sourceDisk": m, "type": lastSeg(str(src, "type")), "sizeGb": src["sizeGb"]}
		var d obj
		var err error
		if zonal {
			zone := strings.TrimPrefix(scope, "zones/")
			if isRegional(src) {
				return nil, invalid("Invalid value for field 'sourceConsistencyGroupPolicy': '%s'. A group of regional disks is cloned with regionDisks.bulkInsert.", pol)
			}
			d, err = s.newDiskLocked(project, zone, body, "resource")
		} else {
			if !isRegional(src) {
				return nil, invalid("Invalid value for field 'sourceConsistencyGroupPolicy': '%s'. A group of zonal disks is cloned with disks.bulkInsert.", pol)
			}
			body["replicaZones"] = toAny(replicaZones(src))
			d, err = s.newRegionDiskLocked(project, region, body, "resource")
		}
		if err != nil {
			return nil, err
		}
		d["sourceConsistencyGroupPolicy"], d["sourceConsistencyGroupPolicyId"] = pol, po["id"]
		clones = append(clones, d)
	}
	type job struct {
		rel       string
		sync      func(context.Context) error
		src, spec DiskSpec
	}
	var jobs []job
	for _, d := range clones {
		rel := str(d, "selfLink")
		s.res[rel] = norm(d)
		src := s.res[str(d, "sourceDisk")]
		jobs = append(jobs, job{rel: rel, sync: s.syncSecondaryLocked(src), src: s.diskSpecLocked(src), spec: s.diskSpecLocked(s.res[rel])})
	}
	return s.runOpLocked(project, scope, "bulkInsert", pol, func(ctx context.Context) error {
		for i, j := range jobs {
			if j.sync != nil {
				// The secondary as replication has it, before it is cloned.
				if err := j.sync(ctx); err != nil {
					return err
				}
			}
			err := s.opts.VMs.CreateDisk(ctx, j.spec)
			if err == nil {
				err = s.opts.VMs.CopyDisk(ctx, j.src, j.spec)
			}
			if err != nil {
				for _, m := range jobs[:i+1] {
					_ = s.opts.VMs.DeleteDisk(context.Background(), m.spec)
				}
				return err
			}
		}
		return nil
	}, func(err error) {
		for _, j := range jobs {
			if err != nil {
				delete(s.res, j.rel)
			} else if o, ok := s.res[j.rel]; ok {
				o["status"] = "READY"
			}
		}
	})
}
