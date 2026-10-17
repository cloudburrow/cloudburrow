package firestoreadmin

import (
	"bytes"
	"testing"
)

// The output files are LevelDB log files: records split across 32 KiB
// blocks read back whole.
func TestLogRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := &logWriter{w: &buf}
	want := [][]byte{[]byte("a"), bytes.Repeat([]byte("x"), 70000), {}, bytes.Repeat([]byte("y"), logBlock-2*logHeader-1), []byte("tail")}
	for _, r := range want {
		w.record(r)
	}
	got, err := readLog(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("record %d: %d bytes, want %d", i, len(got[i]), len(want[i]))
		}
	}
	b := buf.Bytes()
	b[10] ^= 1
	if _, err := readLog(b); err == nil {
		t.Error("a corrupted file read without error")
	}
}
