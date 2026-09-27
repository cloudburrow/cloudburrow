//go:build compat

package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// reusedLayer matches one line of the lifecycle's exporter reporting that a
// layer of the previous image was kept rather than rewritten. The wording is
// the lifecycle's, not pack's: pack runs the lifecycle the pinned builder
// carries (0.21.18, per its io.buildpacks.builder.metadata label) and, in pack
// v0.40.9 (internal/build/phase_config_provider.go WithLogPrefix), prefixes
// each line with the phase name in brackets, such as "[exporter] ". In lifecycle
// v0.21.18, phase/exporter.go logs `Reusing layer '%s'` in
// addOrReuseBuildpackLayer and for a launch layer restored as metadata only,
// and adds `Adding layer '%s'` when the digest differs. "Reusing layers from
// image '%s'" (cmd/lifecycle/exporter.go) does not match: it has no quote
// after "layer".
var reusedLayer = regexp.MustCompile(`Reusing layer '([^']+)'`)

// addedLayer matches the exporter writing a layer afresh (lifecycle
// v0.21.18, phase/exporter.go addOrReuseBuildpackLayer).
var addedLayer = regexp.MustCompile(`Adding layer '([^']+)'`)

// recordingRunner runs pack for real and keeps its combined output, which
// buildpacks.Builder.Build does not return on success.
type recordingRunner struct{ out string }

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := buildpacks.ExecRunner{}.Run(ctx, name, args...)
	r.out = out
	return out, err
}

// requireBuildTools skips unless the functions tests were asked for, and
// fails when a tool the build needs is missing.
func requireBuildTools(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvFunctions) == "" {
		t.Skipf("%s is not set; this test pulls Google's builder and builds from source, so it runs only when asked for", EnvFunctions)
	}
	refuseCloudCredentials(t)
	for _, bin := range []string{"docker", "pack"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required: %v", bin, err)
		}
	}
}

// removeNewPackVolumes removes pack's build-cache volumes that were not
// present before the test. pack names them pack-cache-<image>-<hash>.build
// and .launch; each test's registry name is unique, so its volumes are too.
func removeNewPackVolumes(t *testing.T) {
	t.Helper()
	list := func() []string {
		out, err := exec.Command("docker", "volume", "ls", "-q").Output()
		if err != nil {
			return nil
		}
		return strings.Fields(string(out))
	}
	before := map[string]bool{}
	for _, v := range list() {
		before[v] = true
	}
	t.Cleanup(func() {
		for _, v := range list() {
			if !before[v] && strings.HasPrefix(v, "pack-cache-") {
				_ = exec.Command("docker", "volume", "rm", v).Run()
			}
		}
	})
}

// TestFunctionsRebuildReusesLayers (#678) builds the unmodified
// testdata/function fixture twice, with the pinned Google builder, through
// internal/buildpacks, to the same image in a local registry. The first build
// reuses nothing, since the registry holds no previous image; the second,
// with the source unchanged, must report at least one layer reused.
//
// The build runs on any host (on arm64 under emulation, which is slower); the
// image is not run, so no cluster is needed.
func TestFunctionsRebuildReusesLayers(t *testing.T) {
	requireBuildTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	removeNewPackVolumes(t)

	reg := functionsRegistry(ctx, t)
	req := buildpacks.Request{
		Source:         filepath.Join(moduleRoot(t), "testdata", "function"),
		Image:          reg.name + ":5000/cloudburrow-function-rebuild:compat",
		Registry:       reg.name + ":5000",
		Network:        reg.network,
		Insecure:       true,
		FunctionTarget: "Hello",
		Signature:      buildpacks.SignatureHTTP,
	}

	build := func(which string) string {
		t.Helper()
		rec := &recordingRunner{}
		start := time.Now()
		if err := (&buildpacks.Builder{Runner: rec}).Build(ctx, req); err != nil {
			t.Fatalf("%s build: %v", which, err)
		}
		t.Logf("%s build took %s", which, time.Since(start).Round(time.Second))
		return rec.out
	}

	first := build("first")
	if got := reusedLayer.FindAllStringSubmatch(first, -1); len(got) != 0 {
		t.Errorf("the first build, with no previous image, reported %d reused layers; want none:\n%s", len(got), first)
	}
	if len(addedLayer.FindAllString(first, -1)) == 0 {
		t.Errorf("the first build reported no added layers; pack's output:\n%s", first)
	}

	second := build("second")
	reused := reusedLayer.FindAllStringSubmatch(second, -1)
	if len(reused) == 0 {
		t.Fatalf("the rebuild of unchanged source reported no reused layers; pack's output:\n%s", second)
	}
	names := make([]string, 0, len(reused))
	for _, m := range reused {
		names = append(names, m[1])
	}
	t.Logf("the rebuild reused %d layers: %s", len(names), strings.Join(names, ", "))
}

