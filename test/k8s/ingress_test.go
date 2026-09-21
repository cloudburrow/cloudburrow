//go:build integration

package k8s

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// envIngress is the host port `cloudburrow up` published the gateway on.
const envIngress = "CLOUDBURROW_TEST_INGRESS"

func ingressPort(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envIngress))
	if raw == "" {
		t.Skipf("%s is not set; start an instance with --port-ingress", envIngress)
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a port: %v", envIngress, raw, err)
	}
	return port
}

// deployIngressService deploys a Knative service and returns the URL Knative
// advertises for it.
func deployIngressService(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: %s
  namespace: default
spec:
  template:
    spec:
      containers:
        - image: ghcr.io/knative/helloworld-go:latest
          env:
            - name: TARGET
              value: CloudBurrow
`, name)

	if out, err := kubectl(t, ctx, manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("deploy %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, context.Background(), "", "delete", "ksvc", name, "--ignore-not-found")
	})

	if out, err := kubectl(t, ctx, "", "wait", "ksvc/"+name,
		"--for=condition=Ready", "--timeout=300s"); err != nil {
		diag, _ := kubectl(t, ctx, "", "get", "ksvc", name, "-o", "yaml")
		t.Fatalf("%s never became ready: %v\n%s\n%s", name, err, out, diag)
	}

	url, err := kubectl(t, ctx, "", "get", "ksvc", name, "-o", "jsonpath={.status.url}")
	if err != nil {
		t.Fatalf("read the service URL: %v\n%s", err, url)
	}
	return strings.TrimSpace(url)
}

// TestKnativeServiceIsNamedUnderTheLocalDomain proves the configured domain is
// actually applied, rather than Knative keeping its default.
func TestKnativeServiceIsNamedUnderTheLocalDomain(t *testing.T) {
	ctx := testCtx(t)
	url := deployIngressService(t, ctx, "ingress-named")

	const want = ".cloudburrow.localhost"
	if !strings.HasSuffix(url, want) {
		t.Fatalf("service URL = %q, want a %s name", url, want)
	}
	// The old suffix must be gone, or config-domain holds two defaults and
	// Knative chooses between them.
	if strings.Contains(url, "sslip.io") {
		t.Errorf("the previous domain is still configured: %q", url)
	}
	t.Logf("service named %s", url)
}

// TestBrowserStyleRequestReachesTheService is the point of #86: opening the
// advertised URL, with no Host header manipulation and no port-forward, the
// way a browser or a webhook would.
func TestBrowserStyleRequestReachesTheService(t *testing.T) {
	ctx := testCtx(t)
	port := ingressPort(t)
	url := deployIngressService(t, ctx, "ingress-browser")

	// The advertised URL has no port; the gateway is published on one.
	target := fmt.Sprintf("%s:%d/", url, port)

	body, code := getWithRetry(t, ctx, target, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, code, body)
	}
	if !strings.Contains(body, "CloudBurrow") {
		t.Errorf("response did not come from the service: %q", body)
	}
	t.Logf("GET %s -> %d %q", target, code, strings.TrimSpace(body))
}

// TestGatewayRoutesByHostHeader proves the fallback that always works, even
// where *.localhost does not resolve.
func TestGatewayRoutesByHostHeader(t *testing.T) {
	ctx := testCtx(t)
	port := ingressPort(t)
	url := deployIngressService(t, ctx, "ingress-hosthdr")

	host := strings.TrimPrefix(url, "http://")
	target := fmt.Sprintf("http://127.0.0.1:%d/", port)

	body, code := getWithRetry(t, ctx, target, host)
	if code != http.StatusOK {
		t.Fatalf("GET %s (Host: %s) = %d: %s", target, host, code, body)
	}
	if !strings.Contains(body, "CloudBurrow") {
		t.Errorf("response did not come from the service: %q", body)
	}
}

// TestUnknownHostIsNotRoutedSomewhereElse proves the gateway routes rather
// than forwarding everything to whatever happens to be deployed.
func TestUnknownHostIsNotRoutedSomewhereElse(t *testing.T) {
	ctx := testCtx(t)
	port := ingressPort(t)
	_ = deployIngressService(t, ctx, "ingress-routing")

	body, code := get(t, ctx, fmt.Sprintf("http://127.0.0.1:%d/", port),
		"nosuchservice.default.cloudburrow.localhost")
	if code == http.StatusOK {
		t.Fatalf("an unknown host was served a response: %q", body)
	}
	t.Logf("unknown host = %d", code)
}

// TestIngressIsLoopbackOnly proves the gateway is not published on the LAN.
//
// A local development gateway reachable from the network would expose every
// deployed service to it, which is not a trade a developer opted into.
func TestIngressIsLoopbackOnly(t *testing.T) {
	port := ingressPort(t)

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		target := net.JoinHostPort(ipnet.IP.String(), strconv.Itoa(port))
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			t.Errorf("the ingress gateway answered on %s; it must be loopback only", target)
		}
	}
}

func get(t *testing.T, ctx context.Context, url, host string) (string, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err.Error(), 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode
}

// getWithRetry tolerates the gateway's brief window between a revision
// becoming Ready and a route being programmed, which otherwise shows up as a
// flaky 404.
func getWithRetry(t *testing.T, ctx context.Context, url, host string) (string, int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var body string
	var code int
	for {
		body, code = get(t, ctx, url, host)
		if code == http.StatusOK || time.Now().After(deadline) || ctx.Err() != nil {
			return body, code
		}
		time.Sleep(2 * time.Second)
	}
}
