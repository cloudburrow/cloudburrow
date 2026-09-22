package main

import (
	"fmt"
	"path/filepath"

	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/service/resourcemanager"
	"github.com/identity-wael/cloudburrow/internal/store"
)

// openProjects returns the project registry for this instance.
//
// The instance's own project is registered immediately, so the console always
// has one to select and a fresh instance is never projectless — which is what
// made every per-project screen open on an error.
//
// Persistence follows the instance's mode, like every other store here:
// ephemeral means the registry goes with the instance, persistent means
// projects a developer created are still there tomorrow.
// The returned release closes the registry's store and gives up its claim on
// the data directory. It is returned rather than deferred inside, because
// ownership lasts as long as the instance does — and because nothing used to
// call it at all: the lock file outlived every clean shutdown, so a persistent
// instance could not be restarted without deleting it by hand.
func openProjects(cfg config.Config) (*resourcemanager.Registry, func(), error) {
	var db store.Store = store.NewMemory()
	release := func() {}
	if cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(cfg.StateDir, cfg.Name, "projects"))
		if err != nil {
			return nil, release, fmt.Errorf("open project registry: %w", err)
		}
		db = durable
		release = func() { _ = durable.Close() }
	}

	reg := resourcemanager.New(db)
	if _, err := reg.EnsureExists(cfg.Name); err != nil {
		// Not fatal: the instance runs and the console simply opens with no
		// project preselected. It is returned rather than swallowed so the
		// reason is visible instead of showing up later as an empty picker.
		return reg, release, fmt.Errorf("register the instance project %q: %w", cfg.Name, err)
	}
	return reg, release, nil
}
