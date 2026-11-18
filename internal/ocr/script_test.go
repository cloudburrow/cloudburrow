package ocr

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// page renders lines in Go Regular (whose WGL4 repertoire covers Greek and
// Cyrillic) at 12 points on a white A5-wide page at 300 DPI, a generated
// scan.
func page(t *testing.T, lines []string) *image.Gray {
	t.Helper()
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 12, DPI: 300, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
	g := image.NewGray(image.Rect(0, 0, 1748, 150+len(lines)*75))
	draw.Draw(g, g.Bounds(), image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: g, Src: image.NewUniform(color.Black), Face: face}
	for i, l := range lines {
		d.Dot = fixed.P(150, 150+i*75)
		d.DrawString(l)
	}
	return g
}

// scriptSourceForTest fetches tessdata_fast's script models through
// internal/localai, pinned by SHA-256, into the user's cache directory, so
// repeated runs fetch each once. It skips the test when the network is not
// reachable.
func scriptSourceForTest(t *testing.T) {
	t.Helper()
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	cache := localai.Cache{Dir: filepath.Join(dir, "cloudburrow-test", "models")}
	SetScriptSource(func(ctx context.Context, name string) (string, error) {
		f, ok := localai.TessdataScript(name)
		if !ok {
			t.Fatalf("no pinned script model %s", name)
		}
		p, err := cache.FetchPinnedFile(ctx, localai.TessdataFastScripts, f)
		if err != nil {
			t.Skipf("fetching %s: %v", name, err)
		}
		return p, nil
	})
	t.Cleanup(func() { SetScriptSource(nil) })
}

// TestReadNonLatinScripts reads generated Cyrillic and Greek scans: OSD
// names the script, its tessdata_fast model is fetched and the page read
// with it, with a character error rate under 5%. The Latin scan is still
// read with eng.
func TestReadNonLatinScripts(t *testing.T) {
	if testing.Short() {
		t.Skip("fetches tessdata_fast script models")
	}
	scriptSourceForTest(t)
	for _, c := range []struct {
		script, model string
		lines         []string
	}{
		{"Cyrillic", "script/Cyrillic", []string{
			"Счёт-фактура № 1733 от 17 ноября 2026 года",
			"Поставщик: ООО «Облачная нора», город Москва",
			"Покупатель обязуется оплатить товар в течение",
			"десяти рабочих дней после получения счёта.",
			"Итого к оплате: двенадцать тысяч рублей.",
		}},
		{"Greek", "script/Greek", []string{
			"Τιμολόγιο αριθμός 1733 της 17ης Νοεμβρίου",
			"Ο προμηθευτής παραδίδει τα προϊόντα στην",
			"αποθήκη του αγοραστή μέσα σε δέκα ημέρες",
			"από την ημερομηνία της παραγγελίας αυτής.",
		}},
	} {
		t.Run(c.script, func(t *testing.T) {
			want := ""
			for _, l := range c.lines {
				want += l + "\n"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			start := time.Now()
			r, err := Read(ctx, page(t, c.lines), 300)
			if err != nil {
				t.Fatal(err)
			}
			e := cer(r.Text, want)
			t.Logf("%s: script %q, model %s, CER %.1f%%, %v", c.script, r.Script, r.Model, 100*e, time.Since(start))
			if r.Script != c.script || r.Model != c.model {
				t.Errorf("OSD found %q and the page was read with %s; want %s and %s", r.Script, r.Model, c.script, c.model)
			}
			if e > 0.05 {
				t.Errorf("CER %.1f%%; want under 5%%:\n%s", 100*e, r.Text)
			}
		})
	}
	r, err := Read(context.Background(), load(t, "scan.png"), 300)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "eng" || r.Script != "Latin" {
		t.Errorf("the Latin scan was found %q and read with %s; want Latin and eng", r.Script, r.Model)
	}
}

// TestGrayMatchesColorModel checks gray's fast paths against
// color.GrayModel, pixel by pixel.
func TestGrayMatchesColorModel(t *testing.T) {
	n := image.NewNRGBA(image.Rect(3, 5, 260, 300))
	for i := range n.Pix {
		n.Pix[i] = uint8(i*7919 + i/3)
	}
	sub := n.SubImage(image.Rect(10, 20, 200, 250))
	gr := image.NewGray(image.Rect(1, 2, 40, 50))
	for i := range gr.Pix {
		gr.Pix[i] = uint8(i * 31)
	}
	for _, img := range []image.Image{n, sub, gr, gr.SubImage(image.Rect(5, 6, 30, 40))} {
		g := gray(img)
		b := img.Bounds()
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				want := color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
				if got := g.GrayAt(x, y).Y; got != want {
					t.Fatalf("%T at %d,%d: %d, want %d", img, x, y, got, want)
				}
			}
		}
	}
}
