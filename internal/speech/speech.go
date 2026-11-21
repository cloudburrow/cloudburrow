// Package speech transcribes audio with whisper.cpp, compiled by CloudBurrow
// to a WASI WebAssembly module (third_party/whisper-wasm) and run on wazero,
// so no cgo is needed and the Linux binary stays static. BigQuery ML's
// ML.TRANSCRIBE over a CLOUD_AI_SPEECH_TO_TEXT_V2 remote model reads audio
// with it, through the local Vertex AI endpoint's Speech-to-Text v2
// recognize route (internal/service/vertexai/speech.go, #1799).
//
// The ggml model is not embedded: SetModelSource's function returns the
// path of a verified copy (internal/localai pins it by SHA-256 and fetches
// it into <state-dir>/models the first time it is needed).
package speech

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	whisperwasm "github.com/cloudburrow/cloudburrow/third_party/whisper-wasm"
)

// SampleRate is the rate Transcribe takes: mono samples at 16 kHz.
const SampleRate = 16000

// ErrNoModel is returned when no model source is installed.
var ErrNoModel = errors.New("no speech recognition model is installed on this instance")

// Options are a transcription's settings.
type Options struct {
	// Language is a whisper language code ("en", "de", ...), or "" to
	// detect the language.
	Language string
	// Words computes each word's offsets and confidence.
	Words bool
	// Prompt is whisper's initial prompt, prepended to every decoding
	// window: text the speech is likely to contain.
	Prompt string
	// Phrases bias the decoder towards each phrase (speech adaptation).
	Phrases []Phrase
}

// Phrase is a phrase the decoder is biased towards.
type Phrase struct {
	Text string
	// Bias is added to the logit of each of the phrase's tokens where
	// the text decoded so far could continue with it.
	Bias float64
}

// ChunkSeconds is the longest stretch of audio given to whisper.cpp at
// once: longer audio is cut, at its quietest point near the limit, into
// pieces transcribed one after another, so a file of hours never needs
// all its samples inside the module's 4 GB memory.
const ChunkSeconds = 10 * 60

// Word is one recognised word.
type Word struct {
	Text       string
	Start, End time.Duration
	Confidence float64
	// Speaker is who spoke it, from 1, when the audio was diarized
	// (internal/speaker, #2131), 0 otherwise.
	Speaker int
}

// Segment is one stretch of speech whisper.cpp recognised.
type Segment struct {
	Text       string
	Start, End time.Duration
	// Confidence is the mean probability of the segment's text tokens.
	Confidence float64
	Words      []Word
	// Channel is the 1-based audio channel the segment was heard on with
	// per-channel recognition, 0 otherwise.
	Channel int
}

// Result is a transcription.
type Result struct {
	// Language is the language code whisper used (the one asked for, or
	// the one it detected).
	Language string
	Segments []Segment
}

var (
	rtOnce   sync.Once
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	rtErr    error

	srcMu     sync.Mutex
	modelSrc  func(ctx context.Context) (string, error)
	modelPath string

	// eng is the one live module instance with the model loaded (about
	// 300 MB of linear memory): transcriptions run one at a time.
	engMu sync.Mutex
	eng   *engine
)

// SetModelSource installs the function that returns the path of the ggml
// model, fetching and checking it first when needed.
func SetModelSource(f func(ctx context.Context) (string, error)) {
	srcMu.Lock()
	defer srcMu.Unlock()
	modelSrc, modelPath = f, ""
}

// HasModelSource reports whether a model source is installed.
func HasModelSource() bool {
	srcMu.Lock()
	defer srcMu.Unlock()
	return modelSrc != nil
}

func model(ctx context.Context) (string, error) {
	srcMu.Lock()
	defer srcMu.Unlock()
	if modelSrc == nil {
		return "", ErrNoModel
	}
	if modelPath == "" {
		p, err := modelSrc(ctx)
		if err != nil {
			return "", fmt.Errorf("the speech recognition model: %v", err)
		}
		modelPath = p
	}
	return modelPath, nil
}

func start() error {
	rtOnce.Do(func() {
		ctx := context.Background()
		// whisper.cpp's ggml throws C++ exceptions (as wasm exnref
		// exceptions) on a bad model; wazero runs them with this feature.
		rt = wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
			WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesExceptionHandling))
		if _, rtErr = wasi_snapshot_preview1.Instantiate(ctx, rt); rtErr != nil {
			return
		}
		compiled, rtErr = rt.CompileModule(ctx, whisperwasm.Module)
	})
	if rtErr != nil {
		return fmt.Errorf("starting whisper.cpp: %v", rtErr)
	}
	return nil
}

type engine struct {
	mod api.Module
	mem api.Memory
	fn  map[string]api.Function
	ctx uint64
}

