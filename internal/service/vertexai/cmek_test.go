package vertexai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func testKey(t *testing.T, k *kms.Server, loc, id string) string {
	t.Helper()
	ctx := context.Background()
	parent := "projects/my-proj/locations/" + loc
	ring := parent + "/keyRings/ring"
	if _, err := k.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		if _, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: "ring"}); err != nil {
			t.Fatal(err)
		}
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return key.GetName()
}

func setKeyState(t *testing.T, k *kms.Server, key string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := k.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
}

func useKMS(s *Server, k *kms.Server) {
	s.UseCMEK(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
}

// TestDocumentAICMEK (#1950): a processor with kms_key_name wraps a data
// key with it, its versions report the key version, and processing is
// refused while that version is disabled.
func TestDocumentAICMEK(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	key := testKey(t, k, "us-central1", "dai")
	euKey := testKey(t, k, "europe-west4", "dai-eu")
	file := filepath.Join(t.TempDir(), "processors.json")
	d := newDocAI(t, file)
	in := &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "enc", KmsKeyName: key}
	if _, err := d.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: docAIParentUS, Processor: in}); code(err) != codes.FailedPrecondition {
		t.Fatalf("without Cloud KMS: %v", err)
	}
	useKMS(d.s, k)
	if _, err := d.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: docAIParentUS,
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "eu", KmsKeyName: euKey}}); code(err) != codes.InvalidArgument {
		t.Fatalf("a key in another location: %v", err)
	}
	p, err := d.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: docAIParentUS, Processor: in})
	if err != nil || p.GetKmsKeyName() != key {
		t.Fatalf("CreateProcessor: %v %v", p, err)
	}
	v, err := d.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetDefaultProcessorVersion()})
	if err != nil || v.GetKmsKeyName() != key || v.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Fatalf("GetProcessorVersion: %v %v", v, err)
	}
	raw := &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: []byte("x"), MimeType: "text/html"}}
	setKeyState(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: raw}); code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "cryptoKeyVersions/1") {
		t.Fatalf("ProcessDocument with the version disabled: %v", err)
	}
	if _, err := d.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetDefaultProcessorVersion()}); code(err) != codes.FailedPrecondition {
		t.Fatalf("GetProcessorVersion with the version disabled: %v", err)
	}
	if got, err := d.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: p.GetName()}); err != nil || got.GetKmsKeyName() != key {
		t.Fatalf("GetProcessor (no user data) with the version disabled: %v %v", got, err)
	}
	setKeyState(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	// Reloaded: the wrapped key was kept, and still opens.
	d2 := newDocAI(t, file)
	useKMS(d2.s, k)
	if _, err := d2.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetDefaultProcessorVersion()}); err != nil {
		t.Fatalf("after reload: %v", err)
	}
}

// TestSpeechCMEK (#1950): a config key encrypts the resources created
// after it, which report the key version and are kept sealed; a disabled
// version refuses their reads; an update re-encrypts or decrypts them.
func TestSpeechCMEK(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	key := testKey(t, k, "us", "stt")
	other := testKey(t, k, "europe-west1", "stt-eu")
	file := filepath.Join(t.TempDir(), "recognizers.json")
	v := newSpeech(t, file)
	useKMS(v.s, k)
	cfgName := speechParent + "/config"
	if _, err := v.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName, KmsKeyName: other}}); code(err) != codes.InvalidArgument {
		t.Fatalf("a key outside the location: %v", err)
	}
	c, err := v.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName, KmsKeyName: key}})
	if err != nil || c.GetKmsKeyName() != key || c.GetUpdateTime() == nil {
		t.Fatalf("UpdateConfig: %v %v", c, err)
	}
	if got, _ := v.GetConfig(ctx, &speechpb.GetConfigRequest{Name: cfgName}); got.GetKmsKeyName() != key {
		t.Fatalf("GetConfig: %v", got)
	}
	cfg := &speechpb.RecognitionConfig{Model: "secretmodel", LanguageCodes: []string{"en-US"}}
	if _, err := v.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "rec-enc",
		Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: cfg}}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: speechParent, PhraseSetId: "ps-enc",
		PhraseSet: &speechpb.PhraseSet{Phrases: []*speechpb.PhraseSet_Phrase{{Value: "secretphrase"}}}}); err != nil {
		t.Fatal(err)
	}
	name := speechParent + "/recognizers/rec-enc"
	r, err := v.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name})
	if err != nil || r.GetKmsKeyName() != key || r.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" || r.GetDefaultRecognitionConfig().GetModel() != "secretmodel" {
		t.Fatalf("GetRecognizer: %v %v", r, err)
	}
	for _, f := range []string{"recognizers.json", "phrasesets.json", "sealed.json"} {
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(file), f))
		if strings.Contains(string(b), "secretmodel") || strings.Contains(string(b), "secretphrase") {
			t.Errorf("%s holds the plaintext: %s", f, b)
		}
	}
	setKeyState(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := v.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name}); code(err) != codes.FailedPrecondition {
		t.Fatalf("GetRecognizer with the version disabled: %v", err)
	}
	if _, err := v.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: speechParent + "/phraseSets/ps-enc"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("GetPhraseSet with the version disabled: %v", err)
	}
	if _, err := v.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: name}); code(err) != codes.FailedPrecondition {
		t.Fatalf("Recognize with the version disabled: %v", err)
	}
	setKeyState(t, k, key, kmspb.CryptoKeyVersion_ENABLED)

	// Reloaded: the sealed parts open again.
	v2 := newSpeech(t, file)
	useKMS(v2.s, k)
	r, err = v2.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name})
	if err != nil || r.GetDefaultRecognitionConfig().GetModel() != "secretmodel" {
		t.Fatalf("after reload: %v %v", r, err)
	}
	// The key removed, an update decrypts the recognizer.
	if _, err := v2.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName}}); err != nil {
		t.Fatal(err)
	}
	if _, err := v2.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: name, DisplayName: "plain"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil {
		t.Fatal(err)
	}
	setKeyState(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	r, err = v2.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name})
	if err != nil || r.GetKmsKeyName() != "" || r.GetKmsKeyVersionName() != "" || r.GetDefaultRecognitionConfig().GetModel() != "secretmodel" {
		t.Fatalf("after the update without a key: %v %v", r, err)
	}
}

// TestSpeechCMEKAgent: with IAM enforced, the service agent must hold the
// role on the key.
func TestSpeechCMEKAgent(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	key := testKey(t, k, "us", "stt")
	v := newSpeech(t, "")
	agent := cmek.ServiceAgent("p1", speechAgentDomain)
	v.s.UseCMEK(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }, Check: func(_ context.Context, r iamcheck.Request) (iamcheck.Decision, error) {
		for _, p := range r.Own {
			for _, b := range p.Policy.Bindings {
				for _, m := range b.Members {
					if m == "serviceAccount:"+r.Signer && b.Role == "roles/cloudkms.cryptoKeyEncrypterDecrypter" {
						return iamcheck.Decision{Held: r.Permissions}, nil
					}
				}
			}
		}
		return iamcheck.Decision{}, nil
	}})
	cfg := &speechpb.Config{Name: speechParent + "/config", KmsKeyName: key}
	if _, err := v.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: cfg}); code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), agent) {
		t.Fatalf("without the role: %v", err)
	}
	if _, err := k.IAMPolicy().SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: key, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/cloudkms.cryptoKeyEncrypterDecrypter", Members: []string{"serviceAccount:" + agent}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: cfg}); err != nil {
		t.Fatalf("with the role: %v", err)
	}
}
