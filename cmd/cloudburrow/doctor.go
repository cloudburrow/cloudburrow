package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/prefetch"
)

// errBlocking signals that doctor found a problem that stops `up`, so the
// command exits non-zero. It stays terse because the report above it has
// already named every problem and its fix.
var errBlocking = errors.New("blocking problems found; see the report above")

// runDoctor diagnoses the workstation against the configuration that `up`
// would use.
//
// It takes the same flags as every other command so that the ports it checks
// are the ports `up` would actually bind. Checking the defaults while the
// developer runs with overrides would pass and then fail.
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "cloudburrow doctor: checking prerequisites for instance %q\n\n", cfg.Name)

	report := doctor.Run(ctx, doctor.RealEnv(), doctorOptions(cfg))
	report.Results = append(report.Results, offlineCacheResult(ctx, cfg, prefetch.ExecRunner{}, report))
	report.Write(stdout)

	if report.Blocking() {
		return errBlocking
	}
	return nil
}

// doctorOptions are the checks `doctor` runs for an instance, shared with
// `diagnose` so a bundle reports what doctor would, and with `up`, which
// checks the same ports before it creates anything.
//
// The ports are every endpoint the configuration names, less those `up`
// would not bind: a disabled service's, and the local generation endpoint's
// without a model. A hand-kept list here once checked eight of them, so a
// taken console or Resource Manager port passed doctor and failed `up`.
func doctorOptions(cfg config.Config) doctor.Options {
	opts := doctor.Options{
		BindAddress: cfg.BindAddress,
		// The whole set `up` checks and binds, so a port doctor passes is
		// one `up` will not trip over.
		Ports: hostPorts(cfg),
		// The ingress port is published by the cluster, not bound by this
		// process, so `0` means "publish nothing" rather than "pick one".
		Fixed: map[string]bool{"ingress": true},
	}
	return opts
}

// offlineCacheResult reports whether `cloudburrow prefetch` has stored what
// `up` with this configuration downloads (#604). An incomplete cache is only
// worth a warning when a host it would be downloaded from did not answer.
func offlineCacheResult(ctx context.Context, cfg config.Config, r prefetch.Runner, report doctor.Report) doctor.Result {
	cache := prefetch.Cache{Dir: prefetch.CacheDir(cfg.StateDir)}
	arts := cache.Artifacts(offlinePlan(ctx, cfg, r))
	cached := 0
	for _, a := range arts {
		if cache.Has(a) {
			cached++
		}
	}
	first, missing := cache.FirstMissing(arts)
	if !missing {
		return doctor.Result{Name: "offline cache", Level: doctor.LevelOK,
			Detail: fmt.Sprintf("complete: %d artifacts in %s; `up --offline` can start", len(arts), cache.Dir)}
	}
	res := doctor.Result{Name: "offline cache", Level: doctor.LevelOK,
		Detail: fmt.Sprintf("%d of %d artifacts cached in %s; first missing: %s %s", cached, len(arts), cache.Dir, first.What, first.Ref),
		Remedy: "run `cloudburrow prefetch` with the same flags where the network is reachable"}
	for _, r := range report.Results {
		if strings.HasPrefix(r.Name, "reach ") && r.Level != doctor.LevelOK {
			res.Level = doctor.LevelWarn
		}
	}
	return res
}
