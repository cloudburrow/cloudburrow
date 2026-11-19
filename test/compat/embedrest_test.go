//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsMultichannelWAVAndGrayVideoThroughThePredictionClient
// (#1651) embeds, through the official aiplatform PredictionClient's
// EmbedContent, the WAVs and videos Google takes that were refused before:
//
//   - the model card's jfk.wav (44.1 kHz stereo) rewritten in-test as a
//     six-channel WAV (WAVE_FORMAT_EXTENSIBLE 5.1, its two channels in FL
//     and FR, the rest silent), downmixed as ffmpeg's resampler does: the
//     stereo WAV's vector;
//   - testdata/jfk-alaw.wav, the same clip as G.711 A-law at 16 kHz mono
//     (`ffmpeg -i jfk.wav -ac 1 -ar 16000 -c:a pcm_alaw`): its token count,
//     within cosine 0.99 of the WAV's vector;
//   - a monochrome H.265 copy of the sea turtle clip (libx265 -pix_fmt gray,
//     internal/embedding/testdata/turtle-h265-gray.mp4), converted as
//     ffmpeg converts gray: the colour clip's token count, nearest the
//     turtle query;
//   - a QuickTime clip with a 5.1 PCM track under audio_track_extraction
//     (va-pcm51.mov), which was taken as pairs of channels before.
//
// And valid input this build does not serve is UNIMPLEMENTED, not
// INVALID_ARGUMENT: an HTTP file_uri (an instance fetches nothing), a
// 3-channel Microsoft ADPCM WAV and a QuickTime IMA ADPCM (ima4) audio track
// (3- and 5-bit IMA ADPCM and µ-law tracks, which stood here, decode since
// #1651's later rounds).
func TestEmbedContentEmbedsMultichannelWAVAndGrayVideoThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	wav := fetchJFK(t, ctx)
	alaw, err := os.ReadFile("testdata/jfk-alaw.wav")
	if err != nil {
		t.Fatal(err)
	}
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	embed := func(track bool, parts ...*aiplatformpb.Part) (*aiplatformpb.EmbedContentResponse, error) {
		return pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{AudioTrackExtraction: proto.Bool(track)},
		})
	}
	must := func(what string, track bool, parts ...*aiplatformpb.Part) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := embed(track, parts...)
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		if v := r.GetEmbedding().GetValues(); len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", what, len(v), norm(v))
		}
		return r
	}
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}

	// Multichannel and A-law WAV.
	ref := must("jfk.wav", false, inline("audio/wav", wav))
	refTokens := ref.GetUsageMetadata().GetPromptTokenCount()
	six := must("5.1 WAV", false, inline("audio/wav", stereoAs51(t, wav)))
	if n := six.GetUsageMetadata().GetPromptTokenCount(); n != refTokens {
		t.Errorf("5.1 WAV: %d tokens, the stereo WAV's %d", n, refTokens)
	}
	if c := cosine(six.GetEmbedding().GetValues(), ref.GetEmbedding().GetValues()); c < 0.99999 {
		t.Errorf("5.1 WAV against the stereo WAV: cosine %.6f, want the same vector", c)
	}
	a := must("A-law WAV", false, inline("audio/wav", alaw))
	if n := a.GetUsageMetadata().GetPromptTokenCount(); n != refTokens {
		t.Errorf("A-law WAV: %d tokens, the WAV's %d", n, refTokens)
	}
	ca := cosine(a.GetEmbedding().GetValues(), ref.GetEmbedding().GetValues())
	if ca < 0.99 {
		t.Errorf("A-law WAV against the WAV: cosine %.4f, want at least 0.99", ca)
	}
	t.Logf("jfk.wav: %d tokens; 5.1 copy the same vector; A-law at cosine %.6f", refTokens, ca)

	// Monochrome H.265.
	color := must("H.264 clip", false, inline("video/mp4", embeddingTestdata(t, "turtle-h264.mp4")))
	gray := must("gray H.265 clip", false, inline("video/mp4", embeddingTestdata(t, "turtle-h265-gray.mp4")))
	if n, want := gray.GetUsageMetadata().GetPromptTokenCount(), color.GetUsageMetadata().GetPromptTokenCount(); n != want {
		t.Errorf("gray H.265: %d tokens, the colour clip's %d", n, want)
	}
	queries := []string{"cats sleeping on a couch", "a president's speech about serving your country", "a turtle swimming in the ocean"}
	scores := make([]float64, len(queries))
	for i, q := range queries {
		task := aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: q}}}},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task},
		})
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		scores[i] = cosine(r.GetEmbedding().GetValues(), gray.GetEmbedding().GetValues())
	}
	if scores[2] <= scores[0] || scores[2] <= scores[1] {
		t.Errorf("gray H.265 against %q: %.3f, want the turtle query nearest", queries, scores)
	}
	t.Logf("gray H.265: %d tokens, cosine %.6f to the colour clip, %.3f against %q", gray.GetUsageMetadata().GetPromptTokenCount(),
		cosine(gray.GetEmbedding().GetValues(), color.GetEmbedding().GetValues()), scores, queries)

	// A 5.1 PCM audio track.
	pcm := embeddingTestdata(t, "va-pcm51.mov")
	frames := must("5.1 PCM MOV", false, inline("video/quicktime", pcm))
	withTrack := must("5.1 PCM MOV with its track", true, inline("video/quicktime", pcm))
	// 0.5 seconds of audio: 12 or 13 soft tokens and the two markers.
	if d := withTrack.GetUsageMetadata().GetPromptTokenCount() - frames.GetUsageMetadata().GetPromptTokenCount(); d < 14 || d > 16 {
		t.Errorf("5.1 PCM track: %d tokens more than the frames, want 14 to 16", d)
	}

	// Valid input not served: UNIMPLEMENTED.
	for _, c := range []struct {
		name  string
		track bool
		part  *aiplatformpb.Part
		want  string
	}{
		{"an HTTP file_uri", false, &aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{
			MimeType: "audio/wav", FileUri: "https://storage.googleapis.com/b/jfk.wav"}}}, "HTTP URLs are not fetched"},
		{"a 3-channel Microsoft ADPCM WAV", false, inline("audio/wav", adpcmWAVFile()), "Microsoft ADPCM of 3 channels"},
		{"an ima4 QuickTime audio track", true, inline("video/quicktime", embeddingTestdata(t, "va-ima4.mov")), "ima4"},
	} {
		_, err := embed(c.track, c.part)
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want Unimplemented naming %q", c.name, err, c.want)
		}
	}
}