// brokenModulePath is a module path whose first element has no dot. `go list
// -m` accepts it, but Google's Go Functions Framework buildpack refuses it:
// cmd/go/functions_framework/lib/lib.go, moduleAndPackageNames, returns
// gcp.UserErrorf("the module path in the function's go.mod must contain a dot
// in the first path element before a slash, e.g. example.com/module, found:
// %s") — present since 2020 (4f9ff17e) and unchanged at 983aaa1 (2026-09-24).
const brokenModulePath = "brokenmodule/function"

// wantBrokenModuleMessage is the buildpack's message for brokenModulePath.
const wantBrokenModuleMessage = "the module path in the function's go.mod must contain a dot in the first path element before a slash, e.g. example.com/module, found: " + brokenModulePath

// TestFunctionsBuildBrokenModulePathFails (#678) builds a copy of
// testdata/function whose go.mod declares brokenModulePath, and asserts what
// CloudBurrow surfaces: an error that errors.Is buildpacks.ErrBuildFailed,
// starts "build failed: exit status", and carries, in the tail of pack's log
// that Build appends, the buildpack's message naming the bad path.
func TestFunctionsBuildBrokenModulePathFails(t *testing.T) {
	requireBuildTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	removeNewPackVolumes(t)

	src := filepath.Join(moduleRoot(t), "testdata", "function")
	dir := t.TempDir()
	for _, name := range []string{"function.go", "go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("read the fixture: %v", err)
		}
		if name == "go.mod" {
			const orig = "module example.com/cloudburrow/function\n"
			if !bytes.HasPrefix(b, []byte(orig)) {
				t.Fatalf("testdata/function/go.mod no longer starts %q", orig)
			}
			b = append([]byte("module "+brokenModulePath+"\n"), b[len(orig):]...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatalf("copy the fixture: %v", err)
		}
	}

	// A registry the build can reach, so the only thing wrong is the source.
	reg := functionsRegistry(ctx, t)
	err := (&buildpacks.Builder{}).Build(ctx, buildpacks.Request{
		Source:         dir,
		Image:          reg.name + ":5000/cloudburrow-function-broken:compat",
		Registry:       reg.name + ":5000",
		Network:        reg.network,
		Insecure:       true,
		FunctionTarget: "Hello",
		Signature:      buildpacks.SignatureHTTP,
	})
	if err == nil {
		t.Fatalf("a build of module path %q succeeded; want it refused", brokenModulePath)
	}
	if !errors.Is(err, buildpacks.ErrBuildFailed) {
		t.Errorf("error = %v; want errors.Is ErrBuildFailed", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "build failed: exit status") {
		t.Errorf("error starts %q; want \"build failed: exit status\"", strings.SplitN(msg, "\n", 2)[0])
	}
	if !strings.Contains(msg, wantBrokenModuleMessage) {
		t.Errorf("the surfaced error does not carry the buildpack's message %q:\n%s", wantBrokenModuleMessage, msg)
	}
}
