package main

import (
	"strings"
	"testing"
	"time"
)

func TestStampedWriterStampsEachLineOnce(t *testing.T) {
	var b strings.Builder
	w := newStampedWriter(&b)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return at }

	// A line split across writes is stamped once, where it starts.
	for _, chunk := range []string{"first li", "ne\nsecond\nthi", "rd\n"} {
		n, err := w.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want the caller's own count", chunk, n, err)
		}
	}
	stamp := at.Format(time.RFC3339Nano) + " "
	want := stamp + "first line\n" + stamp + "second\n" + stamp + "third\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
	// And the stamp is the one `logs` parses.
	if ts, msg := splitTimestamp(strings.SplitN(b.String(), "\n", 2)[0]); !ts.Equal(at) || msg != "first line" {
		t.Errorf("splitTimestamp read %v %q", ts, msg)
	}
}
