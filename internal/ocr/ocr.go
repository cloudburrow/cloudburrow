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
// order with automatic page segmentation and orientation detection (--psm 1:
// --psm 3 would save a second detection, about a tenth of a page's time, but
// reads a page turned a quarter that the first pass was not confident
// about, at 0.2 to 0.4, as garbage). A page OSD finds in another
// script is read with tessdata_fast's model of that script (script/Cyrillic,
// script/HanS, ...), which SetScriptSource's function fetches on demand
// (internal/localai pins each by SHA-256 and keeps it in <state-dir>/models).
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
	"os"
	"runtime"
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
	// Model is the language model the page was read with: "eng", or a
	// tessdata_fast script model such as "script/Cyrillic".
	Model string
	// Detected is the clockwise rotation orientation detection reported,
	// and Confidence its confidence (15 is reasonably confident).
	Detected   int
	Confidence float64
	// Blocks is the page's layout, read by ReadLayout alone; its boxes are
	// in img's coordinates from its bounds' origin.
	Blocks []Block
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
// own linear memory (about 100 MB with both models loaded). Pages are read
// in parallel up to this many: half the CPUs, at least 2 and at most 4.
var maxEngines = min(4, max(2, runtime.NumCPU()/2))

func start() error {
	rtOnce.Do(func() {
		ctx := context.Background()
		// Not WithCloseOnContextDone: it makes every loop iteration of the
		// module leave to Go to check the context, which made a page take
		// 6 s instead of 0.8 s (and 15 s under the race detector, #1761).
		// The context is checked before each call into the module
		// instead (engine.call), so a cancelled read stops after the call
		// it is in, under a second of a page on a laptop.
		rt = wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig())
		if _, rtErr = wasi_snapshot_preview1.Instantiate(ctx, rt); rtErr != nil {
			return
		}
		if compiled, rtErr = rt.CompileModule(ctx, tesseractwasm.Module); rtErr != nil {
			return
		}
		var mem fs.FS
		if mem, rtErr = loadTessdata(); rtErr == nil {
			tessFS = layeredFS{mem}
		}
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

// ScriptModels are tessdata_fast 4.1.0's script models a page can be read
// with, by the name OSD reports for it where the two differ: Han text is read
// with simplified Chinese (OSD does not tell simplified from traditional),
// Korean (Hangul with Han) with Hangul, and kana with Japanese. Latin, and a
// script with no model, are read with eng; so is Vietnamese, which OSD
// reports as Latin.
var ScriptModels = map[string]string{
	"Han": "HanS", "Korean": "Hangul", "Hiragana": "Japanese", "Katakana": "Japanese",
	"Arabic": "Arabic", "Armenian": "Armenian", "Bengali": "Bengali", "Canadian_Aboriginal": "Canadian_Aboriginal",
	"Cherokee": "Cherokee", "Cyrillic": "Cyrillic", "Devanagari": "Devanagari", "Ethiopic": "Ethiopic",
	"Fraktur": "Fraktur", "Georgian": "Georgian", "Greek": "Greek", "Gujarati": "Gujarati", "Gurmukhi": "Gurmukhi",
	"Hangul": "Hangul", "Hebrew": "Hebrew", "Japanese": "Japanese", "Kannada": "Kannada", "Khmer": "Khmer",
	"Lao": "Lao", "Malayalam": "Malayalam", "Myanmar": "Myanmar", "Oriya": "Oriya", "Sinhala": "Sinhala",
	"Syriac": "Syriac", "Tamil": "Tamil", "Telugu": "Telugu", "Thaana": "Thaana", "Thai": "Thai", "Tibetan": "Tibetan",
}

// MinScriptConfidence is the script confidence below which a page is read
// with eng whatever script OSD names.
const MinScriptConfidence = 0.5

var (
	scriptMu     sync.Mutex
	scriptSource func(ctx context.Context, model string) (string, error)
	scriptPaths  = map[string]string{} // model name -> verified file on this host
)

// SetScriptSource installs the function that returns the path of
// tessdata_fast's script model (for example "Cyrillic"), fetching and
// checking it first when needed. Without one, every page is read with eng.
func SetScriptSource(f func(ctx context.Context, model string) (string, error)) {
	scriptMu.Lock()
	defer scriptMu.Unlock()
	scriptSource = f
	scriptPaths = map[string]string{}
}

// scriptModel returns the host path of the script model for script, ""
// when the page is read with eng.
func scriptModel(ctx context.Context, script string) (string, error) {
	name := ScriptModels[script]
	if name == "" {
		return "", nil
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if scriptSource == nil {
		return "", nil
	}
	if _, ok := scriptPaths[name]; !ok {
		p, err := scriptSource(ctx, name)
		if err != nil {
			return "", fmt.Errorf("the Tesseract model for the %s script: %v", script, err)
		}
		scriptPaths[name] = p
	}
	return name, nil
}

// layeredFS is the tessdata directory the module sees: the embedded models,
// and script/<name>.traineddata from the files SetScriptSource's function
// returned.
type layeredFS struct{ mem fs.FS }

func (l layeredFS) Open(name string) (fs.File, error) {
	if rest, ok := strings.CutPrefix(name, "script/"); ok {
		scriptMu.Lock()
		p, found := scriptPaths[strings.TrimSuffix(rest, ".traineddata")]
		scriptMu.Unlock()
		if !found || !strings.HasSuffix(rest, ".traineddata") {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return os.Open(p)
	}
	return l.mem.Open(name)
}

// engine is one instance of the module with an LSTM engine and an OSD
// engine initialised.
type engine struct {
	mod  api.Module
	mem  api.Memory
	fn   map[string]api.Function
	lstm uint64
	osd  uint64
	// scripts are the LSTM engines of script models, started the first time
	// a page in that script is read.
	scripts map[string]uint64
}

var exports = []string{"cb_malloc", "cb_free", "cb_create", "cb_init", "cb_set_variable", "cb_set_psm",
	"cb_set_image", "cb_get_text", "cb_free_text", "cb_get_layout", "cb_osd", "cb_clear", "cb_delete"}

func newEngine(ctx context.Context) (*engine, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").
		WithFSConfig(wazero.NewFSConfig().WithFSMount(tessFS, tessdataPath))
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("starting Tesseract: %v", err)
	}
	e := &engine{mod: mod, mem: mod.Memory(), fn: map[string]api.Function{}, scripts: map[string]uint64{}}
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
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r, err := e.fn[name].Call(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("tesseract %s: %w", name, err)
	}
	if len(r) == 0 {
		return 0, nil
	}
	return r[0], nil
}

// cstring copies s, NUL-terminated, into the module's memory.
func (e *engine) cstring(ctx context.Context, s string) (uint64, error) {
	p, err := e.call(ctx, "cb_malloc", uint64(len(s)+1))
	if err != nil {
		return 0, err
	}
	if p == 0 {
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

// Gray is an 8-bit greyscale copy of img, the form Read passes Tesseract;
// a caller holding several pages for Read keeps them a quarter the size of
// RGBA.
func Gray(img image.Image) *image.Gray { return gray(img) }

// gray is an 8-bit greyscale copy of an image, the form Tesseract binarises.
func gray(img image.Image) *image.Gray {
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	switch src := img.(type) {
	case *image.Gray:
		for y := 0; y < b.Dy(); y++ {
			copy(g.Pix[y*g.Stride:y*g.Stride+b.Dx()], src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return g
	case *image.NRGBA:
		// What color.GrayModel computes from NRGBA's RGBA (premultiplied
		// 16-bit values), without an interface call a pixel.
		for y := 0; y < b.Dy(); y++ {
			row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < b.Dx(); x++ {
				p := row[4*x : 4*x+4]
				a := uint32(p[3])
				rr := uint32(p[0]) * 0x101 * a / 0xff
				gg := uint32(p[1]) * 0x101 * a / 0xff
				bb := uint32(p[2]) * 0x101 * a / 0xff
				g.Pix[y*g.Stride+x] = uint8((19595*rr + 38470*gg + 7471*bb + 1<<15) >> 24)
			}
		}
		return g
	}
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
	if err != nil {
		return err
	}
	if p == 0 {
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
	if err != nil {
		return 0, 0, "", false, err
	}
	if out == 0 {
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
	sconf := float64(int32(binary.LittleEndian.Uint32(b[8:]))) / 100
	if sp := binary.LittleEndian.Uint32(b[12:]); sp != 0 {
		script = e.readCString(sp)
	}
	if sconf < MinScriptConfidence && script != "Latin" {
		script = ""
	}
	return deg, conf, script, true, nil
}

// text reads g with the LSTM engine h.
// text reads g with engine h; with lay, also its layout (layoutOf's
// arguments).
func (e *engine) text(ctx context.Context, h uint64, g *image.Gray, ppi int, lay func(h uint64) error) (string, error) {
	if err := e.setImage(ctx, h, g, ppi); err != nil {
		return "", err
	}
	defer e.call(ctx, "cb_clear", h)
	if _, err := e.call(ctx, "cb_set_psm", h, psmAutoOSD); err != nil {
		return "", err
	}
	p, err := e.call(ctx, "cb_get_text", h)
	if err != nil {
		return "", err
	}
	if p == 0 {
		return "", errors.New("tesseract: recognition failed")
	}
	defer e.call(ctx, "cb_free_text", p)
	if lay != nil {
		if err := lay(h); err != nil {
			return "", err
		}
	}
	return e.readCString(uint32(p)), nil
}

// Read recognises the text of img, a page rendered at ppi pixels an inch
// (300 is what Tesseract is trained for).
func Read(ctx context.Context, img image.Image, ppi int) (Result, error) {
	return read(ctx, img, ppi, false)
}

func read(ctx context.Context, img image.Image, ppi int, withLayout bool) (Result, error) {
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
	h, model := e.lstm, "eng"
	// The script of a page OSD is not confident of the orientation of (a
	// Latin page turned a quarter can be named Devanagari) is not trusted.
	script = res.Script
	if res.Confidence < MinConfidence {
		script = ""
	}
	name, err := scriptModel(ctx, script)
	if err != nil {
		ok = true // the engine is unharmed
		return Result{}, err
	}
	if name != "" {
		sh, have := e.scripts[name]
		if !have {
			if sh, err = e.initEngine(ctx, "script/"+name, oemLSTM); err != nil {
				return Result{}, err
			}
			e.scripts[name] = sh
		}
		h, model = sh, "script/"+name
	}
	res.Model = model
	var lay func(uint64) error
	if withLayout {
		w, h0 := img.Bounds().Dx(), img.Bounds().Dy()
		lay = func(h uint64) (err error) {
			res.Blocks, err = e.layout(ctx, h, res.Rotation, w, h0)
			return err
		}
	}
	t, err := e.text(ctx, h, g, ppi, lay)
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
