package main

// Create instance on the Spanner list screen (#1491): the console parity
// audit found an instance made only as a side effect of Create database,
// with its ID as display name, emulator-config and one node. Create
// instance sends InstanceAdmin's CreateInstance with the form's ID, display
// name, configuration and compute capacity, as Google's console does.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const spannerCreateInstanceAction = "createinstance"

// ListActions implements console.ListActor: Create instance, beside Create
// database.
func (p spannerProvider) ListActions(_ context.Context, project string) []console.Action {
	if project == "" {
		return nil
	}
	return []console.Action{{ID: spannerCreateInstanceAction, Label: "Create instance", Fields: []console.Field{
		{Name: "instance", Label: "Instance ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{1,62}[a-z0-9]$`,
			Help: "Lowercase letters, digits and hyphens, 3 to 64; it cannot be changed later."},
		{Name: "displayName", Label: "Instance name", Type: "text", Required: true, Pattern: `^.{4,30}$`, Help: "4 to 30 characters."},
		{Name: "config", Label: "Configuration", Type: "text", Required: true, Default: "emulator-config",
			Help: "An instance configuration ID; the emulator serves emulator-config, and every configuration the same."},
		{Name: "processingUnits", Label: "Processing units", Type: "text", Required: true, Default: "1000", Pattern: `^[0-9]+$`,
			Help: "A multiple of 100 below 1000, and of 1000 (a node) from 1000."},
	}}}
}

// createInstance is CreateInstance, waited on.
func (p spannerProvider) createInstance(ctx context.Context, project string, v map[string]string) error {
	id, display := strings.TrimSpace(v["instance"]), strings.TrimSpace(v["displayName"])
	if id == "" || display == "" {
		return fmt.Errorf("an instance ID and an instance name are required")
	}
	pu, err := strconv.ParseInt(strings.TrimSpace(v["processingUnits"]), 10, 32)
	if err != nil || pu <= 0 || (pu < 1000 && pu%100 != 0) || (pu >= 1000 && pu%1000 != 0) {
		return fmt.Errorf("processing units %q: a multiple of 100 below 1000, and of 1000 from 1000", v["processingUnits"])
	}
	cfg := strings.TrimSpace(v["config"])
	if cfg == "" {
		cfg = "emulator-config"
	}
	if !strings.HasPrefix(cfg, "projects/") {
		cfg = "projects/" + project + "/instanceConfigs/" + cfg
	}
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	op, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: id,
		Instance: &instancepb.Instance{Config: cfg, DisplayName: display, ProcessingUnits: int32(pu)}})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

var _ console.ListActor = spannerProvider{}
