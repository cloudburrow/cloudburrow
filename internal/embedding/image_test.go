package embedding

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
)

func TestImageTargetSizeMatchesTheProcessor(t *testing.T) {
	maxPatches := imageSoftTokens * imagePoolingKernel * imagePoolingKernel
	for _, c := range []struct{ h, w, th, tw int }{
		{768, 768, 768, 768},   // already the size: not resized
		{600, 1000, 576, 1008}, // floor(sqrt(645120/600000)*600/48)*48, likewise the width
		{10, 4000, 48, 13440},  // too thin: one block high, capped at 280 blocks long
		{4000, 10, 13440, 48},
	} {
		th, tw, err := imageTargetSize(c.h, c.w, maxPatches)
		if err != nil || th != c.th || tw != c.tw {
			t.Errorf("%dx%d: %dx%d %v, want %dx%d", c.h, c.w, th, tw, err, c.th, c.tw)
		}
	}
}

func TestPatchImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 768, 768))
	img.Set(17, 0, color.RGBA{255, 0, 0, 255}) // patch (col 1, row 0), dx 1
	p, err := PatchImage(img)
	if err != nil {
		t.Fatal(err)
	}
	if p.NumPatches != 2304 || p.SoftTokens != 256 || len(p.Pixels) != 2304*768 || len(p.Positions) != 2304*2 {
		t.Fatalf("patches %d soft %d pixels %d positions %d", p.NumPatches, p.SoftTokens, len(p.Pixels), len(p.Positions))
	}
	// The second patch is column 1, row 0; its second pixel's red is 1.
	if p.Positions[2] != 1 || p.Positions[3] != 0 || p.Pixels[768+3] != 1 || p.Pixels[768+4] != 0 {
		t.Fatalf("positions %v pixels %v", p.Positions[:4], p.Pixels[768:768+6])
	}
	// A resized image keeps whole 3x3 blocks.
	q, err := PatchImage(image.NewRGBA(image.Rect(0, 0, 1000, 600)))
	if err != nil || q.NumPatches != 36*63 || q.SoftTokens != 252 {
		t.Fatalf("1000x600: %d patches, %d soft tokens, %v", q.NumPatches, q.SoftTokens, err)
	}
}

func TestResizeKeepsAFlatImageFlat(t *testing.T) {
	src := bytes.Repeat([]byte{10, 120, 250}, 100*50)
	out := resizeBicubic(src, 100, 50, 48, 96)
	for i := 0; i < len(out); i += 3 {
		if out[i] != 10 || out[i+1] != 120 || out[i+2] != 250 {
			t.Fatalf("pixel %d is %v", i/3, out[i:i+3])
		}
	}
}

func TestDecodeImage(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	img, err := DecodeImage("image/png", b.Bytes())
	if err != nil || img.Bounds().Dx() != 3 {
		t.Fatalf("%v %v", img, err)
	}
	for mt, want := range map[string]string{"image/heic": "not supported", "image/avif": "not supported", "image/webp": "not a valid", "image/bmp": "not a valid", "image/jpeg": "not a valid", "": "not supported"} {
		if _, err := DecodeImage(mt, b.Bytes()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", mt, err, want)
		}
	}
	if _, err := DecodeImage("image/png", nil); err == nil {
		t.Error("empty data accepted")
	}
}

