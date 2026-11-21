package dataform

import (
	"context"
	"os/exec"
	"testing"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// fakeKMS is one ENCRYPT_DECRYPT key whose version can be disabled.
type fakeKMS struct {
	key      string
	disabled bool
}

func (f *fakeKMS) version() string { return f.key + "/cryptoKeyVersions/1" }

func (f *fakeKMS) Encrypt(_ context.Context, in *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	if in.GetName() != f.key {
		return nil, status.Error(codes.NotFound, "no such key")
	}
	if f.disabled {
		return nil, status.Error(codes.FailedPrecondition, "the version is DISABLED")
	}
	return &kmspb.EncryptResponse{Name: f.version(), Ciphertext: append([]byte("w:"), in.GetPlaintext()...)}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	if f.disabled {
		return nil, status.Error(codes.FailedPrecondition, "the version is DISABLED")
	}
	return &kmspb.DecryptResponse{Plaintext: in.GetCiphertext()[2:]}, nil
}

func (f *fakeKMS) GetCryptoKey(_ context.Context, in *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	if in.GetName() != f.key {
		return nil, status.Error(codes.NotFound, "no such key")
	}
	st := kmspb.CryptoKeyVersion_ENABLED
	if f.disabled {
		st = kmspb.CryptoKeyVersion_DISABLED
	}
	return &kmspb.CryptoKey{Name: f.key, Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
		Primary: &kmspb.CryptoKeyVersion{Name: f.version(), State: st}}, nil
}

func (f *fakeKMS) GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return &iampb.Policy{}, nil
}

func code(err error) codes.Code { return apierror.From(err).Code }

// TestRepositoryKeys: a repository's key must be a usable key of its own
// location; it is recorded in data_encryption_state, cannot be changed,
// comes from Config's default when the request names none, and gates the
// repository's data while its version is disabled.
func TestRepositoryKeys(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx := context.Background()
	f := &fakeKMS{key: "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k"}
	s, err := NewServer(Options{DataDir: t.TempDir(), Keys: cmek.Keys{KMS: func() cmek.KMS { return f }}})
	if err != nil {
		t.Fatal(err)
	}
	parent := "projects/p/locations/us-central1"
	other := "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/k"
	if _, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "wrong",
		Repository: &dfpb.Repository{KmsKeyName: other}}); code(err) != codes.InvalidArgument {
		t.Errorf("a key of another location: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "missing",
		Repository: &dfpb.Repository{KmsKeyName: parent + "/keyRings/r/cryptoKeys/none"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("a missing key: %v, want FAILED_PRECONDITION", err)
	}
	r, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "keyed",
		Repository: &dfpb.Repository{KmsKeyName: f.key}})
	if err != nil {
		t.Fatal(err)
	}
	if r.GetDataEncryptionState().GetKmsKeyVersionName() != f.version() {
		t.Errorf("data_encryption_state = %v", r.GetDataEncryptionState())
	}
	if _, err := s.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: r.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("removing the key: %v, want INVALID_ARGUMENT", err)
	}
	if err := s.checkKey(ctx, r.GetName()+"/workspaces/w"); err != nil {
		t.Errorf("checkKey with the key enabled: %v", err)
	}
	if got := requestResource(&dfpb.WriteFileRequest{Workspace: r.GetName() + "/workspaces/w", Path: "a"}); got != r.GetName()+"/workspaces/w" {
		t.Errorf("requestResource = %q", got)
	}
	f.disabled = true
	if err := s.checkKey(ctx, r.GetName()+"/workspaces/w"); code(err) != codes.FailedPrecondition {
		t.Errorf("checkKey with the version disabled: %v, want FAILED_PRECONDITION", err)
	}
	if _, err := s.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: r.GetName()}); err != nil {
		t.Errorf("GetRepository with the key disabled: %v", err)
	}
	f.disabled = false

	cfg := parent + "/config"
	if _, err := s.UpdateConfig(ctx, &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: cfg, DefaultKmsKeyName: other}}); code(err) != codes.InvalidArgument {
		t.Errorf("a default key of another location: %v, want INVALID_ARGUMENT", err)
	}
	if c, err := s.UpdateConfig(ctx, &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: cfg, DefaultKmsKeyName: f.key}}); err != nil || c.GetDefaultKmsKeyName() != f.key {
		t.Fatalf("UpdateConfig = %v, %v", c, err)
	}
	d, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "defaulted"})
	if err != nil {
		t.Fatal(err)
	}
	if d.GetKmsKeyName() != f.key || d.GetDataEncryptionState() == nil {
		t.Errorf("a repository under a default key = %v", d)
	}
}

// TestExecutionMode: DEFAULT and ALL_EXCEPT_UNIT_TESTS are what every
// invocation does; UNIT_TESTS_ONLY is refused.
func TestExecutionMode(t *testing.T) {
	for mode, want := range map[dfpb.InvocationConfig_ExecutionMode]codes.Code{
		dfpb.InvocationConfig_DEFAULT: codes.OK, dfpb.InvocationConfig_ALL_EXCEPT_UNIT_TESTS: codes.OK,
		dfpb.InvocationConfig_UNIT_TESTS_ONLY: codes.Unimplemented,
	} {
		err := checkInvocationConfig(&dfpb.InvocationConfig{ExecutionMode: mode})
		if got := codes.OK; err != nil {
			got = code(err)
			if got != want {
				t.Errorf("%v: %v, want %v", mode, err, want)
			}
		} else if want != codes.OK {
			t.Errorf("%v accepted, want %v", mode, want)
		}
	}
}

// TestGitRepositoryLink: a link is checked through Developer Connect, and
// refused when Developer Connect is not served.
func TestGitRepositoryLink(t *testing.T) {
	ctx := context.Background()
	link := "projects/p/locations/us-central1/connections/c/gitRepositoryLinks/l"
	gr := &dfpb.Repository_GitRemoteSettings{Url: "http://x/sourcemanager/git/r", DefaultBranch: "main", GitRepositoryLink: &link}
	s, _ := NewServer(Options{})
	if err := s.checkRepository(ctx, &dfpb.Repository{GitRemoteSettings: gr}); code(err) != codes.FailedPrecondition {
		t.Errorf("without Developer Connect: %v, want FAILED_PRECONDITION", err)
	}
	bad := "projects/p/connections/c"
	if err := s.checkRepository(ctx, &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: gr.Url, DefaultBranch: "main", GitRepositoryLink: &bad}}); code(err) != codes.InvalidArgument {
		t.Errorf("a malformed link: %v, want INVALID_ARGUMENT", err)
	}
	asked := ""
	s, _ = NewServer(Options{LinkToken: func(_ context.Context, l string) (string, error) {
		asked = l
		return "", status.Error(codes.NotFound, "no such link")
	}})
	if err := s.checkRepository(ctx, &dfpb.Repository{GitRemoteSettings: gr}); code(err) != codes.InvalidArgument || asked != link {
		t.Errorf("a missing link: %v (asked %q), want INVALID_ARGUMENT", err, asked)
	}
}
