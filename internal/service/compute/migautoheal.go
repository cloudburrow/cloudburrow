package compute

import (
	"sort"
	"strings"
	"time"
)

// Managed instance group autohealing (#2225), as
// https://docs.cloud.google.com/compute/docs/instance-groups/autohealing-instances-in-migs
// describes it (read 2026-11-24): a group has at most one autohealing
// policy, a global or regional health check (legacy HTTP and HTTPS health
// checks "continue to work") and an initialDelaySec, 0 to 3600, 0 by
// default. The group probes each of its VMs with the health check, from
// Google's probe ranges (so the VM's firewall must let them in, as
// lbhealth.go checks). A VM's initial delay "starts when the VM's
// currentAction field changes to VERIFYING", once it is made and running,
// and "stops when the set time completes or when a health check
// succeeds"; during it "the MIG ignores unsuccessful health checks". After
// it, a VM whose health is UNHEALTHY or TIMEOUT is recreated (RECREATING:
// deleted and made again by the same name, through the creation retries
// of migretry.go, so a recreation that fails is retried and listed with
// lastAttempt.errors). A VM whose health is UNKNOWN is left alone.
//
// The group recreates no more VMs at a time than its update policy's
// maxUnavailable (a fixed number, or a percent of its target size; by
// default its number of zones) less those already unavailable (being
// made, re-made, or verified). Its rolling updates likewise count a VM as
// available only once its health check has found it HEALTHY, as
// updatePolicy.maxUnavailable's definition has it ("the instance's health
// check status must be HEALTHY at least once").
//
// listManagedInstances reports each made VM's instanceHealth[]: the
// health check and its detailedHealthState (UNKNOWN until a threshold is
// reached, HEALTHY, TIMEOUT when the probe found no connection or no
// answer in time, UNHEALTHY when the VM answered but not as the health
// check requires).

// checkAutoHealingPoliciesLocked is autoHealingPolicies as insert and
// patch take them, for a group of region (a zonal group's region).
func (s *Server) checkAutoHealingPoliciesLocked(project, region string, v any) ([]any, error) {
	l := listOf(v)
	if len(l) > 1 {
		return nil, invalid("Invalid value for field 'resource.autoHealingPolicies': only one autohealing policy is supported.")
	}
	out := []any{}
	for i, e := range l {
		p, _ := e.(map[string]any)
		delay, hasDelay := 0.0, false
		if d, ok := p["initialDelaySec"]; ok && d != nil {
			f, ok := numOf(d)
			if !ok || f < 0 || f > 3600 || f != float64(int(f)) {
				return nil, invalid("Invalid value for field 'resource.autoHealingPolicies[%d].initialDelaySec': '%v'. Must be greater than or equal to 0 and less than or equal to 3600.", i, d)
			}
			delay, hasDelay = f, true
		}
		ref := str(p, "healthCheck")
		if ref == "" {
			// A policy with no health check: gcloud's update
			// --clear-autohealing sends one empty policy, which clears
			// autohealing; --initial-delay alone sets the delay with no
			// health check to probe with.
			if hasDelay {
				out = append(out, obj{"initialDelaySec": delay})
			}
			continue
		}
		hc := Rel(ref, project)
		o, ok := s.res[hc]
		if !ok || !isAutohealHealthCheck(hc) {
			return nil, notFound(hc)
		}
		if r := lastSeg(str(o, "region")); r != "" && r != region {
			return nil, invalid("Invalid value for field 'resource.autoHealingPolicies[%d].healthCheck': '%s'. A regional health check must be in the group's region, '%s'.", i, ref, region)
		}
		out = append(out, obj{"healthCheck": hc, "initialDelaySec": delay})
	}
	return out, nil
}

// numOf is a JSON number, or a number in a string (int64 fields).
func numOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		var f float64
		n := 0
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0, false
			}
			f = f*10 + float64(c-'0')
			n++
		}
		return f, n > 0
	}
	return 0, false
}

func isAutohealHealthCheck(rel string) bool {
	return strings.Contains(rel, "/healthChecks/") || strings.Contains(rel, "/httpHealthChecks/") || strings.Contains(rel, "/httpsHealthChecks/")
}

// autohealPolicyOf is a manager's autohealing health check and initial
// delay; hc is "" when it has none.
func autohealPolicyOf(m obj) (hc string, delay time.Duration) {
	l := listOf(m["autoHealingPolicies"])
	if len(l) == 0 {
		return "", 0
	}
	p, _ := l[0].(map[string]any)
	d, _ := numOf(p["initialDelaySec"])
	return str(p, "healthCheck"), time.Duration(d) * time.Second
}

// autohealProbeLocked is the probe of one of a manager's VMs by its
// autohealing health check; ok is false when it has none, or the VM is
// gone. The key names the VM's id, so a VM made again is probed afresh.
func (s *Server) autohealProbeLocked(m obj, vm string) (lbProbeTask, bool) {
	hc, _ := autohealPolicyOf(m)
	h, ok := s.res[hc]
	o, made := s.res[vm]
	if hc == "" || !ok || !made {
		return lbProbeTask{}, false
	}
	p := probeOf(h)
	port := p.port
	if p.portName != "" {
		port = namedPort(s.res[str(m, "instanceGroup")], p.portName)
	}
	if port == 0 {
		// USE_SERVING_PORT, or a named port the group lacks: autohealing
		// has no serving port to probe.
		return lbProbeTask{}, false
	}
	ep, ok := s.instanceEndpointLocked(vm, port, str(m, "instanceGroup"))
	if !ok {
		return lbProbeTask{}, false
	}
	sources := hcProbeSources
	if !strings.Contains(hc, "/healthChecks/") {
		sources = legacyProbeSources
	}
	return lbProbeTask{key: probeKeyOf(hc, p, ep, port) + "|autoheal|" + str(o, "id"), probe: p, ep: ep, port: port,
		allowed: s.probesAllowedLocked(ep, port, sources)}, true
}

