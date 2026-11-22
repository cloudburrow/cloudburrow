package vertexai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// Speech-to-Text v1's google.cloud.speech.v1.Speech (#1930) on the local AI
// endpoint, over gRPC: Recognize, LongRunningRecognize and
// StreamingRecognize, as
// https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v1
// documents them (read 2026-11-19). A v1 RecognitionConfig is read as the
// v2 config it maps to (speechconfig.go's checks, whisper.cpp underneath):
//
//   - encoding: ENCODING_UNSPECIFIED (WAV or FLAC, their headers read) is
//     v2's autoDecodingConfig; LINEAR16, MULAW, FLAC and MP3 are
//     decoded, and OGG_OPUS and WEBM_OPUS (#1799) at one of Opus's
//     sample_rate_hertz values, and AMR and AMR_WB (#1799, opencore-amr's
//     decoders); SPEEX_WITH_HEADER_BYTE is UNIMPLEMENTED, as in v2.
//   - model: "" (default), default, latest_long, video and phone_call read
//     as v2's long and telephony, latest_short and command_and_search as
//     short; medical models are UNIMPLEMENTED. use_enhanced is accepted.
//   - speech_contexts (phrases with a boost), adaptation's inline
//     phrase_sets and custom_classes, and its phrase_set_references and
//     stored custom classes (v1's Adaptation service, speechv1adaptation.go,
//     #2106) are applied as v2's adaptation (speechbias.go); abnf_grammar
//     is UNIMPLEMENTED, as are
//     alternative_language_codes and transcript_normalization (spoken
//     punctuation and emojis and the profanity filter are v2's, #1799);
//     diarization is speechdiarize.go's.
//   - audio_channel_count and enable_separate_recognition_per_channel
//     (https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v1,
//     read 2026-11-21): "Valid values for LINEAR16, OGG_OPUS and FLAC are
//     1-8. Valid value for MULAW, AMR, AMR_WB and SPEEX_WITH_HEADER_BYTE is
//     only 1"; "We only recognize the first channel by default". With the
//     flag "set to true explicitly and audio_channel_count > 1", each
//     channel is recognized on its own (v2's SEPARATE_RECOGNITION_PER_CHANNEL,
//     transcribeEachChannel), each result carrying its channel_tag, 1 to N,
//     in Recognize, LongRunningRecognize and StreamingRecognize, and
//     diarized on its own; it is "billed cumulatively for all channels
//     recognized", Recognize's and LongRunningRecognize's total_billed_time
//     counting each channel.
//   - Recognize reads at most a minute of audio ("Sync input too long. For
//     audio longer than 1 min use LongRunningRecognize with a 'uri'
//     parameter."), from content or a gs:// uri of this instance's Cloud
//     Storage; LongRunningRecognize up to 480 minutes, as an operation run
//     in the background, with LongRunningRecognizeMetadata's progress;
//     its output_config (a transcript written to Cloud Storage) is
//     UNIMPLEMENTED.
//   - StreamingRecognize is speechstream.go's, with v1's single_utterance;
//     per channel, each channel is endpointed and recognized on its own.
//
// UNVERIFIED (re-read 2026-11-19: https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v1,
// which does not say): the model mapping (each is the same local whisper model), and
// the texts of the errors other than Google's quoted one; with separate
// recognition per channel, the order of the channels' results (by end,
// then channel), that diarization's last result is one a channel (each
// with that channel's words and channel_tag), that a stream's voice
// activity events and single_utterance are each channel's, and that a
// WAV or FLAC header's channel count is read rather than checked against
// audio_channel_count.

const v1SyncMaxSeconds = 60

type speechV1Service struct {
	speechv1pb.UnimplementedSpeechServer
	s *Server
}

