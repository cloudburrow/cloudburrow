//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// predictorImage is the Vertex custom prediction container fixture.
//
// dev.local is required: Knative resolves tags against a registry, and a
// locally built image has none.
const predictorImage = "dev.local/cloudburrow-predictor:test"

// envKubeconfig points at the kubeconfig `cloudburrow up` generated. It is
// needed because the Knative ingress is not published on a host port, so a
// host-side caller reaches a service through a port-forward.
const envKubeconfig = "CLOUDBURROW_TEST_KUBECONFIG"

// buildPredictor builds the fixture and loads it into the owned cluster.
//
// The image is built once per run and loaded into CloudBurrow's own cluster —
// never into a shared Docker context, and nothing is pruned globally.
func buildPredictor(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, bin := range []string{"docker", "kind"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the prediction fixture", bin)
		}
	}
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the fixture must be loaded into the owned cluster")
	}

	root := moduleRoot(t)
	build := exec.CommandContext(ctx, "docker", "build", "-t", predictorImage, "testdata/predictor")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the prediction fixture in %s: %v\n%s", root, err, out)
	}
	load := exec.CommandContext(ctx, "kind", "load", "docker-image", predictorImage, "--name", cluster)
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("load the prediction fixture into cluster %s: %v\n%s", cluster, err, out)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test directory")
		}
		dir = parent
	}
}

// deployPredictor deploys the fixture as a prediction endpoint on the owned
// runtime, through the official Cloud Run SDK, and returns its URI.
//
// This is the decision #42 asks for, exercised: the container is Google's
// contract, the runtime is CloudBurrow's. Nothing starts a Docker container
// outside the cluster, so there is nothing to orphan.
func deployPredictor(t *testing.T, c *run.ServicesClient, h *Harness, id string, routes prediction.Routes, env map[string]string) *runpb.Service {
	t.Helper()
	ctx := h.Context()
	name := runParent(h) + "/services/" + id

	vars := routes.Env()
	for k, v := range env {
		vars[k] = v
	}
	container := &runpb.Container{Image: predictorImage}
	for k, v := range vars {
		container.Env = append(container.Env, &runpb.EnvVar{
			Name:   k,
			Values: &runpb.EnvVar_Value{Value: v},
		})
	}

	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: id,
		Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{container}},
		},
	})
	if err != nil {
		t.Fatalf("CreateService(%s): %v", id, err)
	}
	// Cleanup is per-endpoint and runs even when the wait below fails, so a
	// failed deployment does not leak a revision into the next test.
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = c.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})

	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("prediction endpoint %s never became ready: %v", id, err)
	}
	return svc
}

// TestPredictionEndpointServesTheVertexContract deploys a custom prediction
// container on the owned runtime and drives the Vertex serving contract
// against it: health, real predictions, the parameters field, and the
// malformed inputs that must be refused.
//
// The routes are deliberately non-default, so the test proves that the
// AIP_* environment is actually plumbed through rather than that the fixture
// happens to use the same paths CloudBurrow assumes.
func TestPredictionEndpointServesTheVertexContract(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	svc := deployPredictor(t, c, h, "compat-predictor", routes, nil)

	uri := svc.GetUri()
	if uri == "" {
		t.Fatal("a ready prediction endpoint has no URI")
	}
	endpoint := prediction.NewEndpoint(svc.GetName(), routes, prediction.ConditionSource{
		Ready:    readyCondition(svc),
		Replicas: 1,
		URI:      uri,
	})
	if endpoint.State != prediction.EndpointReady {
		t.Fatalf("endpoint state = %s, want READY", endpoint.State)
	}
	predictURL, err := endpoint.PredictURL()
	if err != nil {
		t.Fatalf("PredictURL: %v", err)
	}
	if predictURL != uri+routes.Predict {
		t.Errorf("PredictURL() = %q, want the advertised URI plus the configured route", predictURL)
	}
	t.Logf("prediction endpoint ready at %s (predict %s)", uri, predictURL)

	base, host := ingress(t), hostOf(t, uri)

	// Health, on the configured route.
	code, body := httpGet(t, base, host, routes.Health)
	if !prediction.HealthyStatus(code) {
		t.Fatalf("health %s = %d: %s", routes.Health, code, body)
	}

	// A real prediction, decoded through the contract types rather than by
	// string matching, so the response shape is what a Vertex client parses.
	reqBody := `{"instances":[1,2,3.5]}`
	code, body = httpPost(t, base, host, routes.Predict, reqBody, 60*time.Second)
	if code != http.StatusOK {
		t.Fatalf("predict = %d: %s", code, body)
	}
	req, err := prediction.DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("the test's own request is malformed: %v", err)
	}
	var resp prediction.Response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode prediction response: %v\n%s", err, body)
	}
	if err := prediction.ValidateResponse(req, resp); err != nil {
		t.Fatalf("response does not answer the request: %v", err)
	}
	want := []string{"2", "4", "7"}
	for i, p := range resp.Predictions {
		if strings.TrimSpace(string(p)) != want[i] {
			t.Errorf("prediction[%d] = %s, want %s", i, p, want[i])
		}
	}
	if resp.DeployedModelID != "doubler-v1" {
		t.Errorf("deployedModelId = %q, want doubler-v1", resp.DeployedModelID)
	}

	// Malformed input must be refused, not answered with predictions that do
	// not correspond to the caller's input.
	for _, tc := range []struct{ name, body string }{
		{"no instances", `{"instances":[]}`},
		{"wrong instance type", `{"instances":["abc"]}`},
		{"unknown field", `{"instances":[1],"nosuchfield":2}`},
		{"not json", `{`},
	} {
		code, body := httpPost(t, base, host, routes.Predict, tc.body, 30*time.Second)
		if code != http.StatusBadRequest {
			t.Errorf("%s: predict = %d, want 400: %s", tc.name, code, body)
		}
	}

	// The default routes must NOT answer, which is what proves the AIP_*
	// environment was honoured rather than ignored.
	if code, _ := httpGet(t, base, host, prediction.DefaultHealthRoute); code != http.StatusNotFound {
		t.Errorf("default health route = %d, want 404: the container ignored AIP_HEALTH_ROUTE", code)
	}
}

