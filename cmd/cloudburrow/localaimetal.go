package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostai"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// LiteRT-LM generation on the Apple GPU (#2159).

// liteRTLMRunner and probeGenerator are hostai.LiteRTLMRunner and
// vertexai.Probe, replaced in tests.
var (
	liteRTLMRunner = hostai.LiteRTLMRunner
	probeGenerator = vertexai.Probe
)

// probeTimeout bounds the startup probe: a first GPU load of a
// multi-gigabyte model compiles its shaders.
const probeTimeout = 5 * time.Minute

// hostGenerator is Gemini's local generation as a host process on the Apple
// GPU when the accelerator is metal (localAI.accelerator, the default on a
// Mac with Apple silicon), or nil for the runtime container on CPU.
//
// The macOS cloudburrow_generate runs with LiteRT-LM's GPU backend after a
// one-token probe: when the GPU delegate fails, the reason is logged and
// the same runner is used on CPU, still a host process. When no macOS
// runner is installed, the reason is logged and nil is returned.
func hostGenerator(cfg config.Config, modelPath, modelID string, log io.Writer) *vertexai.ExecGenerator {
	switch cfg.LocalAI.Accelerator {
	case "cpu":
		return nil
	case "":
		if !config.MetalAvailable() {
			return nil
		}
	}
	runner, err := liteRTLMRunner()
	if errors.Is(err, hostai.ErrNoLiteRTLMRunner) {
		// One line that says what is lost and how to get it back: an
		// install that skipped the runner otherwise looks like a slow GPU.
		fmt.Fprintf(log, "local AI: warning: the Apple GPU (Metal) runner is not installed, so generation runs on CPU in the runtime container; "+
			"reinstall with scripts/install.sh or Homebrew, or set %s (%v)\n", hostai.LiteRTLMDirEnv, err)
		return nil
	}
	if err != nil {
		fmt.Fprintf(log, "local AI: generating on CPU in the runtime container: %v\n", err)
		return nil
	}
	gpu := vertexai.NewHostGenerator(runner, modelPath, modelID, vertexai.BackendGPU)
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	err = probeGenerator(ctx, gpu)
	if err == nil {
		return gpu
	}
	fmt.Fprintf(log, "local AI: the Apple GPU (LiteRT-LM's GPU delegate) failed for %s, generating on CPU as a host process instead: %v\n", modelID, err)
	return vertexai.NewHostGenerator(runner, modelPath, modelID, vertexai.BackendCPU)
}
