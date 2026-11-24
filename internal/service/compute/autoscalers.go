package compute

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Autoscalers (#2139): autoscalers and regionAutoscalers
// (https://docs.cloud.google.com/compute/docs/reference/rest/v1/autoscalers,
// compute/docs/autoscaler/scaling-cpu and understanding-autoscaler-decisions,
// read 2026-11-22) that really add and remove a managed group's VMs. The
// load signal is the VMs' own CPU: every AutoscaleEvery each running VM's
// CPU seconds are read from its backend, and its utilization is the CPU
// time used since the last reading over the wall time times the machine
// type's vCPUs. An autoscaler's recommendation is, as Google documents it,
// the size that brings the group's average utilization to its target,
// ceil(serving VMs × average utilization / target), from the VMs past
// their initialization period (coolDownPeriodSec), clamped to
// [minNumReplicas, maxNumReplicas]; it scales out at once, and scales in
// only to the highest recommendation of the stabilization period (10
// minutes, or the initialization period when longer) and no further than
// scaleInControl allows below the group's peak size in its time window.
// In mode OFF the recommendation is still made, and shown, but not acted
// on; ONLY_SCALE_OUT acts only when it is larger. The readings are also
// compute.googleapis.com/instance/cpu/utilization (CPUSamples).

// VMCPU is a VMBackend that can read a running VM's CPU time.
type VMCPU interface {
	// CPUUsage is the CPU time, in seconds, the VM has used since it
	// booted (its cgroup's cpu.stat usage_usec).
	CPUUsage(ctx context.Context, vm VMSpec) (float64, error)
}

// CPUSample is a running VM's last measured CPU utilization.
type CPUSample struct {
	Project, Zone, Name string
	// InstanceID is the instance's numeric id.
	InstanceID string
	// Utilization is the fraction of its vCPUs the VM used, in [0, 1],
	// between Start and End.
	Utilization float64
	Start, End  time.Time
}

// stabilization is how far back the autoscaler looks for its highest
// recommendation before it scales in, as Google documents.
const stabilization = 10 * time.Minute

type cpuReading struct {
	at   time.Time
	secs float64
	boot string // lastStartTimestamp: a new boot restarts the counter
}

type sizePoint struct {
	at   time.Time
	size int
}

// scaleState is what the autoscalers remember between passes; it is not
// saved, so a restarted instance starts its windows afresh.
type scaleState struct {
	last map[string]cpuReading // by instance
	util map[string]CPUSample  // by instance
	recs map[string][]sizePoint
	// sizes are each autoscaler's group sizes, for scaleInControl.
	sizes map[string][]sizePoint
}

var autoscalerModes = map[string]bool{"ON": true, "OFF": true, "ONLY_SCALE_OUT": true, "ONLY_UP": true}

func autoscalerMode(a obj) string {
	pol, _ := a["autoscalingPolicy"].(map[string]any)
	if m := str(pol, "mode"); m != "" {
		return m
	}
	return "ON"
}

// autoscalerOfLocked is the autoscaler whose target is a manager, or nil.
func (s *Server) autoscalerOfLocked(mgr string) obj {
	for k, o := range s.res {
		if strings.Contains(k, "/autoscalers/") && str(o, "target") == mgr {
			return o
		}
	}
	return nil
}

func (s *Server) autoscalers(c call, l igLoc) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := l.coll("autoscalers")
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	patch, put := c.method == http.MethodPatch, c.method == http.MethodPut
	switch {
	case n == 3 && get:
		out, err := s.listOut("compute#"+map[bool]string{false: "autoscalerList", true: "regionAutoscalerList"}[l.regional()], coll, s.res, c.query)
		if err == nil {
			for _, a := range out["items"].([]obj) {
				s.refreshAutoscalerLocked(a)
			}
		}
		return out, err
	case n == 3 && post:
		return s.insertAutoscaler(c, l)
	case n == 3 && (patch || put):
		name := first(c.query, "autoscaler")
		if name == "" {
			name = str(c.body, "name")
		}
		if name == "" {
			return nil, required("autoscaler")
		}
		rel := coll + "/" + name
		a, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		return s.updateAutoscaler(c, l, rel, a, patch)
	case n == 4 && get:
		rel := coll + "/" + p[3]
		a, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		s.refreshAutoscalerLocked(a)
		return a, nil
	case n == 4 && del:
		rel := coll + "/" + p[3]
		a, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		delete(s.res, rel)
		delete(s.scale.recs, rel)
		delete(s.scale.sizes, rel)
		return s.done(c.project, l.scope, "compute.autoscalers.delete", rel, a["id"])
	}
	return nil, unimplemented("%s on autoscalers/%s is not served", c.method, strings.Join(p[3:], "/"))
}