// autohealProbesLocked are the probes every autohealing group needs.
func (s *Server) autohealProbesLocked() []lbProbeTask {
	var out []lbProbeTask
	for k, m := range s.res {
		if !strings.Contains(k, "/instanceGroupManagers/") || len(listOf(m["autoHealingPolicies"])) == 0 {
			continue
		}
		for _, vm := range s.liveManagedLocked(k) {
			if t, ok := s.autohealProbeLocked(m, vm); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// detailedHealthLocked is a managed VM's detailedHealthState under its
// group's autohealing health check.
func (s *Server) detailedHealthLocked(m obj, vm string) string {
	t, ok := s.autohealProbeLocked(m, vm)
	if !ok {
		return "UNKNOWN"
	}
	st, ok := s.lb.health.get(t.key)
	switch {
	case !ok || !st.decided:
		return "UNKNOWN"
	case st.healthy:
		return "HEALTHY"
	case st.unreachable:
		return "TIMEOUT"
	}
	return "UNHEALTHY"
}

// instanceHealthLocked is listManagedInstances' instanceHealth[] of a made
// VM; nil when the group has no autohealing policy.
func (s *Server) instanceHealthLocked(m obj, vm string) []any {
	hc, _ := autohealPolicyOf(m)
	if hc == "" {
		return nil
	}
	return []any{obj{"healthCheck": hc, "detailedHealthState": s.detailedHealthLocked(m, vm)}}
}

// maxUnavailableOf is a manager's updatePolicy.maxUnavailable, calculated
// for its target size: by default its number of zones.
func maxUnavailableOf(m obj) int {
	size, _ := m["targetSize"].(float64)
	up, _ := m["updatePolicy"].(map[string]any)
	if up["maxUnavailable"] != nil {
		if _, n, err := fixedOrPercent("maxUnavailable", up["maxUnavailable"], int(size)); err == nil {
			return n
		}
	}
	return len(managerZones(m))
}

// unhealthyYetLocked are a manager's live VMs its autohealing health check
// has not yet found HEALTHY: not available, for its rolling updates.
func (s *Server) unhealthyYetLocked(rel string, m obj) int {
	if hc, _ := autohealPolicyOf(m); hc == "" {
		return 0
	}
	n := 0
	for _, vm := range s.liveManagedLocked(rel) {
		if s.res[vm]["_ahWasHealthy"] != true {
			n++
		}
	}
	return n
}

// autohealLocked takes one autohealing pass over every group: VMs that
// finished being made start VERIFYING, those past their initial delay or
// found HEALTHY are verified, and verified VMs found UNHEALTHY or TIMEOUT
// are recreated, within maxUnavailable. It reports whether it changed
// anything.
func (s *Server) autohealLocked() bool {
	var mgrs []string
	for k, m := range s.res {
		if strings.Contains(k, "/instanceGroupManagers/") && len(listOf(m["autoHealingPolicies"])) > 0 {
			mgrs = append(mgrs, k)
		}
	}
	sort.Strings(mgrs)
	changed := false
	for _, k := range mgrs {
		if s.autohealManagerLocked(k, s.res[k]) {
			changed = true
		}
	}
	return changed
}

func (s *Server) autohealManagerLocked(rel string, m obj) bool {
	now := s.opts.Now()
	_, delay := autohealPolicyOf(m)
	changed := false
	busy := 0
	for vm := range pendingOf(m) {
		if _, made := s.res[vm]; !made {
			busy++ // being made, or made again
		}
	}
	var repair []string
	for _, vm := range s.liveManagedLocked(rel) {
		o := s.res[vm]
		health := s.detailedHealthLocked(m, vm)
		if health == "HEALTHY" && o["_ahWasHealthy"] != true {
			o["_ahWasHealthy"] = true
			changed = true
		}
		if s.baseActionLocked(vm, o) != "NONE" || str(o, "status") != stRunning {
			busy++
			continue
		}
		if o["_ahVerified"] != true {
			since, ok := o["_ahSince"].(float64)
			if !ok {
				since = float64(now.UnixNano())
				o["_ahSince"] = since
				changed = true
			}
			if health == "HEALTHY" || now.Sub(time.Unix(0, int64(since))) >= delay {
				o["_ahVerified"] = true
				delete(o, "_ahVerifying")
				changed = true
			} else {
				if o["_ahVerifying"] != true {
					o["_ahVerifying"] = true
					changed = true
				}
				busy++
				continue
			}
		}
		if health == "UNHEALTHY" || health == "TIMEOUT" {
			repair = append(repair, vm)
		}
	}
	budget := maxUnavailableOf(m) - busy
	sort.Strings(repair)
	for _, vm := range repair {
		if budget <= 0 {
			break
		}
		o := s.res[vm]
		tpl := s.desiredTemplatesLocked(m, s.liveManagedLocked(rel))[vm]
		if tpl == "" {
			tpl = str(m, "instanceTemplate")
		}
		if err := s.remakeManagedLocked(projectOfRel(rel), rel, m, vm, o, tpl); err != nil {
			continue
		}
		budget--
		changed = true
	}
	return changed
}
