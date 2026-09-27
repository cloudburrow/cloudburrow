package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hooks"
)

// hooksComponent runs the instance's lifecycle hooks (#285).
//
// It is registered last, so its Start runs ready.d once every other
// component has started, and /readyz, and with it `wait` and `up --detach`,
// turns green only once the hooks have run. The coordinator stops components
// in reverse, so its Stop runs shutdown.d first, while everything the scripts
// might talk to is still serving. A failed script is reported, not fatal: the
// instance is as usable as it was, and the failure is in `status`.
type hooksComponent struct {
	cfg     config.Config
	env     func() []string
	out     io.Writer
	runtime *runtimeFile
}

func (h *hooksComponent) Name() string { return "init" }

func (h *hooksComponent) Start(ctx context.Context) error {
	h.run(ctx, hooks.Ready)
	return nil
}

func (h *hooksComponent) Stop(ctx context.Context) error {
	h.run(ctx, hooks.Shutdown)
	return nil
}

func (h *hooksComponent) run(ctx context.Context, stage string) {
	names, err := hooks.Scripts(h.cfg.HooksDir, stage)
	if err != nil {
		fmt.Fprintf(h.out, "hooks: cannot read %s: %v\n", h.cfg.HooksDir, err)
		return
	}
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(h.out, "\nrunning %d %s hook(s) from %s\n", len(names), stage, h.cfg.HooksDir)
	res, err := hooks.Run(ctx, h.cfg.HooksDir, stage, h.env(), time.Duration(h.cfg.HookTimeout), h.out)
	if err != nil {
		fmt.Fprintf(h.out, "hooks: %v\n", err)
	}
	if h.runtime != nil {
		_ = h.runtime.RecordHooks(stage, res)
	}
}

// hookEnvBase is what hooks get of `up`'s own environment (#598): enough to
// find programs, a home, a locale and a temporary directory, and nothing a
// developer's shell happens to hold, such as tokens and cloud credentials.
// --hook-env names more.
var hookEnvBase = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TMPDIR", "TERM", "TZ"}

// hookEnvironment is the allow-listed part of `up`'s own environment with
// `cloudburrow env`'s variables for the running instance on top, bound
// ports included. Exec keeps the last of a duplicated key, so a
// STORAGE_EMULATOR_HOST named in --hook-env cannot point a hook anywhere
// else.
func hookEnvironment(cfg config.Config, live func() map[string]string, adcPath string) func() []string {
	return func() []string {
		env := allowedEnv(os.Environ(), cfg.HookEnv)
		for _, v := range envVars(withLivePorts(cfg, live()), cfg.DefaultProject(), adcPath) {
			env = append(env, v.Name+"="+v.Value)
		}
		return env
	}
}

// allowedEnv keeps the entries of environ named by hookEnvBase, an LC_*
// locale variable, or extra.
func allowedEnv(environ, extra []string) []string {
	allowed := map[string]bool{}
	for _, k := range append(append([]string(nil), hookEnvBase...), extra...) {
		allowed[k] = true
	}
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if allowed[k] || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

// printHookResults adds the running instance's hook outcomes to `status`.
// Nothing is printed when no hook ran, so an instance without hooks reads
// exactly as before.
func printHookResults(w io.Writer, cfg config.Config) {
	info, ok := running(cfg)
	if !ok || len(info.Hooks) == 0 {
		return
	}
	fmt.Fprintf(w, "\nhooks (%s):\n", cfg.HooksDir)
	for _, stage := range []string{hooks.Ready, hooks.Shutdown} {
		for _, r := range info.Hooks[stage] {
			state := "ok"
			switch {
			case r.Skipped != "":
				state = "skipped: " + r.Skipped
			case r.Error != "":
				state = "could not run: " + r.Error
			case r.TimedOut:
				state = "killed at its timeout"
			case r.ExitCode != 0:
				state = fmt.Sprintf("failed, exit status %d", r.ExitCode)
			}
			fmt.Fprintf(w, "  %s/%-24s %s\n", stage, r.Name, state)
		}
	}
}
