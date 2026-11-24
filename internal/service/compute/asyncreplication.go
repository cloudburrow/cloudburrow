package compute

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

// Asynchronous Replication (#2206,
// https://docs.cloud.google.com/compute/docs/disks/async-pd/about): a
// secondary disk made with asyncPrimaryDisk, startAsyncReplication on the
// primary, stopAsyncReplication on either disk, and
// stopGroupAsyncReplication on a consistency group. The primary lists its
// secondaries in asyncSecondaryDisks and both disks report the pair's
// state in resourceStatus; the secondary takes the primary's data when
// replication starts, whenever it is read (snapshot, clone) and when
// replication stops.

const (
	asyncActive  = "ACTIVE"
	asyncStopped = "STOPPED"
)

// secondaryOf is the primary disk's entry of its secondary sec, or nil.
func secondaryOf(primary obj, sec string) map[string]any {
	m, _ := primary["asyncSecondaryDisks"].(map[string]any)
	e, _ := m[sec].(map[string]any)
	return e
}

// replicatingPeerLocked is, for a disk in an active pair, the other disk:
// its primary, or one of its secondaries.
func (s *Server) replicatingPeerLocked(d obj) string {
	if ap, ok := d["asyncPrimaryDisk"].(map[string]any); ok {
		if pd, ok := s.res[str(ap, "disk")]; ok && s.asyncStateLocked(pd, str(d, "selfLink")) == asyncActive {
			return str(ap, "disk")
		}
	}
	m, _ := d["asyncSecondaryDisks"].(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s.asyncStateLocked(d, k) == asyncActive {
			return k
		}
	}
	return ""
}

// asyncStateLocked is the replication state of primary to sec.
func (s *Server) asyncStateLocked(primary obj, sec string) string {
	rs, _ := primary["resourceStatus"].(map[string]any)
	m, _ := rs["asyncSecondaryDisks"].(map[string]any)
	e, _ := m[sec].(map[string]any)
	return str(e, "state")
}

// setAsyncStateLocked records the pair's state on both disks.
func (s *Server) setAsyncStateLocked(prel, srel, state string) {
	if pd, ok := s.res[prel]; ok {
		rs, _ := pd["resourceStatus"].(map[string]any)
		if rs == nil {
			rs = map[string]any{}
			pd["resourceStatus"] = rs
		}
		m, _ := rs["asyncSecondaryDisks"].(map[string]any)
		if m == nil {
			m = map[string]any{}
			rs["asyncSecondaryDisks"] = m
		}
		m[srel] = map[string]any{"state": state}
	}
	if sd, ok := s.res[srel]; ok {
		rs, _ := sd["resourceStatus"].(map[string]any)
		if rs == nil {
			rs = map[string]any{}
			sd["resourceStatus"] = rs
		}
		rs["asyncPrimaryDisk"] = map[string]any{"state": state}
	}
}

// forgetSecondaryLocked removes a deleted disk from its primary's
// asyncSecondaryDisks.
func (s *Server) forgetSecondaryLocked(rel string) {
	for k, o := range s.res {
		if !strings.Contains(k, "/disks/") {
			continue
		}
		if m, ok := o["asyncSecondaryDisks"].(map[string]any); ok {
			delete(m, rel)
			if len(m) == 0 {
				delete(o, "asyncSecondaryDisks")
			}
		}
		if rs, ok := o["resourceStatus"].(map[string]any); ok {
			if m, ok := rs["asyncSecondaryDisks"].(map[string]any); ok {
				delete(m, rel)
				if len(m) == 0 {
					delete(rs, "asyncSecondaryDisks")
				}
			}
			if len(rs) == 0 {
				delete(o, "resourceStatus")
			}
		}
	}
}

