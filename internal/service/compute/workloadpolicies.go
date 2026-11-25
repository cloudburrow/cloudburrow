package compute

// Workload policies on managed instance groups (#2206), as Google
// documents them (read 2026-11-24):
// https://docs.cloud.google.com/compute/docs/instance-groups/about-workload-policies,
// .../create-workload-policies, and the instanceGroupManagers and
// resourcePolicies references.
//
//   - A group names one in resourcePolicies.workloadPolicy (a full or
//     partial URL) on insert or patch. The policy is in the group's region.
//   - A regional group takes one only with the ANY or ANY_SINGLE_ZONE
//     target distribution shape.
//   - "You can only apply a workload policy to an existing MIG, or change
//     its workload policy, ... if there are no compute instances in the
//     MIG"; a patch can't clear it (Google's own refusal text).
//   - "You can't configure a second instance template if your MIG uses a
//     workload policy", and "You can't use workload policies together with
//     placement policies".
//   - Its VMs are of the series the overview lists (A4X Max, A4X, A4, A3
//     Ultra, A3 Mega, A3 High, H4D, TPU7x, TPU v6e, TPU v5p). None of them
//     is served here: their GPUs, TPUs and (H4D) 192 vCPUs aren't on this
//     machine, so such a group is kept at zero VMs, and a size above zero
//     is refused with that reason.
//   - A policy a group uses can't be deleted (resourceInUseByAnotherResource).

import (
	"strings"
)

// workloadSeries are the machine series (a machine type name's first
// part) that workload policies support.
var workloadSeries = map[string]bool{"a4x": true, "a4": true, "a3": true, "h4d": true, "tpu7x": true, "ct6e": true, "ct5p": true}

const wpField = "resource.resourcePolicies.workloadPolicy"

// workloadPolicyOf is the workload policy a group uses, or "".
func workloadPolicyOf(m obj) string {
	rp, _ := m["resourcePolicies"].(map[string]any)
	return str(rp, "workloadPolicy")
}

// managerPolicyUsersLocked are the groups that use the workload policy rel.
func (s *Server) managerPolicyUsersLocked(rel string) []string {
	var out []string
	for k, o := range s.res {
		if strings.Contains(k, "/instanceGroupManagers/") && workloadPolicyOf(o) == rel {
			out = append(out, k)
		}
	}
	return out
}

// checkManagerWorkloadLocked is the workload policy b's resourcePolicies
// name for a group at l: its relative name, whether b sets it, or the
// refusal. m is the group (as it will be, on insert), patch whether b
// patches it, live its VMs.
func (s *Server) checkManagerWorkloadLocked(l igLoc, b, m obj, patch bool, live int) (string, bool, error) {
	raw, present := b["resourcePolicies"]
	if !present || raw == nil {
		return "", false, nil
	}
	rp, _ := raw.(map[string]any)
	ref := str(rp, "workloadPolicy")
	if ref == "" {
		if patch {
			return "", false, invalid("Invalid value for field '%s': ''. Resource policies update without workload policy is not allowed.", wpField)
		}
		return "", false, nil
	}
	rel := Rel(ref, l.project)
	if !strings.Contains(rel, "/") {
		rel = regionRel(l.project, l.region, "resourcePolicies", rel)
	}
	po, ok := s.res[rel]
	if !ok || !strings.Contains(rel, "/resourcePolicies/") {
		return "", false, notFound(rel)
	}
	if po["workloadPolicy"] == nil {
		return "", false, invalid("Invalid value for field '%s': '%s'. The resource policy must be a workload policy.", wpField, ref)
	}
	if r := lastSeg(str(po, "region")); r != l.region {
		return "", false, invalid("Invalid value for field '%s': '%s'. The workload policy must be in the managed instance group's region, %s.", wpField, ref, l.region)
	}
	if l.regional() {
		if sh := targetShapeOf(m); sh != "ANY" && sh != "ANY_SINGLE_ZONE" {
			return "", false, invalid("Invalid value for field '%s': '%s'. A regional managed instance group takes a workload policy only with the ANY or ANY_SINGLE_ZONE target distribution shape.", wpField, ref)
		}
	}
	if patch && rel != workloadPolicyOf(m) && live > 0 {
		return "", false, invalid("Workload policy update is not allowed when the MIG has instances.")
	}
	return rel, true, nil
}

// checkWorkloadTemplatesLocked applies a workload policy's rules to a
// group's versions (instance template relative names) and target size.
func (s *Server) checkWorkloadTemplatesLocked(project, region string, templates []string, size int) error {
	if len(templates) > 1 {
		return invalid("Invalid value for field 'resource.versions': a managed instance group that uses a workload policy can't have a second instance template.")
	}
	for _, t := range templates {
		props, _ := s.res[t]["properties"].(map[string]any)
		for _, p := range strs(props, "resourcePolicies") {
			rel := Rel(p, project)
			if !strings.Contains(rel, "/") {
				rel = regionRel(project, region, "resourcePolicies", rel)
			}
			if po, ok := s.res[rel]; ok && po["groupPlacementPolicy"] != nil {
				return invalid("Invalid value for field 'resource.instanceTemplate': '%s'. Workload policies can't be used together with placement policies: the instance template has the placement policy '%s'.", t, rel)
			}
		}
		if size == 0 {
			continue
		}
		mt := lastSeg(str(props, "machineType"))
		series, _, _ := strings.Cut(mt, "-")
		if !workloadSeries[series] {
			return invalid("Invalid value for field 'resource.instanceTemplate': '%s'. Workload policies support the A4X Max, A4X, A4, A3 Ultra, A3 Mega, A3 High and H4D machine series and TPU7x, TPU v6e and TPU v5p, not the machine type %s.", t, mt)
		}
		return unimplemented("a managed instance group with a workload policy is kept at 0 VMs here: the machine series workload policies support (%s's %s) need GPUs, TPUs or (H4D) 192 vCPUs this machine doesn't have", t, mt)
	}
	return nil
}

// managerTemplates are the instance templates of a group's versions.
func managerTemplates(m obj) []string {
	var out []string
	for _, v := range versionsOf(m) {
		if t := str(v, "instanceTemplate"); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 && str(m, "instanceTemplate") != "" {
		out = []string{str(m, "instanceTemplate")}
	}
	return out
}
