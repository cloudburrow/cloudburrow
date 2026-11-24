package compute

// The resource policies that apply to VM instances (#2206): instance
// schedules, placement policies and workload policies, and
// instances.addResourcePolicies and removeResourcePolicies, as Google
// documents them (read 2026-11-24):
// https://docs.cloud.google.com/compute/docs/instances/schedule-instance-start-stop,
// .../use-compact-placement-policies, .../use-spread-placement-policies,
// .../placement-policies-overview and the resourcePolicies reference.
//
//   - instanceSchedulePolicy: vmStartSchedule and vmStopSchedule are
//     unix-cron schedules ("*", "-" and ","), at least one of them, in
//     timeZone (IANA, default UTC), between startTime and expirationTime
//     (RFC 3339); "up to one start and one stop per hour"; a VM follows
//     one schedule, in its region, and a schedule has at most 1,000 VMs.
//     RunSnapshotSchedules' minute tick starts and stops the attached VMs
//     as each schedule falls due, and the policy reports
//     resourceStatus.instanceSchedulePolicy.nextRunStartTime and
//     lastRunStartTime.
//   - groupPlacementPolicy: spread (availabilityDomainCount 1 to 8, at most
//     256 VMs; each VM is given the least used domain, reported as
//     scheduling.availabilityDomain) or compact (collocation COLLOCATED,
//     maxDistance 1 to 3; the machine series the overview lists, here N2,
//     N2D and C3; at most 1,500, 150 or 22 VMs by maxDistance; only
//     TERMINATE host maintenance with maxDistance 1 or 2; added to a
//     stopped VM; its VMs report resourceStatus.physicalHostTopology,
//     sharing a cluster, block or sub-block as maxDistance requires);
//     vmCount caps the VMs.
//   - workloadPolicy (type HIGH_AVAILABILITY or HIGH_THROUGHPUT,
//     maxTopologyDistance and acceleratorTopology with HIGH_THROUGHPUT) is
//     "the intent of the instance placement in the MIG": it is validated
//     and kept; an instance can't take it.
//   - A VM takes one policy of each type
//     (RESOURCE_POLICY_DUPLICATE_TYPE_UNSUPPORTED), and a policy a VM has
//     can't be deleted.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA time zones on any host

	"github.com/robfig/cron/v3"
)

const (
	maxScheduleVMs = 1000
	maxSpreadVMs   = 256
)

// compactSeries are the machine series of this instance's machine types
// that compact placement policies support (placement-policies-overview).
var compactSeries = map[string]bool{"n2": true, "n2d": true, "c3": true}

var instanceCron = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// resourcePolicyKind is a resource policy's type, by its field.
func resourcePolicyKind(po obj) string {
	for _, k := range []string{"snapshotSchedulePolicy", "groupPlacementPolicy", "instanceSchedulePolicy", "diskConsistencyGroupPolicy", "workloadPolicy"} {
		if po[k] != nil {
			return k
		}
	}
	return ""
}

func checkCron(v, field string) (cron.Schedule, error) {
	if strings.ContainsAny(v, "/?LW#@") {
		return nil, invalid("Invalid value for field '%s': '%s'. The schedule must be in unix-cron format, using only the special characters *, - and ,.", field, v)
	}
	sc, err := instanceCron.Parse(v)
	if err != nil {
		return nil, invalid("Invalid value for field '%s': '%s'. The schedule must be in unix-cron format: %v.", field, v, err)
	}
	// "up to one start and one stop per hour".
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prev := sc.Next(t)
	for i := 0; i < 400 && !prev.IsZero(); i++ {
		n := sc.Next(prev)
		if n.IsZero() {
			break
		}
		if n.Sub(prev) < time.Hour {
			return nil, invalid("Invalid value for field '%s': '%s'. An instance schedule can run at most one start and one stop operation per hour.", field, v)
		}
		prev = n
	}
	return sc, nil
}

