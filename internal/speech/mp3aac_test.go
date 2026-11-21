package speech

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

func embeddingTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "embedding", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDecodeMP3AACChannels (#1799): SEPARATE_RECOGNITION_PER_CHANNEL reads
// each channel of a stereo MP3 and of an MP4 or MOV audio track; the
// default mode reads the first channel, as for WAV.
func TestDecodeMP3AACChannels(t *testing.T) {
	for _, tc := range []struct {
		file, encoding string
	}{
		{"tone-stereo-44k.mp3", "MP3"},
		{"va-aac.mp4", "MP4_AAC"},
		{"va-heaacv2.mp4", "M4A_AAC"},
		{"va-alaw-st.mov", "MOV_AAC"},
	} {
		data := embeddingTestdata(t, tc.file)
		for _, d := range []speechconfig.Decoding{{Auto: true}, {Encoding: tc.encoding}} {
			ch, err := DecodeChannelsMax(d, data, MaxSeconds)
			if err != nil || len(ch) != 2 || len(ch[0]) == 0 || len(ch[0]) != len(ch[1]) {
				t.Errorf("%s (%+v): %d channels, %v", tc.file, d, len(ch), err)
				continue
			}
			mono, err := DecodeMax(d, data, MaxSeconds)
			if err != nil || len(mono) != len(ch[0]) || mono[len(mono)/2] != ch[0][len(mono)/2] {
				t.Errorf("%s (%+v) default mode: not the first channel (%v)", tc.file, d, err)
			}
		}
	}
	if ch, err := DecodeChannelsMax(speechconfig.Decoding{Auto: true}, embeddingTestdata(t, "tone-mono-16k.mp3"), MaxSeconds); err != nil || len(ch) != 1 {
		t.Errorf("mono MP3: %d channels, %v", len(ch), err)
	}
}

// TestDecodeLongMP3AAC (#1799): MP3 and AAC are read up to the limit
// asked for (30 minutes for ML.TRANSCRIBE), not embedding's 180 seconds,
// and over it are ErrTooLong.
func TestDecodeLongMP3AAC(t *testing.T) {
	long := bytes.Repeat(embeddingTestdata(t, "tone-notag-16k.mp3"), 200) // about 216 s
	s, err := DecodeMax(speechconfig.Decoding{Auto: true}, long, MaxSeconds)
	if err != nil || len(s) < 200*SampleRate {
		t.Fatalf("a 216-second MP3: %d samples, %v", len(s), err)
	}
	if _, err := DecodeMax(speechconfig.Decoding{Encoding: "MP3"}, long, 200); !errors.Is(err, ErrTooLong) {
		t.Errorf("MP3 over 200 s: %v, want ErrTooLong", err)
	}
	aac := embeddingTestdata(t, "va-aac.mp4")
	if _, err := DecodeChannelsMax(speechconfig.Decoding{Encoding: "MP4_AAC"}, aac, MaxSeconds); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeChannelsMax(speechconfig.Decoding{Encoding: "MP4_AAC"}, aac, 0); !errors.Is(err, ErrTooLong) {
		t.Errorf("AAC over the limit: %v, want ErrTooLong", err)
	}
}
