//go:build compat

package compat

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsAVideosAudioTrackThroughThePredictionClient (#1732)
// embeds a video with embed_content_config.audio_track_extraction through
// the official aiplatform PredictionClient's EmbedContent, as Google
// documents it ("Extract audio from video input", off by default, within
// the 180 seconds of audio a request). testdata/turtle-jfk.mp4 is the
// sea turtle clip of the video tests looped to 11 seconds (H.264, 5 fps)
// with the model card's jfk.wav as its AAC-LC track (16 kHz mono, by ffmpeg
// 9). The track is embedded as a clip right after the frames, as
// EmbeddingGemma 2's processor loads a video's audio
// (load_audio_from_video): the vector is the one of the video's frames
// followed by the WAV as a part of its own, and the speech query scores it
// far higher than the frames alone; without the option it is the frames
// alone, and with video_metadata's offsets the track is cut to them. Since
// #1739 an Opus track is embedded as the AAC one is.
func TestEmbedContentEmbedsAVideosAudioTrackThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	video, err := os.ReadFile("testdata/turtle-jfk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	wav := fetchJFK(t, ctx)
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	embed := func(track bool, parts ...*aiplatformpb.Part) (*aiplatformpb.EmbedContentResponse, error) {
		return pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{AudioTrackExtraction: proto.Bool(track)},
		})
	}
	must := func(r *aiplatformpb.EmbedContentResponse, err error) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		if err != nil {
			t.Fatalf("EmbedContent: %v", err)
		}
		return r
	}
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}

	frames := must(embed(false, inline("video/mp4", video)))
	withTrack := must(embed(true, inline("video/mp4", video)))
	v := withTrack.GetEmbedding().GetValues()
	if len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
		t.Fatalf("with the audio track: %d values, norm %.6f", len(v), norm(v))
	}
	nf, nt := frames.GetUsageMetadata().GetPromptTokenCount(), withTrack.GetUsageMetadata().GetPromptTokenCount()
	// 11 seconds of audio are 275 soft tokens and the two audio markers.
	if nt < nf+277 || nt > nf+279 {
		t.Errorf("tokens: %d with the audio track, %d without; want 277 more", nt, nf)
	}
	// The video's frames followed by the speech as a part of its own.
	parts := must(embed(false, inline("video/mp4", video), inline("audio/wav", wav)))
	if c := cosine(v, parts.GetEmbedding().GetValues()); c < 0.99 {
		t.Errorf("against the video and the WAV as two parts: cosine %.4f, want at least 0.99", c)
	}
	queries := []string{"a president's speech about serving your country", "cats sleeping on a couch", "a turtle swimming in the ocean"}
	score := func(vec []float32) []float64 {
		out := make([]float64, len(queries))
		for i, q := range queries {
			r := must(pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
				Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: q}}}},
				EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY.Enum()},
			}))
			out[i] = cosine(r.GetEmbedding().GetValues(), vec)
		}
		return out
	}
	sf, st := score(frames.GetEmbedding().GetValues()), score(v)
	if st[0] < sf[0]+0.05 || st[1] > st[0] {
		t.Errorf("the speech query: %.3f with the audio track, %.3f without (%.3f, %.3f); want it well up", st[0], sf[0], st, sf)
	}
	t.Logf("tokens %d without the track, %d with; scores %q: frames %.3f, with the track %.3f; cosine with video+WAV parts %.6f",
		nf, nt, queries, sf, st, cosine(v, parts.GetEmbedding().GetValues()))

	// video_metadata's offsets cut the track too: 2 to 6 seconds is 100
	// soft tokens of audio.
	cut := &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: "video/mp4", Data: video}},
		Metadata: &aiplatformpb.Part_VideoMetadata{VideoMetadata: &aiplatformpb.VideoMetadata{
			StartOffset: durationpb.New(2 * time.Second), EndOffset: durationpb.New(6 * time.Second)}}}
	cf, ct := must(embed(false, cut)).GetUsageMetadata().GetPromptTokenCount(), must(embed(true, cut)).GetUsageMetadata().GetPromptTokenCount()
	if ct < cf+102 || ct > cf+104 {
		t.Errorf("2s to 6s: %d tokens with the audio track, %d without; want 102 more", ct, cf)
	}

	// An Opus track (#1739): testdata/turtle-jfk-opus.mp4 is the same file
	// with its track re-encoded by libopus (48 kHz mono, 48 kbit/s): the
	// same tokens, and a vector within 0.99 of the AAC track's.
	opusVideo, err := os.ReadFile("testdata/turtle-jfk-opus.mp4")
	if err != nil {
		t.Fatal(err)
	}
	withOpus := must(embed(true, inline("video/mp4", opusVideo)))
	if n := withOpus.GetUsageMetadata().GetPromptTokenCount(); n != nt {
		t.Errorf("Opus track: %d tokens, the AAC track's %d", n, nt)
	}
	co := cosine(withOpus.GetEmbedding().GetValues(), v)
	if co < 0.99 {
		t.Errorf("Opus track against the AAC track: cosine %.4f, want at least 0.99", co)
	}
	t.Logf("Opus track: cosine %.6f to the AAC track's vector", co)

	// A track this build does not decode is refused naming it.
	if _, err := embed(true, inline("video/mp4", []byte("not a video"))); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "not a valid MP4") {
		t.Errorf("not a video: %v", err)
	}
}