// stereoAs51 rewrites a 16-bit stereo WAV as a WAVE_FORMAT_EXTENSIBLE 5.1
// one (mask 0x3f: FL, FR, FC, LFE, BL, BR), the two channels in FL and FR.
func stereoAs51(t *testing.T, b []byte) []byte {
	t.Helper()
	var rate uint32
	var data []byte
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8 : min(p+8+size, len(b))]
		switch id {
		case "fmt ":
			if binary.LittleEndian.Uint16(body[0:]) != 1 || binary.LittleEndian.Uint16(body[2:]) != 2 || binary.LittleEndian.Uint16(body[14:]) != 16 {
				t.Fatal("jfk.wav is not 16-bit stereo PCM")
			}
			rate = binary.LittleEndian.Uint32(body[4:])
		case "data":
			data = body
		}
		p += 8 + size + size&1
	}
	if rate == 0 || data == nil {
		t.Fatal("jfk.wav has no fmt or data chunk")
	}
	frames := len(data) / 4
	out := make([]byte, frames*12)
	for i := range frames {
		copy(out[12*i:], data[4*i:4*i+4])
	}
	f := binary.LittleEndian.AppendUint16(nil, 0xFFFE)
	f = binary.LittleEndian.AppendUint16(f, 6)
	f = binary.LittleEndian.AppendUint32(f, rate)
	f = binary.LittleEndian.AppendUint32(f, rate*12)
	f = binary.LittleEndian.AppendUint16(f, 12)
	f = binary.LittleEndian.AppendUint16(f, 16)
	f = binary.LittleEndian.AppendUint16(f, 22)
	f = binary.LittleEndian.AppendUint16(f, 16)
	f = binary.LittleEndian.AppendUint32(f, 0x3f)
	f = append(f, 1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71) // KSDATAFORMAT_SUBTYPE_PCM
	w := append([]byte("WAVEfmt "), binary.LittleEndian.AppendUint32(nil, uint32(len(f)))...)
	w = append(w, f...)
	w = append(append(w, "data"...), binary.LittleEndian.AppendUint32(nil, uint32(len(out)))...)
	w = append(w, out...)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(w)))...), w...)
}

