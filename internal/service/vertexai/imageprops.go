package vertexai

import (
	"image"
	"math"
	"sort"
)

// Vision's IMAGE_PROPERTIES feature (#1799), computed from the pixels:
// no model is needed, so it runs locally. The answer has the shape of
// Vision's own (https://docs.cloud.google.com/vision/docs/detecting-properties
// and the AnnotateImageResponse reference, read 2026-11-18):
//
//   - imagePropertiesAnnotation.dominantColors.colors: at most ten
//     ColorInfo entries, each a color with integer red, green and blue in
//     0..255 (as Vision's sample answers them, though google.type.Color
//     documents [0, 1]), a score in [0, 1] and the pixelFraction the color
//     occupies, ordered by score.
//   - cropHintsAnnotation: one crop hint covering the whole image, with
//     confidence 0.8 and importanceFraction 1, its vertices integers with
//     zero coordinates omitted ({} for the origin), as Vision's sample
//     answers it for an image with no salient region.
//
// UNVERIFIED (re-read 2026-11-18, both pages above): Vision's colour
// clustering and score are not documented.
// Here the pixels (sampled on a grid of at most 512×512) are binned at
// five bits a channel, neighbouring bins within a small RGB distance are
// merged greedily from the most populous, each cluster's color is its
// pixels' mean, its pixelFraction its share of the pixels, and its score
// that share among the returned colors. The colors and fractions are
// therefore close to Vision's for flat images, but the scores and the
// exact colors of photographs differ.

// maxDominantColors is how many colors Vision answers.
const maxDominantColors = 10

// imageProperties answers IMAGE_PROPERTIES for img: the
// imagePropertiesAnnotation and cropHintsAnnotation fields.
func imageProperties(img image.Image) map[string]any {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	step := 1
	for w/step > 512 || h/step > 512 {
		step++
	}
	type bin struct {
		n       int
		r, g, b float64
	}
	bins := map[uint16]*bin{}
	total := 0
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			r8, g8, b8 := float64(r>>8), float64(g>>8), float64(bl>>8)
			k := uint16(r>>11)<<10 | uint16(g>>11)<<5 | uint16(bl>>11)
			e := bins[k]
			if e == nil {
				e = &bin{}
				bins[k] = e
			}
			e.n++
			e.r += r8
			e.g += g8
			e.b += b8
			total++
		}
	}
	keys := make([]uint16, 0, len(bins))
	for k := range bins {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if bins[keys[i]].n != bins[keys[j]].n {
			return bins[keys[i]].n > bins[keys[j]].n
		}
		return keys[i] < keys[j]
	})
	// Merge each bin into the first cluster whose mean is near it.
	const near = 40.0
	var clusters []*bin
	for _, k := range keys {
		e := bins[k]
		mr, mg, mb := e.r/float64(e.n), e.g/float64(e.n), e.b/float64(e.n)
		var into *bin
		for _, c := range clusters {
			cr, cg, cb := c.r/float64(c.n), c.g/float64(c.n), c.b/float64(c.n)
			if math.Sqrt((cr-mr)*(cr-mr)+(cg-mg)*(cg-mg)+(cb-mb)*(cb-mb)) <= near {
				into = c
				break
			}
		}
		if into == nil {
			clusters = append(clusters, &bin{n: e.n, r: e.r, g: e.g, b: e.b})
			continue
		}
		into.n += e.n
		into.r += e.r
		into.g += e.g
		into.b += e.b
	}
	sort.SliceStable(clusters, func(i, j int) bool { return clusters[i].n > clusters[j].n })
	if len(clusters) > maxDominantColors {
		clusters = clusters[:maxDominantColors]
	}
	kept := 0
	for _, c := range clusters {
		kept += c.n
	}
	colors := make([]any, 0, len(clusters))
	for _, c := range clusters {
		n := float64(c.n)
		colors = append(colors, map[string]any{
			"color":         visionColor(c.r/n, c.g/n, c.b/n),
			"score":         float32(n / float64(kept)),
			"pixelFraction": float32(n / float64(total)),
		})
	}
	out := map[string]any{
		"imagePropertiesAnnotation": map[string]any{"dominantColors": map[string]any{"colors": colors}},
		"cropHintsAnnotation": map[string]any{"cropHints": []any{map[string]any{
			"boundingPoly": map[string]any{"vertices": []any{
				vertex(0, 0), vertex(w-1, 0), vertex(w-1, h-1), vertex(0, h-1),
			}},
			"confidence":         float32(0.8),
			"importanceFraction": 1,
		}}},
	}
	if total == 0 {
		out["imagePropertiesAnnotation"] = map[string]any{"dominantColors": map[string]any{}}
	}
	return out
}

// visionColor is a Color as Vision answers it: integer channels, zeros
// omitted.
func visionColor(r, g, b float64) map[string]any {
	c := map[string]any{}
	for name, v := range map[string]float64{"red": r, "green": g, "blue": b} {
		if n := int(math.Round(v)); n != 0 {
			c[name] = n
		}
	}
	return c
}

// vertex is a Vertex with zero coordinates omitted, as proto3 JSON writes
// it.
func vertex(x, y int) map[string]any {
	v := map[string]any{}
	if x != 0 {
		v["x"] = x
	}
	if y != 0 {
		v["y"] = y
	}
	return v
}