// newInstanceSchedule validates an instanceSchedulePolicy.
func newInstanceSchedule(v any, field string) (obj, error) {
	in, _ := v.(map[string]any)
	out := obj{}
	n := 0
	for _, k := range []string{"vmStartSchedule", "vmStopSchedule"} {
		m, _ := in[k].(map[string]any)
		if m == nil {
			continue
		}
		sc := str(m, "schedule")
		if sc == "" {
			return nil, required(field + "." + k + ".schedule")
		}
		if _, err := checkCron(sc, field+"."+k+".schedule"); err != nil {
			return nil, err
		}
		out[k] = obj{"schedule": sc}
		n++
	}
	if n == 0 {
		return nil, invalid("Invalid value for field '%s': an instance schedule needs vmStartSchedule, vmStopSchedule or both.", field)
	}
	tz := str(in, "timeZone")
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return nil, invalid("Invalid value for field '%s.timeZone': '%s'. The time zone must be an IANA time zone name.", field, tz)
	}
	out["timeZone"] = tz
	var start, end time.Time
	for _, k := range []string{"startTime", "expirationTime"} {
		v := str(in, k)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, invalid("Invalid value for field '%s.%s': '%s'. The time must be an RFC 3339 timestamp.", field, k, v)
		}
		if k == "startTime" {
			start = t
		} else {
			end = t
		}
		out[k] = v
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return nil, invalid("Invalid value for field '%s.expirationTime': '%s'. The expiration time must be after the start time.", field, str(in, "expirationTime"))
	}
	return out, nil
}

// newPlacementPolicy validates a groupPlacementPolicy.
func newPlacementPolicy(v any, field string) (obj, error) {
	in, _ := v.(map[string]any)
	out := obj{}
	col := str(in, "collocation")
	switch col {
	case "", "UNSPECIFIED_COLLOCATION", "COLLOCATED":
	default:
		return nil, invalid("Invalid value for field '%s.collocation': '%s'. The collocation must be COLLOCATED or UNSPECIFIED_COLLOCATION.", field, col)
	}
	adc, hasADC, err := intField(in["availabilityDomainCount"], field+".availabilityDomainCount")
	if err != nil {
		return nil, err
	}
	vmc, hasVMC, err := intField(in["vmCount"], field+".vmCount")
	if err != nil {
		return nil, err
	}
	md, hasMD, err := intField(in["maxDistance"], field+".maxDistance")
	if err != nil {
		return nil, err
	}
	if col == "COLLOCATED" {
		if hasADC {
			return nil, invalid("Invalid value for field '%s.availabilityDomainCount': '%d'. Availability domains are set on spread placement policies, not on compact (COLLOCATED) ones.", field, adc)
		}
		if hasMD && (md < 1 || md > 3) {
			return nil, invalid("Invalid value for field '%s.maxDistance': '%d'. The maximum distance must be 1, 2 or 3.", field, md)
		}
		max := compactMax(md)
		if hasVMC && (vmc < 2 || vmc > int64(max)) {
			return nil, invalid("Invalid value for field '%s.vmCount': '%d'. A compact placement policy can have from 2 to %d VMs.", field, vmc, max)
		}
		out["collocation"] = "COLLOCATED"
		if hasMD {
			out["maxDistance"] = md
		}
		if g := str(in, "gpuTopology"); g != "" {
			return nil, invalid("Invalid value for field '%s.gpuTopology': '%s'. You can't set GPU topology on a placement policy with this instance's machine types, which have no GPUs.", field, g)
		}
	} else {
		if hasMD || str(in, "gpuTopology") != "" {
			return nil, invalid("Invalid value for field '%s': maxDistance and gpuTopology are set on compact (COLLOCATED) placement policies only.", field)
		}
		if !hasADC {
			return nil, required(field + ".availabilityDomainCount")
		}
		if adc < 1 || adc > 8 {
			return nil, invalid("Invalid value for field '%s.availabilityDomainCount': '%d'. The number of availability domains must be between 1 and 8.", field, adc)
		}
		if hasVMC && (vmc < 1 || vmc > maxSpreadVMs) {
			return nil, invalid("Invalid value for field '%s.vmCount': '%d'. A spread placement policy can have at most %d VMs.", field, vmc, maxSpreadVMs)
		}
		out["availabilityDomainCount"] = adc
	}
	if hasVMC {
		out["vmCount"] = vmc
	}
	return out, nil
}

