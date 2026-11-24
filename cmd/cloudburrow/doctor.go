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
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
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

	env := doctor.RealEnv()
	var remote []doctor.Result
	if _, ok, err := computeTarget(cfg); err != nil {
		remote = append(remote, doctor.Result{Name: "remote host", Level: doctor.LevelFail, Detail: err.Error(),
			Remedy: "fix --compute or the instance's " + remotecompute.RecordFile})
	} else if ok {
		// The compute host (#2209): Docker, CPU, memory and disk are the
		// host's, measured over the connection; the ports and binaries
		// are still this machine's.
		compute, err := connectComputeWith(ctx, cfg, io.Discard, false, true)
		if err != nil {
			remote = append(remote, doctor.Result{Name: "remote host", Level: doctor.LevelFail, Detail: err.Error(),
				Remedy: "check that `ssh` reaches the host with your agent and config, and that it runs Linux with Docker"})
		} else {
			defer compute.Close()
			_, checks := compute.session.Checks(ctx)
			remote = append(remote, doctor.Result{Name: "remote compute", Level: doctor.LevelOK,
				Detail: "the instance's Docker runs on " + compute.record.Target.String() + " over SSH; the CLI and console run here"})
			for _, c := range checks {
				r := doctor.Result{Name: c.Name, Level: doctor.LevelOK, Detail: c.Detail, Remedy: c.Remedy}
				switch {
				case !c.OK:
					r.Level = doctor.LevelFail
				case c.Warn:
					r.Level = doctor.LevelWarn
				default:
					r.Remedy = ""
				}
				remote = append(remote, r)
			}
			s := compute.session
			env.GOOS = "linux"
			env.DiskFree = func(path string) (uint64, string, error) { return s.DiskFree(ctx, path) }
		}
	}
	report := doctor.Run(ctx, env, doctorOptions(cfg))
	report.Results = append(remote, report.Results...)
	arch := daemonArch(ctx, prefetch.ExecRunner{})
	report.Results = append(report.Results, storageEmbedResult(cfg, arch), bigQueryEmbedResult(cfg, arch))
	report.Results = append(report.Results, offlineCacheResult(ctx, cfg, prefetch.ExecRunner{}, report))
	report.Results = append(report.Results, ephemeralPortsResult(cfg))
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
		detail := fmt.Sprintf("complete: %d artifacts in %s; `up --offline` can start", len(arts), cache.Dir)
		for _, a := range arts {
			if a.Optional && !cache.Has(a) {
				// Named, since `up --offline` then has no terminal (#824).
				detail = fmt.Sprintf("%d of %d artifacts cached in %s; `up --offline` can start, but the optional %s is not cached",
					cached, len(arts), cache.Dir, a.What)
				break
			}
		}
		return doctor.Result{Name: "offline cache", Level: doctor.LevelOK, Detail: detail}
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
