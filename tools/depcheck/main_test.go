package main

import "testing"

// A tag of v1.56.1 and an inventory entry of 1.56.1 are the same release.
// Reporting that as an update would train a reader to ignore the output.
func TestNormalizeIgnoresTheVPrefix(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"v1.56.1", "1.56.1"},
		{"1.56.1", "1.56.1"},
		{" v0.33.0 ", "0.33.0"},
	}
	for _, p := range pairs {
		if got := normalize(p[0]); got != p[1] {
			t.Errorf("normalize(%q) = %q, want %q", p[0], got, p[1])
		}
	}
	if normalize("v1.56.1") == normalize("v1.57.0") {
		t.Error("normalize collapsed genuinely different versions")
	}
}