// checkTargetLocked is an autoscaler's target: a managed group of its own
// zone or region, with no other autoscaler.
func (s *Server) checkTargetLocked(c call, l igLoc, target, self string) (string, error) {
	if target == "" {
		return "", required("resource.target")
	}
	t := Rel(target, c.project)
	if _, ok := s.res[t]; !ok || !strings.HasPrefix(t, l.coll("instanceGroupManagers")+"/") {
		return "", invalid("Invalid value for field 'resource.target': '%s'. The target must be a managed instance group in %s.", target, l.scope)
	}
	if a := s.autoscalerOfLocked(t); a != nil && str(a, "selfLink") != self {
		return "", errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
			"The resource '%s' is already being used by '%s'", t, str(a, "selfLink"))
	}
	return t, nil
}

// checkPolicy is an autoscalingPolicy with Google's defaults filled in.
func checkPolicy(v any) (obj, error) {
	in, _ := v.(map[string]any)
	pol := obj{}
	if in == nil {
		return nil, required("resource.autoscalingPolicy.maxNumReplicas")
	}
	for _, k := range []string{"customMetricUtilizations", "loadBalancingUtilization", "scalingSchedules"} {
		switch x := in[k].(type) {
		case nil:
		case []any:
			if len(x) > 0 {
				return nil, unimplemented("autoscalingPolicy.%s is not served: CloudBurrow's autoscalers scale on CPU utilization", k)
			}
		case map[string]any:
			if len(x) > 0 {
				return nil, unimplemented("autoscalingPolicy.%s is not served: CloudBurrow's autoscalers scale on CPU utilization", k)
			}
		}
	}
	maxR, ok := in["maxNumReplicas"].(float64)
	if !ok {
		return nil, required("resource.autoscalingPolicy.maxNumReplicas")
	}
	minR, ok := in["minNumReplicas"].(float64)
	if !ok {
		// "If not provided, autoscaler chooses a default value depending
		// on maximum number of instances allowed": one, or none when at
		// most none are.
		minR = math.Min(1, maxR)
	}
	if minR < 0 || minR != math.Trunc(minR) {
		return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.minNumReplicas': '%v'. Must be greater than or equal to 0.", in["minNumReplicas"])
	}
	if maxR != math.Trunc(maxR) || maxR < minR {
		return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.maxNumReplicas': '%v'. The maximum number of replicas must not be lower than minimal number of replicas.", in["maxNumReplicas"])
	}
	if maxR > 1000 {
		return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.maxNumReplicas': '%v'. Must be less than or equal to 1000.", in["maxNumReplicas"])
	}
	pol["minNumReplicas"], pol["maxNumReplicas"] = minR, maxR
	cool := 60.0
	if v, ok := in["coolDownPeriodSec"].(float64); ok {
		if v < 0 || v > 3600 {
			return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.coolDownPeriodSec': '%v'. Must be between 0 and 3600.", v)
		}
		cool = v
	}
	pol["coolDownPeriodSec"] = cool
	cpu, _ := in["cpuUtilization"].(map[string]any)
	target := 0.6
	if v, ok := cpu["utilizationTarget"].(float64); ok {
		if v <= 0 || v > 1 {
			return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.cpuUtilization.utilizationTarget': '%v'. Must be a float value in the range (0, 1].", v)
		}
		target = v
	}
	method := str(cpu, "predictiveMethod")
	if method == "" {
		method = "NONE"
	}
	switch method {
	case "NONE":
	case "OPTIMIZE_AVAILABILITY":
		return nil, unimplemented("predictive autoscaling (predictiveMethod OPTIMIZE_AVAILABILITY) is not served: it forecasts from days of load history")
	default:
		return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.cpuUtilization.predictiveMethod': '%s'.", method)
	}
	pol["cpuUtilization"] = obj{"utilizationTarget": target, "predictiveMethod": method}
	mode := str(in, "mode")
	if mode == "" {
		mode = "ON"
	}
	if !autoscalerModes[mode] {
		return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.mode': '%s'. Must be one of ON, OFF or ONLY_SCALE_OUT.", mode)
	}
	pol["mode"] = mode
	if sc, ok := in["scaleInControl"].(map[string]any); ok && len(sc) > 0 {
		out := obj{}
		if mr, ok := sc["maxScaledInReplicas"].(map[string]any); ok {
			fx, hasF := mr["fixed"].(float64)
			pc, hasP := mr["percent"].(float64)
			switch {
			case hasF && hasP:
				return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.scaleInControl.maxScaledInReplicas': set fixed or percent, not both.")
			case hasF:
				if fx <= 0 || fx != math.Trunc(fx) {
					return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.scaleInControl.maxScaledInReplicas.fixed': '%v'. This must be a positive integer.", fx)
				}
				out["maxScaledInReplicas"] = obj{"fixed": fx}
			case hasP:
				if pc < 0 || pc > 100 || pc != math.Trunc(pc) {
					return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.scaleInControl.maxScaledInReplicas.percent': '%v'. Specifies a percentage of instances between 0 to 100%%, inclusive.", pc)
				}
				out["maxScaledInReplicas"] = obj{"percent": pc}
			}
		}
		if tw, ok := sc["timeWindowSec"].(float64); ok {
			if tw < 0 || tw > 3600 {
				return nil, invalid("Invalid value for field 'resource.autoscalingPolicy.scaleInControl.timeWindowSec': '%v'. Must be between 0 and 3600.", tw)
			}
			out["timeWindowSec"] = tw
		} else if len(out) > 0 {
			out["timeWindowSec"] = 600.0
		}
		pol["scaleInControl"] = out
	}
	return norm(pol), nil
}