// v1Features are the supported languages table's features a v1 config
// asks for.
func v1Features(c *speechv1pb.RecognitionConfig) speechconfig.Feature {
	var f speechconfig.Feature
	set := func(on bool, x speechconfig.Feature) {
		if on {
			f |= x
		}
	}
	set(c.GetEnableAutomaticPunctuation(), speechconfig.AutomaticPunctuation)
	set(c.GetDiarizationConfig().GetEnableSpeakerDiarization(), speechconfig.Diarization)
	set(len(c.GetSpeechContexts()) > 0 || c.GetAdaptation() != nil, speechconfig.ModelAdaptation)
	set(c.GetEnableWordConfidence(), speechconfig.WordConfidence)
	set(c.GetProfanityFilter(), speechconfig.ProfanityFilter)
	set(c.GetEnableSpokenPunctuation().GetValue(), speechconfig.SpokenPunctuationFeature)
	set(c.GetEnableSpokenEmojis().GetValue(), speechconfig.SpokenEmojiFeature)
	return f
}

// v1Config maps a v1 RecognitionConfig to a v2 one in JSON.
func v1Config(c *speechv1pb.RecognitionConfig) (json.RawMessage, error) {
	if c == nil {
		return nil, apierror.InvalidArgument("config is required")
	}
	unimpl := func(what string) error {
		return apierror.Unimplemented("Not implemented here: %s of Speech-to-Text v1: CloudBurrow transcribes locally with whisper.cpp, "+
			"which has no equivalent, and never calls Google", what)
	}
	switch {
	case c.GetLanguageCode() == "":
		return nil, apierror.InvalidArgument("RecognitionConfig.language_code is required")
	case len(c.GetAlternativeLanguageCodes()) > 0:
		return nil, unimpl("alternative_language_codes")
	case c.GetTranscriptNormalization() != nil:
		return nil, unimpl("transcript_normalization")
	case len(c.GetAdaptation().GetPhraseSetReferences()) > 0:
		// v1Resolve replaces them with the stored sets.
		return nil, apierror.InvalidArgument("adaptation.phrase_set_references were not resolved")
	case c.GetAdaptation().GetAbnfGrammar() != nil:
		return nil, unimpl("adaptation.abnf_grammar")
	}
	out := map[string]any{"languageCodes": []string{c.GetLanguageCode()}}
	switch m := c.GetModel(); m {
	case "", "default", "latest_long", "video":
		out["model"] = "long"
	case "phone_call", "telephony":
		out["model"] = "telephony"
	case "telephony_short":
		out["model"] = "telephony_short"
	case "latest_short", "command_and_search":
		out["model"] = "short"
	default:
		return nil, unimpl("the model " + m)
	}
	enc := c.GetEncoding()
	n := c.GetAudioChannelCount()
	switch enc {
	case speechv1pb.RecognitionConfig_MULAW, speechv1pb.RecognitionConfig_AMR, speechv1pb.RecognitionConfig_AMR_WB,
		speechv1pb.RecognitionConfig_SPEEX_WITH_HEADER_BYTE:
		if n > 1 {
			return nil, apierror.InvalidArgument("audio_channel_count %d is invalid for %s: the only valid value is 1", n, enc)
		}
	default:
		if n < 0 || n > 8 {
			return nil, apierror.InvalidArgument("audio_channel_count %d is invalid: valid values are 1-8", n)
		}
	}
	perChannel := c.GetEnableSeparateRecognitionPerChannel() && n > 1
	switch enc {
	case speechv1pb.RecognitionConfig_ENCODING_UNSPECIFIED:
		out["autoDecodingConfig"] = map[string]any{}
	case speechv1pb.RecognitionConfig_LINEAR16, speechv1pb.RecognitionConfig_MULAW,
		speechv1pb.RecognitionConfig_FLAC, speechv1pb.RecognitionConfig_MP3:
		rate := c.GetSampleRateHertz()
		if enc == speechv1pb.RecognitionConfig_FLAC || enc == speechv1pb.RecognitionConfig_MP3 {
			out["explicitDecodingConfig"] = map[string]any{"encoding": enc.String()}
			break
		}
		if rate == 0 {
			rate = 16000
		}
		out["explicitDecodingConfig"] = map[string]any{"encoding": enc.String(), "sampleRateHertz": rate,
			"audioChannelCount": max(c.GetAudioChannelCount(), 1)}
	case speechv1pb.RecognitionConfig_AMR, speechv1pb.RecognitionConfig_AMR_WB:
		// opencore-amr's decoders (internal/speech/amr.go, #1799): "AMR ...
		// sample_rate_hertz must be 8000", AMR_WB's 16000.
		want := int32(8000)
		if enc == speechv1pb.RecognitionConfig_AMR_WB {
			want = 16000
		}
		if rate := c.GetSampleRateHertz(); rate != 0 && rate != want {
			return nil, apierror.InvalidArgument("sample_rate_hertz %d is invalid for %s: it must be %d", rate, enc, want)
		}
		out["explicitDecodingConfig"] = map[string]any{"encoding": enc.String(), "sampleRateHertz": want}
	case speechv1pb.RecognitionConfig_OGG_OPUS, speechv1pb.RecognitionConfig_WEBM_OPUS:
		// Opus in its container (internal/speech/opus.go, #1799); v1 asks
		// for one of Opus's rates.
		switch rate := c.GetSampleRateHertz(); rate {
		case 8000, 12000, 16000, 24000, 48000:
			out["explicitDecodingConfig"] = map[string]any{"encoding": enc.String(), "sampleRateHertz": rate,
				"audioChannelCount": max(c.GetAudioChannelCount(), 1)}
		default:
			return nil, apierror.InvalidArgument("sample_rate_hertz %d is invalid for %s: it must be one of 8000, 12000, 16000, 24000 or 48000", rate, enc)
		}
	default:
		return nil, apierror.Unimplemented("Not implemented here: %s audio has no local decoder in CloudBurrow, and it never calls Google", enc)
	}
	features := map[string]any{}
	if perChannel {
		features["multiChannelMode"] = "SEPARATE_RECOGNITION_PER_CHANNEL"
	}
	if c.GetEnableWordTimeOffsets() {
		features["enableWordTimeOffsets"] = true
	}
	if c.GetEnableWordConfidence() {
		features["enableWordConfidence"] = true
	}
	if c.GetProfanityFilter() {
		features["profanityFilter"] = true
	}
	if c.GetEnableAutomaticPunctuation() {
		features["enableAutomaticPunctuation"] = true
	}
	if c.GetEnableSpokenPunctuation().GetValue() {
		features["enableSpokenPunctuation"] = true
	}
	if c.GetEnableSpokenEmojis().GetValue() {
		features["enableSpokenEmojis"] = true
	}
	if dc := c.GetDiarizationConfig(); dc.GetEnableSpeakerDiarization() {
		// v1's defaults: "If not set, the default value is 2" and 6.
		lo, hi := v1SpeakerCounts(dc)
		if lo > hi {
			return nil, apierror.InvalidArgument("diarization_config.min_speaker_count %d is greater than max_speaker_count %d", lo, hi)
		}
		// v2 ignores these counts; v1Checked passes them on.
		features["diarizationConfig"] = map[string]any{"minSpeakerCount": lo, "maxSpeakerCount": hi}
	}
	if n := c.GetMaxAlternatives(); n != 0 {
		features["maxAlternatives"] = n
	}
	if len(features) > 0 {
		out["features"] = features
	}
	var sets []any
	for _, sc := range c.GetSpeechContexts() {
		var phrases []any
		for _, p := range sc.GetPhrases() {
			phrases = append(phrases, map[string]any{"value": p})
		}
		set := map[string]any{"phrases": phrases}
		if sc.GetBoost() != 0 {
			set["boost"] = sc.GetBoost()
		}
		sets = append(sets, map[string]any{"inlinePhraseSet": set})
	}
	for _, ps := range c.GetAdaptation().GetPhraseSets() {
		var phrases []any
		for _, p := range ps.GetPhrases() {
			ph := map[string]any{"value": p.GetValue()}
			if p.GetBoost() != 0 {
				ph["boost"] = p.GetBoost()
			}
			phrases = append(phrases, ph)
		}
		set := map[string]any{"phrases": phrases}
		if ps.GetBoost() != 0 {
			set["boost"] = ps.GetBoost()
		}
		sets = append(sets, map[string]any{"inlinePhraseSet": set})
	}
	var classes []any
	for _, cc := range c.GetAdaptation().GetCustomClasses() {
		var items []any
		for _, it := range cc.GetItems() {
			items = append(items, map[string]any{"value": it.GetValue()})
		}
		name := cc.GetCustomClassId()
		if name == "" {
			name = cc.GetName()
		}
		classes = append(classes, map[string]any{"name": name, "items": items})
	}
	if len(sets) > 0 || len(classes) > 0 {
		a := map[string]any{}
		if len(sets) > 0 {
			a["phraseSets"] = sets
		}
		if len(classes) > 0 {
			a["customClasses"] = classes
		}
		out["adaptation"] = a
	}
	return json.Marshal(out)
}

