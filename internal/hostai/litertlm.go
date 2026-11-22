package hostai

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LiteRT-LM generation on the Apple GPU (#2159).
//
// Gemini's local generation runs cloudburrow_generate, CloudBurrow's
// patched LiteRT-LM runner (deploy/litert-lm/runner). In the Linux runtime
// image it is CPU-only on a Mac: Docker Desktop's VM has no Metal. The same
// runner, built for macOS arm64 from the same commit and patches
// (deploy/litert-lm/build-macos.sh), runs on the host with LiteRT-LM's
// --backend=gpu, which drives the GPU through WebGPU on Dawn's Metal
// backend. The release's darwin/arm64 archive carries it in litert-lm/,
// beside the cloudburrow binary, with the prebuilt dylibs it loads.

// LiteRTLMDirEnv names a directory holding cloudburrow_generate and its
// dylibs, in place of the release archive's litert-lm/ (a checkout's
// build-macos.sh output, unpacked).
const LiteRTLMDirEnv = "CLOUDBURROW_LITERT_LM_DIR"

// LiteRTLMRunnerName is the runner's file name.
const LiteRTLMRunnerName = "cloudburrow_generate"

// ErrNoLiteRTLMRunner means no macOS build of cloudburrow_generate is
// installed.
var ErrNoLiteRTLMRunner = errors.New("no macOS build of cloudburrow_generate is installed")

// executable is os.Executable, replaced in tests.
var executable = os.Executable

// LiteRTLMRunner finds the macOS arm64 cloudburrow_generate: in
// $CLOUDBURROW_LITERT_LM_DIR when it is set (and only there), else in
// litert-lm/ beside the running cloudburrow (the release archive's
// layout), or ../libexec/cloudburrow/litert-lm/ from it (a Homebrew
// install, whose bin/ holds a link to the CLI).
func LiteRTLMRunner() (string, error) {
	if ok, why := Metal(); !ok {
		return "", fmt.Errorf("%w: %s", ErrNoMetal, why)
	}
	var dirs []string
	if d := os.Getenv(LiteRTLMDirEnv); d != "" {
		dirs = []string{d}
	} else if exe, err := executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		bin := filepath.Dir(exe)
		dirs = []string{filepath.Join(bin, "litert-lm"), filepath.Join(bin, "..", "libexec", "cloudburrow", "litert-lm")}
	}
	for _, d := range dirs {
		p := filepath.Join(d, LiteRTLMRunnerName)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return filepath.Clean(p), nil
		}
	}
	return "", fmt.Errorf("%w (looked in %s): the darwin/arm64 release archive carries it in litert-lm/; "+
		"in a checkout, build it with deploy/litert-lm/build-macos.sh and set %s to the unpacked directory",
		ErrNoLiteRTLMRunner, strings.Join(dirs, ", "), LiteRTLMDirEnv)
}
