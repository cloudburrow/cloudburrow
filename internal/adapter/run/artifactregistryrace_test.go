package run

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// arHookCustoms runs hook, if set, while a check asks for constraints.
type arHookCustoms struct {
	list []orgpolicy.CustomConstraint
	hook func()
}

func (arHookCustoms) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (h arHookCustoms) Customs(context.Context, string, string, string) ([]orgpolicy.CustomConstraint, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.list, nil
}

func arAllowDocker() []orgpolicy.CustomConstraint {
	return []orgpolicy.CustomConstraint{{Name: "custom.docker", Condition: "resource.format == 'DOCKER'", ActionType: "ALLOW",
		Description: "Docker only.", Effective: orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{
			Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})}}
}

func putTestRepository(t *testing.T, name string) {
	t.Helper()
	repositories.mu.Lock()
	if repositories.repos == nil {
		repositories.repos = map[string]*arpb.Repository{}
	}
	repositories.repos[name] = &arpb.Repository{Name: name, Format: arpb.Repository_DOCKER, Mode: arpb.Repository_STANDARD_REPOSITORY}
	repositories.mu.Unlock()
	t.Cleanup(func() {
		repositories.mu.Lock()
		delete(repositories.repos, name)
		repositories.mu.Unlock()
	})
}

func repositoryStored(name string) bool {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	_, ok := repositories.repos[name]
	return ok
}

// TestRepositoryUpdateCheckOutsideLock (#1902): UpdateRepository's
// custom-constraint check runs without repositories.mu, so a delete of
// the same repository made while it runs neither deadlocks (as #1855's
// did) nor is undone: the update is NOT_FOUND.
func TestRepositoryUpdateCheckOutsideLock(t *testing.T) {
	name := "projects/ar-race-test/locations/us-west1/repositories/lock"
	putTestRepository(t, name)
	var deleted atomic.Bool
	orgpolicy.Use(arHookCustoms{list: arAllowDocker(), hook: func() {
		if deleted.CompareAndSwap(false, true) && !RemoveRepository(name) {
			t.Errorf("RemoveRepository from within the check found nothing")
		}
	}})
	defer orgpolicy.Use(nil)
	done := make(chan error, 1)
	go func() {
		_, err := (&ArtifactRegistryServer{}).UpdateRepository(context.Background(), &arpb.UpdateRepositoryRequest{
			Repository: &arpb.Repository{Name: name, Description: "d"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
		done <- err
	}()
	select {
	case err := <-done:
		if status.Code(err) != codes.NotFound {
			t.Errorf("an update whose repository was deleted during its check = %v, want NOT_FOUND", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UpdateRepository deadlocked against a delete made during its custom-constraint check")
	}
	if repositoryStored(name) {
		t.Error("the update wrote the deleted repository back")
	}
}

// TestRepositoryConcurrentUpdateDelete (#1902, run with -race): updates
// and a delete of one repository, interleaved, each end OK or NOT_FOUND,
// and the deleted repository stays deleted.
func TestRepositoryConcurrentUpdateDelete(t *testing.T) {
	orgpolicy.Use(arHookCustoms{list: arAllowDocker(), hook: func() { time.Sleep(time.Millisecond) }})
	defer orgpolicy.Use(nil)
	a := &ArtifactRegistryServer{}
	var wg sync.WaitGroup
	for i := range 4 {
		name := fmt.Sprintf("projects/ar-race-test/locations/us-west1/repositories/r%d", i)
		putTestRepository(t, name)
		for w := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range 5 {
					_, err := a.UpdateRepository(context.Background(), &arpb.UpdateRepositoryRequest{
						Repository: &arpb.Repository{Name: name, Description: fmt.Sprint(w, "-", n)},
						UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
					if c := status.Code(err); c != codes.OK && c != codes.NotFound {
						t.Errorf("UpdateRepository %s = %v, want OK or NOT_FOUND", name, err)
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(2 * time.Millisecond)
			if !RemoveRepository(name) {
				t.Errorf("RemoveRepository %s found nothing", name)
			}
		}()
	}
	wg.Wait()
	for i := range 4 {
		if name := fmt.Sprintf("projects/ar-race-test/locations/us-west1/repositories/r%d", i); repositoryStored(name) {
			t.Errorf("%s is back after its delete", name)
		}
	}
}