// v1Checked maps and checks a v1 config.
//
// The model, language and features are checked against Speech-to-Text v1's
// supported languages table, single_utterance (streaming's) included
// (speechconfig's CheckV1, #2127), before the config is mapped, so the
// v2 table of the model it maps to does not decide.
func (s *Server) v1Checked(c *speechv1pb.RecognitionConfig, single bool) (speechconfig.Config, speech.Options, error) {
	c, err := s.v1Resolve(c)
	if err != nil {
		return speechconfig.Config{}, speech.Options{}, err
	}
	if c != nil && c.GetLanguageCode() != "" {
		if err := speechconfig.CheckV1(c.GetModel(), c.GetLanguageCode(), single, v1Features(c)); err != nil {
			return speechconfig.Config{}, speech.Options{}, apierror.InvalidArgument("%v", err)
		}
	}
	raw, err := v1Config(c)
	if err != nil {
		return speechconfig.Config{}, speech.Options{}, err
	}
	cfg, err := s.parsedConfig("projects/-/locations/global/recognizers/_", raw)
	if err != nil {
		return cfg, speech.Options{}, err
	}
	if err := cfg.Unserved(); err != nil {
		return cfg, speech.Options{}, apierror.Unimplemented("%v", err)
	}
	if cfg.Diarization {
		// v1 uses min_speaker_count and max_speaker_count, 2 and 6 when
		// unset (v1Config checked them), where v2 does not.
		cfg.MinSpeakers, cfg.MaxSpeakers = v1SpeakerCounts(c.GetDiarizationConfig())
	}
	opt, err := s.transcribeOptions(cfg)
	return cfg, opt, err
}

