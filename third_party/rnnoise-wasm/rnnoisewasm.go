// Package rnnoisewasm holds Xiph's RNNoise noise suppressor, with its
// built-in model, compiled to a WASI WebAssembly module (rnnoise.wasm,
// built by build.sh from the pinned RNNoise release). internal/speech runs
// it on wazero for Speech-to-Text's denoiserConfig (#1799). PROVENANCE.md
// records every input.
package rnnoisewasm

import _ "embed"

// Module is rnnoise.wasm.
//
//go:embed rnnoise.wasm
var Module []byte
