//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/binary"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	speechv1 "cloud.google.com/go/speech/apiv1"
	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// wavPCM is a canonical 16 kHz mono LINEAR16 WAV file's data chunk.
func wavPCM(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:]))
		if id == "data" {
			return b[i+8 : min(i+8+n, len(b))]
		}
		i += 8 + n + n%2
	}
	t.Fatalf("%s has no data chunk", path)
	return nil
}

func linear16V2() *speechpb.RecognitionConfig {
	return &speechpb.RecognitionConfig{Model: "long", LanguageCodes: []string{"en-US"},
		DecodingConfig: &speechpb.RecognitionConfig_ExplicitDecodingConfig{ExplicitDecodingConfig: &speechpb.ExplicitDecodingConfig{
			Encoding: speechpb.ExplicitDecodingConfig_LINEAR16, SampleRateHertz: 16000, AudioChannelCount: 1}}}
}

// TestSpeechAdaptationBoost (#1930): a RecognitionConfig's adaptation biases
// local whisper.cpp. The audio (internal/speech/testdata/adaptation.wav,
// generated with text-to-speech: "We met Siobhan Nyhuis at the Kildrummy
// station.") has two rare names whisper misspells unaided; a stored phrase
// set boosting ${class} items, the class holding Nyhuis, makes it spell
// Nyhuis, while Kildrummy, not boosted, stays misspelt. Through the
// official speech apiv2 client over gRPC.
//
// covers: google.cloud.speech.v2.Speech/Recognize
func TestSpeechAdaptationBoost(t *testing.T) {
	h := New(t)
	g, _ := speechClients(t, h)
	// whisper.cpp takes about 25 s a transcription: longer than h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	parent := "projects/" + h.Project() + "/locations/us"
	wav, err := os.ReadFile("../../internal/speech/testdata/adaptation.wav")
	if err != nil {
		t.Fatal(err)
	}
	cop, err := g.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: parent, CustomClassId: "surnames",
		CustomClass: &speechpb.CustomClass{Items: []*speechpb.CustomClass_ClassItem{{Value: "Nyhuis"}}}})
	if err != nil {
		t.Fatalf("CreateCustomClass: %v", err)
	}
	cc, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateCustomClass wait: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.DeleteCustomClass(context.Background(), &speechpb.DeleteCustomClassRequest{Name: cc.GetName(), AllowMissing: true})
	})
	pop, err := g.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "boost-names",
		PhraseSet: &speechpb.PhraseSet{Boost: 10, Phrases: []*speechpb.PhraseSet_Phrase{{Value: "${" + cc.GetName() + "}"}}}})
	if err != nil {
		t.Fatalf("CreatePhraseSet: %v", err)
	}
	ps, err := pop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePhraseSet wait: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.DeletePhraseSet(context.Background(), &speechpb.DeletePhraseSetRequest{Name: ps.GetName(), AllowMissing: true})
	})

	transcript := func(a *speechpb.SpeechAdaptation) string {
		t.Helper()
		cfg := &speechpb.RecognitionConfig{Model: "chirp_3", LanguageCodes: []string{"en-US"}, Adaptation: a,
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}
		res, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: parent + "/recognizers/_", Config: cfg,
			AudioSource: &speechpb.RecognizeRequest_Content{Content: wav}})
		if err != nil {
			t.Fatalf("Recognize (adaptation %v): %v", a, err)
		}
		var out []string
		for _, r := range res.GetResults() {
			out = append(out, r.GetAlternatives()[0].GetTranscript())
		}
		return strings.Join(out, " ")
	}
	plain := transcript(nil)
	if strings.Contains(plain, "Nyhuis") || strings.Contains(plain, "Kildrummy") {
		t.Fatalf("unadapted transcript %q already spells the rare names", plain)
	}
	boosted := transcript(&speechpb.SpeechAdaptation{PhraseSets: []*speechpb.SpeechAdaptation_AdaptationPhraseSet{{
		Value: &speechpb.SpeechAdaptation_AdaptationPhraseSet_PhraseSet{PhraseSet: ps.GetName()}}}})
	if !strings.Contains(boosted, "Nyhuis") || strings.Contains(boosted, "Kildrummy") {
		t.Errorf("boosted transcript %q: want Nyhuis (boosted) and not Kildrummy (not boosted)", boosted)
	}
	// A prebuilt class has no local grammar.
	if _, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: parent + "/recognizers/_", Config: &speechpb.RecognitionConfig{
		Model: "chirp_3", LanguageCodes: []string{"en-US"},
		DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}},
		Adaptation: &speechpb.SpeechAdaptation{PhraseSets: []*speechpb.SpeechAdaptation_AdaptationPhraseSet{{
			Value: &speechpb.SpeechAdaptation_AdaptationPhraseSet_InlinePhraseSet{InlinePhraseSet: &speechpb.PhraseSet{
				Phrases: []*speechpb.PhraseSet_Phrase{{Value: "$ADDRESSNUM"}}}}}}}},
		AudioSource: &speechpb.RecognizeRequest_Content{Content: wav}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("prebuilt class: %v", err)
	}
}

