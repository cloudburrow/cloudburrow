package embedding

import (
	"errors"
	"image"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// The videos in testdata are the first seconds of the model card's
// sea-turtle.mp4 (Transformers.js's documentation dataset, the clip
// test/compat's TestEmbedContentEmbedsVideo pins), scaled down and
// re-encoded with ffmpeg 9.0.2:
//
//	ffmpeg -t 3 -i sea-turtle.mp4 -vf scale=320:180 -an -c:v libx264 -preset slow -crf 30 -bf 2 -g 60 -map_metadata -1 -fflags +bitexact turtle-h264.mp4
//	ffmpeg -t 3 -i sea-turtle.mp4 -vf scale=320:180 -an -c:v libx265 -preset fast -crf 32 -tag:v hvc1 -x265-params log-level=error:keyint=60 -map_metadata -1 -fflags +bitexact turtle-h265.mov
//	ffmpeg -t 1 -i sea-turtle.mp4 -vf scale=64:36 -an -r 2 -c:v libvpx-vp9 -crf 50 -b:v 0 -map_metadata -1 turtle-vp9.mp4
//	ffmpeg -t 1 -i sea-turtle.mp4 -vf scale=64:36 -an -r 2 -c:v libsvtav1 -crf 60 -map_metadata -1 turtle-av1.mp4
//
// Both are 59.94 frames a second with B-frames; the H.265 one has an open
// GOP (a CRA at 1 s with RASL pictures before it).

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantBlocks are ffmpeg's RGB of frames floor(i * 59.94), i = 0, 1, 2, of
// each test video (select=eq(n\\,N), scale=in_color_matrix=bt601:in_range=tv
// with accurate_rnd and full_chroma_int), as the mean of each of 4x4 blocks.
// The decoders are bit-exact with ffmpeg's (70 dB PSNR on the model card's
// own clip at full size), so only the YUV-to-RGB rounding differs.
var wantBlocks = map[string][3][16][3]float64{
	"turtle-h264.mp4": {{{14.9, 132.6, 219.2}, {16.0, 137.2, 233.7}, {15.4, 130.9, 236.0}, {14.0, 112.8, 228.6}, {16.4, 136.8, 176.3}, {19.0, 149.5, 188.3}, {16.4, 134.8, 203.5}, {13.8, 110.3, 202.0}, {25.8, 123.7, 146.0}, {23.1, 126.9, 147.3}, {19.2, 144.1, 159.5}, {19.5, 118.7, 146.9}, {33.1, 113.5, 130.7}, {26.7, 114.4, 129.5}, {21.2, 87.5, 108.3}, {26.1, 110.6, 126.8}}, {{14.9, 131.1, 215.2}, {15.9, 135.5, 229.2}, {15.4, 128.3, 234.5}, {14.3, 112.4, 227.1}, {18.1, 138.8, 171.3}, {19.2, 151.0, 183.1}, {16.5, 138.4, 200.6}, {13.6, 110.0, 203.8}, {29.9, 120.0, 140.7}, {25.5, 122.9, 143.1}, {20.1, 137.7, 150.4}, {18.7, 115.5, 148.2}, {38.2, 117.5, 131.3}, {31.3, 140.6, 149.5}, {23.5, 91.5, 110.1}, {26.8, 116.4, 129.9}}, {{15.1, 128.2, 216.8}, {16.0, 133.6, 232.5}, {15.0, 130.1, 237.1}, {14.5, 117.2, 230.3}, {17.6, 137.9, 177.6}, {18.0, 150.1, 189.2}, {15.6, 133.3, 205.2}, {14.4, 110.8, 208.1}, {27.1, 124.4, 144.4}, {23.3, 128.4, 147.8}, {19.9, 144.5, 160.4}, {18.5, 113.6, 152.0}, {36.4, 109.0, 125.2}, {28.7, 122.2, 133.8}, {21.8, 89.7, 108.0}, {24.9, 112.7, 128.4}}},
	"turtle-h265.mov": {{{14.8, 132.5, 220.1}, {15.5, 136.7, 234.3}, {15.6, 130.0, 238.3}, {14.1, 113.2, 228.6}, {15.6, 137.0, 175.7}, {18.5, 149.2, 188.4}, {15.1, 135.2, 203.3}, {12.7, 110.1, 203.1}, {25.0, 123.5, 145.9}, {22.7, 127.4, 149.3}, {18.6, 144.4, 159.7}, {19.0, 118.9, 146.4}, {32.0, 113.7, 129.9}, {25.5, 115.3, 128.1}, {20.4, 87.9, 107.6}, {25.6, 110.2, 126.4}}, {{14.8, 130.6, 213.0}, {16.9, 134.6, 228.6}, {15.5, 127.7, 235.0}, {13.0, 112.1, 225.6}, {17.4, 138.5, 171.6}, {18.8, 150.5, 183.0}, {15.9, 137.9, 199.2}, {13.3, 108.7, 203.5}, {29.1, 118.8, 139.5}, {23.2, 122.4, 142.5}, {18.5, 137.9, 148.0}, {17.3, 115.2, 146.2}, {38.2, 116.7, 130.8}, {30.6, 140.9, 148.8}, {22.8, 91.9, 109.6}, {25.6, 116.7, 128.4}}, {{15.4, 128.3, 219.0}, {17.0, 133.7, 234.1}, {16.6, 130.8, 238.4}, {15.2, 117.4, 230.2}, {16.8, 138.0, 179.6}, {17.4, 150.0, 189.7}, {15.8, 133.7, 207.7}, {14.5, 110.9, 209.8}, {26.1, 124.8, 143.5}, {22.2, 128.4, 148.1}, {18.2, 145.3, 158.8}, {18.0, 115.1, 154.1}, {34.7, 109.7, 123.4}, {27.5, 122.1, 133.3}, {21.0, 88.5, 106.7}, {24.8, 114.0, 127.8}}},
}

func blockMeans(img image.Image) [16][3]float64 {
	var out [16][3]float64
	b := img.Bounds()
	bw, bh := b.Dx()/4, b.Dy()/4
	for by := 0; by < 4; by++ {
		for bx := 0; bx < 4; bx++ {
			var s [3]float64
			for y := by * bh; y < (by+1)*bh; y++ {
				for x := bx * bw; x < (bx+1)*bw; x++ {
					r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
					s[0] += float64(r >> 8)
					s[1] += float64(g >> 8)
					s[2] += float64(bl >> 8)
				}
			}
			for k := range s {
				out[by*4+bx][k] = s[k] / float64(bw*bh)
			}
		}
	}
	return out
}

func TestDecodeVideoFrames(t *testing.T) {
	for _, c := range []struct{ file, mt string }{
		{"turtle-h264.mp4", "video/mp4"},
		{"turtle-h265.mov", "video/quicktime"},
	} {
		data := readTestdata(t, c.file)
		frames, err := DecodeVideo(c.mt, data, VideoOptions{})
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if len(frames) != 3 {
			t.Fatalf("%s: %d frames, want 3 (1 a second of 3 seconds)", c.file, len(frames))
		}
		for i, f := range frames {
			if b := f.Bounds(); b.Dx() != 320 || b.Dy() != 180 {
				t.Errorf("%s: frame %d is %v", c.file, i, b)
			}
			got := blockMeans(f)
			for j := range got {
				for k := range got[j] {
					if d := math.Abs(got[j][k] - wantBlocks[c.file][i][j][k]); d > 0.6 {
						t.Fatalf("%s: frame %d block %d channel %d is %.1f, ffmpeg's %.1f", c.file, i, j, k, got[j][k], wantBlocks[c.file][i][j][k])
					}
				}
			}
		}
		// The interval and the rate pick the same frames.
		one, err := DecodeVideo(c.mt, data, VideoOptions{Start: time.Second, End: 2 * time.Second})
		if err != nil || len(one) != 1 || blockMeans(one[0]) != blockMeans(frames[1]) {
			t.Errorf("%s: 1s to 2s: %d frames, %v; want the frame at 1s", c.file, len(one), err)
		}
		two, err := DecodeVideo(c.mt, data, VideoOptions{FPS: 2})
		if err != nil || len(two) != 6 || blockMeans(two[2]) != blockMeans(frames[1]) || blockMeans(two[4]) != blockMeans(frames[2]) {
			t.Errorf("%s: at 2 fps: %d frames, %v; want 6, every other one the 1 fps frame", c.file, len(two), err)
		}
	}
}

func TestDecodeVideoRefuses(t *testing.T) {
	h264 := readTestdata(t, "turtle-h264.mp4")
	for _, c := range []struct {
		name, mt string
		data     []byte
		opt      VideoOptions
		want     string
	}{
		{"VP9", "video/mp4", readTestdata(t, "turtle-vp9.mp4"), VideoOptions{}, "VP9"},
		{"AV1", "video/mp4", readTestdata(t, "turtle-av1.mp4"), VideoOptions{}, "AV1"},
		{"WebM", "video/webm", h264, VideoOptions{}, "not supported"},
		{"not a video", "video/mp4", []byte("hello world, not a video"), VideoOptions{}, "not a valid MP4"},
		{"truncated", "video/mp4", h264[:len(h264)/2], VideoOptions{}, ""},
		{"empty", "video/mp4", nil, VideoOptions{}, "empty"},
		{"too many frames", "video/mp4", h264, VideoOptions{FPS: 60}, "at most 120 frames"},
		{"end before start", "video/mp4", h264, VideoOptions{Start: 2 * time.Second, End: time.Second}, "after start_offset"},
		{"start past the end", "video/mp4", h264, VideoOptions{Start: 5 * time.Second}, "past the video's end"},
		{"negative fps", "video/mp4", h264, VideoOptions{FPS: -1}, "positive"},
	} {
		_, err := DecodeVideo(c.mt, c.data, c.opt)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.name, err, c.want)
		}
		if (c.name == "VP9" || c.name == "AV1") && !errors.Is(err, ErrVideoCodec) {
			t.Errorf("%s: %v is not ErrVideoCodec", c.name, err)
		}
	}
}