// v1SpeakerCounts are a diarization_config's min_speaker_count and
// max_speaker_count: "If not set, the default value is 2" and 6.
func v1SpeakerCounts(dc *speechv1pb.SpeakerDiarizationConfig) (lo, hi int) {
	lo, hi = int(dc.GetMinSpeakerCount()), int(dc.GetMaxSpeakerCount())
	if lo == 0 {
		lo = 2
	}
	if hi == 0 {
		hi = 6
	}
	return lo, hi
}

func v1Alternative(segs []speech.Segment, cfg speechconfig.Config) *speechv1pb.SpeechRecognitionAlternative {
	text, conf, words := joinSegments(segs)
	alt := &speechv1pb.SpeechRecognitionAlternative{Transcript: text, Confidence: float32(conf)}
	for _, w := range words {
		if !cfg.WordOffsets && !cfg.WordConfidence {
			break
		}
		wi := &speechv1pb.WordInfo{Word: w.Text}
		if cfg.WordOffsets {
			wi.StartTime, wi.EndTime = durationpb.New(w.Start), durationpb.New(w.End)
		}
		if cfg.WordConfidence {
			wi.Confidence = float32(w.Confidence)
		}
		alt.Words = append(alt.Words, wi)
	}
	return alt
}

// v1Results are a transcription's results, one a whisper segment.
func v1Results(res speech.Result, cfg speechconfig.Config) []*speechv1pb.SpeechRecognitionResult {
	code := cfg.LanguageCode
	if code == "" {
		code = strings.ToLower(res.Language)
	}
	// The profanity filter and spoken punctuation and emojis (#1799).
	normalize(res.Segments, cfg, code)
	var out []*speechv1pb.SpeechRecognitionResult
	for _, seg := range res.Segments {
		out = append(out, &speechv1pb.SpeechRecognitionResult{Alternatives: []*speechv1pb.SpeechRecognitionAlternative{v1Alternative([]speech.Segment{seg}, cfg)},
			ResultEndTime: durationpb.New(seg.End), LanguageCode: code, ChannelTag: int32(seg.Channel)})
	}
	if !cfg.Diarization || len(res.Segments) == 0 {
		return out
	}
	// The speakers, in one last result of every word (speechdiarize.go):
	// per channel, one a channel, in channel order.
	byChannel := map[int][]speech.Segment{}
	var channels []int
	for _, seg := range res.Segments {
		if _, ok := byChannel[seg.Channel]; !ok {
			channels = append(channels, seg.Channel)
		}
		byChannel[seg.Channel] = append(byChannel[seg.Channel], seg)
	}
	sort.Ints(channels)
	for _, ch := range channels {
		segs := byChannel[ch]
		last := &speechv1pb.SpeechRecognitionAlternative{Words: v1SpeakerWords(segs, cfg)}
		out = append(out, &speechv1pb.SpeechRecognitionResult{Alternatives: []*speechv1pb.SpeechRecognitionAlternative{last},
			ResultEndTime: durationpb.New(segs[len(segs)-1].End), LanguageCode: code, ChannelTag: int32(ch)})
	}
	return out
}

