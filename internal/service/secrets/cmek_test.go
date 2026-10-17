package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// fakeKMS is a KeyService whose keys can be disabled. Its ciphertext names
// the key, so a ciphertext of another key fails as Cloud KMS's would.
type fakeKMS struct {
	mu       sync.Mutex
	disabled map[string]bool
	version  map[string]int
	calls    int
}

func newFakeKMS() *fakeKMS { return &fakeKMS{disabled: map[string]bool{}, version: map[string]int{}} }

func (f *fakeKMS) setDisabled(key string, d bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled[key] = d
}

func (f *fakeKMS) Encrypt(_ context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.disabled[req.GetName()] {
		return nil, status.Errorf(codes.FailedPrecondition, "CryptoKeyVersion %s/cryptoKeyVersions/1 is DISABLED", req.GetName())
	}
	ct := append([]byte(req.GetName()+"|"+string(req.GetAdditionalAuthenticatedData())+"|"), req.GetPlaintext()...)
	return &kmspb.EncryptResponse{Name: req.GetName() + "/cryptoKeyVersions/1", Ciphertext: ct}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.disabled[req.GetName()] {
		return nil, status.Errorf(codes.FailedPrecondition, "CryptoKeyVersion %s/cryptoKeyVersions/1 is DISABLED", req.GetName())
	}
	prefix := []byte(req.GetName() + "|" + string(req.GetAdditionalAuthenticatedData()) + "|")
	if !bytes.HasPrefix(req.GetCiphertext(), prefix) {
		return nil, status.Error(codes.InvalidArgument, "decryption failed")
	}
	return &kmspb.DecryptResponse{Plaintext: bytes.TrimPrefix(req.GetCiphertext(), prefix)}, nil
}

const (
	globalKey  = "projects/p/locations/global/keyRings/r/cryptoKeys/k"
	centralKey = "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k"
	eastKey    = "projects/p/locations/us-east1/keyRings/r/cryptoKeys/k"
)

func cmekStore(t *testing.T) (*Store, *fakeKMS) {
	t.Helper()
	s, f := newTestStore(t), newFakeKMS()
	s.SetKeyService(func() KeyService { return f })
	return s, f
}

func automaticCMEK(key string) *secretmanagerpb.Replication {
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{
		CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: key}}}}
}

func userManagedCMEK(pairs ...string) *secretmanagerpb.Replication {
	um := &secretmanagerpb.Replication_UserManaged{}
	for i := 0; i < len(pairs); i += 2 {
		r := &secretmanagerpb.Replication_UserManaged_Replica{Location: pairs[i]}
		if pairs[i+1] != "" {
			r.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: pairs[i+1]}
		}
		um.Replicas = append(um.Replicas, r)
	}
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: um}}
}

func createWith(g *GRPCServer, id string, r *secretmanagerpb.Replication) (*secretmanagerpb.Secret, error) {
	return g.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: id, Secret: &secretmanagerpb.Secret{Replication: r}})
}

func TestCMEKKeyRules(t *testing.T) {
	t.Parallel()
	s, _ := cmekStore(t)
	g := NewGRPCServer(s)
	for name, r := range map[string]*secretmanagerpb.Replication{
		"regional key on automatic": automaticCMEK(centralKey),
		"key in another location":   userManagedCMEK("us-east1", centralKey),
		"not a CryptoKey":           automaticCMEK("projects/p/locations/global/keyRings/r"),
		"empty kms_key_name": {Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{
			CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{}}}},
	} {
		if _, err := createWith(g, "bad", r); apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if _, err := createWith(g, "good", automaticCMEK(globalKey)); err != nil {
		t.Fatalf("global key on automatic: %v", err)
	}
	got, err := createWith(g, "um", userManagedCMEK("us-central1", centralKey, "us-east1", ""))
	if err != nil {
		t.Fatalf("user-managed: %v", err)
	}
	reps := got.GetReplication().GetUserManaged().GetReplicas()
	if reps[0].GetCustomerManagedEncryption().GetKmsKeyName() != centralKey || reps[1].GetCustomerManagedEncryption() != nil {
		t.Errorf("replicas = %v", reps)
	}

	// Without Cloud KMS on the instance, CMEK cannot be honoured.
	bare := NewGRPCServer(newTestStore(t))
	_, err = createWith(bare, "x", automaticCMEK(globalKey))
	wantCode(t, err, codes.Unimplemented)
}

