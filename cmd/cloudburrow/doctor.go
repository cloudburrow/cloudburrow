package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
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
