// Package ocr reads the text of an image with Tesseract OCR, the LSTM
// engine Google developed and open-sourced, compiled by CloudBurrow to a
// WASI WebAssembly module (third_party/tesseract-wasm) and run on wazero, so
// no cgo is needed and the Linux binary stays static. Vertex AI
// EmbedContent's document_ocr reads scanned PDF pages with it (#1733).
//
// A page is read in two passes, as the tesseract command does with
// --psm 1: orientation and script detection (the osd model) finds how the
// page is turned, the page is turned upright, and the LSTM model
// (tessdata_fast eng, which reads the Latin alphabet) reads it in reading
// order with automatic page segmentation.
package ocr

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	tesseractwasm "github.com/cloudburrow/cloudburrow/third_party/tesseract-wasm"
)

// Tesseract's page segmentation modes and engine modes used here.
const (
	psmAutoOSD   = 1 // automatic page segmentation with orientation and script detection
	oemLegacy    = 0 // the osd model is a legacy-engine model
	oemLSTM      = 1
	tessdataPath = "/tessdata"
)

// MinConfidence is the orientation confidence below which a page is read as
// it is. Tesseract's documentation calls 15 reasonably confident, but a
// scanned page's confidence is lower (about 1.4 on testdata/scan.png in
// every orientation, each detected correctly); a page with little text
// gives under 0.5.
const MinConfidence = 1.0

// Result is what Read finds on a page.
type Result struct {
	// Text is the page's text in reading order, blocks separated by a blank
	// line, lines by a newline.
	Text string
	// Rotation is the clockwise rotation in degrees (0, 90, 180, 270) the
	// page was found turned by, and turned back from before it was read.
	Rotation int
	// Script is the script orientation and script detection found (for
	// example "Latin"), empty when it found none.
	Script string
	// Detected is the clockwise rotation orientation detection reported,
	// and Confidence its confidence (15 is reasonably confident).
	Detected   int
	Confidence float64
}

var (
	rtOnce   sync.Once
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	rtErr    error
	tessFS   fs.FS
	pool     = make(chan *engine, maxEngines)
	slots    = make(chan struct{}, maxEngines)
)

// maxEngines is the most Tesseract instances alive at once; each holds its
// own linear memory (about 100 MB with both models loaded).
const maxEngines = 2

func start() error {
	rtOnce.Do(func() {
		ctx := context.Background()
		rt = wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
		if _, rtErr = wasi_snapshot_preview1.Instantiate(ctx, rt); rtErr != nil {
			return
		}
		if compiled, rtErr = rt.CompileModule(ctx, tesseractwasm.Module); rtErr != nil {
			return
		}
		tessFS, rtErr = loadTessdata()
	})
	if rtErr != nil {
		return fmt.Errorf("starting Tesseract: %v", rtErr)
	}
	return nil
}

// loadTessdata decompresses the embedded models into memory and checks each
// against its published SHA-256.
func loadTessdata() (fs.FS, error) {
	m := fstest.MapFS{}
	for name, want := range tesseractwasm.TessdataSHA256 {
		f, err := tesseractwasm.Tessdata.Open("tessdata/" + name + ".gz")
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		b, err := io.ReadAll(zr)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != want {
			return nil, fmt.Errorf("%s has SHA-256 %s, want %s", name, got, want)
		}
		m[name] = &fstest.MapFile{Data: b, Mode: 0o444}
	}
	return m, nil
}

// engine is one instance of the module with an LSTM engine and an OSD
// engine initialised.
type engine struct {
	mod  api.Module
	mem  api.Memory
	fn   map[string]api.Function
	lstm uint64
	osd  uint64
}

var exports = []string{"cb_malloc", "cb_free", "cb_create", "cb_init", "cb_set_variable", "cb_set_psm",
	"cb_set_image", "cb_get_text", "cb_free_text", "cb_osd", "cb_clear", "cb_delete"}

