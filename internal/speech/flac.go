package speech

import "github.com/cloudburrow/cloudburrow/internal/embedding"

// DecodeFLAC decodes a FLAC file, for Speech-to-Text's FLAC encoding and
// autoDecodingConfig (#1799), to mono samples at SampleRate: frames are
// decoded by internal/embedding's FLAC decoder (RFC 9639, checked sample
// for sample against ffmpeg's), the channels averaged and resampled.
// Speech-to-Text documents FLAC at 16 or 24 bits
// (https://docs.cloud.google.com/speech-to-text/docs/encoding); any depth
// from 4 to 32 bits decodes. Audio over MaxSeconds is ErrTooLong.
func DecodeFLAC(data []byte) ([]float32, error) {
	mono, rate, err := embedding.DecodeFLACFile(data, MaxSeconds)
	if embedding.IsTooLong(err) {
		return nil, ErrTooLong
	}
	if err != nil {
		return nil, err
	}
	return embedding.Resample(mono, rate, SampleRate), nil
}
