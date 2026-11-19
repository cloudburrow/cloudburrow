//go:build compat

package compat

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/components"
)

// forwardPodPort port-forwards a local port to port of deployment's pod,
// past its front, and returns its address.
func forwardPodPort(t *testing.T, deployment string, port int) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the emulator is reached through the cluster", envKubeconfig)
	}
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "port-forward", "--address", "127.0.0.1",
		"deployment/"+deployment, ":"+strconv.Itoa(port))
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("kubectl port-forward: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(out)
		re := regexp.MustCompile(`Forwarding from (127\.0\.0\.1:[0-9]+)`)
		for s.Scan() {
			if m := re.FindStringSubmatch(s.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case addr := <-found:
		return addr
	case <-time.After(30 * time.Second):
		t.Fatal("kubectl port-forward did not say its port")
	}
	return ""
}

type up1885 struct {
	V        string
	Customer *datastore.Key
}

// TestDatastoreNamedDatabaseEntitiesSurviveTheUpgrade (#1885): before
// each named database had an emulator project of its own, the emulator
// kept a named database's entities in its project's, their keys naming the
// database. Such entities, written to the emulator itself as an older
// front wrote them (with the marker it kept, and no record of the move),
// are moved when the front restarts on this build: the official client
// reads each by key and by query, in a namespace too, and a filter on a
// key-valued property finds the entity referring to another; (default)
// sees none of them, and the emulator's project keeps none.
//
// covers: google.datastore.v1.Datastore/Lookup, google.datastore.v1.Datastore/RunQuery
func TestDatastoreNamedDatabaseEntitiesSurviveTheUpgrade(t *testing.T) {
	h := New(t)
	dsAddr := h.Endpoint(EnvDatastore)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	project := h.Project()
	db := uniqueDB("upg")
	if _, err := createDB(t, h, db, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatalf("CreateDatabase %s: %v", db, err)
	}

	// The old layout, written to the emulator past the front.
	conn, err := grpc.NewClient(forwardPodPort(t, "datastore", components.DatastoreEmulatorPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := datastorepb.NewDatastoreClient(conn)
	rawCtx := func(p, d string) context.Context {
		params := "project_id=" + p
		if d != "" {
			params += "&database_id=" + d
		}
		return metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", params)
	}
	kind := uniqueDB("Order1885")
	key := func(ns, k, name string) *datastorepb.Key {
		return &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project, DatabaseId: db, NamespaceId: ns},
			Path: []*datastorepb.Key_PathElement{{Kind: k, IdType: &datastorepb.Key_PathElement_Name{Name: name}}}}
	}
	str := func(s string) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_StringValue{StringValue: s}}
	}
	cust := key("", kind+"Customer", "ada")
	muts := []*datastorepb.Mutation{
		{Operation: &datastorepb.Mutation_Upsert{Upsert: &datastorepb.Entity{Key: cust, Properties: map[string]*datastorepb.Value{"V": str("ada")}}}},
		{Operation: &datastorepb.Mutation_Upsert{Upsert: &datastorepb.Entity{Key: key("", kind, "o1"), Properties: map[string]*datastorepb.Value{
			"V": str("first"), "Customer": {ValueType: &datastorepb.Value_KeyValue{KeyValue: cust}}}}}},
		{Operation: &datastorepb.Mutation_Upsert{Upsert: &datastorepb.Entity{Key: key("tenant", kind, "o2"), Properties: map[string]*datastorepb.Value{"V": str("second")}}}},
	}
	if _, err := raw.Commit(rawCtx(project, db), &datastorepb.CommitRequest{ProjectId: project, DatabaseId: db,
		Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts}); err != nil {
		t.Fatalf("Commit in the old layout: %v", err)
	}
	marker := &datastorepb.Entity{Key: &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: callfront.MarkerProject},
		Path: []*datastorepb.Key_PathElement{{Kind: "Database", IdType: &datastorepb.Key_PathElement_Name{Name: project + "/" + db}}}},
		Properties: map[string]*datastorepb.Value{"project": str(project), "database": str(db)}}
	record := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: callfront.MarkerProject},
		Path: []*datastorepb.Key_PathElement{{Kind: "Migration", IdType: &datastorepb.Key_PathElement_Name{Name: "1885-named-databases-in-own-projects"}}}}
	if _, err := raw.Commit(rawCtx(callfront.MarkerProject, ""), &datastorepb.CommitRequest{ProjectId: callfront.MarkerProject,
		Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL, Mutations: []*datastorepb.Mutation{
			{Operation: &datastorepb.Mutation_Upsert{Upsert: marker}},
			{Operation: &datastorepb.Mutation_Delete{Delete: record}}}}); err != nil {
		t.Fatalf("Commit the marker: %v", err)
	}

	restartFront(t, "datastore", "datastore")

	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	c, err := datastore.NewClientWithDatabase(ctx, project, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var got up1885
	o1 := datastore.NameKey(kind, "o1", nil)
	if err := c.Get(ctx, o1, &got); err != nil || got.V != "first" {
		t.Fatalf("Get %v after the upgrade = %+v, %v; want first", o1, got, err)
	}
	if got.Customer == nil || got.Customer.Name != "ada" || got.Customer.Kind != kind+"Customer" {
		t.Errorf("the key-valued property after the upgrade = %v, want %s/ada", got.Customer, kind+"Customer")
	}
	var all []up1885
	if _, err := c.GetAll(ctx, datastore.NewQuery(kind), &all); err != nil || len(all) != 1 || all[0].V != "first" {
		t.Errorf("kind query after the upgrade = %+v, %v; want first", all, err)
	}
	var tenant []up1885
	if _, err := c.GetAll(ctx, datastore.NewQuery(kind).Namespace("tenant"), &tenant); err != nil || len(tenant) != 1 || tenant[0].V != "second" {
		t.Errorf("namespace query after the upgrade = %+v, %v; want second", tenant, err)
	}
	keys, err := c.GetAll(ctx, datastore.NewQuery(kind).FilterField("Customer", "=", datastore.NameKey(kind+"Customer", "ada", nil)).KeysOnly(), nil)
	if err != nil || len(keys) != 1 || keys[0].Name != "o1" {
		t.Errorf("filter on the key-valued property = %v, %v; want o1", keys, err)
	}
	var cu up1885
	if err := c.Get(ctx, datastore.NameKey(kind+"Customer", "ada", nil), &cu); err != nil || cu.V != "ada" {
		t.Errorf("Get the referred entity = %+v, %v", cu, err)
	}

	// The emulator's project keeps none of them now, and (default) sees
	// none.
	left, err := raw.RunQuery(rawCtx(project, ""), &datastorepb.RunQueryRequest{ProjectId: project,
		QueryType: &datastorepb.RunQueryRequest_Query{Query: &datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: kind}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(left.GetBatch().GetEntityResults()); n != 0 {
		t.Errorf("the emulator's project %s keeps %d of the moved entities", project, n)
	}
	def, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = def.Close() })
	if n, err := def.Count(ctx, datastore.NewQuery(kind)); err != nil || n != 0 {
		t.Errorf("(default): Count = %d, %v, want 0", n, err)
	}
}