func newEngine(ctx context.Context) (*engine, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").
		WithFSConfig(wazero.NewFSConfig().WithFSMount(tessFS, tessdataPath))
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("starting Tesseract: %v", err)
	}
	e := &engine{mod: mod, mem: mod.Memory(), fn: map[string]api.Function{}}
	for _, name := range exports {
		f := mod.ExportedFunction(name)
		if f == nil {
			mod.Close(ctx)
			return nil, fmt.Errorf("tesseract.wasm does not export %s", name)
		}
		e.fn[name] = f
	}
	if e.lstm, err = e.initEngine(ctx, "eng", oemLSTM); err != nil {
		mod.Close(ctx)
		return nil, err
	}
	if e.osd, err = e.initEngine(ctx, "osd", oemLegacy); err != nil {
		mod.Close(ctx)
		return nil, err
	}
	return e, nil
}

func (e *engine) call(ctx context.Context, name string, args ...uint64) (uint64, error) {
	r, err := e.fn[name].Call(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("tesseract %s: %v", name, err)
	}
	if len(r) == 0 {
		return 0, nil
	}
	return r[0], nil
}

// cstring copies s, NUL-terminated, into the module's memory.
func (e *engine) cstring(ctx context.Context, s string) (uint64, error) {
	p, err := e.call(ctx, "cb_malloc", uint64(len(s)+1))
	if err != nil || p == 0 {
		return 0, fmt.Errorf("tesseract: out of memory")
	}
	if !e.mem.Write(uint32(p), append([]byte(s), 0)) {
		return 0, fmt.Errorf("tesseract: write out of range")
	}
	return p, nil
}

func (e *engine) readCString(p uint32) string {
	var b []byte
	for {
		c, ok := e.mem.ReadByte(p)
		if !ok || c == 0 {
			return string(b)
		}
		b = append(b, c)
		p++
	}
}

func (e *engine) initEngine(ctx context.Context, lang string, oem int) (uint64, error) {
	h, err := e.call(ctx, "cb_create")
	if err != nil {
		return 0, err
	}
	dp, err := e.cstring(ctx, tessdataPath)
	if err != nil {
		return 0, err
	}
	lp, err := e.cstring(ctx, lang)
	if err != nil {
		return 0, err
	}
	rc, err := e.call(ctx, "cb_init", h, dp, lp, uint64(oem))
	e.call(ctx, "cb_free", dp)
	e.call(ctx, "cb_free", lp)
	if err != nil {
		return 0, err
	}
	if int32(rc) != 0 {
		return 0, fmt.Errorf("tesseract: loading the %s model failed", lang)
	}
	return h, nil
}

func (e *engine) close(ctx context.Context) { e.mod.Close(ctx) }

// acquire returns an idle engine or starts one, waiting while maxEngines are
// busy.
func acquire(ctx context.Context) (*engine, error) {
	if err := start(); err != nil {
		return nil, err
	}
	select {
	case e := <-pool:
		return e, nil
	default:
	}
	select {
	case e := <-pool:
		return e, nil
	case slots <- struct{}{}:
		e, err := newEngine(ctx)
		if err != nil {
			<-slots
			return nil, err
		}
		return e, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release returns a healthy engine to the pool and drops a broken one.
func release(e *engine, ok bool) {
	if ok {
		pool <- e
		return
	}
	e.close(context.Background())
	<-slots
}

// gray is an 8-bit greyscale copy of an image, the form Tesseract binarises.
func gray(img image.Image) *image.Gray {
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			g.Pix[y*g.Stride+x] = color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
		}
	}
	return g
}

// rotate turns g counter-clockwise by deg (0, 90, 180 or 270), undoing a
// clockwise rotation by deg.
func rotate(g *image.Gray, deg int) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	var out *image.Gray
	switch deg {
	case 90:
		out = image.NewGray(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Pix[(w-1-x)*out.Stride+y] = g.Pix[y*g.Stride+x]
			}
		}
	case 180:
		out = image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Pix[(h-1-y)*out.Stride+(w-1-x)] = g.Pix[y*g.Stride+x]
			}
		}
	case 270:
		out = image.NewGray(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Pix[x*out.Stride+(h-1-y)] = g.Pix[y*g.Stride+x]
			}
		}
	default:
		return g
	}
	return out
}

