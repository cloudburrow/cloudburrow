package apphub

import (
	"context"
	"path/filepath"
	"testing"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
)

// A caller listing every location with WithDiscoveryMemo discovers each
// project once; without it, each listing discovers afresh.
func TestDiscoveryMemo(t *testing.T) {
	calls := 0
	s, err := NewServer(filepath.Join(t.TempDir(), "apphub.json"), Options{Discover: func(_ context.Context, project string) ([]Resource, error) {
		calls++
		return []Resource{{URI: runURI, Project: project, Location: "us-central1"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: "projects/host-project-1/locations/global",
		ServiceProjectAttachmentId: "svc-project-1", ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/svc-project-1"}}); err != nil {
		t.Fatal(err)
	}
	list := func(ctx context.Context) {
		t.Helper()
		for _, loc := range []string{"global", "us-central1", "us-east1"} {
			if _, err := s.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: "projects/host-project-1/locations/" + loc}); err != nil {
				t.Fatal(err)
			}
		}
	}
	calls = 0
	list(WithDiscoveryMemo(ctx))
	memoized := calls
	calls = 0
	list(ctx)
	if memoized == 0 || calls != 3*memoized {
		t.Errorf("discoveries with the memo = %d, without = %d; want each project once with it, and once per listing without", memoized, calls)
	}
}
