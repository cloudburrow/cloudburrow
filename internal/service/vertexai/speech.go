package vertexai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// Speech-to-Text v2's recognize (#1799), for BigQuery ML's ML.TRANSCRIBE
// over a CLOUD_AI_SPEECH_TO_TEXT_V2 remote model, which the BigQuery front
// sends here, through cloudburrow-host, instead of to speech.googleapis.com,
// and the official client's Recognize (speechrecognizers.go): the request
// is v2's RecognizeRequest (config and content) on the default recognizer,
// projects/{project}/locations/{location}/recognizers/_, or on a created
// recognizer, whose default_recognition_config the config is merged over,
// and the answer its RecognizeResponse
// (https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers/recognize,
// read 2026-11-18), for what a local model can answer:
//
//   - models chirp (the one ML.TRANSCRIBE takes), chirp_2, chirp_3,
//     chirp_telephony, long, short, telephony and telephony_short are each
//     read with whisper.cpp and OpenAI's multilingual Whisper base model
//     (internal/speech), not Google's models: the words recognised and the
//     confidences differ (#1930). The medical models are UNIMPLEMENTED.
//   - languageCodes: one BCP-47 code, whose language whisper knows (its
//     primary subtag, cmn as zh, iw as he, fil as tl, nb as no), or "auto"
//     to detect it. Several codes are UNIMPLEMENTED.
//   - features: enableWordTimeOffsets (each word's startOffset and
//     endOffset, from whisper's token timestamps), enableWordConfidence,
//     maxAlternatives (one alternative is returned, which the reference
//     allows) and enableAutomaticPunctuation (whisper punctuates
//     regardless) are read, and SEPARATE_RECOGNITION_PER_CHANNEL
//     transcribes each channel on its own (transcribeChannels, #1799);
//     profanityFilter, enableSpokenPunctuation, enableSpokenEmojis and
//     diarizationConfig are UNIMPLEMENTED, as are translationConfig and denoiserConfig;
//     adaptation biases whisper (speechbias.go), and transcriptNormalization
//     rewrites each final transcript (speechconfig's Normalize, #1799).
//   - decoding (speech/audio.go): WAV, headerless LINEAR16, MULAW and ALAW,
//     MP3, AAC in MP4, M4A or MOV, FLAC (internal/speech/flac.go, #1799),
//     and Opus in Ogg or WebM (internal/speech/opus.go, #1799); AMR and
//     AMR-WB are UNIMPLEMENTED.
//   - each whisper segment is one result, its resultEndOffset the segment's
//     end; metadata.totalBilledDuration is the audio's length rounded up to
//     a second.
//
// UNVERIFIED (re-read 2026-11-18: the reference defines the fields, not
// these choices): the texts of the errors; how Chirp splits results; the
// lower-case languageCode.

// protoDuration writes a google.protobuf.Duration in JSON: "1.500s".
func protoDuration(d time.Duration) string {
	s := strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	return s + "s"
}