// v1SpeakerWords are every word of segs with its speaker_tag and
// speaker_label.
func v1SpeakerWords(segs []speech.Segment, cfg speechconfig.Config) []*speechv1pb.WordInfo {
	var out []*speechv1pb.WordInfo
	for _, seg := range segs {
		for _, w := range seg.Words {
			wi := &speechv1pb.WordInfo{Word: w.Text, SpeakerTag: int32(w.Speaker), SpeakerLabel: strconv.Itoa(w.Speaker)}
			if cfg.WordOffsets {
				wi.StartTime, wi.EndTime = durationpb.New(w.Start), durationpb.New(w.End)
			}
			if cfg.WordConfidence {
				wi.Confidence = float32(w.Confidence)
			}
			out = append(out, wi)
		}
	}
	return out
}

func billedFor(samples []float32) time.Duration {
	return time.Duration(math.Ceil(float64(len(samples))/speech.SampleRate)) * time.Second
}

// v1Audio reads a RecognitionAudio: content, or a gs:// uri.
func (s *Server) v1Audio(ctx context.Context, a *speechv1pb.RecognitionAudio, limit int64) ([]byte, error) {
	switch {
	case a == nil || a.GetAudioSource() == nil:
		return nil, apierror.InvalidArgument("audio is required: set content or uri")
	case a.GetUri() != "":
		return s.readAudioURI(ctx, a.GetUri(), limit)
	}
	return a.GetContent(), nil
}

