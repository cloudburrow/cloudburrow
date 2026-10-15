package kms

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// stallingStore is a store whose Scan reads its records and then waits until
// released, as a sweep over many records waits on kubectl.
type stallingStore struct {
	*store.Memory
	once     sync.Once
	scanning chan struct{}
	release  chan struct{}
}

func (s *stallingStore) Scan(prefix string) (map[string][]byte, error) {
	keys, err := s.List(prefix)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, k := range keys {
		if b, err := s.Get(k); err == nil {
			out[k] = b
		}
	}
	s.once.Do(func() { close(s.scanning) })
	<-s.release
	return out, nil
}

// A request is not held up by a sweep still reading: the sweep reads without
// the server's lock (#1232). Before, it held the lock across one kubectl read
// per record, and with a few hundred records UpdateCryptoKey clearing a
// rotation schedule ran past its 30-second deadline (TestKMSRotationSchedule).
// A schedule cleared while the sweep read the key as due is not rotated: the
// sweep reads the key again under the lock before writing.
func TestARequestIsNotHeldUpByASweep(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(rotationStart)
	db := &stallingStore{Memory: store.NewMemory(), scanning: make(chan struct{}), release: make(chan struct{})}
	srv := NewServerWithClock(db, clock)
	c := clientOf(t, srv)
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "stall"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
			RotationSchedule: periodOf(7 * 24 * time.Hour), NextRotationTime: timestamppb.New(rotationStart.Add(time.Hour))}})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)

	swept := make(chan error, 1)
	go func() { _, err := srv.Sweep(); swept <- err }()
	<-db.scanning
	// The sweep has read the key due for rotation and is still reading.
	done := make(chan error, 1)
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := c.UpdateCryptoKey(rctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: &kmspb.CryptoKey{Name: key.GetName()},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period", "next_rotation_time"}}})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("UpdateCryptoKey clearing the schedule during a sweep: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(db.release)
		<-swept
		t.Fatal("UpdateCryptoKey waited for a sweep that was reading")
	}
	close(db.release)
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got := k.GetPrimary().GetName(); got != versionName(key.GetName(), 1) {
		t.Errorf("a key whose schedule was cleared during the sweep was rotated: primary %s", got)
	}
	if k.GetNextRotationTime() != nil {
		t.Errorf("the cleared schedule came back: next_rotation_time %v", k.GetNextRotationTime().AsTime())
	}
}

// countingKubectl counts the single-Secret reads a fake kubectl is sent.
type countingKubectl struct {
	*recordingKubectl
	mu    sync.Mutex
	gets  int
	lists int
}

func (c *countingKubectl) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	c.mu.Lock()
	switch {
	case strings.Contains(joined, "get secret "):
		c.gets++
	case strings.Contains(joined, "get secrets"):
		c.lists++
	}
	c.mu.Unlock()
	return c.recordingKubectl.Run(ctx, stdin, args...)
}

// Over a KubeStore the sweep reads every record with one list per prefix, not
// a kubectl get per record, so its cost does not grow by a request per record
// earlier runs left (#1232).
func TestTheSweepReadsWithOneListPerPrefix(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	kube := &countingKubectl{recordingKubectl: &recordingKubectl{secrets: map[string]string{}}}
	srv := NewServerWithClock(NewKubeStore(k8s.NewWith(kube, "", "", "cloudburrow"), "i"), clock)
	c := clientOf(t, srv)
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "many"})
	if err != nil {
		t.Fatal(err)
	}
	var due string
	for i := 0; i < 20; i++ {
		key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: fmt.Sprintf("k%d", i),
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			due = key.GetPrimary().GetName()
		}
	}
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: due}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(24 * time.Hour)
	kube.gets, kube.lists = 0, 0
	if _, err := srv.Sweep(); err != nil {
		t.Fatal(err)
	}
	// One get: the due version, read again under the lock before its write.
	if kube.lists != 2 || kube.gets != 1 {
		t.Errorf("the sweep over 41 records sent %d lists and %d gets, want 2 and 1", kube.lists, kube.gets)
	}
	v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: due})
	if err != nil || v.GetState() != kmspb.CryptoKeyVersion_DESTROYED {
		t.Errorf("the due version is %v (%v), want DESTROYED", v.GetState(), err)
	}
}
