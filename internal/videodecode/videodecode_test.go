package videodecode_test

import (
	"context"
	"errors"
	"image"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/videodecode"
)

func TestTag(t *testing.T) {
	if !regexp.MustCompile(`^cloudburrow/videodecode:[0-9a-f]{16}$`).MatchString(videodecode.Tag()) {
		t.Fatalf("Tag() = %q", videodecode.Tag())
	}
}

// Without Docker the decoder is unavailable, not the request's fault.
func TestNoDocker(t *testing.T) {
	d := &videodecode.Docker{Docker: filepath.Join(t.TempDir(), "no-docker")}
	src := d.Source(context.Background())
	defer src.Close()
	_, err := src.ProbeFrames([]byte("RIFF\x00\x00\x00\x00AVI "))
	if !errors.Is(err, embedding.ErrDecoderUnavailable) {
		t.Fatalf("ProbeFrames without docker: %v, want ErrDecoderUnavailable", err)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "embedding", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFramesMatchTheMP4 decodes AVI, FLV, MPEG and WMV fixtures, each made
// from turtle-h264.mp4 with ffmpeg 9 (an H.264 stream copied into AVI and
// FLV; MPEG-2, MPEG-4 Part 2, Sorenson Spark and WMV 8 re-encoded at
// -q:v 6), with the decoder image, and compares the frames sampled at
// 1 fps and at 4 fps with the MP4's, decoded in process: the copied
// streams pixel for pixel, the re-encoded ones within their encoders'
// loss (PSNR over 30 dB). At 4 fps a sample time falls within a
// millisecond of a frame's start (the clip is 59.94 fps), where AVI's
// frame-rate timestamps and FLV's milliseconds round onto the next frame,
// so a copied frame may equal the MP4's next one instead. The image must be built or buildable:
// CLOUDBURROW_VIDEODECODE_IMAGE names its tag (Tag() builds the
// Dockerfile).
func TestFramesMatchTheMP4(t *testing.T) {
	tag := os.Getenv("CLOUDBURROW_VIDEODECODE_IMAGE")
	if tag == "" {
		t.Skip("CLOUDBURROW_VIDEODECODE_IMAGE is not set")
	}
	d := &videodecode.Docker{Image: tag, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), videodecode.BuildTimeout)
	defer cancel()
	if err := d.Build(ctx); err != nil {
		t.Fatal(err)
	}
	mp4 := readTestdata(t, "turtle-h264.mp4")
	for _, fps := range []float64{1, 4} {
		opt := embedding.VideoOptions{FPS: fps}
		want, err := embedding.DecodeVideo("video/mp4", mp4, opt)
		if err != nil {
			t.Fatal(err)
		}
		next := opt
		next.Start = 16683 * time.Microsecond // one frame of 59.94 fps
		wantNext, err := embedding.DecodeVideo("video/mp4", mp4, next)
		if err != nil {
			t.Fatal(err)
		}
		wantDur, err := embedding.VideoDuration("video/mp4", mp4, opt)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			file, mime string
			exact      bool
		}{
			{"turtle-h264.avi", "video/x-msvideo", true},
			{"turtle-h264.flv", "video/x-flv", true},
			{"turtle-mpeg4.avi", "video/x-msvideo", false},
			{"turtle-flv1.flv", "video/x-flv", false},
			{"turtle-mpeg2.mpg", "video/mpeg", false},
			{"turtle-wmv2.wmv", "video/x-ms-wmv", false},
		} {
			src := d.Source(ctx)
			o := opt
			o.Container = src
			data := readTestdata(t, c.file)
			dur, err := embedding.VideoDuration(c.mime, data, o)
			if err != nil {
				t.Fatalf("%s: %v", c.file, err)
			}
			if math.Abs((dur - wantDur).Seconds()) > 0.05 {
				t.Errorf("%s: %v long, the MP4 %v", c.file, dur, wantDur)
			}
			began := time.Now()
			got, err := embedding.DecodeVideo(c.mime, data, o)
			_ = src.Close()
			if err != nil {
				t.Fatalf("%s at %v fps: %v", c.file, fps, err)
			}
			if len(got) != len(want) {
				t.Fatalf("%s at %v fps: %d frames, the MP4 %d", c.file, fps, len(got), len(want))
			}
			worst := math.Inf(1)
			for i := range got {
				p := psnr(got[i], want[i])
				if fps > 1 && i < len(wantNext) {
					p = math.Max(p, psnr(got[i], wantNext[i]))
				}
				worst = math.Min(worst, p)
				if c.exact && !math.IsInf(p, 1) {
					t.Errorf("%s at %v fps: frame %d differs from the MP4's (PSNR %.1f dB)", c.file, fps, i, p)
				}
			}
			if worst < 30 {
				t.Errorf("%s at %v fps: worst frame PSNR %.1f dB against the MP4's", c.file, fps, worst)
			}
			t.Logf("%s at %v fps: %d frames in %s, worst PSNR %.1f dB", c.file, fps, len(got), time.Since(began).Round(time.Millisecond), worst)
		}
	}
}

func psnr(a, b image.Image) float64 {
	if a.Bounds() != b.Bounds() {
		return 0
	}
	r := a.Bounds()
	var se float64
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			ar, ag, ab, _ := a.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			for _, d := range []float64{float64(ar>>8) - float64(br>>8), float64(ag>>8) - float64(bg>>8), float64(ab>>8) - float64(bb>>8)} {
				se += d * d
			}
		}
	}
	if se == 0 {
		return math.Inf(1)
	}
	mse := se / float64(3*r.Dx()*r.Dy())
	return 10 * math.Log10(255*255/mse)
}
