package compute

import (
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Connection draining (#2226), as
// https://docs.cloud.google.com/load-balancing/docs/enabling-connection-draining
// describes it (read 2026-11-24): a backend service's
// connectionDraining.drainingTimeoutSec, 0 to 3600 seconds, is how long a
// VM leaving one of its instance groups keeps the requests it already has.
// A VM leaves a group when the group's manager deletes it (deleteInstances,
// resize, an update's surplus or SUBSTITUTE replacement) or recreates it
// (recreateInstances, an update's RECREATE replacement, autohealing), or
// when removeInstances takes it from an unmanaged group; an endpoint
// leaves a zonal or hybrid network endpoint group by
// detachNetworkEndpoints. While it drains
// it gets no new requests or connections; once a backend service's own
// timeout passes, that backend service stops sending it anything, its
// requests still in flight cut. When groups are shared, "the largest
// timeout value is used": a managed instance group waits that long before
// it deletes or recreates the VM, listing it DELETING or RECREATING, and
// an unmanaged group keeps it as a member until then. A managed
// instance's instanceHealth[].detailedHealthState and a network
// endpoint's health (listNetworkEndpoints) report it DRAINING meanwhile;
// getHealth, whose healthState is only HEALTHY or UNHEALTHY in the REST
// reference, UNHEALTHY. A timeout of 0 (the default) ends its
// connections at once.

const maxDrainingTimeoutSec = 3600

// checkConnectionDraining is a backend service's connectionDraining.
func checkConnectionDraining(b obj) (obj, error) {
	cd := objOf(b["connectionDraining"])
	v := numOr(cd, "drainingTimeoutSec", 0)
	if v < 0 || v > maxDrainingTimeoutSec || v != float64(int64(v)) {
		return nil, invalid("Invalid value for field 'resource.connectionDraining.drainingTimeoutSec': '%v'. Must be an integer between 0 and %d.", v, maxDrainingTimeoutSec)
	}
	return obj{"drainingTimeoutSec": v}, nil
}

// drainingTimeoutOf is a backend service's draining timeout.
func drainingTimeoutOf(bs obj) time.Duration {
	return time.Duration(numOr(objOf(bs["connectionDraining"]), "drainingTimeoutSec", 0)) * time.Second
}

// drainTimeoutLocked is the largest draining timeout of the backend
// services that have group as a backend.
func (s *Server) drainTimeoutLocked(group string) time.Duration {
	var d time.Duration
	if group == "" {
		return 0
	}
	for _, o := range s.res {
		if str(o, "kind") != "compute#backendService" {
			continue
		}
		for _, e := range listOf(o["backends"]) {
			if str(objOf(e), "group") == group {
				d = max(d, drainingTimeoutOf(o))
			}
		}
	}
	return d
}

// managerOfVM is the manager that made a VM, from its created-by
// metadata; "" for one no group made.
func managerOfVM(o obj) string {
	for _, it := range listOf(vmMetadataItems(o)) {
		im, _ := it.(map[string]any)
		if str(im, "key") == "created-by" {
			return str(im, "value")
		}
	}
	return ""
}

// beginDrainLocked starts draining a managed VM before its manager takes
// action (DELETING or RECREATING) on it, then (do: delete, recreate or
// remake, with the template for the last two). It is false, and nothing
// is changed, when no backend service of the VM's group drains.
func (s *Server) beginDrainLocked(vm string, o obj, action string, then obj) bool {
	if o == nil || o["_drain"] != nil || str(o, "status") != stRunning {
		return false
	}
	group := str(s.res[managerOfVM(o)], "instanceGroup")
	d := s.drainTimeoutLocked(group)
	if d <= 0 {
		return false
	}
	now := s.opts.Now()
	o["_drain"] = map[string]any{"start": float64(now.UnixNano()), "until": float64(now.Add(d).UnixNano()),
		"group": group, "action": action, "then": map[string]any(then)}
	return true
}

// drainOf is a VM's drain, nil when it is not draining.
func drainOf(o obj) obj { return objOf(o["_drain"]) }

// drainStartLocked is when ep began draining from its group, if it is.
func (s *Server) drainStartLocked(ep lbEndpoint) (time.Time, bool) {
	if ep.kind != "vm" && ep.kind != "ip" {
		return time.Time{}, false
	}
	if d := drainOf(s.res[ep.instance]); ep.instance != "" && d != nil && str(d, "group") == ep.group {
		return time.Unix(0, int64(numOr(d, "start", 0))), true
	}
	if m := objOf(objOf(s.res[ep.group])["_drainingEndpoints"]); m != nil {
		k := endpointKey(obj{"instance": lastSeg(ep.instance), "ipAddress": ep.ip, "fqdn": ep.fqdn, "port": float64(ep.port)})
		if e := objOf(m[k]); e != nil {
			return time.Unix(0, int64(numOr(e, "start", 0))), true
		}
	}
	if m := objOf(objOf(s.res[ep.group])["_drainingMembers"]); m != nil {
		if e := objOf(m[ep.instance]); e != nil {
			return time.Unix(0, int64(numOr(e, "start", 0))), true
		}
	}
	return time.Time{}, false
}

// drainStateLocked is ep's draining under backend service bs: draining,
// or gone (its timeout passed, or it is 0).
func (s *Server) drainStateLocked(bs obj, ep lbEndpoint) (draining, gone bool) {
	start, ok := s.drainStartLocked(ep)
	if !ok {
		return false, false
	}
	if s.opts.Now().Before(start.Add(drainingTimeoutOf(bs))) {
		return true, false
	}
	return false, true
}

// drainMembersLocked starts draining VMs removeInstances takes from an
// unmanaged group: they stay members until the group's largest draining
// timeout passes. It returns the VMs that leave at once.
func (s *Server) drainMembersLocked(rel string, g obj, vms []string) []string {
	d := s.drainTimeoutLocked(rel)
	if d <= 0 {
		return vms
	}
	m := objOf(g["_drainingMembers"])
	if m == nil {
		m = obj{}
	}
	now := s.opts.Now()
	var now1 []string
	for _, vm := range vms {
		if str(s.res[vm], "status") != stRunning {
			now1 = append(now1, vm)
			continue
		}
		if m[vm] == nil {
			m[vm] = map[string]any{"start": float64(now.UnixNano()), "until": float64(now.Add(d).UnixNano())}
		}
	}
	if len(m) > 0 {
		g["_drainingMembers"] = map[string]any(m)
	}
	return now1
}

// finishDrainsLocked ends the drains whose time is up: a managed VM's
// manager then takes its action, an unmanaged group lets its member go.
// Requests in flight to a VM whose backend service's own timeout passed
// are cut. It reports whether it changed anything.
func (s *Server) finishDrainsLocked() bool {
	now := s.opts.Now()
	changed := false
	var keys []string
	for k := range s.res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := s.res[k]
		if o == nil {
			continue
		}
		if strings.Contains(k, "/instances/") {
			if d := drainOf(o); d != nil {
				s.cutDrainedLocked(k, str(d, "group"), time.Unix(0, int64(numOr(d, "start", 0))))
				if now.UnixNano() >= int64(numOr(d, "until", 0)) {
					delete(o, "_drain")
					s.lb.bal.cut(k, "")
					s.afterDrainLocked(k, o, objOf(d["then"]))
					changed = true
				}
			}
			continue
		}
		if s.finishNEGDrainsLocked(k, o, now) {
			changed = true
			continue
		}
		dm := objOf(o["_drainingMembers"])
		if dm == nil {
			continue
		}
		var gone []string
		for vm, e := range dm {
			s.cutDrainedLocked(vm, k, time.Unix(0, int64(numOr(objOf(e), "start", 0))))
			if now.UnixNano() >= int64(numOr(objOf(e), "until", 0)) || s.res[vm] == nil {
				gone = append(gone, vm)
			}
		}
		if len(gone) == 0 {
			continue
		}
		var keep []string
		for _, m := range groupMembersLocked(o) {
			if !hasStr(gone, m) {
				keep = append(keep, m)
			}
		}
		for _, vm := range gone {
			delete(dm, vm)
			s.lb.bal.cut(vm, "")
		}
		if len(dm) == 0 {
			delete(o, "_drainingMembers")
		}
		o["_members"] = toAny(keep)
		o["size"] = float64(len(keep))
		changed = true
	}
	return changed
}

// finishNEGDrainsLocked drops a network endpoint group's detached
// endpoints whose drain has ended, cutting what other backend services
// still had in flight; it reports whether it changed the group.
func (s *Server) finishNEGDrainsLocked(rel string, g obj, now time.Time) bool {
	dm := objOf(g["_drainingEndpoints"])
	if dm == nil {
		return false
	}
	project, zone := strings.Split(rel, "/")[1], lastSeg(str(g, "zone"))
	flightKey := func(e obj) string {
		if in := str(e, "instance"); in != "" {
			return zoneRel(project, zone, "instances", in)
		}
		port := int(numOr(e, "port", numOr(g, "defaultPort", 0)))
		return rel + "|" + str(e, "ipAddress") + ":" + strconv.Itoa(port)
	}
	var keep []any
	gone := false
	for _, h := range listOf(g["_endpoints"]) {
		e := objOf(h)
		d := objOf(dm[endpointKey(e)])
		if d == nil {
			keep = append(keep, h)
			continue
		}
		s.cutDrainedLocked(flightKey(e), rel, time.Unix(0, int64(numOr(d, "start", 0))))
		if now.UnixNano() < int64(numOr(d, "until", 0)) {
			keep = append(keep, h)
			continue
		}
		delete(dm, endpointKey(e))
		s.lb.bal.cut(flightKey(e), "")
		gone = true
	}
	if !gone {
		return false
	}
	if len(dm) == 0 {
		delete(g, "_drainingEndpoints")
	}
	g["_endpoints"] = keep
	g["size"] = float64(len(keep))
	s.lbChangedLocked()
	return true
}

// cutDrainedLocked cuts the requests in flight to vm, draining from group
// since start, of each backend service whose own timeout has passed.
func (s *Server) cutDrainedLocked(vm, group string, start time.Time) {
	now := s.opts.Now()
	for k, o := range s.res {
		if str(o, "kind") != "compute#backendService" || now.Before(start.Add(drainingTimeoutOf(o))) {
			continue
		}
		for _, e := range listOf(o["backends"]) {
			if str(objOf(e), "group") == group {
				s.lb.bal.cut(vm, k)
			}
		}
	}
}

// afterDrainLocked is the action a manager deferred while its VM drained.
func (s *Server) afterDrainLocked(vm string, o obj, then obj) {
	project := projectOfRel(vm)
	mgr := managerOfVM(o)
	m, ok := s.res[mgr]
	var err error
	switch {
	case str(then, "do") == "delete" || !ok:
		err = s.deleteManagedNowLocked(project, vm)
	case str(then, "do") == "recreate":
		err = s.recreateManagedFromNowLocked(project, mgr, m, vm, o, str(then, "template"))
	case str(then, "do") == "remake":
		err = s.remakeManagedNowLocked(project, mgr, m, vm, o, str(then, "template"))
	}
	if err != nil {
		// As a failed deletion leaves a VM, it stays a member.
		delete(o, "_deleting")
		delete(o, "_mgrAction")
	}
}

// --- requests in flight, to cut -------------------------------------------------

// flightKey is what a drain's end cuts an endpoint's requests by: its
// instance, or a hybrid endpoint's id.
func (ep lbEndpoint) flightKey() string {
	if ep.instance != "" {
		return ep.instance
	}
	return ep.id
}

// lbInflight are the requests in flight to each VM, by backend service,
// so the end of a drain can cut them.
type lbInflight struct {
	mu   sync.Mutex
	reqs map[string]map[*inflightReq]bool // "vm|backendService"
}

type inflightReq struct{ cancel context.CancelFunc }

// track registers a request to vm through backend service bs; the
// returned function ends it.
func (b *lbBalanceState) track(vm, bs string, cancel context.CancelFunc) func() {
	f := &b.flight
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reqs == nil {
		f.reqs = map[string]map[*inflightReq]bool{}
	}
	key := vm + "|" + bs
	r := &inflightReq{cancel: cancel}
	if f.reqs[key] == nil {
		f.reqs[key] = map[*inflightReq]bool{}
	}
	f.reqs[key][r] = true
	return func() {
		f.mu.Lock()
		delete(f.reqs[key], r)
		if len(f.reqs[key]) == 0 {
			delete(f.reqs, key)
		}
		f.mu.Unlock()
		cancel()
	}
}

// cut cancels the requests in flight to vm through backend service bs,
// or through any when bs is "".
func (b *lbBalanceState) cut(vm, bs string) {
	f := &b.flight
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, rs := range f.reqs {
		v, s, _ := strings.Cut(key, "|")
		if v != vm || (bs != "" && s != bs) {
			continue
		}
		for r := range rs {
			r.cancel()
		}
	}
}

// trackedBody ends its request's tracking when the response is read.
type trackedBody struct {
	io.ReadCloser
	once sync.Once
	end  func()
}

func (t *trackedBody) Close() error {
	err := t.ReadCloser.Close()
	t.once.Do(t.end)
	return err
}
