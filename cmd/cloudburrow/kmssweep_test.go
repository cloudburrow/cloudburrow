package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// A version that fell due while nothing was running is DESTROYED on disk as
// soon as the service starts, and Stop ends the sweep (#403).
func TestKMSServiceSweepsOnStartAndStopsTheSweep(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceKMS}
	cfg.Endpoints.KMS = 0
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	db := store.NewMemory()
	const key = "kms/version/projects~demo-project~locations~global~keyRings~r~cryptoKeys~k~cryptoKeyVersions~1"
	rec, _ := json.Marshal(map[string]any{"name": "projects/demo-project/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"created": now.Add(-48 * time.Hour), "material": make([]byte, 32), "state": "DESTROY_SCHEDULED", "destroyTime": now.Add(-time.Hour)})
	if err := db.Put(key, rec); err != nil {
		t.Fatal(err)
	}
	clock := sched.NewFakeClock(now)
	svc := &kmsService{cfg: cfg, db: db, clock: clock}
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := db.Get(key)
	var v struct {
		State            string
		Material         []byte
		DestroyEventTime time.Time
	}
	if err != nil || json.Unmarshal(b, &v) != nil {
		t.Fatal(err)
	}
	if v.State != "DESTROYED" || len(v.Material) != 0 || !v.DestroyEventTime.Equal(now.Add(-time.Hour)) {
		t.Errorf("right after Start the version is stored as %s with %d bytes of material, event time %v", v.State, len(v.Material), v.DestroyEventTime)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := svc.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-svc.swept:
	default:
		t.Error("Stop returned with the sweep still running")
	}
}

// unreachableStore fails every List until reachable is set, as the KMS store
// does before the cluster holding its Secrets exists.
type unreachableStore struct {
	store.Store
	mu        sync.Mutex
	reachable bool
}

func (u *unreachableStore) List(prefix string) ([]string, error) {
	u.mu.Lock()
	ok := u.reachable
	u.mu.Unlock()
	if !ok {
		return nil, errors.New("kubectl: exit status 1: error: stat kubeconfig: no such file or directory")
	}
	return u.Store.List(prefix)
}

// The service starts before the cluster its store lives in (#403 in CI): a
// failed first sweep does not fail Start, and the sweep is retried once the
// store is reachable.
func TestKMSServiceStartsBeforeItsStoreIsReachable(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceKMS}
	cfg.Endpoints.KMS = 0
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	mem := store.NewMemory()
	const key = "kms/version/projects~demo-project~locations~global~keyRings~r~cryptoKeys~k~cryptoKeyVersions~1"
	rec, _ := json.Marshal(map[string]any{"name": "projects/demo-project/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"created": now.Add(-48 * time.Hour), "material": make([]byte, 32), "state": "DESTROY_SCHEDULED", "destroyTime": now.Add(-time.Hour)})
	if err := mem.Put(key, rec); err != nil {
		t.Fatal(err)
	}
	db := &unreachableStore{Store: mem}
	clock := sched.NewFakeClock(now)
	svc := &kmsService{cfg: cfg, db: db, clock: clock}
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start with an unreachable store: %v", err)
	}
	defer svc.Stop(ctx)
	// Run's first sweep fails too, and it waits a second before retrying.
	deadline := time.Now().Add(5 * time.Second)
	for !clock.HasWaiterAt(now.Add(time.Second)) {
		if time.Now().After(deadline) {
			t.Fatal("the sweep never armed its retry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, _ := mem.Get(key); strings.Contains(string(b), `"DESTROYED"`) {
		t.Fatal("swept while the store was unreachable")
	}
	db.mu.Lock()
	db.reachable = true
	db.mu.Unlock()
	clock.Advance(time.Second)
	for {
		b, _ := mem.Get(key)
		var v struct{ State string }
		if json.Unmarshal(b, &v) == nil && v.State == "DESTROYED" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retried sweep did not store DESTROYED: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
