package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// fakeDocker answers `image inspect` from present and records every call.
func fakeDocker(t *testing.T, present map[string]bool, pullOK bool) *[]string {
	t.Helper()
	var calls []string
	old := dockerRun
	dockerRun = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "image":
			if present[args[2]] {
				return "[]", nil
			}
			return "", errors.New("No such image")
		case "pull":
			if pullOK {
				return "pulled", nil
			}
			return "denied", errors.New("pull failed")
		}
		return "", errors.New("unexpected")
	}
	t.Cleanup(func() { dockerRun = old })
	return &calls
}

// `up --local-ai-model` fails at startup, before anything is bound, when
// the model or the runtime is missing, naming which (#602).
func TestLocalAIPreflight(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "gemma.litertlm")
	if err := os.WriteFile(model, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := func(path, image string) config.Config {
		var c config.Config
		c.BindAddress = "127.0.0.1"
		c.LocalAI.ModelPath, c.LocalAI.Image = path, image
		return c
	}

	fakeDocker(t, map[string]bool{config.DefaultLocalAIImage: true}, false)
	if _, err := buildLocalAI(cfg(filepath.Join(dir, "missing.litertlm"), "")); err == nil || !strings.Contains(err.Error(), "missing.litertlm") {
		t.Errorf("a missing model = %v; want an error naming it", err)
	}
	if _, err := buildLocalAI(cfg(dir, "")); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Errorf("a directory as the model = %v; want an error", err)
	}
	if srv, err := buildLocalAI(cfg(model, "")); err != nil || srv == nil {
		t.Errorf("model and default image present = %v, %v", srv, err)
	}

	calls := fakeDocker(t, nil, false)
	_, err := buildLocalAI(cfg(model, ""))
	if err == nil || !strings.Contains(err.Error(), config.DefaultLocalAIImage) || !strings.Contains(err.Error(), "make litert-lm") {
		t.Errorf("the default image missing = %v; want it named with how to build it", err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "pull") {
			t.Errorf("the unpublished default image was pulled: %s", c)
		}
	}

	calls = fakeDocker(t, nil, true)
	if _, err := buildLocalAI(cfg(model, "ghcr.io/example/litert-lm:1")); err != nil {
		t.Errorf("a pullable custom image = %v", err)
	}
	if len(*calls) != 2 || !strings.HasPrefix((*calls)[1], "pull ghcr.io/example/litert-lm:1") {
		t.Errorf("docker calls %v; want inspect then pull", *calls)
	}
	fakeDocker(t, nil, false)
	if _, err := buildLocalAI(cfg(model, "ghcr.io/example/litert-lm:1")); err == nil || !strings.Contains(err.Error(), "could not be pulled") {
		t.Errorf("an unpullable custom image = %v", err)
	}
}
