//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	btadmin "cloud.google.com/go/bigtable/admin/apiv2"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	privatecapb "cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// The list filters and order_by of #1943: AIP-160 filters
// (google.aip.dev/160) and AIP-132 order_by (google.aip.dev/132), through
// each service's official Go client, evaluated by internal/aipfilter.

// drainList reads every item of an iterator, failing the test on an error.
func drainList[T any](t *testing.T, what string, next func() (T, error)) ([]T, error) {
	t.Helper()
	var out []T
	for {
		v, err := next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
}

// covers: google.cloud.eventarc.v1.Eventarc/ListProviders, google.cloud.eventarc.v1.Eventarc/ListTriggers
//
// TestListFiltersEventarc: ListProviders takes an AIP-160
// filter and an order_by ("name desc").
func TestListFiltersEventarc(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	ctx := h.Context()
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	parent := runParent(h)
	all, err := drainList(t, "providers", ec.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: parent}).Next)
	if err != nil || len(all) < 2 {
		t.Fatalf("ListProviders = %d, %v; want at least two", len(all), err)
	}
	pick := all[0]
	got, err := drainList(t, "providers", ec.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: parent,
		Filter: `name = "` + pick.GetName() + `"`}).Next)
	if err != nil || len(got) != 1 || got[0].GetName() != pick.GetName() {
		t.Errorf("ListProviders filter name = %v, %v; want %s", got, err, pick.GetName())
	}
	got, err = drainList(t, "providers", ec.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: parent,
		Filter: `NOT name = "` + pick.GetName() + `"`, OrderBy: "name desc"}).Next)
	if err != nil || len(got) != len(all)-1 {
		t.Fatalf("ListProviders NOT name = %d, %v; want %d", len(got), err, len(all)-1)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].GetName() < got[i].GetName() {
			t.Errorf("order_by name desc: %s before %s", got[i-1].GetName(), got[i].GetName())
		}
	}
	_, err = ec.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: parent, Filter: "nosuch = 1"}).Next()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListProviders filter on an unknown field = %v, want InvalidArgument", err)
	}
	_, err = ec.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: parent, Filter: "destination:gke", OrderBy: "name desc, trigger_id"}).Next()
	if err != nil && err != iterator.Done {
		t.Errorf("ListTriggers destination:gke, order_by name desc, trigger_id = %v", err)
	}
}

