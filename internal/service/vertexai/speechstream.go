package vertexai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// Streaming recognition (#1930), shared by Speech-to-Text v2's
// StreamingRecognize (speechstreamv2.go) and v1's (speechv1.go), on local
// whisper.cpp, as https://docs.cloud.google.com/speech-to-text/docs/streaming-recognize
// and the v1 and v2 RPC references describe it (read 2026-11-19):
//
//   - The audio arrives in chunks of at most streamMaxChunk bytes ("Audio
//     can be sent in chunks of up to 25 KB"), up to streamMaxAudio of it a
//     stream (quotas: "up to 5 minutes"; more is OUT_OF_RANGE, in v1's
//     words). It is decoded as a whole each time it is read (headerless
//     LINEAR16, MULAW and ALAW, or a WAV or FLAC stream), so a container
//     still being written is read once complete enough.
//   - whisper.cpp is not incremental: an utterance is transcribed again from
//     its start each time an interim result is due. Utterances are found by
//     energy (20 ms frames over streamVoiceRMS are speech), an utterance
//     ending after endpointing's silence (STANDARD 800 ms, SHORT 500 ms,
//     SUPERSHORT 250 ms), or at streamMaxUtterance at its quietest point.
//     Each ended utterance is one final result; the audio left when the
//     client closes its side is the last.
//   - Interim results (interim_results) are sent at most every
//     streamInterimEvery of new audio while speech goes on, with stability
//     streamInterimStability; when whisper is slower than the audio arrives,
//     the pending audio is read at once, so fewer interims are sent.
//   - Voice activity events (SPEECH_ACTIVITY_BEGIN and _END) carry their
//     offsets in the audio; voice_activity_timeout's speech_start_timeout
//     and speech_end_timeout close the stream after that much audio
//     without speech. v1's single_utterance ends recognition after the
//     first utterance with END_OF_SINGLE_UTTERANCE, then its final result.
//
// UNVERIFIED (re-read 2026-11-19: https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v2
// and .../quotas define the fields and limits, not these): the energy
// threshold and silences, the interim cadence and stability, whether
// Google counts the 5 minutes as audio or wall time (audio here), and the
// texts of the errors.

const (
	streamMaxChunk         = 25 * 1024
	streamMaxAudio         = 5 * time.Minute
	streamVoiceRMS         = 0.01
	streamFrame            = speech.SampleRate / 50 // 20 ms
	streamMaxUtterance     = 30 * time.Second
	streamInterimEvery     = 2 * time.Second
	streamInterimStability = 0.9
)

// streamTranscribe transcribes a stream's utterances (a test replaces it).
var streamTranscribe = speech.Transcribe

// streamEventKind is what a stream answers.
type streamEventKind int

const (
	evResult streamEventKind = iota
	evSpeechBegin
	evSpeechEnd
	evEndOfUtterance
)

// streamEvent is one answer of a stream.
type streamEvent struct {
	kind   streamEventKind
	offset time.Duration // events: where; results: the result's end
	final  bool
	// segs are the utterance's segments, their times in the stream.
	segs     []speech.Segment
	language string
}

// streamSettings are a stream's recognition and endpointing settings.
type streamSettings struct {
	cfg                      speechconfig.Config
	opt                      speech.Options
	interim, voiceEvents     bool
	singleUtterance          bool
	startTimeout, endTimeout time.Duration
	endSilence               time.Duration
}

// streamAudio is the audio a stream's reader has received.
type streamAudio struct {
	mu     sync.Mutex
	data   []byte
	closed bool
	err    error
	wake   chan struct{}
}

func (a *streamAudio) add(b []byte) {
	a.mu.Lock()
	a.data = append(a.data, b...)
	a.mu.Unlock()
	a.signal()
}

func (a *streamAudio) close(err error) {
	a.mu.Lock()
	a.closed, a.err = true, err
	a.mu.Unlock()
	a.signal()
}

