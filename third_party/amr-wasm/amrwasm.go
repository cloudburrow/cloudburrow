// Package amrwasm holds opencore-amr's AMR-NB and AMR-WB decoders compiled
// to a WASI WebAssembly module (amr.wasm, built by build.sh from the pinned
// opencore-amr release). internal/speech runs the module on wazero to read
// Speech-to-Text's AMR and AMR_WB audio (#1799). PROVENANCE.md records
// every input.
package amrwasm

import _ "embed"

// Module is amr.wasm.
//
//go:embed amr.wasm
var Module []byte
