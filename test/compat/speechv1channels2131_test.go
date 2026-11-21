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

	speechv1 "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestSpeechV1Channels2131 (#2131): v1's multi-channel recognition through
// the official cloud.google.com/go/speech/apiv1 client on the local AI
// endpoint, never Google's: a stereo clip, JFK's inaugural sentence on the
// left channel and the adaptation sample ("... station ...") on the right
// (stereoCall), with audio_channel_count 2 and
// enable_separate_recognition_per_channel:
//
//   - Recognize (a stereo WAV) and LongRunningRecognize (its LINEAR16
//     samples): results with channel_tag 1 hold JFK's words and none of
//     the other's, channel_tag 2 the station sentence's; total_billed_time
//     counts both channels ("billed cumulatively for all channels
//     recognized").
//   - StreamingRecognize (the LINEAR16 samples in chunks): final results of
//     each channel, tagged the same way.
//   - Without the flag only the first channel is recognized ("We only
//     recognize the first channel by default"), with no channel_tag.
//   - audio_channel_count 2 is INVALID_ARGUMENT for MULAW ("Valid value for
//     MULAW, AMR, AMR_WB and SPEEX_WITH_HEADER_BYTE is only 1"), 9 for
//     LINEAR16 ("1-8").
//   - With diarization (the -tags onnx build), twospeakers.wav on the left
//     and JFK on the right: each channel has its own last result of tagged
//     words, the left one the clip's two speakers.
//
// covers: google.cloud.speech.v1.Speech/Recognize, google.cloud.speech.v1.Speech/LongRunningRecognize, google.cloud.speech.v1.Speech/StreamingRecognize
func TestSpeechV1Channels2131(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	v1, err := speechv1.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvLocalAI)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speechv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = v1.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)

	wav := stereoCall(t)
	samples := wav[44:]
	content := func(b []byte) *speechpb.RecognitionAudio {
		return &speechpb.RecognitionAudio{AudioSource: &speechpb.RecognitionAudio_Content{Content: b}}
	}
	perChannel := func(enc speechpb.RecognitionConfig_AudioEncoding) *speechpb.RecognitionConfig {
		c := &speechpb.RecognitionConfig{LanguageCode: "en-US", Model: "latest_long", Encoding: enc,
			AudioChannelCount: 2, EnableSeparateRecognitionPerChannel: true}
		if enc == speechpb.RecognitionConfig_LINEAR16 {
			c.SampleRateHertz = 16000
		}
		return c
	}
	type tagged interface {
		GetChannelTag() int32
		GetAlternatives() []*speechpb.SpeechRecognitionAlternative
	}
	byChannel := func(results []tagged) map[int32]string {
		out := map[int32]string{}
		for _, r := range results {
			if len(r.GetAlternatives()) > 0 {
				out[r.GetChannelTag()] += strings.ToLower(r.GetAlternatives()[0].GetTranscript()) + " "
			}
		}
		return out
	}
	check := func(name string, got map[int32]string) {
		t.Helper()
		if len(got) != 2 || !strings.Contains(got[1], "country") || strings.Contains(got[1], "station") ||
			!strings.Contains(got[2], "station") || strings.Contains(got[2], "country") {
			t.Errorf("%s: transcripts by channel_tag %q, want JFK on 1 and the station sentence on 2", name, got)
		}
	}
	asTagged := func(rs []*speechpb.SpeechRecognitionResult) []tagged {
		out := make([]tagged, len(rs))
		for i, r := range rs {
			out[i] = r
		}
		return out
	}

	res, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: perChannel(speechpb.RecognitionConfig_ENCODING_UNSPECIFIED), Audio: content(wav)})
	if err != nil {
		t.Fatalf("Recognize per channel: %v", err)
	}
	check("Recognize", byChannel(asTagged(res.GetResults())))
	if got := res.GetTotalBilledTime().AsDuration(); got != 22*time.Second {
		t.Errorf("Recognize per channel: total_billed_time %v, want 22s (two 11 s channels)", got)
	}

	op, err := v1.LongRunningRecognize(ctx, &speechpb.LongRunningRecognizeRequest{Config: perChannel(speechpb.RecognitionConfig_LINEAR16), Audio: content(samples)})
	if err != nil {
		t.Fatalf("LongRunningRecognize per channel: %v", err)
	}
	lres, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("LongRunningRecognize wait: %v", err)
	}
	check("LongRunningRecognize", byChannel(asTagged(lres.GetResults())))

	// Without the flag: the first channel only, untagged.
	first := perChannel(speechpb.RecognitionConfig_LINEAR16)
	first.EnableSeparateRecognitionPerChannel = false
	fres, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: first, Audio: content(samples)})
	if err != nil {
		t.Fatalf("Recognize without the flag: %v", err)
	}
	if got := byChannel(asTagged(fres.GetResults())); len(got) != 1 || !strings.Contains(got[0], "country") || strings.Contains(got[0], "station") {
		t.Errorf("Recognize without the flag: %q, want the first channel alone, channel_tag 0", got)
	}

	// The documented channel counts.
	for _, c := range []struct {
		enc speechpb.RecognitionConfig_AudioEncoding
		n   int32
	}{{speechpb.RecognitionConfig_MULAW, 2}, {speechpb.RecognitionConfig_LINEAR16, 9}} {
		bad := perChannel(c.enc)
		bad.SampleRateHertz, bad.AudioChannelCount = 16000, c.n
		if _, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: bad, Audio: content(samples)}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s with %d channels: %v, want InvalidArgument", c.enc, c.n, err)
		}
	}

	// StreamingRecognize per channel.
	streamed := func(cfg *speechpb.RecognitionConfig, audio []byte) []tagged {
		t.Helper()
		vs, err := v1.StreamingRecognize(ctx)
		if err != nil {
			t.Fatalf("StreamingRecognize: %v", err)
		}
		if err := vs.Send(&speechpb.StreamingRecognizeRequest{StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{Config: cfg}}}); err != nil {
			t.Fatal(err)
		}
		go func() {
			for b := audio; len(b) > 0; {
				n := min(16000, len(b))
				if vs.Send(&speechpb.StreamingRecognizeRequest{StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{AudioContent: b[:n]}}) != nil {
					return
				}
				b = b[n:]
			}
			_ = vs.CloseSend()
		}()
		var finals []tagged
		for {
			resp, err := vs.Recv()
			if err == io.EOF {
				return finals
			}
			if err != nil {
				t.Fatalf("StreamingRecognize Recv: %v", err)
			}
			for _, r := range resp.GetResults() {
				if r.GetIsFinal() {
					finals = append(finals, r)
				}
			}
		}
	}
	check("StreamingRecognize", byChannel(streamed(perChannel(speechpb.RecognitionConfig_LINEAR16), samples)))

	if os.Getenv(EnvONNX) == "" {
		dc := perChannel(speechpb.RecognitionConfig_LINEAR16)
		dc.DiarizationConfig = &speechpb.SpeakerDiarizationConfig{EnableSpeakerDiarization: true}
		if _, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: dc, Audio: content(samples)}); status.Code(err) != codes.Unimplemented {
			t.Errorf("diarization per channel without the onnx build: %v", err)
		}
		return
	}

	// Diarization per channel: two speakers on the left, JFK on the right.
	two := wavPCM(t, "../../internal/speech/testdata/twospeakers.wav")
	jfk := wavPCM(t, "../../internal/speech/testdata/jfk.wav")
	n := max(len(two), len(jfk)) / 2
	stereo := make([]byte, 4*n)
	for i := range n {
		if 2*i+1 < len(two) {
			copy(stereo[4*i:], two[2*i:2*i+2])
		}
		if 2*i+1 < len(jfk) {
			copy(stereo[4*i+2:], jfk[2*i:2*i+2])
		}
	}
	dc := perChannel(speechpb.RecognitionConfig_LINEAR16)
	dc.DiarizationConfig = &speechpb.SpeakerDiarizationConfig{EnableSpeakerDiarization: true}
	dres, err := v1.Recognize(ctx, &speechpb.RecognizeRequest{Config: dc, Audio: content(stereo)})
	if err != nil {
		t.Fatalf("Recognize per channel with diarization: %v", err)
	}
	// Each channel's last result holds its words, every one tagged.
	last := map[int32]*speechpb.SpeechRecognitionResult{}
	for _, r := range dres.GetResults() {
		last[r.GetChannelTag()] = r
	}
	for ch, want := range map[int32]string{1: "monday", 2: "country"} {
		r := last[ch]
		if r == nil {
			t.Fatalf("channel %d: no result in %v", ch, dres.GetResults())
		}
		labels, words := map[int32]bool{}, ""
		for _, w := range r.GetAlternatives()[0].GetWords() {
			if w.GetSpeakerTag() < 1 {
				t.Errorf("channel %d: word %q untagged", ch, w.GetWord())
			}
			labels[w.GetSpeakerTag()] = true
			words += bare(w.GetWord()) + " "
		}
		if !strings.Contains(words, want) {
			t.Errorf("channel %d's last result: %q, want %q in it", ch, words, want)
		}
		if ch == 1 {
			of := map[string]int32{}
			for _, w := range r.GetAlternatives()[0].GetWords() {
				if _, ok := of[bare(w.GetWord())]; !ok {
					of[bare(w.GetWord())] = w.GetSpeakerTag()
				}
			}
			if len(labels) != 2 || of["morning"] != of["sorry"] || of["internet"] != of["monday"] || of["morning"] == of["internet"] {
				t.Errorf("channel 1: %s", fmt.Sprint(of))
			}
		}
	}
}
