package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
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

// TestConsoleAPIQuotas (#2063): an API's Quotas & System Limits tab lists
// its request quotas at their defaults; Edit quota sets the project's,
// which the tab then shows adjusted and which refuses the project's calls
// past it; Reset to default restores it.
func TestConsoleAPIQuotas(t *testing.T) {
	cfg := config.Default()
	cfg.Services = append(cfg.Services, config.ServiceParameterManager)
	l, err := newRequestQuotas(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := serviceUsageProvider{requestQuotas: l}
	qs := p.apiQuotas("parametermanager.googleapis.com", "cq")
	if len(qs) != 3 || qs[0].Metric != "parametermanager.googleapis.com/read_requests" || qs[0].Value != 1200 || qs[0].Adjusted {
		t.Fatalf("quotas = %+v", qs)
	}
	if _, err := p.quotaCall("cq", "setquota", []byte(`{"metric":"parametermanager.googleapis.com/read_requests","limit":"1"}`)); err != nil {
		t.Fatal(err)
	}
	if qs := p.apiQuotas("parametermanager.googleapis.com", "cq"); qs[0].Value != 1 || !qs[0].Adjusted {
		t.Errorf("after Edit quota: %+v", qs[0])
	}
	if qs := p.apiQuotas("parametermanager.googleapis.com", "other"); qs[0].Value != 1200 || qs[0].Adjusted {
		t.Errorf("another project: %+v", qs[0])
	}
	_ = l.Charge("parametermanager.googleapis.com/read_requests", "cq")
	if err := l.Charge("parametermanager.googleapis.com/read_requests", "cq"); err == nil {
		t.Error("the project's second read passed a quota of 1")
	}
	if _, err := p.quotaCall("cq", "setquota", []byte(`{"metric":"parametermanager.googleapis.com/read_requests","limit":"many"}`)); err == nil {
		t.Error("a value that is not a number was accepted")
	}
	if _, err := p.quotaCall("cq", "resetquota", []byte(`{"metric":"parametermanager.googleapis.com/read_requests"}`)); err != nil {
		t.Fatal(err)
	}
	if qs := p.apiQuotas("parametermanager.googleapis.com", "cq"); qs[0].Value != 1200 || qs[0].Adjusted {
		t.Errorf("after Reset to default: %+v", qs[0])
	}
}
