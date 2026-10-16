// Package buildpacks turns source into a runnable image using Google
// Buildpacks and the standard pack tool.
//
// This is an optional source-build convenience. It implies no Cloud Build API
// parity: nothing here serves a Google API, and no build is addressable as a
// Cloud Build resource.
package buildpacks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

// Pinned build inputs. Recorded in dependencies.json.
const (
	// BuilderImage is Google's buildpacks builder, pinned by digest.
	//
	// It is published for linux/amd64 only, which is the constraint that
	// shapes everything below.
	BuilderImage = "gcr.io/buildpacks/builder@sha256:12b6bbdd6890685a2a75ac59c2abe7868432bde292275ea92b20711c2432b429"
	// PHPFunctionBuilderImage is Google's PHP builder for Cloud Run
	// functions, pinned by digest, which builds a PHP function (#1319).
	//
	// The general builder has no PHP Functions Framework buildpack: given
	// GOOGLE_FUNCTION_TARGET it still builds a PHP web application, nginx and
	// php-fpm running index.php directly, so the framework's router and
	// autoloader never load. Google builds PHP functions with this
	// per-language builder ("App Engine and Cloud Function Builders" in
	// github.com/GoogleCloudPlatform/buildpacks), whose google.php.functions-
	// framework and google.php.cloudfunctions buildpacks serve
	// vendor/google/cloud-functions-framework/router.php. Also amd64 only.
	PHPFunctionBuilderImage = "us-docker.pkg.dev/serverless-runtimes/google-22-full/builder/php@sha256:f516aa793106906d32a8eb800f733becbc3456cefbbdaa732fda4bc273282a73"
	// BuilderPlatform is the only platform the builder provides.
	BuilderPlatform = "linux/amd64"
)

// Errors callers are expected to distinguish.
var (
	// ErrPackMissing means the pack CLI is not installed.
	ErrPackMissing = errors.New("pack is not installed")
	// ErrRegistryRequired means no registry was supplied.
	ErrRegistryRequired = errors.New("a registry is required")
	// ErrBuildFailed means the build itself failed.
	ErrBuildFailed = errors.New("build failed")
)

// Signature is a Functions Framework signature type.
type Signature string

const (
	SignatureHTTP       Signature = "http"
	SignatureCloudEvent Signature = "cloudevent"
)

// Request describes one build.
type Request struct {
	// Source is the directory to build.
	Source string
	// Image is the fully qualified target, including the registry.
	Image string
	// Registry is the host:port of a registry the build can push to.
	//
	// A registry is required rather than optional. pack's export to the Docker
	// daemon fails on an arm64 host with this amd64 builder, so publishing is
	// the only path that works everywhere — see SupportedHere.
	Registry string
	// Network is a Docker network the build container shares with the
	// registry, so the registry is resolvable from inside the lifecycle.
	Network string
	// Insecure allows a plain-HTTP registry, which a local one is.
	Insecure bool
	// FunctionTarget, when set, builds a Functions Framework function with
	// this exported name rather than an ordinary application.
	FunctionTarget string
	// Signature selects the function signature. Ignored without a target.
	Signature Signature
	// Env are additional build environment variables. GOOGLE_RUNTIME here
	// forces the runtime, as on Google; without it the builder detects one
	// from the source (see Detect).
	Env map[string]string
	// SecretEnv are build environment variables whose values are secrets
	// (#1449): pack gets each as --env NAME and reads its value from its
	// own environment, so no value is on its command line.
	SecretEnv map[string]string
}

// SupportedHere reports whether buildpacks can run on this machine, and why
// not when they cannot.
//
// The architecture answer is deliberately nuanced: an arm64 host *can* build,
// because Docker emulates amd64, but it produces an amd64 image. Reporting
// that plainly is better than either refusing outright or letting a developer
// discover it when the image will not schedule.
func SupportedHere() (ok bool, note string) {
	if _, err := exec.LookPath("pack"); err != nil {
		return false, "pack is not installed; see https://buildpacks.io/docs/install-pack/"
	}
	if runtime.GOARCH != "amd64" {
		return true, fmt.Sprintf(
			"the Google builder is published for %s only, so on %s it runs under emulation "+
				"and produces an %s image; that image needs an %s-capable node to run",
			BuilderPlatform, runtime.GOARCH, BuilderPlatform, BuilderPlatform)
	}
	return true, ""
}

