package main

import (
	"strings"
	"testing"
)

// The terminal pod's gcloud is gcloud-setup's configuration at the
// addresses a pod reaches (#781): every verified override whose service
// has an in-cluster address, with the same path gcloud-setup writes, and
// credentials disabled. A host address is left out, since a pod cannot
// reach it, and no credential of any kind is included.
func TestTerminalEnvIsGcloudSetupAtInClusterAddresses(t *testing.T) {
	t.Parallel()
	addrs := map[string]string{
		"storage":         "storage.cloudburrow.svc.cluster.local:4443",
		"pubsub":          "pubsub.cloudburrow.svc.cluster.local:8085",
		"kms":             "cloudburrow-host.cloudburrow.svc.cluster.local:9005",
		"secretmanager":   "cloudburrow-host.cloudburrow.svc.cluster.local:9004",
		"tasks":           "cloudburrow-host.cloudburrow.svc.cluster.local:9003",
		"scheduler":       "cloudburrow-host.cloudburrow.svc.cluster.local:9006",
		"logging":         "cloudburrow-host.cloudburrow.svc.cluster.local:9007",
		"resourcemanager": "cloudburrow-host.cloudburrow.svc.cluster.local:9008",
		"metadata":        "cloudburrow-host.cloudburrow.svc.cluster.local:9009",
	}
	env := terminalEnv(addrs)
	for name, want := range map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE":              "http://storage.cloudburrow.svc.cluster.local:4443/storage/v1/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB":               "http://pubsub.cloudburrow.svc.cluster.local:8085/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDKMS":             "http://cloudburrow-host.cloudburrow.svc.cluster.local:9005/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER": "http://cloudburrow-host.cloudburrow.svc.cluster.local:9008/",
		"CLOUDSDK_AUTH_DISABLE_CREDENTIALS":                    "true",
		"STORAGE_EMULATOR_HOST":                                "http://storage.cloudburrow.svc.cluster.local:4443",
		"PUBSUB_EMULATOR_HOST":                                 "pubsub.cloudburrow.svc.cluster.local:8085",
		"GCE_METADATA_HOST":                                    "cloudburrow-host.cloudburrow.svc.cluster.local:9009",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	overrides := 0
	for name, value := range env {
		if strings.HasPrefix(name, "CLOUDSDK_API_ENDPOINT_OVERRIDES_") {
			overrides++
		}
		if strings.Contains(name, "CREDENTIALS") && name != "CLOUDSDK_AUTH_DISABLE_CREDENTIALS" {
			t.Errorf("the terminal is given %s=%s", name, value)
		}
		if strings.Contains(value, "127.0.0.1") || strings.Contains(value, "localhost") {
			t.Errorf("%s=%s is a host address a pod cannot reach", name, value)
		}
	}
	if overrides != len(gcloudVerified) {
		t.Errorf("%d overrides; gcloud-setup writes %d", overrides, len(gcloudVerified))
	}

	// Without cloudburrow-host (no Cloud Run), only what is in the cluster.
	hostOnly := terminalEnv(map[string]string{
		"storage": "storage.cloudburrow.svc.cluster.local:4443",
		"kms":     "127.0.0.1:9005",
	})
	if _, ok := hostOnly["CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDKMS"]; ok {
		t.Error("a host address was given to the pod as an override")
	}
	if hostOnly["CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE"] == "" {
		t.Error("storage lost its override")
	}
}
