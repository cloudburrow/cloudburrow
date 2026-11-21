//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	adminpb "cloud.google.com/go/iam/admin/apiv1/adminpb"
	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystore "cloud.google.com/go/memorystore/apiv1"
	redis "cloud.google.com/go/redis/apiv1"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	logrest "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// covers: google.logging.v2.LoggingServiceV2/TailLogEntries
//
// TestIAMEnforcementLoggingTailAndExtras (#1879, round 13), with the
// official Logging clients (gRPC and google.golang.org/api/logging/v2) and
// a minted token: without a role, TailLogEntries' stream is refused
// logging.logEntries.list on its resource, and saved queries' and log
// scopes' list are refused logging.queries.list and logging.logScopes.list,
// in Logging's words; as a Logs Viewer the stream opens and log scopes
// list, but saved queries (logging.queries.*, which no predefined Logging
// role holds) are still refused until Editor; the instance's own account
// is never refused.
func TestIAMEnforcementLoggingTailAndExtras(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLogging)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iel13-")
	pr := "projects/" + project
	parent := pr + "/locations/global"
	p := newIAMPrincipal(t, h, project, "iel13")
	denied := func(perm string) string { return "Permission '" + perm + "' denied on resource (or it may not exist)." }
	client := func(opts []option.ClientOption) *logging.Client {
		c, err := logging.NewClient(ctx, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	as, owner := client(grpcAs(endpoint, p.token)), client(ownerTokenAs(t, endpoint))
	// tail opens a stream and reads its first answer: a refusal, or, once
	// past its check, nothing before the deadline.
	tail := func(c *logging.Client) error {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		s, err := c.TailLogEntries(tctx)
		if err != nil {
			return err
		}
		if err := s.Send(&loggingpb.TailLogEntriesRequest{ResourceNames: []string{pr}}); err != nil {
			return err
		}
		_, err = s.Recv()
		if status.Code(err) == codes.DeadlineExceeded {
			return nil
		}
		return err
	}
	rest := func(token string) *logrest.Service {
		svc, err := logrest.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(endpoint, "http://")+"/"),
			option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}}))
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	asREST := rest(p.token)
	queries := func(s *logrest.Service) error {
		_, err := s.Projects.Locations.SavedQueries.List(parent).Context(ctx).Do()
		return err
	}
	scopes := func(s *logrest.Service) error {
		_, err := s.Projects.Locations.LogScopes.List(parent).Context(ctx).Do()
		return err
	}
	createScope := func(s *logrest.Service) error {
		_, err := s.Projects.Locations.LogScopes.Create(parent, &logrest.LogScope{ResourceNames: []string{pr}}).
			LogScopeId("enforced13").Context(ctx).Do()
		return err
	}

	if err := tail(owner); err != nil {
		t.Fatalf("TailLogEntries as the instance's account: %v", err)
	}
	wantDenied(t, "TailLogEntries without a role", tail(as), denied("logging.logEntries.list"), "logging.logEntries.list", true)
	wantHTTPDenied(t, "savedQueries.list without a role", queries(asREST), denied("logging.queries.list"))
	wantHTTPDenied(t, "logScopes.list without a role", scopes(asREST), denied("logging.logScopes.list"))

	p.grant("roles/logging.viewer")
	t.Cleanup(func() { p.revoke("roles/logging.viewer") })
	waitGranted(t, "TailLogEntries as a Logs Viewer", func() error { return tail(as) })
	waitHTTPGranted(t, "logScopes.list as a Logs Viewer", func() error { return scopes(asREST) })
	wantHTTPDenied(t, "logScopes.create as a Logs Viewer", createScope(asREST), denied("logging.logScopes.create"))
	wantHTTPDenied(t, "savedQueries.list as a Logs Viewer", queries(asREST), denied("logging.queries.list"))

	p.grant("roles/editor")
	t.Cleanup(func() { p.revoke("roles/editor") })
	waitHTTPGranted(t, "savedQueries.list as an Editor", func() error { return queries(asREST) })

	ownerREST := rest("")
	if err := queries(ownerREST); err != nil {
		t.Errorf("savedQueries.list as the instance's account: %v", err)
	}
	if err := scopes(ownerREST); err != nil {
		t.Errorf("logScopes.list as the instance's account: %v", err)
	}
}