var exports = []string{"cb_malloc", "cb_free", "cb_init", "cb_free_ctx", "cb_lang_id", "cb_full", "cb_full_adapt", "cb_lang",
	"cb_n_segments", "cb_segment_text", "cb_segment_times", "cb_n_tokens", "cb_token_text", "cb_token_data"}

func newEngine(ctx context.Context, path string) (*engine, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the speech recognition model: %v", err)
	}
	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		return nil, fmt.Errorf("starting whisper.cpp: %v", err)
	}
	e := &engine{mod: mod, mem: mod.Memory(), fn: map[string]api.Function{}}
	for _, name := range exports {
		f := mod.ExportedFunction(name)
		if f == nil {
			mod.Close(ctx)
			return nil, fmt.Errorf("whisper.wasm does not export %s", name)
		}
		e.fn[name] = f
	}
	p, err := e.bytes(ctx, b)
	if err != nil {
		mod.Close(ctx)
		return nil, err
	}
	h, err := e.call(ctx, "cb_init", p, uint64(len(b)))
	e.call(ctx, "cb_free", p)
	if err != nil || h == 0 {
		mod.Close(ctx)
		if err == nil {
			err = errors.New("whisper.cpp could not load the model")
		}
		return nil, err
	}
	e.ctx = h
	return e, nil
}

func (e *engine) call(ctx context.Context, name string, args ...uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r, err := e.fn[name].Call(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("whisper %s: %w", name, err)
	}
	if len(r) == 0 {
		return 0, nil
	}
	return r[0], nil
}

// bytes copies b into the module's memory.
func (e *engine) bytes(ctx context.Context, b []byte) (uint64, error) {
	p, err := e.call(ctx, "cb_malloc", uint64(len(b)))
	if err != nil {
		return 0, err
	}
	if p == 0 {
		return 0, errors.New("whisper: out of memory")
	}
	if !e.mem.Write(uint32(p), b) {
		return 0, errors.New("whisper: write out of range")
	}
	return p, nil
}

func (e *engine) cstring(p uint64) string {
	if p == 0 {
		return ""
	}
	var b []byte
	for q := uint32(p); ; q++ {
		c, ok := e.mem.ReadByte(q)
		if !ok || c == 0 {
			return string(b)
		}
		b = append(b, c)
	}
}

// LanguageKnown reports whether whisper.cpp has a language code.
func LanguageKnown(ctx context.Context, code string) (bool, error) {
	var known bool
	err := withEngine(ctx, func(e *engine) error {
		p, err := e.bytes(ctx, append([]byte(code), 0))
		if err != nil {
			return err
		}
		defer e.call(ctx, "cb_free", p)
		id, err := e.call(ctx, "cb_lang_id", p)
		known = int32(id) >= 0
		return err
	})
	return known, err
}

func withEngine(ctx context.Context, f func(*engine) error) error {
	if err := start(); err != nil {
		return err
	}
	path, err := model(ctx)
	if err != nil {
		return err
	}
	engMu.Lock()
	defer engMu.Unlock()
	if eng == nil {
		if eng, err = newEngine(ctx, path); err != nil {
			return err
		}
	}
	if err := f(eng); err != nil {
		// A trap or cancelled call leaves the instance in an unknown state.
		eng.mod.Close(context.Background())
		eng = nil
		return err
	}
	return nil
}

func centis(v int64) time.Duration { return time.Duration(v) * 10 * time.Millisecond }

// Transcribe recognises the speech in samples (mono, SampleRate), in
// pieces of at most ChunkSeconds.
func Transcribe(ctx context.Context, samples []float32, opt Options) (Result, error) {
	var res Result
	for off := 0; off < len(samples); {
		end := CutPoint(samples, off, ChunkSeconds*SampleRate)
		part, err := transcribeChunk(ctx, samples[off:end], opt)
		if err != nil {
			return res, err
		}
		if res.Language == "" {
			res.Language = part.Language
		}
		shift := time.Duration(off) * time.Second / SampleRate
		for _, seg := range part.Segments {
			seg.Start += shift
			seg.End += shift
			for i := range seg.Words {
				seg.Words[i].Start += shift
				seg.Words[i].End += shift
			}
			res.Segments = append(res.Segments, seg)
		}
		off = end
	}
	return res, nil
}

// CutPoint is where the piece of samples starting at off ends: off+max, or
// the end, or, in the last tenth of the piece, the start of its quietest
// 100 ms, so a cut falls between words.
func CutPoint(samples []float32, off, max int) int {
	if len(samples)-off <= max {
		return len(samples)
	}
	const win = SampleRate / 10
	best, bestE := off+max, math.Inf(1)
	for at := off + max - max/10; at+win <= off+max; at += win / 2 {
		var e float64
		for _, v := range samples[at : at+win] {
			e += float64(v) * float64(v)
		}
		if e < bestE {
			best, bestE = at+win/2, e
		}
	}
	return best
}

