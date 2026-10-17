package main

import (
	"context"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// Per-project resets for the Cloud SQL Admin API (#1541), Backup and DR
// (#1542) and Artifact Registry (#1543), which /admin/reset did not reach,
// so a purged project's instances, vaults and repositories were read by
// the project created again under its ID. Each also resets every
// project's, for a reset without project=.

// sqlAdminResetter deletes the Admin API's created instances, with their
// servers and volumes, backups, client certificates and operations.
type sqlAdminResetter struct{ srv func() *sqladmin.Server }

func (r *sqlAdminResetter) Name() string { return "sqladmin" }

func (r *sqlAdminResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *sqlAdminResetter) ResetProject(ctx context.Context, project string) error {
	srv := r.srv()
	if srv == nil {
		return nil
	}
	return srv.ResetProject(ctx, project)
}

// backupDRResetter deletes Backup and DR's vaults, plans, associations,
// data sources, backups, trials and operations.
type backupDRResetter struct{ srv func() *backupdr.Server }

func (r *backupDRResetter) Name() string { return "backupdr" }

func (r *backupDRResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *backupDRResetter) ResetProject(ctx context.Context, project string) error {
	srv := r.srv()
	if srv == nil {
		return nil
	}
	return srv.ResetProject(ctx, project)
}

// artifactRegistryResetter deletes Artifact Registry's repositories, with
// their content, rules and policies, and projects' settings and configs.
type artifactRegistryResetter struct{}

func (artifactRegistryResetter) Name() string { return "artifactregistry" }

func (r artifactRegistryResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (artifactRegistryResetter) ResetProject(_ context.Context, project string) error {
	return runadapter.ResetArtifactRegistry(project)
}
