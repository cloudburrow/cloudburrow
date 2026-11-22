package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostai"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

func fakeHostAI(t *testing.T, runnerErr, probeErr error) *[]string {
	t.Helper()
	var probed []string
	or, op := liteRTLMRunner, probeGenerator
	t.Cleanup(func() { liteRTLMRunner, probeGenerator = or, op })
	liteRTLMRunner = func() (string, error) { return "/opt/litert-lm/cloudburrow_generate", runnerErr }
	probeGenerator = func(_ context.Context, g vertexai.Generator) error {
		probed = append(probed, g.(*vertexai.ExecGenerator).Backend)
		return probeErr
	}
	return &probed
}

func accelCfg(a string) config.Config {
	var c config.Config
	c.LocalAI.Accelerator = a
	return c
}

// Metal runs the host runner on the GPU once its probe passes (#2159).
func TestHostGeneratorOnMetal(t *testing.T) {
	probed := fakeHostAI(t, nil, nil)
	var log strings.Builder
	g := hostGenerator(accelCfg("metal"), "/m.litertlm", "m", &log)
	if g == nil || g.Backend != vertexai.BackendGPU || !g.Host || g.Command != "/opt/litert-lm/cloudburrow_generate" {
		t.Fatalf("hostGenerator = %+v", g)
	}
	if len(*probed) != 1 || log.Len() != 0 {
		t.Errorf("probed %v, logged %q", *probed, log.String())
	}
}

// A GPU delegate that fails falls back to the CPU, with the reason logged.
func TestHostGeneratorFallsBackToCPUWithAReason(t *testing.T) {
	fakeHostAI(t, nil, errors.New("Failed to create the GPU delegate"))
	var log strings.Builder
	g := hostGenerator(accelCfg("metal"), "/m.litertlm", "m", &log)
	if g == nil || g.Backend != vertexai.BackendCPU || !g.Host {
		t.Fatalf("hostGenerator = %+v; want the host runner on CPU", g)
	}
	if !strings.Contains(log.String(), "generating on CPU") || !strings.Contains(log.String(), "Failed to create the GPU delegate") {
		t.Errorf("log = %q; want the reason", log.String())
	}
}

// No macOS runner: the container, with the reason logged; cpu: the
// container, with nothing probed.
func TestHostGeneratorUsesTheContainer(t *testing.T) {
	probed := fakeHostAI(t, hostai.ErrNoLiteRTLMRunner, nil)
	var log strings.Builder
	if g := hostGenerator(accelCfg("metal"), "/m", "m", &log); g != nil {
		t.Errorf("hostGenerator without a runner = %+v", g)
	}
	if !strings.Contains(log.String(), "runtime container") {
		t.Errorf("log = %q", log.String())
	}
	probed = fakeHostAI(t, nil, nil)
	if g := hostGenerator(accelCfg("cpu"), "/m", "m", &log); g != nil || len(*probed) != 0 {
		t.Errorf("cpu: hostGenerator = %+v, probed %v", g, *probed)
	}
}
