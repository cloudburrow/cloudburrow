package redisadmin

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"google.golang.org/grpc/status"
)

// testKMS is the Cloud KMS server, in process.
type testKMS struct{ s *kms.Server }

func (k testKMS) Encrypt(ctx context.Context, key string, p []byte) ([]byte, error) {
	r, err := k.s.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: p})
	return r.GetCiphertext(), err
}

func (k testKMS) Decrypt(ctx context.Context, key string, c []byte) ([]byte, error) {
	r, err := k.s.Decrypt(ctx, &kmspb.DecryptRequest{Name: key, Ciphertext: c})
	return r.GetPlaintext(), err
}

func newKey(t *testing.T, k *kms.Server, loc string) string {
	t.Helper()
	ctx := context.Background()
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/my-proj/locations/" + loc, KeyRingId: "ring"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "redis",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return key.GetName()
}

func setVersion(t *testing.T, k *kms.Server, key string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := k.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
}

// TestCustomerManagedKey (#1463): an instance with customer_managed_key
// keeps its AUTH string and CA key sealed with a data key the KMS key
// wrapped; it is suspended (no state, CUSTOMER_MANAGED_KEY_ISSUE) while
// the key's version is disabled, and resumes once it is enabled again.
func TestCustomerManagedKey(t *testing.T) {
	db := store.NewMemory()
	s, p, _ := newCreating(t, db)
	k := kms.NewServer(store.NewMemory())
	s.created.kms = testKMS{k}
	key := newKey(t, k, "us-central1")
	ctx := context.Background()
	in := &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true,
		TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION, CustomerManagedKey: key}
	op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cmek", Instance: in})
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatalf("create: %v", op.GetError())
	}
	name := parent + "/instances/cmek"
	inst, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if inst.GetCustomerManagedKey() != key || len(inst.GetSuspensionReasons()) != 0 || inst.GetState() != redispb.Instance_READY {
		t.Fatalf("instance %v", inst)
	}
	auth, _ := s.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	kept, err := db.Get(recordPrefix + ServerName(name))
	if err != nil {
		t.Fatal(err)
	}
	if auth.GetAuthString() == "" || bytes.Contains(kept, []byte(auth.GetAuthString())) || bytes.Contains(kept, []byte("PRIVATE KEY")) {
		t.Fatalf("the kept record holds the AUTH string or the CA key in the clear: %s", kept)
	}

	setVersion(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	s.created.checkKeys(ctx)
	inst, _ = s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if inst.GetState() != redispb.Instance_STATE_UNSPECIFIED || len(inst.GetSuspensionReasons()) != 1 ||
		inst.GetSuspensionReasons()[0] != redispb.Instance_CUSTOMER_MANAGED_KEY_ISSUE {
		t.Fatalf("with the key disabled: state %v, suspension_reasons %v", inst.GetState(), inst.GetSuspensionReasons())
	}
	if _, err := s.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{Instance: &redispb.Instance{Name: name, DisplayName: "x"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("update of a suspended instance: %v", err)
	}

	setVersion(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	s.created.checkKeys(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		inst, _ = s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if inst.GetState() == redispb.Instance_READY {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not resumed: %v", inst.GetState())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(inst.GetSuspensionReasons()) != 0 {
		t.Errorf("resumed with suspension_reasons %v", inst.GetSuspensionReasons())
	}

	// A restart of CloudBurrow unseals the secrets with the key.
	s2, p2, _ := newCreating(t, db)
	s2.created.kms = testKMS{k}
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		p2.mu.Lock()
		spec, ok := p2.running[ServerName(name)]
		p2.mu.Unlock()
		if ok {
			if spec.Password != auth.GetAuthString() || !strings.Contains(spec.TransitCAKey, "PRIVATE KEY") {
				t.Fatalf("restored spec lost its secrets")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not restored")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = p

	// The key's version destroyed: a restart leaves it suspended.
	if _, err := k.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/1"}); err != nil {
		t.Fatal(err)
	}
	s3, _, _ := newCreating(t, db)
	s3.created.kms = testKMS{k}
	if err := s3.Restore(); err != nil {
		t.Fatal(err)
	}
	inst, _ = s3.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if len(inst.GetSuspensionReasons()) != 1 || inst.GetState() == redispb.Instance_READY {
		t.Errorf("restored with the key destroyed: state %v, suspension_reasons %v", inst.GetState(), inst.GetSuspensionReasons())
	}
}

// TestCustomerManagedKeyRefused: a key in another region, a malformed one,
// a missing one, and one without Cloud KMS are refused.
func TestCustomerManagedKeyRefused(t *testing.T) {
	s, _, _ := newCreating(t, nil)
	k := kms.NewServer(store.NewMemory())
	other := newKey(t, k, "europe-west1")
	ctx := context.Background()
	create := func(key string) error {
		_, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "bad",
			Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, CustomerManagedKey: key}})
		return err
	}
	if err := create("projects/my-proj/locations/us-central1/keyRings/r/cryptoKeys/k"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("without Cloud KMS: %v", err)
	}
	s.created.kms = testKMS{k}
	if err := create(other); status.Code(err) != codes.InvalidArgument {
		t.Errorf("another region: %v", err)
	}
	if err := create("not-a-key"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("malformed: %v", err)
	}
	if err := create("projects/my-proj/locations/us-central1/keyRings/r/cryptoKeys/missing"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("missing: %v", err)
	}
}

// TestRotateServerCA (#1463): a rotation lists a new CA beside the old
// one, and the server is started again with the new CA and trusting both.
func TestRotateServerCA(t *testing.T) {
	s, p, _ := newCreating(t, nil)
	ctx := context.Background()
	op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "tls",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, op)
	name := parent + "/instances/tls"
	before, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err := s.RotateServerCA(ctx, name); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if len(after.GetServerCaCerts()) != 2 || after.GetServerCaCerts()[1].GetCert() != before.GetServerCaCerts()[0].GetCert() {
		t.Fatalf("server_ca_certs after a rotation: %d", len(after.GetServerCaCerts()))
	}
	newCA := after.GetServerCaCerts()[0].GetCert()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		spec := p.running[ServerName(name)]
		p.mu.Unlock()
		if spec.TransitCA == newCA {
			files, err := ServerTLS(spec, time.Now())
			if err != nil || !strings.Contains(files.CA, before.GetServerCaCerts()[0].GetCert()) {
				t.Fatalf("the server does not trust the old CA too: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server was not started again with the new CA")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Five years on, Memorystore rotates on its own.
	for time.Now().Before(deadline) {
		s.created.mu.Lock()
		_, busy := s.created.busy[name]
		if !busy {
			s.created.now = func() time.Time { return time.Now().Add(caRotateAfter + time.Hour) }
		}
		s.created.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.created.rotateDue()
	if got, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); len(got.GetServerCaCerts()) != 3 {
		t.Errorf("after five years: %d CAs", len(got.GetServerCaCerts()))
	}
	plain, _ := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "plain",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	wait(t, s, plain)
	if err := s.RotateServerCA(ctx, parent+"/instances/plain"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("rotation without in-transit encryption: %v", err)
	}
}
