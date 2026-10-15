package main

// Cloud Run: Manage traffic on a service's page (#1224).
//
// The adapter maps a service's traffic onto Knative's traffic block, so a
// split between revisions, a pinned revision and a tag are all real. The
// console offers it as one action on the service's page, prefilled with the
// traffic as it is, which reads the service back through the API, replaces
// its traffic and sends UpdateService: the rest of the service is kept as
// read, since UpdateService replaces the whole configuration.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
// CloudBurrow created: the API updates no other.
func (p runProvider) trafficAction(svc *ksvcStatus) []console.Action {
	if p.runEndpoint() == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	return []console.Action{{ID: runManageTraffic, Label: "Manage traffic", Fields: []console.Field{{
		Name: runTrafficField, Label: "Traffic targets", Type: "textarea", Required: true,
		Default: trafficLines(svc),
		Help: "One target per line: a revision name, or LATEST for the latest ready revision; the percent of " +
			"requests it receives; and an optional tag, which gives it its own URL. Percentages must sum to 100; " +
			"a tagged target may have 0.",
	}}}}
}

// manageTraffic replaces a service's traffic through UpdateService.
func (p runProvider) manageTraffic(ctx context.Context, project, service string, values map[string]string) error {
	traffic, err := parseTrafficLines(values[runTrafficField])
	if err != nil {
		return err
	}
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
	svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		return err
	}
	svc.Traffic = traffic
	// No etag, for the reason Edit sends none: Knative's own status writes
	// move it while a revision settles.
	svc.Etag = ""
	op, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc})
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
