// Package version reports build identification for the cloudburrow binary.
//
// Values are injected at link time by the build; see the Makefile. An
// unstamped build reports "dev" rather than inventing a version number.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Values stamped via -ldflags at build time.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info describes the running binary.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
	Platform  string
}

// Get returns build information, falling back to the Go build info embedded by
// the toolchain when link-time values are absent. Unknown fields are reported
// as "unknown" so that a stale or hand-built binary is identifiable as such.
func Get() Info {
	i := Info{
		Version:   version,
		Commit:    commit,
		BuildDate: date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	// go install and plain `go build` do not pass ldflags, but the toolchain
	// still embeds VCS data when building from a clean checkout.
	if bi, ok := debug.ReadBuildInfo(); ok {
		if i.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			i.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				if i.BuildDate == "" {
					i.BuildDate = s.Value
				}
			}
		}
	}

	if i.Version == "" {
		i.Version = "dev"
	}
	if i.Commit == "" {
		i.Commit = "unknown"
	}
	if i.BuildDate == "" {
		i.BuildDate = "unknown"
	}
	return i
}

// String returns a single-line summary.
func (i Info) String() string {
	return fmt.Sprintf("cloudburrow %s (commit %s, built %s, %s, %s)",
		i.Version, i.Commit, i.BuildDate, i.GoVersion, i.Platform)
}
