package kms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// At destroy_time a version is DESTROYED, on the next read, whether or not
// the sweep has run (#403). The clock is moved; nothing sleeps.
//
// unverified: google.cloud.kms.v1.KeyManagementService/DestroyCryptoKeyVersion FAILED_PRECONDITION: a version whose destroy_time has passed
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion FAILED_PRECONDITION: a version whose destroy_time has passed
func TestAScheduledVersionIsDestroyedAtItsDestroyTime(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	c := clientOf(t, NewServerWithClock(store.NewMemory(), clock))
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "auto"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	name := key.GetPrimary().GetName()
	sched1, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(24*time.Hour - time.Second)
	if v, _ := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name}); v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
		t.Fatalf("1s before destroy_time the version is %s", v.GetState())
	}
	clock.Advance(time.Second)
	get, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name})
	listed, lerr := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()}).Next()
	for what, v := range map[string]*kmspb.CryptoKeyVersion{"Get": get, "List": listed} {
		if v.GetState() != kmspb.CryptoKeyVersion_DESTROYED || v.GetDestroyTime() != nil ||
			!v.GetDestroyEventTime().AsTime().Equal(sched1.GetDestroyTime().AsTime()) {
			t.Errorf("%s at destroy_time = %s, destroy_time %v, destroy_event_time %v; want DESTROYED at %v",
				what, v.GetState(), v.GetDestroyTime(), v.GetDestroyEventTime(), sched1.GetDestroyTime().AsTime())
		}
	}
	if err != nil || (lerr != nil && lerr != iterator.Done) {
		t.Fatal(err, lerr)
	}
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("destroying a DESTROYED version = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: stateMask(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: kmspb.CryptoKeyVersion_ENABLED}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("updating a DESTROYED version = %v, want FAILED_PRECONDITION", err)
	}
}

// recordingKubectl is a fake kubectl that keeps Secrets in memory and records
// every manifest applied.
type recordingKubectl struct {
	mu      sync.Mutex
	secrets map[string]string
	applied []string
}

func (f *recordingKubectl) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, " apply "):
		var m struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdin), &m); err != nil {
			return "", err
		}
		f.applied = append(f.applied, stdin)
		f.secrets[m.Metadata.Name] = m.Data["value"] + "|" + m.Metadata.Annotations[keyAnnotation]
	case strings.Contains(joined, "get secret "):
		name := args[len(args)-3]
		if v, ok := f.secrets[name]; ok {
			return strings.SplitN(v, "|", 2)[0], nil
		}
		return "", errNotFoundKubectl(name)
	case strings.Contains(joined, "get secrets"):
		var keys []string
		for _, v := range f.secrets {
			keys = append(keys, strings.SplitN(v, "|", 2)[1])
		}
		return strings.Join(keys, "\n"), nil
	}
	return "", nil
}

type errNotFoundKubectl string

func (e errNotFoundKubectl) Error() string {
	return `kubectl: exit status 1: Error from server (NotFound): secrets "` + string(e) + `" not found`
}

// The sweep removes the material from the record, and the manifest kubectl
// is sent for the rewrite carries neither the material nor its base64.
func TestTheSweepErasesKeyMaterial(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	kube := &recordingKubectl{secrets: map[string]string{}}
	db := NewKubeStore(kube, "cloudburrow", "i")
	srv := NewServerWithClock(db, clock)
	c := clientOf(t, srv)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "erase"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	name := key.GetPrimary().GetName()
	var before keyVersion
	raw, err := db.Get(dbKey(versionPrefix, name))
	if err != nil || json.Unmarshal(raw, &before) != nil || len(before.Material) != 32 {
		t.Fatalf("the version has no material to erase: %v", err)
	}
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(24 * time.Hour)
	kube.applied = nil
	if _, err := srv.Sweep(); err != nil {
		t.Fatal(err)
	}
	raw, err = db.Get(dbKey(versionPrefix, name))
	var after keyVersion
	if err != nil || json.Unmarshal(raw, &after) != nil {
		t.Fatal(err)
	}
	if after.State != kmspb.CryptoKeyVersion_DESTROYED.String() || len(after.Material) != 0 {
		t.Errorf("stored after the sweep: state %s, %d bytes of material; want DESTROYED and none", after.State, len(after.Material))
	}
	if len(kube.applied) != 1 {
		t.Fatalf("the sweep applied %d manifests, want 1", len(kube.applied))
	}
	for _, form := range []string{base64.StdEncoding.EncodeToString(before.Material), string(before.Material)} {
		if strings.Contains(kube.applied[0], form) {
			t.Error("the rewrite manifest still carries the key material")
		}
	}
}
