package vertexai

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestAnnotateImagesProperties: IMAGE_PROPERTIES answers a two-colour
// image's colors, most populous first, with their pixel fractions, and a
// whole-image crop hint.
func TestAnnotateImagesProperties(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 40; x++ {
			c := color.RGBA{R: 200, G: 30, B: 30, A: 255}
			if x < 10 {
				c = color.RGBA{R: 0, G: 0, B: 255, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	rs := annotate(t, `{"requests":[{"image":{"content":"`+base64.StdEncoding.EncodeToString(buf.Bytes())+`"},"features":[{"type":"IMAGE_PROPERTIES"}]}]}`)
	if len(rs) != 1 {
		t.Fatalf("%d responses", len(rs))
	}
	if _, ok := rs[0]["textAnnotations"]; ok {
		t.Errorf("text without TEXT_DETECTION: %v", rs[0])
	}
	ip, _ := rs[0]["imagePropertiesAnnotation"].(map[string]any)
	dc, _ := ip["dominantColors"].(map[string]any)
	colors, _ := dc["colors"].([]any)
	if len(colors) != 2 {
		t.Fatalf("colors: %v", rs[0])
	}
	first := colors[0].(map[string]any)
	if c := first["color"].(map[string]any); c["red"] != 200.0 || c["green"] != 30.0 || c["blue"] != 30.0 {
		t.Errorf("first color: %v", first)
	}
	if first["pixelFraction"] != 0.75 || first["score"] != 0.75 {
		t.Errorf("first fraction: %v", first)
	}
	if c := colors[1].(map[string]any)["color"].(map[string]any); len(c) != 1 || c["blue"] != 255.0 {
		t.Errorf("second color (zeros omitted): %v", c)
	}
	ch, _ := rs[0]["cropHintsAnnotation"].(map[string]any)
	hints, _ := ch["cropHints"].([]any)
	if len(hints) != 1 {
		t.Fatalf("crop hints: %v", ch)
	}
	v := hints[0].(map[string]any)["boundingPoly"].(map[string]any)["vertices"].([]any)
	if len(v) != 4 || len(v[0].(map[string]any)) != 0 || v[2].(map[string]any)["x"] != 39.0 || v[2].(map[string]any)["y"] != 9.0 {
		t.Errorf("vertices: %v", v)
	}
}
