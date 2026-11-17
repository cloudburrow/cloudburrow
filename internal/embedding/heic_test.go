package embedding

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"testing"
)

// The HEIC, HEIF and animated WebP fixtures (#1724) are made from
// testdata/pattern.png (32 x 24) with Homebrew's libheif 1.23.7 (x265),
// libavif 1.4.2 and libwebp 1.6.0:
//
//	heif-enc -L pattern.png -o pattern-lossless.heic
//	heif-enc -q 90 pattern.png -o pattern-lossy.heic
//	heif-dec -C bilinear pattern-lossy.heic pattern-lossy.heif-dec-bilinear.png
//	magick pattern.png -flop pattern-flop.png
//	heif-enc -S -q 90 pattern.png pattern-flop.png -o pattern-sequence.heic
//	avifenc -q 90 pattern.png pattern.avif
//	cwebp -q 80 pattern.png -o f1.webp
//	cwebp -lossless pattern-flop.png -o f2.webp
//	webpmux -frame f1.webp +100+0+0+0-b -frame f2.webp +100+0+0+0+b -loop 0 -o anim-lossy.webp
//	webpmux -get frame 1 anim-lossy.webp -o fr1.webp && dwebp fr1.webp -o anim-lossy.frame1.dwebp.png
//	webpmux -frame pattern-alpha.webp +100+4+2+0-b -frame f2.webp +100+0+0+0+b -loop 0 -o anim-offset-alpha.webp

// meanDiff is the mean absolute difference per RGB channel of a and b,
// which must be the same size.
func meanDiff(t *testing.T, a, b image.Image) float64 {
	t.Helper()
	if a.Bounds().Size() != b.Bounds().Size() {
		t.Fatalf("sizes differ: %v, %v", a.Bounds(), b.Bounds())
	}
	ga, gb := toRGB(a), toRGB(b)
	sum := 0
	for i := range ga {
		sum += abs(int(ga[i]) - int(gb[i]))
	}
	return float64(sum) / float64(len(ga))
}

func decodePNG(t *testing.T, name string) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(readTestImage(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestDecodeHEICAndHEIF: a lossless HEIC is the PNG's pixels exactly; a
// lossy one is libheif's heif-dec decode with bilinear chroma (what the decoder does) within rounding, and is embedded
// whether sent as image/heic or image/heif; an image sequence gives its
// first frame.
func TestDecodeHEICAndHEIF(t *testing.T) {
	want := decodePNG(t, "pattern.png")
	img, err := DecodeImage("image/heic", readTestImage(t, "pattern-lossless.heic"))
	if err != nil {
		t.Fatal(err)
	}
	if d := meanDiff(t, img, want); d != 0 {
		t.Errorf("lossless HEIC differs from the PNG by %.3f on average", d)
	}
	ref := decodePNG(t, "pattern-lossy.heif-dec-bilinear.png")
	for _, mt := range []string{"image/heic", "image/heif", "IMAGE/HEIF; charset=binary"} {
		img, err := DecodeImage(mt, readTestImage(t, "pattern-lossy.heic"))
		if err != nil {
			t.Fatalf("%s: %v", mt, err)
		}
		if d := meanDiff(t, img, ref); d > 0.05 {
			t.Errorf("%s: lossy HEIC differs from heif-dec's by %.3f on average", mt, d)
		}
		if _, err := PatchImage(img); err != nil {
			t.Errorf("%s: PatchImage: %v", mt, err)
		}
	}
	seq, err := DecodeImage("image/heic", readTestImage(t, "pattern-sequence.heic"))
	if err != nil {
		t.Fatal(err)
	}
	flop := image.NewNRGBA(want.Bounds())
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			flop.Set(31-x, y, want.At(x, y))
		}
	}
	if first, second := meanDiff(t, seq, want), meanDiff(t, seq, flop); first > 8 || second < 4*first {
		t.Errorf("image sequence: %.2f from the first frame, %.2f from the second", first, second)
	}
}