func (a *streamAudio) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *streamAudio) snapshot() ([]byte, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.data, a.closed, a.err
}

// streamTooLong is the stream's duration limit, in v1's words.
func streamTooLong() error {
	return &apierror.Error{Code: codes.OutOfRange, Reason: "outOfRange",
		Message: fmt.Sprintf("Exceeded maximum allowed stream duration of %d seconds.", int(streamMaxAudio/time.Second))}
}

func samplesDur(n int) time.Duration { return time.Duration(n) * time.Second / speech.SampleRate }

// runStream recognises the audio recv returns (nil, io.EOF at the end)
// with st, sending each answer to emit.
func runStream(ctx context.Context, st streamSettings, recv func() ([]byte, error), emit func(streamEvent) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	audio := &streamAudio{wake: make(chan struct{}, 1)}
	go func() {
		for {
			b, err := recv()
			if err == io.EOF {
				audio.close(nil)
				return
			}
			if err != nil {
				audio.close(err)
				return
			}
			if len(b) > streamMaxChunk {
				audio.close(apierror.InvalidArgument("Audio chunk of %d bytes exceeds the limit of %d bytes a request", len(b), streamMaxChunk))
				return
			}
			audio.add(b)
		}
	}()
	s := &streamer{st: st, emit: emit, ctx: ctx}
	for {
		data, closed, err := audio.snapshot()
		if err != nil {
			return err
		}
		done, err := s.step(data, closed)
		if err != nil || done {
			return err
		}
		select {
		case <-audio.wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// streamer is a stream's recognition state.
type streamer struct {
	st   streamSettings
	emit func(streamEvent) error
	ctx  context.Context

	frame      int // the next frame to classify, in samples
	uttStart   int // the current utterance's start, in samples
	inSpeech   bool
	heard      bool // any speech in the current utterance
	everSpoke  bool
	lastVoice  int // the end of the last voiced frame
	lastEnd    int // where the last speech ended
	lastInterm int // the samples an interim was last sent at
	language   string
}

// step reads the audio received so far; done ends the stream.
func (s *streamer) step(data []byte, closed bool) (bool, error) {
	var samples []float32
	if len(data) > 0 {
		var err error
		samples, err = speech.DecodeMax(s.st.cfg.Decoding, data, int(streamMaxAudio/time.Second)+5)
		switch {
		case errors.Is(err, speech.ErrTooLong):
			return true, streamTooLong()
		case errors.Is(err, speech.ErrUnsupported):
			return true, apierror.Unimplemented("Not implemented here: %v", err)
		case err != nil && closed:
			return true, apierror.InvalidArgument("Audio data does not appear to be in a supported encoding: %v", err)
		case err != nil:
			return false, nil // a container not yet complete enough
		}
	}
	if samplesDur(len(samples)) > streamMaxAudio {
		return true, streamTooLong()
	}
	for ; s.frame+streamFrame <= len(samples); s.frame += streamFrame {
		var e float64
		for _, v := range samples[s.frame : s.frame+streamFrame] {
			e += float64(v) * float64(v)
		}
		voiced := math.Sqrt(e/streamFrame) >= streamVoiceRMS
		end := s.frame + streamFrame
		switch {
		case voiced && !s.inSpeech:
			s.inSpeech, s.heard, s.everSpoke = true, true, true
			if s.st.voiceEvents {
				if err := s.emit(streamEvent{kind: evSpeechBegin, offset: samplesDur(s.frame)}); err != nil {
					return true, err
				}
			}
			s.lastVoice = end
		case voiced:
			s.lastVoice = end
		case s.inSpeech && samplesDur(end-s.lastVoice) >= s.st.endSilence:
			s.inSpeech = false
			s.lastEnd = end
			if s.st.voiceEvents {
				if err := s.emit(streamEvent{kind: evSpeechEnd, offset: samplesDur(s.lastVoice)}); err != nil {
					return true, err
				}
			}
			if s.st.singleUtterance {
				if err := s.emit(streamEvent{kind: evEndOfUtterance, offset: samplesDur(s.lastVoice)}); err != nil {
					return true, err
				}
				return true, s.finish(samples, end)
			}
			if err := s.final(samples, end); err != nil {
				return true, err
			}
		}
		if s.inSpeech && samplesDur(end-s.uttStart) >= streamMaxUtterance {
			cut := speech.CutPoint(samples[:end], s.uttStart, end-s.uttStart)
			if err := s.final(samples, cut); err != nil {
				return true, err
			}
		}
		// voice_activity_timeout: no speech for that long closes the stream.
		if !s.inSpeech && !s.everSpoke && s.st.startTimeout > 0 && samplesDur(end) >= s.st.startTimeout {
			return true, s.finish(samples, end)
		}
		if !s.inSpeech && s.everSpoke && s.st.endTimeout > 0 && samplesDur(end-s.lastEnd) >= s.st.endTimeout {
			return true, s.finish(samples, end)
		}
	}
	if closed {
		return true, s.finish(samples, len(samples))
	}
	if s.st.interim && s.inSpeech && samplesDur(len(samples)-max(s.lastInterm, s.uttStart)) >= streamInterimEvery {
		s.lastInterm = len(samples)
		segs, err := s.transcribe(samples, s.uttStart, len(samples))
		if err != nil {
			return true, err
		}
		if len(segs) > 0 {
			if err := s.emit(streamEvent{kind: evResult, offset: samplesDur(len(samples)), segs: segs, language: s.language}); err != nil {
				return true, err
			}
		}
	}
	return false, nil
}

// finish sends the last final result, of the audio up to end.
func (s *streamer) finish(samples []float32, end int) error {
	if s.heard || s.inSpeech {
		return s.final(samples, end)
	}
	return nil
}

// final sends the utterance from uttStart to end as a final result.
func (s *streamer) final(samples []float32, end int) error {
	start := s.uttStart
	s.uttStart, s.heard = end, s.inSpeech
	segs, err := s.transcribe(samples, start, end)
	if err != nil || len(segs) == 0 {
		return err
	}
	return s.emit(streamEvent{kind: evResult, final: true, offset: samplesDur(end), segs: segs, language: s.language})
}

func (s *streamer) transcribe(samples []float32, start, end int) ([]speech.Segment, error) {
	if end <= start {
		return nil, nil
	}
	res, err := streamTranscribe(s.ctx, samples[start:end], s.st.opt)
	if err != nil {
		return nil, speechError(err)
	}
	if s.language == "" {
		s.language = s.st.cfg.LanguageCode
		if s.language == "" {
			s.language = strings.ToLower(res.Language)
		}
	}
	shift := samplesDur(start)
	for i := range res.Segments {
		seg := &res.Segments[i]
		seg.Start += shift
		seg.End += shift
		for j := range seg.Words {
			seg.Words[j].Start += shift
			seg.Words[j].End += shift
		}
	}
	return res.Segments, nil
}

// joinSegments is an utterance's transcript, mean confidence and words.
func joinSegments(segs []speech.Segment) (string, float64, []speech.Word) {
	var text []string
	var conf float64
	var words []speech.Word
	for _, sg := range segs {
		text = append(text, sg.Text)
		conf += sg.Confidence
		words = append(words, sg.Words...)
	}
	if len(segs) > 0 {
		conf /= float64(len(segs))
	}
	return strings.Join(text, " "), math.Round(conf*1e6) / 1e6, words
}

// endSilenceFor is endpointing's silence for a v2 sensitivity name.
func endSilenceFor(sensitivity string) time.Duration {
	switch sensitivity {
	case "ENDPOINTING_SENSITIVITY_SHORT":
		return 500 * time.Millisecond
	case "ENDPOINTING_SENSITIVITY_SUPERSHORT":
		return 250 * time.Millisecond
	}
	return 800 * time.Millisecond
}
