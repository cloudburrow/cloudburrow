//go:build compat

package compat

import (
	"bytes"
	"context"
	"math"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/Eyevinn/mp4ff/mp4"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/isoconformance"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsAFullParametricStereoTrackThroughThePredictionClient
// (#1754) embeds a video whose audio track is HE-AAC v2 with the full
// parametric stereo tool, through the official aiplatform PredictionClient's
// EmbedContent with embed_content_config.audio_track_extraction. The track
// is ISO/IEC 14496-26's conformance stream al_sbr_ps_06 (34 stereo bands,
// switching between the 20- and 34-band configurations, 32 kHz output),
// which AudioToolbox's encoder, the one at hand, cannot make (it emits
// baseline PS only). ISO's files are never committed: internal/isoconformance
// fetches the stream and its unrestricted-PS reference decode at test time
// (pinned by SHA-256 and ISO's MD5; the test skips when they are not cached
// and CLOUDBURROW_TEST_DOWNLOADS is unset), and the test muxes the stream as
// the audio track of testdata/turtle-jfk.mp4's video. The vector is the one
// of the video's frames followed by ISO's reference WAV (24-bit) as a part of
// its own, within 0.99, with the same tokens: the track decodes as ISO's
// reference decoder decodes it. Before #1754 the request was refused
// (UNIMPLEMENTED, 34 stereo bands).
func TestEmbedContentEmbedsAFullParametricStereoTrackThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	stream := isoconformance.File(t, "al_sbr_ps_06.mp4")
	ref := isoconformance.File(t, "al_sbr_ps_06_ur.wav")
	turtle, err := os.ReadFile("testdata/turtle-jfk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	video := muxAudioTrack(t, turtle, stream)
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}
	embed := func(track bool, parts ...*aiplatformpb.Part) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{AudioTrackExtraction: proto.Bool(track)},
		})
		if err != nil {
			t.Fatalf("EmbedContent: %v", err)
		}
		return r
	}

	frames := embed(false, inline("video/mp4", video))
	withTrack := embed(true, inline("video/mp4", video))
	v := withTrack.GetEmbedding().GetValues()
	if len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
		t.Fatalf("with the audio track: %d values, norm %.6f", len(v), norm(v))
	}
	parts := embed(false, inline("video/mp4", video), inline("audio/wav", ref))
	nf, nt, np := frames.GetUsageMetadata().GetPromptTokenCount(), withTrack.GetUsageMetadata().GetPromptTokenCount(),
		parts.GetUsageMetadata().GetPromptTokenCount()
	// al_sbr_ps_06 decodes to 13.6 seconds: 339 soft tokens and the two
	// audio markers. The reference WAV is the decoder's whole output; the
	// stream's edit list may trim its start, a token or two.
	if nt < nf+330 || nt > np || np-nt > 2 {
		t.Errorf("tokens: %d with the audio track, %d without, %d with the reference WAV as a part", nt, nf, np)
	}
	c := cosine(v, parts.GetEmbedding().GetValues())
	if c < 0.99 {
		t.Errorf("against the video and ISO's reference WAV as two parts: cosine %.4f, want at least 0.99", c)
	}
	t.Logf("tokens %d without the track, %d with, %d with the reference WAV; cosine %.6f to the video+WAV vector, %.6f to the frames'",
		nf, nt, np, c, cosine(v, frames.GetEmbedding().GetValues()))
}

// muxAudioTrack returns video (a progressive MP4 with one mdat) with its
// sound track replaced by audio's (likewise), written as ftyp, mdat (the
// video's media data, then the audio's) and moov, every chunk offset moved
// to its new place. The audio track's durations are rescaled to the video's
// movie timescale.
func muxAudioTrack(t *testing.T, video, audio []byte) []byte {
	t.Helper()
	decode := func(b []byte) (*mp4.File, []byte, uint64) {
		f, err := mp4.DecodeFile(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range f.Children {
			if c.Type() == "mdat" {
				n++
			}
		}
		if f.Moov == nil || f.Mdat == nil || n != 1 {
			t.Fatalf("muxAudioTrack: want a progressive MP4 with one mdat")
		}
		payload := f.Mdat.Data
		return f, payload, f.Mdat.StartPos + f.Mdat.Size() - uint64(len(payload))
	}
	vf, vdata, vstart := decode(video)
	af, adata, astart := decode(audio)
	handler := func(tr *mp4.TrakBox) string {
		if tr.Mdia == nil || tr.Mdia.Hdlr == nil {
			return ""
		}
		return tr.Mdia.Hdlr.HandlerType
	}
	var vtrak, atrak *mp4.TrakBox
	for _, tr := range vf.Moov.Traks {
		if handler(tr) == "vide" && vtrak == nil {
			vtrak = tr
		}
	}
	for _, tr := range af.Moov.Traks {
		if handler(tr) == "soun" && atrak == nil {
			atrak = tr
		}
	}
	if vtrak == nil || atrak == nil {
		t.Fatal("muxAudioTrack: no video track or no sound track")
	}

	var ftyp bytes.Buffer
	if err := vf.Ftyp.Encode(&ftyp); err != nil {
		t.Fatal(err)
	}
	mdat := &mp4.MdatBox{}
	mdat.SetData(append(append([]byte{}, vdata...), adata...))
	newStart := uint64(ftyp.Len()) + mdat.Size() - uint64(len(vdata)+len(adata))
	move := func(tr *mp4.TrakBox, from, to uint64) {
		stbl := tr.Mdia.Minf.Stbl
		switch {
		case stbl.Stco != nil:
			for i, o := range stbl.Stco.ChunkOffset {
				stbl.Stco.ChunkOffset[i] = uint32(uint64(o) - from + to)
			}
		case stbl.Co64 != nil:
			for i, o := range stbl.Co64.ChunkOffset {
				stbl.Co64.ChunkOffset[i] = o - from + to
			}
		default:
			t.Fatal("muxAudioTrack: a track without chunk offsets")
		}
	}
	move(vtrak, vstart, newStart)
	move(atrak, astart, newStart+uint64(len(vdata)))

	mvhd := vf.Moov.Mvhd
	rescale := func(d uint64) uint64 { return d * uint64(mvhd.Timescale) / uint64(af.Moov.Mvhd.Timescale) }
	atrak.Tkhd.TrackID = mvhd.NextTrackID
	mvhd.NextTrackID++
	atrak.Tkhd.Duration = rescale(atrak.Tkhd.Duration)
	if atrak.Edts != nil {
		for _, elst := range atrak.Edts.Elst {
			for i := range elst.Entries {
				elst.Entries[i].SegmentDuration = rescale(elst.Entries[i].SegmentDuration)
			}
		}
	}
	moov := mp4.NewMoovBox()
	moov.AddChild(mvhd)
	moov.AddChild(vtrak)
	moov.AddChild(atrak)

	out := bytes.NewBuffer(ftyp.Bytes())
	if err := mdat.Encode(out); err != nil {
		t.Fatal(err)
	}
	if err := moov.Encode(out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
