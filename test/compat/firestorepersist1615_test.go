//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	fsadminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	envFirestoreExpect = "CLOUDBURROW_TEST_FIRESTORE_EXPECT"
	envFirestoreSetup  = "CLOUDBURROW_TEST_FIRESTORE_SETUP"
)

// firestoreProbeProject is fixed, not per test: the restart probe reads
// what an earlier process wrote.
const firestoreProbeProject = "cb-firestore-probe"

// firestoreProbeDatabase is a named database the probe writes to too, made
// with the Admin API, so the probe sees the Admin API's database and its
// documents survive together.
const firestoreProbeDatabase = "probe-1615"

func firestoreProjectClient(t *testing.T, h *Harness, project, database string) *firestore.Client {
	t.Helper()
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	c, err := firestore.NewClientWithDatabase(h.Context(), project, database)
	if err != nil {
		t.Fatalf("firestore.NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// getDocWithin reads a document, retrying while the backend comes back.
func getDocWithin(ctx context.Context, ref *firestore.DocumentRef, within time.Duration) (*firestore.DocumentSnapshot, error) {
	for deadline := time.Now().Add(within); ; time.Sleep(2 * time.Second) {
		s, err := ref.Get(ctx)
		if err == nil || status.Code(err) == codes.NotFound || time.Now().After(deadline) {
			return s, err
		}
	}
}

// TestFirestoreSurvivesAPodRestart (#1615): the front exports the
// emulator's documents when the pod stops, and imports them when the next
// one starts, so in persistent mode, where they are on the pod's volume, a
// document written just before its pod is deleted is read back from the
// new pod, in the default database and in a named one; in ephemeral mode
// they are on the pod's emptyDir and go with it. Repeated, as the Datastore
// test is. Through the official clients.
//
// covers: google.firestore.v1.Firestore/Commit, google.firestore.v1.Firestore/BatchGetDocuments, google.firestore.admin.v1.FirestoreAdmin/CreateDatabase, google.firestore.admin.v1.FirestoreAdmin/GetDatabase
func TestFirestoreSurvivesAPodRestart(t *testing.T) {
	h := New(t)
	ac := firestoreAdmin(t, h) // asks for the Firestore endpoint first
	kc := emulatorPod(t)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := kc(ctx, "get", "deploy", "firestore", "-o", "jsonpath={.spec.template.spec.volumes[*].persistentVolumeClaim.claimName}")
	if err != nil {
		t.Fatalf("read the firestore Deployment's volumes: %v\n%s", err, out)
	}
	persistent := strings.Contains(string(out), "firestore-data")
	t.Logf("persistent mode (the pod mounts the firestore-data volume): %v", persistent)

	db := ensureFirestoreDatabase(t, ctx, ac, h.Project(), "restart-1615")
	fc := firestoreClient(t, h)
	nc := firestoreDatabaseClient(t, h, "restart-1615")
	const restarts = 2
	for i := 1; i <= restarts; i++ {
		id := fmt.Sprintf("pod-restart-%d", i)
		for _, c := range []*firestore.Client{fc, nc} {
			if _, err := c.Collection("durable1615").Doc(id).Set(ctx, map[string]any{"n": i}); err != nil {
				t.Fatalf("restart %d: Set: %v", i, err)
			}
		}

		restartWorkload(t, kubeconfig, "firestore")
		waitForEndpoint(t, h.Endpoint(EnvFirestore), 2*time.Minute)

		for j := 1; j <= i; j++ {
			for name, c := range map[string]*firestore.Client{"(default)": fc, "restart-1615": nc} {
				s, err := getDocWithin(ctx, c.Collection("durable1615").Doc(fmt.Sprintf("pod-restart-%d", j)), 2*time.Minute)
				switch {
				case persistent && (err != nil || s.Data()["n"] != int64(j)):
					t.Fatalf("after pod restart %d of %d in persistent mode: %s's document %d = %v, %v; want it kept", i, restarts, name, j, s, err)
				case !persistent && j == i && status.Code(err) != codes.NotFound:
					t.Fatalf("after a pod restart in ephemeral mode: %s's document = %v, %v; want NOT_FOUND (the emptyDir goes with the pod)", name, s, err)
				}
			}
		}
	}
	if persistent {
		if got, err := ac.GetDatabase(ctx, &fsadminpb.GetDatabaseRequest{Name: db.GetName()}); err != nil || got.GetUid() != db.GetUid() {
			t.Errorf("GetDatabase after the pod restarts = %v, %v; want uid %s", got, err, db.GetUid())
		}
	}
}

// TestFirestoreRestartSetup leaves the probe documents, in the default
// database and in a named one the Admin API makes, that
// TestFirestoreAcrossRestart reads after `stop` and `up`. CI runs it
// alone, just before `stop`, not in the suite (#1636): a later test's
// front-state reset (TestResetClearsTheFrontsState) forgets the Admin
// API's databases, the probe's named one among them.
//
// covers: google.firestore.v1.Firestore/Commit, google.firestore.admin.v1.FirestoreAdmin/CreateDatabase
func TestFirestoreRestartSetup(t *testing.T) {
	if os.Getenv(envFirestoreSetup) == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envFirestoreSetup)
	}
	h := New(t)
	ac := firestoreAdmin(t, h) // asks for the Firestore endpoint first
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pdb := ensureFirestoreDatabase(t, ctx, ac, firestoreProbeProject, firestoreProbeDatabase)
	for _, c := range []*firestore.Client{
		firestoreProjectClient(t, h, firestoreProbeProject, "(default)"),
		firestoreProjectClient(t, h, firestoreProbeProject, firestoreProbeDatabase),
	} {
		if _, err := c.Collection("durable1615").Doc("probe").Set(ctx, map[string]any{"note": "written-before-stop", "uid": pdb.GetUid()}); err != nil {
			t.Fatal(err)
		}
	}
}

// ensureFirestoreDatabase creates the named database, or gets it when an
// earlier run made it.
func ensureFirestoreDatabase(t *testing.T, ctx context.Context, ac *admin.FirestoreAdminClient, project, id string) *fsadminpb.Database {
	t.Helper()
	name := "projects/" + project + "/databases/" + id
	if db, err := ac.GetDatabase(ctx, &fsadminpb.GetDatabaseRequest{Name: name}); err == nil {
		return db
	}
	op, err := ac.CreateDatabase(ctx, &fsadminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
		Database: &fsadminpb.Database{LocationId: "nam5", Type: fsadminpb.Database_FIRESTORE_NATIVE}})
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", name, err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", name, err)
	}
	return db
}

