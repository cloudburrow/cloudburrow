package beamrun

import "testing"

// TestLauncherCause: a failed Java launcher's error is its exception and
// last cause, not the trace's last frames.
func TestLauncherCause(t *testing.T) {
	trace := "INFO starting\nException in thread \"main\" java.lang.IllegalArgumentException: bad\n\tat a.B.c(B.java:1)\nCaused by: java.io.IOException: x\n\tat d.E.f(E.java:2)\nCaused by: java.net.ConnectException: refused\n\tat g.H.i(H.java:3)\n"
	if got, want := launcherCause(trace), "Exception in thread \"main\" java.lang.IllegalArgumentException: bad | Caused by: java.net.ConnectException: refused"; got != want {
		t.Errorf("launcherCause = %q; want %q", got, want)
	}
	if got := launcherCause("a\nb\nc\nd\n"); got != "b | c | d" {
		t.Errorf("launcherCause(plain) = %q", got)
	}
}
