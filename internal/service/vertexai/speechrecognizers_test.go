package vertexai

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const speechParent = "projects/p1/locations/us"

func newSpeech(t *testing.T, file string) *speechService {
	t.Helper()
	s := NewServer(nil)
	s.SetSpeechState(file)
	return &speechService{s: s}
}

// TestSpeechRecognizers: a recognizer's lifecycle, its refusals and its
// persistence.
func TestSpeechRecognizers(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "recognizers.json")
	v := newSpeech(t, file)
	cfg := &speechpb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"},
		DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}
	op, err := v.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "rec-1",
		Recognizer: &speechpb.Recognizer{DisplayName: "one", DefaultRecognitionConfig: cfg}})
	if err != nil || !op.GetDone() || op.GetResponse() == nil {
		t.Fatalf("CreateRecognizer: %v %v", op, err)
	}
	name := speechParent + "/recognizers/rec-1"
	for _, c := range []struct {
		req  *speechpb.CreateRecognizerRequest
		want codes.Code
	}{
		{&speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "rec-1", Recognizer: &speechpb.Recognizer{}}, codes.AlreadyExists},
		{&speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "Bad_ID", Recognizer: &speechpb.Recognizer{}}, codes.InvalidArgument},
		{&speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "abc", Recognizer: &speechpb.Recognizer{}}, codes.InvalidArgument},
		{&speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "rec-2", Recognizer: &speechpb.Recognizer{Model: "chirp"}}, codes.Unimplemented},
	} {
		if _, err := v.CreateRecognizer(ctx, c.req); code(err) != c.want {
			t.Errorf("CreateRecognizer %s: %v, want %v", c.req.GetRecognizerId(), err, c.want)
		}
	}
	if _, err := v.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "dry-run",
		Recognizer: &speechpb.Recognizer{}, ValidateOnly: true}); err != nil {
		t.Errorf("validate_only: %v", err)
	}
	if _, err := v.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: speechParent + "/recognizers/dry-run"}); code(err) != codes.NotFound {
		t.Errorf("validate_only created it: %v", err)
	}

	got, err := v.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name})
	if err != nil || got.GetState() != speechpb.Recognizer_ACTIVE || got.GetEtag() == "" || got.GetDefaultRecognitionConfig().GetModel() != "chirp" {
		t.Fatalf("GetRecognizer: %v %v", got, err)
	}
	// An update of one config field keeps the others; a stale etag aborts.
	if _, err := v.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: name, Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
	op, err = v.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: name, DisplayName: "renamed",
		DefaultRecognitionConfig: &speechpb.RecognitionConfig{Features: &speechpb.RecognitionFeatures{EnableWordTimeOffsets: true}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "default_recognition_config.features"}}})
	if err != nil {
		t.Fatalf("UpdateRecognizer: %v", err)
	}
	got, _ = v.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name})
	if got.GetDisplayName() != "renamed" || got.GetDefaultRecognitionConfig().GetModel() != "chirp" ||
		!got.GetDefaultRecognitionConfig().GetFeatures().GetEnableWordTimeOffsets() {
		t.Errorf("after update: %v", got)
	}

	// Recognize merges the request's config over the recognizer's.
	merged, err := v.s.recognizerConfig(name, json.RawMessage(`{"language_codes": ["de-DE"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(merged, &m)
	if m["model"] != "chirp" || m["languageCodes"].([]any)[0] != "de-DE" || m["features"] == nil || m["autoDecodingConfig"] == nil {
		t.Errorf("merged config: %s", merged)
	}

	op, err = v.DeleteRecognizer(ctx, &speechpb.DeleteRecognizerRequest{Name: name, AllowMissing: true})
	if err != nil || !op.GetDone() {
		t.Fatalf("DeleteRecognizer: %v %v", op, err)
	}
	list, _ := v.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: speechParent})
	all, _ := v.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: speechParent, ShowDeleted: true})
	if len(list.GetRecognizers()) != 0 || len(all.GetRecognizers()) != 1 || all.Recognizers[0].GetState() != speechpb.Recognizer_DELETED ||
		all.Recognizers[0].GetExpireTime() == nil {
		t.Errorf("lists after delete: %v / %v", list, all)
	}
	if _, err := v.s.recognizerConfig(name, nil); code(grpcError(err)) != codes.FailedPrecondition {
		t.Errorf("recognize on a deleted recognizer: %v", err)
	}
	if _, err := v.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: speechParent + "/recognizers/none",
		AudioSource: &speechpb.RecognizeRequest_Content{Content: []byte("x")}}); code(err) != codes.NotFound {
		t.Errorf("recognize on a missing recognizer: %v", err)
	}
	if _, err := v.DeleteRecognizer(ctx, &speechpb.DeleteRecognizerRequest{Name: speechParent + "/recognizers/none", AllowMissing: true}); err != nil {
		t.Errorf("allow_missing: %v", err)
	}

	// The recognizers survive a restart, and undelete brings it back.
	v2 := newSpeech(t, file)
	if _, err := v2.UndeleteRecognizer(ctx, &speechpb.UndeleteRecognizerRequest{Name: name}); err != nil {
		t.Fatalf("UndeleteRecognizer after reload: %v", err)
	}
	got, err = v2.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: name})
	if err != nil || got.GetState() != speechpb.Recognizer_ACTIVE || got.GetDisplayName() != "renamed" || got.GetDeleteTime() != nil {
		t.Errorf("after undelete: %v %v", got, err)
	}
	if _, err := v2.UndeleteRecognizer(ctx, &speechpb.UndeleteRecognizerRequest{Name: name}); code(err) != codes.FailedPrecondition {
		t.Errorf("undelete of an active recognizer: %v", err)
	}
	if _, err := v2.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: name, ConfigMask: &fieldmaskpb.FieldMask{Paths: []string{"model"}},
		AudioSource: &speechpb.RecognizeRequest_Content{Content: []byte("x")}}); code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "config_mask") {
		t.Errorf("config_mask: %v", err)
	}
}
