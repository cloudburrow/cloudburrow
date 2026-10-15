//go:build compat

package compat

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
	"github.com/cloudburrow/cloudburrow/internal/images"
)

// languageFunction is one Functions Framework fixture in a language other
// than Go (#1303; Java, Ruby, PHP and .NET since #1319).
type languageFunction struct {
	// runtime is what the builder detects the fixture as, and names its
	// images and services.
	runtime string
	// dir is the fixture under testdata.
	dir string
	// httpTarget and eventTarget are the fixture's two functions.
	httpTarget, eventTarget string
	// marker is the file the runtime is detected from.
	marker string
	// rejectsNonEvents: the framework answers HTTP 400 to a request with no
	// ce-* headers and no legacy event to convert. The Python framework does
	// (functions_framework 3.10.2, _cloud_event_view_func_wrapper aborts 400
	// when background_event_to_cloud_event fails); the Node.js framework
	// (5.0.5, parseCloudEventRequest) hands the body to the function as a
	// structured-mode event instead, so nothing is asserted there.
	rejectsNonEvents bool
}

// TestFunctionsNodejsBuiltWithBuildpacks (#1303) builds the unmodified
// testdata/function-nodejs fixture (package.json and its lockfile, index.js
// on @google-cloud/functions-framework) with the pinned Google builder, which
// detects Node.js from package.json; deploys it through the official Cloud
// Run v2 client; and asserts the http and cloudevent signatures as
// TestFunctionsFrameworkBuiltWithBuildpacks does for Go.
func TestFunctionsNodejsBuiltWithBuildpacks(t *testing.T) {
	testLanguageFunction(t, languageFunction{
		runtime: "nodejs", dir: "function-nodejs", httpTarget: "Hello", eventTarget: "Event",
		marker: "package.json",
	})
}

// TestFunctionsPythonBuiltWithBuildpacks (#1303) builds the unmodified
// testdata/function-python fixture (requirements.txt pinning
// functions-framework, main.py) with the pinned Google builder, which detects
// Python from requirements.txt; deploys it through the official Cloud Run v2
// client; and asserts the http and cloudevent signatures, and the framework's
// 400 for a request that is not a CloudEvent.
func TestFunctionsPythonBuiltWithBuildpacks(t *testing.T) {
	testLanguageFunction(t, languageFunction{
		runtime: "python", dir: "function-python", httpTarget: "hello", eventTarget: "event",
		marker: "requirements.txt", rejectsNonEvents: true,
	})
}

// TestFunctionsJavaBuiltWithBuildpacks (#1319) builds the unmodified
// testdata/function-java fixture (a Maven pom.xml on functions-framework-api
// 1.1.4 and Gson, the classes functions.Hello and functions.Event) with the
// pinned Google builder, which detects Java from pom.xml and runs the class
// GOOGLE_FUNCTION_TARGET names with the framework's invoker; deploys it
// through the official Cloud Run v2 client; and asserts the http and
// cloudevent signatures as the Node.js test does. No 400 is asserted for a
// request that is not a CloudEvent: nothing has measured the invoker's answer.
func TestFunctionsJavaBuiltWithBuildpacks(t *testing.T) {
	testLanguageFunction(t, languageFunction{
		runtime: "java", dir: "function-java", httpTarget: "functions.Hello", eventTarget: "functions.Event",
		marker: "pom.xml",
	})
}

// TestFunctionsRubyBuiltWithBuildpacks (#1319) builds the unmodified
// testdata/function-ruby fixture (a Gemfile and its lockfile pinning
// functions_framework 1.7.0, app.rb) with the pinned Google builder, which
// detects Ruby from the Gemfile; deploys it through the official Cloud Run v2
// client; and asserts the http and cloudevent signatures.
func TestFunctionsRubyBuiltWithBuildpacks(t *testing.T) {
	testLanguageFunction(t, languageFunction{
		runtime: "ruby", dir: "function-ruby", httpTarget: "hello", eventTarget: "event",
		marker: "Gemfile",
	})
}

// TestFunctionsPHPBuiltWithBuildpacks (#1319) builds the unmodified
// testdata/function-php fixture (composer.json pinning
// google/cloud-functions-framework 1.4.3, index.php) with the pinned Google
// builder, which detects PHP from composer.json; deploys it through the
// official Cloud Run v2 client; and asserts the http and cloudevent
// signatures. The CloudEvent function logs to stderr: a PHP function's
// standard output is its response.
func TestFunctionsPHPBuiltWithBuildpacks(t *testing.T) {
	testLanguageFunction(t, languageFunction{
		runtime: "php", dir: "function-php", httpTarget: "hello", eventTarget: "event",
		marker: "composer.json",
	})
}

