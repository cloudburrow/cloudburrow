package embedding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	mp3 "github.com/cloudburrow/cloudburrow/third_party/go-mp3"
)

// MP3 (#1721), the other audio format Google documents for
// gemini-embedding-2 ("MP3, WAV"; its samples send mime_type audio/mpeg),
// decoded with github.com/hajimehoshi/go-mp3 (pure Go, Apache 2.0; chosen in
// docs/upstream-evaluation.md), copied into third_party/go-mp3 and extended
// there to MPEG-2.5 (8, 11.025 and 12 kHz; #1734). The decoder gives 16-bit stereo PCM at the
// stream's rate, a mono stream copied to both channels; here a mono stream is
// read back as one channel, a stereo one downmixed as a stereo WAV is, and
// the encoder's delay and padding a LAME or Lavc "Info"/"Xing" frame records
// are trimmed as ffmpeg trims them (that frame decoded as silence, and the
// encoder delay plus the decoder's 529 samples dropped from the start), so an
// MP3 lines up with the WAV it was made from.

// mp3MIMETypes are the names MP3 goes by.
var mp3MIMETypes = map[string]bool{
	"audio/mpeg": true, "audio/mp3": true, "audio/mpeg3": true, "audio/x-mp3": true, "audio/x-mpeg": true, "audio/x-mpeg-3": true,
}

// mp3DecoderDelay is the samples an MP3 decoder's synthesis filter bank
// delays its output by (ffmpeg's 528 + 1).
const mp3DecoderDelay = 529

// mp3Stream is what the first frame and its Info tag say of a stream.
type mp3Stream struct {
	mono bool
	// samplesPerFrame is 1152 for MPEG-1 and 576 for MPEG-2 and 2.5.
	samplesPerFrame int
	// infoFrame is true when the first frame is an Info/Xing frame, which
	// holds no audio.
	infoFrame bool
	// frames is the Info tag's count of audio frames, 0 when absent.
	frames int
	// delay and padding are the encoder's, from a LAME tag; hasGapless is
	// true when the tag was found.
	delay, padding int
	hasGapless     bool
}

// skipID3v2 returns b past any ID3v2 tags at its start.
func skipID3v2(b []byte) []byte {
	for len(b) >= 10 && string(b[:3]) == "ID3" {
		size := int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
		size += 10
		if b[5]&0x10 != 0 { // a footer
			size += 10
		}
		if size > len(b) {
			return nil
		}
		b = b[size:]
	}
	return b
}

// parseMP3Stream reads the first MPEG audio frame header of b (after any
// ID3v2 tag) and the Info tag in that frame, if any.
func parseMP3Stream(b []byte) (mp3Stream, error) {
	b = skipID3v2(b)
	// The first frame header: a sync of 11 set bits, a version other than
	// "reserved", layer III, a bitrate index other than 15 and a sample rate
	// index other than 3.
	p := -1
	for i := 0; i+4 <= len(b); i++ {
		if b[i] != 0xff || b[i+1]&0xe0 != 0xe0 {
			continue
		}
		version, layer := b[i+1]>>3&3, b[i+1]>>1&3
		if version == 1 || layer == 0 || b[i+2]>>4 == 15 || b[i+2]>>2&3 == 3 {
			continue
		}
		if layer != 1 {
			return mp3Stream{}, fmt.Errorf("the stream is MPEG audio layer %d; only layer III (MP3) is embedded", 4-layer)
		}
		p = i
		break
	}
	if p < 0 {
		return mp3Stream{}, errors.New("no MPEG audio frame")
	}
	h := b[p:]
	mpeg1 := h[1]>>3&3 == 3
	s := mp3Stream{mono: h[3]>>6 == 3, samplesPerFrame: 576}
	sideInfo := 17
	switch {
	case mpeg1 && !s.mono:
		sideInfo = 32
	case !mpeg1 && s.mono:
		sideInfo = 9
	}
	if mpeg1 {
		s.samplesPerFrame = 1152
	}
	t := 4 + sideInfo
	if h[1]&1 == 0 { // a CRC follows the header
		t += 2
	}
	if t+8 > len(h) || (string(h[t:t+4]) != "Xing" && string(h[t:t+4]) != "Info") {
		return s, nil
	}
	s.infoFrame = true
	flags := binary.BigEndian.Uint32(h[t+4:])
	q := t + 8
	if flags&1 != 0 && q+4 <= len(h) {
		s.frames = int(binary.BigEndian.Uint32(h[q:]))
	}
	for bit, n := range []int{4, 4, 100, 4} { // frames, bytes, TOC, quality
		if flags&(1<<bit) != 0 {
			q += n
		}
	}
	// The LAME tag: a 9-byte encoder name, then 12 bytes, then the delay
	// and padding, 12 bits each.
	if q+24 <= len(h) {
		d := h[q+21:]
		s.delay = int(d[0])<<4 | int(d[1])>>4
		s.padding = int(d[1]&0x0f)<<8 | int(d[2])
		s.hasGapless = true
	}
	return s, nil
}

