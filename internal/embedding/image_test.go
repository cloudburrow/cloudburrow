package embedding

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
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
	for mt, want := range map[string]string{"image/webp": "not supported", "image/jpeg": "not a valid", "": "not supported"} {
		if _, err := DecodeImage(mt, b.Bytes()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", mt, err, want)
		}
	}
	if _, err := DecodeImage("image/png", nil); err == nil {
		t.Error("empty data accepted")
	}
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
