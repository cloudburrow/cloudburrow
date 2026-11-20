//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	functions "cloud.google.com/go/functions/apiv2"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	privateca "cloud.google.com/go/security/privateca/apiv1"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"golang.org/x/oauth2"
	ak "google.golang.org/api/apikeys/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// wantHTTPDenied fails unless err is a 403 whose message is msg.
func wantHTTPDenied(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusForbidden || ge.Message != msg {
		t.Fatalf("%s = %v; want 403 %q", what, err, msg)
	}
}

// waitHTTPGranted retries call while it is a 403.
func waitHTTPGranted(t *testing.T, what string, call func() error) {
	t.Helper()
	waitGranted(t, what, func() error {
		err := call()
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code != http.StatusForbidden {
			return nil // past its IAM check
		}
		return err
	})
}

// TestIAMEnforcementPrivateCA (#1879), with the official Certificate
// Authority Service client: without a role ListCaPools is refused; CA
// Service Auditor on the project lists and reads (a missing pool
// NOT_FOUND) but cannot create a pool; the instance's own account creates
// one, through its operation.
func TestIAMEnforcementPrivateCA(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvPrivateCA)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iepca7-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iepca7")
	c, err := privateca.NewCertificateAuthorityClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	list := func() error {
		_, err := c.ListCaPools(ctx, &privatecapb.ListCaPoolsRequest{Parent: parent}).Next()
		return first(err)
	}
	wantDenied(t, "ListCaPools without a role", list(), newerDenied("privateca.caPools.list", parent), "privateca.caPools.list", true)
	p.grant("roles/privateca.auditor")
	t.Cleanup(func() { p.revoke("roles/privateca.auditor") })
	waitGranted(t, "ListCaPools as a CA Service Auditor", list)
	if _, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: parent + "/caPools/missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetCaPool of a missing pool as an Auditor = %v; want NOT_FOUND", err)
	}
	pool := &privatecapb.CaPool{Tier: privatecapb.CaPool_DEVOPS}
	_, err = c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: parent, CaPoolId: "enforced", CaPool: pool})
	wantDenied(t, "CreateCaPool as an Auditor", err, newerDenied("privateca.caPools.create", parent), "privateca.caPools.create", true)
	owner, err := privateca.NewCertificateAuthorityClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	op, err := owner.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: parent, CaPoolId: "owned", CaPool: proto.Clone(pool).(*privatecapb.CaPool)})
	if err != nil {
		t.Fatalf("CreateCaPool as the instance's account: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateCaPool's operation: %v", err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteCaPool(h.Context(), &privatecapb.DeleteCaPoolRequest{Name: parent + "/caPools/owned"}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	if _, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: parent + "/caPools/owned"}); err != nil {
		t.Fatalf("GetCaPool as an Auditor: %v", err)
	}
}

// TestIAMEnforcementGKE (#1879), with the official Kubernetes Engine
// client: without a role ListClusters is refused in GKE's words;
// Kubernetes Engine Viewer lists, reads (a missing cluster NOT_FOUND) and
// gets the server config but cannot create a cluster (refused before any
// cluster is made); the instance's own account lists.
func TestIAMEnforcementGKE(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvGKE)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iegke7-")
	parent := "projects/" + project + "/locations/-"
	p := newIAMPrincipal(t, h, project, "iegke7")
	c, err := container.NewClusterManagerClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	list := func() error {
		_, err := c.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: parent})
		return err
	}
	wantDenied(t, "ListClusters without a role", list(), `Required "container.clusters.list" permission(s) for "`+parent+`".`, "container.clusters.list", false)
	p.grant("roles/container.viewer")
	t.Cleanup(func() { p.revoke("roles/container.viewer") })
	waitGranted(t, "ListClusters as a Kubernetes Engine Viewer", list)
	name := "projects/" + project + "/locations/us-central1/clusters/missing"
	if _, err := c.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetCluster of a missing cluster as a Viewer = %v; want NOT_FOUND", err)
	}
	if _, err := c.GetServerConfig(ctx, &containerpb.GetServerConfigRequest{Name: "projects/" + project + "/locations/us-central1"}); err != nil {
		t.Fatalf("GetServerConfig as a Viewer: %v", err)
	}
	loc := "projects/" + project + "/locations/us-central1"
	_, err = c.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: loc, Cluster: &containerpb.Cluster{Name: "enforced", InitialNodeCount: 1}})
	wantDenied(t, "CreateCluster as a Viewer", err, `Required "container.clusters.create" permission(s) for "`+loc+`".`, "container.clusters.create", false)
	owner, err := container.NewClusterManagerClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: parent}); err != nil {
		t.Fatalf("ListClusters as the instance's account: %v", err)
	}
}

