package vertexai

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// pcm is LINEAR16 at 16 kHz: a 440 Hz tone for each true, silence for
// each false, each part dur long.
func pcm(dur time.Duration, parts ...bool) []byte {
	n := int(dur.Seconds() * speech.SampleRate)
	out := make([]byte, 0, 2*n*len(parts))
	for _, voiced := range parts {
		for i := 0; i < n; i++ {
			var v int16
			if voiced {
				v = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/speech.SampleRate))
			}
			out = binary.LittleEndian.AppendUint16(out, uint16(v))
		}
	}
	return out
}

// fakeTranscribe answers "speech N.Ns" for audio of N.N seconds.
func fakeTranscribe(t *testing.T) *int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	old := streamTranscribe
	streamTranscribe = func(_ context.Context, s []float32, _ speech.Options) (speech.Result, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		d := samplesDur(len(s))
		return speech.Result{Language: "en", Segments: []speech.Segment{{Text: "speech " + strings.TrimSuffix(d.String(), "s") + "s",
			End: d, Confidence: 0.5, Words: []speech.Word{{Text: "speech", End: d, Confidence: 0.5}}}}}, nil
	}
	t.Cleanup(func() { streamTranscribe = old })
	return &calls
}

func linear16Settings() streamSettings {
	return streamSettings{cfg: speechconfig.Config{Decoding: speechconfig.Decoding{Encoding: "LINEAR16", SampleRate: 16000, Channels: 1},
		LanguageCode: "en-us"}, endSilence: endSilenceFor("")}
}

// chunks splits b into requests of at most n bytes.
func chunks(b []byte, n int) func() ([]byte, error) {
	var mu sync.Mutex
	return func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(b) == 0 {
			return nil, io.EOF
		}
		k := min(n, len(b))
		out := b[:k]
		b = b[k:]
		return out, nil
	}
}

func collectStream(t *testing.T, st streamSettings, recv func() ([]byte, error)) ([]streamEvent, error) {
	t.Helper()
	var evs []streamEvent
	err := runStream(context.Background(), st, recv, func(ev streamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	return evs, err
}

// TestStreamEndpointing: two utterances parted by a second of silence are
// two final results, with voice activity events at their offsets.
func TestStreamEndpointing(t *testing.T) {
	fakeTranscribe(t)
	st := linear16Settings()
	st.voiceEvents = true
	audio := pcm(time.Second, false, true, true, false, true, false)
	evs, err := collectStream(t, st, chunks(audio, 8000))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []streamEventKind
	var finals []streamEvent
	for _, ev := range evs {
		kinds = append(kinds, ev.kind)
		if ev.kind == evResult {
			if !ev.final {
				t.Errorf("interim result without interim_results: %+v", ev)
			}
			finals = append(finals, ev)
		}
	}
	want := []streamEventKind{evSpeechBegin, evSpeechEnd, evResult, evSpeechBegin, evSpeechEnd, evResult}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v, want %v", kinds, want)
		}
	}
	if evs[0].offset != time.Second || evs[1].offset != 3*time.Second || evs[3].offset != 4*time.Second {
		t.Errorf("event offsets %v %v %v", evs[0].offset, evs[1].offset, evs[3].offset)
	}
	// The second utterance's word times are in the stream, not the piece.
	if w := finals[1].segs[0].Words[0]; w.End <= 4*time.Second || finals[1].language != "en-us" {
		t.Errorf("second final %+v", finals[1])
	}
}

// TestStreamInterimAndSingleUtterance: interim results while speech goes
// on; single_utterance ends the stream at the first utterance's end.
func TestStreamInterimAndSingleUtterance(t *testing.T) {
	fakeTranscribe(t)
	st := linear16Settings()
	st.interim, st.singleUtterance = true, true
	audio := pcm(time.Second, true, true, true, true, true, false, true, true)
	// Send slowly enough for interims to be due between reads.
	next := chunks(audio, 3200)
	recv := func() ([]byte, error) {
		time.Sleep(time.Millisecond)
		return next()
	}
	evs, err := collectStream(t, st, recv)
	if err != nil {
		t.Fatal(err)
	}
	var interims, finals, eou int
	for _, ev := range evs {
		switch {
		case ev.kind == evResult && ev.final:
			finals++
			if ev.offset > 6*time.Second {
				t.Errorf("final after the first utterance: %+v", ev)
			}
		case ev.kind == evResult:
			interims++
		case ev.kind == evEndOfUtterance:
			eou++
			if finals != 0 {
				t.Error("END_OF_SINGLE_UTTERANCE after the final result")
			}
		}
	}
	if interims == 0 || finals != 1 || eou != 1 {
		t.Errorf("interims %d finals %d END_OF_SINGLE_UTTERANCE %d: %+v", interims, finals, eou, evs)
	}
}

