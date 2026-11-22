//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/apphub/apiv1/apphubpb"
	"cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/api/iterator"
)

// TestAppHubDiscoversDataform (#2110, round 4), with the official Dataform
// and App Hub clients over gRPC and REST: a Dataform repository is a
// discovered service in its region, under the URI supported-resources
// writes (//dataform.googleapis.com/projects/PROJECT_ID/locations/LOCATION/repositories/REPOSITORY),
// found by LookupDiscoveredService and registered to a REGIONAL application
// in that region.
// Docs re-read 2026-11-22: https://docs.cloud.google.com/app-hub/docs/supported-resources.
func TestAppHubDiscoversDataform(t *testing.T) {
	h := New(t)
	clients := appHubClients(t, h)
	df := dataformClients(t, h)["grpc"]
	ctx := h.Context()
	project := h.Project()
	suffix := time.Now().UnixNano() % 1e6
	const region = "us-central1"

	repoID := fmt.Sprintf("ah-repo-%d", suffix)
	repo, err := df.CreateRepository(ctx, &dataformpb.CreateRepositoryRequest{
		Parent: "projects/" + project + "/locations/" + region, RepositoryId: repoID})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = df.DeleteRepository(h.Context(), &dataformpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	uri := fmt.Sprintf("//dataform.googleapis.com/projects/%s/locations/%s/repositories/%s", project, region, repoID)

	for _, via := range []string{"grpc", "rest"} {
		t.Run(via, func(t *testing.T) {
			h := h.For(t)
			c := clients[via]
			detachAppHubProject(t, h, c, project)
			host := "projects/" + project + "/locations/"
			op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: host + "global",
				ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if _, err := op.Wait(ctx); err != nil {
				t.Fatal(err)
			}

			var found *apphubpb.DiscoveredService
			it := c.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: host + region})
			for {
				ds, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("ListDiscoveredServices: %v", err)
				}
				if ds.GetServiceReference().GetUri() == uri {
					found = ds
				}
			}
			if found == nil {
				t.Fatalf("%s is not discovered in %s", uri, region)
			}
			if p := found.GetServiceProperties(); p.GetLocation() != region || p.GetGcpProject() != "projects/"+project {
				t.Errorf("properties %v", p)
			}
			got, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + region, Uri: uri})
			if err != nil || got.GetDiscoveredService().GetName() != found.GetName() {
				t.Fatalf("LookupDiscoveredService: %v, %v", got, err)
			}
			// Not in global: a regional resource is discovered in its region only.
			if got, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + "global", Uri: uri}); err == nil && got.GetDiscoveredService() != nil {
				t.Errorf("found in global too: %v", got)
			}

			app := fmt.Sprintf("ah-df-%s-%d", via, suffix)
			aop, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: host + region, ApplicationId: app,
				Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_REGIONAL}}})
			if err != nil {
				t.Fatalf("CreateApplication: %v", err)
			}
			if _, err := aop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			name := host + region + "/applications/" + app
			t.Cleanup(func() {
				if op, err := c.DeleteService(h.Context(), &apphubpb.DeleteServiceRequest{Name: name + "/services/repo"}); err == nil {
					_ = op.Wait(h.Context())
				}
				if op, err := c.DeleteApplication(h.Context(), &apphubpb.DeleteApplicationRequest{Name: name}); err == nil {
					_ = op.Wait(h.Context())
				}
			})
			sop, err := c.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: name, ServiceId: "repo",
				Service: &apphubpb.Service{DiscoveredService: found.GetName()}})
			if err != nil {
				t.Fatalf("CreateService: %v", err)
			}
			svc, err := sop.Wait(ctx)
			if err != nil || svc.GetServiceReference().GetUri() != uri {
				t.Fatalf("registered: %v, %v", svc, err)
			}
		})
	}
}
