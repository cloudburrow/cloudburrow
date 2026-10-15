package main

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestBaseFor(t *testing.T) {
	ids := []int64{100, 200, 300}
	for _, c := range []struct {
		at   int64
		want int64
		ok   bool
	}{{99, 0, false}, {100, 100, true}, {250, 200, true}, {1000, 300, true}} {
		got, ok := baseFor(ids, time.Unix(c.at, 5e8))
		if c.at == 99 {
			got, ok = baseFor(ids, time.Unix(c.at, 0))
		}
		if got != c.want || ok != c.ok {
			t.Errorf("baseFor(%d) = %d, %v; want %d, %v", c.at, got, ok, c.want, c.ok)
		}
	}
}

func TestParseEpoch(t *testing.T) {
	for in, want := range map[string]time.Time{
		"1792100967.375349009": time.Unix(1792100967, 375349009),
		"1792100967.5":         time.Unix(1792100967, 500000000),
		"1792100967":           time.Unix(1792100967, 0),
	} {
		if got, err := parseEpoch(in); err != nil || !got.Equal(want) {
			t.Errorf("parseEpoch(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseEpoch("x"); err == nil {
		t.Error("parseEpoch(x): no error")
	}
}

// binlogEvent is one event: a header and body, its size covering a
// 4-byte checksum.
func binlogEvent(typ byte, body []byte) []byte {
	ev := make([]byte, 19, 19+len(body)+4)
	ev[4] = typ
	binary.LittleEndian.PutUint32(ev[9:13], uint32(19+len(body)+4))
	return append(append(ev, body...), 0, 0, 0, 0)
}

func gtidBody(micros int64) []byte {
	body := make([]byte, 1+16+8+1+8+8+7+7)
	var ts [8]byte
	binary.LittleEndian.PutUint64(ts[:], uint64(micros)|1<<55)
	copy(body[42:49], ts[:7])
	return body
}

func TestFirstCommitAfter(t *testing.T) {
	raw := []byte("\xfebin")
	raw = append(raw, binlogEvent(15, make([]byte, 80))...) // format description
	first := int64(len(raw))
	raw = append(raw, binlogEvent(34, gtidBody(1_000_000_500_000))...)
	raw = append(raw, binlogEvent(2, []byte("BEGIN"))...)
	second := int64(len(raw))
	raw = append(raw, binlogEvent(33, gtidBody(1_000_000_700_000))...)
	raw = append(raw, 1, 2, 3) // an event being written
	for _, c := range []struct {
		at    time.Time
		from  int64
		pos   int64
		found bool
	}{
		{time.UnixMicro(1_000_000_400_000), 4, first, true},
		{time.UnixMicro(1_000_000_500_000), 4, second, true}, // a commit at the target is kept
		{time.UnixMicro(1_000_000_600_000), first, second, true},
		{time.UnixMicro(1_000_000_700_000), 4, 0, false},
	} {
		pos, found, err := firstCommitAfter(raw, c.from, c.at)
		if err != nil || pos != c.pos || found != c.found {
			t.Errorf("firstCommitAfter(%v, %d) = %d, %v, %v; want %d, %v", c.at, c.from, pos, found, err, c.pos, c.found)
		}
	}
	if _, _, err := firstCommitAfter([]byte("nope"), 4, time.Now()); err == nil {
		t.Error("not a binary log: no error")
	}
}