// TestIAMEnforcementMemorystoreMixins (#1879, round 13), with the official
// Memorystore for Redis and Valkey clients (gRPC, and Valkey's REST
// client): without a role, ListLocations and ListOperations are refused
// redis.locations.list and redis.operations.list (the port's one mixin
// cannot tell the APIs apart over gRPC, so the denial names Redis'), and
// Valkey's REST ListLocations memorystore.locations.list; Memorystore
// Viewer (memorystore.*) lets the gRPC calls through on both clients and
// Valkey's REST list; the instance's own account lists both.
func TestIAMEnforcementMemorystoreMixins(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvMemorystoreAdmin)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iemm13-")
	pr := "projects/" + project
	parent := pr + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iemm13")
	rc, err := redis.NewCloudRedisClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := memorystore.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	vrest, err := memorystore.NewRESTClient(ctx, option.WithEndpoint("http://"+stripScheme(endpoint)+"/memorystore"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	orc, err := redis.NewCloudRedisClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close(); _ = vc.Close(); _ = vrest.Close(); _ = orc.Close() })
	done := func(err error) error {
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	redisLocs := func(c *redis.CloudRedisClient) error {
		_, err := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: pr}).Next()
		return done(err)
	}
	redisOps := func(c *redis.CloudRedisClient) error {
		_, err := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent}).Next()
		return done(err)
	}
	valkeyLocs := func(c *memorystore.Client) error {
		_, err := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: pr}).Next()
		return done(err)
	}

	if err := redisLocs(orc); err != nil {
		t.Fatalf("ListLocations as the instance's account: %v", err)
	}
	if err := redisOps(orc); err != nil {
		t.Fatalf("ListOperations as the instance's account: %v", err)
	}
	wantDenied(t, "Redis ListLocations without a role", redisLocs(rc), newerDenied("redis.locations.list", pr), "redis.locations.list", true)
	wantDenied(t, "Redis ListOperations without a role", redisOps(rc), newerDenied("redis.operations.list", parent), "redis.operations.list", true)
	wantDenied(t, "Valkey ListLocations without a role", valkeyLocs(vc), newerDenied("redis.locations.list", pr), "redis.locations.list", true)
	if err := valkeyLocs(vrest); err == nil || !strings.Contains(err.Error(), "403") ||
		!strings.Contains(err.Error(), newerDenied("memorystore.locations.list", pr)) {
		t.Fatalf("Valkey ListLocations over REST without a role = %v; want 403 memorystore.locations.list", err)
	}

	p.grant("roles/memorystore.viewer")
	t.Cleanup(func() { p.revoke("roles/memorystore.viewer") })
	waitGranted(t, "Valkey ListLocations as a Memorystore Viewer", func() error { return valkeyLocs(vc) })
	waitGranted(t, "Valkey ListLocations over REST as a Memorystore Viewer", func() error { return valkeyLocs(vrest) })
	if err := redisLocs(rc); err != nil {
		t.Errorf("ListLocations (the shared mixin) as a Memorystore Viewer: %v", err)
	}
	if err := redisOps(rc); err != nil {
		t.Errorf("ListOperations (the shared mixin) as a Memorystore Viewer: %v", err)
	}
}

