//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	speechv1 "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/speech/apiv1/speechpb"
	speech "cloud.google.com/go/speech/apiv2"
	speechv2pb "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestSpeechDiarization2131 (#2131): speaker diarization through the
// official cloud.google.com/go/speech clients (v2 gRPC and REST, v1 gRPC)
// on the local AI endpoint, never Google's, on a two-speaker clip
// (internal/speech/testdata/twospeakers.wav: macOS's Samantha, then Daniel,
// then Samantha, then Daniel, a sentence each):
//
//   - v2 Recognize, chirp_3 in us, an empty diarizationConfig (Chirp 3's
//     page's way): every word has a speakerLabel, Samantha's words one
//     label and Daniel's another; gRPC and REST alike.
//   - v2's minSpeakerCount and maxSpeakerCount are "not currently used"
//     (v2 reference): whatever they are set to (1-1, 3-6, 2-2, even 3-2),
//     the clip has its two speakers.
//   - SEPARATE_RECOGNITION_PER_CHANNEL: a stereo LINEAR16 clip with the
//     clip on both channels has each channel's words labelled, two
//     speakers in each.
//   - v2 BatchRecognize (inline output) labels the words too; on
//     StreamingRecognize it is INVALID_ARGUMENT (Chirp 3 diarizes "only in
//     BatchRecognize and Recognize").
//   - v1 Recognize, latest_long, enable_speaker_diarization: one more, last
//     result holds every word with its speaker_tag and speaker_label, two
//     speakers (v1's default min 2, max 6); v1 does use min_speaker_count
//     and max_speaker_count: 1-1 gives one speaker, 3-6 three.
//   - v1 StreamingRecognize: each final result's alternative holds every
//     word from the start of the audio ("we send all the words from the
//     beginning of the audio for the top alternative in every consecutive
//     STREAMING responses"), the last one the clip's two speakers.
//
// Only the `-tags onnx` build runs the speaker model (CLOUDBURROW_TEST_ONNX);
// against the default build every diarization request is UNIMPLEMENTED,
// with the reason.
//
// covers: google.cloud.speech.v2.Speech/Recognize, google.cloud.speech.v2.Speech/BatchRecognize, google.cloud.speech.v1.Speech/Recognize, google.cloud.speech.v1.Speech/StreamingRecognize
func TestSpeechDiarization2131(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	g, r := speechClients(t, h)
	v1, err := speechv1.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvLocalAI)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speechv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = v1.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	clip, err := os.ReadFile("../../internal/speech/testdata/twospeakers.wav")
	if err != nil {
		t.Fatal(err)
	}
	rec := "projects/" + h.Project() + "/locations/us/recognizers/_"
	auto := &speechv2pb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechv2pb.AutoDetectDecodingConfig{}}
	cfg := func(dc *speechv2pb.SpeakerDiarizationConfig) *speechv2pb.RecognitionConfig {
		return &speechv2pb.RecognitionConfig{Model: "chirp_3", LanguageCodes: []string{"en-US"}, DecodingConfig: auto,
			Features: &speechv2pb.RecognitionFeatures{DiarizationConfig: dc}}
	}
	if os.Getenv(EnvONNX) == "" {
		_, err := g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: rec, Config: cfg(&speechv2pb.SpeakerDiarizationConfig{}),
			AudioSource: &speechv2pb.RecognizeRequest_Content{Content: clip}})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "-tags onnx") {
			t.Errorf("diarization without the onnx build: %v", err)
		}
		_, err = v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: &speechpb.RecognitionConfig{LanguageCode: "en-US", Model: "latest_long",
			DiarizationConfig: &speechpb.SpeakerDiarizationConfig{EnableSpeakerDiarization: true}},
			Audio: &speechpb.RecognitionAudio{AudioSource: &speechpb.RecognitionAudio_Content{Content: clip}}})
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("v1 diarization without the onnx build: %v", err)
		}
		return
	}

	// labelled is a word, lower case without punctuation, and its speaker.
	type labelled struct{ word, label string }
	v2Words := func(res *speechv2pb.RecognizeResponse) []labelled {
		var out []labelled
		for _, x := range res.GetResults() {
			if len(x.GetAlternatives()) == 0 {
				continue
			}
			for _, w := range x.GetAlternatives()[0].GetWords() {
				out = append(out, labelled{bare(w.GetWord()), w.GetSpeakerLabel()})
			}
		}
		return out
	}
	speakers := func(ws []labelled) map[string]bool {
		set := map[string]bool{}
		for _, w := range ws {
			set[w.label] = true
		}
		return set
	}
	// checkTurns: Samantha's words ("morning", "sorry") share a label, and
	// Daniel's ("internet", "monday") another.
	checkTurns := func(name string, ws []labelled) {
		t.Helper()
		of := map[string]string{}
		for _, w := range ws {
			if w.label == "" {
				t.Errorf("%s: %q has no speakerLabel", name, w.word)
			}
			if _, ok := of[w.word]; !ok {
				of[w.word] = w.label
			}
		}
		for _, k := range []string{"morning", "sorry", "internet", "monday"} {
			if of[k] == "" {
				t.Fatalf("%s: no word %q in %v", name, k, ws)
			}
		}
		if n := len(speakers(ws)); n != 2 || of["morning"] != of["sorry"] || of["internet"] != of["monday"] || of["morning"] == of["internet"] {
			t.Errorf("%s: %d speakers, morning %s sorry %s internet %s monday %s: %v", name, n,
				of["morning"], of["sorry"], of["internet"], of["monday"], ws)
		}
	}

	for name, c := range map[string]*speech.Client{"gRPC": g, "REST": r} {
		res, err := c.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: rec, Config: cfg(&speechv2pb.SpeakerDiarizationConfig{}),
			AudioSource: &speechv2pb.RecognizeRequest_Content{Content: clip}})
		if err != nil {
			t.Fatalf("%s Recognize with diarization: %v", name, err)
		}
		checkTurns(name, v2Words(res))
	}

	// minSpeakerCount and maxSpeakerCount: "not currently used" on v2.
	for _, c := range []struct{ min, max int32 }{{1, 1}, {3, 6}, {2, 2}, {3, 2}} {
		res, err := g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: rec,
			Config:      cfg(&speechv2pb.SpeakerDiarizationConfig{MinSpeakerCount: c.min, MaxSpeakerCount: c.max}),
			AudioSource: &speechv2pb.RecognizeRequest_Content{Content: clip}})
		if err != nil {
			t.Fatalf("Recognize with speakers %d-%d: %v", c.min, c.max, err)
		}
		checkTurns(fmt.Sprintf("v2 speakers %d-%d", c.min, c.max), v2Words(res))
	}

	// SEPARATE_RECOGNITION_PER_CHANNEL: the clip on both channels.
	mono := wavPCM(t, "../../internal/speech/testdata/twospeakers.wav")
	stereo := make([]byte, 0, 2*len(mono))
	for i := 0; i+1 < len(mono); i += 2 {
		stereo = append(stereo, mono[i], mono[i+1], mono[i], mono[i+1])
	}
	pc := cfg(&speechv2pb.SpeakerDiarizationConfig{})
	pc.DecodingConfig = &speechv2pb.RecognitionConfig_ExplicitDecodingConfig{ExplicitDecodingConfig: &speechv2pb.ExplicitDecodingConfig{
		Encoding: speechv2pb.ExplicitDecodingConfig_LINEAR16, SampleRateHertz: 16000, AudioChannelCount: 2}}
	pc.Features.MultiChannelMode = speechv2pb.RecognitionFeatures_SEPARATE_RECOGNITION_PER_CHANNEL
	pres, err := g.Recognize(ctx, &speechv2pb.RecognizeRequest{Recognizer: rec, Config: pc,
		AudioSource: &speechv2pb.RecognizeRequest_Content{Content: stereo}})
	if err != nil {
		t.Fatalf("Recognize per channel with diarization: %v", err)
	}
	for ch := int32(1); ch <= 2; ch++ {
		var own []*speechv2pb.SpeechRecognitionResult
		for _, x := range pres.GetResults() {
			if x.GetChannelTag() == ch {
				own = append(own, x)
			}
		}
		checkTurns(fmt.Sprintf("channel %d", ch), v2Words(&speechv2pb.RecognizeResponse{Results: own}))
	}

	// BatchRecognize, inline: the clip's words labelled the same way.
	if h.Endpoint(EnvStorage) != "" {
		sc := storageClient(t, h)
		bh := bucket(t, h, sc)
		w := bh.Object("audio/twospeakers.wav").NewWriter(ctx)
		w.ContentType = "audio/wav"
		if _, err := w.Write(clip); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload: %v", err)
		}
		uri := "gs://" + bh.BucketName() + "/audio/twospeakers.wav"
		op, err := g.BatchRecognize(ctx, &speechv2pb.BatchRecognizeRequest{Recognizer: rec, Config: cfg(&speechv2pb.SpeakerDiarizationConfig{}),
			Files: []*speechv2pb.BatchRecognizeFileMetadata{{AudioSource: &speechv2pb.BatchRecognizeFileMetadata_Uri{Uri: uri}}},
			RecognitionOutputConfig: &speechv2pb.RecognitionOutputConfig{Output: &speechv2pb.RecognitionOutputConfig_InlineResponseConfig{
				InlineResponseConfig: &speechv2pb.InlineOutputConfig{}}}})
		if err != nil {
			t.Fatalf("BatchRecognize with diarization: %v", err)
		}
		res, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("BatchRecognize wait: %v", err)
		}
		tr := res.GetResults()[uri].GetInlineResult().GetTranscript()
		checkTurns("BatchRecognize", v2Words(&speechv2pb.RecognizeResponse{Results: tr.GetResults()}))
	}

	// StreamingRecognize: Chirp 3 does not diarize there.
	stream, err := g.StreamingRecognize(ctx)
	if err != nil {
		t.Fatalf("StreamingRecognize: %v", err)
	}
	if err := stream.Send(&speechv2pb.StreamingRecognizeRequest{Recognizer: rec, StreamingRequest: &speechv2pb.StreamingRecognizeRequest_StreamingConfig{
		StreamingConfig: &speechv2pb.StreamingRecognitionConfig{Config: cfg(&speechv2pb.SpeakerDiarizationConfig{})}}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = stream.CloseSend()
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("StreamingRecognize with diarization: %v", err)
	}

	// v1: the last result holds every word, tagged.
	v1Cfg := func(dc *speechpb.SpeakerDiarizationConfig) *speechpb.RecognitionConfig {
		dc.EnableSpeakerDiarization = true
		return &speechpb.RecognitionConfig{LanguageCode: "en-US", Model: "latest_long", EnableWordTimeOffsets: true, DiarizationConfig: dc}
	}
	v1Last := func(dc *speechpb.SpeakerDiarizationConfig) ([]labelled, int) {
		t.Helper()
		res, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: v1Cfg(dc),
			Audio: &speechpb.RecognitionAudio{AudioSource: &speechpb.RecognitionAudio_Content{Content: clip}}})
		if err != nil {
			t.Fatalf("v1 Recognize with diarization: %v", err)
		}
		results := res.GetResults()
		if len(results) < 2 {
			t.Fatalf("v1: %d results", len(results))
		}
		var all []labelled
		for _, w := range results[len(results)-1].GetAlternatives()[0].GetWords() {
			if w.GetSpeakerTag() < 1 || w.GetSpeakerLabel() == "" || w.GetStartTime() == nil {
				t.Errorf("v1 word %+v", w)
			}
			all = append(all, labelled{bare(w.GetWord()), w.GetSpeakerLabel()})
		}
		var earlier int
		for _, x := range results[:len(results)-1] {
			for _, w := range x.GetAlternatives()[0].GetWords() {
				earlier++
				if w.GetSpeakerTag() != 0 {
					t.Errorf("v1: an earlier result's word %q is tagged", w.GetWord())
				}
			}
		}
		if earlier != len(all) {
			t.Errorf("v1: the last result has %d words, the others %d", len(all), earlier)
		}
		return all, len(speakers(all))
	}
	all, _ := v1Last(&speechpb.SpeakerDiarizationConfig{})
	checkTurns("v1", all)
	// v1 does use its counts.
	for _, c := range []struct {
		min, max int32
		want     int
	}{{1, 1, 1}, {3, 6, 3}} {
		if ws, n := v1Last(&speechpb.SpeakerDiarizationConfig{MinSpeakerCount: c.min, MaxSpeakerCount: c.max}); n != c.want {
			t.Errorf("v1 speakers %d-%d: %d labels, want %d: %v", c.min, c.max, n, c.want, ws)
		}
	}
	if _, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: v1Cfg(&speechpb.SpeakerDiarizationConfig{MinSpeakerCount: 3, MaxSpeakerCount: 2}),
		Audio: &speechpb.RecognitionAudio{AudioSource: &speechpb.RecognitionAudio_Content{Content: clip}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("v1 min 3 max 2: %v", err)
	}

	// v1 StreamingRecognize: every final result holds the words so far.
	sc := v1Cfg(&speechpb.SpeakerDiarizationConfig{})
	sc.Encoding, sc.SampleRateHertz = speechpb.RecognitionConfig_LINEAR16, 16000
	vs, err := v1.StreamingRecognize(ctx)
	if err != nil {
		t.Fatalf("v1 StreamingRecognize: %v", err)
	}
	if err := vs.Send(&speechpb.StreamingRecognizeRequest{StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
		StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: sc}}}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for b := mono; len(b) > 0; {
			n := min(16000, len(b))
			if vs.Send(&speechpb.StreamingRecognizeRequest{StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{AudioContent: b[:n]}}) != nil {
				return
			}
			b = b[n:]
		}
		_ = vs.CloseSend()
	}()
	var finals [][]labelled
	for {
		resp, err := vs.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("v1 StreamingRecognize Recv: %v", err)
		}
		for _, r := range resp.GetResults() {
			if !r.GetIsFinal() {
				continue
			}
			var ws []labelled
			for _, w := range r.GetAlternatives()[0].GetWords() {
				if w.GetSpeakerTag() < 1 || w.GetSpeakerLabel() == "" {
					t.Errorf("v1 stream word %+v", w)
				}
				ws = append(ws, labelled{bare(w.GetWord()), w.GetSpeakerLabel()})
			}
			finals = append(finals, ws)
		}
	}
	if len(finals) == 0 {
		t.Fatal("v1 StreamingRecognize: no final result")
	}
	for i := 1; i < len(finals); i++ {
		if len(finals[i]) <= len(finals[i-1]) {
			t.Errorf("v1 stream: final %d has %d words, final %d %d: not every word from the beginning", i, len(finals[i]), i-1, len(finals[i-1]))
		}
	}
	checkTurns("v1 stream", finals[len(finals)-1])
}

// bare is a word in lower case without punctuation.
func bare(w string) string {
	return strings.ToLower(strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
}
