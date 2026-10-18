//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	fsadminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// emulatorPod is the kubectl of the instance's namespace, for the tests
// that end an emulator's process; it skips where the cluster cannot be
// reached.
func emulatorPod(t *testing.T) func(context.Context, ...string) ([]byte, error) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the emulator's process is ended through the cluster", envKubeconfig)
	}
	for _, bin := range []string{"docker", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH; the emulator's process is ended through the kind node", bin)
		}
	}
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	return func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...).CombinedOutput()
	}
}

// killEmulator ends the JVM of app's emulator with SIGKILL, from the kind
// node the pod runs on, as -XX:+ExitOnOutOfMemoryError ends it when its
// bounded heap fills (#1603): at once, running no shutdown hook, so the
// emulator saves nothing. It waits for the kubelet to start the
// emulator's container again, ready; the front's container is not
// touched, so what the front keeps is what a real exit leaves (#1611).
func killEmulator(t *testing.T, kc func(context.Context, ...string) ([]byte, error), app string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	before := readFrontPod(t, ctx, kc, app)
	front, emulator := before.containers["front"], before.containers[app]
	if front.id == "" || emulator.id == "" {
		t.Fatalf("the %s pod %s has containers %v; want front and %s", app, before.name, before.containers, app)
	}
	// The JVM is the container's process in persistent Datastore and a
	// child of gcloud's wrapper otherwise: the java process whose cgroup
	// is the container's.
	find := `for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = java ] && grep -qs "` + emulator.id + `" $p/cgroup && echo ${p#/proc/}; done; true`
	out, err := exec.CommandContext(ctx, "docker", "exec", before.node, "sh", "-c", find).CombinedOutput()
	pid := strings.TrimSpace(string(out))
	if _, perr := strconv.Atoi(pid); err != nil || perr != nil {
		t.Fatalf("find the %s emulator's JVM on %s: %v, %q", app, before.node, err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", before.node, "kill", "-KILL", pid).CombinedOutput(); err != nil {
		t.Fatalf("kill the %s emulator's JVM %s on %s: %v\n%s", app, pid, before.node, err, out)
	}
	for {
		now := readFrontPod(t, ctx, kc, app)
		f, e := now.containers["front"], now.containers[app]
		if now.name != before.name {
			t.Fatalf("the pod was replaced (%s, then %s); only the emulator's container was to restart", before.name, now.name)
		}
		if f.restarts != front.restarts || f.id != front.id {
			t.Fatalf("the front's container restarted too: %+v, then %+v", front, f)
		}
		if e.restarts > emulator.restarts && e.ready {
			t.Logf("the %s emulator's JVM %s was killed and its container restarted: %s, restarts %d", app, pid, e.id, e.restarts)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the %s emulator did not come back ready: %+v", app, e)
		case <-time.After(time.Second):
		}
	}
}

// TestFirestoreAfterTheEmulatorIsKilled (#1611): when the Firestore
// emulator's JVM ends as it does on OutOfMemoryError, its container
// restarts empty, in either --mode, since the emulator keeps documents in
// memory alone: every document is gone. What the Firestore Admin API
// keeps is the front's, on the pod's emptyDir, and survives: a named
// database, its composite index, its backup schedule and the backup it
// took, which restores the documents into a new database. A listener the
// official client had open reconnects by itself (it may first be told its
// document no longer exists) and sees a write made after the restart.
// Through the official clients.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/GetDatabase, google.firestore.admin.v1.FirestoreAdmin/GetIndex, google.firestore.admin.v1.FirestoreAdmin/GetBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/ListBackups, google.firestore.admin.v1.FirestoreAdmin/RestoreDatabase, google.firestore.v1.Firestore/Listen, google.firestore.v1.Firestore/BatchGetDocuments
func TestFirestoreAfterTheEmulatorIsKilled(t *testing.T) {
	h := New(t)
	ac := firestoreAdmin(t, h) // asks for the Firestore endpoint first
	fc := firestoreClient(t, h)
	kc := emulatorPod(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	project := "projects/" + h.Project()
	op, err := ac.CreateDatabase(ctx, &fsadminpb.CreateDatabaseRequest{Parent: project, DatabaseId: "killed-1611",
		Database: &fsadminpb.Database{LocationId: "nam5", Type: fsadminpb.Database_FIRESTORE_NATIVE}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ac.DeleteDatabase(context.Background(), &fsadminpb.DeleteDatabaseRequest{Name: db.GetName()})
	})
	iop, err := ac.CreateIndex(ctx, &fsadminpb.CreateIndexRequest{Parent: db.GetName() + "/collectionGroups/cities",
		Index: &fsadminpb.Index{QueryScope: fsadminpb.Index_COLLECTION, Fields: []*fsadminpb.Index_IndexField{
			{FieldPath: "country", ValueMode: &fsadminpb.Index_IndexField_Order_{Order: fsadminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "pop", ValueMode: &fsadminpb.Index_IndexField_Order_{Order: fsadminpb.Index_IndexField_DESCENDING}}}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	idx, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	dc := firestoreDatabaseClient(t, h, "killed-1611")
	for i := 1; i <= 3; i++ {
		if _, err := dc.Collection("kept").Doc(fmt.Sprintf("d%d", i)).Set(ctx, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	sched, err := ac.CreateBackupSchedule(ctx, &fsadminpb.CreateBackupScheduleRequest{Parent: db.GetName(), BackupSchedule: &fsadminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &fsadminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &fsadminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	doc := fc.Collection("killed1611").Doc("plain")
	if _, err := doc.Set(ctx, map[string]any{"v": "before"}); err != nil {
		t.Fatal(err)
	}
	listened := fc.Collection("killed1611").Doc("listened")
	if _, err := listened.Set(ctx, map[string]any{"v": "before"}); err != nil {
		t.Fatal(err)
	}
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	it := listened.Snapshots(lctx)
	defer it.Stop()
	if s, err := it.Next(); err != nil || s.Data()["v"] != "before" {
		t.Fatalf("the listener's first snapshot = %v, %v", s, err)
	}
	seen := make(chan *firestore.DocumentSnapshot, 8)
	ended := make(chan error, 1)
	go func() {
		for {
			s, err := it.Next()
			if err != nil {
				ended <- err
				return
			}
			seen <- s
		}
	}()

	killEmulator(t, kc, "firestore")

	// The documents were the emulator's memory: gone, in every database.
	var gerr error
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		_, gerr = doc.Get(ctx)
		if status.Code(gerr) != codes.Unavailable || time.Now().After(deadline) {
			break
		}
	}
	if status.Code(gerr) != codes.NotFound {
		t.Errorf("a document written before the emulator was killed: Get = %v; want NOT_FOUND (the emulator keeps documents in memory)", gerr)
	}
	if docs, err := dc.Collection("kept").Documents(ctx).GetAll(); err != nil || len(docs) != 0 {
		t.Errorf("the named database after the kill holds %d documents, %v; want none", len(docs), err)
	}

	// What the front keeps is still there.
	if got, err := ac.GetDatabase(ctx, &fsadminpb.GetDatabaseRequest{Name: db.GetName()}); err != nil || got.GetUid() != db.GetUid() {
		t.Errorf("GetDatabase after the kill = %v, %v; want uid %s", got, err, db.GetUid())
	}
	if i, err := ac.GetIndex(ctx, &fsadminpb.GetIndexRequest{Name: idx.GetName()}); err != nil || len(i.GetFields()) != 3 {
		t.Errorf("GetIndex after the kill = %v, %v", i, err)
	}
	if s, err := ac.GetBackupSchedule(ctx, &fsadminpb.GetBackupScheduleRequest{Name: sched.GetName()}); err != nil || s.GetRetention().AsDuration() != 24*time.Hour {
		t.Errorf("GetBackupSchedule after the kill = %v, %v", s, err)
	}
	lb, err := ac.ListBackups(ctx, &fsadminpb.ListBackupsRequest{Parent: project + "/locations/-"})
	if err != nil {
		t.Fatalf("ListBackups after the kill: %v", err)
	}
	var backup *fsadminpb.Backup
	for _, b := range lb.GetBackups() {
		if b.GetDatabase() == db.GetName() {
			backup = b
		}
	}
	if backup == nil || backup.GetStats().GetDocumentCount() != 3 {
		t.Fatalf("ListBackups after the kill = %v; want the schedule's backup of 3 documents", lb.GetBackups())
	}
	rop, err := ac.RestoreDatabase(ctx, &fsadminpb.RestoreDatabaseRequest{Parent: project, DatabaseId: "killed-1611-restored", Backup: backup.GetName()})
	if err != nil {
		t.Fatalf("RestoreDatabase after the kill: %v", err)
	}
	restored, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("RestoreDatabase after the kill: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ac.DeleteDatabase(context.Background(), &fsadminpb.DeleteDatabaseRequest{Name: restored.GetName()})
	})
	rc := firestoreDatabaseClient(t, h, "killed-1611-restored")
	if docs, err := rc.Collection("kept").Documents(ctx).GetAll(); err != nil || len(docs) != 3 {
		t.Errorf("the database restored after the kill holds %d documents, %v; want 3", len(docs), err)
	}

	// The listener reconnects by itself and sees a write made after it.
	if _, err := listened.Set(ctx, map[string]any{"v": "after"}); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(time.Minute)
	for {
		select {
		case s := <-seen:
			t.Logf("listener: exists %v, data %v", s.Exists(), s.Data())
			if s.Exists() && s.Data()["v"] == "after" {
				return
			}
		case err := <-ended:
			t.Fatalf("the listener ended after the emulator was killed: %v; want it to reconnect", err)
		case <-timeout:
			t.Fatal("the listener saw no write made after the emulator was killed within a minute")
		}
	}
}

// TestDatastoreAfterTheEmulatorIsKilled (#1611): when the Datastore
// emulator's JVM ends as it does on OutOfMemoryError, without saving, its
// container restarts. In persistent mode the emulator writes its whole
// store to the volume about every 30 seconds while it has changes (a
// fixed delay inside the emulator, which no flag sets), so an entity
// written before its last save is read back and only what was written
// since is lost; in ephemeral mode it keeps nothing on disk and every
// entity is gone. Through the official client.
//
// covers: google.datastore.v1.Datastore/Lookup, google.datastore.v1.Datastore/Commit
func TestDatastoreAfterTheEmulatorIsKilled(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project()) // asks for the Datastore endpoint first
	kc := emulatorPod(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Persistent mode mounts the volume; ephemeral mode leaves an earlier
	// run's claim unmounted.
	out, err := kc(ctx, "get", "deploy", "datastore", "-o", "jsonpath={.spec.template.spec.volumes[*].persistentVolumeClaim.claimName}")
	if err != nil {
		t.Fatalf("read the datastore Deployment's volumes: %v\n%s", err, out)
	}
	persistent := strings.Contains(string(out), "datastore-data")
	t.Logf("persistent mode (the emulator mounts the datastore-data volume): %v", persistent)

	saved := datastore.NameKey("Killed1611", "saved", nil)
	written := time.Now()
	if _, err := c.Put(ctx, saved, &durable{Note: "saved before the kill"}); err != nil {
		t.Fatal(err)
	}
	if persistent {
		// Wait for a save of the store that ended over a second after the
		// write, so it began after it: the emulator's next save.
		file := "/data/datastore/WEB-INF/appengine-generated/local_db.bin"
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) {
			out, err := kc(ctx, "exec", "deploy/datastore", "-c", "datastore", "--", "stat", "-c", "%.3Y", file)
			s, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
			if saved := time.UnixMilli(int64(s * 1000)); err == nil && perr == nil && saved.After(written.Add(time.Second)) {
				t.Logf("the emulator saved its store %s after the write", saved.Sub(written).Round(time.Second))
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the emulator did not save %s within 90 s of a write: %v %s", file, err, out)
			}
		}
	}
	unsaved := datastore.NameKey("Killed1611", "unsaved", nil)
	if _, err := c.Put(ctx, unsaved, &durable{Note: "written just before the kill"}); err != nil {
		t.Fatal(err)
	}

	killEmulator(t, kc, "datastore")

	got, err := getWithin(t, h, c, saved, 2*time.Minute)
	switch {
	case persistent && (err != nil || got.Note != "saved before the kill"):
		t.Errorf("persistent mode: the entity saved before the kill = %+v, %v; want it read back", got, err)
	case !persistent && err != datastore.ErrNoSuchEntity:
		t.Errorf("ephemeral mode: the entity written before the kill = %+v, %v; want none", got, err)
	}
	// What was written after the last save is lost; were a save to fall
	// between the write and the kill, it would be kept: logged, not
	// asserted.
	got, err = getWithin(t, h, c, unsaved, time.Minute)
	t.Logf("the entity written just before the kill: %+v, %v", got, err)
	if _, err := c.Put(ctx, unsaved, &durable{Note: "after"}); err != nil {
		t.Errorf("Put after the kill: %v", err)
	}
	_ = c.Delete(context.Background(), saved)
	_ = c.Delete(context.Background(), unsaved)
}