// v1Transcribe decodes and transcribes audio of at most maxSeconds (a
// channel's, per channel).
func (s *Server) v1Transcribe(ctx context.Context, cfg speechconfig.Config, opt speech.Options, data []byte, maxSeconds int, tooLong string) (speech.Result, []float32, error) {
	if cfg.PerChannel {
		chans, err := speech.DecodeChannelsMax(cfg.Decoding, data, maxSeconds)
		switch {
		case errors.Is(err, speech.ErrTooLong):
			return speech.Result{}, nil, apierror.InvalidArgument("%s", tooLong)
		case err != nil:
			return speech.Result{}, nil, decodeError(err)
		}
		if err := s.checkStreamLanguage(ctx, cfg); err != nil {
			return speech.Result{}, nil, err
		}
		res, samples, _, err := transcribeEachChannel(ctx, cfg, opt, chans)
		return res, samples, err
	}
	samples, err := speech.DecodeMax(cfg.Decoding, data, maxSeconds)
	switch {
	case errors.Is(err, speech.ErrTooLong):
		return speech.Result{}, nil, apierror.InvalidArgument("%s", tooLong)
	case err != nil:
		return speech.Result{}, nil, decodeError(err)
	}
	if err := s.checkStreamLanguage(ctx, cfg); err != nil {
		return speech.Result{}, nil, err
	}
	res, err := speech.Transcribe(ctx, samples, opt)
	if err != nil {
		return res, nil, speechError(err)
	}
	if err := diarizeSegments(ctx, samples, res.Segments, cfg); err != nil {
		return res, nil, err
	}
	return res, samples, nil
}

func (v *speechV1Service) Recognize(ctx context.Context, req *speechv1pb.RecognizeRequest) (*speechv1pb.RecognizeResponse, error) {
	cfg, opt, err := v.s.v1Checked(req.GetConfig(), false)
	if err != nil {
		return nil, grpcError(err)
	}
	data, err := v.s.v1Audio(ctx, req.GetAudio(), recognizeMaxContent)
	if err != nil {
		return nil, grpcError(err)
	}
	res, samples, err := v.s.v1Transcribe(ctx, cfg, opt, data, v1SyncMaxSeconds,
		"Sync input too long. For audio longer than 1 min use LongRunningRecognize with a 'uri' parameter.")
	if err != nil {
		return nil, grpcError(err)
	}
	return &speechv1pb.RecognizeResponse{Results: v1Results(res, cfg), TotalBilledTime: durationpb.New(billedFor(samples))}, nil
}

func (v *speechV1Service) LongRunningRecognize(ctx context.Context, req *speechv1pb.LongRunningRecognizeRequest) (*longrunningpb.Operation, error) {
	if req.GetOutputConfig() != nil {
		return nil, grpcError(apierror.Unimplemented("Not implemented here: LongRunningRecognizeRequest.output_config: " +
			"the transcript is answered in the operation's response"))
	}
	cfg, opt, err := v.s.v1Checked(req.GetConfig(), false)
	if err != nil {
		return nil, grpcError(err)
	}
	data, err := v.s.v1Audio(ctx, req.GetAudio(), batchMaxAudioBytes)
	if err != nil {
		return nil, grpcError(err)
	}
	v.s.mu.Lock()
	v.s.speechRecognizers() // the operations store
	v.s.mu.Unlock()
	op := v.s.newOp("projects/-/locations/global")
	start := timestamppb.Now()
	_ = v.s.ops.SetMetadata(op, &speechv1pb.LongRunningRecognizeMetadata{StartTime: start, LastUpdateTime: start, Uri: req.GetAudio().GetUri()})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		res, samples, err := v.s.v1Transcribe(ctx, cfg, opt, data, batchMaxSeconds, "Audio exceeds the limit of 480 minutes.")
		_ = v.s.ops.SetMetadata(op, &speechv1pb.LongRunningRecognizeMetadata{ProgressPercent: 100, StartTime: start,
			LastUpdateTime: timestamppb.Now(), Uri: req.GetAudio().GetUri()})
		if err != nil {
			_ = v.s.ops.Fail(op, err)
			return
		}
		_ = v.s.ops.Succeed(op, &speechv1pb.LongRunningRecognizeResponse{Results: v1Results(res, cfg),
			TotalBilledTime: durationpb.New(billedFor(samples))})
	}()
	got, _ := v.s.ops.Get(op)
	out, err := opProto(got)
	return out, grpcError(err)
}