// startAsyncReplication starts replication from the primary prel to the
// secondary the body names, which must have been made with prel as its
// asyncPrimaryDisk.
func (s *Server) startAsyncReplication(project, scope, prel string, pd, b obj) (obj, error) {
	ref := str(b, "asyncSecondaryDisk")
	if ref == "" {
		return nil, required("asyncSecondaryDisk")
	}
	srel, ok := resolveDisk(ref, project)
	if !ok {
		return nil, invalid("Invalid value for field 'asyncSecondaryDisk': '%s'.", ref)
	}
	sd, ok := s.res[srel]
	if !ok {
		return nil, notFound(srel)
	}
	ap, _ := sd["asyncPrimaryDisk"].(map[string]any)
	if str(ap, "disk") != prel {
		return nil, invalid("Invalid value for field 'asyncSecondaryDisk': '%s'. The secondary disk must be created with asyncPrimaryDisk set to '%s'.", srel, prel)
	}
	if pd["asyncPrimaryDisk"] != nil {
		return nil, invalid("Invalid value for field 'disk': '%s'. A secondary disk cannot replicate to another disk.", prel)
	}
	if str(pd, "status") != "READY" || str(sd, "status") != "READY" {
		return nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", srel)
	}
	if st := s.asyncStateLocked(pd, srel); st == asyncActive {
		return nil, invalid("Asynchronous replication from '%s' to '%s' is already active.", prel, srel)
	} else if st == asyncStopped {
		return nil, invalid("Asynchronous replication from '%s' to '%s' was stopped: a stopped pair cannot be restarted. Create a new secondary disk.", prel, srel)
	}
	if users := strs(sd, "users"); len(users) > 0 {
		return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
			"The disk resource '%s' is already being used by '%s'", srel, users[0])
	}
	if s.replicatingPeerLocked(pd) != "" {
		return nil, invalid("Invalid value for field 'disk': '%s'. The disk already replicates to a secondary disk.", prel)
	}
	m, _ := pd["asyncSecondaryDisks"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		pd["asyncSecondaryDisks"] = m
	}
	ard := map[string]any{"disk": srel, "diskId": sd["id"]}
	// Replication started on a disk of a consistency group records the
	// group on both disks (DiskAsyncReplication.consistencyGroupPolicy).
	if pol := s.consistencyGroupLocked(pd); pol != "" {
		ard["consistencyGroupPolicy"], ard["consistencyGroupPolicyId"] = pol, s.res[pol]["id"]
		ap["consistencyGroupPolicy"], ap["consistencyGroupPolicyId"] = pol, s.res[pol]["id"]
	}
	m[srel] = map[string]any{"asyncReplicationDisk": ard}
	s.setAsyncStateLocked(prel, srel, asyncActive)
	src, dst := s.diskSpecLocked(pd), s.diskSpecLocked(sd)
	return s.runOpLocked(project, scope, "startAsyncReplication", prel, func(ctx context.Context) error {
		return s.opts.VMs.CopyDisk(ctx, src, dst)
	}, func(err error) {
		if err != nil {
			if p, ok := s.res[prel]; ok {
				if m, ok := p["asyncSecondaryDisks"].(map[string]any); ok {
					delete(m, srel)
				}
			}
			s.forgetSecondaryLocked(srel)
			if sd, ok := s.res[srel]; ok {
				delete(sd, "resourceStatus")
			}
		}
	})
}

// stopAsyncReplication stops the active replication of rel, a primary or
// a secondary disk: the secondary takes the primary's data a last time.
func (s *Server) stopAsyncReplication(project, scope, rel string, d obj) (obj, error) {
	prel, srel := "", ""
	if ap, ok := d["asyncPrimaryDisk"].(map[string]any); ok {
		prel, srel = str(ap, "disk"), rel
		if pd, ok := s.res[prel]; !ok || s.asyncStateLocked(pd, srel) != asyncActive {
			prel = ""
		}
	} else if peer := s.replicatingPeerLocked(d); peer != "" {
		prel, srel = rel, peer
	}
	if prel == "" {
		return nil, invalid("Invalid value for field 'disk': '%s'. The disk is not in an active asynchronous replication pair.", rel)
	}
	return s.stopPairsLocked(project, scope, rel, "stopAsyncReplication", [][2]string{{prel, srel}})
}