// adpcmWAVFile is a 3-channel Microsoft ADPCM WAV (format 2), valid and not
// decoded here (2- to 5-bit IMA and mono and stereo Microsoft ADPCM are,
// #1651).
func adpcmWAVFile() []byte {
	f := []byte{2, 0, 3, 0, 0x80, 0x3e, 0, 0, 0, 0x5e, 0, 0, 0, 1, 4, 0}
	w := append([]byte("WAVEfmt "), byte(len(f)), 0, 0, 0)
	w = append(append(w, f...), 'd', 'a', 't', 'a', 64, 0, 0, 0)
	w = append(w, make([]byte, 64)...)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(w)))...), w...)
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsADPCMWAVAndLosslessAndMultichannelTracksThroughThePredictionClient
// (#1651) embeds, through the official aiplatform PredictionClient's
// EmbedContent, media that was UNIMPLEMENTED before, all made with ffmpeg 9
// from the first 6 seconds of testdata/jfk-alaw.wav:
//
//   - IMA and Microsoft ADPCM WAVs (jfk-ima.wav, jfk-ms.wav: `-c:a
//     adpcm_ima_wav`, `-c:a adpcm_ms`): as many tokens as the A-law clip
//     cut to 6 seconds would have, within cosine 0.99 of each other;
//   - a 64x36 clip at 1 fps with a FLAC and with an ALAC audio track
//     (va-jfk-flac.mp4, va-jfk-alac.mp4, 16-bit mono), under
//     audio_track_extraction: lossless, so the same vector;
//   - the clip with a 5.1 AAC-LC track (va-jfk-aac51.mp4, the speech in FL
//     and FR, `-filter:a pan=5.1|FL=c0|FR=c0 -c:a aac -b:a 96k`): the FLAC
//     track's token count, within cosine 0.98 of its vector.
func TestEmbedContentEmbedsADPCMWAVAndLosslessAndMultichannelTracksThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	read := func(f string) []byte {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	must := func(what string, track bool, mt string, data []byte) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name),
			Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_InlineData{
				InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}}},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{AudioTrackExtraction: proto.Bool(track)},
		})
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		if v := r.GetEmbedding().GetValues(); len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", what, len(v), norm(v))
		}
		return r
	}

	ima := must("IMA ADPCM WAV", false, "audio/wav", read("jfk-ima.wav"))
	ms := must("Microsoft ADPCM WAV", false, "audio/wav", read("jfk-ms.wav"))
	// About 6.1 seconds (each codec pads its last block): 25 soft tokens a
	// second and the two markers.
	for _, r := range []*aiplatformpb.EmbedContentResponse{ima, ms} {
		if n := r.GetUsageMetadata().GetPromptTokenCount(); n < 150 || n > 160 {
			t.Errorf("ADPCM WAV: %d tokens, want 150 to 160", n)
		}
	}
	if c := cosine(ima.GetEmbedding().GetValues(), ms.GetEmbedding().GetValues()); c < 0.99 {
		t.Errorf("IMA against Microsoft ADPCM: cosine %.4f, want at least 0.99", c)
	}

	flac := must("FLAC track", true, "video/mp4", read("va-jfk-flac.mp4"))
	alac := must("ALAC track", true, "video/mp4", read("va-jfk-alac.mp4"))
	frames := must("the clip's frames", false, "video/mp4", read("va-jfk-flac.mp4"))
	// 6 seconds of audio: 150 soft tokens and the two markers.
	if d := flac.GetUsageMetadata().GetPromptTokenCount() - frames.GetUsageMetadata().GetPromptTokenCount(); d < 150 || d > 154 {
		t.Errorf("FLAC track: %d tokens more than the frames, want 150 to 154", d)
	}
	if a, f := alac.GetUsageMetadata().GetPromptTokenCount(), flac.GetUsageMetadata().GetPromptTokenCount(); a != f {
		t.Errorf("ALAC track: %d tokens, the FLAC track's %d", a, f)
	}
	if c := cosine(alac.GetEmbedding().GetValues(), flac.GetEmbedding().GetValues()); c < 0.99999 {
		t.Errorf("ALAC against FLAC: cosine %.6f, want the same vector", c)
	}
	aac := must("5.1 AAC track", true, "video/mp4", read("va-jfk-aac51.mp4"))
	if a, f := aac.GetUsageMetadata().GetPromptTokenCount(), flac.GetUsageMetadata().GetPromptTokenCount(); a != f {
		t.Errorf("5.1 AAC track: %d tokens, the FLAC track's %d", a, f)
	}
	ca := cosine(aac.GetEmbedding().GetValues(), flac.GetEmbedding().GetValues())
	if ca < 0.98 {
		t.Errorf("5.1 AAC against FLAC: cosine %.4f, want at least 0.98", ca)
	}
	t.Logf("ADPCM: %d and %d tokens; FLAC %d tokens (frames %d), ALAC the same vector, 5.1 AAC at cosine %.4f",
		ima.GetUsageMetadata().GetPromptTokenCount(), ms.GetUsageMetadata().GetPromptTokenCount(),
		flac.GetUsageMetadata().GetPromptTokenCount(), frames.GetUsageMetadata().GetPromptTokenCount(), ca)
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsSurroundAACAndG711TracksAnd3BitADPCMThroughThePredictionClient
// (#1651) embeds, through the official aiplatform PredictionClient's
// EmbedContent, media that was UNIMPLEMENTED before, made from the first 6
// seconds of testdata/jfk-alaw.wav (the speech in FL and FR) on the 64x36
// clip at 1 fps of va-jfk-flac.mp4, under audio_track_extraction:
//
//   - 7.1 AAC-LC as ffmpeg 9 writes it, channel configuration 12
//     (va-jfk-aac71.mp4, `-filter:a pan=7.1|FL=c0|FR=c0 -c:a aac -b:a 128k`),
//     and 7.1(wide), a program_config_element (va-jfk-aac71w.mp4);
//   - 7.1 AAC-LC as AudioToolbox writes it, configuration 7
//     (va-jfk-aac71-at.mp4, `aresample=48000,pan=7.1(wide)|FL=c0|FR=c0
//     -c:a aac_at -b:a 256k`), and 5.1 HE-AAC, SBR over every element
//     (va-jfk-heaac51.mp4, `pan=5.1(side)|... -c:a aac_at -profile:a 4
//     -b:a 128k`);
//   - a QuickTime µ-law track (va-jfk-ulaw.mov, `-c:a pcm_mulaw -f mov`);
//
// each the FLAC track's token count (AudioToolbox's within 4, for its
// padding) and within cosine 0.98 of its vector; and a 3-bit IMA ADPCM WAV
// of the whole clip (testdata/jfk-ima3.wav, written by a scratch encoder,
// since ffmpeg writes no 3-bit IMA ADPCM; ffmpeg 9 decodes it as this
// build does), within cosine 0.97 of the A-law clip's vector.
func TestEmbedContentEmbedsSurroundAACAndG711TracksAnd3BitADPCMThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	read := func(f string) []byte {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	must := func(what string, track bool, mt string, data []byte) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name),
			Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_InlineData{
				InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}}},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{AudioTrackExtraction: proto.Bool(track)},
		})
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		if v := r.GetEmbedding().GetValues(); len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", what, len(v), norm(v))
		}
		return r
	}

	flac := must("FLAC track", true, "video/mp4", read("va-jfk-flac.mp4"))
	ft := flac.GetUsageMetadata().GetPromptTokenCount()
	for _, c := range []struct {
		file, mt string
		slack    int32 // tokens more than the FLAC track's
	}{
		{"va-jfk-aac71.mp4", "video/mp4", 0},
		{"va-jfk-aac71w.mp4", "video/mp4", 0},
		{"va-jfk-aac71-at.mp4", "video/mp4", 4},
		{"va-jfk-heaac51.mp4", "video/mp4", 4},
		{"va-jfk-ulaw.mov", "video/quicktime", 0},
	} {
		r := must(c.file, true, c.mt, read(c.file))
		n := r.GetUsageMetadata().GetPromptTokenCount()
		if n < ft || n > ft+c.slack {
			t.Errorf("%s: %d tokens, the FLAC track's %d (up to %d more)", c.file, n, ft, c.slack)
		}
		cs := cosine(r.GetEmbedding().GetValues(), flac.GetEmbedding().GetValues())
		if cs < 0.98 {
			t.Errorf("%s against the FLAC track: cosine %.4f, want at least 0.98", c.file, cs)
		}
		t.Logf("%s: %d tokens (FLAC %d), cosine %.6f to the FLAC track", c.file, n, ft, cs)
	}

	alaw := must("A-law WAV", false, "audio/wav", read("jfk-alaw.wav"))
	ima3 := must("3-bit IMA ADPCM WAV", false, "audio/wav", read("jfk-ima3.wav"))
	if n, want := ima3.GetUsageMetadata().GetPromptTokenCount(), alaw.GetUsageMetadata().GetPromptTokenCount(); n < want || n > want+1 {
		t.Errorf("3-bit IMA ADPCM WAV: %d tokens, the A-law clip's %d (its last block pads it)", n, want)
	}
	c3 := cosine(ima3.GetEmbedding().GetValues(), alaw.GetEmbedding().GetValues())
	if c3 < 0.97 {
		t.Errorf("3-bit IMA ADPCM against A-law: cosine %.4f, want at least 0.97", c3)
	}
	t.Logf("3-bit IMA ADPCM: %d tokens, cosine %.6f to the A-law clip", ima3.GetUsageMetadata().GetPromptTokenCount(), c3)
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbeds2And5BitADPCMAndMP3WAVsThroughThePredictionClient
// (#1651) embeds, through the official aiplatform PredictionClient's
// EmbedContent, WAVs that were UNIMPLEMENTED before, each of
// testdata/jfk-alaw.wav's clip:
//
//   - 2- and 5-bit IMA ADPCM (jfk-ima2.wav, jfk-ima5.wav, 256-byte blocks),
//     written by a scratch encoder, since ffmpeg writes neither; ffmpeg 9
//     decodes each as this build does (internal/embedding's
//     TestDecodeADPCMWAV);
//   - MP3 in a WAV, WAVE_FORMAT_MPEGLAYER3 (jfk-mp3.wav, `ffmpeg -i
//     jfk-alaw.wav -c:a libmp3lame -b:a 64k -f wav`), decoded whole as
//     ffmpeg's wav demuxer does (TestDecodeMP3WAV);
//
// each the A-law clip's token count (up to 1 more for the last ADPCM
// block's padding, 2 more for the MP3 frames' delay) and within cosine
// 0.97 of its vector. And an 8704x4352 video in H.264, H.265, VP9 and AV1
// (internal/embedding/testdata/over8k-*.mp4) is UNIMPLEMENTED naming its
// size, not INVALID_ARGUMENT.
func TestEmbedContentEmbeds2And5BitADPCMAndMP3WAVsThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	read := func(f string) []byte {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
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
	alaw := embed("A-law WAV", read("jfk-alaw.wav"))
	want := alaw.GetUsageMetadata().GetPromptTokenCount()
	for _, c := range []struct {
		file   string
		slack  int32
		cosine float64
	}{
		{"jfk-ima5.wav", 1, 0.97},
		{"jfk-ima2.wav", 1, 0.97},
		{"jfk-mp3.wav", 2, 0.97},
	} {
		r := embed(c.file, read(c.file))
		n := r.GetUsageMetadata().GetPromptTokenCount()
		if n < want || n > want+c.slack {
			t.Errorf("%s: %d tokens, the A-law clip's %d (up to %d more)", c.file, n, want, c.slack)
		}
		cs := cosine(r.GetEmbedding().GetValues(), alaw.GetEmbedding().GetValues())
		if cs < c.cosine {
			t.Errorf("%s against the A-law clip: cosine %.4f, want at least %.2f", c.file, cs, c.cosine)
		}
		t.Logf("%s: %d tokens (A-law %d), cosine %.6f to the A-law clip", c.file, n, want, cs)
	}

	// A video over 8K UHD is UNIMPLEMENTED in every codec, naming its size
	// (a VP9 frame wider than 8192, and H.265 and AV1 frames over their
	// decoders' limits, were INVALID_ARGUMENT).
	for _, c := range []string{"h264", "h265", "vp9", "av1"} {
		_, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name),
			Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_InlineData{
				InlineData: &aiplatformpb.Blob{MimeType: "video/mp4", Data: embeddingTestdata(t, "over8k-"+c+".mp4")}}}}},
		})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "8704x4352") {
			t.Errorf("an 8704x4352 %s video: %v; want Unimplemented naming its size", c, err)
		}
	}
}