// compactMax is the most VMs a compact policy of maxDistance md has.
func compactMax(md int64) int {
	switch md {
	case 1:
		return 22
	case 2:
		return 150
	}
	return 1500
}

// newWorkloadPolicy validates a workloadPolicy.
func newWorkloadPolicy(v any, field string) (obj, error) {
	in, _ := v.(map[string]any)
	t := str(in, "type")
	switch t {
	case "HIGH_AVAILABILITY", "HIGH_THROUGHPUT":
	case "":
		return nil, required(field + ".type")
	default:
		return nil, invalid("Invalid value for field '%s.type': '%s'. The type must be HIGH_AVAILABILITY or HIGH_THROUGHPUT.", field, t)
	}
	out := obj{"type": t}
	if d := str(in, "maxTopologyDistance"); d != "" {
		if t != "HIGH_THROUGHPUT" {
			return nil, invalid("Invalid value for field '%s.maxTopologyDistance': '%s'. maxTopologyDistance is set with type HIGH_THROUGHPUT only.", field, d)
		}
		switch d {
		case "BLOCK", "CLUSTER", "SUBBLOCK":
		default:
			return nil, invalid("Invalid value for field '%s.maxTopologyDistance': '%s'. The distance must be BLOCK, CLUSTER or SUBBLOCK.", field, d)
		}
		out["maxTopologyDistance"] = d
	}
	if a := str(in, "acceleratorTopology"); a != "" {
		if t != "HIGH_THROUGHPUT" {
			return nil, invalid("Invalid value for field '%s.acceleratorTopology': '%s'. acceleratorTopology is set with type HIGH_THROUGHPUT only.", field, a)
		}
		out["acceleratorTopology"] = a
	}
	return out, nil
}