// biasSpec writes phrases as cb_full_adapt's lines.
func biasSpec(phrases []Phrase) string {
	var b strings.Builder
	for _, p := range phrases {
		text := strings.Join(strings.Fields(p.Text), " ")
		if text == "" || p.Bias == 0 {
			continue
		}
		fmt.Fprintf(&b, "%g\t%s\n", p.Bias, text)
	}
	return b.String()
}

func transcribeChunk(ctx context.Context, samples []float32, opt Options) (Result, error) {
	var res Result
	if len(samples) == 0 {
		return res, nil
	}
	lang := opt.Language
	if lang == "" {
		lang = "auto"
	}
	err := withEngine(ctx, func(e *engine) error {
		buf := make([]byte, 4*len(samples))
		for i, s := range samples {
			binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(s))
		}
		sp, err := e.bytes(ctx, buf)
		if err != nil {
			return err
		}
		defer e.call(ctx, "cb_free", sp)
		lp, err := e.bytes(ctx, append([]byte(lang), 0))
		if err != nil {
			return err
		}
		defer e.call(ctx, "cb_free", lp)
		words := uint64(0)
		if opt.Words {
			words = 1
		}
		var pp, bp uint64
		if opt.Prompt != "" {
			if pp, err = e.bytes(ctx, append([]byte(opt.Prompt), 0)); err != nil {
				return err
			}
			defer e.call(ctx, "cb_free", pp)
		}
		if spec := biasSpec(opt.Phrases); spec != "" {
			if bp, err = e.bytes(ctx, append([]byte(spec), 0)); err != nil {
				return err
			}
			defer e.call(ctx, "cb_free", bp)
		}
		rc, err := e.call(ctx, "cb_full_adapt", e.ctx, sp, uint64(len(samples)), lp, words, pp, bp)
		if err != nil {
			return err
		}
		if int32(rc) != 0 {
			return fmt.Errorf("whisper.cpp failed to transcribe the audio (%d)", int32(rc))
		}
		lc, err := e.call(ctx, "cb_lang", e.ctx)
		if err != nil {
			return err
		}
		res.Language = e.cstring(lc)
		out, err := e.call(ctx, "cb_malloc", 24)
		if err != nil {
			return err
		}
		defer e.call(ctx, "cb_free", out)
		n, err := e.call(ctx, "cb_n_segments", e.ctx)
		if err != nil {
			return err
		}
		for i := uint64(0); i < uint64(int32(n)); i++ {
			tp, err := e.call(ctx, "cb_segment_text", e.ctx, i)
			if err != nil {
				return err
			}
			if _, err := e.call(ctx, "cb_segment_times", e.ctx, i, out); err != nil {
				return err
			}
			tb, _ := e.mem.Read(uint32(out), 16)
			seg := Segment{Text: strings.TrimSpace(e.cstring(tp)),
				Start: centis(int64(binary.LittleEndian.Uint64(tb[0:]))),
				End:   centis(int64(binary.LittleEndian.Uint64(tb[8:])))}
			nt, err := e.call(ctx, "cb_n_tokens", e.ctx, i)
			if err != nil {
				return err
			}
			var sum float64
			var count int
			var cur *Word
			for j := uint64(0); j < uint64(int32(nt)); j++ {
				if _, err := e.call(ctx, "cb_token_data", e.ctx, i, j, out); err != nil {
					return err
				}
				d, _ := e.mem.Read(uint32(out), 24)
				if math.Float32frombits(binary.LittleEndian.Uint32(d[20:])) == 0 {
					continue // a special or timestamp token
				}
				p := float64(math.Float32frombits(binary.LittleEndian.Uint32(d[16:])))
				sum += p
				count++
				if !opt.Words {
					continue
				}
				ttp, err := e.call(ctx, "cb_token_text", e.ctx, i, j)
				if err != nil {
					return err
				}
				text := e.cstring(ttp)
				t0 := centis(int64(binary.LittleEndian.Uint64(d[0:])))
				t1 := centis(int64(binary.LittleEndian.Uint64(d[8:])))
				// A token starting with a space starts a word; others
				// (sub-word pieces, punctuation) continue the current one.
				if cur == nil || strings.HasPrefix(text, " ") {
					if strings.TrimSpace(text) == "" {
						continue
					}
					seg.Words = append(seg.Words, Word{Text: strings.TrimSpace(text), Start: t0, End: t1, Confidence: p})
					cur = &seg.Words[len(seg.Words)-1]
					cur.Confidence = p
					continue
				}
				cur.Text += text
				cur.End = max(cur.End, t1)
				cur.Confidence = min(cur.Confidence, p)
			}
			if count > 0 {
				seg.Confidence = sum / float64(count)
			}
			if seg.Text != "" {
				res.Segments = append(res.Segments, seg)
			}
		}
		return nil
	})
	return res, err
}