// TestPredictionDeadlineBoundsASlowPrediction proves a hung prediction fails
// the caller rather than blocking it, using the contract's own parameters
// field to make the container slow.
func TestPredictionDeadlineBoundsASlowPrediction(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	routes := prediction.DefaultRoutes()
	svc := deployPredictor(t, c, h, "compat-predictor-slow", routes, nil)
	base, host := ingress(t), hostOf(t, svc.GetUri())

	// A prediction slower than the caller's deadline must fail the caller.
	start := time.Now()
	_, _, err := httpPostErr(base, host, routes.Predict,
		`{"instances":[1],"parameters":{"delayMs":10000}}`, 2*time.Second)
	if err == nil {
		t.Fatal("a 10s prediction returned within a 2s deadline")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("the deadline did not bound the call: waited %v", elapsed)
	}
	t.Logf("slow prediction correctly failed the caller: %v", err)

	// The endpoint is still usable afterwards: an abandoned request must not
	// wedge it.
	code, body := httpPost(t, base, host, routes.Predict, `{"instances":[4]}`, 60*time.Second)
	if code != http.StatusOK {
		t.Fatalf("endpoint unusable after an abandoned request: %d %s", code, body)
	}
	if !strings.Contains(body, "8") {
		t.Errorf("unexpected prediction after recovery: %s", body)
	}
}

// TestPredictionStartupFailureIsReported proves a container that cannot start
// is reported as failed, with the container's own output, rather than left
// pending forever.
//
// This is the case that matters most operationally: a caller waiting on an
// endpoint that will never become ready needs to be told, not stalled.
//
// It is slow by nature. Knative declares a revision failed only after its
// progress deadline, which defaults to 600s, and the Cloud Run v2 surface
// exposes no field that shortens it. Measured here at 602s. The test budgets
// 13 minutes and logs what it actually observed, because the latency is the
// finding — see the Local AI prediction section of docs/compatibility.md.
func TestPredictionStartupFailureIsReported(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	id := "compat-predictor-broken"
	name := runParent(h) + "/services/" + id
	routes := prediction.DefaultRoutes()
	vars := routes.Env()
	vars["FAIL_STARTUP"] = "true"

	container := &runpb.Container{Image: predictorImage}
	for k, v := range vars {
		container.Env = append(container.Env, &runpb.EnvVar{
			Name: k, Values: &runpb.EnvVar_Value{Value: v},
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if _, err := c.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: id,
		Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{container}},
		},
	}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		delCtx, dc := context.WithTimeout(context.Background(), 60*time.Second)
		defer dc()
		_, _ = c.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})

	started := time.Now()
	deadline := started.Add(13 * time.Minute)
	for {
		got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		ep := prediction.NewEndpoint(name, routes, prediction.ConditionSource{
			Ready:   readyCondition(got),
			Reason:  terminalReason(got),
			Message: got.GetTerminalCondition().GetMessage(),
			URI:     got.GetUri(),
		})
		switch ep.State {
		case prediction.EndpointFailed:
			if !ep.State.Terminal() {
				t.Error("a failed endpoint must be terminal")
			}
			t.Logf("startup failure reported after %v: reason=%q message=%q",
				time.Since(started).Round(time.Second), ep.Reason, ep.Message)
			// The message must name what actually broke. "does not have any
			// ready Revision" is true and useless; the container's own log
			// line is what a developer can act on.
			if !strings.Contains(ep.Message, "FAIL_STARTUP is set") {
				t.Errorf("failure message did not carry the container output: %q", ep.Message)
			}
			return
		case prediction.EndpointReady, prediction.EndpointScaledToZero:
			t.Fatalf("a container that exits at startup was reported %s", ep.State)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the endpoint was still %s after %v; a failure that is never "+
				"reported is indistinguishable from a hang",
				ep.State, time.Since(started).Round(time.Second))
		}
		time.Sleep(5 * time.Second)
	}
}

