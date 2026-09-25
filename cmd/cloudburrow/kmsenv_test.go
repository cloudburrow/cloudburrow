package main

import (
	"strings"
	"testing"
)

// env exports CLOUDBURROW_KMS_ENDPOINT only when kms is enabled (#385), so a
// client pointed at it is never pointed at a port nothing serves.
func TestKMSEndpointIsExportedOnlyWhenEnabled(t *testing.T) {
	find := func(services string) (string, bool) {
		cfg := formatsConfig(t, services)
		for _, v := range envVars(cfg, cfg.DefaultProject(), "/adc.json") {
			if v.Name == "CLOUDBURROW_KMS_ENDPOINT" {
				return v.Value, true
			}
		}
		return "", false
	}
	if v, ok := find("storage,pubsub"); ok {
		t.Errorf("kms disabled, but CLOUDBURROW_KMS_ENDPOINT=%q is exported", v)
	}
	if v, ok := find("storage,kms"); !ok || !strings.HasSuffix(v, ":9018") {
		t.Errorf("kms enabled: CLOUDBURROW_KMS_ENDPOINT = %q, %v; want the default port 9018", v, ok)
	}
}