// decodeMP3 decodes an MP3 file to mono samples at its own rate.
func decodeMP3(b []byte) (samples []float32, rate int, err error) {
	pcm, channels, rate, err := decodeMP3Channels(b, MaxAudioSeconds)
	if err != nil {
		return nil, 0, err
	}
	if channels == 1 {
		return pcm, rate, nil
	}
	out := make([]float32, len(pcm)/2)
	for i := range out {
		out[i] = float32(math.Sqrt2 * (float64(pcm[2*i]) + float64(pcm[2*i+1])) / 2) // as downmix does
	}
	return out, rate, nil
}

// DecodeMP3Channels decodes an MP3 file of at most maxSeconds to
// interleaved samples (one or two channels) at its own rate, trimmed as
// ffmpeg's mp3 demuxer trims it (the Info frame, the LAME delay and
// padding). Longer audio is an error IsTooLong reports.
func DecodeMP3Channels(data []byte, maxSeconds int) (pcm []float32, channels, rate int, err error) {
	pcm, channels, rate, err = decodeMP3Channels(data, maxSeconds)
	if err != nil && !IsTooLong(err) {
		err = fmt.Errorf("the data is not a valid MP3 file: %v", err)
	}
	if err == nil && len(pcm)/channels > maxSeconds*rate {
		return nil, 0, 0, &tooLongError{float64(len(pcm)/channels) / float64(rate)}
	}
	return pcm, channels, rate, err
}

func decodeMP3Channels(b []byte, maxSeconds int) (samples []float32, channels, rate int, err error) {
	s, err := parseMP3Stream(b)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() {
		// go-mp3 can index out of range on a corrupt frame.
		if r := recover(); r != nil {
			samples, channels, rate, err = nil, 0, 0, fmt.Errorf("a corrupt frame (%v)", r)
		}
	}()
	d, err := mp3.NewDecoder(bytes.NewReader(b))
	if err != nil {
		return nil, 0, 0, err
	}
	rate = d.SampleRate()
	if rate < 8000 || rate > 48000 {
		return nil, 0, 0, fmt.Errorf("a sample rate of %d Hz", rate)
	}
	// The length is known before decoding (the decoder scans the frames),
	// so a clip far past the limit is refused without decoding it; the
	// second's slack covers the Info frame, delay and padding trimmed below,
	// and DecodeAudio checks the trimmed clip exactly.
	if n := d.Length(); n > 0 {
		if secs := float64(n) / 4 / float64(rate); secs > float64(maxSeconds)+1 {
			return nil, 0, 0, &tooLongError{secs}
		}
	}
	pcm, err := io.ReadAll(d)
	if err != nil {
		return nil, 0, 0, err
	}
	frames := len(pcm) / 4
	if frames == 0 {
		return nil, 0, 0, errors.New("no audio samples")
	}
	// What ffmpeg keeps of the decoded samples.
	start, end := 0, frames
	if s.infoFrame {
		start = s.samplesPerFrame
		if s.hasGapless {
			start += s.delay + mp3DecoderDelay
			if s.frames > 0 {
				end = min(end, s.samplesPerFrame*(1+s.frames)+mp3DecoderDelay-s.padding)
			}
		}
	}
	if start >= end {
		return nil, 0, 0, errors.New("no audio samples")
	}
	channels = 2
	if s.mono {
		channels = 1
	}
	out := make([]float32, (end-start)*channels)
	for i := range end - start {
		f := pcm[4*(start+i):]
		out[i*channels] = float32(int16(binary.LittleEndian.Uint16(f))) / 32768
		if channels == 2 {
			out[2*i+1] = float32(int16(binary.LittleEndian.Uint16(f[2:]))) / 32768
		}
	}
	return out, channels, rate, nil
}

// tooLongError is a clip past MaxAudioSeconds.
type tooLongError struct{ secs float64 }

func (e *tooLongError) Error() string {
	return fmt.Sprintf("the clip is %.1f seconds; at most %d seconds are embedded", e.secs, MaxAudioSeconds)
}

// decodeMP3PCM decodes an MP3 stream to interleaved stereo samples at its
// own rate (a mono stream in both channels), trimming nothing: an MP4's MP3
// track is trimmed by its edit list instead.
func decodeMP3PCM(b []byte, limit int) (samples []float32, rate int, err error) {
	defer func() {
		if r := recover(); r != nil {
			samples, rate, err = nil, 0, fmt.Errorf("a corrupt frame (%v)", r)
		}
	}()
	d, err := mp3.NewDecoder(bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	rate = d.SampleRate()
	if n := d.Length(); n > 0 {
		if secs := float64(n) / 4 / float64(rate); secs > float64(limit)+60 {
			return nil, 0, &tooLongError{secs}
		}
	}
	pcm, err := io.ReadAll(d)
	if err != nil {
		return nil, 0, err
	}
	samples = make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return samples, rate, nil
}
