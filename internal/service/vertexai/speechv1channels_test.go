package vertexai

import (
	"encoding/binary"
	"testing"
	"time"

	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// TestSpeechV1Channels: audio_channel_count and
// enable_separate_recognition_per_channel, as the v1 reference documents
// them.
func TestSpeechV1Channels(t *testing.T) {
	v := newSpeech(t, "")
	for _, c := range []struct {
		name string
		cfg  *speechv1pb.RecognitionConfig
		want codes.Code
		per  bool
	}{
		{"linear16 stereo", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_LINEAR16,
			SampleRateHertz: 16000, AudioChannelCount: 2, EnableSeparateRecognitionPerChannel: true}, codes.OK, true},
		{"flag without channels", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", EnableSeparateRecognitionPerChannel: true}, codes.OK, false},
		{"flag with one channel", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", AudioChannelCount: 1, EnableSeparateRecognitionPerChannel: true}, codes.OK, false},
		{"channels without flag", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_LINEAR16,
			SampleRateHertz: 16000, AudioChannelCount: 2}, codes.OK, false},
		{"wav stereo", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", AudioChannelCount: 2, EnableSeparateRecognitionPerChannel: true}, codes.OK, true},
		{"flac eight", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_FLAC, AudioChannelCount: 8,
			EnableSeparateRecognitionPerChannel: true}, codes.OK, true},
		{"nine channels", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_LINEAR16, AudioChannelCount: 9}, codes.InvalidArgument, false},
		{"mulaw stereo", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_MULAW, AudioChannelCount: 2}, codes.InvalidArgument, false},
		{"amr stereo", &speechv1pb.RecognitionConfig{LanguageCode: "en-US", Encoding: speechv1pb.RecognitionConfig_AMR, AudioChannelCount: 2}, codes.InvalidArgument, false},
	} {
		cfg, _, err := v.s.v1Checked(c.cfg, false)
		if code(grpcError(err)) != c.want || err == nil && cfg.PerChannel != c.per {
			t.Errorf("%s: %v per channel %v, want %v per channel %v", c.name, err, cfg.PerChannel, c.want, c.per)
		}
	}

	// Each channel's results carry its channel_tag; diarization's last
	// result is one a channel.
	cfg := speechconfig.Config{LanguageCode: "en-us", Options: speechconfig.Options{PerChannel: true, Diarization: true}}
	cfg.AutomaticPunctuation = true
	res := speech.Result{Segments: []speech.Segment{
		{Text: "left", End: time.Second, Channel: 1, Words: []speech.Word{{Text: "left", End: time.Second, Speaker: 1}}},
		{Text: "right", End: 2 * time.Second, Channel: 2, Words: []speech.Word{{Text: "right", End: 2 * time.Second, Speaker: 1}}},
	}}
	out := v1Results(res, cfg)
	if len(out) != 4 {
		t.Fatalf("results %v, want two transcripts and two speaker results", out)
	}
	for i, want := range []int32{1, 2, 1, 2} {
		if out[i].GetChannelTag() != want {
			t.Errorf("result %d channel_tag %d, want %d", i, out[i].GetChannelTag(), want)
		}
	}
	if w := out[3].GetAlternatives()[0].GetWords(); len(w) != 1 || w[0].GetWord() != "right" || w[0].GetSpeakerTag() != 1 {
		t.Errorf("channel 2's speaker words %v", w)
	}
}

// TestStreamPerChannel: a stereo LINEAR16 stream, a tone on the left then
// on the right, answers one final result a channel, each with its channel.
func TestStreamPerChannel(t *testing.T) {
	fakeTranscribe(t)
	st := linear16Settings()
	st.cfg.Decoding.Channels, st.cfg.PerChannel = 2, true
	left := pcm(time.Second, true, false, false, false)
	right := pcm(time.Second, false, false, true, false)
	stereo := make([]byte, 0, 2*len(left))
	for i := 0; i+1 < len(left); i += 2 {
		stereo = binary.LittleEndian.AppendUint16(stereo, binary.LittleEndian.Uint16(left[i:]))
		stereo = binary.LittleEndian.AppendUint16(stereo, binary.LittleEndian.Uint16(right[i:]))
	}
	evs, err := collectStream(t, st, chunks(stereo, 16000))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, ev := range evs {
		if ev.kind == evResult && ev.final {
			got = append(got, ev.channel)
			if ev.channel == 2 && ev.offset < 3*time.Second {
				t.Errorf("channel 2's result ends at %v, before its speech", ev.offset)
			}
		}
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("final results' channels %v, want [1 2]", got)
	}
}
