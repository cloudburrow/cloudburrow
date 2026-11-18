package ocr

import (
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, name string) image.Image {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// cer is the character error rate: the edit distance between got and want,
// with runs of white space as one space, over want's length.
func cer(got, want string) float64 {
	a := []rune(strings.Join(strings.Fields(got), " "))
	b := []rune(strings.Join(strings.Fields(want), " "))
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := prev[j-1]
			if a[i-1] != b[j-1] {
				c++
			}
			cur[j] = min(c, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return float64(prev[len(b)]) / float64(len(b))
}

func TestReadScan(t *testing.T) {
	want, err := os.ReadFile("testdata/scan.txt")
	if err != nil {
		t.Fatal(err)
	}
	g := gray(load(t, "scan.png"))
	for _, deg := range []int{0, 90, 180, 270} {
		// Turn the page clockwise by deg, as a scanner might have: rotate
		// turns it counter-clockwise.
		turned := rotate(g, (360-deg)%360)
		start := time.Now()
		r, err := Read(context.Background(), turned, 300)
		if err != nil {
			t.Fatalf("%d: %v", deg, err)
		}
		c := cer(r.Text, string(want))
		t.Logf("turned %d: found rotation %d (detected %d, %.2f), script %q, CER %.3f, %v", deg, r.Rotation, r.Detected, r.Confidence, r.Script, c, time.Since(start))
		// Automatic page segmentation reads a page turned a quarter
		// either way as vertical text; a page upside down is turned back.
		if (deg == 0 || deg == 180) && r.Rotation != deg {
			t.Errorf("turned %d: found rotation %d", deg, r.Rotation)
		}
		if c > 0.05 {
			t.Errorf("turned %d: CER %.3f over 0.05; text:\n%s", deg, c, r.Text)
		}
	}
}

// TestReadStopsWhenCancelled checks that a read whose context is done
// stops (the runtime no longer closes the module on it, engine.call checks
// it), and that the next read still works.
func TestReadStopsWhenCancelled(t *testing.T) {
	img := load(t, "scan.png")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, img, 300); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read with a cancelled context = %v; want context.Canceled", err)
	}
	if r, err := Read(context.Background(), img, 300); err != nil || r.Model != "eng" {
		t.Fatalf("Read after a cancelled one = %+v, %v", r, err)
	}
}
