package speech

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"

	opus "github.com/darui3018823/opus"
	"github.com/darui3018823/opus/oggopus"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// opusFrames encodes testdata/jfk.wav as 20 ms Opus packets at 48 kHz,
// on channels channels (the speech in each): the packets, the encoder's
// pre-skip and the samples per channel encoded.
func opusFrames(t *testing.T, channels int) ([][]byte, int, int) {
	t.Helper()
	wav, err := os.ReadFile("testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	mono, err := embedding.DecodeWAVSpeech(wav)
	if err != nil {
		t.Fatal(err)
	}
	up := embedding.Resample(mono, SampleRate, opusRate)
	enc, err := opus.NewEncoder(opusRate, channels, opus.ApplicationVOIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.SetBitrate(24000 * channels); err != nil {
		t.Fatal(err)
	}
	var packets [][]byte
	frame := make([]int16, opus.FrameSize20ms*channels)
	for at := 0; at < len(up)+enc.Lookahead(); at += opus.FrameSize20ms { // the lookahead flushed too
		for i := range opus.FrameSize20ms {
			v := float32(0)
			if at+i < len(up) {
				v = up[at+i]
			}
			for c := range channels {
				frame[i*channels+c] = int16(max(-32768, min(32767, math.Round(float64(v)*32767))))
			}
		}
		p, err := enc.Encode(frame, opus.FrameSize20ms)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), p...))
	}
	return packets, enc.Lookahead(), len(up)
}

