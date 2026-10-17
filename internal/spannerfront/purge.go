package spannerfront

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// ResetProject deletes one project's Spanner state, as Google does when the
// project is purged (#1540): the emulator's instances under it, their
// databases first, and everything the front keeps for it: instance and
// partition changes, operations, deletion protection, IAM policies,
// backups and their rows, restore records, backup schedules, database
// roles and the project's custom instance configurations.
func (s *Instances) ResetProject(ctx context.Context, project string) error {
	if project == "" {
		return errors.New("a project is required")
	}
	parent := "projects/" + project
	var errs []error
	token := ""
	for {
		resp, err := s.emu.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: parent, PageToken: token})
		if err != nil {
			errs = append(errs, fmt.Errorf("list instances: %w", err))
			break
		}
		for _, inst := range resp.GetInstances() {
			if !strings.HasPrefix(inst.GetName(), parent+"/") {
				continue // an emulator that lists every project's
			}
			if err := s.dropInstance(ctx, inst.GetName()); err != nil {
				errs = append(errs, err)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}

	s.mu.Lock()
	frontstate.DropProject(s.changes, project)
	frontstate.DropProject(s.ops, project)
	s.opOrder = frontstate.KeepOthers(s.opOrder, project)
	frontstate.DropProject(s.protected, project)
	frontstate.DropProject(s.policies, project)
	frontstate.DropProject(s.backups, project)
	frontstate.DropProject(s.restored, project)
	frontstate.DropProject(s.schedules, project)
	frontstate.DropProject(s.roleStmts, project)
	s.emuOpNames = frontstate.KeepOthers(s.emuOpNames, project)
	frontstate.DropProject(s.configs, project)
	frontstate.DropProject(s.partitions, project)
	s.mu.Unlock()
	if err := s.save(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// dropInstance drops an emulator instance's databases and then the
// instance, through the emulator's own admin API.
func (s *Instances) dropInstance(ctx context.Context, name string) error {
	token := ""
	for {
		resp, err := s.emuDB.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: name, PageToken: token})
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list databases of %s: %w", name, err)
		}
		for _, db := range resp.GetDatabases() {
			if _, err := s.emuDB.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: db.GetName()}); err != nil && status.Code(err) != codes.NotFound {
				return fmt.Errorf("drop %s: %w", db.GetName(), err)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if _, err := s.emu.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: name}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}
