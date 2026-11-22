//go:build compat

package compat

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	speechv1 "cloud.google.com/go/speech/apiv1"
	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	speechv2pb "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestSpeechFeatureMatrix2127 (#2127): Speech-to-Text's per-model,
// per-location, per-language feature matrix, as Google's supported
// languages tables mark it (v2's and v1's, read 2026-11-21;
// internal/speechconfig/data/supported_languages.tsv), through the official
// cloud.google.com/go/speech apiv2 (gRPC and REST) and apiv1 clients on the
// local AI endpoint, never Google's. A sample of the cells:
//
//   - supported, and working on whisper.cpp's JFK sample: chirp's automatic
//     punctuation in us-central1 (the transcript keeps its commas); long in
//     global without enableAutomaticPunctuation has none, with it has
//     them; chirp_2's word-level confidence in us-central1 (each word has
//     one); chirp_3's profanity filter in us; v1's latest_long spoken
//     punctuation, and v1's default model's automatic punctuation, which
//     without the flag leaves none.
//   - unsupported, INVALID_ARGUMENT: chirp's profanity filter, word-level
//     confidence, spoken punctuation, spoken emoji and adaptation in
//     us-central1; chirp in global; chirp_3's word confidence;
//     chirp_telephony's automatic punctuation; long's diarization; a
//     language short does not take; long in us-central1; and in v1, the
//     default model's spoken punctuation and af-ZA command_and_search's
//     automatic punctuation.
//
// covers: google.cloud.speech.v2.Speech/Recognize, google.cloud.speech.v1.Speech/Recognize
func TestSpeechFeatureMatrix2127(t *testing.T) {
	h := New(t)
	g, r := speechClients(t, h)
	v1, err := speechv1.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvLocalAI)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speechv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = v1.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	jfk, err := os.ReadFile("../../internal/speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	recognizer := func(loc string) string { return "projects/" + h.Project() + "/locations/" + loc + "/recognizers/_" }
	auto := &speechv2pb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechv2pb.AutoDetectDecodingConfig{}}
	type cell struct {
		model, loc, lang string
		f                *speechv2pb.RecognitionFeatures
		adaptation       bool
	}
	request := func(c cell) *speechv2pb.RecognizeRequest {
		cfg := &speechv2pb.RecognitionConfig{Model: c.model, LanguageCodes: []string{c.lang}, DecodingConfig: auto, Features: c.f}
		if c.adaptation {
			cfg.Adaptation = &speechv2pb.SpeechAdaptation{PhraseSets: []*speechv2pb.SpeechAdaptation_AdaptationPhraseSet{{
				Value: &speechv2pb.SpeechAdaptation_AdaptationPhraseSet_InlinePhraseSet{InlinePhraseSet: &speechv2pb.PhraseSet{
					Phrases: []*speechv2pb.PhraseSet_Phrase{{Value: "fellow Americans"}}}}}}}
		}
		return &speechv2pb.RecognizeRequest{Recognizer: recognizer(c.loc), Config: cfg, AudioSource: &speechv2pb.RecognizeRequest_Content{Content: jfk}}
	}
	transcript := func(res *speechv2pb.RecognizeResponse) string {
		var b []string
		for _, x := range res.GetResults() {
			if len(x.GetAlternatives()) > 0 {
				b = append(b, x.GetAlternatives()[0].GetTranscript())
			}
		}
		return strings.Join(b, " ")
	}

	// Unsupported cells, over gRPC and REST.
	for name, c := range map[string]cell{
		"chirp profanity filter":         {"chirp", "us-central1", "en-US", &speechv2pb.RecognitionFeatures{ProfanityFilter: true}, false},
		"chirp word confidence":          {"chirp", "europe-west4", "en-US", &speechv2pb.RecognitionFeatures{EnableWordConfidence: true}, false},
		"chirp spoken punctuation":       {"chirp", "us-central1", "en-US", &speechv2pb.RecognitionFeatures{EnableSpokenPunctuation: true}, false},
		"chirp spoken emoji":             {"chirp", "asia-southeast1", "en-US", &speechv2pb.RecognitionFeatures{EnableSpokenEmojis: true}, false},
		"chirp adaptation":               {"chirp", "us-central1", "en-US", nil, true},
		"chirp in global":                {"chirp", "global", "en-US", nil, false},
		"chirp_3 word confidence":        {"chirp_3", "us", "en-US", &speechv2pb.RecognitionFeatures{EnableWordConfidence: true}, false},
		"chirp_telephony punctuation":    {"chirp_telephony", "us-central1", "en-US", &speechv2pb.RecognitionFeatures{EnableAutomaticPunctuation: true}, false},
		"long diarization":               {"long", "global", "en-US", &speechv2pb.RecognitionFeatures{DiarizationConfig: &speechv2pb.SpeakerDiarizationConfig{}}, false},
		"short in an unlisted language":  {"short", "eu", "af-ZA", nil, false},
		"long in us-central1":            {"long", "us-central1", "en-US", nil, false},
		"chirp_2 spoken punctuation":     {"chirp_2", "us-central1", "en-US", &speechv2pb.RecognitionFeatures{EnableSpokenPunctuation: true}, false},
		"long de-DE spoken emoji":        {"long", "global", "de-DE", &speechv2pb.RecognitionFeatures{EnableSpokenEmojis: true}, false},
		"telephony diarization in us":    {"telephony", "us", "en-US", &speechv2pb.RecognitionFeatures{DiarizationConfig: &speechv2pb.SpeakerDiarizationConfig{}}, false},
		"chirp_3 spoken punctuation, eu": {"chirp_3", "eu", "en-US", &speechv2pb.RecognitionFeatures{EnableSpokenPunctuation: true}, false},
	} {
		if _, err := g.Recognize(ctx, request(c)); status.Code(err) != codes.InvalidArgument {
			t.Errorf("gRPC %s: %v, want InvalidArgument", name, err)
		}
		if _, err := r.Recognize(ctx, request(c)); status.Code(err) != codes.InvalidArgument {
			t.Errorf("REST %s: %v, want InvalidArgument", name, err)
		}
	}

	// Supported cells, transcribed.
	res, err := g.Recognize(ctx, request(cell{"chirp", "us-central1", "en-US", &speechv2pb.RecognitionFeatures{EnableAutomaticPunctuation: true}, false}))
	if err != nil || !strings.Contains(transcript(res), ",") || !strings.Contains(strings.ToLower(transcript(res)), "your country") {
		t.Errorf("chirp with automatic punctuation: %q %v", transcript(res), err)
	}
	res, err = r.Recognize(ctx, request(cell{"long", "global", "en-US", &speechv2pb.RecognitionFeatures{EnableWordTimeOffsets: true}, false}))
	if text := transcript(res); err != nil || strings.ContainsAny(text, ",.?!") || !strings.Contains(strings.ToLower(text), "your country") {
		t.Errorf("long without automatic punctuation: %q %v", text, err)
	}
	for _, x := range res.GetResults() {
		for _, w := range x.GetAlternatives()[0].GetWords() {
			if strings.ContainsAny(w.GetWord(), ",.?!") {
				t.Errorf("long without automatic punctuation: word %q", w.GetWord())
			}
		}
	}
	res, err = g.Recognize(ctx, request(cell{"long", "global", "en-US", &speechv2pb.RecognitionFeatures{EnableAutomaticPunctuation: true}, false}))
	if err != nil || !strings.Contains(transcript(res), ",") {
		t.Errorf("long with automatic punctuation: %q %v", transcript(res), err)
	}
	res, err = g.Recognize(ctx, request(cell{"chirp_2", "us-central1", "en-US", &speechv2pb.RecognitionFeatures{EnableWordConfidence: true}, false}))
	if err != nil || len(res.GetResults()) == 0 || len(res.GetResults()[0].GetAlternatives()[0].GetWords()) == 0 {
		t.Fatalf("chirp_2 with word confidence: %v %v", res, err)
	}
	for _, w := range res.GetResults()[0].GetAlternatives()[0].GetWords() {
		if w.GetConfidence() <= 0 || w.GetConfidence() > 1 {
			t.Errorf("chirp_2 word %q confidence %v", w.GetWord(), w.GetConfidence())
		}
	}
	res, err = r.Recognize(ctx, request(cell{"chirp_3", "us", "en-US", &speechv2pb.RecognitionFeatures{ProfanityFilter: true}, true}))
	if err != nil || !strings.Contains(strings.ToLower(transcript(res)), "your country") {
		t.Errorf("chirp_3 with the profanity filter and adaptation: %q %v", transcript(res), err)
	}

	// v1: its own table.
	v1cfg := func(model string, c *speechv1pb.RecognitionConfig) *speechv1pb.RecognizeRequest {
		c.LanguageCode, c.Model = "en-US", model
		c.Encoding, c.SampleRateHertz = speechv1pb.RecognitionConfig_LINEAR16, 16000
		return &speechv1pb.RecognizeRequest{Config: c, Audio: &speechv1pb.RecognitionAudio{AudioSource: &speechv1pb.RecognitionAudio_Content{
			Content: wavPCM(t, "../../internal/speech/testdata/jfk.wav")}}}
	}
	v1text := func(res *speechv1pb.RecognizeResponse) string {
		var b []string
		for _, x := range res.GetResults() {
			b = append(b, x.GetAlternatives()[0].GetTranscript())
		}
		return strings.Join(b, " ")
	}
	if _, err := v1.Recognize(ctx, v1cfg("default", &speechv1pb.RecognitionConfig{EnableSpokenPunctuation: wrapperspb.Bool(true)})); status.Code(err) != codes.InvalidArgument {
		t.Errorf("v1 default spoken punctuation: %v, want InvalidArgument", err)
	}
	afZA := v1cfg("command_and_search", &speechv1pb.RecognitionConfig{EnableAutomaticPunctuation: true})
	afZA.Config.LanguageCode = "af-ZA"
	if _, err := v1.Recognize(ctx, afZA); status.Code(err) != codes.InvalidArgument {
		t.Errorf("v1 af-ZA command_and_search automatic punctuation: %v, want InvalidArgument", err)
	}
	if res, err := v1.Recognize(ctx, v1cfg("latest_long", &speechv1pb.RecognitionConfig{EnableSpokenPunctuation: wrapperspb.Bool(true)})); err != nil ||
		!strings.Contains(strings.ToLower(v1text(res)), "your country") {
		t.Errorf("v1 latest_long spoken punctuation: %q %v", v1text(res), err)
	}
	if res, err := v1.Recognize(ctx, v1cfg("", &speechv1pb.RecognitionConfig{EnableAutomaticPunctuation: true})); err != nil || !strings.Contains(v1text(res), ",") {
		t.Errorf("v1 default automatic punctuation: %q %v", v1text(res), err)
	}
	if res, err := v1.Recognize(ctx, v1cfg("", &speechv1pb.RecognitionConfig{})); err != nil || strings.ContainsAny(v1text(res), ",.?!") ||
		!strings.Contains(strings.ToLower(v1text(res)), "your country") {
		t.Errorf("v1 default without automatic punctuation: %q %v", v1text(res), err)
	}
}