func (s *Server) insertAutoscaler(c call, l igLoc) (obj, error) {
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := l.coll("autoscalers") + "/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	t, err := s.checkTargetLocked(c, l, str(b, "target"), rel)
	if err != nil {
		return nil, err
	}
	pol, err := checkPolicy(b["autoscalingPolicy"])
	if err != nil {
		return nil, err
	}
	locKey, locVal := l.field()
	a := obj{"kind": "compute#autoscaler", "id": newID(), "name": name, locKey: locVal, "selfLink": rel,
		"creationTimestamp": s.stamp(), "target": t, "autoscalingPolicy": pol, "status": "ACTIVE", "statusDetails": []any{}}
	if d := str(b, "description"); d != "" {
		a["description"] = d
	}
	s.res[rel] = norm(a)
	s.refreshAutoscalerLocked(s.res[rel])
	return s.done(c.project, l.scope, "compute.autoscalers.insert", rel, a["id"])
}

// updateAutoscaler is autoscalers.patch (the fields given replace the
// autoscaler's, autoscalingPolicy's field by field) and autoscalers.update
// (the body replaces it).
func (s *Server) updateAutoscaler(c call, l igLoc, rel string, a obj, patch bool) (obj, error) {
	b := c.body
	target := str(b, "target")
	if target == "" && patch {
		target = str(a, "target")
	}
	t, err := s.checkTargetLocked(c, l, target, rel)
	if err != nil {
		return nil, err
	}
	in := b["autoscalingPolicy"]
	if patch {
		merged, _ := deepCopy(a["autoscalingPolicy"]).(map[string]any)
		if merged == nil {
			merged = map[string]any{}
		}
		if bp, ok := in.(map[string]any); ok {
			for k, v := range bp {
				if k == "cpuUtilization" || k == "scaleInControl" {
					sub, _ := merged[k].(map[string]any)
					if sub == nil {
						sub = map[string]any{}
					}
					if vm, ok := v.(map[string]any); ok {
						for sk, sv := range vm {
							sub[sk] = sv
						}
						merged[k] = sub
						continue
					}
				}
				merged[k] = v
			}
		}
		in = merged
	}
	pol, err := checkPolicy(in)
	if err != nil {
		return nil, err
	}
	a["target"], a["autoscalingPolicy"] = t, pol
	if d, ok := b["description"].(string); ok || !patch {
		a["description"] = d
	}
	s.refreshAutoscalerLocked(a)
	return s.done(c.project, l.scope, map[bool]string{true: "compute.autoscalers.patch", false: "compute.autoscalers.update"}[patch], rel, a["id"])
}

