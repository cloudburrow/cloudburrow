package main

import (
	"strings"
	"testing"
)

// gcloud kms is pointed at the local JSON API only when kms is enabled and
// its port is known (#426).
func TestGcloudSetupWritesTheCloudKMSOverride(t *testing.T) {
	cfg := formatsConfig(t, "storage,kms")
	if got := gcloudConfiguration(cfg, "/adc.json"); !strings.Contains(got, "cloudkms = http://127.0.0.1:9018/\n") {
		t.Errorf("kms enabled, but no cloudkms override:\n%s", got)
	}
	if got := gcloudConfiguration(formatsConfig(t, "storage"), "/adc.json"); strings.Contains(got, "cloudkms") {
		t.Errorf("kms disabled, but a cloudkms line:\n%s", got)
	}
	cfg.Endpoints.KMS = 0
	if got := gcloudConfiguration(cfg, "/adc.json"); strings.Contains(got, "cloudkms") {
		t.Errorf("kms on an OS-assigned port, but a cloudkms line:\n%s", got)
	}
}

func TestEnvExportsTheGcloudKMSOverrideOnlyWhenEnabled(t *testing.T) {
	find := func(services string, port int) (string, bool) {
		cfg := formatsConfig(t, services)
		if port >= 0 {
			cfg.Endpoints.KMS = port
		}
		for _, v := range envVars(cfg, cfg.DefaultProject(), "/adc.json") {
			if v.Name == "CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDKMS" {
				return v.Value, true
			}
		}
		return "", false
	}
	if v, ok := find("storage", -1); ok {
		t.Errorf("kms disabled, but the override is %q", v)
	}
	if v, ok := find("storage,kms", 0); ok {
		t.Errorf("kms on port 0, but the override is %q", v)
	}
	v, ok := find("storage,kms", -1)
	if !ok || v != "http://127.0.0.1:9018/" || strings.Contains(v, "googleapis.com") {
		t.Errorf("kms enabled: override = %q, %v; want http://127.0.0.1:9018/", v, ok)
	}
}
