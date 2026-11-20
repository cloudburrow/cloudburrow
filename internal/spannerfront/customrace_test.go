package spannerfront

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// hookSource runs hook, if set, while a check asks for constraints.
type hookSource struct {
	customSource
	hook func()
}

func (h hookSource) Customs(ctx context.Context, r, resourceType, m string) ([]orgpolicy.CustomConstraint, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.customSource.Customs(ctx, r, resourceType, m)
}

func raceConfigs(t *testing.T, s *Instances, n int) []string {
	t.Helper()
	base := managedConfig("projects/p", "nam3")
	var names []string
	for i := range n {
		id := fmt.Sprintf("custom-ok%d", i)
		if _, err := s.CreateInstanceConfig(context.Background(), &instancepb.CreateInstanceConfigRequest{Parent: "projects/p", InstanceConfigId: id,
			InstanceConfig: &instancepb.InstanceConfig{DisplayName: "ok", BaseConfig: base.GetName(), Replicas: base.GetReplicas()}}); err != nil {
			t.Fatal(err)
		}
		names = append(names, "projects/p/instanceConfigs/"+id)
	}
	return names
}

func dropConfig(s *Instances, name string) {
	s.mu.Lock()
	delete(s.configs, name)
	s.mu.Unlock()
}

func rename(s *Instances, name, display string) error {
	_, err := s.UpdateInstanceConfig(context.Background(), &instancepb.UpdateInstanceConfigRequest{
		InstanceConfig: &instancepb.InstanceConfig{Name: name, DisplayName: display},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	return err
}

// TestInstanceConfigCheckOutsideLock (#1902): UpdateInstanceConfig's
// custom-constraint check runs without s.mu, so a delete of the
// configuration made while it runs neither deadlocks nor is undone: the
// update is NOT_FOUND.
func TestInstanceConfigCheckOutsideLock(t *testing.T) {
	s := &Instances{configs: map[string]*instancepb.InstanceConfig{}, now: time.Now, ops: map[string]*longrunningpb.Operation{}}
	src := customSource{orgpolicy.TypeSpannerInstanceConfig: {enforcedCustom("custom.cfgName", "resource.displayName.startsWith('ok')", "ALLOW", "ok")}}
	orgpolicy.Use(src)
	defer orgpolicy.Use(nil)
	name := raceConfigs(t, s, 1)[0]
	var deleted atomic.Bool
	orgpolicy.Use(hookSource{customSource: src, hook: func() {
		if deleted.CompareAndSwap(false, true) {
			dropConfig(s, name)
		}
	}})
	done := make(chan error, 1)
	go func() { done <- rename(s, name, "ok-renamed") }()
	select {
	case err := <-done:
		if status.Code(err) != codes.NotFound {
			t.Errorf("an update whose configuration was deleted during its check = %v, want NOT_FOUND", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UpdateInstanceConfig deadlocked against a delete made during its custom-constraint check")
	}
	s.mu.Lock()
	_, back := s.configs[name]
	s.mu.Unlock()
	if back {
		t.Error("the update wrote the deleted configuration back")
	}
}

// TestInstanceConfigConcurrentUpdateDelete (#1902, run with -race):
// updates and a delete of each configuration, interleaved, end OK or
// NOT_FOUND, and a deleted configuration stays deleted.
func TestInstanceConfigConcurrentUpdateDelete(t *testing.T) {
	s := &Instances{configs: map[string]*instancepb.InstanceConfig{}, now: time.Now, ops: map[string]*longrunningpb.Operation{}}
	src := customSource{orgpolicy.TypeSpannerInstanceConfig: {enforcedCustom("custom.cfgName", "resource.displayName.startsWith('ok')", "ALLOW", "ok")}}
	orgpolicy.Use(src)
	defer orgpolicy.Use(nil)
	names := raceConfigs(t, s, 4)
	orgpolicy.Use(hookSource{customSource: src, hook: func() { time.Sleep(time.Millisecond) }})
	var wg sync.WaitGroup
	for _, name := range names {
		for w := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range 5 {
					if c := status.Code(rename(s, name, fmt.Sprint("ok", w, n))); c != codes.OK && c != codes.NotFound {
						t.Errorf("UpdateInstanceConfig %s = %v, want OK or NOT_FOUND", name, c)
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(2 * time.Millisecond)
			dropConfig(s, name)
		}()
	}
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		if _, ok := s.configs[name]; ok {
			t.Errorf("%s is back after its delete", name)
		}
	}
}
