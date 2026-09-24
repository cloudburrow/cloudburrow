package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

// eventLog records what happened in what order, across components and
// scripts.
type eventLog struct {
	mu     sync.Mutex
	events []string
	file   string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	e.events = append(e.events, s)
	e.mu.Unlock()
}

// all merges the components' events with the lines scripts appended.
func (e *eventLog) all(t *testing.T) []string {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := os.ReadFile(e.file)
	var out []string
	out = append(out, e.events...)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// fakeCluster stands in for what the hooks depend on.
type orderedComponent struct {
	name string
	log  *eventLog
}

func (c orderedComponent) Name() string                { return c.name }
func (c orderedComponent) Start(context.Context) error { c.log.add(c.name + " started"); return nil }
func (c orderedComponent) Stop(context.Context) error {
	// Written to the same file the scripts write to, so one sequence holds
	// both, in the order they happened.
	f, _ := os.OpenFile(c.log.file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	_, _ = f.WriteString(c.name + " stopped\n")
	_ = f.Close()
	return nil
}

func hookScript(t *testing.T, dir, stage, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, stage), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stage, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestHooksRunAfterStartupAndBeforeShutdown: ready.d runs once every other
// component has started and sees the instance's environment; shutdown.d runs
// before the cluster is stopped; init is a readiness component.
func TestHooksRunAfterStartupAndBeforeShutdown(t *testing.T) {
	dir := t.TempDir()
	log := &eventLog{file: filepath.Join(t.TempDir(), "events")}
	hookScript(t, dir, "ready.d", "10-ready.sh", `echo "ready hook saw $STORAGE_EMULATOR_HOST" >> `+log.file)
	hookScript(t, dir, "shutdown.d", "10-bye.sh", `echo "shutdown hook ran" >> `+log.file)

	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage}
	cfg.HooksDir = dir
	cfg.HookTimeout = config.Duration(10 * time.Second)
	live := func() map[string]string { return map[string]string{"storage": "127.0.0.1:43210"} }

	coord := lifecycle.New(10 * time.Second)
	coord.Register(orderedComponent{"cluster", log}, orderedComponent{"components", log},
		&hooksComponent{cfg: cfg, env: hookEnvironment(cfg, live, ""), out: &strings.Builder{}})
	if err := coord.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	names, ready := coord.SortedReady()
	if !strings.Contains(strings.Join(names, ","), "init") || !ready["init"] {
		t.Errorf("init is not a ready component: %v %v", names, ready)
	}
	if err := coord.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(log.all(t), "\n")
	want := strings.Join([]string{
		"cluster started", "components started",
		"ready hook saw http://127.0.0.1:43210",
		"shutdown hook ran", "components stopped", "cluster stopped",
	}, "\n")
	if got != want {
		t.Errorf("order:\n%s\nwant:\n%s", got, want)
	}
}

// A hooks directory that does not exist changes nothing.
func TestNoHooksDirectoryIsNoHooks(t *testing.T) {
	cfg := config.Default()
	cfg.HooksDir = filepath.Join(t.TempDir(), "absent")
	var out strings.Builder
	h := &hooksComponent{cfg: cfg, env: func() []string { return nil }, out: &out}
	if err := h.Start(context.Background()); err != nil || out.Len() != 0 {
		t.Errorf("an absent hooks directory printed %q (%v)", out.String(), err)
	}
}