// instancePolicyUsersLocked are the instances that have the policy rel.
func (s *Server) instancePolicyUsersLocked(rel string) []string {
	var out []string
	for k, o := range s.res {
		if strings.Contains(k, "/instances/") && hasStr(strs(o, "resourcePolicies"), rel) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// checkInstancePoliciesLocked validates policies added to the instance o
// and returns its policies with them.
func (s *Server) checkInstancePoliciesLocked(project string, o obj, add []string, field string) ([]any, error) {
	cur := strs(o, "resourcePolicies")
	out := toAny(cur)
	zone := lastSeg(str(o, "zone"))
	region := regionOfZone(zone)
	self := str(o, "selfLink")
	for i, ref := range add {
		rel := Rel(ref, project)
		if !strings.Contains(rel, "/") {
			rel = regionRel(project, region, "resourcePolicies", rel)
		}
		po, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		f := field + "[" + strconv.Itoa(i) + "]"
		if hasStr(cur, rel) {
			return nil, invalid("Invalid value for field '%s': '%s'. The instance already has this resource policy.", f, rel)
		}
		if r := lastSeg(str(po, "region")); r != region {
			return nil, invalid("Invalid value for field '%s': '%s'. The resource policy must be in the instance's region, %s.", f, rel, region)
		}
		kind := resourcePolicyKind(po)
		switch kind {
		case "instanceSchedulePolicy", "groupPlacementPolicy":
		case "workloadPolicy":
			return nil, invalid("Invalid value for field '%s': '%s'. A workload policy specifies the placement of a managed instance group's instances: it is set on the group, not on an instance.", f, rel)
		default:
			return nil, invalid("Invalid value for field '%s': '%s'. A %s resource policy applies to disks, not to instances.", f, rel, kind)
		}
		for _, c := range cur {
			if cp, ok := s.res[c]; ok && resourcePolicyKind(cp) == kind {
				// RESOURCE_POLICY_DUPLICATE_TYPE_UNSUPPORTED.
				return nil, invalid("Can't apply resource policy '%s'. Multiple resource policies of the same type aren't supported on an instance.", rel)
			}
		}
		users := s.instancePolicyUsersLocked(rel)
		if kind == "instanceSchedulePolicy" && len(users) >= maxScheduleVMs {
			return nil, invalid("Invalid value for field '%s': '%s'. An instance schedule can be attached to at most %d VMs.", f, rel, maxScheduleVMs)
		}
		if kind == "groupPlacementPolicy" {
			if err := s.checkPlacementLocked(o, po, rel, users, self, f); err != nil {
				return nil, err
			}
		}
		cur = append(cur, rel)
		out = append(out, rel)
	}
	return out, nil
}

// checkPlacementLocked applies a placement policy's rules to o joining it.
func (s *Server) checkPlacementLocked(o, po obj, rel string, users []string, self, field string) error {
	gp, _ := po["groupPlacementPolicy"].(map[string]any)
	n := len(users)
	if hasStr(users, self) {
		n--
	}
	mt := lastSeg(str(o, "machineType"))
	if str(gp, "collocation") == "COLLOCATED" {
		md, _, _ := intField(gp["maxDistance"], "")
		if !compactSeries[strings.SplitN(mt, "-", 2)[0]] {
			// PLACEMENT_POLICY_MACHINE_TYPE_UNSUPPORTED.
			return invalid("Placement policies aren't supported for machine type '%s'.", mt)
		}
		if st := str(o, "status"); st != stTerminated && st != stProvisioning && st != "" {
			return errf(http.StatusBadRequest, "resourceNotReady",
				"The resource '%s' is not ready: stop the instance before you apply a compact placement policy to it (it is %s).", self, st)
		}
		sc, _ := o["scheduling"].(map[string]any)
		if (md == 1 || md == 2) && str(sc, "onHostMaintenance") != "TERMINATE" {
			// COLLOCATED_PLACEMENT_POLICY_LIVE_MIGRATION_UNSUPPORTED.
			return invalid("Instances with a collocated placement policy don't support live migration.")
		}
		if n >= compactMax(md) {
			return errf(http.StatusTooManyRequests, "PLACEMENT_CONSTRAINTS_INFEASIBLE",
				"The system cannot provision 1 instance(s) with the '%s' placement policy constraint.", rel)
		}
	} else if n >= maxSpreadVMs {
		// SPREAD_PLACEMENT_POLICY_INSTANCE_LIMIT_EXCEEDED.
		return invalid("The spread placement policy has reached its limit of '%d' instances.", maxSpreadVMs)
	}
	if vmc, ok, _ := intField(gp["vmCount"], ""); ok && int64(n) >= vmc {
		return errf(http.StatusTooManyRequests, "PLACEMENT_CONSTRAINTS_INFEASIBLE",
			"The system cannot provision 1 instance(s) with the '%s' placement policy constraint.", rel)
	}
	return nil
}

// placeLocked reports the placement of o under its placement policy: a
// spread policy's availability domain (the least used one, unless o has
// one), a compact policy's physical host topology.
func (s *Server) placeLocked(o obj) {
	sc, _ := o["scheduling"].(map[string]any)
	rs, _ := o["resourceStatus"].(map[string]any)
	if rs != nil {
		delete(rs, "physicalHostTopology")
		if len(rs) == 0 {
			delete(o, "resourceStatus")
		}
	}
	var pol string
	var po obj
	for _, p := range strs(o, "resourcePolicies") {
		if x, ok := s.res[p]; ok && x["groupPlacementPolicy"] != nil {
			pol, po = p, x
		}
	}
	if po == nil {
		return
	}
	gp, _ := po["groupPlacementPolicy"].(map[string]any)
	if str(gp, "collocation") != "COLLOCATED" {
		adc, _, _ := intField(gp["availabilityDomainCount"], "")
		if d, ok, _ := intField(sc["availabilityDomain"], ""); ok && d >= 1 && d <= adc {
			return
		}
		count := make([]int, adc+1)
		for _, u := range s.instancePolicyUsersLocked(pol) {
			if uo := s.res[u]; u != str(o, "selfLink") {
				usc, _ := uo["scheduling"].(map[string]any)
				if d, ok, _ := intField(usc["availabilityDomain"], ""); ok && d >= 1 && d <= adc {
					count[d]++
				}
			}
		}
		best := int64(1)
		for d := int64(2); d <= adc; d++ {
			if count[d] < count[best] {
				best = d
			}
		}
		if sc == nil {
			sc = obj{}
			o["scheduling"] = sc
		}
		sc["availabilityDomain"] = best
		return
	}
	md, _, _ := intField(gp["maxDistance"], "")
	id := str(po, "id")
	topo := obj{"cluster": "cluster-" + id[len(id)-4:]}
	if md == 1 || md == 2 {
		topo["block"] = "block-" + id[len(id)-6:len(id)-3]
	}
	if md == 1 {
		topo["subblock"] = "subblock-" + id[len(id)-3:]
	}
	topo["host"] = "host-" + str(o, "id")[len(str(o, "id"))-6:]
	if rs == nil {
		rs = obj{}
		o["resourceStatus"] = rs
	}
	rs["physicalHostTopology"] = topo
}

// instancePolicies is instances.addResourcePolicies or
// removeResourcePolicies.
func (s *Server) instancePolicies(c call, scope, rel string, o obj, add bool) (obj, error) {
	refs := strs(c.body, "resourcePolicies")
	if v := str(c.body, "resourcePolicies"); v != "" {
		refs = []string{v}
	}
	if len(refs) == 0 {
		return nil, required("resourcePolicies")
	}
	verb := "removeResourcePolicies"
	if add {
		verb = "addResourcePolicies"
		l, err := s.checkInstancePoliciesLocked(c.project, o, refs, "resourcePolicies")
		if err != nil {
			return nil, err
		}
		o["resourcePolicies"] = l
		s.placeLocked(o)
		return s.done(c.project, scope, verb, rel, o["id"])
	}
	cur := strs(o, "resourcePolicies")
	for i, ref := range refs {
		pr := Rel(ref, c.project)
		if !strings.Contains(pr, "/") {
			pr = regionRel(c.project, regionOfZone(lastSeg(str(o, "zone"))), "resourcePolicies", pr)
		}
		if !hasStr(cur, pr) {
			return nil, invalid("Invalid value for field 'resourcePolicies[%d]': '%s'. The instance does not have this resource policy.", i, ref)
		}
		refs[i] = pr
	}
	var keep []any
	for _, p := range cur {
		if !hasStr(refs, p) {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		delete(o, "resourcePolicies")
	} else {
		o["resourcePolicies"] = keep
	}
	s.placeLocked(o)
	return s.done(c.project, scope, verb, rel, o["id"])
}

// runInstanceSchedulesLocked starts and stops the VMs of each instance
// schedule that fell due since it was last run.
func (s *Server) runInstanceSchedulesLocked(now time.Time) {
	var pols []string
	for k, o := range s.res {
		if strings.Contains(k, "/resourcePolicies/") && o["instanceSchedulePolicy"] != nil {
			pols = append(pols, k)
		}
	}
	sort.Strings(pols)
	for _, pol := range pols {
		po := s.res[pol]
		isp, _ := po["instanceSchedulePolicy"].(map[string]any)
		loc, err := time.LoadLocation(str(isp, "timeZone"))
		if err != nil {
			loc = time.UTC
		}
		since, _ := time.Parse(time.RFC3339Nano, str(po, "_lastChecked"))
		if since.IsZero() {
			since, _ = time.Parse(time.RFC3339Nano, str(po, "creationTimestamp"))
		}
		start, _ := time.Parse(time.RFC3339, str(isp, "startTime"))
		end, _ := time.Parse(time.RFC3339, str(isp, "expirationTime"))
		var next time.Time
		for _, k := range []string{"vmStartSchedule", "vmStopSchedule"} {
			m, _ := isp[k].(map[string]any)
			if m == nil {
				continue
			}
			sc, err := instanceCron.Parse(str(m, "schedule"))
			if err != nil {
				continue
			}
			due := time.Time{}
			for t := sc.Next(since.In(loc)); !t.After(now); t = sc.Next(t) {
				due = t
			}
			if n := sc.Next(now.In(loc)); next.IsZero() || n.Before(next) {
				next = n
			}
			if due.IsZero() || (!start.IsZero() && due.Before(start)) || (!end.IsZero() && due.After(end)) {
				continue
			}
			rs, _ := po["resourceStatus"].(map[string]any)
			if rs == nil {
				rs = obj{}
				po["resourceStatus"] = rs
			}
			st, _ := rs["instanceSchedulePolicy"].(map[string]any)
			if st == nil {
				st = obj{}
				rs["instanceSchedulePolicy"] = st
			}
			st["lastRunStartTime"] = due.UTC().Format(time.RFC3339)
			for _, u := range s.instancePolicyUsersLocked(pol) {
				s.scheduledLocked(u, k == "vmStartSchedule", due)
			}
		}
		if !next.IsZero() && (end.IsZero() || !next.After(end)) {
			rs, _ := po["resourceStatus"].(map[string]any)
			if rs == nil {
				rs = obj{}
				po["resourceStatus"] = rs
			}
			st, _ := rs["instanceSchedulePolicy"].(map[string]any)
			if st == nil {
				st = obj{}
				rs["instanceSchedulePolicy"] = st
			}
			st["nextRunStartTime"] = next.UTC().Format(time.RFC3339)
		}
		po["_lastChecked"] = now.UTC().Format(time.RFC3339Nano)
	}
}

// scheduledLocked starts or stops one VM of an instance schedule: only
// one attached before the operation fell due, and in the state it acts on.
func (s *Server) scheduledLocked(rel string, start bool, due time.Time) {
	o, ok := s.res[rel]
	if !ok || s.opts.VMs == nil {
		return
	}
	if at, err := time.Parse(time.RFC3339Nano, str(o, "_policiesAt")); err == nil && at.After(due) {
		return
	}
	p := strings.Split(rel, "/")
	verb := "stop"
	if start {
		if str(o, "status") != stTerminated {
			return
		}
		verb = "start"
	} else if st := str(o, "status"); st != stRunning && st != stSuspended {
		return
	}
	_, _ = s.instances(call{method: http.MethodPost, project: p[1], parts: []string{"zones", p[3], "instances", p[5], verb}, body: obj{}}, p[3])
}

// patchInstanceSchedule is resourcePolicies.patch of an instance
// schedule: "Modify a schedule" with its description and
// instanceSchedulePolicy.
func (s *Server) patchInstanceSchedule(c call, rel string, po obj) (obj, error) {
	b := c.body
	if v, ok := b["instanceSchedulePolicy"]; ok && v != nil {
		isp, err := newInstanceSchedule(v, "resource.instanceSchedulePolicy")
		if err != nil {
			return nil, err
		}
		po["instanceSchedulePolicy"] = isp
		delete(po, "resourceStatus")
	}
	if _, ok := b["description"]; ok {
		if d := str(b, "description"); d != "" {
			po["description"] = d
		} else {
			delete(po, "description")
		}
	}
	return s.done(c.project, "regions/"+lastSeg(str(po, "region")), "patch", rel, po["id"])
}
