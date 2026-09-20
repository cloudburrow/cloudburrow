package apicontract

import (
	"strings"
	"testing"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
)

// The pinned revision must be an explicit commit, never a branch name.
// googleapis publishes no release tags, so a mutable reference here would make
// the whole contract set unreproducible.
func TestGoogleapisCommitIsPinned(t *testing.T) {
	t.Parallel()
	if len(GoogleapisCommit) != 40 {
		t.Fatalf("GoogleapisCommit = %q, want a 40-character commit SHA", GoogleapisCommit)
	}
	for _, r := range GoogleapisCommit {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("GoogleapisCommit contains %q; want lowercase hex only", r)
		}
	}
	for _, bad := range []string{"master", "main", "HEAD", "latest"} {
		if strings.EqualFold(GoogleapisCommit, bad) {
			t.Fatalf("GoogleapisCommit is the mutable reference %q", bad)
		}
	}
}

// Every surface must say where it comes from and what we do with it, so the
// matrix cannot claim a service we merely integrate.
func TestSurfacesAreFullyDescribed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range Surfaces() {
		if s.Service == "" || s.Source == "" || s.RESTSurface == "" {
			t.Errorf("incomplete surface: %+v", s)
		}
		switch s.Role {
		case RoleImplement, RoleAdapt, RoleUpstream:
		default:
			t.Errorf("surface %s has unknown role %q", s.Service, s.Role)
		}
		if seen[s.Service] {
			t.Errorf("duplicate surface %s", s.Service)
		}
		seen[s.Service] = true
	}
	// The two services CloudBurrow is responsible for serving must be present.
	for _, want := range []string{"google.cloud.tasks.v2", "google.cloud.run.v2"} {
		if !seen[want] {
			t.Errorf("missing contract for %s", want)
		}
	}
}

// Services served by an upstream must not be described as implemented by us.
func TestUpstreamServicesAreNotClaimedAsImplemented(t *testing.T) {
	t.Parallel()
	for _, s := range Surfaces() {
		if s.Role != RoleUpstream {
			continue
		}
		if !strings.Contains(s.RESTSurface, "not served by CloudBurrow") {
			t.Errorf("%s is upstream but its REST surface is described as %q", s.Service, s.RESTSurface)
		}
	}
}

// The generated types must round-trip, which proves we are using real protobuf
// messages rather than hand-rolled structs that merely look similar.
func TestContractTypesRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("cloud tasks", func(t *testing.T) {
		t.Parallel()
		in := &taskspb.Queue{
			Name: "projects/p/locations/l/queues/q",
			RateLimits: &taskspb.RateLimits{
				MaxDispatchesPerSecond:  10,
				MaxConcurrentDispatches: 5,
			},
		}
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var out taskspb.Queue
		if err := proto.Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if out.Name != in.Name {
			t.Errorf("name = %q, want %q", out.Name, in.Name)
		}
		if out.RateLimits.GetMaxConcurrentDispatches() != 5 {
			t.Errorf("rate limits did not survive the round trip: %+v", out.RateLimits)
		}
	})

	t.Run("cloud run", func(t *testing.T) {
		t.Parallel()
		in := &runpb.Service{
			Name: "projects/p/locations/l/services/s",
			Template: &runpb.RevisionTemplate{
				Containers: []*runpb.Container{{Image: "dev.local/app:v1"}},
			},
		}
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var out runpb.Service
		if err := proto.Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got := out.GetTemplate().GetContainers()[0].GetImage(); got != "dev.local/app:v1" {
			t.Errorf("image = %q, want dev.local/app:v1", got)
		}
	})
}

// Resource-name fields carry google.api.resource annotations that our
// resource-name handling depends on; confirm the descriptors are real.
func TestProtoDescriptorsArePresent(t *testing.T) {
	t.Parallel()
	if got := (&taskspb.Task{}).ProtoReflect().Descriptor().FullName(); got != "google.cloud.tasks.v2.Task" {
		t.Errorf("task descriptor = %q, want google.cloud.tasks.v2.Task", got)
	}
	if got := (&runpb.Service{}).ProtoReflect().Descriptor().FullName(); got != "google.cloud.run.v2.Service" {
		t.Errorf("run descriptor = %q, want google.cloud.run.v2.Service", got)
	}
}
