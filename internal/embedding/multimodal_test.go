package embedding

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

func TestPreprocessCLIPNormalisesAndCrops(t *testing.T) {
	// A 300x200 image, red on the left third, grey elsewhere: resized to
	// 336x224 and centre-cropped, so the crop starts 56 px in.
	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 300; x++ {
			c := color.RGBA{128, 128, 128, 255}
			if x < 100 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	px := PreprocessCLIP(img)
	if len(px) != 3*CLIPImageSize*CLIPImageSize {
		t.Fatalf("%d values", len(px))
	}
	plane := CLIPImageSize * CLIPImageSize
	at := func(c, x, y int) float64 { return float64(px[c*plane+y*CLIPImageSize+x]) }
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-3 }
	// Column 0 of the crop is source x 56/1.12 = 50: red.
	if !near(at(0, 0, 100), (1-clipMean[0])/clipStd[0]) || !near(at(1, 0, 100), (0-clipMean[1])/clipStd[1]) {
		t.Errorf("left: %v %v", at(0, 0, 100), at(1, 0, 100))
	}
	grey := (128.0/255 - clipMean[2]) / clipStd[2]
	if !near(at(2, 223, 100), grey) {
		t.Errorf("right: %v, want %v", at(2, 223, 100), grey)
	}
}

func TestDecodeImageChecksType(t *testing.T) {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 2)))
	if _, err := DecodeImage(buf.Bytes(), "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeImage(buf.Bytes(), "image/jpeg"); err == nil {
		t.Fatal("a PNG labelled image/jpeg was accepted")
	}
	if _, err := DecodeImage([]byte("not an image"), ""); err == nil {
		t.Fatal("garbage decoded")
	}
}

func TestCLIPTokensKeepsEOS(t *testing.T) {
	ids := make([]uint32, 100)
	got, cut := CLIPTokens(ids)
	if !cut || len(got) != CLIPMaxTokens || got[CLIPMaxTokens-1] != CLIPEOS {
		t.Fatalf("len %d cut %v last %d", len(got), cut, got[len(got)-1])
	}
	if _, cut := CLIPTokens(ids[:10]); cut {
		t.Fatal("short text cut")
	}
	if ValidateMultimodalDim(1408) == nil || ValidateMultimodalDim(256) != nil {
		t.Fatal("dimension validation")
	}
}
