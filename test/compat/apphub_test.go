//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	apphub "cloud.google.com/go/apphub/apiv1"
	"cloud.google.com/go/apphub/apiv1/apphubpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvAppHub points at the local App Hub endpoint (#2110), gRPC and JSON
// on one port.
const EnvAppHub = "CLOUDBURROW_TEST_APPHUB"

// appHubClients are the official client over gRPC and over REST, both
// pointed at the local endpoint: App Hub has no emulator variable.
func appHubClients(t *testing.T, h *Harness) map[string]*apphub.Client {
	t.Helper()
	addr := h.Endpoint(EnvAppHub)
	ctx := h.Context()
	g, err := apphub.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := apphub.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*apphub.Client{"grpc": g, "rest": r}
}

// deployAppHubService deploys a Cloud Run service and a Cloud Run job in
// the harness project's us-central1, for App Hub to discover.
func deployAppHubResources(t *testing.T, h *Harness, id string) (service, job string) {
	t.Helper()
	ctx := runWait(t)
	rc := runClient(t, h)
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest"}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	service = runParent(h) + "/services/" + id
	t.Cleanup(func() { _, _ = rc.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: service}) })
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("waiting for the Cloud Run service: %v", err)
	}
	jc, err := run.NewJobsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id + "-job", Job: shellJob("echo hi", 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	job = runParent(h) + "/jobs/" + id + "-job"
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: job}) })
	if _, err := waitLRO(ctx, jop); err != nil {
		t.Fatalf("waiting for CreateJob: %v", err)
	}
	return service, job
}

