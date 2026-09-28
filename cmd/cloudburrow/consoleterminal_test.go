package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// The terminal waits for `up`'s background import of its image from the
// offline cache, reporting it, rather than racing it with a pull; with no
// import started it does not wait at all, and a failed import is left to
// the kubelet's pull (#824).
func TestTerminalWaitsForTheBackgroundImport(t *testing.T) {
	var none *terminalWarm
	if err := none.wait(context.Background(), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := (&terminalWarm{}).wait(context.Background(), func(string) { t.Error("waited with no import") }); err != nil {
		t.Fatal(err)
	}

	w := &terminalWarm{}
	release := make(chan struct{})
	var out bytes.Buffer
	w.start(context.Background(), func(context.Context) error { <-release; return errors.New("disk full") }, &out)
	reported := make(chan string, 16)
	waited := make(chan error, 1)
	go func() { waited <- w.wait(context.Background(), func(s string) { reported <- s }) }()
	if msg := <-reported; !strings.Contains(msg, "Importing the terminal image from the offline cache") {
		t.Errorf("progress = %q", msg)
	}
	select {
	case err := <-waited:
		t.Fatalf("wait returned %v before the import finished", err)
	default:
	}
	close(release)
	if err := <-waited; err != nil {
		t.Errorf("a failed import failed the terminal: %v", err)
	}
	// The warning is written before the import is marked done.
	if !strings.Contains(out.String(), "disk full") {
		t.Errorf("the failed import was not reported: %q", out.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	w2 := &terminalWarm{}
	w2.start(context.Background(), func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, io.Discard)
	cancel()
	if err := w2.wait(ctx, func(string) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait = %v", err)
	}
}
