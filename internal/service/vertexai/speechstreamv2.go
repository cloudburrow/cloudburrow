package vertexai

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"time"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// StreamingRecognize serves Speech-to-Text v2's bidirectional stream
// (speechstream.go has the recognition): the first request names the
// recognizer and carries streaming_config (config, merged through
// config_mask over the recognizer's default_recognition_config as
// Recognize's is, and streaming_features); every later one carries audio
// only. https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v2#streamingrecognizerequest
// (read 2026-11-19).
func (v *speechService) StreamingRecognize(stream speechpb.Speech_StreamingRecognizeServer) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	st, err := v.s.streamSettingsV2(stream.Context(), first)
	if err != nil {
		return grpcError(err)
	}
	recv := func() ([]byte, error) {
		req, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if req.GetStreamingConfig() != nil || req.GetRecognizer() != "" {
			return nil, apierror.InvalidArgument("Only the first StreamingRecognizeRequest may carry recognizer and streaming_config; later ones carry audio")
		}
		return req.GetAudio(), nil
	}
	var billed time.Duration
	err = runStream(stream.Context(), st, recv, func(ev streamEvent) error {
		out := &speechpb.StreamingRecognizeResponse{}
		switch ev.kind {
		case evSpeechBegin:
			out.SpeechEventType, out.SpeechEventOffset = speechpb.StreamingRecognizeResponse_SPEECH_ACTIVITY_BEGIN, durationpb.New(ev.offset)
		case evSpeechEnd:
			out.SpeechEventType, out.SpeechEventOffset = speechpb.StreamingRecognizeResponse_SPEECH_ACTIVITY_END, durationpb.New(ev.offset)
		case evEndOfUtterance:
			out.SpeechEventType, out.SpeechEventOffset = speechpb.StreamingRecognizeResponse_END_OF_SINGLE_UTTERANCE, durationpb.New(ev.offset)
		case evResult:
			r := &speechpb.StreamingRecognitionResult{IsFinal: ev.final, ResultEndOffset: durationpb.New(ev.offset), LanguageCode: ev.language,
				Alternatives: []*speechpb.SpeechRecognitionAlternative{alternativeV2(ev.segs, st)}}
			if !ev.final {
				r.Stability = streamInterimStability
			}
			out.Results = []*speechpb.StreamingRecognitionResult{r}
			billed = time.Duration(math.Ceil(ev.offset.Seconds())) * time.Second
			out.Metadata = &speechpb.RecognitionResponseMetadata{TotalBilledDuration: durationpb.New(billed)}
		}
		return stream.Send(out)
	})
	return grpcError(err)
}

func alternativeV2(segs []speech.Segment, st streamSettings) *speechpb.SpeechRecognitionAlternative {
	text, conf, words := joinSegments(segs)
	alt := &speechpb.SpeechRecognitionAlternative{Transcript: text, Confidence: float32(conf)}
	if st.cfg.WordOffsets || st.cfg.WordConfidence {
		for _, w := range words {
			wi := &speechpb.WordInfo{Word: w.Text}
			if st.cfg.WordOffsets {
				wi.StartOffset, wi.EndOffset = durationpb.New(w.Start), durationpb.New(w.End)
			}
			if st.cfg.WordConfidence {
				wi.Confidence = float32(w.Confidence)
			}
			alt.Words = append(alt.Words, wi)
		}
	}
	return alt
}

// streamSettingsV2 checks a v2 stream's first request.
func (s *Server) streamSettingsV2(ctx context.Context, first *speechpb.StreamingRecognizeRequest) (streamSettings, error) {
	var st streamSettings
	if _, _, _, err := parseChild("recognizer", "recognizers", first.GetRecognizer()); err != nil {
		return st, err
	}
	sc := first.GetStreamingConfig()
	if sc == nil {
		return st, apierror.InvalidArgument("The first StreamingRecognizeRequest must carry streaming_config")
	}
	if len(first.GetAudio()) > 0 {
		return st, apierror.InvalidArgument("The first StreamingRecognizeRequest carries streaming_config only; send audio in the requests after it")
	}
	var config json.RawMessage
	if sc.GetConfig() != nil {
		config, _ = protojson.Marshal(sc.GetConfig())
	}
	config, err := s.effectiveConfig(first.GetRecognizer(), config, sc.GetConfigMask().GetPaths())
	if err != nil {
		return st, err
	}
	cfg, err := s.checkedConfig(defaultRecognizer(first.GetRecognizer()), config)
	if err != nil {
		return st, err
	}
	if cfg.Diarization {
		// Chirp 3's page (read 2026-11-21): "Chirp 3 supports
		// transcription and diarization only in BatchRecognize and
		// Recognize"; chirp_3 is v2's only model the table marks.
		return st, apierror.InvalidArgument("features.diarizationConfig is not supported in StreamingRecognize: the model %q diarizes only in BatchRecognize and Recognize", cfg.Model)
	}
	if cfg.PerChannel {
		return st, apierror.Unimplemented("Not implemented here: SEPARATE_RECOGNITION_PER_CHANNEL in StreamingRecognize: CloudBurrow's " +
			"stream transcribes one mixed channel (Recognize and BatchRecognize read each channel), and never calls Google")
	}
	if len(cfg.Candidates) > 0 || cfg.TranslateTo != "" || cfg.Denoise {
		return st, apierror.Unimplemented("Not implemented here: several languageCodes, translationConfig and denoiserConfig in StreamingRecognize: " +
			"CloudBurrow's stream transcribes each utterance in one language (Recognize and BatchRecognize read both), and never calls Google")
	}
	opt, err := s.transcribeOptions(cfg)
	if err != nil {
		return st, err
	}
	if err := s.checkStreamLanguage(ctx, cfg); err != nil {
		return st, err
	}
	f := sc.GetStreamingFeatures()
	st = streamSettings{cfg: cfg, opt: opt, interim: f.GetInterimResults(), voiceEvents: f.GetEnableVoiceActivityEvents(),
		endSilence: endSilenceFor(f.GetEndpointingSensitivity().String())}
	if t := f.GetVoiceActivityTimeout(); t != nil {
		if !st.voiceEvents {
			return st, apierror.InvalidArgument("streaming_features.voice_activity_timeout requires enable_voice_activity_events")
		}
		if t.GetSpeechStartTimeout().AsDuration() < 0 || t.GetSpeechEndTimeout().AsDuration() < 0 {
			return st, apierror.InvalidArgument("voice_activity_timeout's durations must not be negative")
		}
		st.startTimeout, st.endTimeout = t.GetSpeechStartTimeout().AsDuration(), t.GetSpeechEndTimeout().AsDuration()
	}
	return st, nil
}

// checkStreamLanguage refuses a language whisper does not know, before
// any audio is read.
func (s *Server) checkStreamLanguage(ctx context.Context, cfg speechconfig.Config) error {
	if cfg.Language == "" {
		return nil
	}
	known, err := speech.LanguageKnown(ctx, cfg.Language)
	if err != nil {
		return speechError(err)
	}
	if !known {
		return apierror.Unimplemented("Not implemented here: the language %s: CloudBurrow's local Whisper model "+
			"does not recognise it, and CloudBurrow never calls Google", cfg.Language)
	}
	return nil
}
