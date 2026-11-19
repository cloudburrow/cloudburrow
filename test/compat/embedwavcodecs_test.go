//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/proto"
)

// pcm16WAV wraps 16-bit little-endian mono samples in a RIFF WAVE header.
func pcm16WAV(pcm []byte, rate uint32) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], rate)
	binary.LittleEndian.PutUint32(h[28:], rate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

// float32WAV wraps interleaved 32-bit float samples in a RIFF WAVE header.
func float32WAV(f32 []byte, channels int, rate uint32) []byte {
	h := pcm16WAV(nil, rate)
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(f32)))
	binary.LittleEndian.PutUint16(h[20:], 3) // IEEE float
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[28:], rate*4*uint32(channels))
	binary.LittleEndian.PutUint16(h[32:], uint16(4*channels))
	binary.LittleEndian.PutUint16(h[34:], 32)
	binary.LittleEndian.PutUint32(h[40:], uint32(len(f32)))
	return append(h, f32...)
}

// embedWAV is EmbedContent of one inline audio/wav part, checked for a
// 768-value unit vector.
func embedWAV(t *testing.T, ctx context.Context, pc *aiplatform.PredictionClient, name, what string, data []byte) *aiplatformpb.EmbedContentResponse {
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

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsMPEGLayerIAndJointStereoWAVThroughThePredictionClient
// (#1651) embeds, through the official aiplatform PredictionClient's
// EmbedContent, WAVs (WAVE_FORMAT_MPEG) of MPEG audio Layer I and of Layer
// II joint stereo, which were UNIMPLEMENTED: internal/embedding/testdata/
// wav-mp1-js-24.wav and wav-mp2-js-24.wav, 1 second of MPEG-2 LSF frames
// at 24 kHz of random parameters, every subband allocated (a silent band
// is where ffmpeg's 16-bit rounding noise moves the log-mel features: 16
// kHz frames with bands left empty were cosine 0.995), with intensity
// stereo above a random bound (ffmpeg 9 encodes neither), decoded within 1.1 16-bit steps of
// ffmpeg 9's decode (internal/embedding's TestDecodeMPEGLayerIIWAV). Each
// is the token count of ffmpeg's decode (its .raw.f32, as a float WAV) and
// within cosine 0.9999 of its vector.
func TestEmbedContentEmbedsMPEGLayerIAndJointStereoWAVThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	for _, file := range []string{"wav-mp1-js-24", "wav-mp2-js-24"} {
		m := embedWAV(t, ctx, pc, name, file+".wav", embeddingTestdata(t, file+".wav"))
		p := embedWAV(t, ctx, pc, name, file+".raw.f32 as a float WAV", float32WAV(embeddingTestdata(t, file+".raw.f32"), 2, 24000))
		n, want := m.GetUsageMetadata().GetPromptTokenCount(), p.GetUsageMetadata().GetPromptTokenCount()
		c := cosine(m.GetEmbedding().GetValues(), p.GetEmbedding().GetValues())
		if n != want || c < 0.9999 {
			t.Errorf("%s.wav: %d tokens at cosine %.6f to ffmpeg's decode (%d tokens); want the same tokens, cosine at least 0.9999", file, n, c, want)
		}
		t.Logf("%s.wav: %d tokens (ffmpeg's decode %d), cosine %.6f", file, n, want, c)
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsG726WAVThroughThePredictionClient (#1651) embeds,
// through the official aiplatform PredictionClient's EmbedContent, G.726
// WAVs (WAVE_FORMAT_G726_ADPCM, 0x45) at 16, 24, 32 and 40 kbit/s, which
// were UNIMPLEMENTED: internal/embedding/testdata/wav-g726-{2,3,4,5}.wav,
// ffmpeg 9's g726 encodes of 2 seconds of speech and a clipping sine,
// which this build decodes sample for sample as ffmpeg 9 does
// (internal/embedding's TestDecodeG726WAV). Each is the vector and token
// count of ffmpeg's decode of it (wav-g726-N.s16) as 16-bit PCM.
func TestEmbedContentEmbedsG726WAVThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	for bits := 2; bits <= 5; bits++ {
		file := fmt.Sprintf("wav-g726-%d", bits)
		g := embedWAV(t, ctx, pc, name, file+".wav", embeddingTestdata(t, file+".wav"))
		p := embedWAV(t, ctx, pc, name, file+".s16 as PCM", pcm16WAV(embeddingTestdata(t, file+".s16"), 8000))
		n, want := g.GetUsageMetadata().GetPromptTokenCount(), p.GetUsageMetadata().GetPromptTokenCount()
		c := cosine(g.GetEmbedding().GetValues(), p.GetEmbedding().GetValues())
		if n != want || c < 0.99999 {
			t.Errorf("%s.wav (%d kbit/s): %d tokens at cosine %.6f to ffmpeg's decode (%d tokens); want the same vector", file, 8*bits, n, c, want)
		}
		t.Logf("%s.wav (%d kbit/s): %d tokens (ffmpeg's PCM decode %d), cosine %.6f", file, 8*bits, n, want, c)
	}
}
