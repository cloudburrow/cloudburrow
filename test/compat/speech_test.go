//go:build compat

package compat

import (
	"os"
	"strings"
	"testing"

	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// speechClients are the official Speech-to-Text v2 clients, gRPC and REST,
// on the instance's local AI endpoint (#1799), never Google's.
func speechClients(t *testing.T, h *Harness) (*speech.Client, *speech.Client) {
	t.Helper()
	addr := h.Endpoint(EnvLocalAI)
	if addr == "" {
		t.Fatal("no local AI endpoint")
	}
	g, err := speech.NewClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speech.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	r, err := speech.NewRESTClient(h.Context(), option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("speech.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return g, r
}

// createRecognizer makes a chirp recognizer with word offsets in parent and
// deletes it when the test ends.
func createRecognizer(t *testing.T, h *Harness, c *speech.Client, parent, id string) *speechpb.Recognizer {
	t.Helper()
	op, err := c.CreateRecognizer(h.Context(), &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: id,
		Recognizer: &speechpb.Recognizer{DisplayName: id, DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp",
			LanguageCodes: []string{"en-US"}, Features: &speechpb.RecognitionFeatures{EnableWordTimeOffsets: true},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatalf("CreateRecognizer: %v", err)
	}
	r, err := op.Wait(h.Context())
	if err != nil {
		t.Fatalf("CreateRecognizer wait: %v", err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteRecognizer(h.Context(), &speechpb.DeleteRecognizerRequest{Name: r.GetName(), AllowMissing: true})
	})
	return r
}

// TestSpeechRecognizers (#1799): Speech-to-Text v2's recognizers on the
// local AI endpoint, through the official cloud.google.com/go/speech
// client over gRPC and REST: a recognizer created (an operation waited
// on), read, listed, updated by field mask (a stale etag ABORTED), deleted
// (listed only with show_deleted, recognize on it FAILED_PRECONDITION) and
// undeleted; an invalid recognizer_id INVALID_ARGUMENT; Recognize on the
// recognizer transcribing whisper.cpp's JFK sample with no config of its
// own (the recognizer's chirp and word offsets apply) and with a config
// that overrides its language.
//
// covers: google.cloud.speech.v2.Speech/CreateRecognizer, google.cloud.speech.v2.Speech/GetRecognizer, google.cloud.speech.v2.Speech/ListRecognizers, google.cloud.speech.v2.Speech/UpdateRecognizer, google.cloud.speech.v2.Speech/DeleteRecognizer, google.cloud.speech.v2.Speech/UndeleteRecognizer, google.cloud.speech.v2.Speech/Recognize
func TestSpeechRecognizers(t *testing.T) {
	h := New(t)
	g, r := speechClients(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/us"
	wav, err := os.ReadFile("../../internal/speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: "Not_Valid",
		Recognizer: &speechpb.Recognizer{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid recognizer_id: %v", err)
	}
	rec := createRecognizer(t, h, g, parent, "jfk-rec")
	if rec.GetState() != speechpb.Recognizer_ACTIVE || rec.GetEtag() == "" || rec.GetUid() == "" {
		t.Errorf("created: %v", rec)
	}
	got, err := r.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: rec.GetName()})
	if err != nil || got.GetDefaultRecognitionConfig().GetModel() != "chirp" || got.GetDisplayName() != "jfk-rec" {
		t.Fatalf("GetRecognizer (REST): %v %v", got, err)
	}
	found := false
	it := r.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: parent})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListRecognizers: %v", err)
		}
		found = found || x.GetName() == rec.GetName()
	}
	if !found {
		t.Errorf("ListRecognizers lacks %s", rec.GetName())
	}
	if _, err := g.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: rec.GetName(), Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
	uop, err := r.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: rec.GetName(), DisplayName: "kennedy"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateRecognizer (REST): %v", err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetDisplayName() != "kennedy" || up.GetDefaultRecognitionConfig().GetModel() != "chirp" {
		t.Errorf("updated: %v %v", up, err)
	}

	// Recognize on the recognizer: its config, then one overriding it.
	for _, c := range []struct {
		client *speech.Client
		config *speechpb.RecognitionConfig
	}{{g, nil}, {r, &speechpb.RecognitionConfig{LanguageCodes: []string{"en-GB"}}}} {
		res, err := c.client.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: rec.GetName(), Config: c.config,
			AudioSource: &speechpb.RecognizeRequest_Content{Content: wav}})
		if err != nil {
			t.Fatalf("Recognize %v: %v", c.config, err)
		}
		var text []string
		for _, x := range res.GetResults() {
			text = append(text, x.GetAlternatives()[0].GetTranscript())
		}
		words := res.GetResults()[0].GetAlternatives()[0].GetWords()
		if !strings.Contains(strings.ToLower(strings.Join(text, " ")), "ask not what your country can do for you") || len(words) == 0 ||
			words[1].GetStartOffset() == nil || res.GetMetadata().GetTotalBilledDuration().GetSeconds() != 11 {
			t.Errorf("Recognize %v: %v", c.config, res)
		}
		if want := map[bool]string{true: "en-us", false: "en-gb"}[c.config == nil]; res.GetResults()[0].GetLanguageCode() != want {
			t.Errorf("Recognize %v: language %q, want %q", c.config, res.GetResults()[0].GetLanguageCode(), want)
		}
	}

	dop, err := g.DeleteRecognizer(ctx, &speechpb.DeleteRecognizerRequest{Name: rec.GetName()})
	if err != nil {
		t.Fatalf("DeleteRecognizer: %v", err)
	}
	if d, err := dop.Wait(ctx); err != nil || d.GetState() != speechpb.Recognizer_DELETED || d.GetExpireTime() == nil {
		t.Errorf("deleted: %v %v", d, err)
	}
	if _, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: rec.GetName(),
		AudioSource: &speechpb.RecognizeRequest_Content{Content: wav}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Recognize on a deleted recognizer: %v", err)
	}
	it = g.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: parent, ShowDeleted: true})
	if x, err := it.Next(); err != nil || x.GetName() != rec.GetName() {
		t.Errorf("ListRecognizers show_deleted: %v %v", x, err)
	}
	unop, err := r.UndeleteRecognizer(ctx, &speechpb.UndeleteRecognizerRequest{Name: rec.GetName()})
	if err != nil {
		t.Fatalf("UndeleteRecognizer (REST): %v", err)
	}
	if u, err := unop.Wait(ctx); err != nil || u.GetState() != speechpb.Recognizer_ACTIVE {
		t.Errorf("undeleted: %v %v", u, err)
	}
}
