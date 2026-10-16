package run

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Traffic (#1224): a Cloud Run service's traffic block on Knative's.
//
// Knative Serving routes by traffic targets natively, so the mapping is one
// to one: a LATEST target is `latestRevision: true`, a REVISION target is
// `revisionName`, and percent and tag carry over. A tag gets its own URL,
// which Knative publishes as status.traffic[].url and is reported as the
// target's traffic_statuses uri. A pinned revision is named as the API names
// it, and rendered as the Knative revision it is (kRevisionName, #1400).

// ktraffic is one Knative traffic target, in spec or status.
type ktraffic struct {
	RevisionName      string `json:"revisionName,omitempty"`
	ConfigurationName string `json:"configurationName,omitempty"`
	LatestRevision    *bool  `json:"latestRevision,omitempty"`
	Percent           *int64 `json:"percent,omitempty"`
	Tag               string `json:"tag,omitempty"`
	URL               string `json:"url,omitempty"`
}

func (t ktraffic) percent() int32 {
	if t.Percent == nil {
		return 0
	}
	return int32(*t.Percent)
}

func (t ktraffic) latest() bool { return t.LatestRevision != nil && *t.LatestRevision }

// trafficTagRE is a tag Cloud Run accepts and Knative can put in a host
// name: a DNS label, starting with a letter.
var trafficTagRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// trafficRevision is a target's revision as Knative names it: the short
// name, or the last segment of a full revision name.
func trafficRevision(t *runpb.TrafficTarget) string {
	r := t.GetRevision()
	if i := strings.LastIndex(r, "/revisions/"); i >= 0 {
		r = r[i+len("/revisions/"):]
	}
	return r
}

// trafficIsLatest reports whether a target follows the latest revision:
// LATEST, or no type and no revision.
func trafficIsLatest(t *runpb.TrafficTarget) bool {
	switch t.GetType() {
	case runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST:
		return true
	case runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION:
		return false
	}
	return t.GetRevision() == ""
}

// validateTraffic checks a service's traffic before anything is applied:
// every percentage in 0..100 and summing to 100, a REVISION target naming its
// revision, a LATEST one naming none, and tags valid and distinct.
func validateTraffic(traffic []*runpb.TrafficTarget) error {
	if len(traffic) == 0 {
		return nil
	}
	sum := int32(0)
	tags := map[string]bool{}
	for i, t := range traffic {
		field := fmt.Sprintf("traffic[%d]", i)
		if t.GetPercent() < 0 || t.GetPercent() > 100 {
			return apierror.InvalidArgument("%s.percent is %d; it must be between 0 and 100", field, t.GetPercent())
		}
		sum += t.GetPercent()
		latest := trafficIsLatest(t)
		if !latest && trafficRevision(t) == "" {
			return apierror.InvalidArgument("%s has type REVISION but names no revision", field)
		}
		if latest && t.GetRevision() != "" {
			return apierror.InvalidArgument("%s has type LATEST and names revision %q; set one or the other", field, t.GetRevision())
		}
		if tag := t.GetTag(); tag != "" {
			if !trafficTagRE.MatchString(tag) {
				return apierror.InvalidArgument("%s.tag %q is not valid: use lowercase letters, digits and hyphens, "+
					"starting with a letter and at most 63 characters", field, tag)
			}
			if tags[tag] {
				return apierror.InvalidArgument("%s.tag %q is used by more than one traffic target", field, tag)
			}
			tags[tag] = true
		}
		if t.GetPercent() == 0 && t.GetTag() == "" {
			return apierror.InvalidArgument("%s sends 0%% of traffic and has no tag, so it routes nothing", field)
		}
	}
	if sum != 100 {
		return apierror.InvalidArgument("traffic percentages sum to %d; they must sum to 100", sum)
	}
	return nil
}

// checkTrafficRevisions refuses a target pinned to a revision the service,
// name, whose Knative Service is kname, does not have: Knative would accept
// it and then leave the route unready.
func (s *Server) checkTrafficRevisions(ctx context.Context, name, kname string, traffic []*runpb.TrafficTarget) error {
	for i, t := range traffic {
		if trafficIsLatest(t) {
			continue
		}
		r := trafficRevision(t)
		if _, err := s.kn.Revision(ctx, kname, kRevisionName(name, kname, r)); err != nil {
			return apierror.InvalidArgument("traffic[%d] names revision %s, which service %s does not have", i, r, name)
		}
	}
	return nil
}

// renderTraffic writes a Knative Service's spec.traffic. No targets means
// Knative's default, all traffic to the latest revision, which is Cloud Run's.
func renderTraffic(b *strings.Builder, traffic []*runpb.TrafficTarget, krev func(string) string) {
	if len(traffic) == 0 {
		return
	}
	b.WriteString("  traffic:\n")
	for _, t := range traffic {
		if trafficIsLatest(t) {
			b.WriteString("    - latestRevision: true\n")
		} else {
			fmt.Fprintf(b, "    - latestRevision: false\n      revisionName: %q\n", krev(trafficRevision(t)))
		}
		fmt.Fprintf(b, "      percent: %d\n", t.GetPercent())
		if t.GetTag() != "" {
			fmt.Fprintf(b, "      tag: %q\n", t.GetTag())
		}
	}
}

// fromKnativeTraffic reads a Knative Service's traffic back as Cloud Run's:
// what was asked for (spec) and where requests go (status).
func fromKnativeTraffic(k ksvc, rev func(string) string) ([]*runpb.TrafficTarget, []*runpb.TrafficTargetStatus) {
	var targets []*runpb.TrafficTarget
	for _, t := range k.Spec.Traffic {
		out := &runpb.TrafficTarget{Percent: t.percent(), Tag: t.Tag}
		if t.latest() || (t.RevisionName == "" && t.LatestRevision == nil) {
			out.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
		} else {
			out.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
			out.Revision = rev(t.RevisionName)
		}
		targets = append(targets, out)
	}
	if len(targets) == 0 {
		// Knative's default, and Cloud Run's: everything to the latest.
		targets = []*runpb.TrafficTarget{{
			Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST, Percent: 100}}
	}
	var statuses []*runpb.TrafficTargetStatus
	for _, t := range k.Status.Traffic {
		st := &runpb.TrafficTargetStatus{Revision: rev(t.RevisionName), Percent: t.percent(), Tag: t.Tag}
		st.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
		if t.latest() {
			st.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
		}
		if t.Tag != "" {
			st.Uri = t.URL
		}
		statuses = append(statuses, st)
	}
	return targets, statuses
}