// TestDecodeImageRefusesBadHEIFAndAVIF: an AV1-coded HEIF (AVIF), random
// bytes and a truncated HEIC are INVALID_ARGUMENT-ready errors naming the
// type, and image/avif is not a served type.
func TestDecodeImageRefusesBadHEIFAndAVIF(t *testing.T) {
	avif := readTestImage(t, "pattern.avif")
	heic := readTestImage(t, "pattern-lossy.heic")
	for name, c := range map[string]struct {
		mt   string
		data []byte
		want string
	}{
		"avif as heif": {"image/heif", avif, "not a valid image/heif"},
		"garbage":      {"image/heic", []byte("not an image at all"), "not a valid image/heic"},
		"truncated":    {"image/heic", heic[:len(heic)/2], "not a valid image/heic"},
		"png as heic":  {"image/heic", readTestImage(t, "pattern.png"), "not a valid image/heic"},
		"avif":         {"image/avif", avif, `mime_type "image/avif" is not supported`},
	} {
		_, err := DecodeImage(c.mt, c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, c.want)
		}
	}
}

// TestDecodeAnimatedWebPFirstFrame: an animated WebP is its first frame,
// pixel-identical to libwebp's dwebp of that frame; a frame placed at an
// offset sits there on a transparent canvas.
func TestDecodeAnimatedWebPFirstFrame(t *testing.T) {
	for _, name := range []string{"pattern-animated.webp", "anim-lossy.webp", "anim-offset-alpha.webp"} {
		if !webpAnimated(readTestImage(t, name)) {
			t.Fatalf("%s is not seen as animated", name)
		}
	}
	img, err := DecodeImage("image/webp", readTestImage(t, "anim-lossy.webp"))
	if err != nil {
		t.Fatal(err)
	}
	ref := decodePNG(t, "anim-lossy.frame1.dwebp.png")
	if img.Bounds() != ref.Bounds() {
		t.Fatalf("anim-lossy: %v, dwebp %v", img.Bounds(), ref.Bounds())
	}
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			got := color.NRGBAModel.Convert(img.At(x, y))
			if want := color.NRGBAModel.Convert(ref.At(x, y)); got != want {
				t.Fatalf("anim-lossy (%d, %d): %v, dwebp %v", x, y, got, want)
			}
		}
	}

	img, err = DecodeImage("image/webp", readTestImage(t, "anim-offset-alpha.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 36 || b.Dy() != 26 {
		t.Fatalf("anim-offset-alpha: %v, want the 36 x 26 canvas", b)
	}
	frame := decodePNG(t, "pattern-alpha.dwebp.png") // its first frame is pattern-alpha.webp
	for y := 0; y < 26; y++ {
		for x := 0; x < 36; x++ {
			got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			want := color.NRGBA{}
			if x >= 4 && y >= 2 && x < 36 && y < 26 {
				want = color.NRGBAModel.Convert(frame.At(x-4, y-2)).(color.NRGBA)
			}
			if got != want {
				t.Fatalf("anim-offset-alpha (%d, %d): %v, want %v", x, y, got, want)
			}
		}
	}

	// The two-frame lossless fixture of #1651 decodes too.
	if _, err := DecodeImage("image/webp", readTestImage(t, "pattern-animated.webp")); err != nil {
		t.Errorf("pattern-animated.webp: %v", err)
	}
}

// TestDecodeImageRefusesBadAnimatedWebP: a truncated or malformed animation
// is refused naming the type, never a panic.
func TestDecodeImageRefusesBadAnimatedWebP(t *testing.T) {
	data := readTestImage(t, "anim-offset-alpha.webp")
	cases := map[string][]byte{
		"flag only": []byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x02\x00\x00\x00\x0f\x00\x00\x0f\x00\x00"),
	}
	for _, n := range []int{20, 30, 40, 60, len(data) - 10} {
		cut := append([]byte(nil), data[:n]...)
		cases["cut at "+strconv.Itoa(n)] = cut
	}
	// A frame that leaves the canvas.
	off := append([]byte(nil), data...)
	if i := bytes.Index(off, []byte("ANMF")); i > 0 {
		off[i+8] = 0x7f // x offset 254
	}
	cases["off canvas"] = off
	for name, b := range cases {
		if _, err := DecodeImage("image/webp", b); err == nil || !strings.Contains(err.Error(), "not a valid image/webp") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// webpAnimated: still WebPs and truncated data are not animated.
	for _, still := range []string{"pattern-lossless.webp", "pattern-lossy.webp", "pattern-alpha.webp"} {
		if webpAnimated(readTestImage(t, still)) {
			t.Errorf("%s is seen as animated", still)
		}
	}
	if webpAnimated([]byte("RIFF\xff\xff\xff\xffWEBPVP8L\xff\xff\xff\x7f")) || webpAnimated([]byte("RIFF")) {
		t.Error("truncated data is seen as animated")
	}
}