// computeAs are the Compute REST clients' options for token ("" none: the
// instance's own account).
func computeAs(endpoint, token string) []option.ClientOption {
	opts := []option.ClientOption{option.WithEndpoint("http://" + stripScheme(endpoint))}
	if token == "" {
		return append(opts, option.WithoutAuthentication())
	}
	return append(opts, option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}}))
}

// TestIAMEnforcementCompute (#1879), with the official Compute Engine REST
// clients: without a role networks.list is a 403 in Compute's words, while
// a public image family still reads; Compute Viewer lists (a missing
// network 404) but cannot insert one; the instance's own account lists.
func TestIAMEnforcementCompute(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvCompute)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iece7-")
	p := newIAMPrincipal(t, h, project, "iece7")
	nets, err := computeapi.NewNetworksRESTClient(ctx, computeAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nets.Close() })
	images, err := computeapi.NewImagesRESTClient(ctx, computeAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = images.Close() })
	list := func() error {
		_, err := nets.List(ctx, &computepb.ListNetworksRequest{Project: project}).Next()
		return first(err)
	}
	wantHTTPDenied(t, "networks.list without a role", list(), "Required 'compute.networks.list' permission for 'projects/"+project+"'")
	if _, err := images.GetFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: "debian-cloud", Family: "debian-12"}); err != nil {
		t.Fatalf("images.getFromFamily of a public image without a role: %v", err)
	}
	p.grant("roles/compute.viewer")
	t.Cleanup(func() { p.revoke("roles/compute.viewer") })
	waitHTTPGranted(t, "networks.list as a Compute Viewer", list)
	if _, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: project, Network: "missing"}); computeCode(err) != http.StatusNotFound {
		t.Fatalf("networks.get of a missing network as a Viewer = %v; want 404", err)
	}
	_, err = nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{Name: proto.String("enforced"),
		AutoCreateSubnetworks: proto.Bool(false)}})
	wantHTTPDenied(t, "networks.insert as a Viewer", err, "Required 'compute.networks.create' permission for 'projects/"+project+"'")
	owner, err := computeapi.NewNetworksRESTClient(ctx, computeAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.List(ctx, &computepb.ListNetworksRequest{Project: project}).Next(); first(err) != nil {
		t.Fatalf("networks.list as the instance's account: %v", err)
	}
}

