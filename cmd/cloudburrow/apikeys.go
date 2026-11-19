package main

import (
	"context"
	"net"
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// apiKeysService keys the API Keys API, always served beside Service Usage
// (#1915).
const apiKeysService config.Service = "apikeys"

// newAPIKeysServer serves the API Keys API v2 (#1915) over st, the project
// registry's store, so its keys persist with the instance's projects.
func newAPIKeysServer(cfg config.Config, reg *resourcemanager.Registry, st store.Store) *apikeys.Server {
	if cfg.Endpoints.APIKeys < 0 {
		return nil
	}
	return apikeys.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.APIKeys)),
		st, serviceUsageProjects{reg: reg})
}

// apiKeysResetter forgets each project's API keys (#1915).
type apiKeysResetter struct {
	srv func() *apikeys.Server
}

func (r *apiKeysResetter) Name() string { return "apikeys" }

func (r *apiKeysResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

// ResetProject forgets one project's keys, or every project's for "".
func (r *apiKeysResetter) ResetProject(_ context.Context, project string) error {
	srv := r.srv()
	if srv == nil {
		return nil
	}
	return srv.ResetProject(project)
}
