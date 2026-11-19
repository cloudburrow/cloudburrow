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
	var ok []string
	dest, err := sinkDestination(values["service"], values["destination"])
	if err != nil {
		return "", fmt.Errorf("sink destination: %w", err)
	}
	if strings.TrimSpace(values["destination"]) == "" {
		return "", fmt.Errorf("sink destination: none is given")
	}
	ok = append(ok, "destination "+dest)
	if rest, found := strings.CutPrefix(dest, "logging.googleapis.com/"); found {
		a, err := p.admin()
		if err != nil {
			return "", err
		}
		if strings.Contains(rest, "/buckets/") {
			if _, err := a.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: rest}); err != nil {
				return "", fmt.Errorf("sink destination: the log bucket %s: %v", rest, err)
			}
			ok = append(ok, "the log bucket exists")
		}
	} else {
		ok = append(ok, "its existence is checked by the destination's service when entries are routed")
	}
	if f := strings.TrimSpace(values["filter"]); f != "" {
		if _, err := console.CompileQuery(f); err != nil {
			return "", fmt.Errorf("inclusion filter: %v", err)
		}
		ok = append(ok, "the inclusion filter parses")
	} else {
		ok = append(ok, "no inclusion filter: every entry is included")
	}
	rows, err := console.ParseGroup(values["exclusions"])
	if err != nil {
		return "", fmt.Errorf("exclusion filters: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		n := strings.TrimSpace(r["name"])
		if seen[n] {
			return "", fmt.Errorf("exclusion filter %q is named twice", n)
		}
		seen[n] = true
		if _, err := console.CompileQuery(r["filter"]); err != nil {
			return "", fmt.Errorf("exclusion filter %s: %v", n, err)
		}
	}
	if len(rows) > 0 {
		ok = append(ok, fmt.Sprintf("%d exclusion filter(s) parse", len(rows)))
	}
	return "No problems found: " + strings.Join(ok, "; ") + ".", nil
}

var _ console.CreateChecker = loggingSinksProvider{}