// recognize serves POST /v2/projects/{p}/locations/{l}/recognizers/{r}:recognize
// (its uri a gs:// object, speechbatch.go);
// the recognizers' other JSON methods go to the REST transcoder
// (speechrecognizers.go).
func (s *Server) recognize(w http.ResponseWriter, r *http.Request) {
	rec, verb, _ := strings.Cut(r.PathValue("recognizer"), ":")
	if verb != "recognize" {
		s.serveREST(w, r, nil)
		return
	}
	var req struct {
		Config  json.RawMessage `json:"config"`
		Content string          `json:"content"`
		URI     string          `json:"uri"`
		Mask    json.RawMessage `json:"configMask"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil || json.Unmarshal(body, &req) != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("malformed request body: expected a RecognizeRequest"))
		return
	}
	var mask []string
	if len(req.Mask) > 0 && string(req.Mask) != "null" {
		var m string
		if json.Unmarshal(req.Mask, &m) != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("RecognizeRequest.configMask is not a field mask"))
			return
		}
		for _, p := range strings.Split(m, ",") {
			if p = strings.TrimSpace(p); p != "" {
				mask = append(mask, p)
			}
		}
	}
	data, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("RecognizeRequest.content is not valid base64"))
		return
	}
	if req.URI != "" {
		if data, err = s.readAudioURI(r.Context(), req.URI, recognizeMaxContent); err != nil {
			apierror.WriteJSON(w, err)
			return
		}
	}
	name := "projects/" + r.PathValue("project") + "/locations/" + r.PathValue("location") + "/recognizers/" + rec
	config := req.Config
	if len(mask) > 0 {
		if config, err = s.effectiveConfig(name, config, mask); err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		name = defaultRecognizer(name)
	}
	out, err := s.recognizeAudio(r.Context(), name, config, data)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// recognizeAudio recognises data with a recognizer, named in full: the
// default recognizer _ takes config whole, another merges config over its
// default_recognition_config. The answer is a RecognizeResponse's JSON.
func (s *Server) recognizeAudio(ctx context.Context, name string, config json.RawMessage, data []byte) (map[string]any, error) {
	res, samples, cfg, err := s.transcribeAudio(ctx, name, config, data)
	if err != nil {
		return nil, err
	}
	return recognizeResponse(res, samples, cfg), nil
}

// checkedConfig is the recognizer's config with config merged over it,
// checked.
func (s *Server) checkedConfig(name string, config json.RawMessage) (speechconfig.Config, error) {
	if !strings.HasSuffix(name, "/recognizers/_") {
		merged, err := s.recognizerConfig(name, config)
		if err != nil {
			return speechconfig.Config{}, err
		}
		config = merged
	}
	if len(config) == 0 || string(config) == "null" {
		return speechconfig.Config{}, apierror.InvalidArgument("RecognizeRequest.config is required for the default recognizer")
	}
	cfg, code, err := speechconfig.Parse(config)
	if code == 501 {
		return cfg, apierror.Unimplemented("%v", err)
	} else if code != 0 {
		return cfg, apierror.InvalidArgument("%v", err)
	}
	if _, _, err := s.adaptationOptions(cfg.Adaptation); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// transcribeAudio is recognizeAudio's transcription, before it is written
// as a response.
func (s *Server) transcribeAudio(ctx context.Context, name string, config json.RawMessage, data []byte) (speech.Result, []float32, speechconfig.Config, error) {
	return s.transcribeAudioMax(ctx, name, config, data, speech.MaxSeconds)
}

// transcribeAudioMax is transcribeAudio for audio of at most maxSeconds.
func (s *Server) transcribeAudioMax(ctx context.Context, name string, config json.RawMessage, data []byte, maxSeconds int) (speech.Result, []float32, speechconfig.Config, error) {
	var none speech.Result
	cfg, err := s.checkedConfig(name, config)
	if err != nil {
		return none, nil, cfg, err
	}
	if cfg.PerChannel {
		return s.transcribeChannels(ctx, cfg, data, maxSeconds)
	}
	samples, err := speech.DecodeMax(cfg.Decoding, data, maxSeconds)
	if err != nil {
		return none, nil, cfg, decodeError(err)
	}
	if err := checkLanguage(ctx, cfg); err != nil {
		return none, nil, cfg, err
	}
	opt, err := s.transcribeOptions(cfg)
	if err != nil {
		return none, nil, cfg, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Hour)
	defer cancel()
	res, err := speech.Transcribe(ctx, samples, opt)
	if err != nil {
		return none, nil, cfg, speechError(err)
	}
	normalize(res.Segments, cfg)
	return res, samples, cfg, nil
}

// transcribeOptions are whisper's options for a checked config, its
// adaptation resolved (speechbias.go).
func (s *Server) transcribeOptions(cfg speechconfig.Config) (speech.Options, error) {
	phrases, prompt, err := s.adaptationOptions(cfg.Adaptation)
	if err != nil {
		return speech.Options{}, err
	}
	return speech.Options{Language: cfg.Language, Words: cfg.Words, Phrases: phrases, Prompt: prompt}, nil
}

// checkLanguage refuses a language the local Whisper model does not know.
func checkLanguage(ctx context.Context, cfg speechconfig.Config) error {
	lang := cfg.Language
	if lang == "" {
		return nil
	}
	known, err := speech.LanguageKnown(ctx, lang)
	if err != nil {
		return speechError(err)
	}
	if !known {
		return apierror.Unimplemented("Not implemented here: the language %s: CloudBurrow's local Whisper model "+
			"does not recognise it, and CloudBurrow never calls Google", lang)
	}
	return nil
}

// transcribeChannels is transcribeAudioMax with
// SEPARATE_RECOGNITION_PER_CHANNEL (#1799): each channel of the audio is
// decoded on its own (speech.DecodeChannelsMax) and transcribed
// independently, as the reference's MultiChannelMode says, each segment
// tagged with its 1-based channel; the results are ordered by their end,
// then channel. The samples returned are the channels one after another,
// so the billed duration counts each channel.
//
// UNVERIFIED (re-read 2026-11-20:
// https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers#MultiChannelMode
// and .../multi-channel, which do not say): the order of the channels'
// results, and that each channel is billed.
func (s *Server) transcribeChannels(ctx context.Context, cfg speechconfig.Config, data []byte, maxSeconds int) (speech.Result, []float32, speechconfig.Config, error) {
	var none speech.Result
	chans, err := speech.DecodeChannelsMax(cfg.Decoding, data, maxSeconds)
	if err != nil {
		return none, nil, cfg, decodeError(err)
	}
	if err := checkLanguage(ctx, cfg); err != nil {
		return none, nil, cfg, err
	}
	opt, err := s.transcribeOptions(cfg)
	if err != nil {
		return none, nil, cfg, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Hour)
	defer cancel()
	var (
		out     speech.Result
		samples []float32
	)
	for c, ch := range chans {
		res, err := speech.Transcribe(ctx, ch, opt)
		if err != nil {
			return none, nil, cfg, speechError(err)
		}
		if out.Language == "" {
			out.Language = res.Language
		}
		for _, seg := range res.Segments {
			seg.Channel = c + 1
			out.Segments = append(out.Segments, seg)
		}
		samples = append(samples, ch...)
	}
	sort.SliceStable(out.Segments, func(i, j int) bool {
		a, b := out.Segments[i], out.Segments[j]
		if a.End != b.End {
			return a.End < b.End
		}
		return a.Channel < b.Channel
	})
	normalize(out.Segments, cfg)
	return out, samples, cfg, nil
}

// normalize applies the config's transcriptNormalization to each final
// segment's transcript (speechconfig's Normalize).
func normalize(segs []speech.Segment, cfg speechconfig.Config) {
	if len(cfg.Normalization) == 0 {
		return
	}
	for i := range segs {
		segs[i].Text = cfg.Normalize(segs[i].Text)
	}
}

// decodeError is a speech.Decode error as the API answers it.
func decodeError(err error) error {
	switch {
	case errors.Is(err, speech.ErrUnsupported):
		return apierror.Unimplemented("Not implemented here: %v", err)
	case errors.Is(err, speech.ErrTooLong):
		return apierror.InvalidArgument("%v", err)
	}
	return apierror.InvalidArgument("Audio data does not appear to be in a supported encoding: %v", err)
}

func speechError(err error) error {
	if errors.Is(err, speech.ErrNoModel) {
		return apierror.Unimplemented("Not implemented here: %v", err)
	}
	return apierror.Internal(err, "speech recognition failed: %v", err)
}

// recognizeResponse is res as a RecognizeResponse.
func recognizeResponse(res speech.Result, samples []float32, f speechconfig.Config) map[string]any {
	code := f.LanguageCode
	if code == "" {
		code = strings.ToLower(res.Language) // detected
	}
	var results []any
	for _, seg := range res.Segments {
		alt := map[string]any{"transcript": seg.Text, "confidence": math.Round(seg.Confidence*1e6) / 1e6}
		if f.WordOffsets || f.WordConfidence {
			var words []any
			for _, wd := range seg.Words {
				m := map[string]any{"word": wd.Text}
				if f.WordOffsets {
					m["startOffset"], m["endOffset"] = protoDuration(wd.Start), protoDuration(wd.End)
				}
				if f.WordConfidence {
					m["confidence"] = math.Round(wd.Confidence*1e6) / 1e6
				}
				words = append(words, m)
			}
			alt["words"] = words
		}
		r := map[string]any{"alternatives": []any{alt}, "resultEndOffset": protoDuration(seg.End), "languageCode": code}
		if seg.Channel > 0 {
			r["channelTag"] = seg.Channel
		}
		results = append(results, r)
	}
	billed := time.Duration(math.Ceil(float64(len(samples))/speech.SampleRate)) * time.Second
	out := map[string]any{"metadata": map[string]any{"totalBilledDuration": protoDuration(billed)}}
	if len(results) > 0 {
		out["results"] = results
	}
	return out
}
