//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	speechv1 "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/speech/apiv1/speechpb"
	speech "cloud.google.com/go/speech/apiv2"
	speechv2pb "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestSpeechRecognitionFeatures1799 (#1799): Speech-to-Text v2's
// Recognize, through the official cloud.google.com/go/speech/apiv2 clients
// (gRPC and REST) on the local AI endpoint, never Google's, with the
// features local whisper.cpp now answers:
//
//   - profanityFilter, enableSpokenPunctuation and enableSpokenEmojis on a
//     clip (macOS say: "Hello comma this shit is great exclamation mark
//     thumbs up"): the profane word keeps its first letter ("s***"), the
//     spoken marks become "!" and the emoji 👍, from Google's inventories;
//     the words[] are masked too. Without the features the words stay.
//   - several languageCodes (long, global): a German clip (say -v Anna,
//     "Guten Morgen. Das Wetter ist heute sehr schön.") with en-US and
//     de-DE is recognised in German and labelled de-de; four codes, a
//     chirp model, or the us-central1 location are INVALID_ARGUMENT.
//   - translationConfig (chirp_2, de-DE to en-US): the German clip's
//     transcript in English, with no confidence; with no local generation
//     model, UNIMPLEMENTED. A pair Chirp 2 does not list, or another
//     model, is INVALID_ARGUMENT.
//   - AMR and AMR-WB audio (opencore-amr's decoders in WebAssembly): JFK's
//     sample as AMR 12.2 kbit/s and AMR-WB 23.85 kbit/s, detected by its
//     magic and as an explicit AMR / AMR_WB config, reads the sentence; v1's
//     AMR encoding too; AMR at 16000 Hz is INVALID_ARGUMENT.
//   - denoiserConfig.denoiseAudio (RNNoise in WebAssembly): JFK's sample
//     with seeded white noise added, denoised first, reads the sentence;
//     snrThreshold (#1799) silences the half-second windows whose
//     signal-to-noise ratio is below it: at 1 the sentence is still read,
//     at 1000 none of it is. diarizationConfig is INVALID_ARGUMENT on
//     chirp_2 (no diarization in the supported languages table) and stays
//     UNIMPLEMENTED on chirp_3.
//
// Speech-to-Text v1's Recognize (cloud.google.com/go/speech/apiv1) takes
// enable_spoken_punctuation, enable_spoken_emojis and profanity_filter too.
//
// covers: google.cloud.speech.v2.Speech/Recognize, google.cloud.speech.v1.Speech/Recognize
func TestSpeechRecognitionFeatures1799(t *testing.T) {
	h := New(t)
	g, r := speechClients(t, h)
	v1, err := speechv1.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvLocalAI)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speechv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = v1.Close() })
	// Several whisper recognitions: more than h.Context()'s minute.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	spoken, err := os.ReadFile("../../internal/speech/testdata/spoken.wav")
	if err != nil {
		t.Fatal(err)
	}
	german, err := os.ReadFile("../../internal/speech/testdata/german.wav")
	if err != nil {
		t.Fatal(err)
	}
	amrNB, err := os.ReadFile("../../internal/speech/testdata/jfk.amr")
	if err != nil {
		t.Fatal(err)
	}
	amrWB, err := os.ReadFile("../../internal/speech/testdata/jfk.awb")
	if err != nil {
		t.Fatal(err)
	}
	project := h.Project()
	recognizer := func(loc string) string { return "projects/" + project + "/locations/" + loc + "/recognizers/_" }
	auto := &speechv2pb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechv2pb.AutoDetectDecodingConfig{}}
	transcript := func(res *speechv2pb.RecognizeResponse) string {
		var b []string
		for _, x := range res.GetResults() {
			if len(x.GetAlternatives()) > 0 {
				b = append(b, x.GetAlternatives()[0].GetTranscript())
			}
		}
		return strings.Join(b, " ")
	}

	// Profanity filter, spoken punctuation and emojis, gRPC then REST.
	plain, err := g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("global"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: spoken},
		Config: &speechv2pb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"}, DecodingConfig: auto}})
	if err != nil {
		t.Fatalf("Recognize without the features: %v", err)
	}
	if text := strings.ToLower(transcript(plain)); !strings.Contains(text, "shit") || !strings.Contains(text, "exclamation") {
		t.Errorf("without the features: %q", text)
	}
	features := &speechv2pb.RecognitionFeatures{ProfanityFilter: true, EnableSpokenPunctuation: true, EnableSpokenEmojis: true, EnableWordTimeOffsets: true}
	for name, c := range map[string]*speech.Client{"gRPC": g, "REST": r} {
		res, err := c.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("global"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: spoken},
			Config: &speechv2pb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"}, DecodingConfig: auto, Features: features}})
		if err != nil {
			t.Fatalf("%s Recognize with the features: %v", name, err)
		}
		text := transcript(res)
		if !strings.Contains(text, "s***") || strings.Contains(strings.ToLower(text), "shit") || !strings.Contains(text, "!") ||
			strings.Contains(strings.ToLower(text), "exclamation") || !strings.Contains(text, "👍") || strings.Contains(strings.ToLower(text), "thumbs") {
			t.Errorf("%s with the features: %q", name, text)
		}
		for _, x := range res.GetResults() {
			for _, w := range x.GetAlternatives()[0].GetWords() {
				if strings.EqualFold(strings.Trim(w.GetWord(), ".,!?"), "shit") {
					t.Errorf("%s: words[] keeps %q", name, w.GetWord())
				}
			}
		}
	}

	// v1: the same features.
	v1res, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Audio: &speechpb.RecognitionAudio{AudioSource: &speechpb.RecognitionAudio_Content{Content: spoken}},
		Config: &speechpb.RecognitionConfig{LanguageCode: "en-US", ProfanityFilter: true,
			EnableSpokenPunctuation: wrapperspb.Bool(true), EnableSpokenEmojis: wrapperspb.Bool(true)}})
	if err != nil {
		t.Fatalf("v1 Recognize with the features: %v", err)
	}
	var v1text string
	for _, x := range v1res.GetResults() {
		v1text += x.GetAlternatives()[0].GetTranscript()
	}
	if !strings.Contains(v1text, "s***") || !strings.Contains(v1text, "!") || !strings.Contains(v1text, "👍") {
		t.Errorf("v1 with the features: %q", v1text)
	}

	// Several languageCodes: German, labelled de-de.
	res, err := r.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("global"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: german},
		Config: &speechv2pb.RecognitionConfig{Model: "long", LanguageCodes: []string{"en-US", "de-DE"}, DecodingConfig: auto}})
	if err != nil {
		t.Fatalf("Recognize with en-US and de-DE: %v", err)
	}
	if text := transcript(res); !strings.Contains(text, "Wetter") || len(res.GetResults()) == 0 || res.GetResults()[0].GetLanguageCode() != "de-de" {
		t.Errorf("en-US and de-DE: %q %v", text, res.GetResults())
	}
	for name, req := range map[string]*speechv2pb.RecognizeRequest{
		"four codes": {Recognizer: recognizer("global"), Config: &speechv2pb.RecognitionConfig{Model: "long",
			LanguageCodes: []string{"en-US", "de-DE", "fr-FR", "es-ES"}, DecodingConfig: auto}},
		"chirp": {Recognizer: recognizer("global"), Config: &speechv2pb.RecognitionConfig{Model: "chirp",
			LanguageCodes: []string{"en-US", "de-DE"}, DecodingConfig: auto}},
		"us-central1": {Recognizer: recognizer("us-central1"), Config: &speechv2pb.RecognitionConfig{Model: "long",
			LanguageCodes: []string{"en-US", "de-DE"}, DecodingConfig: auto}},
		"translation on chirp": {Recognizer: recognizer("us-central1"), Config: &speechv2pb.RecognitionConfig{Model: "chirp",
			LanguageCodes: []string{"de-DE"}, DecodingConfig: auto, TranslationConfig: &speechv2pb.TranslationConfig{TargetLanguage: "en-US"}}},
		"unlisted pair": {Recognizer: recognizer("us-central1"), Config: &speechv2pb.RecognitionConfig{Model: "chirp_2",
			LanguageCodes: []string{"de-DE"}, DecodingConfig: auto, TranslationConfig: &speechv2pb.TranslationConfig{TargetLanguage: "fr-FR"}}},
	} {
		req.AudioSource = &speechv2pb.RecognizeRequest_Content{Content: german}
		if _, err := g.Recognize(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}

	// Translation, de-DE to en-US, with the local generation model.
	res, err = g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("us-central1"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: german},
		Config: &speechv2pb.RecognitionConfig{Model: "chirp_2", LanguageCodes: []string{"de-DE"}, DecodingConfig: auto,
			TranslationConfig: &speechv2pb.TranslationConfig{TargetLanguage: "en-US"}}})
	switch {
	case status.Code(err) == codes.Unimplemented && strings.Contains(err.Error(), "no local generation model"):
		t.Logf("translationConfig: this instance has no local generation model: %v", err)
	case err != nil:
		t.Errorf("Recognize with translationConfig: %v", err)
	default:
		text := strings.ToLower(transcript(res))
		if !strings.Contains(text, "weather") || strings.Contains(text, "wetter") || res.GetResults()[0].GetAlternatives()[0].GetConfidence() != 0 {
			t.Errorf("translated: %q %v", text, res.GetResults())
		}
	}

	// AMR and AMR-WB.
	for name, c := range map[string]struct {
		data []byte
		dec  *speechv2pb.RecognitionConfig
	}{
		"AMR auto":        {amrNB, &speechv2pb.RecognitionConfig{DecodingConfig: auto}},
		"AMR_WB auto":     {amrWB, &speechv2pb.RecognitionConfig{DecodingConfig: auto}},
		"AMR explicit":    {amrNB, &speechv2pb.RecognitionConfig{DecodingConfig: &speechv2pb.RecognitionConfig_ExplicitDecodingConfig{ExplicitDecodingConfig: &speechv2pb.ExplicitDecodingConfig{Encoding: speechv2pb.ExplicitDecodingConfig_AMR, SampleRateHertz: 8000, AudioChannelCount: 1}}}},
		"AMR_WB explicit": {amrWB, &speechv2pb.RecognitionConfig{DecodingConfig: &speechv2pb.RecognitionConfig_ExplicitDecodingConfig{ExplicitDecodingConfig: &speechv2pb.ExplicitDecodingConfig{Encoding: speechv2pb.ExplicitDecodingConfig_AMR_WB, SampleRateHertz: 16000, AudioChannelCount: 1}}}},
	} {
		c.dec.Model, c.dec.LanguageCodes = "chirp", []string{"en-US"}
		res, err := r.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("global"), Config: c.dec,
			AudioSource: &speechv2pb.RecognizeRequest_Content{Content: c.data}})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if text := strings.ToLower(transcript(res)); !strings.Contains(text, "what your country can do for you") ||
			res.GetMetadata().GetTotalBilledDuration().GetSeconds() != 11 {
			t.Errorf("%s: %q %v", name, text, res.GetMetadata())
		}
	}
	if _, err := g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("global"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: amrNB},
		Config: &speechv2pb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"}, DecodingConfig: &speechv2pb.RecognitionConfig_ExplicitDecodingConfig{
			ExplicitDecodingConfig: &speechv2pb.ExplicitDecodingConfig{Encoding: speechv2pb.ExplicitDecodingConfig_AMR, SampleRateHertz: 16000, AudioChannelCount: 1}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("AMR at 16000 Hz: %v", err)
	}
	v1amr, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Audio: &speechpb.RecognitionAudio{AudioSource: &speechpb.RecognitionAudio_Content{Content: amrNB}},
		Config: &speechpb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechpb.RecognitionConfig_AMR, SampleRateHertz: 8000}})
	if err != nil || len(v1amr.GetResults()) == 0 || !strings.Contains(strings.ToLower(v1amr.GetResults()[0].GetAlternatives()[0].GetTranscript()), "what your country can do for you") {
		t.Errorf("v1 AMR: %v %v", v1amr, err)
	}

	// denoiseAudio on noisy audio.
	res, err = g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("us-central1"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: noisyJFK(t)},
		Config: &speechv2pb.RecognitionConfig{Model: "chirp_2", LanguageCodes: []string{"en-US"}, DecodingConfig: auto,
			DenoiserConfig: &speechv2pb.DenoiserConfig{DenoiseAudio: true}}})
	if err != nil {
		t.Errorf("Recognize with denoiseAudio: %v", err)
	} else if text := strings.ToLower(transcript(res)); !strings.Contains(text, "what your country can do for you") {
		t.Errorf("denoised: %q", text)
	}

	// snrThreshold: a low one keeps the speech, the documented top one
	// silences it all.
	for _, snr := range []float32{1, 1000} {
		res, err = g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("us-central1"), AudioSource: &speechv2pb.RecognizeRequest_Content{Content: noisyJFK(t)},
			Config: &speechv2pb.RecognitionConfig{Model: "chirp_2", LanguageCodes: []string{"en-US"}, DecodingConfig: auto,
				DenoiserConfig: &speechv2pb.DenoiserConfig{DenoiseAudio: true, SnrThreshold: snr}}})
		if err != nil {
			t.Errorf("Recognize with snrThreshold %v: %v", snr, err)
			continue
		}
		text := strings.ToLower(transcript(res))
		t.Logf("snrThreshold %v (recorded): %q", snr, text)
		if heard := strings.Contains(text, "your country"); heard != (snr == 1) {
			t.Errorf("snrThreshold %v: %q", snr, text)
		}
	}

	// Diarization: not a feature of chirp_2; chirp_3's not served.
	for model, want := range map[string]codes.Code{"chirp_2": codes.InvalidArgument, "chirp_3": codes.Unimplemented} {
		cfg := &speechv2pb.RecognitionConfig{Model: model, LanguageCodes: []string{"en-US"}, DecodingConfig: auto,
			Features: &speechv2pb.RecognitionFeatures{DiarizationConfig: &speechv2pb.SpeakerDiarizationConfig{}}}
		if _, err := g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: recognizer("us-central1"), Config: cfg,
			AudioSource: &speechv2pb.RecognizeRequest_Content{Content: spoken}}); status.Code(err) != want {
			t.Errorf("diarization on %s: %v, want %v", model, err, want)
		}
	}
}

// noisyJFK is JFK's sample (16 kHz LINEAR16 WAV) with seeded white noise
// added, as a WAV.
func noisyJFK(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../internal/speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), b...)
	at := bytes.Index(out, []byte("data"))
	if at < 0 {
		t.Fatal("jfk.wav has no data chunk")
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := at + 8; i+1 < len(out); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(out[i:]))) + r.NormFloat64()*1000
		binary.LittleEndian.PutUint16(out[i:], uint16(int16(max(-32768, min(32767, v)))))
	}
	return out
}
