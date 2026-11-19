package main

// Cloud Run: Manage traffic on a service's page (#1224).
//
// The adapter maps a service's traffic onto Knative's traffic block, so a
// split between revisions, a pinned revision and a tag are all real. The
// console offers it as one action on the service's page, prefilled with the
// traffic as it is, which sends UpdateService with an update_mask of
// traffic alone (#1260), so the rest of the service is left as it is.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const (
	runManageTraffic = "manage-traffic"
	runTrafficField  = "traffic"
	runTrafficLatest = "LATEST"
)

// trafficLines renders a Knative Service's requested traffic as the form's
// text: one target per line, "REVISION PERCENT [TAG]", LATEST for the latest
// revision. No targets is Knative's default, all to the latest.
func trafficLines(svc *ksvcStatus) string {
	if len(svc.Spec.Traffic) == 0 {
		return runTrafficLatest + " 100"
	}
	var lines []string
	for _, t := range svc.Spec.Traffic {
		rev := t.RevisionName
		if t.LatestRevision != nil && *t.LatestRevision || rev == "" {
			rev = runTrafficLatest
		}
		line := fmt.Sprintf("%s %d", rev, t.Percent)
		if t.Tag != "" {
			line += " " + t.Tag
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// parseTrafficLines reads the form's text back as Cloud Run traffic targets.
// The adapter checks what it means (sums, tags, revisions); this only checks
// that every line has the shape the form asks for.
func parseTrafficLines(text string) ([]*runpb.TrafficTarget, error) {
	var out []*runpb.TrafficTarget
	for i, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 2 || len(f) > 3 {
			return nil, fmt.Errorf("line %d: %q is not REVISION PERCENT [TAG]", i+1, strings.TrimSpace(line))
		}
		pct, err := strconv.Atoi(strings.TrimSuffix(f[1], "%"))
		if err != nil {
			return nil, fmt.Errorf("line %d: percent %q is not a whole number", i+1, f[1])
		}
		t := &runpb.TrafficTarget{Percent: int32(pct)}
		if strings.EqualFold(f[0], runTrafficLatest) {
			t.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
		} else {
			t.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
			t.Revision = f[0]
		}
		if len(f) == 3 {
			t.Tag = f[2]
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("traffic needs at least one target, such as LATEST 100")
	}
	return out, nil
}

// trafficAction is a service's Manage traffic, offered on a service
// CloudBurrow created: the API updates no other. It is Google's per-revision
// dialog (#1852): the latest healthy revision and each of the service's
// revisions, with the percent of requests each receives, ending in Save.
// Tags are kept as they are: Manage revision tags changes them.
func (p runProvider) trafficAction(svc *ksvcStatus, revisions []string) []console.Action {
	if p.runEndpoint() == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	share := trafficShares(svc)
	pct := func(key string) string { return strconv.Itoa(share[key]) }
	fields := []console.Field{{
		Name: runTrafficLatestField, Label: "Latest healthy revision", Type: "text", Default: pct(runTrafficLatest),
		Pattern: `^[0-9]{1,3}$`, Section: "Traffic",
		Help: "Percent of requests the latest ready revision receives, whichever revision that is after a deploy.",
	}}
	for _, rev := range trafficRevisions(svc, revisions) {
		fields = append(fields, console.Field{
			Name: runTrafficRevisionPrefix + rev, Label: rev, Type: "text", Default: pct(rev),
			Pattern: `^[0-9]{1,3}$`, Section: "Traffic",
			Help: "Percent of requests this revision receives, pinned to it.",
		})
	}
	return []console.Action{{ID: runManageTraffic, Label: "Manage traffic", Submit: "Save", Fields: fields}}
}

// runTrafficLatestField and runTrafficRevisionPrefix name Manage traffic's
// fields: the latest healthy revision's percent, and each revision's.
const (
	runTrafficLatestField    = "traffic-latest"
	runTrafficRevisionPrefix = "traffic-revision-"
)

// trafficRevisions are the revisions the dialog lists: the service's, then
// any its traffic names that the listing did not (so none is dropped).
func trafficRevisions(svc *ksvcStatus, revisions []string) []string {
	out := append([]string(nil), revisions...)
	for _, t := range svc.Spec.Traffic {
		if t.RevisionName != "" && !(t.LatestRevision != nil && *t.LatestRevision) && !slices.Contains(out, t.RevisionName) {
			out = append(out, t.RevisionName)
		}
	}
	return out
}

// trafficShares is each target's percent, LATEST for the latest revision.
func trafficShares(svc *ksvcStatus) map[string]int {
	out := map[string]int{}
	targets, err := parseTrafficLines(trafficLines(svc))
	if err != nil {
		return out
	}
	for _, t := range targets {
		out[trafficKey(t)] += int(t.GetPercent())
	}
	return out
}

func trafficKey(t *runpb.TrafficTarget) string {
	if t.GetType() == runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST {
		return runTrafficLatest
	}
	return t.GetRevision()
}

// trafficTags is each target's tags, in the order the service holds them.
func trafficTags(svc *ksvcStatus) map[string][]string {
	out := map[string][]string{}
	targets, _ := parseTrafficLines(trafficLines(svc))
	for _, t := range targets {
		if t.GetTag() != "" {
			out[trafficKey(t)] = append(out[trafficKey(t)], t.GetTag())
		}
	}
	return out
}

// buildTraffic is the targets for a percent and the tags of each key, in
// order: one target per key with a share or a tag, its first tag on it, and
// a 0% target for each further tag.
func buildTraffic(order []string, share map[string]int, tags map[string][]string) []*runpb.TrafficTarget {
	var out []*runpb.TrafficTarget
	for _, key := range order {
		pct, ts := share[key], tags[key]
		if pct == 0 && len(ts) == 0 {
			continue
		}
		first := ""
		if len(ts) > 0 {
			first, ts = ts[0], ts[1:]
		}
		out = append(out, trafficTarget(key, pct, first))
		for _, tag := range ts {
			out = append(out, trafficTarget(key, 0, tag))
		}
	}
	return out
}

func trafficTarget(key string, pct int, tag string) *runpb.TrafficTarget {
	t := &runpb.TrafficTarget{Percent: int32(pct), Tag: tag,
		Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION, Revision: key}
	if key == runTrafficLatest {
		t.Type, t.Revision = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST, ""
	}
	return t
}

// trafficFromDialog is the targets Manage traffic's fields describe, with
// the service's tags kept.
func trafficFromDialog(svc *ksvcStatus, values map[string]string) ([]*runpb.TrafficTarget, error) {
	share := map[string]int{}
	order := []string{runTrafficLatest}
	read := func(key, field, label string) error {
		v := strings.TrimSuffix(strings.TrimSpace(values[field]), "%")
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return fmt.Errorf("%s: %q is not a whole percent from 0 to 100", label, values[field])
		}
		share[key] = n
		return nil
	}
	if err := read(runTrafficLatest, runTrafficLatestField, "Latest healthy revision"); err != nil {
		return nil, err
	}
	for field := range values {
		if rev, ok := strings.CutPrefix(field, runTrafficRevisionPrefix); ok && rev != "" {
			order = append(order, rev)
		}
	}
	slices.Sort(order[1:])
	slices.Reverse(order[1:])
	for _, rev := range order[1:] {
		if err := read(rev, runTrafficRevisionPrefix+rev, rev); err != nil {
			return nil, err
		}
	}
	tags := trafficTags(svc)
	for key := range tags {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	total := 0
	for _, n := range share {
		total += n
	}
	if total != 100 {
		return nil, fmt.Errorf("the revisions' percentages sum to %d; they must sum to 100", total)
	}
	return buildTraffic(order, share, tags), nil
}

// runManageTags is a revision's Manage revision tags (#1852): its tags, one
// per line, each giving the revision its own URL.
const (
	runManageTags = "manage-revision-tags"
	runTagsField  = "tags"
)

func (p runProvider) tagsAction(svc *ksvcStatus, revision string) []console.Action {
	if p.runEndpoint() == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	return []console.Action{{ID: runManageTags, Label: "Manage revision tags", Submit: "Save", Fields: []console.Field{{
		Name: runTagsField, Label: "Revision tags", Type: "textarea",
		Default: strings.Join(trafficTags(svc)[revision], "\n"),
		Help: "One tag per line. A tag gives the revision its own URL (TAG---SERVICE), with no share of the " +
			"service's traffic; remove a line to delete its tag. Lowercase letters, numbers and hyphens, " +
			"starting with a letter.",
	}}}}
}

var runTagPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,44}[a-z0-9])?$`)

// trafficWithTags is the service's traffic with revision's tags replaced.
func trafficWithTags(svc *ksvcStatus, revision, text string) ([]*runpb.TrafficTarget, error) {
	var tags []string
	for _, line := range strings.Split(text, "\n") {
		tag := strings.TrimSpace(line)
		if tag == "" {
			continue
		}
		if !runTagPattern.MatchString(tag) {
			return nil, fmt.Errorf("tag %q: use lowercase letters, numbers and hyphens, starting with a letter", tag)
		}
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	targets, err := parseTrafficLines(trafficLines(svc))
	if err != nil {
		return nil, err
	}
	share, all := map[string]int{}, trafficTags(svc)
	var order []string
	for _, t := range targets {
		key := trafficKey(t)
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
		share[key] += int(t.GetPercent())
	}
	for key, ts := range all {
		if key == revision {
			continue
		}
		for _, tag := range tags {
			if slices.Contains(ts, tag) {
				return nil, fmt.Errorf("tag %q is already on %s: remove it there first", tag, strings.ToLower(key))
			}
		}
	}
	all[revision] = tags
	if !slices.Contains(order, revision) {
		order = append(order, revision)
	}
	return buildTraffic(order, share, all), nil
}

// manageTraffic replaces a service's traffic through UpdateService.
// The traffic is the text field's when it is sent (the API's own shape, one
// target per line), else the dialog's percentages and the service's tags.
func (p runProvider) manageTraffic(ctx context.Context, project, service string, values map[string]string) error {
	var traffic []*runpb.TrafficTarget
	var err error
	if text, ok := values[runTrafficField]; ok {
		traffic, err = parseTrafficLines(text)
	} else {
		var svc *ksvcStatus
		if svc, err = p.service(ctx, project, service); err == nil && svc == nil {
			err = fmt.Errorf("service %s not found", service)
		}
		if err == nil {
			traffic, err = trafficFromDialog(svc, values)
		}
	}
	if err != nil {
		return err
	}
	return p.sendTraffic(ctx, project, service, traffic)
}

// manageTags replaces one revision's tags through UpdateService.
func (p runProvider) manageTags(ctx context.Context, project, service, revision string, values map[string]string) error {
	svc, err := p.service(ctx, project, service)
	if err == nil && svc == nil {
		err = fmt.Errorf("service %s not found", service)
	}
	if err != nil {
		return err
	}
	traffic, err := trafficWithTags(svc, revision, values[runTagsField])
	if err != nil {
		return err
	}
	return p.sendTraffic(ctx, project, service, traffic)
}

// sendTraffic replaces a service's traffic, and nothing else.
func (p runProvider) sendTraffic(ctx context.Context, project, service string, traffic []*runpb.TrafficTarget) error {
	if project == "" {
		project = p.defaultProject
	}
	if project == "" {
		return errors.New("choose a project before changing a service's traffic")
	}
	c, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	name := service
	if !strings.HasPrefix(name, "projects/") {
		name = fmt.Sprintf("projects/%s/locations/%s/services/%s", project, p.location(), service)
	}
	// Only traffic is sent, and the mask names it: the adapter keeps every
	// other field as it is. No etag, for the reason Edit sends none:
	// Knative's own status writes move it while a revision settles.
	op, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{
		Service:    &runpb.Service{Name: name, Traffic: traffic},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic"}},
	})
	if err != nil {
		return err
	}
	if _, err := op.Wait(ctx); err != nil {
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
			return status.Errorf(st.Code(), "the traffic change did not become ready: %s", st.Message())
		}
		return fmt.Errorf("the traffic change did not become ready: %w", err)
	}
	return nil
}
