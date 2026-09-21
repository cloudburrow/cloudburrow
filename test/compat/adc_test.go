//go:build compat

package compat

import (
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/auth/credentials"
)

// detectADC performs the lookup any Google SDK performs. It only locates and
// parses credentials; it does not fetch a token, so nothing here contacts
// Google.
func detectADC() error {
	_, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	return err
}

// TestApplicationDefaultCredentialsCannotBeFoundDuringClientConstruction
// proves the guard works, rather than asserting that it was called.
//
// It exists because the environment-variable check was believed to be
// sufficient and was not. The official Gen AI SDK calls DetectDefault on its
// Vertex backend even when the base URL is local; on a machine with gcloud
// configured it found the developer's credentials on disk, minted a live
// access token and attached it — with their real quota project — to a request
// aimed at 127.0.0.1. No environment variable was involved.
func TestApplicationDefaultCredentialsCannotBeFoundDuringClientConstruction(t *testing.T) {
	New(t)
	WithoutADC(t, func() {
		if err := detectADC(); err == nil {
			t.Error("application default credentials were found while a client was being " +
				"constructed; an SDK pointed at a local endpoint could send a real Google token to it")
		}
	})
}

// TestTheADCGuardIsLoadBearing is the negative control.
//
// A guard that passes because there was nothing to find proves nothing. This
// makes the same call inside and outside the guard and requires the result to
// differ on a machine that has credentials on disk.
func TestTheADCGuardIsLoadBearing(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to inspect: %v", err)
	}
	wellKnown := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
	if _, err := os.Stat(wellKnown); err != nil {
		t.Skipf("this machine has no credentials on disk, so there is nothing for the guard "+
			"to neutralise and this control cannot run: %v", err)
	}
	if err := detectADC(); err != nil {
		t.Skipf("credentials exist on disk but are not usable (%v); the control cannot "+
			"distinguish a working guard from an empty machine", err)
	}

	New(t)
	WithoutADC(t, func() {
		if err := detectADC(); err == nil {
			t.Fatal("the guard did not neutralise application default credentials")
		}
	})
	t.Logf("control satisfied: credentials at %s are reachable outside the guard "+
		"and unreachable inside it", wellKnown)
}

// The guard borrows the environment and must give it back. A suite that ran
// docker, kind or kubectl under a moved HOME afterwards would fail in ways
// that had nothing to do with what it was testing.
func TestTheADCGuardRestoresTheEnvironment(t *testing.T) {
	New(t)
	before, hadBefore := os.LookupEnv("HOME")

	WithoutADC(t, func() {
		if during := os.Getenv("HOME"); during == before {
			t.Error("HOME was not moved inside the guard, so it does nothing")
		}
	})

	after, hadAfter := os.LookupEnv("HOME")
	if hadBefore != hadAfter || before != after {
		t.Errorf("HOME was not restored: before=%q(%v) after=%q(%v)",
			before, hadBefore, after, hadAfter)
	}
}
