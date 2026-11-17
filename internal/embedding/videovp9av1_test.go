package embedding

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/third_party/govpx"
	"github.com/gen2brain/gav1d/av1"
)

// ffmpegFrameMD5s reads an `ffmpeg -i f -f framemd5 -pix_fmt yuv420p`
// file: each output frame's MD5, in output order.
func ffmpegFrameMD5s(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(string(readTestdata(t, name)), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		out = append(out, strings.TrimSpace(f[len(f)-1]))
	}
	return out
}

// planesMD5 is the MD5 of a frame's visible planes, packed, as framemd5
// hashes them.
func planesMD5(w, h int, planes [3][]byte, strides [3]int) string {
	m := md5.New()
	for p := range 3 {
		pw, ph := w, h
		if p > 0 {
			pw, ph = (w+1)/2, (h+1)/2
		}
		for y := range ph {
			m.Write(planes[p][y*strides[p] : y*strides[p]+pw])
		}
	}
	return hex.EncodeToString(m.Sum(nil))
}

// TestVP9AndAV1MatchFFmpeg (#1731): every frame of testdata's VP9 (libvpx,
// with alternate reference frames hidden in superframes) and AV1 (SVT-AV1)
// turtle clips, 180 frames each, decodes to the bytes ffmpeg 9's own vp9
// and libdav1d decoders give (testdata/*.framemd5).
func TestVP9AndAV1MatchFFmpeg(t *testing.T) {
	for _, file := range []string{"turtle-vp9.mp4", "turtle-av1.mp4"} {
		want := ffmpegFrameMD5s(t, strings.TrimSuffix(file, ".mp4")+".framemd5")
		tr, err := readVideoTrack(readTestdata(t, file))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		switch tr.codec {
		case "vp09":
			d, err := govpx.NewVP9Decoder(govpx.VP9DecoderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tr.samples {
				if err := d.Decode(s); err != nil {
					t.Fatal(err)
				}
				if img, ok := d.NextFrame(); ok {
					got = append(got, planesMD5(img.Width, img.Height, [3][]byte{img.Y, img.U, img.V}, [3]int{img.YStride, img.UStride, img.VStride}))
				}
			}
			d.Close()
		case "av01":
			d := &av1.Decoder{}
			for _, s := range tr.samples {
				ps, err := d.DecodeOBUs(s)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range ps {
					got = append(got, planesMD5(p.Width, p.Height, p.Data, p.Stride))
					p.Release()
				}
			}
		}
		if len(got) != len(want) || len(got) != 180 {
			t.Fatalf("%s: %d frames, ffmpeg %d", file, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: frame %d's MD5 is %s, ffmpeg's %s", file, i, got[i], want[i])
			}
		}
	}
}

// TestDecode10BitAV1: a 10-bit AV1 decodes (gav1d takes 8 to 12 bits), its
// frames the size of the video.
func TestDecode10BitAV1(t *testing.T) {
	frames, err := DecodeVideo("video/mp4", readTestdata(t, "turtle-av1-10bit.mp4"), VideoOptions{})
	if err != nil || len(frames) == 0 {
		t.Fatalf("%d frames, %v", len(frames), err)
	}
	for _, f := range frames {
		if b := f.Bounds(); b.Dx() != 64 || b.Dy() != 36 {
			t.Errorf("a frame is %v, want 64x36", b)
		}
	}
}