// refreshAutoscalerLocked sets an autoscaler's status and details from
// its target and policy.
func (s *Server) refreshAutoscalerLocked(a obj) {
	if _, ok := s.res[str(a, "target")]; !ok {
		a["status"] = "ERROR"
		a["statusDetails"] = []any{obj{"type": "SCALING_TARGET_DOES_NOT_EXIST",
			"message": "The target instance group manager '" + lastSeg(str(a, "target")) + "' does not exist."}}
		delete(a, "recommendedSize")
		return
	}
	a["status"] = "ACTIVE"
	var details []any
	pol, _ := a["autoscalingPolicy"].(map[string]any)
	if pol["minNumReplicas"] == pol["maxNumReplicas"] {
		details = append(details, obj{"type": "MIN_EQUALS_MAX",
			"message": "The minimum and maximum number of instances are equal: the autoscaler keeps the group at that size."})
	}
	switch autoscalerMode(a) {
	case "OFF":
		details = append(details, obj{"type": "MODE_OFF",
			"message": "Autoscaling is turned off. The number of instances in the group won't change automatically. The autoscaling configuration is preserved."})
	case "ONLY_SCALE_OUT", "ONLY_UP":
		details = append(details, obj{"type": "MODE_ONLY_SCALE_OUT",
			"message": "Autoscaling is limited to scaling out: instances in the group are only added."})
	}
	if a["_capped"] == true {
		details = append(details, obj{"type": "CAPPED_AT_MAX_NUM_REPLICAS",
			"message": "The number of instances has reached the maxNumReplicas. The autoscaler cannot add more instances."})
	}
	if a["_unhealthy"] == true {
		details = append(details, obj{"type": "ALL_INSTANCES_UNHEALTHY",
			"message": "All instances in the instance group are unhealthy (not in RUNNING state). If this is an error, check the instances."})
	}
	if details == nil {
		details = []any{}
	}
	a["statusDetails"] = details
}

func (s *Server) cpuOf() func(ctx context.Context, vm VMSpec) (float64, error) {
	if s.opts.CPU != nil {
		return s.opts.CPU
	}
	if r, ok := s.opts.VMs.(VMCPU); ok {
		return r.CPUUsage
	}
	return nil
}

// autoscaleLoop reads the VMs' CPU and runs the autoscalers every
// AutoscaleEvery until ctx ends.
func (s *Server) autoscaleLoop(ctx context.Context) {
	every := s.opts.AutoscaleEvery
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.autoscaleOnce(ctx)
	}
}

