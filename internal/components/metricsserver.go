package components

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// metrics-server (#1789, #1800): the cluster's CPU signal.
//
// Cloud Run scales on CPU utilization ("averaging the CPU usage per second
// over a 1-minute period ... divides this by the number of CPUs per
// instance", about-instance-autoscaling, read 2026-11-18). A kind cluster
// has no metrics pipeline, so nothing could read a pod's CPU. Google offers
// no component for this; metrics-server is the Kubernetes SIG's own
// (kubernetes-sigs/metrics-server, Apache-2.0), serving metrics.k8s.io from
// the kubelets' resource metrics. It is installed only when Cloud Run is on.
//
// The release manifest is pinned by its sha256 (dependencies.json
// serving.metricsServer), and its image is rewritten from the release tag to
// the tag and digest pinned beside it, so neither bytes nor image can change
// under the pin. kind's kubelets serve self-signed certificates that do not
// name their node IPs, so metrics-server is given --kubelet-insecure-tls, as
// metrics-server's own README says to for kind; it reaches only the
// cluster's own kubelets.
const (
	MetricsServerVersion = "v0.9.1"
	// MetricsServerImage is the release's image by tag and the digest of
	// its multi-platform index.
	MetricsServerImage = "registry.k8s.io/metrics-server/metrics-server:v0.9.1@sha256:9b0ba1926e1d3089673f45461c3c1416109dea33b3ba3434048defd380df329b"

	metricsServerTagImage = "registry.k8s.io/metrics-server/metrics-server:" + MetricsServerVersion
	metricsServerCertArg  = "- --cert-dir=/tmp"
	metricsServerTLSArg   = "- --kubelet-insecure-tls"
)

// MetricsServerManifest is metrics-server's release components.yaml.
func MetricsServerManifest() Manifest {
	return Manifest{"metrics-server-components.yaml",
		"https://github.com/kubernetes-sigs/metrics-server/releases/download/" + MetricsServerVersion + "/components.yaml",
		"3c7f57cdbee0f57fb0259a150e8f6d6cb4f29d72d1aa5dce8803151dd6d4c2a1"}
}

// metricsServerYAML is the verified release manifest with the pinned image
// and the kind kubelet flag.
func metricsServerYAML(body []byte) (string, error) {
	y := string(body)
	if strings.Count(y, "image: "+metricsServerTagImage+"\n") != 1 || strings.Count(y, metricsServerCertArg+"\n") != 1 {
		return "", fmt.Errorf("%w: metrics-server's manifest does not have the image and arguments its pin was made for", ErrInstallFailed)
	}
	y = strings.Replace(y, "image: "+metricsServerTagImage+"\n", "image: "+MetricsServerImage+"\n", 1)
	i := strings.Index(y, metricsServerCertArg+"\n")
	indent := y[strings.LastIndex(y[:i], "\n")+1 : i]
	y = y[:i] + metricsServerCertArg + "\n" + indent + metricsServerTLSArg + "\n" + y[i+len(metricsServerCertArg)+1:]
	return y, nil
}

// InstallMetricsServer applies metrics-server, unless the pinned image
// already runs, and waits for it to roll out.
func (i *Installer) InstallMetricsServer(ctx context.Context, timeout time.Duration) error {
	if cur, err := i.kubectl(ctx, "", "-n", "kube-system", "get", "deployment/metrics-server",
		"-o", "jsonpath={.spec.template.spec.containers[0].image}"); err == nil && strings.TrimSpace(cur) == MetricsServerImage {
		return i.waitDeployment(ctx, "kube-system", "metrics-server", timeout)
	}
	m := MetricsServerManifest()
	fetch := i.Fetch
	if fetch == nil {
		fetch = httpFetch
	}
	body, err := fetch(ctx, m.URL)
	if err != nil {
		return fmt.Errorf("%w: download %s: %w", ErrInstallFailed, m.Name, err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
		return fmt.Errorf("%w: %s has sha256 %s, not the pinned %s; refusing to apply it",
			ErrInstallFailed, m.Name, got, m.SHA256)
	}
	y, err := metricsServerYAML(body)
	if err != nil {
		return err
	}
	i.logf("  applying metrics-server %s (the CPU signal for Cloud Run scaling)\n", MetricsServerVersion)
	if _, err := i.kubectl(ctx, y, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("%w: apply %s: %w", ErrInstallFailed, m.Name, err)
	}
	return i.waitDeployment(ctx, "kube-system", "metrics-server", timeout)
}
