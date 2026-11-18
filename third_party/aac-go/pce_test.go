package aac

import (
	"errors"
	"slices"
	"testing"

	"github.com/cloudburrow/cloudburrow/third_party/aac-go/internal/bits"
)

// TestParseASCProgramConfig (#1651): the AudioSpecificConfig ffmpeg 9's
// encoder writes for 7.1 (channel configuration 0 and a
// program_config_element: a centre, two front pairs, a back pair and the
// LFE, then its "Lavc" comment) gives eight named channels; a layout with
// two front centres is refused as unsupported, not invalid.
func TestParseASCProgramConfig(t *testing.T) {
	asc := []byte{0x14, 0x00, 0x06, 0x0c, 0x05, 0x00, 0x01, 0x08, 0xc8, 0x00, 0x04, 0x4c, 0x61, 0x76, 0x63, 0x56, 0xe5, 0x00}
	a, err := ParseASC(asc)
	if err != nil {
		t.Fatal(err)
	}
	if a.ChannelConfiguration != 0 || a.program == nil || a.program.channels != 8 {
		t.Fatalf("configuration %d, program %+v", a.ChannelConfiguration, a.program)
	}
	d, err := NewDecoderASC(asc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"FC", "FL", "FR", "FLC", "FRC", "BL", "BR", "LFE"}
	if got := d.ChannelNames(); !slices.Equal(got, want) || d.OutputChannels() != 8 {
		t.Errorf("channels %d %v, want 8 %v", d.OutputChannels(), got, want)
	}
	// Two front SCEs (two centres).
	w := bits.NewWriter()
	for _, f := range [][2]uint64{{2, 5}, {8, 4}, {0, 4}, {0, 3}, // ASC, GASpecificConfig
		{0, 4}, {1, 2}, {8, 4}, {2, 4}, {0, 4}, {0, 4}, {0, 2}, {0, 3}, {0, 4}, {0, 3}, // PCE counts
		{0, 1}, {0, 4}, {0, 1}, {1, 4}} { // two SCEs, tags 0 and 1
		_ = w.WriteBits(f[0], int(f[1]))
	}
	w.Align()
	_ = w.WriteBits(0, 8) // comment_field_bytes
	if _, err := ParseASC(w.Bytes()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("two front centres: %v, want ErrUnsupported", err)
	}
}