// autoscaleOnce is one pass: every running VM's CPU read (without the
// lock: the backend may take a while), then each autoscaler run.
func (s *Server) autoscaleOnce(ctx context.Context) {
	read := s.cpuOf()
	type vmAt struct {
		rel, boot string
		spec      VMSpec
		id        string
	}
	var vms []vmAt
	s.mu.Lock()
	if s.scale.last == nil {
		s.scale = scaleState{last: map[string]cpuReading{}, util: map[string]CPUSample{}, recs: map[string][]sizePoint{}, sizes: map[string][]sizePoint{}}
	}
	if read != nil {
		for k, o := range s.res {
			if strings.Contains(k, "/instances/") && str(o, "status") == stRunning && s.queued[k] == 0 {
				vms = append(vms, vmAt{rel: k, boot: str(o, "lastStartTimestamp"), spec: s.vmSpecLocked(o), id: str(o, "id")})
			}
		}
	}
	s.mu.Unlock()
	type got struct {
		vmAt
		secs float64
	}
	var readings []got
	for _, v := range vms {
		secs, err := read(ctx, v.spec)
		if err != nil {
			continue
		}
		readings = append(readings, got{v, secs})
	}
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range readings {
		seen[r.rel] = true
		prev, ok := s.scale.last[r.rel]
		s.scale.last[r.rel] = cpuReading{at: now, secs: r.secs, boot: r.boot}
		if !ok || prev.boot != r.boot || r.secs < prev.secs || !now.After(prev.at) || r.spec.CPUs <= 0 {
			continue
		}
		u := (r.secs - prev.secs) / (now.Sub(prev.at).Seconds() * float64(r.spec.CPUs))
		u = math.Max(0, math.Min(1, u))
		s.scale.util[r.rel] = CPUSample{Project: r.spec.Project, Zone: r.spec.Zone, Name: r.spec.Name, InstanceID: r.id,
			Utilization: u, Start: prev.at, End: now}
	}
	for k := range s.scale.last {
		if !seen[k] {
			delete(s.scale.last, k)
			delete(s.scale.util, k)
		}
	}
	keys := make([]string, 0)
	for k := range s.res {
		if strings.Contains(k, "/autoscalers/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	changed := false
	for _, k := range keys {
		if s.runAutoscalerLocked(k, s.res[k], now) {
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

// runAutoscalerLocked makes an autoscaler's recommendation and, as its
// mode allows, sets its group's size to it; it reports whether it did.
func (s *Server) runAutoscalerLocked(rel string, a obj, now time.Time) bool {
	mrel := str(a, "target")
	m, ok := s.res[mrel]
	if !ok {
		s.refreshAutoscalerLocked(a)
		return false
	}
	pol, _ := a["autoscalingPolicy"].(map[string]any)
	minR, _ := pol["minNumReplicas"].(float64)
	maxR, _ := pol["maxNumReplicas"].(float64)
	cool, _ := pol["coolDownPeriodSec"].(float64)
	cpu, _ := pol["cpuUtilization"].(map[string]any)
	target, _ := cpu["utilizationTarget"].(float64)
	if target <= 0 {
		target = 0.6
	}
	cur := int(vpcFloat(m["targetSize"]))
	vms := s.liveManagedLocked(mrel)
	var load float64
	serving, running := 0, 0
	for _, vm := range vms {
		o := s.res[vm]
		if str(o, "status") != stRunning {
			continue
		}
		running++
		// A VM in its initialization period is left out: its CPU is the
		// boot's, not the load's.
		if st, err := time.Parse(time.RFC3339Nano, str(o, "lastStartTimestamp")); err == nil && now.Sub(st) < time.Duration(cool)*time.Second {
			continue
		}
		u, ok := s.scale.util[vm]
		if !ok {
			continue
		}
		load += u.Utilization
		serving++
	}
	a["_unhealthy"] = len(vms) > 0 && running == 0
	rec := cur
	if serving > 0 {
		// The size whose average utilization is the target; a hair under
		// a whole number is that number, not the next.
		rec = int(math.Ceil(load/target - 1e-9))
	}
	a["_capped"] = rec > int(maxR)
	rec = int(math.Max(minR, math.Min(maxR, float64(rec))))
	// Scale in only to the stabilization period's highest
	// recommendation.
	window := stabilization
	if c := time.Duration(cool) * time.Second; c > window {
		window = c
	}
	hist := append(trimPoints(s.scale.recs[rel], now, window), sizePoint{now, rec})
	s.scale.recs[rel] = hist
	final := rec
	if rec < cur {
		peak := 0
		for _, p := range hist {
			peak = max(peak, p.size)
		}
		final = min(cur, peak)
		// scaleInControl: no more than maxScaledInReplicas below the
		// group's peak size in its time window.
		if sc, ok := pol["scaleInControl"].(map[string]any); ok {
			tw := time.Duration(vpcFloat(sc["timeWindowSec"])) * time.Second
			sizes := append(trimPoints(s.scale.sizes[rel], now, tw), sizePoint{now, cur})
			top := 0
			for _, p := range sizes {
				top = max(top, p.size)
			}
			if mr, ok := sc["maxScaledInReplicas"].(map[string]any); ok {
				var allowed int
				if f, ok := mr["fixed"].(float64); ok {
					allowed = int(f)
					mr["calculated"] = f
				} else {
					allowed = int(math.Floor(vpcFloat(mr["percent"]) * float64(top) / 100))
					mr["calculated"] = float64(allowed)
				}
				final = max(final, top-allowed)
			}
		}
		final = int(math.Max(minR, float64(final)))
	}
	a["recommendedSize"] = float64(final)
	mode := autoscalerMode(a)
	apply := final != cur && (mode == "ON" || ((mode == "ONLY_SCALE_OUT" || mode == "ONLY_UP") && final > cur))
	if apply {
		m["targetSize"] = float64(final)
		if v, ok := listOf(m["versions"])[0].(map[string]any); ok {
			v["targetSize"] = map[string]any{"calculated": float64(final)}
		}
		if err := s.resizeToLocked(strings.Split(mrel, "/")[1], mrel, m); err != nil {
			a["status"] = "ERROR"
			a["statusDetails"] = []any{obj{"type": "UNKNOWN", "message": err.Error()}}
			return false
		}
		cur = final
	}
	s.scale.sizes[rel] = append(trimPoints(s.scale.sizes[rel], now, time.Hour), sizePoint{now, cur})
	s.refreshAutoscalerLocked(a)
	return apply
}

func trimPoints(l []sizePoint, now time.Time, window time.Duration) []sizePoint {
	var out []sizePoint
	for _, p := range l {
		if now.Sub(p.at) <= window {
			out = append(out, p)
		}
	}
	return out
}

func vpcFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// CPUSamples are the running VMs' last measured CPU utilizations,
// compute.googleapis.com/instance/cpu/utilization's points.
func (s *Server) CPUSamples() []CPUSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CPUSample, 0, len(s.scale.util))
	for _, u := range s.scale.util {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name || (out[i].Name == out[j].Name && out[i].Zone < out[j].Zone)
	})
	return out
}
