package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// Cloud Scheduler is wired for Terraform (#591): its JSON API is served on
// its gRPC port, and TestTerraformSchedulerAndSubscription applies, updates
// and destroys a google_cloud_scheduler_job through it. Cloud Logging's JSON
// API is served too, but no provider resource uses the part of it that is,
// so the wrapper sets no logging endpoint and names logging as left out.
func TestTerraformSetsSchedulerAndLeavesLoggingOut(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceScheduler, config.ServiceLogging}
	body, skipped := terraformProvider(cfg, false)
	if slices.Contains(skipped, config.ServiceScheduler) {
		t.Errorf("scheduler is named as left out: %v", skipped)
	}
	if !strings.Contains(body, `cloud_scheduler_custom_endpoint = "http://127.0.0.1:9008/v1/"`) {
		t.Errorf("cloud_scheduler_custom_endpoint is not the local endpoint:\n%s", body)
	}
	if !slices.Contains(skipped, config.ServiceLogging) || strings.Contains(body, "logging_custom_endpoint") {
		t.Errorf("logging is not left out (skipped %v):\n%s", skipped, body)
	}
}

func TestEnvTerraformExportsScheduler(t *testing.T) {
	var out bytes.Buffer
	writeTerraformEnv(&out, formatsConfig(t, "scheduler,logging"))
	got := out.String()
	if !strings.Contains(got, `GOOGLE_CLOUD_SCHEDULER_CUSTOM_ENDPOINT="http://127.0.0.1:9008/v1/"`) {
		t.Errorf("GOOGLE_CLOUD_SCHEDULER_CUSTOM_ENDPOINT is not the local endpoint:\n%s", got)
	}
	if strings.Contains(got, "GOOGLE_LOGGING_CUSTOM_ENDPOINT") || !strings.Contains(got, "# logging: no Verified Terraform support") {
		t.Errorf("logging is not reported as left out:\n%s", got)
	}
}

// gcloud scheduler and gcloud logging reach the JSON APIs through the
// overrides env exports and gcloud-setup writes (TestGcloudScheduler,
// TestGcloudLogging); for an instance without the services, neither is set.
func TestEnvExportsSchedulerAndLoggingGcloudOverrides(t *testing.T) {
	names := func(services string) string {
		cfg := formatsConfig(t, services)
		var b strings.Builder
		for _, v := range envVars(cfg, cfg.DefaultProject(), "/adc.json") {
			b.WriteString(v.Name + "=" + v.Value + "\n")
		}
		return b.String()
	}
	got := names("scheduler,logging")
	for _, want := range []string{"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDSCHEDULER=http://127.0.0.1:9008/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_LOGGING=http://127.0.0.1:9009/"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("env lacks %s:\n%s", want, got)
		}
	}
	if got := names("tasks"); strings.Contains(got, "CLOUDSCHEDULER") || strings.Contains(got, "OVERRIDES_LOGGING") {
		t.Errorf("an instance without scheduler and logging exports their overrides:\n%s", got)
	}
	cfg := formatsConfig(t, "scheduler,logging")
	conf := gcloudConfiguration(cfg, "/adc.json")
	for _, want := range []string{"cloudscheduler = http://127.0.0.1:9008/", "logging = http://127.0.0.1:9009/"} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("gcloud-setup's configuration lacks %q:\n%s", want, conf)
		}
	}
}