// TestFunctionsDotnetBuiltWithBuildpacks (#1319) builds the unmodified
// testdata/function-dotnet fixture (Function.csproj on
// Google.Cloud.Functions.Hosting 3.0.1 for net8.0, the classes Fixture.Hello
// and Fixture.Event) with the pinned Google builder, which detects .NET from
// the .csproj; deploys it through the official Cloud Run v2 client; and
// asserts the http and cloudevent signatures.
func TestFunctionsDotnetBuiltWithBuildpacks(t *testing.T) {
	testLanguageFunction(t, languageFunction{
		runtime: "dotnet", dir: "function-dotnet", httpTarget: "Fixture.Hello", eventTarget: "Fixture.Event",
		marker: "Function.csproj",
	})
}

func testLanguageFunction(t *testing.T, fn languageFunction) {
	t.Helper()
	if os.Getenv(EnvFunctions) == "" {
		t.Skipf("%s is not set; this test pulls Google's builder and builds from source, so it runs only when asked for", EnvFunctions)
	}
	fixture := filepath.Join(moduleRoot(t), "testdata", fn.dir)
	// What CloudBurrow reports before pack runs: the runtime is detected from
	// the fixture's marker, and no GOOGLE_RUNTIME is forced.
	if det, err := buildpacks.Detect(fixture, nil); err != nil || det.Runtime != fn.runtime || det.From != fn.marker {
		t.Fatalf("Detect(%s) = %+v, %v; want runtime %s from %s", fn.dir, det, err, fn.runtime, fn.marker)
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
	removeNewPackVolumes(t)

	loader := &images.Loader{ClusterName: cluster, Runner: buildpacks.ExecRunner{}}
	arch, err := loader.NodeArchitecture(ctx, kubeconfig)
	if err != nil {
		t.Fatalf("read the node architecture: %v", err)
	}
	if want := strings.TrimPrefix(buildpacks.BuilderPlatform, "linux/"); arch != want {
		t.Skipf("the cluster's nodes are %s; the Google builder produces %s images only, which cannot run here", arch, want)
	}

	before := map[string]bool{}
	if out, err := exec.CommandContext(ctx, "docker", "images", "-q", "--no-trunc").Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			before[id] = true
		}
	}

	reg := functionsRegistry(ctx, t)
	httpImage := buildFunction(ctx, t, loader, kubeconfig, before, reg, fixture, fn.runtime+"-http", fn.httpTarget, buildpacks.SignatureHTTP)
	eventImage := buildFunction(ctx, t, loader, kubeconfig, before, reg, fixture, fn.runtime+"-event", fn.eventTarget, buildpacks.SignatureCloudEvent)

	base := ingress(t)

	httpSvc := deployFunction(ctx, t, h, rc, "compat-fn-"+fn.runtime+"-http", httpImage)
	httpHost := hostOf(t, httpSvc.GetUri())
	code, body := functionRequest(t, base, httpHost, `{"name":"CloudBurrow"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello CloudBurrow"`) {
		t.Errorf("POST / with a name = %d %q; want 200 and the greeting", code, body)
	}
	code, body = functionRequest(t, base, httpHost, "", nil)
	if code != http.StatusOK || !strings.Contains(body, `"greeting":"hello world"`) {
		t.Errorf("POST / with no body = %d %q; want 200 and the default greeting", code, body)
	}

	eventSvcName := "compat-fn-" + fn.runtime + "-event"
	eventSvc := deployFunction(ctx, t, h, rc, eventSvcName, eventImage)
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
	want := "received CloudEvent type=" + ceType + " subject=" + ceSubject + ` data={"bucket":"compat-fn-bucket","name":"compat-fn.txt"}`
	if logs := functionLogs(ctx, t, kubeconfig, eventSvcName, want); !strings.Contains(logs, want) {
		t.Errorf("the handler never logged %q; its logs:\n%s", want, logs)
	}

	if fn.rejectsNonEvents {
		code, body = functionRequest(t, base, eventHost, `{"bucket":"compat-fn-bucket"}`, map[string]string{"Content-Type": "application/json"})
		if code != http.StatusBadRequest {
			t.Errorf("a request with no ce-* headers = %d %q; want 400", code, body)
		}
	}
}
