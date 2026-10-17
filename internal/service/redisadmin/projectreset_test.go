package redisadmin

import (
	"context"
	"testing"

	"cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestResetProjectDropsOnlyThatProjectsInstances: a project's reset (#1540)
// removes its instances' servers and records, and leaves another project's.
func TestResetProjectDropsOnlyThatProjectsInstances(t *testing.T) {
	db := store.NewMemory()
	s, p, _ := newCreating(t, db)
	ctx := context.Background()
	for _, parent := range []string{"projects/gone/locations/us-central1", "projects/kept/locations/us-central1"} {
		op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cache",
			Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if op = wait(t, s, op); op.GetError() != nil {
			t.Fatalf("create in %s: %v", parent, op.GetError())
		}
	}
	if err := s.ResetProject(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	gone := "projects/gone/locations/us-central1/instances/cache"
	kept := "projects/kept/locations/us-central1/instances/cache"
	if _, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: gone}); code(err) != codes.NotFound {
		t.Errorf("GetInstance(%s) after the reset = %v, want NOT_FOUND", gone, err)
	}
	if _, ok := p.running[ServerName(gone)]; ok {
		t.Errorf("the reset project's server still runs")
	}
	if _, err := db.Get(recordPrefix + ServerName(gone)); err == nil {
		t.Errorf("the reset project's record is still kept")
	}
	if _, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: kept}); err != nil {
		t.Errorf("another project's instance: %v", err)
	}
	if _, ok := p.running[ServerName(kept)]; !ok {
		t.Errorf("another project's server was removed")
	}
	// The project's ID is free again.
	op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/gone/locations/us-central1", InstanceId: "cache",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatalf("create again: %v", op.GetError())
	}
}
