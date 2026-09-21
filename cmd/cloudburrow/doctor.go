package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/doctor"
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

	report := doctor.Run(ctx, doctor.RealEnv(), doctor.Options{
		BindAddress: cfg.BindAddress,
		Ports: map[string]int{
			"control": cfg.Endpoints.Control,
			"storage": cfg.Endpoints.Storage,
			"pubsub":  cfg.Endpoints.PubSub,
			"tasks":   cfg.Endpoints.Tasks,
			"run":     cfg.Endpoints.Run,
			"ingress": cfg.Endpoints.Ingress,
		},
		// The ingress port is published by the cluster, not bound by this
		// process, so `0` means "publish nothing" rather than "pick one".
		Fixed: map[string]bool{"ingress": true},
	})
	report.Write(stdout)

	if report.Blocking() {
		return errBlocking
	}
	return nil
}
