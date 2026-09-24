package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The startup seed (#286): --seed-file names a /admin/seed document that
// `up` validates before touching any state and applies once the services
// have started, before the instance reports ready.

// planSeedFile reads and validates the seed file against the seeders the
// enabled services have. It creates nothing: the seeders' Validate methods
// need no running service, which is what lets an invalid file stop `up`
// before a cluster is created or a credential is written.
func planSeedFile(cfg config.Config) (*admin.SeedPlan, error) {
	if cfg.SeedFile == "" {
		return nil, nil
	}
	doc, err := os.ReadFile(cfg.SeedFile)
	if err != nil {
		return nil, fmt.Errorf("seed file: %w", err)
	}
	api := admin.NewAPI(nil)
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServiceTasks:
			api.RegisterSeeder(&tasksSeeder{})
		case config.ServiceStorage:
			api.RegisterSeeder(&storageSeeder{})
		case config.ServicePubSub:
			api.RegisterSeeder(&pubsubSeeder{})
		case config.ServiceSecrets:
			api.RegisterSeeder(&secretsSeeder{})
		}
	}
	plan, err := api.PlanSeed(doc, true)
	if err != nil {
		return nil, fmt.Errorf("seed file %s: %w (a component must name an enabled service); nothing was seeded", cfg.SeedFile, err)
	}
	return plan, nil
}

// seedComponent applies the startup seed. It is registered just before the
// hooks, so a ready.d script sees the seeded resources, and /readyz is green
// only once they exist.
type seedComponent struct {
	api  *admin.API
	plan *admin.SeedPlan
	file string
	out  io.Writer
}

func (s *seedComponent) Name() string { return "seed" }

func (s *seedComponent) Start(ctx context.Context) error {
	seeded, err := s.api.ApplySeed(ctx, s.plan, nil)
	if err != nil {
		return fmt.Errorf("apply seed file %s: %w", s.file, err)
	}
	fmt.Fprintf(s.out, "seeded %s from %s\n", strings.Join(seeded, ", "), s.file)
	return nil
}

func (s *seedComponent) Stop(context.Context) error { return nil }
