//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	notebooks "cloud.google.com/go/notebooks/apiv2"
	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// workbenchClients are the official Workbench (notebooks.googleapis.com v2)
// clients, gRPC and REST, at the local Vertex AI endpoint the run shard's
// instance serves.
func workbenchClients(t *testing.T, h *Harness) (string, map[string]*notebooks.NotebookClient) {
	t.Helper()
	addr := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	ctx := h.Context()
	g, err := notebooks.NewNotebookClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("notebooks.NewNotebookClient: %v", err)
	}
	r, err := notebooks.NewNotebookRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("notebooks.NewNotebookRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return addr, map[string]*notebooks.NotebookClient{"grpc": g, "rest": r}
}

// covers: google.cloud.notebooks.v2.NotebookService/CreateInstance, google.cloud.notebooks.v2.NotebookService/GetInstance, google.cloud.notebooks.v2.NotebookService/ListInstances, google.cloud.notebooks.v2.NotebookService/UpdateInstance, google.cloud.notebooks.v2.NotebookService/StopInstance, google.cloud.notebooks.v2.NotebookService/StartInstance, google.cloud.notebooks.v2.NotebookService/ResetInstance, google.cloud.notebooks.v2.NotebookService/CheckInstanceUpgradability, google.cloud.notebooks.v2.NotebookService/UpgradeInstance, google.cloud.notebooks.v2.NotebookService/DeleteInstance
//
// TestWorkbenchInstances (#2173): with the official
// cloud.google.com/go/notebooks/apiv2 client over gRPC and REST, an
// instance created with only its ID is a notebook runtime of the
// instance's: the operation (projects/P/locations/ZONE/operations/ID) ends
// with it ACTIVE and HEALTHY, with Google's documented defaults (boot disk
// 150 GB, data disk 100 GB, the cloud-notebooks-managed image project);
// its labels and idle-timeout-seconds metadata update, its machine type
// only while STOPPED; it stops, starts, resets, is not upgradeable, lists
// with a filter and under locations/-, and is deleted (NOT_FOUND after).
// The documented ID, owner, disk and idle-shutdown limits are
// INVALID_ARGUMENT, accelerators FAILED_PRECONDITION and a second
// instance of the same ID ALREADY_EXISTS.
func TestWorkbenchInstances(t *testing.T) {
	h := New(t)
	addr, clients := workbenchClients(t, h)
	// The kernel queries the instance's BigQuery.
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	zone := "us-central1-a"
	parent := fmt.Sprintf("projects/%s/locations/%s", project, zone)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			setup := func(g *nbpb.GceSetup) *nbpb.Instance {
				return &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: g}}
			}
			for _, bad := range []*nbpb.CreateInstanceRequest{
				{Parent: parent, InstanceId: "Bad-" + suffix, Instance: &nbpb.Instance{}},
				{Parent: parent, InstanceId: "ends-", Instance: &nbpb.Instance{}},
				{Parent: parent, InstanceId: "wb-" + suffix, Instance: &nbpb.Instance{InstanceOwners: []string{"a@example.com", "b@example.com"}}},
				{Parent: parent, InstanceId: "wb-" + suffix, Instance: setup(&nbpb.GceSetup{DataDisks: []*nbpb.DataDisk{{}, {}}})},
				{Parent: parent, InstanceId: "wb-" + suffix, Instance: setup(&nbpb.GceSetup{BootDisk: &nbpb.BootDisk{DiskSizeGb: 64001}})},
				{Parent: parent, InstanceId: "wb-" + suffix, Instance: setup(&nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": "300"}})},
			} {
				if _, err := c.CreateInstance(ctx, bad); quotaCode(err) != codes.InvalidArgument {
					t.Errorf("CreateInstance %v = %v, want INVALID_ARGUMENT", bad, err)
				}
			}
			if _, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "wb-gpu-" + suffix,
				Instance: setup(&nbpb.GceSetup{AcceleratorConfigs: []*nbpb.AcceleratorConfig{{Type: nbpb.AcceleratorConfig_NVIDIA_TESLA_T4, CoreCount: 1}}})}); quotaCode(err) != codes.FailedPrecondition {
				t.Errorf("an instance with a GPU = %v, want FAILED_PRECONDITION", err)
			}

			id := "wb-" + transport + "-" + suffix
			op, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: id,
				Instance: &nbpb.Instance{Labels: map[string]string{"env": "dev"}}})
			if err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}
			if !strings.HasPrefix(op.Name(), parent+"/operations/") {
				t.Errorf("operation %s, want one of %s/operations", op.Name(), parent)
			}
			in, err := op.Wait(ctx)
			if err != nil {
				t.Fatalf("the create operation: %v", err)
			}
			t.Cleanup(func() {
				if op, err := c.DeleteInstance(context.Background(), &nbpb.DeleteInstanceRequest{Name: in.GetName()}); err == nil {
					_ = op.Wait(context.Background())
				}
			})
			g := in.GetGceSetup()
			if in.GetState() != nbpb.State_ACTIVE || in.GetHealthState() != nbpb.HealthState_HEALTHY || in.GetId() == "" ||
				g.GetBootDisk().GetDiskSizeGb() != 150 || len(g.GetDataDisks()) != 1 || g.GetDataDisks()[0].GetDiskSizeGb() != 100 ||
				g.GetVmImage().GetProject() != "cloud-notebooks-managed" || g.GetMachineType() == "" || in.GetCreateTime() == nil {
				t.Errorf("created instance = %v", in)
			}
			if _, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &nbpb.Instance{}}); quotaCode(err) != codes.AlreadyExists {
				t.Errorf("the same ID again = %v, want ALREADY_EXISTS", err)
			}
			if code, body := notebookJSON(t, http.MethodGet, "http://"+addr+"/v2/"+in.GetName(), ""); code != http.StatusOK || !strings.Contains(body, `"ACTIVE"`) {
				t.Errorf("JSON get = %d %s", code, body)
			}

			upd, err := c.UpdateInstance(ctx, &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: in.GetName(), Labels: map[string]string{"env": "prod"},
				Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": "3600"}}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "gce_setup.metadata"}}})
			if err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}
			if got, err := upd.Wait(ctx); err != nil || got.GetLabels()["env"] != "prod" || got.GetGceSetup().GetMetadata()["idle-timeout-seconds"] != "3600" {
				t.Errorf("updated = %v, %v", got, err)
			}
			machine := &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: in.GetName(), Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{MachineType: "e2-standard-8"}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"gce_setup.machine_type"}}}
			if _, err := c.UpdateInstance(ctx, machine); quotaCode(err) != codes.FailedPrecondition {
				t.Errorf("the machine type of an ACTIVE instance = %v, want FAILED_PRECONDITION", err)
			}

			it := c.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: parent, Filter: `labels.env="prod" AND state=ACTIVE`})
			found := false
			for {
				x, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("ListInstances: %v", err)
				}
				found = found || x.GetName() == in.GetName()
			}
			if !found {
				t.Errorf("ListInstances with a filter did not list %s", in.GetName())
			}
			if x, err := c.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-", Filter: fmt.Sprintf("name=%q", in.GetName())}).Next(); err != nil || x.GetName() != in.GetName() {
				t.Errorf("ListInstances under locations/- = %v, %v", x, err)
			}

			stop, err := c.StopInstance(ctx, &nbpb.StopInstanceRequest{Name: in.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := stop.Wait(ctx); err != nil || got.GetState() != nbpb.State_STOPPED {
				t.Errorf("after stop = %v, %v", got.GetState(), err)
			}
			mop, err := c.UpdateInstance(ctx, machine)
			if err != nil {
				t.Fatalf("the machine type of a STOPPED instance: %v", err)
			}
			if got, err := mop.Wait(ctx); err != nil || got.GetGceSetup().GetMachineType() != "e2-standard-8" {
				t.Errorf("machine type = %v, %v", got.GetGceSetup().GetMachineType(), err)
			}
			if _, err := c.ResetInstance(ctx, &nbpb.ResetInstanceRequest{Name: in.GetName()}); quotaCode(err) != codes.FailedPrecondition {
				t.Errorf("reset a STOPPED instance = %v, want FAILED_PRECONDITION", err)
			}
			start, err := c.StartInstance(ctx, &nbpb.StartInstanceRequest{Name: in.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := start.Wait(ctx); err != nil || got.GetState() != nbpb.State_ACTIVE {
				t.Errorf("after start = %v, %v", got.GetState(), err)
			}
			reset, err := c.ResetInstance(ctx, &nbpb.ResetInstanceRequest{Name: in.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := reset.Wait(ctx); err != nil || got.GetState() != nbpb.State_ACTIVE {
				t.Errorf("after reset = %v, %v", got.GetState(), err)
			}
			if u, err := c.CheckInstanceUpgradability(ctx, &nbpb.CheckInstanceUpgradabilityRequest{NotebookInstance: in.GetName()}); err != nil || u.GetUpgradeable() {
				t.Errorf("CheckInstanceUpgradability = %v, %v", u, err)
			}
			if _, err := c.UpgradeInstance(ctx, &nbpb.UpgradeInstanceRequest{Name: in.GetName()}); quotaCode(err) != codes.FailedPrecondition {
				t.Errorf("UpgradeInstance = %v, want FAILED_PRECONDITION", err)
			}

			del, err := c.DeleteInstance(ctx, &nbpb.DeleteInstanceRequest{Name: in.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if err := del.Wait(ctx); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := c.GetInstance(ctx, &nbpb.GetInstanceRequest{Name: in.GetName()}); quotaCode(err) != codes.NotFound {
				t.Errorf("GetInstance after delete = %v, want NOT_FOUND", err)
			}
		})
	}
}

// covers: google.cloud.notebooks.v2.NotebookService/ListInstances, google.cloud.notebooks.v2.NotebookService/CreateInstance
//
// TestIAMEnforcementWorkbench (#2173): with enforcement on, a principal
// with no role is refused ListInstances (notebooks.instances.list on the
// parent) and CreateInstance (notebooks.instances.create) in Google's
// newer APIs' words; Notebooks Viewer lists, and Notebooks Runner
// creates (past the check, an invalid ID is the request's own
// INVALID_ARGUMENT, so no instance starts).
func TestIAMEnforcementWorkbench(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLocalAI)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iewb-")
	p := newIAMPrincipal(t, h, project, "iewb")
	as, err := notebooks.NewNotebookClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	parent := "projects/" + project + "/locations/us-central1-a"
	list := func() error {
		_, err := as.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: parent}).Next()
		return first(err)
	}
	create := func() error {
		_, err := as.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "Not-Valid", Instance: &nbpb.Instance{}})
		return err
	}
	wantDenied(t, "ListInstances without a role", list(), newerDenied("notebooks.instances.list", parent), "notebooks.instances.list", true)
	wantDenied(t, "CreateInstance without a role", create(), newerDenied("notebooks.instances.create", parent), "notebooks.instances.create", true)
	p.grant("roles/notebooks.viewer")
	waitGranted(t, "ListInstances as Notebooks Viewer", list)
	wantDenied(t, "CreateInstance as Notebooks Viewer", create(), newerDenied("notebooks.instances.create", parent), "notebooks.instances.create", true)
	p.grant("roles/notebooks.runner")
	var last error
	for i := 0; i < 10; i++ {
		if last = create(); quotaCode(last) == codes.InvalidArgument {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("CreateInstance as Notebooks Runner = %v, want the request's INVALID_ARGUMENT", last)
}
