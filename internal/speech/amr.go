package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	amrwasm "github.com/cloudburrow/cloudburrow/third_party/amr-wasm"
)

// AMR and AMR-WB audio (#1799), Speech-to-Text's AMR (8 kHz) and AMR_WB
// (16 kHz) encodings, decoded with opencore-amr's decoders (Android's
// OpenCORE, Apache-2.0) compiled to WebAssembly (third_party/amr-wasm) and
// run on wazero, so the CLI stays static. The data is the RFC 4867 storage
// format (section 5): the "#!AMR\n" or "#!AMR-WB\n" magic, which auto
// decoding detects, then frames, each a TOC byte (its frame type in bits
// 3-6) and the frame's bytes. An explicit AMR or AMR_WB config may leave
// the magic out. Single-channel files only: the multi-channel magics
// ("#!AMR_MC1.0\n", "#!AMR-WB_MC1.0\n") are refused.

// amrFrameBytes are each frame type's bytes after the TOC byte (RFC 4867's
// tables, 3GPP TS 26.101 and 26.201): types 0-7 (NB) or 0-8 (WB) are
// speech, 8 (NB) or 9 (WB) comfort noise, 15 no data.
var (
	amrNBFrameBytes = [16]int{12, 13, 15, 17, 19, 20, 26, 31, 5, -1, -1, -1, -1, -1, -1, 0}
	amrWBFrameBytes = [16]int{17, 23, 32, 36, 40, 46, 50, 58, 60, 5, -1, -1, -1, -1, 0, 0}
)

var (
	amrOnce     sync.Once
	amrRT       wazero.Runtime
	amrCompiled wazero.CompiledModule
	amrErr      error
)

func amrStart() error {
	amrOnce.Do(func() {
		ctx := context.Background()
		amrRT = wazero.NewRuntime(ctx)
		if _, amrErr = wasi_snapshot_preview1.Instantiate(ctx, amrRT); amrErr != nil {
			return
		}
		amrCompiled, amrErr = amrRT.CompileModule(ctx, amrwasm.Module)
	})
	if amrErr != nil {
		return fmt.Errorf("starting the AMR decoder: %v", amrErr)
	}
	return nil
}

// amrPCM decodes AMR (wide false) or AMR-WB (wide true) storage-format
// data to samples in [-1, 1] at 8 or 16 kHz, at most maxSeconds of them.
func amrPCM(data []byte, wide bool, maxSeconds int) ([]float32, int, error) {
	magic, rate, perFrame, sizes := "#!AMR\n", 8000, 160, amrNBFrameBytes
	name := "AMR"
	if wide {
		magic, rate, perFrame, sizes, name = "#!AMR-WB\n", 16000, 320, amrWBFrameBytes, "AMR-WB"
	}
	switch {
	case bytes.HasPrefix(data, []byte(magic)):
		data = data[len(magic):]
	case bytes.HasPrefix(data, []byte("#!AMR_MC1.0\n")) || bytes.HasPrefix(data, []byte("#!AMR-WB_MC1.0\n")):
		return nil, 0, fmt.Errorf("%w: multi-channel AMR storage format", ErrUnsupported)
	case bytes.HasPrefix(data, []byte("#!AMR")):
		return nil, 0, fmt.Errorf("the audio is not %s: it starts with another AMR magic", name)
	}
	if len(data) == 0 {
		return nil, 0, fmt.Errorf("the %s audio has no frames", name)
	}
	// Check every frame before decoding any.
	frames := 0
	for off := 0; off < len(data); frames++ {
		ft := int(data[off]>>3) & 0x0f
		n := sizes[ft]
		if n < 0 || data[off]&0x83 != 0 { // F and the padding bits are 0 in the storage format
			return nil, 0, fmt.Errorf("invalid %s frame header 0x%02x at byte %d", name, data[off], off)
		}
		if off+1+n > len(data) {
			return nil, 0, fmt.Errorf("the %s audio ends inside a frame at byte %d", name, off)
		}
		off += 1 + n
	}
	if frames*perFrame > maxSeconds*rate {
		return nil, 0, ErrTooLong
	}
	if err := amrStart(); err != nil {
		return nil, 0, err
	}
	ctx := context.Background()
	mod, err := amrRT.InstantiateModule(ctx, amrCompiled, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		return nil, 0, fmt.Errorf("starting the AMR decoder: %v", err)
	}
	defer mod.Close(ctx)
	prefix := "cb_nb_"
	if wide {
		prefix = "cb_wb_"
	}
	fn := map[string]api.Function{}
	for _, n := range []string{"cb_malloc", prefix + "init", prefix + "exit", prefix + "decode"} {
		if fn[n] = mod.ExportedFunction(n); fn[n] == nil {
			return nil, 0, fmt.Errorf("amr.wasm does not export %s", n)
		}
	}
	call := func(n string, args ...uint64) (uint64, error) {
		r, err := fn[n].Call(ctx, args...)
		if err != nil {
			return 0, fmt.Errorf("the AMR decoder's %s: %w", n, err)
		}
		if len(r) == 0 {
			return 0, nil
		}
		return r[0], nil
	}
	st, err := call(prefix + "init")
	if err != nil || st == 0 {
		return nil, 0, errors.Join(errors.New("the AMR decoder could not start"), err)
	}
	defer call(prefix+"exit", st)
	in, err := call("cb_malloc", 64)
	if err != nil || in == 0 {
		return nil, 0, errors.New("the AMR decoder is out of memory")
	}
	out, err := call("cb_malloc", uint64(2*perFrame))
	if err != nil || out == 0 {
		return nil, 0, errors.New("the AMR decoder is out of memory")
	}
	mem := mod.Memory()
	pcm := make([]float32, 0, frames*perFrame)
	for off := 0; off < len(data); {
		n := 1 + sizes[int(data[off]>>3)&0x0f]
		if !mem.Write(uint32(in), data[off:off+n]) {
			return nil, 0, errors.New("the AMR decoder's memory is out of range")
		}
		if _, err := call(prefix+"decode", st, in, out); err != nil {
			return nil, 0, err
		}
		b, ok := mem.Read(uint32(out), uint32(2*perFrame))
		if !ok {
			return nil, 0, errors.New("the AMR decoder's memory is out of range")
		}
		for i := 0; i < perFrame; i++ {
			pcm = append(pcm, float32(int16(binary.LittleEndian.Uint16(b[2*i:])))/32768)
		}
		off += n
	}
	return pcm, rate, nil
}