// Runner executes an external command. Injected for testing.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the real runner.
type ExecRunner struct{}

// EnvRunner is a Runner that can add variables to the command's
// environment, which a build with SecretEnv needs.
type EnvRunner interface {
	RunEnv(ctx context.Context, env []string, name string, args ...string) (string, error)
}

// RunEnv runs the command with env added to this process's environment.
func (ExecRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Builder builds images from source.
type Builder struct {
	Runner Runner
}

// Args renders the pack command line for a request.
//
// Separated from Build so the flags can be asserted without running a build,
// which takes minutes.
func Args(req Request) ([]string, error) {
	if req.Source == "" {
		return nil, fmt.Errorf("%w: source directory is required", ErrBuildFailed)
	}
	if req.Image == "" {
		return nil, fmt.Errorf("%w: target image is required", ErrBuildFailed)
	}
	if req.Registry == "" {
		return nil, fmt.Errorf("%w: pack cannot export to the Docker daemon with this builder on every host, so a registry must be supplied", ErrRegistryRequired)
	}

	args := []string{
		"build", req.Image,
		"--publish",
		"--platform", BuilderPlatform,
		"--path", req.Source,
		"--builder", builderFor(req),
	}
	if req.Network != "" {
		args = append(args, "--network", req.Network)
	}
	if req.Insecure {
		args = append(args, "--insecure-registry", req.Registry)
	}
	if req.FunctionTarget != "" {
		sig := req.Signature
		if sig == "" {
			sig = SignatureHTTP
		}
		args = append(args,
			"--env", "GOOGLE_FUNCTION_TARGET="+req.FunctionTarget,
			"--env", "GOOGLE_FUNCTION_SIGNATURE_TYPE="+string(sig))
	}
	// Deterministic order so the command is reproducible and testable.
	for _, k := range sortedKeys(req.Env) {
		if _, secret := req.SecretEnv[k]; !secret {
			args = append(args, "--env", k+"="+req.Env[k])
		}
	}
	for _, k := range sortedKeys(req.SecretEnv) {
		args = append(args, "--env", k)
	}
	return args, nil
}

// builderFor picks the builder: the PHP functions builder for a PHP function,
// the general builder for everything else. A source Detect cannot read gets
// the general builder; Build refuses it with Detect's reason.
func builderFor(req Request) string {
	if req.FunctionTarget == "" {
		return BuilderImage
	}
	if det, err := Detect(req.Source, req.Env); err == nil && det.Runtime == "php" {
		return PHPFunctionBuilderImage
	}
	return BuilderImage
}

// Build runs the build.
func (b *Builder) Build(ctx context.Context, req Request) error {
	args, err := Args(req)
	if err != nil {
		return err
	}
	// The runtime is the builder's to choose (Detect); a source it would
	// refuse for want of any language is refused here, before minutes of
	// pulling, with the reason.
	det, err := Detect(req.Source, req.Env)
	if err != nil {
		if errors.Is(err, ErrBuildFailed) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrBuildFailed, err)
	}
	if ok, note := SupportedHere(); !ok {
		return fmt.Errorf("%w: %s", ErrPackMissing, note)
	}
	r := b.Runner
	if r == nil {
		r = ExecRunner{}
	}
	var out string
	if len(req.SecretEnv) > 0 {
		er, ok := r.(EnvRunner)
		if !ok {
			return fmt.Errorf("%w: this runner cannot pass secret build variables", ErrBuildFailed)
		}
		env := make([]string, 0, len(req.SecretEnv))
		for _, k := range sortedKeys(req.SecretEnv) {
			env = append(env, k+"="+req.SecretEnv[k])
		}
		out, err = er.RunEnv(ctx, env, "pack", args...)
	} else {
		out, err = r.Run(ctx, "pack", args...)
	}
	if err != nil {
		// Surface the tail of the build log: a buildpack failure is
		// self-explanatory, but only if the caller can see it.
		return fmt.Errorf("%w: %v (%s)\n%s", ErrBuildFailed, err, det, tail(out, 25))
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) <= lines {
		return strings.Join(parts, "\n")
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}
