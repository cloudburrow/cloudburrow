package embedding

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

// DecodeVideoAudioChannels decodes the first sound track of an MP4, M4A or
// MOV file of at most maxSeconds to interleaved samples at its own rate, its
// channels kept apart (Speech-to-Text's per-channel recognition, #1799):
// what DecodeVideoAudio reads, the edit list applied, without the downmix
// (AC-3 and E-AC-3 tracks come as one downmixed channel). No sound track is
// nil samples and a nil error; audio over maxSeconds is an error IsTooLong
// reports.
func DecodeVideoAudioChannels(mimeType string, data []byte, maxSeconds int) (pcm []float32, channels, rate int, err error) {
	if !videoMIMETypes[baseMIME(mimeType)] {
		return nil, 0, 0, fmt.Errorf("mime_type %q is not supported; send one of %s", mimeType, joinTypes(VideoMIMETypes))
	}
	defer func() {
		if r := recover(); r != nil {
			pcm, channels, rate, err = nil, 0, 0, fmt.Errorf("the audio track: %v", r)
		}
	}()
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil || f.Moov == nil {
		return nil, 0, 0, fmt.Errorf("the data is not a valid MP4 or MOV file")
	}
	for _, t := range f.Moov.Traks {
		if t.Mdia == nil || t.Mdia.Hdlr == nil || t.Mdia.Hdlr.HandlerType != "soun" {
			continue
		}
		pcm, channels, rate, _, err = decodeSoundTrackChannels(f, t, data, maxSeconds)
		if err != nil {
			if IsTooLong(err) {
				return nil, 0, 0, err
			}
			return nil, 0, 0, fmt.Errorf("the audio track: %w", err)
		}
		if len(pcm)/channels > maxSeconds*rate {
			return nil, 0, 0, &tooLongError{float64(len(pcm)/channels) / float64(rate)}
		}
		return pcm, channels, rate, nil
	}
	return nil, 0, 0, nil
}
