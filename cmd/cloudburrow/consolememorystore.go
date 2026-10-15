package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// memorystoreProvider is Memorystore's Instances page (#1302): the one
// instance, read through the admin API's own ListInstances, so the screen
// shows what gcloud and the SDK see. Flush empties it, as FLUSHALL does —
// the Memorystore console's own data-plane flush; the admin API has no
// method for it.
type memorystoreProvider struct {
	api     *redisadmin.Server
	backend redisadmin.Valkey
}

// newMemorystoreProvider builds the screen for the server at addr.
func newMemorystoreProvider(cfg config.Config, addr string) memorystoreProvider {
	be := redisadmin.Valkey{Addr: func() string { return addr }}
	return memorystoreProvider{api: redisadmin.NewServer(memorystoreInstance(cfg, time.Time{}, be.Addr), be), backend: be}
}

func (memorystoreProvider) ID() string    { return "memorystore" }
func (memorystoreProvider) Title() string { return "Memorystore" }

const memorystoreNote = "One instance: the Valkey server --services memorystore runs. " +
	"It cannot be created, resized, upgraded or deleted here; AUTH and in-transit encryption are off."

// List implements console.Provider: the project's instances.
func (p memorystoreProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Location", "Version", "Endpoint", "Tier", "Capacity"},
		NameColumn: "Instance ID",
		Noun:       "instances",
		Note:       memorystoreNote,
	}
	resp, err := p.api.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil {
		l.Unavailable = "cannot reach Memorystore: " + err.Error()
		return l, nil
	}
	for _, inst := range resp.GetInstances() {
		l.Items = append(l.Items, console.Resource{
			Name:   redisadmin.InstanceID,
			Status: inst.GetState().String(),
			Fields: map[string]string{
				"Location": inst.GetLocationId(),
				"Version":  inst.GetRedisVersion(),
				"Endpoint": net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))),
				"Tier":     inst.GetTier().String(),
				"Capacity": fmt.Sprintf("%d GB", inst.GetMemorySizeGb()),
			},
			Actions: p.Actions(console.Resource{Name: redisadmin.InstanceID}),
		})
	}
	l.Total = len(l.Items)
	return l, nil
}

// Actions implements console.Actor.
func (memorystoreProvider) Actions(console.Resource) []console.Action {
	return []console.Action{{ID: "flush", Label: "Flush", Destructive: true,
		Confirm: "Every key in every database of the instance is deleted (FLUSHALL)."}}
}

// Act implements console.Actor.
func (p memorystoreProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "flush" {
		return fmt.Errorf("unknown action %q", action)
	}
	if name != redisadmin.InstanceID {
		return errors.New("no such instance: " + name)
	}
	// The instance must be this project's, as the list shows it.
	resp, err := p.api.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil {
		return err
	}
	if len(resp.GetInstances()) == 0 {
		return errors.New("project " + project + " has no Memorystore instance")
	}
	return p.backend.Flush(ctx)
}
