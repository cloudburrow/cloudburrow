//go:build compat

package compat

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/proto"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsGSM610WAVThroughThePredictionClient (#1651) embeds,
// through the official aiplatform PredictionClient's EmbedContent, a GSM
// 6.10 WAV (WAVE_FORMAT_GSM610, Microsoft's 65-byte blocks), which was
// UNIMPLEMENTED: internal/embedding/testdata/wav-gsm.wav, 2 seconds of
// frames of random parameters (no GSM encoder is at hand; ffmpeg 9 has
// none), which this build decodes sample for sample as ffmpeg 9 does
// (internal/embedding's TestDecodeADPCMWAV). It is the vector and token
// count of wav-gsm-ffmpeg.wav, ffmpeg 9's decode of it as 16-bit PCM.
func TestEmbedContentEmbedsGSM610WAVThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	embed := func(file string) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name),
			Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_InlineData{
				InlineData: &aiplatformpb.Blob{MimeType: "audio/wav", Data: embeddingTestdata(t, file)}}}}},
		})
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", file, err)
		}
		if v := r.GetEmbedding().GetValues(); len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", file, len(v), norm(v))
		}
		return r
	}
	gsm, pcm := embed("wav-gsm.wav"), embed("wav-gsm-ffmpeg.wav")
	n, want := gsm.GetUsageMetadata().GetPromptTokenCount(), pcm.GetUsageMetadata().GetPromptTokenCount()
	c := cosine(gsm.GetEmbedding().GetValues(), pcm.GetEmbedding().GetValues())
	if n != want || c < 0.99999 {
		t.Errorf("the GSM 6.10 WAV: %d tokens at cosine %.6f to ffmpeg's decode (%d tokens); want the same vector", n, c, want)
	}
	t.Logf("wav-gsm.wav: %d tokens (ffmpeg's PCM decode %d), cosine %.6f", n, want, c)
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsMPEGLayerIIWAVThroughThePredictionClient (#1651)
// embeds, through the official aiplatform PredictionClient's EmbedContent,
// a WAV of MPEG-1 Layer II frames (WAVE_FORMAT_MPEG), which was
// UNIMPLEMENTED: internal/embedding/testdata/jfk-mp2.wav, testdata/
// jfk-alaw.wav's clip encoded by ffmpeg 9 (`-c:a mp2 -ar 48000 -ac 1 -b:a
// 128k -f wav`), decoded within 1.1 16-bit steps of ffmpeg's decode
// (internal/embedding's TestDecodeMPEGLayerIIWAV). It is the A-law clip's
// token count (up to 2 more for the encoder's delay and padding) and
// within cosine 0.97 of its vector.
func TestEmbedContentEmbedsMPEGLayerIIWAVThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	embed := func(what string, data []byte) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name),
			Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_InlineData{
				InlineData: &aiplatformpb.Blob{MimeType: "audio/wav", Data: data}}}}},
		})
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		if v := r.GetEmbedding().GetValues(); len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", what, len(v), norm(v))
		}
		return r
	}
	alawData, err := os.ReadFile("testdata/jfk-alaw.wav")
	if err != nil {
		t.Fatal(err)
	}
	alaw := embed("jfk-alaw.wav", alawData)
	mp2 := embed("jfk-mp2.wav", embeddingTestdata(t, "jfk-mp2.wav"))
	n, want := mp2.GetUsageMetadata().GetPromptTokenCount(), alaw.GetUsageMetadata().GetPromptTokenCount()
	if n < want || n > want+2 {
		t.Errorf("jfk-mp2.wav: %d tokens, the A-law clip's %d (up to 2 more)", n, want)
	}
	c := cosine(mp2.GetEmbedding().GetValues(), alaw.GetEmbedding().GetValues())
	if c < 0.97 {
		t.Errorf("jfk-mp2.wav against the A-law clip: cosine %.4f, want at least 0.97", c)
	}
	t.Logf("jfk-mp2.wav: %d tokens (A-law %d), cosine %.6f to the A-law clip", n, want, c)
}
