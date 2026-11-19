package main

import (
	"context"
	"strings"
	"testing"
)

// Create sink's Preview logs opens the Logs Explorer on the inclusion
// filter; Troubleshoot sink names what is wrong with the form, or that
// nothing is (#1863).
func TestCreateSinkPreviewAndTroubleshoot(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	p := loggingSinksProvider{loggingAdminScreen{svc: svc}}
	href, err := p.CheckCreate(ctx, "proj-1", sinkCheckPreview, map[string]string{"filter": "severity>=ERROR"})
	if err != nil || href != "/logs?project=proj-1&query=severity%3E%3DERROR" {
		t.Errorf("Preview logs = %q, %v", href, err)
	}
	good := map[string]string{"service": "Cloud Logging bucket", "destination": "projects/proj-1/locations/global/buckets/_Default",
		"filter": "severity>=ERROR", "exclusions": `[{"name":"no-debug","filter":"severity=DEBUG","disabled":"true"}]`}
	if msg, err := p.CheckCreate(ctx, "proj-1", sinkCheckTroubleshoot, good); err != nil || !strings.HasPrefix(msg, "No problems found") {
		t.Errorf("a good sink = %q, %v", msg, err)
	}
	for name, bad := range map[string]map[string]string{
		"absent bucket": {"service": "Cloud Logging bucket", "destination": "projects/proj-1/locations/global/buckets/nope"},
		"bad filter":    {"service": "Pub/Sub topic", "destination": "projects/proj-1/topics/t", "filter": "severity>="},
		"twice": {"service": "Pub/Sub topic", "destination": "projects/proj-1/topics/t",
			"exclusions": `[{"name":"a","filter":"severity=DEBUG"},{"name":"a","filter":"severity=INFO"}]`},
	} {
		if msg, err := p.CheckCreate(ctx, "proj-1", sinkCheckTroubleshoot, bad); err == nil {
			t.Errorf("%s: Troubleshoot sink = %q; want a problem", name, msg)
		}
	}
}
