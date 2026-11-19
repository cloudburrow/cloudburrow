// Package whisperwasm holds whisper.cpp compiled to a WASI WebAssembly module
// (whisper.wasm, built by build.sh from the pinned whisper.cpp release).
// internal/speech runs the module on wazero; the ggml model it reads is not
// embedded but fetched at first use (internal/localai's WhisperModel).
// PROVENANCE.md records every input.
package whisperwasm

import _ "embed"

// Module is whisper.wasm.
//
//go:embed whisper.wasm
var Module []byte
