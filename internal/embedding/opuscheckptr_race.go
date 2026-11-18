//go:build race

package embedding

import (
	"errors"
	"runtime/debug"
	"strings"
	"sync"
)

// opusCheckptr reports why the Opus decoder cannot run in this binary, or
// nil when it can (#1808).
//
// -race turns on checkptr in every package. github.com/kazzmir/opus-go is
// libopus transpiled to Go: its libc shim hands out Go-heap memory (a
// pseudo-stack and malloc'd blocks) as uintptr, and the decoder converts
// those back with unsafe.Pointer, which checkptr rejects with a fatal
// error, not a panic, on the first opus.NewDecoder. That kills the whole
// test binary, so every later test in the package goes unreported. v1.7.0
// is upstream's newest release and its master (kazzmir/opus-go#79 tracks
// it). The binary is fine when checkptr is off for opus-go alone, which
// `make test-race` (RACE_GCFLAGS) and CI do; a race build without that
// flag gets this error instead of the crash.
var opusCheckptr = sync.OnceValue(func() error {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "-gcflags" && strings.Contains(s.Value, "checkptr=0") {
				return nil
			}
		}
	}
	return errors.New("this -race build has checkptr on in github.com/kazzmir/opus-go, whose transpiled decoder checkptr kills (#1808): " +
		"run `make test-race` (RACE_PKGS=./internal/embedding/ for one package), or add -gcflags='github.com/kazzmir/opus-go/...=-d=checkptr=0'")
})
