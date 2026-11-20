package main

// Create sink's Preview logs and Troubleshoot sink (#1863). Preview logs,
// beside Build inclusion filter on Google's page, opens the Logs Explorer
// on the inclusion filter in a new tab. Troubleshoot sink checks what the
// form holds before anything is made: the sink service and destination
// resolve, a Cloud Logging bucket or project destination exists, the
// inclusion and exclusion filters parse, and each exclusion is named once.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	sinkCheckPreview      = "preview-logs"
	sinkCheckTroubleshoot = "troubleshoot-sink"
)

func (loggingSinksProvider) CreateChecks() []console.CreateCheck {
	return []console.CreateCheck{
		{ID: sinkCheckPreview, Label: "Preview logs", Opens: true},
		{ID: sinkCheckTroubleshoot, Label: "Troubleshoot sink"},
	}
}

func (p loggingSinksProvider) CheckCreate(ctx context.Context, project, check string, values map[string]string) (string, error) {
	switch check {
	case sinkCheckPreview:
		q := url.Values{}
		if project != "" {
			q.Set("project", project)
		}
		if f := strings.TrimSpace(values["filter"]); f != "" {
			q.Set("query", f)
		}
		return "/logs?" + q.Encode(), nil
	case sinkCheckTroubleshoot:
		return p.troubleshoot(ctx, values)
	}
	return "", fmt.Errorf("no check %q", check)
}

func (p loggingSinksProvider) troubleshoot(ctx context.Context, values map[string]string) (string, error) {
	dest, err := sinkDestination(values["service"], values["destination"])
	if err != nil {
		return "", fmt.Errorf("sink destination: %w", err)
	}
	if strings.TrimSpace(values["destination"]) == "" {
		return "", fmt.Errorf("sink destination: none is given")
	}
	rows, err := console.ParseGroup(values["exclusions"])
	if err != nil {
		return "", fmt.Errorf("exclusion filters: %w", err)
	}
	var excl []sinkExclusionCheck
	for _, r := range rows {
		excl = append(excl, sinkExclusionCheck{strings.TrimSpace(r["name"]), r["filter"]})
	}
	ok, err := p.checkSink(ctx, dest, values["filter"], excl)
	if err != nil {
		return "", err
	}
	return "No problems found: " + strings.Join(ok, "; ") + ".", nil
}

type sinkExclusionCheck struct{ name, filter string }

// checkSink is Troubleshoot sink's checks, on the create page's form or a
// sink's row: the destination resolves (a log bucket destination exists),
// the inclusion filter and each exclusion filter parse, and no exclusion
// is named twice. It answers what passed, or the first problem.
func (p loggingSinksProvider) checkSink(ctx context.Context, dest, filter string, excl []sinkExclusionCheck) ([]string, error) {
	var ok []string
	ok = append(ok, "destination "+dest)
	if rest, found := strings.CutPrefix(dest, "logging.googleapis.com/"); found {
		a, err := p.admin()
		if err != nil {
			return nil, err
		}
		if strings.Contains(rest, "/buckets/") {
			if _, err := a.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: rest}); err != nil {
				return nil, fmt.Errorf("sink destination: the log bucket %s: %v", rest, err)
			}
			ok = append(ok, "the log bucket exists")
		}
	} else {
		ok = append(ok, "its existence is checked by the destination's service when entries are routed")
	}
	if f := strings.TrimSpace(filter); f != "" {
		if _, err := console.CompileQuery(f); err != nil {
			return nil, fmt.Errorf("inclusion filter: %v", err)
		}
		ok = append(ok, "the inclusion filter parses")
	} else {
		ok = append(ok, "no inclusion filter: every entry is included")
	}
	seen := map[string]bool{}
	for _, e := range excl {
		if seen[e.name] {
			return nil, fmt.Errorf("exclusion filter %q is named twice", e.name)
		}
		seen[e.name] = true
		if _, err := console.CompileQuery(e.filter); err != nil {
			return nil, fmt.Errorf("exclusion filter %s: %v", e.name, err)
		}
	}
	if len(excl) > 0 {
		ok = append(ok, fmt.Sprintf("%d exclusion filter(s) parse", len(excl)))
	}
	return ok, nil
}

// troubleshootStored is a sink row's Troubleshoot sink: checkSink on the
// stored sink, its findings one row each.
func (p loggingSinksProvider) troubleshootStored(ctx context.Context, project, name string) (*console.Listing, error) {
	sk, err := p.get(ctx, project, name)
	if err != nil {
		return nil, err
	}
	var excl []sinkExclusionCheck
	for _, e := range sk.GetExclusions() {
		excl = append(excl, sinkExclusionCheck{e.GetName(), e.GetFilter()})
	}
	ok, err := p.checkSink(ctx, sk.GetDestination(), sk.GetFilter(), excl)
	if err != nil {
		return nil, err
	}
	l := &console.Listing{Columns: []string{"Check"}, NameColumn: "Result", Noun: "checks"}
	for _, c := range ok {
		l.Items = append(l.Items, console.Resource{Name: "OK", Fields: map[string]string{"Check": c}})
	}
	l.Total = len(l.Items)
	return l, nil
}

var _ console.CreateChecker = loggingSinksProvider{}