// TestSpeechStreamingRecognize (#1930): v2's StreamingRecognize through the
// official client: whisper.cpp's JFK sample as LINEAR16 in 25 KB requests,
// with interim results and voice activity events; the final results read
// the sentence, offsets increase, and SPEECH_ACTIVITY_BEGIN comes first. A
// 26 KB request is INVALID_ARGUMENT.
//
// covers: google.cloud.speech.v2.Speech/StreamingRecognize
func TestSpeechStreamingRecognize(t *testing.T) {
	h := New(t)
	g, _ := speechClients(t, h)
	// whisper.cpp takes about 25 s a transcription: longer than h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rec := "projects/" + h.Project() + "/locations/us/recognizers/_"
	pcm := wavPCM(t, "../../internal/speech/testdata/jfk.wav")
	stream, err := g.StreamingRecognize(ctx)
	if err != nil {
		t.Fatalf("StreamingRecognize: %v", err)
	}
	if err := stream.Send(&speechpb.StreamingRecognizeRequest{Recognizer: rec, StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
		StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: linear16V2(), StreamingFeatures: &speechpb.StreamingRecognitionFeatures{
			InterimResults: true, EnableVoiceActivityEvents: true}}}}); err != nil {
		t.Fatalf("send config: %v", err)
	}
	go func() {
		for b := pcm; len(b) > 0; {
			n := min(25*1024, len(b))
			if stream.Send(&speechpb.StreamingRecognizeRequest{StreamingRequest: &speechpb.StreamingRecognizeRequest_Audio{Audio: b[:n]}}) != nil {
				return
			}
			b = b[n:]
			time.Sleep(200 * time.Millisecond)
		}
		_ = stream.CloseSend()
	}()
	var finals []string
	var interims, begins int
	var last time.Duration
	first := true
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if first && resp.GetSpeechEventType() != speechpb.StreamingRecognizeResponse_SPEECH_ACTIVITY_BEGIN {
			t.Errorf("first response %v, want SPEECH_ACTIVITY_BEGIN", resp)
		}
		first = false
		if resp.GetSpeechEventType() == speechpb.StreamingRecognizeResponse_SPEECH_ACTIVITY_BEGIN {
			begins++
		}
		for _, r := range resp.GetResults() {
			if !r.GetIsFinal() {
				interims++
				continue
			}
			end := r.GetResultEndOffset().AsDuration()
			if end <= last || r.GetLanguageCode() != "en-us" {
				t.Errorf("final result %v after %v", r, last)
			}
			last = end
			finals = append(finals, r.GetAlternatives()[0].GetTranscript())
		}
	}
	text := strings.ToLower(strings.Join(finals, " "))
	if !strings.Contains(text, "ask not what your country can do for you") || begins == 0 || interims == 0 {
		t.Errorf("finals %q, interims %d, SPEECH_ACTIVITY_BEGIN %d", text, interims, begins)
	}

	big, err := g.StreamingRecognize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = big.Send(&speechpb.StreamingRecognizeRequest{Recognizer: rec, StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
		StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: linear16V2()}}})
	_ = big.Send(&speechpb.StreamingRecognizeRequest{StreamingRequest: &speechpb.StreamingRecognizeRequest_Audio{Audio: make([]byte, 26*1024)}})
	_ = big.CloseSend()
	for {
		_, err := big.Recv()
		if err == nil {
			continue
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("26 KB request: %v", err)
		}
		break
	}
}

