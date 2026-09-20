package version

import (
	"strings"
	"testing"
)

func TestGetFillsUnknownFields(t *testing.T) {
	i := Get()

	// An unstamped test binary must still report identifiable values rather
	// than empty strings, so a bug report from a hand-built binary is useful.
	for _, f := range []struct{ name, got string }{
		{"Version", i.Version},
		{"Commit", i.Commit},
		{"BuildDate", i.BuildDate},
		{"GoVersion", i.GoVersion},
		{"Platform", i.Platform},
	} {
		if f.got == "" {
			t.Errorf("Info.%s is empty; expected a value or a placeholder", f.name)
		}
	}
}

func TestStringIncludesVersionAndPlatform(t *testing.T) {
	i := Info{Version: "1.2.3", Commit: "abc", BuildDate: "today", GoVersion: "go1.26.0", Platform: "linux/amd64"}
	got := i.String()
	for _, want := range []string{"1.2.3", "abc", "today", "go1.26.0", "linux/amd64"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}