// covers: google.cloud.apphub.v1.AppHub/CreateServiceProjectAttachment, google.cloud.apphub.v1.AppHub/GetServiceProjectAttachment, google.cloud.apphub.v1.AppHub/ListServiceProjectAttachments, google.cloud.apphub.v1.AppHub/LookupServiceProjectAttachment, google.cloud.apphub.v1.AppHub/DetachServiceProjectAttachment, google.cloud.apphub.v1.AppHub/DeleteServiceProjectAttachment, google.cloud.apphub.v1.AppHub/GetBoundary, google.cloud.apphub.v1.AppHub/UpdateBoundary, google.cloud.apphub.v1.AppHub/ListDiscoveredServices, google.cloud.apphub.v1.AppHub/GetDiscoveredService, google.cloud.apphub.v1.AppHub/LookupDiscoveredService, google.cloud.apphub.v1.AppHub/ListDiscoveredWorkloads, google.cloud.apphub.v1.AppHub/GetDiscoveredWorkload, google.cloud.apphub.v1.AppHub/LookupDiscoveredWorkload, google.cloud.apphub.v1.AppHub/CreateApplication, google.cloud.apphub.v1.AppHub/GetApplication, google.cloud.apphub.v1.AppHub/ListApplications, google.cloud.apphub.v1.AppHub/UpdateApplication, google.cloud.apphub.v1.AppHub/DeleteApplication, google.cloud.apphub.v1.AppHub/CreateService, google.cloud.apphub.v1.AppHub/GetService, google.cloud.apphub.v1.AppHub/ListServices, google.cloud.apphub.v1.AppHub/UpdateService, google.cloud.apphub.v1.AppHub/DeleteService, google.cloud.apphub.v1.AppHub/CreateWorkload, google.cloud.apphub.v1.AppHub/GetWorkload, google.cloud.apphub.v1.AppHub/ListWorkloads, google.cloud.apphub.v1.AppHub/UpdateWorkload, google.cloud.apphub.v1.AppHub/DeleteWorkload, google.cloud.apphub.v1.AppHub/ListExtendedMetadataSchemas, google.cloud.apphub.v1.AppHub/GetExtendedMetadataSchema
//
// TestAppHubLifecycle (#2110), with the official client over gRPC and over
// REST: the harness project is attached to itself as a service project
// (looked up, listed, its number answered); a Cloud Run service and job
// deployed there are discovered in us-central1 and looked up by URI; a
// GLOBAL and a REGIONAL application are created, read, listed and updated
// (attributes: criticality, environment, owners); a GLOBAL one in a region
// and a us-central1 service in a us-east1 application are refused; the
// service and the job are registered, read, listed and updated; the
// boundary is read and set to the project; IAM, schemas, locations and
// operations answer; everything is then unregistered, deleted and
// detached.
func TestAppHubLifecycle(t *testing.T) {
	h := New(t)
	clients := appHubClients(t, h) // skips without CLOUDBURROW_TEST_APPHUB
	h.Endpoint(EnvRun)
	ctx := h.Context()
	project := h.Project()
	suffix := time.Now().UnixNano() % 1e6
	svcName, jobName := deployAppHubResources(t, h, fmt.Sprintf("ah-hello-%d", suffix))
	svcURI, jobURI := "//run.googleapis.com/"+svcName, "//run.googleapis.com/"+jobName
	global := "projects/" + project + "/locations/global"
	region := "projects/" + project + "/locations/us-central1"
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: global,
				ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
			if err != nil {
				t.Fatalf("CreateServiceProjectAttachment: %v", err)
			}
			spa, err := op.Wait(ctx)
			if err != nil || spa.GetState() != apphubpb.ServiceProjectAttachment_ACTIVE || spa.GetServiceProject() == "projects/"+project {
				t.Fatalf("the attachment: %v, %v (service_project must be the number)", spa, err)
			}
			if got, err := c.GetServiceProjectAttachment(ctx, &apphubpb.GetServiceProjectAttachmentRequest{Name: spa.GetName()}); err != nil || got.GetUid() != spa.GetUid() {
				t.Fatalf("GetServiceProjectAttachment: %v, %v", got, err)
			}
			if lk, err := c.LookupServiceProjectAttachment(ctx, &apphubpb.LookupServiceProjectAttachmentRequest{Name: global}); err != nil ||
				lk.GetServiceProjectAttachment().GetName() != spa.GetName() {
				t.Fatalf("LookupServiceProjectAttachment: %v, %v", lk, err)
			}
			it := c.ListServiceProjectAttachments(ctx, &apphubpb.ListServiceProjectAttachmentsRequest{Parent: global})
			if a, err := it.Next(); err != nil || a.GetName() != spa.GetName() {
				t.Fatalf("ListServiceProjectAttachments: %v, %v", a, err)
			}
			if _, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: global,
				ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}}); pmCode(err) != codes.AlreadyExists {
				t.Fatalf("a second attachment: %v, want ALREADY_EXISTS", err)
			}

			// Discovery.
			ds, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: region, Uri: svcURI})
			if err != nil || ds.GetDiscoveredService().GetServiceProperties().GetLocation() != "us-central1" {
				t.Fatalf("LookupDiscoveredService: %v, %v", ds, err)
			}
			disc := ds.GetDiscoveredService()
			if got, err := c.GetDiscoveredService(ctx, &apphubpb.GetDiscoveredServiceRequest{Name: disc.GetName()}); err != nil || got.GetServiceReference().GetUri() != svcURI {
				t.Fatalf("GetDiscoveredService: %v, %v", got, err)
			}
			if !listHas(t, c.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: region}), func(d *apphubpb.DiscoveredService) bool { return d.GetName() == disc.GetName() }) {
				t.Error("ListDiscoveredServices omits the Cloud Run service")
			}
			dw, err := c.LookupDiscoveredWorkload(ctx, &apphubpb.LookupDiscoveredWorkloadRequest{Parent: region, Uri: jobURI})
			if err != nil || dw.GetDiscoveredWorkload() == nil {
				t.Fatalf("LookupDiscoveredWorkload: %v, %v", dw, err)
			}
			discW := dw.GetDiscoveredWorkload()
			if got, err := c.GetDiscoveredWorkload(ctx, &apphubpb.GetDiscoveredWorkloadRequest{Name: discW.GetName()}); err != nil || got.GetWorkloadReference().GetUri() != jobURI {
				t.Fatalf("GetDiscoveredWorkload: %v, %v", got, err)
			}
			if !listHas(t, c.ListDiscoveredWorkloads(ctx, &apphubpb.ListDiscoveredWorkloadsRequest{Parent: region}), func(d *apphubpb.DiscoveredWorkload) bool { return d.GetName() == discW.GetName() }) {
				t.Error("ListDiscoveredWorkloads omits the Cloud Run job")
			}

			// Applications.
			attrs := &apphubpb.Attributes{Criticality: &apphubpb.Criticality{Type: apphubpb.Criticality_HIGH},
				Environment:     &apphubpb.Environment{Type: apphubpb.Environment_PRODUCTION},
				DeveloperOwners: []*apphubpb.ContactInfo{{DisplayName: "Dev", Email: "dev@example.com"}}}
			appID := fmt.Sprintf("ah-%s-%d", transport, suffix)
			aop, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: global, ApplicationId: appID,
				Application: &apphubpb.Application{DisplayName: "Burrow", Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}, Attributes: attrs}})
			if err != nil {
				t.Fatalf("CreateApplication: %v", err)
			}
			app, err := aop.Wait(ctx)
			if err != nil || app.GetState() != apphubpb.Application_ACTIVE || app.GetAttributes().GetCriticality().GetType() != apphubpb.Criticality_HIGH {
				t.Fatalf("the application: %v, %v", app, err)
			}
			rop, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: "projects/" + project + "/locations/us-east1", ApplicationId: appID + "-east",
				Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_REGIONAL}}})
			if err != nil {
				t.Fatalf("CreateApplication REGIONAL: %v", err)
			}
			east, err := rop.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: "projects/" + project + "/locations/us-east1", ApplicationId: appID + "-bad",
				Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}}}); pmCode(err) != codes.InvalidArgument {
				t.Fatalf("a GLOBAL application in us-east1: %v, want INVALID_ARGUMENT", err)
			}
			if _, err := c.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: east.GetName(), ServiceId: "hello",
				Service: &apphubpb.Service{DiscoveredService: disc.GetName()}}); pmCode(err) != codes.InvalidArgument {
				t.Fatalf("a us-central1 service in a us-east1 application: %v, want INVALID_ARGUMENT", err)
			}
			if got, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: app.GetName()}); err != nil || got.GetDisplayName() != "Burrow" {
				t.Fatalf("GetApplication: %v, %v", got, err)
			}
			if !listHas(t, c.ListApplications(ctx, &apphubpb.ListApplicationsRequest{Parent: global}), func(a *apphubpb.Application) bool { return a.GetName() == app.GetName() }) {
				t.Error("ListApplications omits the application")
			}
			uop, err := c.UpdateApplication(ctx, &apphubpb.UpdateApplicationRequest{Application: &apphubpb.Application{Name: app.GetName(), Description: "updated",
				Attributes: &apphubpb.Attributes{Environment: &apphubpb.Environment{Type: apphubpb.Environment_STAGING}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "attributes.environment"}}})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := uop.Wait(ctx); err != nil || got.GetDescription() != "updated" || got.GetAttributes().GetEnvironment().GetType() != apphubpb.Environment_STAGING ||
				got.GetAttributes().GetCriticality().GetType() != apphubpb.Criticality_HIGH {
				t.Fatalf("UpdateApplication: %v, %v", got, err)
			}

			// Registration.
			sop, err := c.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: app.GetName(), ServiceId: "hello",
				Service: &apphubpb.Service{DiscoveredService: disc.GetName(), Attributes: attrs}})
			if err != nil {
				t.Fatalf("CreateService: %v", err)
			}
			svc, err := sop.Wait(ctx)
			if err != nil || svc.GetServiceReference().GetUri() != svcURI || svc.GetState() != apphubpb.Service_ACTIVE {
				t.Fatalf("the registered service: %v, %v", svc, err)
			}
			if got, err := c.GetService(ctx, &apphubpb.GetServiceRequest{Name: svc.GetName()}); err != nil || got.GetDiscoveredService() != disc.GetName() {
				t.Fatalf("GetService: %v, %v", got, err)
			}
			if !listHas(t, c.ListServices(ctx, &apphubpb.ListServicesRequest{Parent: app.GetName()}), func(s *apphubpb.Service) bool { return s.GetName() == svc.GetName() }) {
				t.Error("ListServices omits the registered service")
			}
			usop, err := c.UpdateService(ctx, &apphubpb.UpdateServiceRequest{Service: &apphubpb.Service{Name: svc.GetName(), DisplayName: "Hello"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := usop.Wait(ctx); err != nil || got.GetDisplayName() != "Hello" {
				t.Fatalf("UpdateService: %v, %v", got, err)
			}
			wop, err := c.CreateWorkload(ctx, &apphubpb.CreateWorkloadRequest{Parent: app.GetName(), WorkloadId: "job",
				Workload: &apphubpb.Workload{DiscoveredWorkload: discW.GetName()}})
			if err != nil {
				t.Fatalf("CreateWorkload: %v", err)
			}
			wl, err := wop.Wait(ctx)
			if err != nil || wl.GetWorkloadReference().GetUri() != jobURI {
				t.Fatalf("the registered workload: %v, %v", wl, err)
			}
			if got, err := c.GetWorkload(ctx, &apphubpb.GetWorkloadRequest{Name: wl.GetName()}); err != nil || got.GetState() != apphubpb.Workload_ACTIVE {
				t.Fatalf("GetWorkload: %v, %v", got, err)
			}
			if !listHas(t, c.ListWorkloads(ctx, &apphubpb.ListWorkloadsRequest{Parent: app.GetName()}), func(w *apphubpb.Workload) bool { return w.GetName() == wl.GetName() }) {
				t.Error("ListWorkloads omits the registered workload")
			}
			uwop, err := c.UpdateWorkload(ctx, &apphubpb.UpdateWorkloadRequest{Workload: &apphubpb.Workload{Name: wl.GetName(), Description: "nightly"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := uwop.Wait(ctx); err != nil || got.GetDescription() != "nightly" {
				t.Fatalf("UpdateWorkload: %v, %v", got, err)
			}

			// Boundary, IAM, schemas, locations and operations.
			b, err := c.GetBoundary(ctx, &apphubpb.GetBoundaryRequest{Name: global + "/boundary"})
			if err != nil || b.GetType() != apphubpb.Boundary_MANUAL {
				t.Fatalf("GetBoundary: %v, %v", b, err)
			}
			bop, err := c.UpdateBoundary(ctx, &apphubpb.UpdateBoundaryRequest{Boundary: &apphubpb.Boundary{Name: global + "/boundary",
				Scope: &apphubpb.Boundary_CrmNode{CrmNode: "projects/" + project}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crm_node"}}})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := bop.Wait(ctx); err != nil || got.GetType() != apphubpb.Boundary_AUTOMATIC || !strings.HasPrefix(got.GetCrmNode(), "projects/") {
				t.Fatalf("UpdateBoundary: %v, %v", got, err)
			}
			if _, err := c.UpdateBoundary(ctx, &apphubpb.UpdateBoundaryRequest{Boundary: &apphubpb.Boundary{Name: global + "/boundary"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crm_node"}}}); err != nil {
				t.Fatalf("clearing the boundary: %v", err)
			}
			if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: app.GetName(), Policy: &iampb.Policy{
				Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:dev@example.com"}}}}}); err != nil {
				t.Fatalf("SetIamPolicy: %v", err)
			}
			if p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: app.GetName()}); err != nil || len(p.GetBindings()) != 1 {
				t.Fatalf("GetIamPolicy: %v, %v", p, err)
			}
			if _, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: app.GetName(), Permissions: []string{"apphub.applications.get"}}); err != nil {
				t.Fatalf("TestIamPermissions: %v", err)
			}
			if !listHas(t, c.ListExtendedMetadataSchemas(ctx, &apphubpb.ListExtendedMetadataSchemasRequest{Parent: global}),
				func(s *apphubpb.ExtendedMetadataSchema) bool {
					return strings.HasSuffix(s.GetName(), "/apphub.googleapis.com/McpServer")
				}) {
				t.Error("ListExtendedMetadataSchemas omits apphub.googleapis.com/McpServer")
			}
			if s, err := c.GetExtendedMetadataSchema(ctx, &apphubpb.GetExtendedMetadataSchemaRequest{Name: global + "/extendedMetadataSchemas/apphub.googleapis.com/AgentProperties"}); err != nil ||
				!strings.Contains(s.GetJsonSchema(), "agentCardUri") {
				t.Fatalf("GetExtendedMetadataSchema: %v, %v", s, err)
			}
			if !listHas(t, c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project}), func(l *locationpb.Location) bool { return l.GetLocationId() == "us-central1" }) {
				t.Error("ListLocations omits us-central1")
			}
			if l, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: global}); err != nil || l.GetLocationId() != "global" {
				t.Fatalf("GetLocation: %v, %v", l, err)
			}
			if o, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: aop.Name()}); err != nil || !o.GetDone() {
				t.Fatalf("GetOperation: %v, %v", o, err)
			}
			if !listHas(t, c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global}), func(o *longrunningpb.Operation) bool { return o.GetName() == aop.Name() }) {
				t.Error("ListOperations omits the application's create")
			}

			// Unregister, delete and detach.
			if _, err := c.DeleteApplication(ctx, &apphubpb.DeleteApplicationRequest{Name: app.GetName()}); pmCode(err) != codes.FailedPrecondition {
				t.Fatalf("deleting an application with registrations: %v, want FAILED_PRECONDITION", err)
			}
			dsop, err := c.DeleteService(ctx, &apphubpb.DeleteServiceRequest{Name: svc.GetName()})
			if err != nil || dsop.Wait(ctx) != nil {
				t.Fatalf("DeleteService: %v", err)
			}
			dwop, err := c.DeleteWorkload(ctx, &apphubpb.DeleteWorkloadRequest{Name: wl.GetName()})
			if err != nil || dwop.Wait(ctx) != nil {
				t.Fatalf("DeleteWorkload: %v", err)
			}
			for _, n := range []string{app.GetName(), east.GetName()} {
				dop, err := c.DeleteApplication(ctx, &apphubpb.DeleteApplicationRequest{Name: n})
				if err != nil || dop.Wait(ctx) != nil {
					t.Fatalf("DeleteApplication %s: %v", n, err)
				}
				if _, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: n}); pmCode(err) != codes.NotFound {
					t.Errorf("GetApplication after delete: %v", err)
				}
			}
			if transport == "grpc" {
				if _, err := c.DetachServiceProjectAttachment(ctx, &apphubpb.DetachServiceProjectAttachmentRequest{Name: global}); err != nil {
					t.Fatalf("DetachServiceProjectAttachment: %v", err)
				}
			} else {
				dop, err := c.DeleteServiceProjectAttachment(ctx, &apphubpb.DeleteServiceProjectAttachmentRequest{Name: spa.GetName()})
				if err != nil || dop.Wait(ctx) != nil {
					t.Fatalf("DeleteServiceProjectAttachment: %v", err)
				}
			}
			if lk, err := c.LookupServiceProjectAttachment(ctx, &apphubpb.LookupServiceProjectAttachmentRequest{Name: global}); err != nil || lk.GetServiceProjectAttachment() != nil {
				t.Fatalf("after detaching: %v, %v", lk, err)
			}
		})
	}
}

// listHas reports whether an iterator yields an item with ok, failing the
// test on an error.
func listHas[T any](t *testing.T, it interface{ Next() (T, error) }, ok func(T) bool) bool {
	t.Helper()
	for {
		v, err := it.Next()
		if err == iterator.Done {
			return false
		}
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if ok(v) {
			return true
		}
	}
}
