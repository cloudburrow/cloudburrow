//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSpeechCMEK (#1950): Speech-to-Text v2's Config.kms_key_name, a key of
// the instance's Cloud KMS in the endpoint's location, through the official
// cloud.google.com/go/speech/apiv2 client: UpdateConfig sets it and
// GetConfig answers it; a recognizer and a phrase set created after it
// report kms_key_name and kms_key_version_name; while the version is
// disabled GetRecognizer, GetPhraseSet and Recognize are FAILED_PRECONDITION;
// enabled again they read; the key removed, an UpdateRecognizer decrypts the
// recognizer. Measured first: GetConfig and UpdateConfig were 501 and
// Recognizer.kms_key_name refused.
//
// covers: google.cloud.speech.v2.Speech/GetConfig, google.cloud.speech.v2.Speech/UpdateConfig, google.cloud.speech.v2.Speech/CreateRecognizer, google.cloud.speech.v2.Speech/GetRecognizer, google.cloud.speech.v2.Speech/UpdateRecognizer, google.cloud.speech.v2.Speech/CreatePhraseSet, google.cloud.speech.v2.Speech/GetPhraseSet
func TestSpeechCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	g, r := speechClients(t, h)
	k := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	agent := aiServiceAgent(h.Project(), "gcp-sa-speech.iam.gserviceaccount.com")
	key := aiCMEKKey(t, h, k, "us", "speech-cmek", agent)
	wrong := aiCMEKKey(t, h, k, "europe-west1", "speech-cmek-eu", agent)
	parent := "projects/" + h.Project() + "/locations/us"
	cfgName := parent + "/config"
	if _, err := g.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName, KmsKeyName: wrong}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key outside the endpoint's location: %v", err)
	}
	c, err := g.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName, KmsKeyName: key}})
	if err != nil || c.GetKmsKeyName() != key {
		t.Fatalf("UpdateConfig: %v %v", c, err)
	}
	t.Cleanup(func() {
		_, _ = g.UpdateConfig(h.Context(), &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName}})
	})
	if got, err := r.GetConfig(ctx, &speechpb.GetConfigRequest{Name: cfgName}); err != nil || got.GetKmsKeyName() != key || got.GetUpdateTime() == nil {
		t.Fatalf("GetConfig over REST: %v %v", got, err)
	}
	rec := createRecognizer(t, h, g, parent, "cmek-rec")
	v1 := key + "/cryptoKeyVersions/1"
	if rec.GetKmsKeyName() != key || rec.GetKmsKeyVersionName() != v1 {
		t.Fatalf("recognizer created with the config key: kms_key_name %q, kms_key_version_name %q", rec.GetKmsKeyName(), rec.GetKmsKeyVersionName())
	}
	op, err := g.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "cmek-phrases",
		PhraseSet: &speechpb.PhraseSet{Phrases: []*speechpb.PhraseSet_Phrase{{Value: "CloudBurrow"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := op.Wait(ctx)
	if err != nil || ps.GetKmsKeyVersionName() != v1 {
		t.Fatalf("CreatePhraseSet: %v %v", ps, err)
	}
	t.Cleanup(func() {
		if op, err := g.DeletePhraseSet(h.Context(), &speechpb.DeletePhraseSetRequest{Name: ps.GetName()}); err == nil {
			_, _ = op.Wait(h.Context())
		}
	})

	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := g.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: rec.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetRecognizer with the version disabled: %v; want FAILED_PRECONDITION", err)
	}
	if _, err := r.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: ps.GetName()}); !restFailedPrecondition(err) {
		t.Errorf("GetPhraseSet over REST with the version disabled: %v; want FAILED_PRECONDITION", err)
	}
	if _, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: rec.GetName(),
		AudioSource: &speechpb.RecognizeRequest_Content{Content: []byte("RIFF")}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Recognize with the version disabled: %v; want FAILED_PRECONDITION", err)
	}
	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_ENABLED)
	got, err := g.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: rec.GetName()})
	if err != nil || got.GetDefaultRecognitionConfig().GetModel() != "chirp" {
		t.Fatalf("GetRecognizer with the version enabled again: %v %v", got, err)
	}

	// The key removed: an Update* decrypts.
	if _, err := g.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{Name: cfgName}}); err != nil {
		t.Fatal(err)
	}
	uop, err := g.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: rec.GetName(), DisplayName: "plain"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatal(err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetKmsKeyName() != "" || u.GetKmsKeyVersionName() != "" {
		t.Fatalf("UpdateRecognizer without a config key: %v %v", u, err)
	}
	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_DISABLED)
	defer setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_ENABLED)
	if _, err := g.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: rec.GetName()}); err != nil {
		t.Errorf("GetRecognizer once decrypted, with the version disabled: %v", err)
	}
}