func (v *speechV1Service) StreamingRecognize(stream speechv1pb.Speech_StreamingRecognizeServer) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	sc := first.GetStreamingConfig()
	if sc == nil {
		return grpcError(apierror.InvalidArgument("The first StreamingRecognizeRequest must carry streaming_config"))
	}
	cfg, opt, err := v.s.v1Checked(sc.GetConfig(), sc.GetSingleUtterance())
	if err != nil {
		return grpcError(err)
	}
	if err := v.s.checkStreamLanguage(stream.Context(), cfg); err != nil {
		return grpcError(err)
	}
	st := streamSettings{cfg: cfg, opt: opt, interim: sc.GetInterimResults(), voiceEvents: sc.GetEnableVoiceActivityEvents(),
		singleUtterance: sc.GetSingleUtterance(), endSilence: endSilenceFor("")}
	if t := sc.GetVoiceActivityTimeout(); t != nil {
		if !st.voiceEvents {
			return grpcError(apierror.InvalidArgument("voice_activity_timeout requires enable_voice_activity_events"))
		}
		st.startTimeout, st.endTimeout = t.GetSpeechStartTimeout().AsDuration(), t.GetSpeechEndTimeout().AsDuration()
	}
	recv := func() ([]byte, error) {
		req, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if req.GetStreamingConfig() != nil {
			return nil, apierror.InvalidArgument("Only the first StreamingRecognizeRequest may carry streaming_config; later ones carry audio_content")
		}
		return req.GetAudioContent(), nil
	}
	// said are the stream's final segments so far, a channel's per channel,
	// which diarization labels again at each final result (speechdiarize.go).
	said := map[int][]speech.Segment{}
	err = runStream(stream.Context(), st, recv, func(ev streamEvent) error {
		out := &speechv1pb.StreamingRecognizeResponse{}
		switch ev.kind {
		case evSpeechBegin:
			out.SpeechEventType, out.SpeechEventTime = speechv1pb.StreamingRecognizeResponse_SPEECH_ACTIVITY_BEGIN, durationpb.New(ev.offset)
		case evSpeechEnd:
			out.SpeechEventType, out.SpeechEventTime = speechv1pb.StreamingRecognizeResponse_SPEECH_ACTIVITY_END, durationpb.New(ev.offset)
		case evEndOfUtterance:
			out.SpeechEventType, out.SpeechEventTime = speechv1pb.StreamingRecognizeResponse_END_OF_SINGLE_UTTERANCE, durationpb.New(ev.offset)
		case evResult:
			r := &speechv1pb.StreamingRecognitionResult{IsFinal: ev.final, ResultEndTime: durationpb.New(ev.offset), LanguageCode: ev.language,
				Alternatives: []*speechv1pb.SpeechRecognitionAlternative{v1Alternative(ev.segs, cfg)}, ChannelTag: int32(ev.channel)}
			if !ev.final {
				r.Stability = streamInterimStability
			}
			if cfg.Diarization && ev.final {
				said[ev.channel] = append(said[ev.channel], ev.segs...)
				if err := diarizeSegments(stream.Context(), ev.samples, said[ev.channel], cfg); err != nil {
					return err
				}
				r.Alternatives[0].Words = v1SpeakerWords(said[ev.channel], cfg)
			}
			out.Results = []*speechv1pb.StreamingRecognitionResult{r}
			out.TotalBilledTime = durationpb.New(time.Duration(math.Ceil(ev.offset.Seconds())) * time.Second)
		}
		return stream.Send(out)
	})
	return grpcError(err)
}
