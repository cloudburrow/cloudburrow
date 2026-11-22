package embedding

import (
	"errors"
	"image"
	"testing"
	"time"
)

// TestMatroskaFramesMatchMP4 (#1651): each fixture is its MP4 or MOV
// remuxed by ffmpeg into Matroska or WebM (-c copy), so the demuxer must
// give the same samples at the same times, and the frames sampled from it
// must be the MP4's, pixel for pixel. turtle-vp9-live.webm is ffmpeg's
// -live 1 WebM, whose Segment and Clusters are of unknown size.
func TestMatroskaFramesMatchMP4(t *testing.T) {
	for _, c := range []struct{ mkv, mt, mp4, mp4mt string }{
		{"turtle-h264.mkv", "video/x-matroska", "turtle-h264.mp4", "video/mp4"},
		{"turtle-h265.mkv", "video/x-matroska", "turtle-h265.mov", "video/quicktime"},
		{"turtle-vp9.webm", "video/webm", "turtle-vp9.mp4", "video/mp4"},
		{"turtle-vp9-live.webm", "video/webm", "turtle-vp9.mp4", "video/mp4"},
		{"turtle-av1.webm", "video/webm", "turtle-av1.mp4", "video/mp4"},
	} {
		t.Run(c.mkv, func(t *testing.T) {
			mkv, ref := readTestdata(t, c.mkv), readTestdata(t, c.mp4)
			opt := VideoOptions{Matroska: true, FPS: 2}
			d, err := VideoDuration(c.mt, mkv, opt)
			if err != nil {
				t.Fatal(err)
			}
			dRef, err := VideoDuration(c.mp4mt, ref, VideoOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if diff := d - dRef; diff < -40*time.Millisecond || diff > 40*time.Millisecond {
				t.Errorf("duration %v, the MP4's %v", d, dRef)
			}
			got, err := DecodeVideo(c.mt, mkv, opt)
			if err != nil {
				t.Fatal(err)
			}
			want, err := DecodeVideo(c.mp4mt, ref, VideoOptions{FPS: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%d frames, the MP4 %d", len(got), len(want))
			}
			for i := range got {
				if !sameImage(got[i], want[i]) {
					t.Errorf("frame %d differs from the MP4's", i)
				}
			}
		})
	}
	// Without the option, Matroska is not a type EmbedContent takes.
	if _, err := DecodeVideo("video/webm", readTestdata(t, "turtle-vp9.webm"), VideoOptions{}); err == nil {
		t.Error("WebM decoded without VideoOptions.Matroska")
	}
	// A truncated file, and an EBML file of no video track.
	b := readTestdata(t, "turtle-h264.mkv")
	if _, err := DecodeVideo("video/x-matroska", b[:len(b)/3], VideoOptions{Matroska: true}); err == nil {
		t.Error("a truncated MKV decoded")
	}
	if _, err := DecodeVideo("video/webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, 0x18, 0x53, 0x80, 0x67, 0x80},
		VideoOptions{Matroska: true}); err == nil {
		t.Error("an empty Segment decoded")
	}
}

func TestMatroskaRefusesLacingAndUnknownCodecs(t *testing.T) {
	tr := &videoTrack{}
	if err := matroskaCodec(tr, &mkvTrack{codec: "V_THEORA"}); !errors.Is(err, ErrVideoCodec) {
		t.Errorf("Theora: %v", err)
	}
	// EBML header, then a Segment holding a Tracks of one VP9 video track
	// and a Cluster whose SimpleBlock is Xiph-laced.
	seg := []byte{
		0x16, 0x54, 0xAE, 0x6B, 0x8F, // Tracks
		0xAE, 0x8D, // TrackEntry
		0xD7, 0x81, 0x01, // TrackNumber 1
		0x83, 0x81, 0x01, // TrackType video
		0x86, 0x85, 'V', '_', 'V', 'P', '9',
		0x1F, 0x43, 0xB6, 0x75, 0x8A, // Cluster
		0xE7, 0x81, 0x00, // Timecode 0
		0xA3, 0x85, 0x81, 0x00, 0x00, 0x82, 0x00, // SimpleBlock track 1, Xiph lacing
	}
	b := append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, 0x18, 0x53, 0x80, 0x67, 0x80 | byte(len(seg))}, seg...)
	if _, err := readMatroskaTrack(b); !errors.Is(err, ErrNotDecoded) {
		t.Errorf("laced: %v, want ErrNotDecoded", err)
	}
}

func sameImage(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				return false
			}
		}
	}
	return true
}
