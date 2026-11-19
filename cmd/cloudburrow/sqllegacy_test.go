package main

import (
	"strings"
	"testing"
)

// TestAMD64Unrunnable (#1524): a node runs MYSQL_5_7's linux/amd64 image
// natively, or through an enabled x86-64 binfmt_misc handler with the F
// flag; anything else says why not.
func TestAMD64Unrunnable(t *testing.T) {
	for probe, runs := range map[string]bool{
		"x86_64\n":                          true,
		"aarch64\nrosetta enabled POCF\n":   true,
		"aarch64\nx86_64 enabled POCF\n":    true,
		"aarch64\n":                         false,
		"aarch64\nx86_64 disabled POCF\n":   false,
		"aarch64\nqemu-x86_64 enabled OC\n": false,
	} {
		why := amd64Unrunnable(probe)
		if (why == "") != runs {
			t.Errorf("%q: why %q, want runs %v", probe, why, runs)
		}
		if !runs && !strings.Contains(why, "aarch64") {
			t.Errorf("%q: why %q does not name the machine", probe, why)
		}
	}
}
