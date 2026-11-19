package embedding

import "testing"

// TestG711 checks the G.711 decoders against the standard's table values
// (ITU-T G.711, as Sun's g711.c reference computes them): mu-law 0xFF and
// A-law 0xD5 are the smallest positive steps, 0x80 and 0xAA full scale.
func TestG711(t *testing.T) {
	for _, c := range []struct {
		got  float32
		want int
	}{
		{MuLaw(0xFF), 0}, {MuLaw(0x7F), 0}, {MuLaw(0x80), 32124}, {MuLaw(0x00), -32124}, {MuLaw(0xFE), 8},
		{ALaw(0xD5), 8}, {ALaw(0x55), -8}, {ALaw(0xAA), 32256}, {ALaw(0x2A), -32256},
	} {
		if int(c.got*32768) != c.want {
			t.Errorf("got %v, want %d", c.got*32768, c.want)
		}
	}
}