// TestStreamLimits: a chunk over 25 KB is INVALID_ARGUMENT, audio over five
// minutes OUT_OF_RANGE, and speech_start_timeout closes a silent stream.
func TestStreamLimits(t *testing.T) {
	fakeTranscribe(t)
	st := linear16Settings()
	if _, err := collectStream(t, st, chunks(pcm(time.Second, true), streamMaxChunk+2)); code(grpcError(err)) != codes.InvalidArgument {
		t.Errorf("oversized chunk: %v", err)
	}
	long := pcm(time.Minute, false, false, false, false, false, false)
	if _, err := collectStream(t, st, chunks(long, streamMaxChunk)); code(grpcError(err)) != codes.OutOfRange ||
		!strings.Contains(err.Error(), "stream duration") {
		t.Errorf("six minutes: %v", err)
	}
	st.voiceEvents, st.startTimeout = true, 2*time.Second
	evs, err := collectStream(t, st, chunks(pcm(time.Second, false, false, false, true, true), 3200))
	if err != nil || len(evs) != 0 {
		t.Errorf("speech_start_timeout: %v %+v", err, evs)
	}
}

// TestSpeechStreamingRecognizeV2Settings: the first request's checks.
func TestSpeechStreamingRecognizeV2Settings(t *testing.T) {
	v := newSpeech(t, "")
	ctx := context.Background()
	cfg := &speechpb.RecognitionConfig{Model: "short", LanguageCodes: []string{"auto"},
		DecodingConfig: &speechpb.RecognitionConfig_ExplicitDecodingConfig{ExplicitDecodingConfig: &speechpb.ExplicitDecodingConfig{
			Encoding: speechpb.ExplicitDecodingConfig_LINEAR16, SampleRateHertz: 16000, AudioChannelCount: 1}}}
	rec := speechParent + "/recognizers/_"
	for _, c := range []struct {
		name string
		req  *speechpb.StreamingRecognizeRequest
		want codes.Code
	}{
		{"no config", &speechpb.StreamingRecognizeRequest{Recognizer: rec}, codes.InvalidArgument},
		{"audio first", &speechpb.StreamingRecognizeRequest{Recognizer: rec, StreamingRequest: &speechpb.StreamingRecognizeRequest_Audio{Audio: []byte{1}}}, codes.InvalidArgument},
		{"bad recognizer", &speechpb.StreamingRecognizeRequest{Recognizer: "nope", StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: cfg}}}, codes.InvalidArgument},
		{"timeout without events", &speechpb.StreamingRecognizeRequest{Recognizer: rec, StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: cfg, StreamingFeatures: &speechpb.StreamingRecognitionFeatures{
				VoiceActivityTimeout: &speechpb.StreamingRecognitionFeatures_VoiceActivityTimeout{}}}}}, codes.InvalidArgument},
		{"ok", &speechpb.StreamingRecognizeRequest{Recognizer: rec, StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: cfg, StreamingFeatures: &speechpb.StreamingRecognitionFeatures{
				InterimResults: true, EndpointingSensitivity: speechpb.StreamingRecognitionFeatures_ENDPOINTING_SENSITIVITY_SHORT}}}}, codes.OK},
	} {
		st, err := v.s.streamSettingsV2(ctx, c.req)
		if code(grpcError(err)) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		if c.want == codes.OK && (!st.interim || st.endSilence != 500*time.Millisecond) {
			t.Errorf("%s: settings %+v", c.name, st)
		}
	}
}