// TestPredictionEndpointDeletionIsComplete proves the lifecycle closes: a
// deleted endpoint is gone from the API, so nothing is left running.
func TestPredictionEndpointDeletionIsComplete(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	id := "compat-predictor-ephemeral"
	name := runParent(h) + "/services/" + id
	deployPredictor(t, c, h, id, prediction.DefaultRoutes(), nil)

	ctx := h.Context()
	op, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for deletion: %v", err)
	}

	_, err = c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("after deletion GetService returned %v, want NotFound", err)
	}
}

// ingress forwards the Knative gateway to a loopback port and returns the
// base URL to send requests to.
//
// `cloudburrow up` publishes the gateway on a host port (#86), but the port is
// fixed at cluster creation and a compatibility run must work against a
// cluster it did not create. A port-forward reaches the same gateway without
// depending on how the instance was started, and the Host header is what
// Knative routes on either way — so this needs no resolver and no published
// port, while still going through the real ingress rather than the pod.
func ingress(t *testing.T) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the Knative ingress is reached through a port-forward", envKubeconfig)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to reach the cluster ingress")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a local port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// The port is released immediately; port-forward binds it next. A race
	// here would surface as a bind error, not a silent wrong-target forward.
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", "kourier-system", "port-forward", "svc/kourier-internal",
		fmt.Sprintf("%d:80", port))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start port-forward: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			return base
		}
		if time.Now().After(deadline) {
			t.Fatalf("port-forward to the Knative gateway never came up: %v\n%s", err, out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// hostOf returns the Host header the gateway routes on for a service URI.
func hostOf(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse service URI %q: %v", uri, err)
	}
	return u.Host
}

// terminalReason renders the runtime's own reason. Cloud Run models the
// common reason as an enum, so the enum name is what a caller can search for.
func terminalReason(svc *runpb.Service) string {
	c := svc.GetTerminalCondition()
	if r := c.GetRevisionReason(); r != runpb.Condition_REVISION_REASON_UNDEFINED {
		return r.String()
	}
	return c.GetReason().String()
}

func readyCondition(svc *runpb.Service) string {
	switch svc.GetTerminalCondition().GetState() {
	case runpb.Condition_CONDITION_SUCCEEDED:
		return "True"
	case runpb.Condition_CONDITION_FAILED:
		return "False"
	default:
		return "Unknown"
	}
}

func httpGet(t *testing.T, base, host, path string) (int, string) {
	t.Helper()
	code, body, err := doRequest(http.MethodGet, base+path, host, "", 30*time.Second)
	if err != nil {
		t.Fatalf("GET %s%s: %v", host, path, err)
	}
	return code, body
}

func httpPost(t *testing.T, base, host, path, body string, timeout time.Duration) (int, string) {
	t.Helper()
	code, got, err := httpPostErr(base, host, path, body, timeout)
	if err != nil {
		t.Fatalf("POST %s%s: %v", host, path, err)
	}
	return code, got
}

func httpPostErr(base, host, path, body string, timeout time.Duration) (int, string, error) {
	return doRequest(http.MethodPost, base+path, host, body, timeout)
}

// doRequest sends one request, addressing the gateway by IP and naming the
// service in the Host header, which is how Knative routes.
func doRequest(method, url, host, body string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		return resp.StatusCode, "", fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, string(got), nil
}
