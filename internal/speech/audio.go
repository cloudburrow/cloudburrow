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
		"LINEAR16, MULAW and ALAW, FLAC, MP3, Opus in Ogg or WebM, and AAC in MP4, M4A or MOV are decoded)", ErrUnsupported, format)
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
	switch format {
	case "WAV", "LINEAR16", "MULAW", "ALAW", "FLAC", "OGG_OPUS", "WEBM_OPUS":
		// MultiChannelMode's default: "If the audio contains multiple
		// channels, only the first channel will be transcribed; other
		// channels will be ignored" (the v2 reference, read 2026-11-20).
		chans, err := DecodeChannelsMax(d, data, maxSeconds)
		if err != nil {
			return nil, err
		}
		if len(chans[0]) > maxSeconds*SampleRate {
			return nil, ErrTooLong
		}
		return chans[0], nil
	}
	var (
		mono []float32
		err  error
	)
	switch format {
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

// DecodeChannelsMax decodes audio as DecodeMax does but keeps each channel
// apart, for SEPARATE_RECOGNITION_PER_CHANNEL (#1799): each channel's
// samples at SampleRate. WAV, headerless LINEAR16, MULAW and ALAW, FLAC
// and Opus in Ogg or WebM are read per channel; MP3 and AAC, whose
// decoders here mix to mono, are ErrUnsupported.
func DecodeChannelsMax(d speechconfig.Decoding, data []byte, maxSeconds int) ([][]float32, error) {
	if len(data) == 0 {
		return nil, errors.New("the audio is empty")
	}
	format := d.Encoding
	if d.Auto || (format == "LINEAR16" || format == "MULAW" || format == "ALAW") && sniff(data) == "WAV" {
		if format = sniff(data); format == "" {
			_, err := DecodeMax(d, data, maxSeconds) // DecodeMax's error
			return nil, err
		}
	}
	var (
		pcm            []float32
		channels, rate int
		err            error
	)
	switch format {
	case "WAV":
		pcm, channels, rate, err = embedding.DecodeWAVChannels(data)
	case "LINEAR16", "MULAW", "ALAW":
		pcm, channels, rate, err = headerlessChannels(format, d.SampleRate, d.Channels, data)
	case "FLAC":
		pcm, channels, rate, err = embedding.DecodeFLACChannels(data, maxSeconds)
		if embedding.IsTooLong(err) {
			return nil, ErrTooLong
		}
	case "OGG_OPUS", "WEBM_OPUS":
		var p *opusPCM
		if format == "OGG_OPUS" {
			p, err = oggOpusPCM(data, maxSeconds)
		} else {
			p, err = webmOpusPCM(data, maxSeconds)
		}
		if err == nil {
			pcm, channels, rate = p.pcm, p.h.channels, opusRate
		}
	case "MP3", "MP4_AAC", "M4A_AAC", "MOV_AAC":
		return nil, fmt.Errorf("%w: recognising each channel of %s audio: CloudBurrow's MP3 and AAC decoders mix to mono "+
			"(WAV, headerless audio, FLAC and Opus are read per channel)", ErrUnsupported, format)
	default:
		return nil, unsupported(format)
	}
	if err != nil {
		return nil, err
	}
	if channels < 1 {
		channels = 1
	}
	n := len(pcm) / channels
	if n > maxSeconds*rate {
		return nil, ErrTooLong
	}
	out := make([][]float32, channels)
	for c := range out {
		ch := make([]float32, n)
		for i := range ch {
			ch[i] = pcm[i*channels+c]
		}
		out[c] = embedding.Resample(ch, rate, SampleRate)
	}
	return out, nil
}

// headerlessSamples are headerless LINEAR16 (16-bit signed
// little-endian), MULAW or ALAW bytes as samples in [-1, 1].
func headerlessSamples(enc string, b []byte) []float32 {
	var s []float32
	if enc == "LINEAR16" {
		s = make([]float32, len(b)/2)
		for i := range s {
			s[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
		}
		return s
	}
	s = make([]float32, len(b))
	for i, c := range b {
		if enc == "MULAW" {
			s[i] = embedding.MuLaw(c)
		} else {
			s[i] = embedding.ALaw(c)
		}
	}
	return s
}

// headerlessChannels reads headerless audio as headerless does, its
// channels kept interleaved.
func headerlessChannels(enc string, rate, channels int, b []byte) ([]float32, int, int, error) {
	if rate < 8000 || rate > 48000 {
		return nil, 0, 0, fmt.Errorf("explicitDecodingConfig.sampleRateHertz %d is out of range: valid values are 8000-48000", rate)
	}
	if channels == 0 {
		channels = 1
	}
	if channels < 1 || channels > 8 {
		return nil, 0, 0, fmt.Errorf("explicitDecodingConfig.audioChannelCount %d is out of range: at most 8", channels)
	}
	s := headerlessSamples(enc, b)
	return s[:len(s)/channels*channels], channels, rate, nil
}
