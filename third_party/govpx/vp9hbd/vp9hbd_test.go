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