// writeOgg muxes packets as Ogg Opus.
func writeOgg(t *testing.T, packets [][]byte, channels, preSkip, length int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := oggopus.NewWriter(&buf, 1799, oggopus.Head{Version: 1, Channels: uint8(channels), PreSkip: uint16(preSkip), InputSampleRate: 16000},
		oggopus.Tags{Vendor: "cloudburrow test"})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range packets {
		granule := int64(min((i+1)*opus.FrameSize20ms, length+preSkip))
		if err := w.WriteValidatedPacket(p, oggopus.PacketWriteOptions{GranulePosition: granule, EOS: i == len(packets)-1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil && !errors.Is(err, oggopus.ErrWriterClosed) {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ebml writes one element: its ID, its size (-1 for unknown) and body.
func ebml(id uint64, body []byte, unknown bool) []byte {
	var out []byte
	for s := 3; s >= 0; s-- {
		if b := byte(id >> (8 * s)); b != 0 || len(out) > 0 {
			out = append(out, b)
		}
	}
	if unknown {
		out = append(out, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	} else {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(body))|1<<56)
		out = append(out, size[:]...)
	}
	return append(out, body...)
}

func ebmlUint(id, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return ebml(id, b[:], false)
}

// writeWebM muxes packets as WebM: an unknown-size Segment and Cluster, as
// a live recording writes them, SimpleBlocks, and the last packet in a
// BlockGroup with its DiscardPadding.
func writeWebM(t *testing.T, packets [][]byte, channels, preSkip, length int) []byte {
	t.Helper()
	head, err := oggopus.Head{Version: 1, Channels: uint8(channels), PreSkip: uint16(preSkip), InputSampleRate: 16000}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var freq [8]byte
	binary.BigEndian.PutUint64(freq[:], math.Float64bits(opusRate))
	out := ebml(mkvEBML, append(ebmlUint(0x4286, 1), ebml(mkvDocType, []byte("webm"), false)...), false)
	var tracks []byte
	tracks = append(tracks, ebmlUint(mkvTrackNumber, 1)...)
	tracks = append(tracks, ebmlUint(0x73C5, 1)...) // TrackUID
	tracks = append(tracks, ebmlUint(mkvTrackType, 2)...)
	tracks = append(tracks, ebml(mkvCodecID, []byte("A_OPUS"), false)...)
	tracks = append(tracks, ebml(mkvCodecPrivate, head, false)...)
	tracks = append(tracks, ebmlUint(mkvCodecDelay, uint64(preSkip)*1e9/opusRate)...)
	tracks = append(tracks, ebml(0xE1, append(ebml(0xB5, freq[:], false), ebmlUint(0x9F, uint64(channels))...), false)...)
	seg := ebml(0x1549A966, ebmlUint(0x2AD7B1, 1e6), false) // Info, TimecodeScale
	seg = append(seg, ebml(mkvTracks, ebml(mkvTrackEntry, tracks, false), false)...)
	seg = append(seg, ebml(mkvCluster, nil, true)...)
	seg = append(seg, ebmlUint(0xE7, 0)...) // Timecode
	for i, p := range packets {
		blk := []byte{0x81, byte(i * 20 >> 8), byte(i * 20), 0x80}
		if i < len(packets)-1 {
			seg = append(seg, ebml(mkvSimpleBlock, append(blk, p...), false)...)
			continue
		}
		blk[3] = 0
		pad := (len(packets)*opus.FrameSize20ms - length - preSkip) * 1e9 / opusRate
		var pb [8]byte
		binary.BigEndian.PutUint64(pb[:], uint64(pad))
		seg = append(seg, ebml(mkvBlockGroup, append(ebml(mkvBlock, append(blk, p...), false), ebml(mkvDiscardPadding, pb[:], false)...), false)...)
	}
	return append(out, ebml(mkvSegment, seg, true)...)
}

// correlation is the normalised correlation of two signals over their
// common length, at the best of a few small lags.
func correlation(a, b []float32) float64 {
	best := -1.0
	for lag := -40; lag <= 40; lag++ {
		var ab, aa, bb float64
		for i := range a {
			j := i + lag
			if j < 0 || j >= len(b) {
				continue
			}
			ab += float64(a[i]) * float64(b[j])
			aa += float64(a[i]) * float64(a[i])
			bb += float64(b[j]) * float64(b[j])
		}
		if c := ab / math.Sqrt(aa*bb); c > best {
			best = c
		}
	}
	return best
}

// TestOpus (#1799): JFK's speech, encoded as Opus, decodes from Ogg (mono)
// and WebM (stereo, unknown-size Segment and Cluster, DiscardPadding) to
// the WAV's length and waveform; autoDecodingConfig and the explicit
// encodings read both; the committed testdata files are what this test
// writes (CLOUDBURROW_WRITE_OPUS=1 rewrites them).
func TestOpus(t *testing.T) {
	wav, err := os.ReadFile("testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Decode(speechconfig.Decoding{Auto: true}, wav)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file, encoding string
		channels       int
		mux            func(*testing.T, [][]byte, int, int, int) []byte
		decode         func([]byte, int) ([]float32, error)
	}{
		{"testdata/jfk.opus", "OGG_OPUS", 1, writeOgg, DecodeOggOpus},
		{"testdata/jfk-stereo.webm", "WEBM_OPUS", 2, writeWebM, DecodeWebMOpus},
	} {
		if os.Getenv("CLOUDBURROW_WRITE_OPUS") == "1" {
			packets, preSkip, length := opusFrames(t, c.channels)
			if err := os.WriteFile(c.file, c.mux(t, packets, c.channels, preSkip, length), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.decode(data, MaxSeconds)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if d := len(got) - len(ref); d < -2 || d > 2 {
			t.Errorf("%s: %d samples, the WAV's %d", c.file, len(got), len(ref))
		}
		if r := correlation(ref, got); r < 0.9 {
			t.Errorf("%s: correlation %.3f to the WAV, want at least 0.9", c.file, r)
		}
		for _, d := range []speechconfig.Decoding{{Auto: true}, {Encoding: c.encoding, SampleRate: 16000, Channels: c.channels}} {
			if s, err := Decode(d, data); err != nil || len(s) != len(got) {
				t.Errorf("%s with %+v: %d samples, %v", c.file, d, len(s), err)
			}
		}
		if _, err := c.decode(data[:len(data)/2], MaxSeconds); c.encoding == "OGG_OPUS" && err == nil {
			t.Errorf("%s truncated: no error", c.file)
		}
		if _, err := c.decode(data, 5); !errors.Is(err, ErrTooLong) {
			t.Errorf("%s over 5 s: %v, want ErrTooLong", c.file, err)
		}
	}
	for name, data := range map[string][]byte{"Ogg": []byte("OggS\x00\x02 not really"), "WebM": {0x1a, 0x45, 0xdf, 0xa3, 0x01}} {
		if _, err := Decode(speechconfig.Decoding{Auto: true}, data); err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s garbage: %v, want a data error", name, err)
		}
	}
}

// TestMKVLacing: a block's Xiph, fixed-size and EBML lacing split into
// its frames (Matroska's block lacing).
func TestMKVLacing(t *testing.T) {
	a, b, c := bytes.Repeat([]byte{1}, 300), bytes.Repeat([]byte{2}, 3), bytes.Repeat([]byte{3}, 5)
	hdr := func(flags byte) []byte { return []byte{0x81, 0, 0, flags, 2} }
	xiph := append(append(hdr(0x02), 255, 45, 3), append(append(a, b...), c...)...)
	// EBML: 300 as a 2-byte vint, then 3-300 = -297 as a signed 2-byte vint.
	d := -297 + (1<<13 - 1)
	ebmlLaced := append(append(hdr(0x06), 0x40|byte(300>>8), byte(300&0xff), 0x40|byte(d>>8), byte(d)), append(append(a, b...), c...)...)
	fixed := append(hdr(0x04), bytes.Repeat([]byte{9}, 9)...)
	for name, tc := range map[string]struct {
		block []byte
		want  []int
	}{"xiph": {xiph, []int{300, 3, 5}}, "ebml": {ebmlLaced, []int{300, 3, 5}}, "fixed": {fixed, []int{3, 3, 3}}} {
		track, frames, err := mkvFrames(tc.block)
		if err != nil || track != 1 || len(frames) != len(tc.want) {
			t.Fatalf("%s: track %d, %d frames, %v", name, track, len(frames), err)
		}
		for i, f := range frames {
			if len(f) != tc.want[i] {
				t.Errorf("%s: frame %d is %d bytes, want %d", name, i, len(f), tc.want[i])
			}
		}
	}
}
