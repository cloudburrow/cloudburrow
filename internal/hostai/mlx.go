package hostai

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MLXLock is mlx-lm's server and its dependencies, every one pinned with
// its hashes (mlx==0.32.4, mlx-lm==0.32.2, MIT; dependencies.json
// localai.hostMLX): a universal lock, so it installs on any Python from
// 3.11 the packages have wheels for.
//
//go:embed mlx-lock.txt
var MLXLock string

// pythonVersions are the Pythons the venv is made with, in order of
// preference: those mlx 0.32.4 publishes macOS wheels for.
var pythonVersions = []string{"3.12", "3.13", "3.11"}

// officialPython reports whether a Python's real path is an official
// python.org framework build or Homebrew's: the venv is never made from
// some other interpreter that happens to be on PATH.
func officialPython(real string) bool {
	for _, p := range []string{"/Library/Frameworks/Python.framework/", "/opt/homebrew/Cellar/", "/opt/homebrew/opt/",
		"/usr/local/Cellar/", "/usr/local/opt/"} {
		if strings.HasPrefix(real, p) {
			return true
		}
	}
	return false
}

func lockID() string {
	h := sha256.Sum256([]byte(MLXLock))
	return hex.EncodeToString(h[:6])
}

// MLXPython returns the Python of a venv that has mlx-lm installed from
// MLXLock, making it the first time under Dir/mlx-<lock>: with uv when it
// is on PATH (uv venv, uv pip install --require-hashes), or else with
// python3.12, 3.13 or 3.11 from python.org or Homebrew (python -m venv,
// pip install --require-hashes). The venv is complete only once
// everything installed; a half-made one is made again.
func (in *Installer) MLXPython(ctx context.Context, progress func(string)) (string, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	dir := filepath.Join(in.Dir, "mlx-"+lockID())
	venv := filepath.Join(dir, "venv")
	py := filepath.Join(venv, "bin", "python")
	done := filepath.Join(dir, ".complete")
	if _, err := os.Stat(done); err == nil {
		return py, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	_ = os.RemoveAll(venv)
	lock := filepath.Join(dir, "requirements.txt")
	if err := os.WriteFile(lock, []byte(MLXLock), 0o644); err != nil {
		return "", err
	}
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if uv, err := in.lookPath("uv"); err == nil {
		say("Creating the MLX runtime's Python environment with uv")
		var made error
		for _, v := range pythonVersions {
			if _, made = in.run(ctx, uv, "venv", "--quiet", "--python", v, venv); made == nil {
				break
			}
		}
		if made != nil {
			return "", fmt.Errorf("create the MLX runtime's venv with uv: %w", made)
		}
		say("Installing mlx-lm (pinned, hash-checked) with uv")
		if _, err := in.run(ctx, uv, "pip", "install", "--quiet", "--python", py, "--require-hashes", "--no-deps", "-r", lock); err != nil {
			return "", fmt.Errorf("install mlx-lm: %w", err)
		}
	} else {
		base, err := in.findPython()
		if err != nil {
			return "", err
		}
		say("Creating the MLX runtime's Python environment with " + base)
		if _, err := in.run(ctx, base, "-m", "venv", venv); err != nil {
			return "", fmt.Errorf("create the MLX runtime's venv: %w", err)
		}
		say("Installing mlx-lm (pinned, hash-checked) with pip")
		if _, err := in.run(ctx, py, "-m", "pip", "install", "--quiet", "--require-hashes", "--no-deps", "-r", lock); err != nil {
			return "", fmt.Errorf("install mlx-lm: %w", err)
		}
	}
	// MLX itself says whether it sees the GPU.
	out, err := in.run(ctx, py, "-c", "import mlx.core as mx, mlx_lm; print(mx.metal.is_available())")
	if err != nil {
		return "", fmt.Errorf("the MLX runtime does not start: %w", err)
	}
	if strings.TrimSpace(out) != "True" {
		return "", fmt.Errorf("%w: MLX reports no Metal device", ErrNoMetal)
	}
	if err := os.WriteFile(done, []byte(lockID()+"\n"), 0o644); err != nil {
		return "", err
	}
	return py, nil
}

// findPython is the first python3.12, 3.13 or 3.11 on PATH that is
// python.org's or Homebrew's.
func (in *Installer) findPython() (string, error) {
	var seen []string
	for _, v := range pythonVersions {
		p, err := in.lookPath("python" + v)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		if officialPython(real) {
			return p, nil
		}
		seen = append(seen, real)
	}
	why := "none is on PATH"
	if len(seen) > 0 {
		why = "found only " + strings.Join(seen, ", ") + ", which are not python.org's or Homebrew's"
	}
	return "", fmt.Errorf("the MLX runtime needs uv, or Python 3.12, 3.13 or 3.11 from python.org or Homebrew (%s). "+
		"Install uv (docs.astral.sh/uv) or `brew install python@3.12`, then deploy again", why)
}
