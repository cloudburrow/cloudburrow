// Package hostai runs local AI model servers as host processes, so they
// can use the Apple GPU through Metal (#2155).
//
// Docker Desktop's Linux VM gives containers no access to the Apple GPU,
// so on macOS every runtime CloudBurrow runs in the kind cluster is
// CPU-only. On darwin/arm64 this package instead runs, as processes of the
// developer's own machine:
//
//   - llama.cpp's llama-server, from the official macOS arm64 release
//     archive (MIT), pinned by SHA-256 (LlamaCppArchive; dependencies.json
//     localai.hostLlamaCpp), which offloads every layer to the GPU;
//   - mlx-lm's server (MIT) on MLX's Metal backend, in a virtual
//     environment CloudBurrow creates from a hash-locked requirements file
//     (mlx-lock.txt), with uv when it is on PATH, or else with an official
//     python.org or Homebrew Python found on PATH.
//
// Both are kept under Dir (CacheDir: the user's cache directory), never
// installed system-wide. Each server is supervised (Process): health
// checks, a restart with back-off when it exits, its output as log lines,
// and a stop that ends its whole process group. A pid file (Pids) lets a
// later run end any server an abruptly killed CLI left behind.
package hostai

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DirEnv overrides CacheDir.
const DirEnv = "CLOUDBURROW_HOSTAI_DIR"

// CacheDir is where the host runtimes are installed: $CLOUDBURROW_HOSTAI_DIR,
// or cloudburrow/hostai in the user's cache directory
// (~/Library/Caches on macOS), shared by every instance.
func CacheDir() (string, error) {
	if d := os.Getenv(DirEnv); d != "" {
		return filepath.Abs(d)
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "cloudburrow", "hostai"), nil
}

// ErrNoMetal means the Apple GPU cannot be used on this machine.
var ErrNoMetal = errors.New("the Apple GPU (Metal) is not available")

// goos and goarch are runtime's, replaced in tests.
var goos, goarch = runtime.GOOS, runtime.GOARCH

// Metal reports whether model servers can run on the Apple GPU here, and
// why not: Metal is used on macOS on Apple silicon (darwin/arm64), where
// every Mac has an Apple GPU and llama.cpp's and MLX's macOS builds
// target it. An Intel Mac's GPU is not one llama.cpp's arm64 build or MLX
// supports, and Linux and Windows have no Metal.
func Metal() (bool, string) {
	switch {
	case goos != "darwin":
		return false, "Metal is Apple's GPU API, on macOS only; this machine runs " + goos
	case goarch != "arm64":
		return false, "the Apple GPU is used on Apple silicon (arm64) Macs; this Mac is " + goarch
	}
	return true, ""
}

// Device names the Apple GPU, its chip as sysctl reports it (for example
// "Apple M4 Max"), or "Apple GPU" when that cannot be read.
func Device() string {
	if ok, _ := Metal(); !ok {
		return ""
	}
	out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if s := strings.TrimSpace(string(out)); err == nil && strings.HasPrefix(s, "Apple ") {
		return s
	}
	return "Apple GPU"
}