// stopPairsLocked stops replication of each primary, secondary pair, the
// secondaries copied first.
func (s *Server) stopPairsLocked(project, scope, target, opType string, pairs [][2]string) (obj, error) {
	type cp struct{ src, dst DiskSpec }
	var copies []cp
	for _, p := range pairs {
		pd, sd := s.res[p[0]], s.res[p[1]]
		copies = append(copies, cp{s.diskSpecLocked(pd), s.diskSpecLocked(sd)})
		s.setAsyncStateLocked(p[0], p[1], "STOPPING")
	}
	return s.runOpLocked(project, scope, opType, target, func(ctx context.Context) error {
		for _, c := range copies {
			if err := s.opts.VMs.CopyDisk(ctx, c.src, c.dst); err != nil {
				return err
			}
		}
		return nil
	}, func(err error) {
		for _, p := range pairs {
			s.setAsyncStateLocked(p[0], p[1], asyncStopped)
		}
	})
}

// stopGroupAsyncReplication stops the replication of every disk in the
// consistency group the body names, from either region.
func (s *Server) stopGroupAsyncReplication(project, scope, region string, b obj) (obj, error) {
	ref := str(b, "resourcePolicy")
	if ref == "" {
		return nil, required("resourcePolicy")
	}
	pol := Rel(ref, project)
	po, ok := s.res[pol]
	if !ok {
		return nil, notFound(pol)
	}
	if po["diskConsistencyGroupPolicy"] == nil {
		return nil, invalid("Invalid value for field 'resourcePolicy': '%s'. The resource policy is not a consistency group (diskConsistencyGroupPolicy).", pol)
	}
	var pairs [][2]string
	seen := map[[2]string]bool{}
	add := func(pr [2]string) {
		if !seen[pr] {
			seen[pr] = true
			pairs = append(pairs, pr)
		}
	}
	keys := make([]string, 0, len(s.res))
	for k := range s.res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// The group holds primary disks, or secondary disks ("but not both").
	for _, k := range keys {
		d := s.res[k]
		if !strings.Contains(k, "/disks/") || !hasStr(strs(d, "resourcePolicies"), pol) {
			continue
		}
		if ap, ok := d["asyncPrimaryDisk"].(map[string]any); ok {
			if pd, ok := s.res[str(ap, "disk")]; ok && s.asyncStateLocked(pd, k) == asyncActive {
				add([2]string{str(ap, "disk"), k})
			}
			continue
		}
		m, _ := d["asyncSecondaryDisks"].(map[string]any)
		for sec := range m {
			if s.asyncStateLocked(d, sec) == asyncActive {
				add([2]string{k, sec})
			}
		}
	}
	if len(pairs) == 0 {
		return nil, invalid("Invalid value for field 'resourcePolicy': '%s'. No disk of the consistency group is replicating.", pol)
	}
	return s.stopPairsLocked(project, scope, pol, "stopGroupAsyncReplication", pairs)
}

// syncSecondaryLocked is the copy that brings a secondary disk up to date
// before it is read, or nil when d is not an actively replicating
// secondary.
func (s *Server) syncSecondaryLocked(d obj) func(context.Context) error {
	ap, ok := d["asyncPrimaryDisk"].(map[string]any)
	if !ok {
		return nil
	}
	pd, ok := s.res[str(ap, "disk")]
	if !ok || s.asyncStateLocked(pd, str(d, "selfLink")) != asyncActive {
		return nil
	}
	src, dst := s.diskSpecLocked(pd), s.diskSpecLocked(d)
	return func(ctx context.Context) error { return s.opts.VMs.CopyDisk(ctx, src, dst) }
}
