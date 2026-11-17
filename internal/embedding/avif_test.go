package embedding

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"
)

// The AVIF fixtures (#1729) are made from testdata/pattern.png (32 x 24)
// and pattern-alpha.dwebp.png with Homebrew's libavif 1.4.2 (aom 3.15.3
// encoding, dav1d 1.5.4 decoding), libheif 1.23.7 and ImageMagick 7.1.2:
//
//	avifenc -l pattern.png pattern-lossless.avif
//	avifenc -q 80 -y 420 pattern.png pattern-420.avif
//	avifdec -u nearest pattern-420.avif pattern-420.avifdec-nearest.png
//	avifenc -d 10 -q 90 pattern.png pattern-10bit.avif
//	avifdec -d 16 pattern-10bit.avif pattern-10bit.avifdec.png
//	avifenc -l pattern-alpha.dwebp.png pattern-alpha.avif
//	magick pattern.png -flop pattern-flop.png
//	avifenc -l pattern.png pattern-flop.png pattern-sequence.avif
//	avifenc -l --irot 1 --imir 0 --crop 2,4,20,16 pattern.png pattern-transformed.avif
//	heif-dec pattern-transformed.avif pattern-transformed.heif-dec.png
//
// pattern-transformed.heif-dec.png is also what
// `magick pattern.png -crop 20x16+2+4 -rotate -90 -flip` makes, pixel for
// pixel (and what `magick pattern-transformed.avif` decodes).

// TestDecodeAVIF: a lossless AVIF is the PNG's pixels exactly; lossy 4:2:0
// and 10-bit ones are avifdec's decode within rounding; alpha is kept; an
// image sequence (AVIS) gives its first frame; clap, irot and imir are
// applied as heif-dec applies them.
func TestDecodeAVIF(t *testing.T) {
	want := decodePNG(t, "pattern.png")
	for _, mt := range []string{"image/avif", "IMAGE/AVIF; charset=binary"} {
		img, err := DecodeImage(mt, readTestImage(t, "pattern-lossless.avif"))
		if err != nil {
			t.Fatalf("%s: %v", mt, err)
		}
		if d := meanDiff(t, img, want); d != 0 {
			t.Errorf("%s: lossless AVIF differs from the PNG by %.3f on average", mt, d)
		}
		if _, err := PatchImage(img); err != nil {
			t.Errorf("%s: PatchImage: %v", mt, err)
		}
	}

	// The reference decode is the avifdec of the same libavif release. The
	// WASM build converts YUV to RGB with libyuv, whose 4:2:0 chroma
	// upsampling (libavif's default, AUTOMATIC) is nearest-neighbour, so the
	// 4:2:0 reference is avifdec -u nearest (Homebrew's avifdec has no
	// libyuv, and its default is bilinear: 3.5 a channel from this on
	// average, 86 at a sharp edge). The two YUV-to-RGB conversions round
	// differently: a channel may differ by one step, 0.5 on average at most.
	for name, ref := range map[string]string{
		"pattern-420.avif":   "pattern-420.avifdec-nearest.png",
		"pattern-10bit.avif": "pattern-10bit.avifdec.png",
		"pattern.avif":       "", // q90 4:4:4: near the PNG
	} {
		img, err := DecodeImage("image/avif", readTestImage(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := want
		if ref != "" {
			r = decodePNG(t, ref)
		}
		limit, avg := 1, 0.5
		if ref == "" {
			limit, avg = 32, 4
		}
		if d, m := meanDiff(t, img, r), maxDiff(img, r); d > avg || m > limit {
			t.Errorf("%s: differs from %s by %.3f on average, %d at most", name, ref, d, m)
		}
		if name == "pattern-10bit.avif" {
			if _, deep := img.(*image.RGBA64); !deep {
				t.Errorf("%s: a %T, want 16 bits a channel", name, img)
			}
		}
	}

	// Alpha: lossless, so each pixel is the source's, premultiplied.
	img, err := DecodeImage("image/avif", readTestImage(t, "pattern-alpha.avif"))
	if err != nil {
		t.Fatal(err)
	}
	src := decodePNG(t, "pattern-alpha.dwebp.png")
	translucent := 0
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			w := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			if got.A != w.A {
				t.Fatalf("alpha (%d, %d): %v, want %v", x, y, got, w)
			}
			if w.A < 255 {
				translucent++
			}
			if w.A == 0 {
				continue
			}
			// Premultiplying to 8 bits loses up to 255/A steps a channel.
			tol := 255/int(w.A) + 1
			if abs(int(got.R)-int(w.R)) > tol || abs(int(got.G)-int(w.G)) > tol || abs(int(got.B)-int(w.B)) > tol {
				t.Fatalf("alpha (%d, %d): %v, want %v", x, y, got, w)
			}
		}
	}
	if translucent == 0 {
		t.Error("pattern-alpha.avif has no translucent pixel; the fixture is wrong")
	}

	// An AVIS sequence: the first frame (pattern.png), not the second (its
	// mirror image).
	seq, err := DecodeImage("image/avif", readTestImage(t, "pattern-sequence.avif"))
	if err != nil {
		t.Fatal(err)
	}
	if d := meanDiff(t, seq, want); d != 0 {
		t.Errorf("image sequence: %.3f from the first frame", d)
	}

	// clap 20 x 16 at (2, 4), rotated a quarter turn anticlockwise, then
	// top and bottom exchanged: heif-dec's picture exactly.
	tr, err := DecodeImage("image/avif", readTestImage(t, "pattern-transformed.avif"))
	if err != nil {
		t.Fatal(err)
	}
	if d := meanDiff(t, tr, decodePNG(t, "pattern-transformed.heif-dec.png")); d != 0 {
		t.Errorf("transformed: %v differs from heif-dec's by %.3f on average", tr.Bounds(), d)
	}
}