// covers: compute.addresses.insert, compute.firewalls.patch
//
// TestIAMEnforcementComputeRound13 (#1879, round 13), with the official
// Compute Engine REST clients: an INTERNAL address's insert is refused
// compute.addresses.createInternal without a role and allowed as a Compute
// Network User, which holds createInternal (and subnetworks.use) but not
// compute.addresses.create; a firewall patch by a custom role holding
// compute.firewalls.update alone is refused compute.firewalls.get, which
// its page also lists, and Compute Security Admin patches; the instance's
// own account does all three.
func TestIAMEnforcementComputeRound13(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvCompute)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iec13-")
	pr := "projects/" + project
	region := "us-central1"
	p := newIAMPrincipal(t, h, project, "iec13")
	owner := computeAs(endpoint, "")
	onets, err := computeapi.NewNetworksRESTClient(ctx, owner...)
	if err != nil {
		t.Fatal(err)
	}
	osubs, err := computeapi.NewSubnetworksRESTClient(ctx, owner...)
	if err != nil {
		t.Fatal(err)
	}
	ofws, err := computeapi.NewFirewallsRESTClient(ctx, owner...)
	if err != nil {
		t.Fatal(err)
	}
	oaddrs, err := computeapi.NewAddressesRESTClient(ctx, owner...)
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := computeapi.NewAddressesRESTClient(ctx, computeAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	fws, err := computeapi.NewFirewallsRESTClient(ctx, computeAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = onets.Close()
		_ = osubs.Close()
		_ = ofws.Close()
		_ = oaddrs.Close()
		_ = addrs.Close()
		_ = fws.Close()
	})
	wait := func(what string, op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("%s as the instance's account: %v", what, err)
		}
	}
	op, err := onets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{
		Name: proto.String("net13"), AutoCreateSubnetworks: proto.Bool(false)}})
	wait("networks.insert", op, err)
	network := pr + "/global/networks/net13"
	op, err = osubs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: project, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String("sub13"), Network: proto.String(network), IpCidrRange: proto.String("10.13.0.0/24")}})
	wait("subnetworks.insert", op, err)
	subnet := pr + "/regions/" + region + "/subnetworks/sub13"
	op, err = ofws.Insert(ctx, &computepb.InsertFirewallRequest{Project: project, FirewallResource: &computepb.Firewall{
		Name: proto.String("fw13"), Network: proto.String(network), SourceRanges: []string{"10.0.0.0/8"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}}}})
	wait("firewalls.insert", op, err)

	n := 0
	internal := func(c *computeapi.AddressesClient) error {
		n++
		op, err := c.Insert(ctx, &computepb.InsertAddressRequest{Project: project, Region: region, AddressResource: &computepb.Address{
			Name: proto.String("internal13-" + string(rune('a'+n))), AddressType: proto.String("INTERNAL"), Subnetwork: proto.String(subnet)}})
		// Not waited for: reading the operation is
		// compute.regionOperations.get, which Network User lacks.
		if err == nil && op.Proto().GetError() != nil {
			err = errors.New(op.Proto().GetError().String())
		}
		return err
	}
	patch := func(c *computeapi.FirewallsClient) error {
		op, err := c.Patch(ctx, &computepb.PatchFirewallRequest{Project: project, Firewall: "fw13",
			FirewallResource: &computepb.Firewall{Description: proto.String("patched")}})
		if err == nil && op.Proto().GetError() != nil {
			err = errors.New(op.Proto().GetError().String())
		}
		return err
	}
	if err := internal(oaddrs); err != nil {
		t.Fatalf("an INTERNAL address's insert as the instance's account: %v", err)
	}
	if err := patch(ofws); err != nil {
		t.Fatalf("firewalls.patch as the instance's account: %v", err)
	}
	wantHTTPDenied(t, "an INTERNAL address's insert without a role", internal(addrs),
		"Required 'compute.addresses.createInternal' permission for '"+pr+"'")
	p.grant("roles/compute.networkUser")
	t.Cleanup(func() { p.revoke("roles/compute.networkUser") })
	waitHTTPGranted(t, "an INTERNAL address's insert as a Compute Network User", func() error { return internal(addrs) })
	if err := internal(addrs); err != nil {
		t.Errorf("an INTERNAL address's insert as a Compute Network User: %v", err)
	}

	// Network User also reads firewalls: the patch is checked without it.
	p.revoke("roles/compute.networkUser")
	role, err := iamAdmin(t, h).CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: pr, RoleId: "firewallUpdate13",
		Role: &adminpb.Role{Title: "firewall update", IncludedPermissions: []string{"compute.firewalls.update"}}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	t.Cleanup(func() {
		_, _ = iamAdmin(t, h).DeleteRole(context.Background(), &adminpb.DeleteRoleRequest{Name: role.GetName()})
	})
	p.grant(role.GetName())
	t.Cleanup(func() { p.revoke(role.GetName()) })
	firewall := pr + "/global/firewalls/fw13"
	waitGranted(t, "firewalls.patch past compute.firewalls.update", func() error {
		err := patch(fws)
		if err != nil && strings.Contains(err.Error(), "compute.firewalls.update") {
			return err
		}
		return nil
	})
	wantHTTPDenied(t, "firewalls.patch with compute.firewalls.update alone", patch(fws),
		"Required 'compute.firewalls.get' permission for '"+firewall+"'")
	p.grant("roles/compute.securityAdmin")
	t.Cleanup(func() { p.revoke("roles/compute.securityAdmin") })
	waitHTTPGranted(t, "firewalls.patch as a Compute Security Admin", func() error { return patch(fws) })
	if err := patch(fws); err != nil {
		t.Errorf("firewalls.patch as a Compute Security Admin: %v", err)
	}
}
