package embedding

import (
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
//	ffmpeg -t 3 -i sea-turtle.mp4 -vf scale=320:180 -an -c:v libvpx-vp9 -crf 48 -b:v 0 -g 60 -auto-alt-ref 1 -lag-in-frames 16 -pass 1 -f null - (then -pass 2) -map_metadata -1 turtle-vp9.mp4
//	ffmpeg -t 3 -i sea-turtle.mp4 -vf scale=320:180 -an -c:v libsvtav1 -crf 50 -g 60 -map_metadata -1 turtle-av1.mp4
//	ffmpeg -i turtle-h264.mp4 -c:v libvpx-vp9 -pix_fmt yuv444p -b:v 150k -pass 1 -an -f null - (then -pass 2) -map_metadata -1 turtle-vp9-444.mp4
//	ffmpeg -i turtle-h264.mp4 -c:v libvpx-vp9 -pix_fmt yuv420p10le -b:v 150k -pass 1 -an -f null - (then -pass 2) -map_metadata -1 turtle-vp9-10bit.mp4
//	ffmpeg -i turtle-h264.mp4 -c:v libvpx-vp9 -pix_fmt yuv422p12le -b:v 150k -pass 1 -an -f null - (then -pass 2) -map_metadata -1 turtle-vp9-422-12bit.mp4
//	ffmpeg -t 1 -i sea-turtle.mp4 -vf scale=64:36 -an -r 2 -c:v libsvtav1 -pix_fmt yuv420p10le -crf 60 -map_metadata -1 turtle-av1-10bit.mp4
//
// (The three VP9 profile 1 to 3 clips, #1739, are re-encoded from the H.264
// one.) All the 3-second ones are 59.94 frames a second; the H.264 and H.265 ones
// have B-frames, the H.265 one an open GOP (a CRA at 1 s with RASL pictures
// before it), the VP9 one hidden alternate reference frames in superframes,
// the AV1 one SVT-AV1's hierarchical random access.

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
	"turtle-h264.mp4":          {{{14.9, 132.6, 219.2}, {16.0, 137.2, 233.7}, {15.4, 130.9, 236.0}, {14.0, 112.8, 228.6}, {16.4, 136.8, 176.3}, {19.0, 149.5, 188.3}, {16.4, 134.8, 203.5}, {13.8, 110.3, 202.0}, {25.8, 123.7, 146.0}, {23.1, 126.9, 147.3}, {19.2, 144.1, 159.5}, {19.5, 118.7, 146.9}, {33.1, 113.5, 130.7}, {26.7, 114.4, 129.5}, {21.2, 87.5, 108.3}, {26.1, 110.6, 126.8}}, {{14.9, 131.1, 215.2}, {15.9, 135.5, 229.2}, {15.4, 128.3, 234.5}, {14.3, 112.4, 227.1}, {18.1, 138.8, 171.3}, {19.2, 151.0, 183.1}, {16.5, 138.4, 200.6}, {13.6, 110.0, 203.8}, {29.9, 120.0, 140.7}, {25.5, 122.9, 143.1}, {20.1, 137.7, 150.4}, {18.7, 115.5, 148.2}, {38.2, 117.5, 131.3}, {31.3, 140.6, 149.5}, {23.5, 91.5, 110.1}, {26.8, 116.4, 129.9}}, {{15.1, 128.2, 216.8}, {16.0, 133.6, 232.5}, {15.0, 130.1, 237.1}, {14.5, 117.2, 230.3}, {17.6, 137.9, 177.6}, {18.0, 150.1, 189.2}, {15.6, 133.3, 205.2}, {14.4, 110.8, 208.1}, {27.1, 124.4, 144.4}, {23.3, 128.4, 147.8}, {19.9, 144.5, 160.4}, {18.5, 113.6, 152.0}, {36.4, 109.0, 125.2}, {28.7, 122.2, 133.8}, {21.8, 89.7, 108.0}, {24.9, 112.7, 128.4}}},
	"turtle-vp9.mp4":           {{{15.6, 132.4, 219.4}, {15.8, 137.0, 233.8}, {15.4, 130.2, 237.4}, {13.8, 113.1, 228.3}, {16.1, 136.9, 175.8}, {17.8, 150.2, 187.4}, {15.5, 135.1, 202.9}, {13.5, 110.0, 202.3}, {24.6, 123.9, 145.6}, {23.1, 126.9, 148.0}, {18.7, 144.5, 159.6}, {18.9, 119.1, 146.6}, {32.5, 113.9, 130.1}, {26.1, 114.7, 128.6}, {20.0, 88.0, 108.1}, {25.2, 110.7, 125.7}}, {{15.1, 130.4, 213.5}, {16.0, 134.6, 228.1}, {14.9, 127.4, 234.9}, {14.3, 111.9, 227.3}, {17.8, 138.1, 171.3}, {18.0, 150.6, 182.8}, {16.1, 138.1, 198.6}, {13.9, 109.5, 202.5}, {29.1, 119.0, 139.3}, {24.0, 121.7, 141.9}, {19.8, 137.2, 149.5}, {18.1, 115.2, 148.5}, {38.5, 117.3, 130.2}, {29.8, 140.5, 148.7}, {22.9, 91.7, 110.0}, {25.6, 117.0, 128.7}}, {{15.2, 128.6, 218.8}, {16.4, 133.6, 233.5}, {15.4, 129.7, 237.4}, {14.6, 116.3, 230.0}, {15.9, 138.1, 177.9}, {17.4, 149.2, 189.3}, {14.8, 133.0, 205.4}, {14.1, 110.4, 208.1}, {25.1, 124.7, 143.9}, {22.2, 128.7, 147.2}, {18.1, 145.3, 159.5}, {17.8, 113.8, 151.6}, {35.0, 110.7, 124.9}, {27.3, 123.3, 134.4}, {20.1, 89.8, 107.6}, {23.5, 112.8, 128.7}}},
	"turtle-av1.mp4":           {{{15.6, 132.7, 219.8}, {16.3, 136.9, 233.7}, {15.7, 130.3, 237.5}, {14.1, 113.2, 228.3}, {16.6, 137.3, 176.0}, {18.0, 150.8, 187.5}, {16.0, 135.1, 203.8}, {13.9, 110.3, 202.1}, {24.9, 124.0, 146.0}, {23.3, 127.5, 148.1}, {18.5, 144.7, 159.9}, {18.7, 119.5, 146.7}, {32.8, 114.3, 130.9}, {26.3, 115.1, 129.1}, {20.6, 88.1, 108.0}, {25.1, 110.7, 125.8}}, {{15.0, 131.1, 214.6}, {16.2, 135.3, 229.4}, {14.9, 127.9, 235.1}, {13.2, 112.4, 227.5}, {17.7, 139.0, 171.8}, {18.3, 151.3, 182.3}, {16.0, 138.7, 198.6}, {13.6, 109.5, 203.6}, {29.9, 119.5, 140.2}, {23.7, 123.0, 142.1}, {19.9, 137.8, 149.9}, {17.8, 116.0, 149.2}, {38.7, 116.6, 130.4}, {29.6, 141.1, 149.0}, {22.6, 91.9, 110.1}, {26.1, 117.3, 129.6}}, {{15.0, 128.5, 218.5}, {16.5, 133.7, 233.3}, {15.3, 130.3, 237.8}, {14.4, 117.3, 230.7}, {16.2, 138.8, 178.5}, {17.5, 150.2, 189.9}, {15.3, 133.2, 206.5}, {14.3, 110.4, 208.8}, {25.0, 124.6, 144.4}, {22.5, 128.6, 147.2}, {18.6, 145.4, 159.8}, {18.0, 114.4, 152.6}, {35.1, 110.3, 124.5}, {28.0, 123.0, 133.5}, {20.5, 89.5, 107.5}, {23.5, 113.6, 128.1}}},
	"turtle-h265.mov":          {{{14.8, 132.5, 220.1}, {15.5, 136.7, 234.3}, {15.6, 130.0, 238.3}, {14.1, 113.2, 228.6}, {15.6, 137.0, 175.7}, {18.5, 149.2, 188.4}, {15.1, 135.2, 203.3}, {12.7, 110.1, 203.1}, {25.0, 123.5, 145.9}, {22.7, 127.4, 149.3}, {18.6, 144.4, 159.7}, {19.0, 118.9, 146.4}, {32.0, 113.7, 129.9}, {25.5, 115.3, 128.1}, {20.4, 87.9, 107.6}, {25.6, 110.2, 126.4}}, {{14.8, 130.6, 213.0}, {16.9, 134.6, 228.6}, {15.5, 127.7, 235.0}, {13.0, 112.1, 225.6}, {17.4, 138.5, 171.6}, {18.8, 150.5, 183.0}, {15.9, 137.9, 199.2}, {13.3, 108.7, 203.5}, {29.1, 118.8, 139.5}, {23.2, 122.4, 142.5}, {18.5, 137.9, 148.0}, {17.3, 115.2, 146.2}, {38.2, 116.7, 130.8}, {30.6, 140.9, 148.8}, {22.8, 91.9, 109.6}, {25.6, 116.7, 128.4}}, {{15.4, 128.3, 219.0}, {17.0, 133.7, 234.1}, {16.6, 130.8, 238.4}, {15.2, 117.4, 230.2}, {16.8, 138.0, 179.6}, {17.4, 150.0, 189.7}, {15.8, 133.7, 207.7}, {14.5, 110.9, 209.8}, {26.1, 124.8, 143.5}, {22.2, 128.4, 148.1}, {18.2, 145.3, 158.8}, {18.0, 115.1, 154.1}, {34.7, 109.7, 123.4}, {27.5, 122.1, 133.3}, {21.0, 88.5, 106.7}, {24.8, 114.0, 127.8}}},
	"turtle-vp9-444.mp4":       {{{14.9, 132.6, 219.2}, {16.0, 137.2, 233.8}, {15.2, 130.9, 236.1}, {14.0, 112.8, 228.6}, {16.3, 136.7, 176.4}, {19.1, 149.5, 188.3}, {16.4, 134.9, 203.6}, {13.8, 110.4, 202.0}, {25.8, 123.7, 146.0}, {23.2, 126.9, 147.4}, {19.1, 144.2, 159.6}, {19.6, 118.7, 147.0}, {33.1, 113.5, 130.7}, {26.7, 114.5, 129.6}, {21.2, 87.5, 108.4}, {26.0, 110.7, 126.9}}, {{15.7, 131.0, 215.4}, {16.6, 135.4, 229.6}, {15.5, 128.7, 235.9}, {14.1, 113.6, 228.2}, {18.4, 138.4, 172.9}, {19.2, 150.9, 184.8}, {17.0, 138.4, 201.7}, {14.0, 110.4, 205.3}, {29.7, 119.2, 141.3}, {25.0, 122.9, 143.4}, {20.9, 137.1, 150.9}, {19.0, 115.9, 149.6}, {39.1, 116.6, 131.5}, {30.4, 141.2, 150.1}, {23.9, 91.5, 111.5}, {26.9, 115.8, 130.8}}, {{15.0, 128.7, 217.6}, {15.9, 133.4, 232.7}, {15.2, 129.7, 237.4}, {14.3, 117.3, 230.5}, {17.3, 138.0, 178.2}, {17.8, 149.9, 189.5}, {15.9, 133.2, 205.7}, {14.3, 110.6, 208.0}, {26.8, 124.3, 145.4}, {22.9, 128.1, 148.0}, {19.7, 144.4, 160.4}, {18.6, 113.2, 152.2}, {36.7, 109.5, 125.7}, {29.0, 122.7, 134.6}, {21.8, 89.7, 109.0}, {25.1, 112.9, 129.5}}},
	"turtle-vp9-10bit.mp4":     {{{14.9, 132.5, 219.3}, {16.0, 137.3, 233.7}, {15.3, 130.9, 235.9}, {14.0, 112.9, 228.6}, {16.3, 136.8, 176.2}, {18.9, 149.5, 188.3}, {16.3, 134.9, 203.6}, {13.7, 110.3, 201.9}, {25.8, 123.8, 146.0}, {23.1, 126.9, 147.3}, {19.0, 144.2, 159.6}, {19.6, 118.6, 147.1}, {33.1, 113.5, 130.8}, {26.7, 114.4, 129.6}, {21.1, 87.5, 108.2}, {26.0, 110.7, 126.7}}, {{15.3, 131.0, 215.6}, {16.2, 135.9, 228.3}, {14.9, 128.7, 235.3}, {13.8, 113.5, 228.0}, {17.5, 138.5, 171.9}, {18.8, 150.7, 183.8}, {16.3, 139.0, 199.8}, {14.1, 110.2, 204.0}, {28.8, 119.2, 140.4}, {24.7, 122.5, 142.4}, {20.5, 136.9, 150.2}, {18.5, 115.4, 148.5}, {38.8, 116.4, 131.3}, {29.9, 141.1, 149.7}, {23.5, 90.6, 111.1}, {26.4, 116.3, 130.7}}, {{15.4, 128.3, 217.6}, {16.4, 133.5, 233.1}, {15.2, 130.0, 236.8}, {14.4, 117.0, 230.3}, {17.1, 137.7, 178.2}, {18.4, 150.0, 190.7}, {15.5, 133.1, 206.3}, {14.2, 110.3, 208.2}, {26.9, 124.3, 145.5}, {22.6, 128.0, 147.7}, {19.3, 144.8, 160.4}, {18.0, 113.5, 151.9}, {36.2, 109.1, 125.9}, {28.2, 122.5, 134.2}, {21.6, 89.7, 108.6}, {25.0, 112.3, 128.9}}},
	"turtle-vp9-422-12bit.mp4": {{{15.0, 132.5, 219.2}, {16.1, 137.2, 233.8}, {15.4, 130.9, 235.8}, {14.0, 113.0, 228.5}, {16.3, 136.8, 176.3}, {19.0, 149.5, 188.4}, {16.3, 134.9, 203.6}, {13.7, 110.4, 202.1}, {25.7, 123.7, 146.1}, {23.1, 126.8, 147.4}, {19.0, 144.1, 159.6}, {19.4, 118.7, 146.9}, {33.1, 113.4, 130.8}, {26.6, 114.4, 129.6}, {21.1, 87.5, 108.3}, {26.0, 110.6, 126.8}}, {{14.9, 130.8, 214.6}, {16.4, 135.3, 227.7}, {15.0, 128.9, 234.7}, {14.0, 113.0, 227.5}, {17.3, 138.2, 171.2}, {18.4, 150.6, 183.4}, {16.2, 139.1, 199.7}, {13.6, 110.5, 204.1}, {29.3, 119.5, 140.1}, {24.4, 122.7, 142.3}, {20.1, 137.3, 149.6}, {18.5, 115.3, 148.5}, {38.4, 116.7, 130.9}, {29.9, 141.2, 149.0}, {23.4, 90.8, 110.5}, {26.3, 116.3, 130.0}}, {{15.4, 128.4, 217.8}, {16.0, 133.7, 232.8}, {15.1, 130.2, 237.4}, {14.6, 116.8, 230.3}, {17.2, 137.7, 178.1}, {18.0, 150.2, 189.8}, {15.4, 132.9, 205.8}, {14.4, 110.4, 208.2}, {26.7, 124.3, 145.2}, {22.8, 128.0, 147.4}, {19.5, 144.8, 160.3}, {18.1, 113.3, 151.9}, {36.3, 109.2, 125.4}, {28.7, 122.8, 134.0}, {21.4, 90.1, 108.4}, {24.2, 112.6, 128.3}}},
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

// TestDecodeVideoFrames decodes each clip at 1 frame a second against
// ffmpeg's frames. Each codec's clip also checks that an interval and a
// rate pick the same frames; the VP9 profile 1 to 3 clips (options false)
// share profile 0's demuxing and frame choice, and TestVP9ProfilesMatchFFmpeg
// checks every one of their frames bit for bit, so they are not decoded
// three times more under the race detector (#1761). The clips decode in
// parallel.
func TestDecodeVideoFrames(t *testing.T) {
	for _, c := range []struct {
		file, mt string
		options  bool
	}{
		{"turtle-h264.mp4", "video/mp4", true},
		{"turtle-h265.mov", "video/quicktime", true},
		{"turtle-vp9.mp4", "video/mp4", true},
		{"turtle-av1.mp4", "video/mp4", true},
		{"turtle-vp9-444.mp4", "video/mp4", false},
		{"turtle-vp9-10bit.mp4", "video/mp4", false},
		{"turtle-vp9-422-12bit.mp4", "video/mp4", false},
	} {
		t.Run(c.file, func(t *testing.T) {
			t.Parallel()
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
			if !c.options {
				return
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
		})
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