// maxDiff is the largest difference of one RGB channel of a and b.
func maxDiff(a, b image.Image) int {
	ga, gb := toRGB(a), toRGB(b)
	m := 0
	for i := range ga {
		m = max(m, abs(int(ga[i])-int(gb[i])))
	}
	return m
}

// TestDecodeImageRefusesBadAVIF: random bytes, a truncated AVIF, another
// format sent as image/avif and an AVIF wider than Google's 16384 pixels
// are refused naming the type, before any pixel is decoded.
func TestDecodeImageRefusesBadAVIF(t *testing.T) {
	data := readTestImage(t, "pattern-lossless.avif")
	wide := append([]byte(nil), data...)
	i := bytes.Index(wide, []byte("ispe"))
	if i < 0 {
		t.Fatal("no ispe box")
	}
	copy(wide[i+8:], []byte{0, 0, 0x40, 0x01}) // width 16385
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"garbage":       {[]byte("not an image at all"), "not a valid image/avif"},
		"truncated":     {data[:len(data)/2], "not a valid image/avif"},
		"header only":   {data[:40], "not a valid image/avif"},
		"png as avif":   {readTestImage(t, "pattern.png"), "not a valid image/avif"},
		"heic as avif":  {readTestImage(t, "pattern-lossy.heic"), "not a valid image/avif"},
		"16385 wide":    {wide, "the image is 16385x24; each side must be 1 to 16384 pixels"},
		"empty":         {nil, "the image data is empty"},
		"webp as avif":  {readTestImage(t, "pattern-lossy.webp"), "not a valid image/avif"},
		"avif as heic":  {data, "not a valid image/heic"},
		"avif as webp":  {data, "not a valid image/webp"},
		"avif as jpeg":  {data, "not a valid image/jpeg"},
		"avif as image": {data, "not supported"},
	} {
		mt := "image/avif"
		switch {
		case strings.HasSuffix(name, "as heic"):
			mt = "image/heic"
		case strings.HasSuffix(name, "as webp"):
			mt = "image/webp"
		case strings.HasSuffix(name, "as jpeg"):
			mt = "image/jpeg"
		case strings.HasSuffix(name, "as image"):
			mt = "image/x-avif"
		}
		_, err := DecodeImage(mt, c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, c.want)
		}
	}
}

// TestAVIFCrop: the clean aperture as libavif derives it, and the invalid
// ones it does not apply.
func TestAVIFCrop(t *testing.T) {
	clap := func(v ...int32) []byte {
		b := make([]byte, 32)
		for i, x := range v {
			b[4*i], b[4*i+1], b[4*i+2], b[4*i+3] = byte(x>>24), byte(x>>16), byte(x>>8), byte(x)
		}
		return b
	}
	for name, c := range map[string]struct {
		clap []byte
		want image.Rectangle
		ok   bool
	}{
		"avifenc --crop 2,4,20,16": {clap(20, 1, 16, 1, -4, 1, 0, 1), image.Rect(2, 4, 22, 20), true},
		"centred":                  {clap(16, 1, 12, 1, 0, 1, 0, 1), image.Rect(8, 6, 24, 18), true},
		"half-pixel offset":        {clap(16, 1, 12, 1, 1, 2, 0, 1), image.Rectangle{}, false},
		"fractional width":         {clap(31, 2, 12, 1, 0, 1, 0, 1), image.Rectangle{}, false},
		"zero denominator":         {clap(16, 0, 12, 1, 0, 1, 0, 1), image.Rectangle{}, false},
		"outside":                  {clap(32, 1, 24, 1, 2, 1, 0, 1), image.Rectangle{}, false},
		"whole":                    {clap(32, 1, 24, 1, 0, 1, 0, 1), image.Rect(0, 0, 32, 24), true},
	} {
		r, ok := avifCrop(c.clap, 32, 24)
		if ok != c.ok || r != c.want {
			t.Errorf("%s: %v %v, want %v %v", name, r, ok, c.want, c.ok)
		}
	}
}