// TestSpeechV1 (#1930): Speech-to-Text v1 through the official apiv1
// client over gRPC: Recognize with speech_contexts, more than a minute
// refused in Google's words, LongRunningRecognize waited on, and
// StreamingRecognize with single_utterance ending at the first utterance
// with END_OF_SINGLE_UTTERANCE.
//
// covers: google.cloud.speech.v1.Speech/Recognize, google.cloud.speech.v1.Speech/LongRunningRecognize, google.cloud.speech.v1.Speech/StreamingRecognize
func TestSpeechV1(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvLocalAI)
	if addr == "" {
		t.Fatal("no local AI endpoint")
	}
	// whisper.cpp takes about 25 s a transcription: longer than h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := speechv1.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speechv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	adapt := wavPCM(t, "../../internal/speech/testdata/adaptation.wav")
	cfg := &speechv1pb.RecognitionConfig{Encoding: speechv1pb.RecognitionConfig_LINEAR16, SampleRateHertz: 16000, LanguageCode: "en-US",
		EnableWordTimeOffsets: true, SpeechContexts: []*speechv1pb.SpeechContext{{Phrases: []string{"Nyhuis"}, Boost: 10}}}
	res, err := c.Recognize(ctx, &speechv1pb.RecognizeRequest{Config: cfg, Audio: &speechv1pb.RecognitionAudio{
		AudioSource: &speechv1pb.RecognitionAudio_Content{Content: adapt}}})
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if len(res.GetResults()) == 0 || !strings.Contains(res.GetResults()[0].GetAlternatives()[0].GetTranscript(), "Nyhuis") ||
		len(res.GetResults()[0].GetAlternatives()[0].GetWords()) == 0 || res.GetTotalBilledTime().AsDuration() <= 0 {
		t.Errorf("Recognize: %v", res)
	}
	long := bytes.Repeat([]byte{0, 0}, 61*16000)
	if _, err := c.Recognize(ctx, &speechv1pb.RecognizeRequest{Config: cfg, Audio: &speechv1pb.RecognitionAudio{
		AudioSource: &speechv1pb.RecognitionAudio_Content{Content: long}}}); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "Sync input too long") {
		t.Errorf("61 s Recognize: %v", err)
	}
	op, err := c.LongRunningRecognize(ctx, &speechv1pb.LongRunningRecognizeRequest{Config: cfg, Audio: &speechv1pb.RecognitionAudio{
		AudioSource: &speechv1pb.RecognitionAudio_Content{Content: adapt}}})
	if err != nil {
		t.Fatalf("LongRunningRecognize: %v", err)
	}
	lr, err := op.Wait(ctx)
	if err != nil || len(lr.GetResults()) == 0 || !strings.Contains(lr.GetResults()[0].GetAlternatives()[0].GetTranscript(), "Nyhuis") {
		t.Errorf("LongRunningRecognize: %v %v", lr, err)
	}

	// Two utterances parted by two seconds of silence: single_utterance
	// stops after the first.
	pcm := append(append(append([]byte{}, adapt...), make([]byte, 2*2*16000)...), adapt...)
	stream, err := c.StreamingRecognize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&speechv1pb.StreamingRecognizeRequest{StreamingRequest: &speechv1pb.StreamingRecognizeRequest_StreamingConfig{
		// v1's supported languages table has no single utterance for en-US's
		// default model (#2127): latest_long takes it.
		StreamingConfig: &speechv1pb.StreamingRecognitionConfig{Config: v1LatestLong(cfg), SingleUtterance: true}}}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for b := pcm; len(b) > 0; {
			n := min(16000, len(b))
			if stream.Send(&speechv1pb.StreamingRecognizeRequest{StreamingRequest: &speechv1pb.StreamingRecognizeRequest_AudioContent{AudioContent: b[:n]}}) != nil {
				return
			}
			b = b[n:]
		}
		_ = stream.CloseSend()
	}()
	var eou bool
	var finals []string
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("StreamingRecognize Recv: %v", err)
		}
		if resp.GetSpeechEventType() == speechv1pb.StreamingRecognizeResponse_END_OF_SINGLE_UTTERANCE {
			eou = true
		}
		for _, r := range resp.GetResults() {
			if r.GetIsFinal() {
				finals = append(finals, r.GetAlternatives()[0].GetTranscript())
				if r.GetResultEndTime().AsDuration() > 7*time.Second {
					t.Errorf("final result past the first utterance: %v", r)
				}
			}
		}
	}
	if !eou || len(finals) != 1 || !strings.Contains(finals[0], "Nyhuis") {
		t.Errorf("single_utterance: END_OF_SINGLE_UTTERANCE %v, finals %q", eou, finals)
	}
}

// v1LatestLong is a copy of a v1 config on the latest_long model.
func v1LatestLong(c *speechv1pb.RecognitionConfig) *speechv1pb.RecognitionConfig {
	out := proto.Clone(c).(*speechv1pb.RecognitionConfig)
	out.Model = "latest_long"
	return out
}