// setImage passes g to engine h.
func (e *engine) setImage(ctx context.Context, h uint64, g *image.Gray, ppi int) error {
	n := len(g.Pix)
	p, err := e.call(ctx, "cb_malloc", uint64(n))
	if err != nil || p == 0 {
		return fmt.Errorf("tesseract: out of memory for a %dx%d page", g.Rect.Dx(), g.Rect.Dy())
	}
	defer e.call(ctx, "cb_free", p)
	if !e.mem.Write(uint32(p), g.Pix) {
		return fmt.Errorf("tesseract: write out of range")
	}
	_, err = e.call(ctx, "cb_set_image", h, p, uint64(g.Rect.Dx()), uint64(g.Rect.Dy()), 1, uint64(g.Stride), uint64(ppi))
	return err
}

// osd finds the page's rotation and script; ok is false when the page has
// too little text to tell.
func (e *engine) detect(ctx context.Context, g *image.Gray, ppi int) (deg int, conf float64, script string, ok bool, err error) {
	if err = e.setImage(ctx, e.osd, g, ppi); err != nil {
		return
	}
	out, err := e.call(ctx, "cb_malloc", 16)
	if err != nil || out == 0 {
		return 0, 0, "", false, fmt.Errorf("tesseract: out of memory")
	}
	defer e.call(ctx, "cb_free", out)
	r, err := e.call(ctx, "cb_osd", e.osd, out)
	defer e.call(ctx, "cb_clear", e.osd)
	if err != nil || int32(r) != 1 {
		return 0, 0, "", false, err
	}
	b, _ := e.mem.Read(uint32(out), 16)
	deg = int(int32(binary.LittleEndian.Uint32(b[0:])))
	conf = float64(int32(binary.LittleEndian.Uint32(b[4:]))) / 100
	if sp := binary.LittleEndian.Uint32(b[12:]); sp != 0 {
		script = e.readCString(sp)
	}
	return deg, conf, script, true, nil
}

func (e *engine) text(ctx context.Context, g *image.Gray, ppi int) (string, error) {
	if err := e.setImage(ctx, e.lstm, g, ppi); err != nil {
		return "", err
	}
	defer e.call(ctx, "cb_clear", e.lstm)
	if _, err := e.call(ctx, "cb_set_psm", e.lstm, psmAutoOSD); err != nil {
		return "", err
	}
	p, err := e.call(ctx, "cb_get_text", e.lstm)
	if err != nil {
		return "", err
	}
	if p == 0 {
		return "", errors.New("tesseract: recognition failed")
	}
	defer e.call(ctx, "cb_free_text", p)
	return e.readCString(uint32(p)), nil
}

// Read recognises the text of img, a page rendered at ppi pixels an inch
// (300 is what Tesseract is trained for).
func Read(ctx context.Context, img image.Image, ppi int) (Result, error) {
	if img.Bounds().Empty() {
		return Result{}, nil
	}
	e, err := acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	ok := false
	defer func() { release(e, ok) }()
	g := gray(img)
	var res Result
	deg, conf, script, found, err := e.detect(ctx, g, ppi)
	if err != nil {
		return Result{}, err
	}
	if found {
		res.Script, res.Detected, res.Confidence = script, deg, conf
		// deg is how far the page must be turned clockwise to stand
		// upright: a page scanned turned 90 degrees clockwise reports 270.
		if turned := (360 - deg) % 360; conf >= MinConfidence && turned != 0 {
			res.Rotation = turned
			g = rotate(g, turned)
		}
	}
	t, err := e.text(ctx, g, ppi)
	if err != nil {
		return Result{}, err
	}
	res.Text = normalize(t)
	ok = true
	return res, nil
}

// normalize trims each line, drops Tesseract's trailing form feed and keeps
// at most one blank line between blocks.
func normalize(t string) string {
	t = strings.ReplaceAll(t, "\f", "")
	var out []string
	blank := false
	for _, l := range strings.Split(t, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