// TestIAMEnforcementAPIKeysServiceUsage (#1879), with the official API
// Keys and Service Usage clients (google.golang.org/api): without a role
// keys.list and services.list are 403s; API Keys Viewer lists keys but
// cannot create one, and Service Usage Viewer lists and reads services but
// cannot enable one; the instance's own account lists both.
func TestIAMEnforcementAPIKeysServiceUsage(t *testing.T) {
	h := New(t)
	keysEndpoint := h.Endpoint(EnvAPIKeys)
	usageEndpoint := h.Endpoint(EnvServiceUsage)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieak7-")
	p := newIAMPrincipal(t, h, project, "ieak7")
	keys, err := ak.NewService(ctx, restAs(keysEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := su.NewService(ctx, restAs(usageEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	loc := "projects/" + project + "/locations/global"
	listKeys := func() error { _, err := keys.Projects.Locations.Keys.List(loc).Context(ctx).Do(); return err }
	listServices := func() error { _, err := usage.Services.List("projects/" + project).Context(ctx).Do(); return err }
	wantHTTPDenied(t, "keys.list without a role", listKeys(), newerDenied("apikeys.keys.list", loc))
	wantHTTPDenied(t, "services.list without a role", listServices(), newerDenied("serviceusage.services.list", "projects/"+project))
	p.grant("roles/serviceusage.apiKeysViewer")
	t.Cleanup(func() { p.revoke("roles/serviceusage.apiKeysViewer") })
	waitHTTPGranted(t, "keys.list as an API Keys Viewer", listKeys)
	_, err = keys.Projects.Locations.Keys.Create(loc, &ak.V2Key{DisplayName: "enforced"}).Context(ctx).Do()
	wantHTTPDenied(t, "keys.create as an API Keys Viewer", err, newerDenied("apikeys.keys.create", loc))
	p.grant("roles/serviceusage.serviceUsageViewer")
	t.Cleanup(func() { p.revoke("roles/serviceusage.serviceUsageViewer") })
	waitHTTPGranted(t, "services.list as a Service Usage Viewer", listServices)
	svc := "projects/" + project + "/services/pubsub.googleapis.com"
	if _, err := usage.Services.Get(svc).Context(ctx).Do(); err != nil {
		t.Fatalf("services.get as a Service Usage Viewer: %v", err)
	}
	_, err = usage.Services.Enable(svc, &su.EnableServiceRequest{}).Context(ctx).Do()
	wantHTTPDenied(t, "services.enable as a Service Usage Viewer", err, newerDenied("serviceusage.services.enable", svc))
	ownerKeys, err := ak.NewService(ctx, option.WithEndpoint("http://"+stripScheme(keysEndpoint)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerKeys.Projects.Locations.Keys.List(loc).Context(ctx).Do(); err != nil {
		t.Fatalf("keys.list as the instance's account: %v", err)
	}
	ownerUsage, err := su.NewService(ctx, option.WithEndpoint("http://"+stripScheme(usageEndpoint)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerUsage.Services.Get(svc).Context(ctx).Do(); err != nil {
		t.Fatalf("services.get as the instance's account: %v", err)
	}
}

// TestIAMEnforcementRunPortMixins (#1879), with the official Cloud Run and
// Cloud Functions clients: without a role UploadSource, ListOperations and
// ListLocations are refused on the Cloud Run port; Cloud Run Viewer lists
// operations and locations but cannot upload source; the instance's own
// account lists operations.
func TestIAMEnforcementRunPortMixins(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iemx7-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iemx7")
	sc, err := run.NewServicesClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	up, err := run.NewSourceUploadClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	fc, err := functions.NewFunctionClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	ops := func() error {
		_, err := sc.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent}).Next()
		return first(err)
	}
	locs := func() error {
		_, err := fc.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project}).Next()
		return first(err)
	}
	upload := func() error {
		_, err := up.UploadSource(ctx, &runpb.UploadSourceRequest{Parent: parent})
		return err
	}
	wantDenied(t, "UploadSource without a role", upload(), newerDenied("run.locations.uploadSource", parent), "run.locations.uploadSource", true)
	wantDenied(t, "ListOperations without a role", ops(), newerDenied("run.operations.list", parent), "run.operations.list", true)
	wantDenied(t, "ListLocations without a role", locs(), newerDenied("run.locations.list", "projects/"+project), "run.locations.list", true)
	p.grant("roles/run.viewer")
	t.Cleanup(func() { p.revoke("roles/run.viewer") })
	waitGranted(t, "ListOperations as a Cloud Run Viewer", ops)
	waitGranted(t, "ListLocations as a Cloud Run Viewer", locs)
	wantDenied(t, "UploadSource as a Cloud Run Viewer", upload(), newerDenied("run.locations.uploadSource", parent), "run.locations.uploadSource", true)
	owner, err := run.NewServicesClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent}).Next(); first(err) != nil {
		t.Fatalf("ListOperations as the instance's account: %v", err)
	}
	ownerUp, err := run.NewSourceUploadClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerUp.Close() })
	_, err = ownerUp.UploadSource(ctx, &runpb.UploadSourceRequest{Parent: parent})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "uploadSource") {
		t.Fatalf("UploadSource without media as the instance's account = %v; want INVALID_ARGUMENT", err)
	}
}