// TestFirestoreAcrossRestart (#1615): after `stop` and `up` in persistent
// mode the probe documents TestFirestoreRestartSetup wrote are there, in the default database and in the
// named one, which the Admin API still lists with its uid; after `up
// --mode ephemeral` they are gone.
//
// covers: google.firestore.v1.Firestore/BatchGetDocuments, google.firestore.admin.v1.FirestoreAdmin/GetDatabase
func TestFirestoreAcrossRestart(t *testing.T) {
	expect := os.Getenv(envFirestoreExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envFirestoreExpect)
	}
	h := New(t)
	ac := firestoreAdmin(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, database := range []string{"(default)", firestoreProbeDatabase} {
		c := firestoreProjectClient(t, h, firestoreProbeProject, database)
		s, err := getDocWithin(ctx, c.Collection("durable1615").Doc("probe"), 2*time.Minute)
		switch expect {
		case "present":
			if err != nil || s.Data()["note"] != "written-before-stop" {
				t.Fatalf("persistent mode after stop/up: %s's probe = %v, %v; want the document written before stop", database, s, err)
			}
			if database == firestoreProbeDatabase {
				db, err := ac.GetDatabase(ctx, &fsadminpb.GetDatabaseRequest{Name: "projects/" + firestoreProbeProject + "/databases/" + database})
				if err != nil || db.GetUid() != s.Data()["uid"] {
					t.Fatalf("persistent mode after stop/up: GetDatabase = %v, %v; want the database the probe was written to (uid %v)", db, err, s.Data()["uid"])
				}
			}
		case "absent":
			if status.Code(err) != codes.NotFound {
				t.Fatalf("ephemeral mode after stop/up: %s's probe = %v, %v; want NOT_FOUND", database, s, err)
			}
		default:
			t.Fatalf("%s must be present or absent, not %q", envFirestoreExpect, expect)
		}
	}
}
