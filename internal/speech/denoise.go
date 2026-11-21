package speech

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	rnnoisewasm "github.com/cloudburrow/cloudburrow/third_party/rnnoise-wasm"
)

// Denoising (denoiserConfig.denoiseAudio, #1799): "Denoise audio before
// sending to the transcription model" (the v2 reference's DenoiserConfig,
// read 2026-11-20). The samples are cleaned with Xiph's RNNoise
// (BSD-3-Clause, its built-in model) compiled to WebAssembly
// (third_party/rnnoise-wasm) and run on wazero: resampled to the 48 kHz it
// reads, denoised 480 samples (10 ms) at a time, its two frames of delay
// (measured: TestDenoise) taken off, and resampled back.

const (
	rnnoiseRate  = 48000
	rnnoiseFrame = 480
	rnnoiseDelay = 2 * rnnoiseFrame
)

var (
	rnnOnce     sync.Once
	rnnRT       wazero.Runtime
	rnnCompiled wazero.CompiledModule
	rnnErr      error
)

// Denoise is samples (mono, SampleRate) with their background noise
// suppressed.
func Denoise(ctx context.Context, samples []float32) ([]float32, error) {
	if len(samples) == 0 {
		return samples, nil
	}
	rnnOnce.Do(func() {
		c := context.Background()
		rnnRT = wazero.NewRuntime(c)
		if _, rnnErr = wasi_snapshot_preview1.Instantiate(c, rnnRT); rnnErr != nil {
			return
		}
		rnnCompiled, rnnErr = rnnRT.CompileModule(c, rnnoisewasm.Module)
	})
	if rnnErr != nil {
		return nil, fmt.Errorf("starting the denoiser: %v", rnnErr)
	}
	mod, err := rnnRT.InstantiateModule(ctx, rnnCompiled, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		return nil, fmt.Errorf("starting the denoiser: %v", err)
	}
	defer mod.Close(context.Background())
	fn := map[string]api.Function{}
	for _, n := range []string{"cb_malloc", "cb_create", "cb_destroy", "cb_process", "cb_frame_size"} {
		if fn[n] = mod.ExportedFunction(n); fn[n] == nil {
			return nil, fmt.Errorf("rnnoise.wasm does not export %s", n)
		}
	}
	call := func(n string, args ...uint64) (uint64, error) {
		r, err := fn[n].Call(ctx, args...)
		if err != nil {
			return 0, fmt.Errorf("the denoiser's %s: %w", n, err)
		}
		if len(r) == 0 {
			return 0, nil
		}
		return r[0], nil
	}
	if fs, err := call("cb_frame_size"); err != nil || int(fs) != rnnoiseFrame {
		return nil, errors.Join(fmt.Errorf("the denoiser's frame is %d samples, not %d", fs, rnnoiseFrame), err)
	}
	st, err := call("cb_create")
	if err != nil || st == 0 {
		return nil, errors.Join(errors.New("the denoiser could not start"), err)
	}
	defer call("cb_destroy", st)
	in, err1 := call("cb_malloc", 4*rnnoiseFrame)
	out, err2 := call("cb_malloc", 4*rnnoiseFrame)
	if err1 != nil || err2 != nil || in == 0 || out == 0 {
		return nil, errors.New("the denoiser is out of memory")
	}
	mem := mod.Memory()
	hi := embedding.Resample(samples, SampleRate, rnnoiseRate)
	n := len(hi)
	// Two frames more, for the delay the denoiser's output has.
	padded := make([]float32, (n/rnnoiseFrame+3)*rnnoiseFrame)
	copy(padded, hi)
	clean := make([]float32, 0, len(padded))
	buf := make([]byte, 4*rnnoiseFrame)
	for off := 0; off < len(padded); off += rnnoiseFrame {
		for i, v := range padded[off : off+rnnoiseFrame] {
			binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(v*32768))
		}
		if !mem.Write(uint32(in), buf) {
			return nil, errors.New("the denoiser's memory is out of range")
		}
		if _, err := call("cb_process", st, out, in); err != nil {
			return nil, err
		}
		b, ok := mem.Read(uint32(out), 4*rnnoiseFrame)
		if !ok {
			return nil, errors.New("the denoiser's memory is out of range")
		}
		for i := 0; i < rnnoiseFrame; i++ {
			v := math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])) / 32768
			clean = append(clean, max(-1, min(1, v)))
		}
	}
	return embedding.Resample(clean[rnnoiseDelay:rnnoiseDelay+n], rnnoiseRate, SampleRate), nil
}