// TestVideoFramesAndPrompt: more than 32 frames are taken uniformly, as
// np.linspace(0, n-1, 32).astype(int) picks them, and a video's place in the
// prompt is the video placeholder.
func TestVideoFramesAndPrompt(t *testing.T) {
	frames := make([]image.Image, 120)
	for i := range frames {
		frames[i] = image.NewGray(image.Rect(0, 0, i+1, 1))
	}
	got := VideoFrames(frames)
	if len(got) != 32 {
		t.Fatalf("%d frames, want 32", len(got))
	}
	// linspace(0, 119, 32) = 0, 3.839, 7.677, ... 119
	for i, want := range map[int]int{0: 0, 1: 3, 2: 7, 16: 61, 30: 115, 31: 119} {
		if w := got[i].Bounds().Dx() - 1; w != want {
			t.Errorf("frame %d is input frame %d, want %d", i, w, want)
		}
	}
	if len(VideoFrames(frames[:32])) != 32 {
		t.Errorf("32 frames are not all kept")
	}
	p, n, err := MultimodalPrompt(Input{Parts: []Part{{Text: "a turtle"}, {Video: frames[:2]}}})
	if err != nil || n != 1 || p != "task: search result | query: a turtle<|video|>" {
		t.Errorf("prompt %q, %d media, %v", p, n, err)
	}
	ip, err := PatchVideoFrame(image.NewRGBA(image.Rect(0, 0, 1280, 720)))
	if err != nil || ip.SoftTokens > videoSoftTokens || ip.SoftTokens < videoSoftTokens-20 {
		t.Errorf("a 720p frame is %d soft tokens (%v), want at most %d", ip.SoftTokens, err, videoSoftTokens)
	}
}