func TestCMEKEncryptsAndFailsWhenTheKeyIsDisabled(t *testing.T) {
	t.Parallel()
	s, f := cmekStore(t)
	g := NewGRPCServer(s)
	if _, err := createWith(g, "a", automaticCMEK(globalKey)); err != nil {
		t.Fatal(err)
	}
	v, err := g.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{
		Parent: "projects/p/secrets/a", Payload: &secretmanagerpb.SecretPayload{Data: []byte("hunter2")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName(); got != globalKey+"/cryptoKeyVersions/1" {
		t.Errorf("kms_key_version_name = %q", got)
	}
	stored, err := s.GetVersion("p", "a", "1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Payload, []byte("hunter2")) || stored.Size() != 7 {
		t.Errorf("stored payload is not sealed, or size %d", stored.Size())
	}
	raw, _ := json.Marshal(stored)
	if bytes.Contains(raw, []byte("hunter2")) {
		t.Error("stored version holds the plaintext")
	}
	acc, err := g.AccessSecretVersion(context.Background(), &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/p/secrets/a/versions/1"})
	if err != nil || string(acc.GetPayload().GetData()) != "hunter2" || acc.GetPayload().GetDataCrc32C() != payloadCRC32C([]byte("hunter2")) {
		t.Fatalf("access = %v, %v", acc, err)
	}

	f.setDisabled(globalKey, true)
	_, err = g.AccessSecretVersion(context.Background(), &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/p/secrets/a/versions/latest"})
	wantCode(t, err, codes.FailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), globalKey) {
		t.Errorf("error does not name the key: %v", err)
	}
	_, err = s.AddVersion("p", "a", []byte("next"))
	wantCode(t, err, codes.FailedPrecondition)

	// The failed add used no version number.
	f.setDisabled(globalKey, false)
	v2, err := s.AddVersion("p", "a", []byte("next"))
	if err != nil || v2.Number != 2 {
		t.Fatalf("add after re-enable = %v, %v", v2.Number, err)
	}
	if _, err := s.AccessVersion("p", "a", "1"); err != nil {
		t.Errorf("access after re-enable: %v", err)
	}

	// Without a cluster there is nowhere to mount the decrypted copy.
	_, _, err = s.ResolveSecretRef("p", "a", "1")
	wantCode(t, err, codes.Unimplemented)

	// Destroying drops the wrapped data key with the payload.
	d, err := s.DestroyVersion("p", "a", "1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Payload != nil || d.Replicas[0].WrappedKey != nil || d.Replicas[0].KeyVersion == "" {
		t.Errorf("destroyed = %+v", d)
	}
}

func TestCMEKEveryReplicaKeyMustDecrypt(t *testing.T) {
	t.Parallel()
	s, f := cmekStore(t)
	g := NewGRPCServer(s)
	if _, err := createWith(g, "um", userManagedCMEK("us-central1", centralKey, "us-east1", eastKey, "europe-west1", "")); err != nil {
		t.Fatal(err)
	}
	v, err := s.AddVersion("p", "um", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	st := toProtoVersion(v).GetReplicationStatus().GetUserManaged().GetReplicas()
	if len(st) != 3 || st[0].GetLocation() != "us-central1" || st[0].GetCustomerManagedEncryption().GetKmsKeyVersionName() != centralKey+"/cryptoKeyVersions/1" ||
		st[1].GetCustomerManagedEncryption().GetKmsKeyVersionName() != eastKey+"/cryptoKeyVersions/1" || st[2].GetCustomerManagedEncryption() != nil {
		t.Errorf("replication_status = %v", st)
	}
	f.setDisabled(eastKey, true)
	_, err = s.AccessVersion("p", "um", "1")
	wantCode(t, err, codes.FailedPrecondition)
	f.setDisabled(eastKey, false)
	if got, err := s.AccessVersion("p", "um", "1"); err != nil || string(got.Payload) != "payload" {
		t.Errorf("access = %q, %v", got.Payload, err)
	}
}

func TestCMEKKeysCanBeChangedNothingElse(t *testing.T) {
	t.Parallel()
	s, f := cmekStore(t)
	g := NewGRPCServer(s)
	ctx := context.Background()
	if _, err := createWith(g, "a", automaticCMEK(globalKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddVersion("p", "a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	update := func(r *secretmanagerpb.Replication) (*secretmanagerpb.Secret, error) {
		return g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
			Secret:     &secretmanagerpb.Secret{Name: "projects/p/secrets/a", Replication: r},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replication"}}})
	}
	other := "projects/p/locations/global/keyRings/r/cryptoKeys/other"
	got, err := update(automaticCMEK(other))
	if err != nil || got.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName() != other {
		t.Fatalf("set-kms-key = %v, %v", got, err)
	}
	v2, err := s.AddVersion("p", "a", []byte("two"))
	if err != nil || v2.Replicas[0].KMSKey != other {
		t.Fatalf("version 2 = %+v, %v", v2, err)
	}
	// Existing versions are not re-encrypted: version 1 still needs the
	// first key.
	f.setDisabled(globalKey, true)
	_, err = s.AccessVersion("p", "a", "1")
	wantCode(t, err, codes.FailedPrecondition)
	if _, err := s.AccessVersion("p", "a", "2"); err != nil {
		t.Errorf("version 2: %v", err)
	}

	// remove-cmek sends automatic {}.
	got, err = update(&secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}})
	if err != nil || got.GetReplication().GetAutomatic().GetCustomerManagedEncryption() != nil {
		t.Fatalf("remove-cmek = %v, %v", got, err)
	}
	v3, err := s.AddVersion("p", "a", []byte("three"))
	if err != nil || v3.encrypted() || string(v3.Payload) != "three" {
		t.Errorf("version 3 = %+v, %v", v3, err)
	}

	// The policy itself stays immutable.
	_, err = update(userManagedCMEK("global", ""))
	wantCode(t, err, codes.InvalidArgument)
}

func TestCMEKOverREST(t *testing.T) {
	t.Parallel()
	s, _ := cmekStore(t)
	if _, err := createWith(NewGRPCServer(s), "a", userManagedCMEK("us-central1", centralKey)); err != nil {
		t.Fatal(err)
	}
	sec, _ := s.GetSecret("p", "a")
	b, _ := json.Marshal(renderSecret(sec))
	if !strings.Contains(string(b), `"customerManagedEncryption":{"kmsKeyName":"`+centralKey+`"}`) {
		t.Errorf("secret JSON = %s", b)
	}
	v, err := s.AddVersion("p", "a", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(renderVersion(v))
	if !strings.Contains(string(b), `"kmsKeyVersionName":"`+centralKey+`/cryptoKeyVersions/1"`) {
		t.Errorf("version JSON = %s", b)
	}

	filter, err := parseSecretFilter("replication.user_managed.replicas.customer_managed_encryption.kms_key_name:" + centralKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := filterList([]Secret{sec}, filter); len(got) != 1 {
		t.Errorf("filter matched %d", len(got))
	}
}
