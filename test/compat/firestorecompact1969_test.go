//go:build compat

package compat

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFirestoreCompactsItsVersionsBeforeTheHeapFills (#1969): the
// Firestore emulator keeps every version of a document in its heap, so 8
// documents of 10 KB overwritten 100,000 times filled a 1 GiB heap and the
// JVM exited, losing up to 30 seconds of writes. The official Go client
// now overwrites 8 such documents until twice the emulator's heap has
// been written (under a reduced --emulator-heap-mib this stays fast), and
// leaves a small ledger document every 50 writes: the front compacts the
// emulator's versions on the way (it restarts the JVM in its container,
// the documents exported and imported back), the emulator's container
// never restarts nor logs an OutOfMemoryError, and every write the client
// was answered for is there at the end. A read at a time before the
// compaction, which is still inside Google's window, is refused
// FAILED_PRECONDITION, as one before the database's earliestVersionTime
// is, and that time has moved past it. A listener open throughout is
// ended UNAVAILABLE at each compaction, reconnects by itself, and sees the
// last write.
//
// covers: google.firestore.v1.Firestore/Commit, google.firestore.v1.Firestore/BatchGetDocuments, google.firestore.v1.Firestore/Listen, google.firestore.admin.v1.FirestoreAdmin/GetDatabase
func TestFirestoreCompactsItsVersionsBeforeTheHeapFills(t *testing.T) {
	h := New(t)
	ac := firestoreAdmin(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE
	fc := firestoreClient(t, h)
	kc := emulatorPod(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	out, err := kc(ctx, "get", "pod", "-l", "app=firestore", "-o", `jsonpath={.items[0].spec.containers[?(@.name=="firestore")].env}`)
	if err != nil {
		t.Fatalf("read the emulator's environment: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`-Xmx(\d+)m`).FindSubmatch(out)
	if m == nil {
		t.Skip("the emulator's heap is unbounded (--emulator-heap-mib 0): it cannot be filled in a test's time")
	}
	heapMiB, _ := strconv.Atoi(string(m[1]))
	if heapMiB > 1024 {
		t.Skipf("the emulator's heap is %d MiB: filling it twice takes too long; run with --emulator-heap-mib 256", heapMiB)
	}
	before := readFrontPod(t, ctx, kc, "firestore")
	compactionsBefore := countFrontLog(t, ctx, kc, before.name, "compacted the emulator's versions")

	run := fmt.Sprintf("c1969-%d", time.Now().UnixNano()%1e9)
	pitr := fc.Collection(run + "-pitr").Doc("a")
	wr, err := pitr.Set(ctx, map[string]any{"v": 1})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	readAt := wr.UpdateTime

	// A listener open across the compactions: its stream is ended
	// UNAVAILABLE, and the client reconnects by itself.
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	var seen atomic.Int64
	go func() {
		it := fc.Collection(run).Doc("d0").Snapshots(lctx)
		defer it.Stop()
		for {
			snap, err := it.Next()
			if err != nil {
				return
			}
			if snap.Exists() {
				v, _ := snap.DataAt("i")
				n, _ := v.(int64)
				seen.Store(n)
			}
		}
	}()

	const docs, size, ledgerEvery = 8, 10_000, 50
	total := 2 * heapMiB << 20 / size
	t.Logf("the emulator's heap is %d MiB: writing %d overwrites of %d bytes to %d documents", heapMiB, total, size, docs)
	pad := strings.Repeat("x", size)
	var written atomic.Int64
	last := make([]int, docs)
	ledgers := make([][]string, docs)
	var wg sync.WaitGroup
	errs := make(chan error, docs)
	start := time.Now()
	for w := 0; w < docs; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ref := fc.Collection(run).Doc(fmt.Sprintf("d%d", w))
			for i := 1; written.Load() < int64(total); i++ {
				// A write answered with an error may or may not have
				// been applied: it is repeated, the same value, until
				// one is answered.
				set := func(ref *firestore.DocumentRef, data map[string]any) error {
					var err error
					for try := 0; try < 20; try++ {
						if _, err = ref.Set(ctx, data); err == nil {
							return nil
						}
						if c := status.Code(err); c != codes.Unavailable && c != codes.DeadlineExceeded && c != codes.Aborted {
							return err
						}
						time.Sleep(time.Second)
					}
					return err
				}
				if err := set(ref, map[string]any{"i": i, "pad": pad}); err != nil {
					errs <- fmt.Errorf("document %d, write %d: %w", w, i, err)
					return
				}
				last[w] = i
				written.Add(1)
				if i%ledgerEvery == 0 {
					id := fmt.Sprintf("w%d-%d", w, i)
					if err := set(fc.Collection(run+"-ledger").Doc(id), map[string]any{"i": i}); err != nil {
						errs <- fmt.Errorf("ledger %s: %w", id, err)
						return
					}
					ledgers[w] = append(ledgers[w], id)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("%d writes of %d bytes in %s (%.0f a second)", written.Load(), size, took.Round(time.Second), float64(written.Load())/took.Seconds())

	after := readFrontPod(t, ctx, kc, "firestore")
	if after.name != before.name {
		t.Fatalf("the pod was replaced (%s, then %s)", before.name, after.name)
	}
	if e, b := after.containers["firestore"], before.containers["firestore"]; e.restarts != b.restarts {
		t.Fatalf("the emulator's container restarted (%d, then %d restarts): its JVM exited", b.restarts, e.restarts)
	}
	if out, err := kc(ctx, "logs", after.name, "-c", "firestore"); err != nil || strings.Contains(string(out), "java.lang.OutOfMemoryError") {
		t.Fatalf("the emulator's log (%v) names a java.lang.OutOfMemoryError:\n%s", err, tail(string(out), 2000))
	}
	compactions := countFrontLog(t, ctx, kc, after.name, "compacted the emulator's versions") - compactionsBefore
	if compactions < 1 {
		t.Fatalf("the front compacted the emulator's versions %d times while %d MiB were written to a %d MiB heap; want at least once", compactions, total*size>>20, heapMiB)
	}
	t.Logf("the front compacted the emulator's versions %d times", compactions)

	// The listener, reconnected, sees the last write.
	for deadline := time.Now().Add(time.Minute); seen.Load() != int64(last[0]); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the listener saw document 0 at write %d; want its last, %d", seen.Load(), last[0])
		}
	}
	lcancel()

	// Every answered write is there.
	for w := 0; w < docs; w++ {
		snap, err := fc.Collection(run).Doc(fmt.Sprintf("d%d", w)).Get(ctx)
		if err != nil {
			t.Fatalf("document %d: %v", w, err)
		}
		if got, _ := snap.DataAt("i"); got != int64(last[w]) {
			t.Errorf("document %d holds write %v; want %d, the last one answered", w, got, last[w])
		}
	}
	var refs []*firestore.DocumentRef
	for w := range ledgers {
		for _, id := range ledgers[w] {
			refs = append(refs, fc.Collection(run+"-ledger").Doc(id))
		}
	}
	for i := 0; i < len(refs); i += 500 {
		snaps, err := fc.GetAll(ctx, refs[i:min(i+500, len(refs))])
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		for _, s := range snaps {
			if !s.Exists() {
				t.Errorf("ledger document %s, answered, is gone", s.Ref.ID)
			}
		}
	}
	t.Logf("all %d ledger documents are there", len(refs))

	// The versions before the compaction are gone, and said to be.
	// WithReadOptions sets the read time on the reference itself.
	if _, err := fc.Collection(run + "-pitr").Doc("a").WithReadOptions(firestore.ReadTime(readAt)).Get(ctx); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("read at %v, before the compaction: %v; want FAILED_PRECONDITION (earlier than the earliest version time)", readAt, err)
	}
	db, err := ac.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/" + h.Project() + "/databases/(default)"})
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if e := db.GetEarliestVersionTime().AsTime(); !e.After(readAt) {
		t.Errorf("earliestVersionTime = %v; want it after the compaction, past %v", e, readAt)
	}
	if snap, err := pitr.Get(ctx); err != nil || snap.Data()["v"] != int64(1) {
		t.Errorf("the document read now = %v, %v; want v 1", snap, err)
	}
}

// countFrontLog counts the front's log lines in pod that contain s.
func countFrontLog(t *testing.T, ctx context.Context, kc func(context.Context, ...string) ([]byte, error), pod, s string) int {
	t.Helper()
	out, err := kc(ctx, "logs", pod, "-c", "front")
	if err != nil {
		t.Fatalf("kubectl logs %s -c front: %v\n%s", pod, err, out)
	}
	return strings.Count(string(out), s)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