// covers: google.cloud.functions.v2.FunctionService/ListRuntimes, google.cloud.functions.v2.FunctionService/ListFunctions, google.cloud.location.Locations/ListLocations
//
// TestListFiltersFunctions: ListRuntimes and ListFunctions take an
// AIP-160 filter, ListFunctions an order_by, and ListLocations a filter.
func TestListFiltersFunctions(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	ctx := h.Context()
	c, err := functions.NewFunctionClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	parent := runParent(h)
	rt, err := c.ListRuntimes(ctx, &functionspb.ListRuntimesRequest{Parent: parent, Filter: `environment = GEN_2 AND name:go`})
	if err != nil || len(rt.GetRuntimes()) == 0 {
		t.Fatalf("ListRuntimes environment=GEN_2 AND name:go = %v, %v", rt, err)
	}
	for _, r := range rt.GetRuntimes() {
		if r.GetEnvironment() != functionspb.Environment_GEN_2 || !strings.Contains(r.GetName(), "go") {
			t.Errorf("ListRuntimes let %s (%v) through", r.GetName(), r.GetEnvironment())
		}
	}
	_, err = c.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent, Filter: `environment="GEN_2" AND state != FAILED`,
		OrderBy: "update_time desc, name"}).Next()
	if err != nil && err != iterator.Done {
		t.Errorf("ListFunctions with a filter and order_by = %v", err)
	}
	_, err = c.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent, OrderBy: "nosuch"}).Next()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListFunctions order_by an unknown field = %v, want InvalidArgument", err)
	}
	locs, err := drainList(t, "locations", c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + h.Project(),
		Filter: `displayName = "europe-*"`}).Next)
	if err != nil || len(locs) == 0 {
		t.Fatalf("Functions ListLocations filter = %v, %v", locs, err)
	}
	for _, l := range locs {
		if !strings.HasPrefix(l.GetLocationId(), "europe-") {
			t.Errorf("ListLocations filter let %s through", l.GetName())
		}
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/ListFiles, google.cloud.location.Locations/ListLocations
//
// TestListFiltersArtifactRegistry: the content lists take their
// documented filter fields and an order_by; another field is refused.
func TestListFiltersArtifactRegistry(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	ctx := h.Context()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	repo := runParent(h) + "/repositories/filters-1943"
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: runParent(h), RepositoryId: "filters-1943",
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateRepository wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(h.Context(), &arpb.DeleteRepositoryRequest{Name: repo}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	if _, err := drainList(t, "packages", ar.ListPackages(ctx, &arpb.ListPackagesRequest{Parent: repo,
		Filter: `name = "` + repo + `/packages/*" AND annotations:*`, OrderBy: "update_time desc"}).Next); err != nil {
		t.Errorf("ListPackages with a filter and order_by = %v", err)
	}
	if _, err := drainList(t, "files", ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: repo,
		Filter: `owner = "` + repo + `/packages/p/versions/v"`, OrderBy: "name desc"}).Next); err != nil {
		t.Errorf("ListFiles with a filter and order_by = %v", err)
	}
	_, err = ar.ListPackages(ctx, &arpb.ListPackagesRequest{Parent: repo, Filter: `create_time > "2020-01-01T00:00:00Z"`}).Next()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListPackages filter on create_time, which is not documented as filterable = %v, want InvalidArgument", err)
	}
	locs, err := drainList(t, "locations", ar.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + h.Project(),
		Filter: `locationId = "us-*" AND NOT locationId = us-central1`}).Next)
	if err != nil || len(locs) == 0 {
		t.Fatalf("Artifact Registry ListLocations filter = %v, %v", locs, err)
	}
	for _, l := range locs {
		if !strings.HasPrefix(l.GetLocationId(), "us-") || l.GetLocationId() == "us-central1" {
			t.Errorf("ListLocations filter let %s through", l.GetName())
		}
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/ListInstances, google.cloud.location.Locations/ListLocations
//
// TestListFiltersMemorystore: ListInstances takes an AIP-160 filter
// and order_by, and refuses an unknown field; ListLocations filters.
func TestListFiltersMemorystore(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx := h.Context()
	parent := "projects/" + project + "/locations/-"
	if _, err := drainList(t, "instances", c.ListInstances(ctx, &memorystorepb.ListInstancesRequest{Parent: parent,
		Filter: `state = ACTIVE OR labels.env:*`, OrderBy: "create_time desc"}).Next); err != nil {
		t.Errorf("ListInstances with a filter and order_by = %v", err)
	}
	_, err := c.ListInstances(ctx, &memorystorepb.ListInstancesRequest{Parent: parent, Filter: "nosuch = 1"}).Next()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListInstances filter on an unknown field = %v, want InvalidArgument", err)
	}
	locs, err := drainList(t, "locations", c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project,
		Filter: `locationId = "us-central1"`}).Next)
	if err != nil || len(locs) != 1 {
		t.Errorf("Memorystore ListLocations filter = %v, %v", locs, err)
	}
}

// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/ListCaPools, google.cloud.location.Locations/ListLocations
//
// TestListFiltersPrivateCA: ListCaPools filters on tier and orders by
// name desc, over gRPC and REST.
func TestListFiltersPrivateCA(t *testing.T) {
	h := New(t)
	for transport, c := range privateCAClients(t, h) {
		ctx := h.Context()
		loc := "projects/" + h.Project() + "/locations/us-west1"
		for _, p := range []struct {
			id   string
			tier privatecapb.CaPool_Tier
		}{{"flt-" + transport + "-a", privatecapb.CaPool_DEVOPS}, {"flt-" + transport + "-b", privatecapb.CaPool_ENTERPRISE}, {"flt-" + transport + "-c", privatecapb.CaPool_DEVOPS}} {
			op, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: p.id, CaPool: &privatecapb.CaPool{Tier: p.tier}})
			if err != nil {
				t.Fatalf("%s CreateCaPool %s: %v", transport, p.id, err)
			}
			if _, err := op.Wait(ctx); err != nil {
				t.Fatalf("%s CreateCaPool %s wait: %v", transport, p.id, err)
			}
			name := loc + "/caPools/" + p.id
			t.Cleanup(func() {
				if op, err := c.DeleteCaPool(h.Context(), &privatecapb.DeleteCaPoolRequest{Name: name}); err == nil {
					_ = op.Wait(h.Context())
				}
			})
		}
		got, err := drainList(t, "pools", c.ListCaPools(ctx, &privatecapb.ListCaPoolsRequest{Parent: loc,
			Filter: `tier = DEVOPS AND name:"flt-` + transport + `"`, OrderBy: "name desc"}).Next)
		var ids []string
		for _, p := range got {
			ids = append(ids, lastSegment(p.GetName()))
		}
		if want := "flt-" + transport + "-c,flt-" + transport + "-a"; err != nil || strings.Join(ids, ",") != want {
			t.Errorf("%s ListCaPools tier=DEVOPS name desc = %v, %v; want %s", transport, ids, err, want)
		}
		_, err = c.ListCaPools(ctx, &privatecapb.ListCaPoolsRequest{Parent: loc, Filter: "tier > DEVOPS"}).Next()
		wantPrivateCACode(t, transport+" ListCaPools tier > DEVOPS", err, codes.InvalidArgument)
		locs, err := drainList(t, "locations", c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + h.Project(),
			Filter: `locationId = "us-west1"`}).Next)
		if err != nil || len(locs) != 1 {
			t.Errorf("%s Private CA ListLocations filter = %v, %v", transport, locs, err)
		}
	}
}

// covers: google.bigtable.admin.v2.BigtableTableAdmin/ListBackups
//
// TestListFiltersBigtableBackups: ListBackups takes the documented
// filter fields and a one-field order_by, through the GAPIC admin client.
func TestListFiltersBigtableBackups(t *testing.T) {
	h := New(t)
	admin, _ := bigtableTable(t, h, "flt-src")
	ctx := h.Context()
	expire := time.Now().Add(48 * time.Hour)
	for i, id := range []string{"flt-b1", "flt-b2"} {
		if err := admin.CreateBackup(ctx, "flt-src", "c1", id, expire.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("CreateBackup %s: %v", id, err)
		}
		id := id
		t.Cleanup(func() { _ = admin.DeleteBackup(h.Context(), "c1", id) })
	}
	gc, err := btadmin.NewBigtableTableAdminClient(ctx, option.WithEndpoint(h.Endpoint(EnvBigtable)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	cluster := "projects/" + h.Project() + "/instances/" + bigtableInstance + "/clusters/c1"
	list := func(filter, orderBy string) ([]string, error) {
		bs, err := drainList(t, "backups", gc.ListBackups(ctx, &btapb.ListBackupsRequest{Parent: cluster, Filter: filter, OrderBy: orderBy}).Next)
		var ids []string
		for _, b := range bs {
			ids = append(ids, lastSegment(b.GetName()))
		}
		return ids, err
	}
	for _, tc := range []struct{ filter, orderBy, want string }{
		{`source_table:flt-src`, "expire_time desc", "flt-b2,flt-b1"},
		{`source_table:flt-src AND state = READY`, "name", "flt-b1,flt-b2"},
		{`name:flt-b2`, "", "flt-b2"},
		{`source_table:flt-src AND expire_time > "` + expire.Add(30*time.Minute).UTC().Format(time.RFC3339) + `"`, "", "flt-b2"},
	} {
		if got, err := list(tc.filter, tc.orderBy); err != nil || strings.Join(got, ",") != tc.want {
			t.Errorf("ListBackups filter %q order_by %q = %v, %v; want %s", tc.filter, tc.orderBy, got, err, tc.want)
		}
	}
	if _, err := list("", "name, expire_time"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListBackups order_by of two fields = %v, want InvalidArgument", err)
	}
	if _, err := list("encryption_info:*", ""); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListBackups filter on an undocumented field = %v, want InvalidArgument", err)
	}
}
