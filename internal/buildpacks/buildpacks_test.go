package buildpacks

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func hasFlag(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// A registry is required, not optional: pack's export to the Docker daemon
// fails on an arm64 host with this amd64 builder, so publishing is the only
// path that works everywhere.
func TestRegistryIsRequired(t *testing.T) {
	t.Parallel()
	_, err := Args(Request{Source: "./src", Image: "reg:5000/app:v1"})
	if !errors.Is(err, ErrRegistryRequired) {
		t.Fatalf("Args() = %v, want ErrRegistryRequired", err)
	}
	if !strings.Contains(err.Error(), "Docker daemon") {
		t.Errorf("error should explain why, got: %v", err)
	}
}

// The builder is published for amd64 only, so the platform must always be
// pinned. Letting it default would silently produce an arm64 build request
// the builder cannot satisfy.
func TestPlatformIsAlwaysPinned(t *testing.T) {
	t.Parallel()
	args, err := Args(Request{Source: "./src", Image: "reg:5000/app:v1", Registry: "reg:5000"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(args, "--platform", BuilderPlatform) {
		t.Errorf("--platform %s missing from %v", BuilderPlatform, args)
	}
	if !hasFlag(args, "--builder", BuilderImage) {
		t.Errorf("builder is not pinned by digest: %v", args)
	}
	if !strings.Contains(BuilderImage, "@sha256:") {
		t.Error("builder must be pinned by digest, not a tag")
	}
}

func TestFunctionTargetAddsFrameworkEnv(t *testing.T) {
	t.Parallel()
	args, err := Args(Request{
		Source: "./src", Image: "reg:5000/fn:v1", Registry: "reg:5000",
		FunctionTarget: "Hello", Signature: SignatureCloudEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(args, "--env", "GOOGLE_FUNCTION_TARGET=Hello") {
		t.Errorf("function target missing: %v", args)
	}
	if !hasFlag(args, "--env", "GOOGLE_FUNCTION_SIGNATURE_TYPE=cloudevent") {
		t.Errorf("signature type missing: %v", args)
	}
}

// An unset signature defaults to http, matching the Functions Framework.
func TestSignatureDefaultsToHTTP(t *testing.T) {
	t.Parallel()
	args, _ := Args(Request{
		Source: "./src", Image: "reg:5000/fn:v1", Registry: "reg:5000", FunctionTarget: "Hello",
	})
	if !hasFlag(args, "--env", "GOOGLE_FUNCTION_SIGNATURE_TYPE=http") {
		t.Errorf("signature did not default to http: %v", args)
	}
}

// An ordinary application build must not be turned into a function.
func TestNoFunctionEnvWithoutATarget(t *testing.T) {
	t.Parallel()
	args, _ := Args(Request{Source: "./src", Image: "reg:5000/app:v1", Registry: "reg:5000"})
	for _, a := range args {
		if strings.HasPrefix(a, "GOOGLE_FUNCTION_") {
			t.Errorf("function environment leaked into an application build: %v", args)
		}
	}
}

func TestEnvOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	req := Request{
		Source: "./src", Image: "reg:5000/app:v1", Registry: "reg:5000",
		Env: map[string]string{"ZULU": "z", "ALPHA": "a", "MIKE": "m"},
	}
	first, err := Args(req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, _ := Args(req)
		if strings.Join(again, " ") != strings.Join(first, " ") {
			t.Fatal("Args is not deterministic across calls")
		}
	}
	if strings.Index(strings.Join(first, " "), "ALPHA") > strings.Index(strings.Join(first, " "), "ZULU") {
		t.Error("env is not sorted")
	}
}

// The architecture answer must be nuanced rather than a flat refusal: an arm64
// host can build, it just produces an amd64 image.
func TestSupportedHereExplainsArchitecture(t *testing.T) {
	t.Parallel()
	ok, note := SupportedHere()
	if !ok && !strings.Contains(note, "pack is not installed") {
		t.Fatalf("unsupported for an unexpected reason: %q", note)
	}
	if ok && note != "" {
		// On a non-amd64 host the note must say what the developer gets.
		for _, want := range []string{BuilderPlatform, "emulation"} {
			if !strings.Contains(note, want) {
				t.Errorf("note should mention %q, got: %q", want, note)
			}
		}
	}
}

func TestMissingSourceOrImageIsRejected(t *testing.T) {
	t.Parallel()
	if _, err := Args(Request{Image: "reg:5000/a:v1", Registry: "reg:5000"}); err == nil {
		t.Error("missing source was accepted")
	}
	if _, err := Args(Request{Source: "./src", Registry: "reg:5000"}); err == nil {
		t.Error("missing image was accepted")
	}
}

// A PHP function is built with Google's PHP functions builder (#1319): the
// general builder has no PHP Functions Framework buildpack and would build
// a web application that runs index.php without the framework's router. A
// PHP application, and a function in any other language, keep the general
// builder.
func TestPHPFunctionUsesThePHPFunctionsBuilder(t *testing.T) {
	t.Parallel()
	php := filepath.Join("..", "..", "testdata", "function-php")
	node := filepath.Join("..", "..", "testdata", "function-nodejs")
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"php function", Request{Source: php, FunctionTarget: "hello"}, PHPFunctionBuilderImage},
		{"php application", Request{Source: php}, BuilderImage},
		{"nodejs function", Request{Source: node, FunctionTarget: "Hello"}, BuilderImage},
		{"GOOGLE_RUNTIME php function", Request{Source: node, FunctionTarget: "hello",
			Env: map[string]string{EnvRuntime: "php83"}}, PHPFunctionBuilderImage},
	} {
		tc.req.Image, tc.req.Registry = "reg:5000/fn:v1", "reg:5000"
		args, err := Args(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !hasFlag(args, "--builder", tc.want) {
			t.Errorf("%s: --builder %s missing from %v", tc.name, tc.want, args)
		}
	}
	if !strings.Contains(PHPFunctionBuilderImage, "@sha256:") {
		t.Error("the PHP functions builder must be pinned by digest, not a tag")
	}
}