// readTestImage reads testdata/name. The fixtures are made from
// testdata/pattern.png, a 32 x 24 image of red, blue and green quarters on
// white: pattern-lossless.webp by cwebp -lossless -exact, pattern-lossy.webp
// by cwebp -q 90, pattern-alpha.webp (the right half at alpha 0.5) by
// cwebp -q 90 -exact, pattern-24.bmp and pattern-4.bmp (a 16-colour
// palette) by ImageMagick, and pattern-animated.webp, two 16 x 16 frames,
// by img2webp, and odd-lossy.webp, the pattern resized to 33 x 25, by
// cwebp -q 90 (libwebp 1.6). Each *.dwebp.png is libwebp 1.6's dwebp
// decoding of the WebP of that name.
func readTestImage(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDecodeWebPAndBMP (#1651): WebP and BMP decode through
// golang.org/x/image to the pixels of the PNG they were made from (exactly
// when lossless, within a few levels when lossy), so they go through the
// same preprocessing as PNG and JPEG. (A lossy WebP's exact pixels are
// TestLossyWebPMatchesLibwebp's.)
func TestDecodeWebPAndBMP(t *testing.T) {
	ref, err := DecodeImage("image/png", readTestImage(t, "pattern.png"))
	if err != nil {
		t.Fatal(err)
	}
	want := toRGB(ref)
	for _, c := range []struct {
		file, mimeType string
		maxDiff        int // the mean per channel, 0 for lossless
	}{
		{"pattern-lossless.webp", "image/webp", 0},
		{"pattern-lossy.webp", "image/webp", 8},
		{"pattern-24.bmp", "image/bmp", 0},
		{"pattern-4.bmp", "image/bmp", 0},
		{"pattern-24.bmp", "IMAGE/BMP; charset=binary", 0},
	} {
		img, err := DecodeImage(c.mimeType, readTestImage(t, c.file))
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 24 {
			t.Errorf("%s: %v", c.file, b)
			continue
		}
		got, sum := toRGB(img), 0
		for i := range got {
			sum += abs(int(got[i]) - int(want[i]))
		}
		if mean := float64(sum) / float64(len(got)); mean > float64(c.maxDiff) {
			t.Errorf("%s: the channels differ from the PNG's by %.2f on average, want at most %d", c.file, mean, c.maxDiff)
		}
		if _, err := PatchImage(img); err != nil {
			t.Errorf("%s: PatchImage: %v", c.file, err)
		}
	}
	// A WebP with alpha keeps its colours, alpha dropped as for PNG.
	img, err := DecodeImage("image/webp", readTestImage(t, "pattern-alpha.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if c := color.NRGBAModel.Convert(img.At(24, 4)).(color.NRGBA); c.A < 120 || c.A > 136 || c.B < 200 {
		t.Errorf("alpha WebP: the blue half at alpha 0.5 is %v", c)
	}
	// Each format's data under another's type is refused, not sniffed.
	if _, err := DecodeImage("image/bmp", readTestImage(t, "pattern-lossless.webp")); err == nil || !strings.Contains(err.Error(), "not a valid image/bmp") {
		t.Errorf("WebP sent as BMP: %v", err)
	}
}

// TestDecodeImageRefusesAnimatedWebP: golang.org/x/image decodes no
// animation, so an animated WebP is refused saying so, whatever its frames.
func TestDecodeImageRefusesAnimatedWebP(t *testing.T) {
	data := readTestImage(t, "pattern-animated.webp")
	if !webpAnimated(data) {
		t.Fatal("the animated fixture is not seen as animated")
	}
	if _, err := DecodeImage("image/webp", data); err != ErrAnimatedWebP {
		t.Fatalf("animated WebP: %v, want %v", err, ErrAnimatedWebP)
	}
	for _, still := range []string{"pattern-lossless.webp", "pattern-lossy.webp", "pattern-alpha.webp"} {
		if webpAnimated(readTestImage(t, still)) {
			t.Errorf("%s is seen as animated", still)
		}
	}
	// The VP8X animation flag alone is enough; truncated chunks are not.
	flagOnly := []byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x02\x00\x00\x00\x0f\x00\x00\x0f\x00\x00")
	if !webpAnimated(flagOnly) {
		t.Error("a VP8X animation flag is not seen")
	}
	if webpAnimated([]byte("RIFF\xff\xff\xff\xffWEBPVP8L\xff\xff\xff\x7f")) || webpAnimated([]byte("RIFF")) {
		t.Error("truncated data is seen as animated")
	}
}

// TestLossyWebPMatchesLibwebp: a lossy WebP's pixels, with and without
// alpha, odd sizes included, are exactly those libwebp's dwebp writes (its
// limited-range conversion and fancy chroma upsampling), as sharp decodes
// them for Transformers.js, not image.YCbCr's JPEG conversion.
func TestLossyWebPMatchesLibwebp(t *testing.T) {
	for _, name := range []string{"pattern-lossy", "pattern-alpha", "odd-lossy"} {
		img, err := DecodeImage("image/webp", readTestImage(t, name+".webp"))
		if err != nil {
			t.Fatal(err)
		}
		ref, err := png.Decode(bytes.NewReader(readTestImage(t, name+".dwebp.png")))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds() != ref.Bounds() {
			t.Fatalf("%s: %v, dwebp %v", name, img.Bounds(), ref.Bounds())
		}
		diff := 0
		for y := 0; y < ref.Bounds().Dy(); y++ {
			for x := 0; x < ref.Bounds().Dx(); x++ {
				got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				want := color.NRGBAModel.Convert(ref.At(x, y)).(color.NRGBA)
				if got != want {
					if diff++; diff <= 3 {
						t.Errorf("%s (%d, %d): %v, dwebp %v", name, x, y, got, want)
					}
				}
			}
		}
		if diff > 0 {
			t.Errorf("%s: %d pixels differ from dwebp's", name, diff)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestMultimodalPrompt(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	for _, c := range []struct {
		in   Input
		want string
		n    int
	}{
		{Input{Parts: []Part{{Image: img}}}, "<|image|>", 1},
		{Input{Parts: []Part{{Image: img}, {Image: img}}}, "<|image|> <|image|>", 2},
		{Input{TaskType: "RETRIEVAL_QUERY", Parts: []Part{{Text: "cats"}, {Image: img}}}, "task: search result | query: cats<|image|>", 1},
		{Input{TaskType: "RETRIEVAL_DOCUMENT", Parts: []Part{{Text: "a"}, {Text: "b"}, {Image: img}, {Text: "c"}}}, "title: none | text: a\nb<|image|>c", 1},
	} {
		got, n, err := MultimodalPrompt(c.in)
		if err != nil || got != c.want || n != c.n {
			t.Errorf("%+v: %q %d %v, want %q %d", c.in, got, n, err, c.want, c.n)
		}
	}
	if _, _, err := MultimodalPrompt(Input{Parts: []Part{{Text: "x <|image|>"}}}); err == nil {
		t.Error("a text holding a media marker was accepted")
	}
	seven := make([]Part, 7)
	for i := range seven {
		seven[i] = Part{Image: img}
	}
	if _, _, err := MultimodalPrompt(Input{Parts: seven}); err == nil {
		t.Error("seven images were accepted")
	}
}
