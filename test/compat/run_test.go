//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// runClient returns the official Cloud Run client pointed at the local adapter.
//
// Like Cloud Tasks, Cloud Run has no emulator environment variable in any
// official client, so the endpoint and insecure credentials are explicit.
func runClient(t *testing.T, h *Harness) *run.ServicesClient {
	t.Helper()
	endpoint := h.Endpoint(EnvRun)
	c, err := run.NewServicesClient(h.Context(),
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("run.NewServicesClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// runClientOptions are the options every Cloud Run client here uses.
func runClientOptions(h *Harness) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvRun)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func runParent(h *Harness) string {
	return fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Services/ListServices, google.cloud.run.v2.Services/DeleteService
//
// TestRunServiceLifecycle deploys a real container through the Cloud Run API,
// waits for Knative to report it serving, and deletes it.
func TestRunServiceLifecycle(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	ctx := h.Context()
	id := "compat-hello"
	name := runParent(h) + "/services/" + id

	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: id,
		Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{
				Containers: []*runpb.Container{{
					Image: "ghcr.io/knative/helloworld-go:latest",
					Env: []*runpb.EnvVar{{
						Name:   "TARGET",
						Values: &runpb.EnvVar_Value{Value: "CloudBurrow"},
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name})
	})

	// Cloud Run returns an operation, so the caller polls. Wait returns when
	// the revision is genuinely ready, not when the API accepted the request.
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the service to become ready: %v", err)
	}
	if svc.GetUri() == "" {
		t.Error("ready service has no URI")
	}
	if svc.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("terminal condition = %v, want SUCCEEDED", svc.GetTerminalCondition().GetState())
	}
	t.Logf("service ready at %s", svc.GetUri())

	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.GetName() != name {
		t.Errorf("name = %q, want %q", got.GetName(), name)
	}
	if len(got.GetTemplate().GetContainers()) == 0 {
		t.Fatal("service has no containers")
	}

	// The service must appear in a listing.
	it := c.ListServices(ctx, &runpb.ListServicesRequest{Parent: runParent(h)})
	found := false
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListServices: %v", err)
		}
		if s.GetName() == name {
			found = true
		}
	}
	if !found {
		t.Error("ListServices omitted the service just created")
	}

	// Delete, and wait for it: the service must then be gone.
	dop, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("waiting for the delete: %v", err)
	}
	if _, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after DeleteService = %v, want NotFound", err)
	}
}

// covers: google.cloud.run.v2.Revisions/ListRevisions, google.cloud.run.v2.Revisions/GetRevision, google.cloud.run.v2.Revisions/DeleteRevision
//
// TestRunRevisions (#299): a deployed service has exactly one revision, of
// generation 1, belonging to it; GetRevision returns the same one; an unknown
// revision is NOT_FOUND; and the revision serving traffic cannot be deleted.
func TestRunRevisions(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	ctx := h.Context()
	id := "compat-revs"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest"}}}},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}

	rc, err := run.NewRevisionsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	var revs []*runpb.Revision
	it := rc.ListRevisions(ctx, &runpb.ListRevisionsRequest{Parent: name})
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListRevisions: %v", err)
		}
		revs = append(revs, r)
	}
	if len(revs) != 1 {
		t.Fatalf("ListRevisions returned %d revisions, want 1: %v", len(revs), revs)
	}
	r := revs[0]
	if r.GetService() != name || r.GetGeneration() != 1 {
		t.Errorf("revision service = %q, generation = %d; want %q, 1", r.GetService(), r.GetGeneration(), name)
	}
	if latest := svc.GetLatestReadyRevision(); latest != "" && !strings.HasSuffix(r.GetName(), "/revisions/"+latest) {
		t.Errorf("revision %s is not the service's latest ready revision %s", r.GetName(), latest)
	}
	if len(r.GetContainers()) != 1 || !strings.Contains(r.GetContainers()[0].GetImage(), "helloworld-go") {
		t.Errorf("revision containers = %v", r.GetContainers())
	}

	got, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: r.GetName()})
	if err != nil || got.GetName() != r.GetName() || got.GetGeneration() != 1 || got.GetService() != name {
		t.Errorf("GetRevision = %v, %v; want the listed revision", got, err)
	}
	if _, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: name + "/revisions/" + id + "-99999"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRevision(unknown) = %v, want NotFound", err)
	}
	if _, err := rc.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{Name: r.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteRevision(serving) = %v, want FailedPrecondition", err)
	}
	if _, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: r.GetName()}); err != nil {
		t.Errorf("the serving revision is gone after a refused delete: %v", err)
	}
}

func TestRunMissingServiceIsNotFound(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	_, err := c.GetService(h.Context(), &runpb.GetServiceRequest{
		Name: runParent(h) + "/services/absent-service",
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetService(absent) = %v, want NotFound", status.Code(err))
	}
}

// Configuration the adapter does not map must be reported through the SDK, not
// silently dropped — a caller who set it would otherwise believe it applied.
func TestRunUnsupportedConfigurationIsReported(t *testing.T) {
	h := New(t)
	c := runClient(t, h)

	_, err := c.CreateService(h.Context(), &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: "unsupported-svc",
		Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{
				ServiceAccount: "sa@project.iam.gserviceaccount.com",
				Containers:     []*runpb.Container{{Image: "gcr.io/p/app:v1"}},
			},
		},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("serviceAccount = %v, want Unimplemented", status.Code(err))
	}
	if !strings.Contains(err.Error(), "serviceAccount") {
		t.Errorf("error should name the field, got: %v", err)
	}
}

// An untagged image is a mutable target and must be refused.
func TestRunUntaggedImageIsRefused(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	_, err := c.CreateService(h.Context(), &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: "untagged-svc",
		Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{
				Containers: []*runpb.Container{{Image: "someimage"}},
			},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("untagged image = %v, want InvalidArgument", status.Code(err))
	}
}