// TestSpeechConfigMask: config_mask's three cases over a recognizer.
func TestSpeechConfigMask(t *testing.T) {
	v := newSpeech(t, "")
	ctx := context.Background()
	base := &speechpb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"},
		Features:       &speechpb.RecognitionFeatures{EnableWordTimeOffsets: true},
		DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}
	if _, err := v.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: speechParent, RecognizerId: "masked",
		Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: base}}); err != nil {
		t.Fatal(err)
	}
	name := speechParent + "/recognizers/masked"
	over := json.RawMessage(`{"model":"short","languageCodes":["de-DE"]}`)
	read := func(mask ...string) map[string]any {
		t.Helper()
		b, err := v.s.effectiveConfig(name, over, mask)
		if err != nil {
			t.Fatalf("mask %v: %v", mask, err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	if m := read(); m["model"] != "short" || m["features"] == nil {
		t.Errorf("no mask: %v", m)
	}
	if m := read("language_codes"); m["model"] != "chirp" || m["languageCodes"].([]any)[0] != "de-DE" {
		t.Errorf("language_codes: %v", m)
	}
	if m := read("features.enable_word_time_offsets"); m["model"] != "chirp" || len(m["features"].(map[string]any)) != 0 {
		t.Errorf("features.enable_word_time_offsets: %v", m)
	}
	if m := read("*"); m["features"] != nil || m["model"] != "short" {
		t.Errorf("*: %v", m)
	}
	if _, err := v.s.effectiveConfig(name, over, []string{"features.nope"}); code(grpcError(err)) != codes.InvalidArgument {
		t.Errorf("unknown path: %v", err)
	}
	_ = fieldmaskpb.FieldMask{}
}

// TestSpeechAdaptationOptions: phrase sets inline and stored, custom
// classes expanded, boosts and limits.
func TestSpeechAdaptationOptions(t *testing.T) {
	v := newSpeech(t, "")
	ctx := context.Background()
	if _, err := v.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "cities",
		CustomClass: &speechpb.CustomClass{Items: []*speechpb.CustomClass_ClassItem{{Value: "Kildrummy"}, {Value: "Ballater"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: speechParent, PhraseSetId: "trains",
		PhraseSet: &speechpb.PhraseSet{Boost: 4, Phrases: []*speechpb.PhraseSet_Phrase{{Value: "the ${" + speechParent + "/customClasses/cities} station"}}}}); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"phraseSets":[{"phraseSet":"` + speechParent + `/phraseSets/trains"},
		{"inlinePhraseSet":{"phrases":[{"value":"Nyhuis","boost":10},{"value":"${names} said"}]}}],
		"customClasses":[{"name":"names","items":[{"value":"Siobhan"}]}]}`)
	phrases, prompt, err := v.s.adaptationOptions(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, p := range phrases {
		got[p.Text] = p.Bias
	}
	want := map[string]float64{"the Kildrummy station": 4 * biasPerBoost, "the Ballater station": 4 * biasPerBoost,
		"Nyhuis": 10 * biasPerBoost, "Siobhan said": defaultBoost * biasPerBoost}
	for k, w := range want {
		if math.Abs(got[k]-w) > 1e-6 {
			t.Errorf("phrase %q bias %v, want %v (all %v)", k, got[k], w, got)
		}
	}
	if phrases[0].Text != "Nyhuis" || !strings.HasSuffix(prompt, "Nyhuis") {
		t.Errorf("highest boost first in phrases, last in the prompt: %v %q", phrases, prompt)
	}
	for _, c := range []struct {
		name string
		raw  string
		want codes.Code
	}{
		{"missing set", `{"phraseSets":[{"phraseSet":"` + speechParent + `/phraseSets/none"}]}`, codes.InvalidArgument},
		{"missing class", `{"phraseSets":[{"inlinePhraseSet":{"phrases":[{"value":"${nope}"}]}}]}`, codes.InvalidArgument},
		{"prebuilt class", `{"phraseSets":[{"inlinePhraseSet":{"phrases":[{"value":"$ADDRESSNUM"}]}}]}`, codes.Unimplemented},
		{"negative boost", `{"phraseSets":[{"inlinePhraseSet":{"phrases":[{"value":"a","boost":-1}]}}]}`, codes.InvalidArgument},
		{"long phrase", `{"phraseSets":[{"inlinePhraseSet":{"phrases":[{"value":"` + strings.Repeat("a", 101) + `"}]}}]}`, codes.InvalidArgument},
		{"empty", `{"phraseSets":[{}]}`, codes.InvalidArgument},
	} {
		if _, _, err := v.s.adaptationOptions(json.RawMessage(c.raw)); code(grpcError(err)) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	var many []string
	for i := 0; i < adaptMaxSets+1; i++ {
		many = append(many, `{"inlinePhraseSet":{"phrases":[{"value":"a"}]}}`)
	}
	if _, _, err := v.s.adaptationOptions(json.RawMessage(`{"phraseSets":[` + strings.Join(many, ",") + `]}`)); code(grpcError(err)) != codes.InvalidArgument {
		t.Errorf("21 phrase sets: %v", err)
	}
}

// TestSpeechV1Config: v1's RecognitionConfig read as v2's.
func TestSpeechV1Config(t *testing.T) {
	v := newSpeech(t, "")
	cfg, opt, err := v.s.v1Checked(&speechv1pb.RecognitionConfig{Encoding: speechv1pb.RecognitionConfig_LINEAR16, SampleRateHertz: 8000,
		LanguageCode: "en-US", Model: "phone_call", EnableWordTimeOffsets: true,
		SpeechContexts: []*speechv1pb.SpeechContext{{Phrases: []string{"Nyhuis"}, Boost: 15}}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "telephony" || cfg.Decoding.SampleRate != 8000 || !cfg.WordOffsets || cfg.Language != "en" ||
		len(opt.Phrases) != 1 || opt.Phrases[0].Bias != 15*biasPerBoost {
		t.Errorf("config %+v options %+v", cfg, opt)
	}
	for _, c := range []struct {
		name string
		cfg  *speechv1pb.RecognitionConfig
		want codes.Code
	}{
		{"no language", &speechv1pb.RecognitionConfig{}, codes.InvalidArgument},
		{"amr", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_AMR}, codes.Unimplemented},
		{"opus rate", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_OGG_OPUS, SampleRateHertz: 44100}, codes.InvalidArgument},
		{"medical", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Model: "medical_dictation"}, codes.Unimplemented},
		{"alternatives", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", AlternativeLanguageCodes: []string{"de-DE"}}, codes.Unimplemented},
		{"references", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Adaptation: &speechv1pb.SpeechAdaptation{PhraseSetReferences: []string{"x"}}}, codes.Unimplemented},
	} {
		if _, _, err := v.s.v1Checked(c.cfg); code(grpcError(err)) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}
