package components

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeKubectl func(args []string) (string, error)

func (f fakeKubectl) Run(_ context.Context, _ string, args ...string) (string, error) { return f(args) }

// The metrics-server pins match dependencies.json, and the verified
// manifest is applied with the pinned image and kind's kubelet flag
// (#1789); bytes that do not match the pin are refused before kubectl.
func TestMetricsServerPinAndManifest(t *testing.T) {
	body := "      containers:\n      - args:\n        - --cert-dir=/tmp\n        - --secure-port=10250\n" +
		"        image: registry.k8s.io/metrics-server/metrics-server:v0.9.1\n"
	y, err := metricsServerYAML([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y, "        - --cert-dir=/tmp\n        - --kubelet-insecure-tls\n        - --secure-port") ||
		!strings.Contains(y, "image: "+MetricsServerImage+"\n") {
		t.Errorf("rendered:\n%s", y)
	}
	if _, err := metricsServerYAML([]byte("image: other:1\n")); err == nil {
		t.Error("a manifest without the pinned image was accepted")
	}
	if !strings.HasPrefix(MetricsServerImage, metricsServerTagImage+"@sha256:") {
		t.Errorf("MetricsServerImage %q is not the release tag pinned by digest", MetricsServerImage)
	}

	var applied bool
	i := &Installer{Kube: fakeKubectl(func(args []string) (string, error) {
		if slices.Contains(args, "apply") {
			applied = true
		}
		return "", errors.New("NotFound")
	}), Fetch: func(context.Context, string) ([]byte, error) { return []byte(body), nil }}
	if err := i.InstallMetricsServer(context.Background(), 0); err == nil || applied {
		t.Errorf("bytes that do not match the pin: err %v, applied %v", err, applied)
	}
}
