package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Service-level scaling (#1790) and the revision-level utilization targets.
//
// runpb's ServiceScaling (cloud.google.com/go/run v1.25.0):
//
//   - minInstanceCount, maxInstanceCount: "total min [max] instances for the
//     service. This number of instances is divided among all revisions with
//     specified traffic based on the percent of traffic they are receiving."
//   - scalingMode AUTOMATIC or MANUAL, and manualInstanceCount: "total
//     instance count for the service in manual scaling mode", divided the
//     same way.
//
// Google's docs (read 2026-11-18,
// https://docs.cloud.google.com/run/docs/configuring/min-instances,
// .../max-instances, .../services/manual-scaling) say how the two levels
// combine, which is what effectiveScale computes:
//
//   - "Maximum limits always cap minimum limits"; of two maximums the lower
//     wins; of two minimums "the autoscaler satisfies the highest";
//   - service-level counts are split by traffic percent; "the first-listed
//     revision in the traffic section gets the rounded-up value", the total
//     still matching the service's count;
//   - a revision serving no traffic (tag-only) takes no share of a service's
//     minimum or maximum; under MANUAL it "scales out to at most one
//     instance";
//   - under MANUAL, "any revision-level minimum and maximum instance
//     settings are ignored"; changing the count "does not create a new
//     revision", and neither does a service-level change here: the counts
//     live on the Knative Service's metadata and on each Knative Revision's
//     autoscaling annotations, never on the template.
//
// Knative's KPA reads autoscaling.knative.dev/min-scale and max-scale from
// each Revision, and syncs them onto its PodAutoscaler when they change, so
// patching a Revision rescales it in place (applyServiceScaling).

// annServiceScaling is a service's ServiceScaling, as protojson, on the
// Knative Service; annRevisionScaling is a template's own RevisionScaling,
// on the template (and so on every revision cut from it), kept apart from
// the min-scale/max-scale the service-level split writes over.
const (
	annServiceScaling  = "cloudburrow.dev/service-scaling"
	annRevisionScaling = "cloudburrow.dev/revision-scaling"

	annMinScale = "autoscaling.knative.dev/min-scale"
	annMaxScale = "autoscaling.knative.dev/max-scale"
	// annTargetUtilization is KPA's concurrency target as a percentage of
	// containerConcurrency: Cloud Run's concurrencyUtilization.
	annTargetUtilization = "autoscaling.knative.dev/target-utilization-percentage"
)

// checkServiceScaling enforces ServiceScaling's limits: counts are never
// negative, and a maximum, when set, is at least 1 ("an integer from 1 up to
// the maximum limit", max-instances). A minimum above the maximum is
// accepted, as the docs say the maximum caps it.
func checkServiceScaling(s *runpb.ServiceScaling) error {
	if s == nil {
		return nil
	}
	switch {
	case s.GetMinInstanceCount() < 0:
		return apierror.InvalidArgument("scaling.minInstanceCount must not be negative")
	case s.GetMaxInstanceCount() < 0:
		return apierror.InvalidArgument("scaling.maxInstanceCount must not be negative")
	case s.GetManualInstanceCount() < 0:
		return apierror.InvalidArgument("scaling.manualInstanceCount must not be negative")
	case s.GetScalingMode() != runpb.ServiceScaling_MANUAL && s.GetManualInstanceCount() > 0:
		return apierror.InvalidArgument("scaling.manualInstanceCount applies only when scaling.scalingMode is MANUAL")
	}
	if _, ok := runpb.ServiceScaling_ScalingMode_name[int32(s.GetScalingMode())]; !ok {
		return apierror.InvalidArgument("scaling.scalingMode %d is not a scaling mode", s.GetScalingMode())
	}
	return nil
}

// checkRevisionScaling enforces RevisionScaling's documented limits: the
// utilization targets are 0.1-0.90 (CPU) and 0.1-0.95 (concurrency), or
// 0.0 to disable, and "CPU and concurrency scaling cannot both be
// disabled"; counts are never negative.
func checkRevisionScaling(s *runpb.RevisionScaling) error {
	if s == nil {
		return nil
	}
	switch {
	case s.GetMinInstanceCount() < 0:
		return apierror.InvalidArgument("template.scaling.minInstanceCount must not be negative")
	case s.GetMaxInstanceCount() < 0:
		return apierror.InvalidArgument("template.scaling.maxInstanceCount must not be negative")
	case s.GetMaxInstanceCount() > 0 && s.GetMinInstanceCount() > s.GetMaxInstanceCount():
		return apierror.InvalidArgument("template.scaling.minInstanceCount (%d) must not exceed template.scaling.maxInstanceCount (%d)",
			s.GetMinInstanceCount(), s.GetMaxInstanceCount())
	}
	if c := s.CpuUtilization; c != nil && *c != 0 && (*c < 0.1 || *c > 0.9) {
		return apierror.InvalidArgument("template.scaling.cpuUtilization must be between 0.1 and 0.90 (inclusive), or 0.0 to disable it; got %v", *c)
	}
	if c := s.ConcurrencyUtilization; c != nil && *c != 0 && (*c < 0.1 || *c > 0.95) {
		return apierror.InvalidArgument("template.scaling.concurrencyUtilization must be between 0.1 and 0.95 (inclusive), or 0.0 to disable it; got %v", *c)
	}
	if s.CpuUtilization != nil && *s.CpuUtilization == 0 && s.ConcurrencyUtilization != nil && *s.ConcurrencyUtilization == 0 {
		return apierror.InvalidArgument("template.scaling: CPU and concurrency scaling cannot both be disabled")
	}
	return nil
}

// serviceScalingSet reports whether s says anything.
func serviceScalingSet(s *runpb.ServiceScaling) bool {
	if s == nil {
		return false
	}
	return s.GetMinInstanceCount() > 0 || s.GetMaxInstanceCount() > 0 || s.ManualInstanceCount != nil ||
		s.GetScalingMode() != runpb.ServiceScaling_SCALING_MODE_UNSPECIFIED
}

// renderRevisionScaling writes a template's scaling annotations: KPA's
// min-scale, max-scale and concurrency target, and the template's own
// values for reading back.
func renderRevisionScaling(b interface{ WriteString(string) (int, error) }, s *runpb.RevisionScaling) {
	if s == nil {
		return
	}
	if s.GetMinInstanceCount() > 0 {
		b.WriteString(fmt.Sprintf("        %s: %q\n", annMinScale, fmt.Sprint(s.GetMinInstanceCount())))
	}
	if s.GetMaxInstanceCount() > 0 {
		b.WriteString(fmt.Sprintf("        %s: %q\n", annMaxScale, fmt.Sprint(s.GetMaxInstanceCount())))
	}
	if c := s.GetConcurrencyUtilization(); c > 0 {
		b.WriteString(fmt.Sprintf("        %s: %q\n", annTargetUtilization, strconv.FormatFloat(float64(c)*100, 'f', -1, 32)))
	}
	raw, _ := protojson.Marshal(s)
	b.WriteString(fmt.Sprintf("        %s: %q\n", annRevisionScaling, string(raw)))
}

// readRevisionScaling reads a template's (or revision's) own scaling back.
func readRevisionScaling(ann map[string]string) *runpb.RevisionScaling {
	if raw := ann[annRevisionScaling]; raw != "" {
		var s runpb.RevisionScaling
		if protojson.Unmarshal([]byte(raw), &s) == nil {
			return &s
		}
	}
	// A revision rendered before #1790 carries only KPA's annotations.
	var s runpb.RevisionScaling
	fmt.Sscan(ann[annMinScale], &s.MinInstanceCount)
	fmt.Sscan(ann[annMaxScale], &s.MaxInstanceCount)
	if s.MinInstanceCount > 0 || s.MaxInstanceCount > 0 {
		return &s
	}
	return nil
}

// readServiceScaling reads a service's scaling from its Knative Service.
func readServiceScaling(ann map[string]string) *runpb.ServiceScaling {
	raw := ann[annServiceScaling]
	if raw == "" {
		return nil
	}
	var s runpb.ServiceScaling
	if protojson.Unmarshal([]byte(raw), &s) != nil {
		return nil
	}
	return &s
}

// revisionShare is one revision's place in the service's resolved traffic.
type revisionShare struct {
	revision string // Knative Revision name
	percent  int32
}

// splitCount divides n among shares by percent: each takes the floor of its
// share, and what rounding leaves goes one each to the revisions in traffic
// order, the first listed first, as the docs' example has it.
func splitCount(n int32, shares []revisionShare) []int32 {
	out := make([]int32, len(shares))
	var given int32
	frac := make([]bool, len(shares))
	for i, s := range shares {
		out[i] = n * s.percent / 100
		frac[i] = n*s.percent%100 != 0
		given += out[i]
	}
	for pass := 0; pass < 2 && given < n; pass++ {
		for i, s := range shares {
			if given >= n {
				break
			}
			if s.percent > 0 && (frac[i] || pass == 1) {
				out[i]++
				given++
				frac[i] = false
			}
		}
	}
	return out
}

// revisionScale is a revision's effective min-scale and max-scale; max 0 is
// Knative's "no limit".
type revisionScale struct{ min, max int32 }

// effectiveScale is each traffic revision's scale under the service's
// scaling, given each revision's own template scaling (own may be nil).
// ok is false when a MANUAL split leaves a serving revision with no
// instances, which Knative cannot express: its smallest maximum is one.
func effectiveScale(svc *runpb.ServiceScaling, shares []revisionShare, own func(string) *runpb.RevisionScaling) (map[string]revisionScale, error) {
	out := map[string]revisionScale{}
	if disabledService(svc) {
		// A disabled service (#1800): every revision with traffic runs no
		// instance; one reached only through a tag is "not disabled" and
		// runs at most one, as under any MANUAL count. Knative's smallest
		// max-scale is 1, so a serving revision gets min 0 and max 1 and
		// the route stops taking requests (applyDisabled), which is what
		// holds it at zero.
		for _, s := range shares {
			if _, seen := out[s.revision]; !seen || s.percent > 0 {
				out[s.revision] = revisionScale{0, 1}
			}
		}
		return out, nil
	}
	if svc.GetScalingMode() == runpb.ServiceScaling_MANUAL {
		counts := splitCount(svc.GetManualInstanceCount(), shares)
		for i, s := range shares {
			if s.percent == 0 {
				if _, seen := out[s.revision]; !seen {
					out[s.revision] = revisionScale{0, 1} // tag-only: at most one
				}
				continue
			}
			if counts[i] == 0 {
				return nil, apierror.Unimplemented("scaling.manualInstanceCount %d leaves revision %s, which serves %d%% of traffic, with no instances; "+
					"a revision with traffic and zero instances (a disabled service) is not supported locally",
					svc.GetManualInstanceCount(), s.revision, s.percent)
			}
			out[s.revision] = revisionScale{counts[i], counts[i]}
		}
		return out, nil
	}
	mins := splitCount(svc.GetMinInstanceCount(), shares)
	maxs := splitCount(svc.GetMaxInstanceCount(), shares)
	for i, s := range shares {
		r := own(s.revision)
		sc := revisionScale{min: r.GetMinInstanceCount(), max: r.GetMaxInstanceCount()}
		if s.percent > 0 {
			if svc.GetMaxInstanceCount() > 0 {
				m := maxs[i]
				if m < 1 {
					m = 1 // Knative's smallest bound; 0 would mean none
				}
				if sc.max == 0 || m < sc.max {
					sc.max = m
				}
			}
			if mins[i] > sc.min {
				sc.min = mins[i]
			}
		}
		if sc.max > 0 && sc.min > sc.max {
			sc.min = sc.max
		}
		if prev, seen := out[s.revision]; seen && s.percent == 0 {
			sc = prev
		}
		out[s.revision] = sc
	}
	return out, nil
}

// trafficShares is k's resolved traffic, one entry per revision in the
// order the traffic lists them, percents summed.
func trafficShares(k ksvc) []revisionShare {
	var out []revisionShare
	idx := map[string]int{}
	for _, t := range k.Status.Traffic {
		if t.RevisionName == "" {
			continue
		}
		if i, ok := idx[t.RevisionName]; ok {
			out[i].percent += t.percent()
			continue
		}
		idx[t.RevisionName] = len(out)
		out = append(out, revisionShare{revision: t.RevisionName, percent: t.percent()})
	}
	return out
}

// applyServiceScaling writes each traffic revision's effective min-scale
// and max-scale onto its Knative Revision, where KPA reads them. A service
// with no service-level scaling restores each revision's own values.
func (s *Server) applyServiceScaling(ctx context.Context, k ksvc) error {
	svc := readServiceScaling(k.Metadata.Annotations)
	revs, err := s.kn.Revisions(ctx, k.Metadata.Name)
	if err != nil {
		return err
	}
	byName := map[string]krev{}
	for _, r := range revs {
		byName[r.Metadata.Name] = r
	}
	own := func(name string) *runpb.RevisionScaling {
		r := byName[name]
		return readRevisionScaling(r.Metadata.Annotations)
	}
	scales, err := effectiveScale(svc, trafficShares(k), own)
	if err != nil {
		return err
	}
	if err := s.applyDisabled(ctx, k, disabledService(svc)); err != nil {
		return err
	}
	for _, r := range revs {
		sc, ok := scales[r.Metadata.Name]
		if !ok {
			// Not in the traffic: the revision's own values.
			o := own(r.Metadata.Name)
			sc = revisionScale{o.GetMinInstanceCount(), o.GetMaxInstanceCount()}
		}
		// The CPU driver's recommendation is a floor under KPA's
		// concurrency one, within the revision's maximum (cpuscaling.go).
		// MANUAL counts are fixed, so it does not apply to them.
		if f := s.cpu.floor(r.Metadata.Name); f > sc.min && svc.GetScalingMode() != runpb.ServiceScaling_MANUAL {
			sc.min = f
			if sc.max > 0 && sc.min > sc.max {
				sc.min = sc.max
			}
		}
		want := map[string]*string{annMinScale: nil, annMaxScale: nil}
		if sc.min > 0 {
			v := strconv.Itoa(int(sc.min))
			want[annMinScale] = &v
		}
		if sc.max > 0 {
			v := strconv.Itoa(int(sc.max))
			want[annMaxScale] = &v
		}
		same := true
		for key, v := range want {
			cur, has := r.Metadata.Annotations[key]
			if (v == nil) == has || (v != nil && *v != cur) {
				same = false
			}
		}
		if same {
			continue
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": want}})
		if err := s.kn.Kube.Patch(ctx, "revision", r.Metadata.Name, string(patch)); err != nil {
			return apierror.Internal(err, "scale revision %s", r.Metadata.Name)
		}
	}
	return nil
}

// checkScaling validates a service's scaling at both levels, and refuses
// what the local cluster cannot do. A CPU threshold is the CPU driver's
// (cpuscaling.go), beside KPA's concurrency one; cpuUtilization 0.0 turns
// the CPU driver off. Disabling the concurrency threshold is Unimplemented:
// KPA always scales on concurrency. A MANUAL split that leaves a serving
// revision without instances is refused before anything is applied.
func checkScaling(svc *runpb.Service) error {
	if err := checkServiceScaling(svc.GetScaling()); err != nil {
		return err
	}
	ts := svc.GetTemplate().GetScaling()
	if err := checkRevisionScaling(ts); err != nil {
		return err
	}
	if ts != nil && ts.ConcurrencyUtilization != nil && ts.GetConcurrencyUtilization() == 0 {
		return apierror.Unimplemented("template.scaling.concurrencyUtilization 0.0: disabling the concurrency threshold is not supported locally; " +
			"Knative's KPA scales on concurrency")
	}
	sc := svc.GetScaling()
	if sc.GetScalingMode() != runpb.ServiceScaling_MANUAL {
		return nil
	}
	var shares []revisionShare
	for _, t := range svc.GetTraffic() {
		shares = append(shares, revisionShare{revision: trafficRevision(t), percent: t.GetPercent()})
	}
	if len(shares) == 0 {
		shares = []revisionShare{{revision: "LATEST", percent: 100}}
	}
	_, err := effectiveScale(sc, shares, func(string) *runpb.RevisionScaling { return nil })
	return err
}

// Cloud Run v1 carries service-level scaling in the service's annotations
// (max-instances, min-instances and manual-scaling docs, "YAML" tabs);
// gcloud's --min, --max and --scaling write them.
const (
	v1ServiceMinScale = "run.googleapis.com/minScale"
	v1ServiceMaxScale = "run.googleapis.com/maxScale"
	v1ScalingMode     = "run.googleapis.com/scalingMode"
)

// fromV1ServiceScaling reads one v1 service-level scaling annotation.
func fromV1ServiceScaling(svc *runpb.Service, key, value string) error {
	if svc.Scaling == nil {
		svc.Scaling = &runpb.ServiceScaling{}
	}
	if key == v1ScalingMode {
		switch value {
		case "automatic", "AUTOMATIC":
			svc.Scaling.ScalingMode = runpb.ServiceScaling_AUTOMATIC
		case "manual", "MANUAL":
			svc.Scaling.ScalingMode = runpb.ServiceScaling_MANUAL
		default:
			return apierror.InvalidArgument("metadata.annotations[%s]: %q is not manual or automatic", key, value)
		}
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return apierror.InvalidArgument("metadata.annotations[%s]: %q is not a count", key, value)
	}
	switch key {
	case v1ServiceMinScale:
		svc.Scaling.MinInstanceCount = int32(n)
	case v1ServiceMaxScale:
		svc.Scaling.MaxInstanceCount = int32(n)
	default:
		c := int32(n)
		svc.Scaling.ManualInstanceCount = &c
	}
	return nil
}

// v1ServiceScalingAnnotations are a service's annotations as v1 reports
// them: the caller's, and its service-level scaling.
func v1ServiceScalingAnnotations(svc *runpb.Service) map[string]string {
	sc := svc.GetScaling()
	if !serviceScalingSet(sc) {
		return svc.GetAnnotations()
	}
	out := map[string]string{}
	for k, v := range svc.GetAnnotations() {
		out[k] = v
	}
	if n := sc.GetMinInstanceCount(); n > 0 {
		out[v1ServiceMinScale] = strconv.Itoa(int(n))
	}
	if n := sc.GetMaxInstanceCount(); n > 0 {
		out[v1ServiceMaxScale] = strconv.Itoa(int(n))
	}
	switch sc.GetScalingMode() {
	case runpb.ServiceScaling_MANUAL:
		out[v1ScalingMode] = "manual"
	case runpb.ServiceScaling_AUTOMATIC:
		out[v1ScalingMode] = "automatic"
	}
	if sc.ManualInstanceCount != nil {
		out[v1ManualInstanceCount] = strconv.Itoa(int(sc.GetManualInstanceCount()))
	}
	return out
}

// ReadServiceScaling is a Knative Service's Cloud Run service-level
// scaling, nil when it has none: the console's Edit form prefills from it.
func ReadServiceScaling(ann map[string]string) *runpb.ServiceScaling { return readServiceScaling(ann) }

// ReadRevisionScaling is a Knative template's (or revision's) own Cloud
// Run scaling, nil when it has none.
func ReadRevisionScaling(ann map[string]string) *runpb.RevisionScaling {
	return readRevisionScaling(ann)
}

// Disabled services (#1800). Google (https://docs.cloud.google.com/run/docs/configuring/services/manual-scaling,
// read 2026-11-18): "Specify a value of 0 to disable the service";
// "requests that are currently being processed will be allowed to
// complete. However, any further requests to the service URL will fail";
// revisions reached only through a tag "are not disabled".
//
// Locally the Knative Service is made cluster-local
// (networking.knative.dev/visibility), so the instance's gateway stops
// routing its URL, its serving revisions get no instances and scale to zero
// once their requests finish; setting a count again makes it public again.
// The URL it had is kept in annDisabledURL, so the service reads back with
// the same uri while disabled.
const (
	labelVisibility     = "networking.knative.dev/visibility"
	visibilityClusterOn = "cluster-local"
	annDisabledURL      = "cloudburrow.dev/disabled-url"
)

// disabledService reports whether s disables the service: MANUAL with a
// manualInstanceCount of 0. An unset count is 0 too: hashicorp/google
// (8.4.0) omits manual_instance_count = 0, Google's own Terraform example
// of a disabled service, from the request.
func disabledService(s *runpb.ServiceScaling) bool {
	return s.GetScalingMode() == runpb.ServiceScaling_MANUAL && s.GetManualInstanceCount() == 0
}

// applyDisabled makes k's route cluster-local when disabled, and public
// again when not; it changes nothing that is already so.
func (s *Server) applyDisabled(ctx context.Context, k ksvc, disabled bool) error {
	_, marked := k.Metadata.Annotations[annDisabledURL]
	if disabled == marked {
		return nil
	}
	var patch map[string]any
	if disabled {
		patch = map[string]any{"metadata": map[string]any{
			"labels":      map[string]any{labelVisibility: visibilityClusterOn},
			"annotations": map[string]any{annDisabledURL: k.Status.URL}}}
	} else {
		patch = map[string]any{"metadata": map[string]any{
			"labels":      map[string]any{labelVisibility: nil},
			"annotations": map[string]any{annDisabledURL: nil}}}
	}
	raw, _ := json.Marshal(patch)
	if err := s.kn.Kube.Patch(ctx, "ksvc", k.Metadata.Name, string(raw)); err != nil {
		return apierror.Internal(err, "disable service %s", k.Metadata.Name)
	}
	return nil
}
