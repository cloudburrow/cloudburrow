package embedding

import (
	"crypto/md5"
	"encoding/hex"
	"testing"

	"github.com/cloudburrow/cloudburrow/third_party/govpx/vp9hbd"
)

// hbdPlanesMD5 is the MD5 of a vp9hbd frame's visible planes as framemd5
// hashes them at the frame's own pixel format: bytes at 8 bits,
// little-endian 16-bit samples above.
func hbdPlanesMD5(f *vp9hbd.Frame) string {
	m := md5.New()
	for p := range 3 {
		pw, ph := f.Width, f.Height
		if p > 0 {
			pw = (f.Width + f.SubsamplingX) >> f.SubsamplingX
			ph = (f.Height + f.SubsamplingY) >> f.SubsamplingY
		}
		row := make([]byte, 0, 2*pw)
		for y := range ph {
			row = row[:0]
			for _, v := range f.Planes[p][y*f.Strides[p] : y*f.Strides[p]+pw] {
				if f.BitDepth == 8 {
					row = append(row, byte(v))
				} else {
					row = append(row, byte(v), byte(v>>8))
				}
			}
			m.Write(row)
		}
	}
	return hex.EncodeToString(m.Sum(nil))
}

// TestVP9ProfilesMatchFFmpeg (#1739): every frame of the sea turtle clip in
// VP9 profiles 1 (8-bit 4:4:4), 2 (10-bit 4:2:0) and 3 (12-bit 4:2:2),
// 180 frames each with hidden alternate reference frames, decodes to the
// samples ffmpeg 9's vp9 decoder gives (testdata/*.framemd5, `ffmpeg -i f
// -f framemd5 -pix_fmt <its format>`), and so does profile 0's clip. The
// clips decode in parallel.
func TestVP9ProfilesMatchFFmpeg(t *testing.T) {
	for _, file := range []string{"turtle-vp9-444.mp4", "turtle-vp9-10bit.mp4", "turtle-vp9-422-12bit.mp4", "turtle-vp9.mp4"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			vp9MatchesFFmpeg(t, file)
		})
	}
}

// vp9MatchesFFmpeg checks every frame of file against its framemd5.
func vp9MatchesFFmpeg(t *testing.T, file string) {
	want := ffmpegFrameMD5s(t, file[:len(file)-4]+".framemd5")
	tr, err := readVideoTrack(readTestdata(t, file))
	if err != nil {
		t.Fatal(err)
	}
	d := vp9hbd.NewDecoder()
	var got []string
	for _, s := range tr.samples {
		fs, err := d.Decode(s)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, f := range fs {
			got = append(got, hbdPlanesMD5(f))
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

// TestDecodeRGBVP9 (#1739): a VP9 stream coded as RGB (color_space CS_RGB,
// planes G, B and R, profiles 1 and 3) is its pixels, the 10-bit one scaled
// to 8 bits, as ffmpeg's `-pix_fmt rgb24` gives its first frame
// (testdata/turtle-vp9-gbrp*.frame0.rgb24) within one step:
//
//	ffmpeg -t 1 -i turtle-h264.mp4 -vf scale=64:36 -r 2 -an -c:v libvpx-vp9 -pix_fmt gbrp -crf 40 -b:v 0 -map_metadata -1 turtle-vp9-gbrp.mp4
//	ffmpeg -t 1 -i turtle-h264.mp4 -vf scale=64:36 -r 2 -an -c:v libvpx-vp9 -pix_fmt gbrp10le -crf 40 -b:v 0 -map_metadata -1 turtle-vp9-gbrp10le.mp4
func TestDecodeRGBVP9(t *testing.T) {
	for _, file := range []string{"turtle-vp9-gbrp.mp4", "turtle-vp9-gbrp10le.mp4"} {
		frames, err := DecodeVideo("video/mp4", readTestdata(t, file), VideoOptions{})
		if err != nil || len(frames) == 0 {
			t.Fatalf("%s: %d frames, %v", file, len(frames), err)
		}
		want := readTestdata(t, file[:len(file)-4]+".frame0.rgb24")
		img := frames[0]
		if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 36 {
			t.Fatalf("%s: %v, want 64x36", file, b)
		}
		worst := 0
		for y := range 36 {
			for x := range 64 {
				r, g, b, _ := img.At(x, y).RGBA()
				for i, v := range []uint32{r >> 8, g >> 8, b >> 8} {
					d := int(v) - int(want[(y*64+x)*3+i])
					worst = max(worst, d, -d)
				}
			}
		}
		if worst > 1 {
			t.Errorf("%s: %d steps from ffmpeg's RGB, want at most 1", file, worst)
		}
	}
}
