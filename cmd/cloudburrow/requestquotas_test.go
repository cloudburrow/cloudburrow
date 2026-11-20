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
