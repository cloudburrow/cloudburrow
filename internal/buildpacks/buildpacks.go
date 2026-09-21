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
	// Env are additional build environment variables.
	Env map[string]string
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
		"--builder", BuilderImage,
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
		args = append(args, "--env", k+"="+req.Env[k])
	}
	return args, nil
}

// Build runs the build.
func (b *Builder) Build(ctx context.Context, req Request) error {
	if ok, note := SupportedHere(); !ok {
		return fmt.Errorf("%w: %s", ErrPackMissing, note)
	}
	args, err := Args(req)
	if err != nil {
		return err
	}
	r := b.Runner
	if r == nil {
		r = ExecRunner{}
	}
	out, err := r.Run(ctx, "pack", args...)
	if err != nil {
		// Surface the tail of the build log: a buildpack failure is
		// self-explanatory, but only if the caller can see it.
		return fmt.Errorf("%w: %v\n%s", ErrBuildFailed, err, tail(out, 25))
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
