//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
	"github.com/cloudburrow/cloudburrow/internal/images"
)

// EnvFunctions opts in to TestFunctionsFrameworkBuiltWithBuildpacks. The
// test pulls Google's builder and run images and builds twice, which takes
// minutes, so it never runs unless asked for; it is listed in
// docs/compatibility.md "What CI does not run, and why".
const EnvFunctions = "CLOUDBURROW_TEST_FUNCTIONS"

// functionsRegistryImage is the local registry the build publishes to,
// pinned by digest (registry 2.8.3). A registry is required: see
// buildpacks.Request.Registry and docs/functions-and-builds.md.
const functionsRegistryImage = "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

// TestFunctionsFrameworkBuiltWithBuildpacks (#678) builds the unmodified
// testdata/function fixture with the pinned Google builder, through
// internal/buildpacks, into a local registry; loads each image into the owned
// cluster; deploys it through the official Cloud Run v2 client; and asserts,
// through the Knative ingress:
//
//   - the `http` signature: POST / returns the handler's JSON, and a request
//     with no body still answers 200;
//   - the `cloudevent` signature: a binary-mode CloudEvent is accepted, and
//     the handler logs its type, subject and data;
//   - a request with no ce-* headers to the CloudEvent function is HTTP 400.
//
// Google's builder is linux/amd64 only, so the image runs only on amd64
// nodes; on any other node architecture the test skips before building.
func TestFunctionsFrameworkBuiltWithBuildpacks(t *testing.T) {
	if os.Getenv(EnvFunctions) == "" {
		t.Skipf("%s is not set; this test pulls Google's builder and builds from source, so it runs only when asked for", EnvFunctions)
	}
	h := New(t)
	rc := runClient(t, h)
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Fatal("CLOUDBURROW_TEST_CLUSTER is not set; the built images must be loaded into the owned cluster")
	}
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Fatalf("%s is not set; the node architecture and the function's logs are read through it", envKubeconfig)
	}
	for _, bin := range []string{"docker", "kind", "kubectl", "pack"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required: %v", bin, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	loader := &images.Loader{ClusterName: cluster, Runner: buildpacks.ExecRunner{}}
	arch, err := loader.NodeArchitecture(ctx, kubeconfig)
	if err != nil {
		t.Fatalf("read the node architecture: %v", err)
	}
	if want := strings.TrimPrefix(buildpacks.BuilderPlatform, "linux/"); arch != want {
		t.Skipf("the cluster's nodes are %s; the Google builder produces %s images only, which cannot run here", arch, want)
	}

	// Images present before the test, so cleanup removes only what it made.
	before := map[string]bool{}
	if out, err := exec.CommandContext(ctx, "docker", "images", "-q", "--no-trunc").Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			before[id] = true
		}
	}

	reg := functionsRegistry(ctx, t)
	fixture := filepath.Join(moduleRoot(t), "testdata", "function")

	httpImage := buildFunction(ctx, t, loader, kubeconfig, before, reg, fixture, "Hello", buildpacks.SignatureHTTP)
	eventImage := buildFunction(ctx, t, loader, kubeconfig, before, reg, fixture, "Event", buildpacks.SignatureCloudEvent)

	base := ingress(t)

	// The http signature.
	httpSvc := deployFunction(ctx, t, h, rc, "compat-fn-http", httpImage)
	httpHost := hostOf(t, httpSvc.GetUri())
	code, body := functionRequest(t, base, httpHost, `{"name":"CloudBurrow"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello CloudBurrow"`) {
		t.Errorf("POST / with a name = %d %q; want 200 and the greeting", code, body)
	}
	code, body = functionRequest(t, base, httpHost, "", nil)
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello world"`) {
		t.Errorf("POST / with no body = %d %q; want 200 and the default greeting", code, body)
	}

	// The cloudevent signature, in binary mode: attributes as ce-* headers,
	// data as the body, in the schema Cloud Storage events use.
	eventSvc := deployFunction(ctx, t, h, rc, "compat-fn-event", eventImage)
	eventHost := hostOf(t, eventSvc.GetUri())
	const (
		ceType    = "google.cloud.storage.object.v1.finalized"
		ceSubject = "objects/compat-fn.txt"
	)
	code, body = functionRequest(t, base, eventHost, `{"bucket":"compat-fn-bucket","name":"compat-fn.txt"}`, map[string]string{
		"Content-Type":   "application/json",
		"ce-specversion": "1.0",
		"ce-id":          "compat-fn-1",
		"ce-source":      "//storage.googleapis.com/projects/_/buckets/compat-fn-bucket",
		"ce-type":        ceType,
		"ce-subject":     ceSubject,
	})
	if code < 200 || code > 299 {
		t.Errorf("binary-mode CloudEvent = %d %q; want 2xx", code, body)
	}
	// The handler prints what it received; its log is the observation.
	want := "received CloudEvent type=" + ceType + " subject=" + ceSubject + " data=map[bucket:compat-fn-bucket name:compat-fn.txt]"
	if logs := functionLogs(ctx, t, kubeconfig, "compat-fn-event", want); !strings.Contains(logs, want) {
		t.Errorf("the handler never logged %q; its logs:\n%s", want, logs)
	}

	// No ce-* headers: not a CloudEvent, so the framework refuses it.
	code, body = functionRequest(t, base, eventHost, `{"bucket":"compat-fn-bucket"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusBadRequest {
		t.Errorf("a request with no ce-* headers = %d %q; want 400", code, body)
	}
}

// localRegistry is a registry reachable by name from inside the build's
// lifecycle container and by port from the host.
type localRegistry struct {
	network, name string
	port          int
}

// functionsRegistry starts a local registry on a Docker network of its own.
// The build's lifecycle runs in a container, so it reaches the registry by
// name on that network; the host pulls the result back through the port
// published on loopback.
func functionsRegistry(ctx context.Context, t *testing.T) localRegistry {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	reg := localRegistry{network: "cb-fn-build-" + suffix, name: "cb-fn-registry-" + suffix}
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", reg.network).CombinedOutput(); err != nil {
		t.Fatalf("create the build network: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", reg.network).Run() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a local port: %v", err)
	}
	reg.port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if b, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", reg.name, "--network", reg.network,
		"-p", fmt.Sprintf("127.0.0.1:%d:5000", reg.port), functionsRegistryImage).CombinedOutput(); err != nil {
		t.Fatalf("start the local registry: %v\n%s", err, b)
	}
	// Registered after the network's cleanup, so it runs first: a network
	// cannot be removed while a container is attached.
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", reg.name).Run() })

	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v2/", reg.port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return reg
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the local registry never answered on port %d: %v", reg.port, err)
		}
		time.Sleep(time.Second)
	}
}

// buildFunction builds the fixture for one signature through
// internal/buildpacks, pulls the result from the local registry, and loads it
// into the cluster under a dev.local/ name, which Knative runs without
// resolving it against a registry.
func buildFunction(ctx context.Context, t *testing.T, loader *images.Loader, kubeconfig string, before map[string]bool,
	reg localRegistry, fixture, target string, sig buildpacks.Signature) string {
	t.Helper()
	repo := "cloudburrow-function-" + string(sig) + ":compat"
	start := time.Now()
	err := (&buildpacks.Builder{}).Build(ctx, buildpacks.Request{
		Source:         fixture,
		Image:          reg.name + ":5000/" + repo,
		Registry:       reg.name + ":5000",
		Network:        reg.network,
		Insecure:       true,
		FunctionTarget: target,
		Signature:      sig,
	})
	if err != nil {
		t.Fatalf("build %s (%s): %v", target, sig, err)
	}
	t.Logf("built %s (%s) in %s", target, sig, time.Since(start).Round(time.Second))

	pulled := fmt.Sprintf("127.0.0.1:%d/%s", reg.port, repo)
	if out, err := exec.CommandContext(ctx, "docker", "pull", "--platform", buildpacks.BuilderPlatform, pulled).CombinedOutput(); err != nil {
		t.Fatalf("pull %s from the local registry: %v\n%s", pulled, err, out)
	}
	local := images.LocalPrefix + repo
	if out, err := exec.CommandContext(ctx, "docker", "tag", pulled, local).CombinedOutput(); err != nil {
		t.Fatalf("tag %s: %v\n%s", local, err, out)
	}
	t.Cleanup(func() { removeNewImage(before, pulled, local) })
	if err := loader.Load(ctx, local, kubeconfig); err != nil {
		t.Fatalf("load %s into the cluster: %v", local, err)
	}
	return local
}

// removeNewImage removes the tags the test made and then the image, by ID,
// only when that ID was not present before the test started.
func removeNewImage(before map[string]bool, refs ...string) {
	out, err := exec.Command("docker", "image", "inspect", refs[0], "--format", "{{.Id}}").Output()
	if err != nil {
		return
	}
	id := strings.TrimSpace(string(out))
	if before[id] {
		return
	}
	for _, ref := range refs {
		_ = exec.Command("docker", "image", "rm", ref).Run()
	}
	_ = exec.Command("docker", "image", "rm", id).Run()
}

// deployFunction deploys a built function through the official Cloud Run
// client and waits for it to be ready.
func deployFunction(ctx context.Context, t *testing.T, h *Harness, rc *run.ServicesClient, id, image string) *runpb.Service {
	t.Helper()
	name := runParent(h) + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{Image: image}}}},
	})
	if err != nil {
		t.Fatalf("CreateService %s: %v", id, err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("%s never became ready: %v", id, err)
	}
	return svc
}

// functionRequest POSTs to a function through the ingress, retrying while
// the route settles, and returns the first answer that is not a gateway
// error.
func functionRequest(t *testing.T, base, host, body string, headers map[string]string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, got, err := func() (int, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", bytes.NewReader([]byte(body)))
			if err != nil {
				return 0, "", err
			}
			req.Host = host
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, "", err
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b), err
		}()
		// 404 and 5xx are the gateway before the route is programmed.
		if err == nil && code != http.StatusNotFound && code < 500 {
			return code, got
		}
		if time.Now().After(deadline) {
			if err != nil {
				return 0, err.Error()
			}
			return code, got
		}
		time.Sleep(2 * time.Second)
	}
}

// functionLogs reads the function's container logs until they contain want
// or a minute passes, and returns the last read.
func functionLogs(ctx context.Context, t *testing.T, kubeconfig, service, want string) string {
	t.Helper()
	var logs string
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "default",
			"logs", "-l", "serving.knative.dev/service="+service, "-c", "user-container", "--tail", "-1").CombinedOutput()
		logs = string(out)
		if err == nil && strings.Contains(logs, want) {
			return logs
		}
		if time.Now().After(deadline) {
			return logs
		}
	}
}
