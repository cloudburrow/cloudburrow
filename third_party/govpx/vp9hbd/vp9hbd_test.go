package vp9hbd

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readIVF returns an IVF file's frames (container samples).
func readIVF(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 32 || string(b[:4]) != "DKIF" {
		t.Fatalf("%s: not IVF", path)
	}
	hdr := int(binary.LittleEndian.Uint16(b[6:]))
	b = b[hdr:]
	var out [][]byte
	for len(b) >= 12 {
		n := int(binary.LittleEndian.Uint32(b))
		b = b[12:]
		if n > len(b) {
			t.Fatalf("%s: truncated", path)
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

// frameMD5s reads an `ffmpeg -f framemd5` file: each frame's MD5.
func frameMD5s(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		out = append(out, strings.TrimSpace(f[len(f)-1]))
	}
	return out
}

// frameMD5 hashes a frame's visible planes as framemd5 does: bytes for 8
// bits, little-endian 16-bit words above.
func frameMD5(f *Frame) string {
	m := md5.New()
	for p := range 3 {
		w, h := f.Width, f.Height
		if p > 0 {
			w = (w + f.SubsamplingX) >> f.SubsamplingX
			h = (h + f.SubsamplingY) >> f.SubsamplingY
		}
		row := make([]byte, 0, 2*w)
		for y := range h {
			row = row[:0]
			for _, v := range f.Planes[p][y*f.Strides[p] : y*f.Strides[p]+w] {
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

func checkClip(t *testing.T, ivf, md5s string) {
	t.Helper()
	want := frameMD5s(t, md5s)
	d := NewDecoder()
	var got []string
	for i, s := range readIVF(t, ivf) {
		fs, err := d.Decode(s)
		if err != nil {
			t.Fatalf("%s: sample %d: %v", ivf, i, err)
		}
		for _, f := range fs {
			got = append(got, frameMD5(f))
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: %d frames, ffmpeg %d", ivf, len(got), len(want))
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			t.Errorf("%s: frame %d's MD5 is %s, ffmpeg's %s", ivf, i, got[i], want[i])
			return
		}
	}
}

// TestFixtures decodes every clip in testdata and checks each shown frame's
// MD5 against ffmpeg's decode of the same clip, made with
//
//	ffmpeg -i X.ivf -noautoscale -f framemd5 -pix_fmt PIXFMT X.framemd5
//
// where PIXFMT is the clip's pixel format (the last part of its name). The
// MD5s cover the visible planes, packed: bytes at 8 bits, little-endian
// 16-bit words at 10 and 12. The clips were made by ffmpeg 9.0.2's libvpx-vp9
// encoder as follows; ARGS is "-hide_banner -y" and every two-pass encode
// runs the command with "-pass 1 -f null -" and then "-pass 2 X.ivf".
//
// twopass-PIXFMT.ivf, for each of yuv420p, yuv422p, yuv440p, yuv444p,
// yuv420p10le, yuv420p12le, yuv422p10le, yuv444p12le, yuv440p10le (hidden
// alt-ref frames in superframes, compound prediction, switchable filters,
// sub-8x8 blocks, odd size):
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=98x66:r=10:d=2.5 -vf rotate=t*0.3:c=gray
//	  -pix_fmt PIXFMT -c:v libvpx-vp9 -b:v 60k -g 30 -auto-alt-ref 1
//	  -lag-in-frames 16 -passlogfile pl (two passes)
//
// tiles-yuv420p10le.ivf (two tile columns, sharpness):
//
//	ffmpeg ARGS -f lavfi -i mandelbrot=s=512x40:r=10 -t 0.8 -pix_fmt yuv420p10le
//	  -c:v libvpx-vp9 -b:v 400k -tile-columns 1 -auto-alt-ref 1
//	  -lag-in-frames 16 -sharpness 3 -passlogfile pl (two passes)
//
// lossless-yuv444p10le.ivf (WHT):
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=40x24:r=10:d=1 -pix_fmt yuv444p10le
//	  -c:v libvpx-vp9 -lossless 1 lossless-yuv444p10le.ivf
//
// errres-yuv422p.ivf (error resilient, frame parallel, cyclic-refresh
// segmentation with temporal map updates):
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=80x48:r=10:d=2 -pix_fmt yuv422p
//	  -c:v libvpx-vp9 -b:v 150k -deadline realtime -cpu-used 8 -aq-mode 3
//	  -error-resilient 1 -frame-parallel 1 -g 30 errres-yuv422p.ivf
//
// aq-yuv440p12le.ivf (variance segmentation: per-segment quantizers):
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=80x48:r=10:d=2 -pix_fmt yuv440p12le
//	  -c:v libvpx-vp9 -b:v 200k -aq-mode 1 -auto-alt-ref 1 -lag-in-frames 10
//	  -frame-parallel 0 aq-yuv440p12le.ivf
//
// tiny-yuv444p.ivf (17x9: partial 8x8 blocks at both edges):
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=17x9:r=10:d=1 -pix_fmt yuv444p
//	  -c:v libvpx-vp9 -b:v 50k tiny-yuv444p.ivf
//
// showexisting-yuv420p.ivf (multi-layer alt-refs: show_existing_frame):
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=64x48:r=10:d=3 -pix_fmt yuv420p
//	  -c:v libvpx-vp9 -b:v 40k -g 40 -auto-alt-ref 6 -lag-in-frames 25
//	  -passlogfile pl -f ivf (two passes)
//
// keyresize-yuv420p10le.ivf (a new size at a keyframe): the frames of
// k96x64.ivf then those of k50x30.ivf, in one IVF file, from
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=WxH:r=10:d=1 -pix_fmt yuv420p10le
//	  -c:v libvpx-vp9 -b:v 100k -g 100 -f ivf kWxH.ivf
//
// turtle-yuv420p.ivf (a camera clip): the first 60 frames of
// internal/embedding/testdata/turtle-vp9.mp4,
//
//	ffmpeg ARGS -i turtle-vp9.mp4 -c copy -an turtle.ivf
//	ffmpeg ARGS -i turtle.ivf -c copy -frames:v 60 turtle-yuv420p.ivf
//
// (all 180 frames of turtle.ivf match internal/embedding's
// turtle-vp9.framemd5 too).
//
// scaled-PIXFMT.ivf, for yuv420p, yuv422p10le and yuv444p12le (reference
// scaling, both up and down): src.ivf from
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=96x64:r=10:d=1.6 -pix_fmt PIXFMT
//	  -c:v libvpx-vp9 -b:v 60k -g 100 -auto-alt-ref 0 -lag-in-frames 0
//	  -f ivf src.ivf
//
// then rewritten by TestRewriteResize (tools_test.go) with
// VP9HBD_RESIZE="src.ivf scaled-PIXFMT.ivf 4:64x48,9:80x40,13:96x64".
//
// intraonly-PIXFMT.ivf, for yuv420p (reset_frame_context 3), yuv422p12le
// (2) and yuv444p (0): src.ivf from
//
//	ffmpeg ARGS -f lavfi -i testsrc2=s=72x40:r=10:d=2 -pix_fmt PIXFMT
//	  -c:v libvpx-vp9 -b:v 120k -g 10 -auto-alt-ref 0 -lag-in-frames 0
//	  -f ivf src.ivf
//
// then rewritten by TestRewriteIntraOnly with
// VP9HBD_INTRAONLY="src.ivf intraonly-PIXFMT.ivf 10 RESET".
func TestFixtures(t *testing.T) {
	clips, err := filepath.Glob("testdata/*.ivf")
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) < 23 {
		t.Fatalf("found %d clips in testdata", len(clips))
	}
	for _, clip := range clips {
		t.Run(strings.TrimSuffix(filepath.Base(clip), ".ivf"), func(t *testing.T) {
			checkClip(t, clip, strings.TrimSuffix(clip, ".ivf")+".framemd5")
		})
	}
}

// TestFramesStayValid checks that a decoded frame is untouched by later
// Decode calls.
func TestFramesStayValid(t *testing.T) {
	d := NewDecoder()
	var frames []*Frame
	var sums []string
	for _, s := range readIVF(t, "testdata/twopass-yuv422p10le.ivf") {
		fs, err := d.Decode(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			frames = append(frames, f)
			sums = append(sums, frameMD5(f))
		}
	}
	for i, f := range frames {
		if frameMD5(f) != sums[i] {
			t.Fatalf("frame %d changed after later frames were decoded", i)
		}
	}
	f := frames[0]
	if f.Width != 98 || f.Height != 66 || f.BitDepth != 10 || f.SubsamplingX != 1 || f.SubsamplingY != 0 {
		t.Errorf("frame 0 is %dx%d, %d bits, subsampling %d,%d", f.Width, f.Height, f.BitDepth, f.SubsamplingX, f.SubsamplingY)
	}
}

// TestCorrupt checks that damaged samples give errors, not panics.
func TestCorrupt(t *testing.T) {
	samples := readIVF(t, "testdata/twopass-yuv420p10le.ivf")
	for cut := 1; cut < 40; cut += 3 {
		d := NewDecoder()
		for i, s := range samples {
			if i == 3 {
				s = s[:min(len(s), cut)]
			}
			if i == 5 {
				s = append([]byte(nil), s...)
				for k := 10; k < len(s); k += 7 {
					s[k] ^= 0x5a
				}
			}
			d.Decode(s) // must not panic
		}
	}
	if _, err := NewDecoder().Decode([]byte{0x82, 0x49, 0x83}); err == nil {
		t.Error("a truncated keyframe decoded without error")
	}
}

// TestDevClips checks every clip in $VP9HBD_DEV against its framemd5.
func TestDevClips(t *testing.T) {
	dir := os.Getenv("VP9HBD_DEV")
	if dir == "" {
		t.Skip()
	}
	ms, _ := filepath.Glob(filepath.Join(dir, "*.ivf"))
	for _, m := range ms {
		t.Run(filepath.Base(m), func(t *testing.T) {
			checkClip(t, m, strings.TrimSuffix(m, ".ivf")+".framemd5")
		})
	}
}

// FuzzDecode feeds damaged samples after a clip's first two: Decode must
// return, with frames or an error, and never panic.
func FuzzDecode(f *testing.F) {
	samples := readIVF(&testing.T{}, "testdata/tiles-yuv420p10le.ivf")
	for _, s := range samples[2:6] {
		f.Add(s)
	}
	f.Add(readIVF(&testing.T{}, "testdata/twopass-yuv444p12le.ivf")[0])
	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDecoder()
		d.Decode(samples[0])
		d.Decode(samples[1])
		d.Decode(data)
	})
}
