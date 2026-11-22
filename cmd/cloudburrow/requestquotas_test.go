package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/quotalimit"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func TestNewRequestQuotasAppliesTheConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Services = append(cfg.Services, config.ServiceParameterManager)
	cfg.RequestQuotas = map[string]int64{"parametermanager.googleapis.com/read_requests": 2}
	l, err := newRequestQuotas(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := l.Charge("parametermanager.googleapis.com/read_requests", "quota-config"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if err := l.Charge("parametermanager.googleapis.com/read_requests", "quota-config"); err == nil || !strings.Contains(err.Error(), "Quota exceeded") {
		t.Fatalf("third call = %v; want the quota refusal", err)
	}
	cfg.RequestQuotas = map[string]int64{"parametermanager.googleapis.com/reads": 2}
	if _, err := newRequestQuotas(cfg); err == nil {
		t.Fatal("an unknown metric was accepted")
	}
}

// TestConsoleAPIQuotas (#2063, #2090): an API's Quotas & System Limits tab
// lists its request quotas, from the Cloud Quotas API, at their defaults
// and enforced; Edit quotas (quota-set, an UpdateQuotaPreference) sets the
// project's, which the tab then shows and which refuses the project's calls
// past it, while another project keeps the default.
func TestConsoleAPIQuotas(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Services = append(cfg.Services, config.ServiceParameterManager)
	l, err := newRequestQuotas(cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := cloudquotas.NewServer(store.NewMemory(), numberedProjects{"cq", "other"}, nil)
	q.AddQuotas(requestQuotaInfos(l))
	quotalimit.Use(q)
	t.Cleanup(func() { quotalimit.Use(nil) })
	p := serviceUsageProvider{quotas: q}
	rows := func(project string) []consoleQuota {
		t.Helper()
		out, err := p.quotasCall(ctx, project, "quotas", []byte(`{"service":"parametermanager.googleapis.com"}`))
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)["quotas"].([]consoleQuota)
	}
	read := func(rs []consoleQuota) consoleQuota {
		for _, r := range rs {
			if r.Metric == "parametermanager.googleapis.com/read_requests" {
				return r
			}
		}
		t.Fatalf("no read quota in %+v", rs)
		return consoleQuota{}
	}
	rs := rows("cq")
	if len(rs) != 3 {
		t.Fatalf("quotas = %+v", rs)
	}
	if r := read(rs); r.QuotaID != "ReadRequestsPerMinutePerProject" || r.Value != 1200 || !r.Enforced || !r.Adjustable || r.Preference != "" {
		t.Fatalf("read quota = %+v", r)
	}
	if _, err := p.quotasCall(ctx, "cq", "quota-set", []byte(`{"service":"parametermanager.googleapis.com","quotaId":"ReadRequestsPerMinutePerProject","value":"1100","justification":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if r := read(rows("cq")); r.Value != 1100 || r.Default != 1200 || r.Granted == nil || *r.Granted != 1100 {
		t.Errorf("after Edit quotas: %+v", r)
	}
	if r := read(rows("other")); r.Value != 1200 || r.Preference != "" {
		t.Errorf("another project: %+v", r)
	}
	for i := 0; i < 1100; i++ {
		if err := l.Charge("parametermanager.googleapis.com/read_requests", "cq"); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	if err := l.Charge("parametermanager.googleapis.com/read_requests", "cq"); err == nil || !strings.Contains(err.Error(), "Quota exceeded") {
		t.Errorf("read 1101 = %v; want the quota refusal", err)
	}
	if _, err := p.quotasCall(ctx, "cq", "quota-set", []byte(`{"service":"parametermanager.googleapis.com","quotaId":"ReadRequestsPerMinutePerProject","value":"many"}`)); err == nil {
		t.Error("a value that is not a number was accepted")
	}
}

// numberedProjects resolves its projects by ID or by number.
type numberedProjects []string

func (ps numberedProjects) Resolve(s string) (string, error) {
	for _, p := range ps {
		if s == p || s == strconv.FormatInt(resource.ProjectNumber(p), 10) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no project %s", s)
}
