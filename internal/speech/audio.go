package speech

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// ErrUnsupported is audio Speech-to-Text reads that no local decoder does:
// the caller answers it 501.
var ErrUnsupported = errors.New("unsupported audio")

// ErrTooLong is audio longer than allowed (MaxSeconds unless the caller
// names another limit), in the words ML.TRANSCRIBE's page quotes for it.
var ErrTooLong = errors.New("The audio file cannot be processed in time.")

// MaxSeconds is the longest audio BigQuery's ML.TRANSCRIBE processes: 30
// minutes (bigqueryml-syntax-transcribe, "Limitations").
const MaxSeconds = 30 * 60

func unsupported(format string) error {
	return fmt.Errorf("%w: %s audio has no local decoder in CloudBurrow (WAV of LINEAR16, MULAW or ALAW, headerless "+
		"LINEAR16, MULAW and ALAW, FLAC, MP3, and AAC in MP4, M4A or MOV are decoded)", ErrUnsupported, format)
}

// sniff names the container of data as autoDecodingConfig detects it.
func sniff(b []byte) string {
	switch {
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "WAV"
	case len(b) >= 4 && string(b[0:4]) == "fLaC":
		return "FLAC"
	case len(b) >= 4 && string(b[0:4]) == "OggS":
		return "OGG_OPUS"
	case len(b) >= 4 && bytes.Equal(b[0:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "WEBM_OPUS"
	case bytes.HasPrefix(b, []byte("#!AMR-WB\n")):
		return "AMR_WB"
	case bytes.HasPrefix(b, []byte("#!AMR\n")):
		return "AMR"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		if string(b[8:12]) == "qt  " {
			return "MOV_AAC"
		}
		return "MP4_AAC"
	case len(b) >= 3 && string(b[0:3]) == "ID3", len(b) >= 2 && b[0] == 0xff && b[1]&0xe0 == 0xe0:
		return "MP3"
	}
	return ""
}

// Decode decodes audio to mono samples at SampleRate. An error wrapping
// ErrUnsupported is a format no local decoder reads; any other is data
// that is not what the config says.
func Decode(d speechconfig.Decoding, data []byte) ([]float32, error) {
	return DecodeMax(d, data, MaxSeconds)
}

// DecodeMax is Decode for audio of at most maxSeconds.
func DecodeMax(d speechconfig.Decoding, data []byte, maxSeconds int) ([]float32, error) {
	if len(data) == 0 {
		return nil, errors.New("the audio is empty")
	}
	format := d.Encoding
	if d.Auto {
		if format = sniff(data); format == "" {
			return nil, errors.New("the audio's format could not be detected; autoDecodingConfig reads WAV, AMR, AMR-WB, FLAC, " +
				"MP3, Opus in Ogg or WebM, and AAC in MP4, M4A or MOV")
		}
	}
	var (
		mono []float32
		err  error
	)
	switch format {
	case "WAV":
		mono, err = embedding.DecodeWAVSpeech(data)
	case "LINEAR16", "MULAW", "ALAW":
		if sniff(data) == "WAV" {
			// A WAV file sent with an explicit encoding: its header
			// is read, as the service reads it.
			mono, err = embedding.DecodeWAVSpeech(data)
			break
		}
		mono, err = headerless(format, d.SampleRate, d.Channels, data)
	case "FLAC":
		mono, err = DecodeFLAC(data) // flac.go, #1799
	case "MP3":
		mono, err = embedding.DecodeAudio("audio/mpeg", data)
	case "MP4_AAC", "M4A_AAC", "MOV_AAC":
		mt := "video/mp4"
		if format == "MOV_AAC" {
			mt = "video/quicktime"
		}
		mono, err = embedding.DecodeVideoAudio(mt, data, embedding.VideoOptions{})
		if err == nil && mono == nil {
			err = errors.New("the file has no audio track")
		}
	default:
		return nil, unsupported(format)
	}
	if embedding.IsTooLong(err) {
		return nil, fmt.Errorf("%w: %s audio longer than %d seconds: CloudBurrow's MP3 and AAC decoders stop there "+
			"(WAV and headerless audio are read up to %d minutes)", ErrUnsupported, format, embedding.MaxAudioSeconds, maxSeconds/60)
	}
	if err != nil {
		return nil, err
	}
	if len(mono) > maxSeconds*SampleRate {
		return nil, ErrTooLong
	}
	return mono, nil
}

// headerless decodes LINEAR16 (16-bit signed little-endian), MULAW or ALAW
// samples of channels interleaved channels at rate Hz.
func headerless(enc string, rate, channels int, b []byte) ([]float32, error) {
	if rate < 8000 || rate > 48000 {
		return nil, fmt.Errorf("explicitDecodingConfig.sampleRateHertz %d is out of range: valid values are 8000-48000", rate)
	}
	if channels == 0 {
		channels = 1
	}
	if channels < 1 || channels > 8 {
		return nil, fmt.Errorf("explicitDecodingConfig.audioChannelCount %d is out of range: at most 8", channels)
	}
	var s []float32
	if enc == "LINEAR16" {
		s = make([]float32, len(b)/2)
		for i := range s {
			s[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
		}
	} else {
		s = make([]float32, len(b))
		for i, c := range b {
			if enc == "MULAW" {
				s[i] = embedding.MuLaw(c)
			} else {
				s[i] = embedding.ALaw(c)
			}
		}
	}
	s = s[:len(s)/channels*channels]
	return embedding.Resample(embedding.Average(s, channels), rate, SampleRate), nil
}
