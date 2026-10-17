package run

import "testing"

// TestIsSSMInstanceName: the names the Run port's one gRPC IAMPolicy
// service passes to Secure Source Manager's instances (#1304); a
// function's, and a repository's, stay where they were.
func TestIsSSMInstanceName(t *testing.T) {
	for name, want := range map[string]bool{
		"projects/p/locations/us-central1/instances/i":    true,
		"projects/p/locations/us-central1/functions/f":    false,
		"projects/p/locations/us-central1/repositories/r": false,
		"projects/p/locations/us-central1/instances/":     false,
		"projects/p/locations/us-central1/instances/i/x":  false,
	} {
		if got := isSSMInstanceName(name); got != want {
			t.Errorf("isSSMInstanceName(%q) = %v, want %v", name, got, want)
		}
	}
}
