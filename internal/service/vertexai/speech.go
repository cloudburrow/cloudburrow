package vertexai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
//     to detect it, or up to three (long, short and telephony, in global,
//     us and eu), the audio recognised in the best fit (transcribeBestFit,
//     #1799).
//   - features: enableWordTimeOffsets (each word's startOffset and
//     endOffset, from whisper's token timestamps), enableWordConfidence,
//     maxAlternatives (one alternative is returned, which the reference
//     allows) and enableAutomaticPunctuation (whisper punctuates
//     regardless) are read, and SEPARATE_RECOGNITION_PER_CHANNEL
//     transcribes each channel on its own (transcribeChannels, #1799);
//     profanityFilter, enableSpokenPunctuation and enableSpokenEmojis
//     rewrite each final transcript (speechconfig's Format, #1799), and
//     translationConfig (chirp_2) translates it with the served
//     Translation API's model (translateSegments, #1799);
//     denoiserConfig.denoiseAudio denoises the audio first (RNNoise,
//     internal/speech/denoise.go, #1799), a non-zero snrThreshold is
//     UNIMPLEMENTED; diarizationConfig is UNIMPLEMENTED (no permissive
//     local model, docs/compatibility.md);
//     adaptation biases whisper (speechbias.go), and transcriptNormalization
//     rewrites each final transcript (speechconfig's Normalize, #1799).
//   - decoding (speech/audio.go): WAV, headerless LINEAR16, MULAW and ALAW,
//     MP3, AAC in MP4, M4A or MOV, FLAC (internal/speech/flac.go, #1799),
//     Opus in Ogg or WebM (internal/speech/opus.go, #1799), and AMR and
//     AMR-WB (internal/speech/amr.go, opencore-amr's decoders in
//     WebAssembly, #1799).
//   - each whisper segment is one result, its resultEndOffset the segment's
//     end; metadata.totalBilledDuration is the audio's length rounded up to
//     a second.
//
// UNVERIFIED (re-read 2026-11-18: the reference defines the fields, not
// these choices): the texts of the errors; how Chirp splits results; the
// lower-case languageCode.

// protoDuration writes a google.protobuf.Duration in JSON: "1.500s".
// It is written from the integer nanoseconds: d.Seconds()'s float would
// write 2.97 s as "2.9699999999999998s", which protojson refuses.
func protoDuration(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	s := strconv.FormatInt(int64(d/time.Second), 10)
	if ns := int64(d % time.Second); ns != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%09d", ns), "0")
	}
	return sign + s + "s"
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
	if len(cfg.Candidates) > 0 {
		loc := ""
		if _, rest, ok := strings.Cut(name, "/locations/"); ok {
			loc, _, _ = strings.Cut(rest, "/")
		}
		if !speechconfig.MultiLanguageLocations[loc] {
			return cfg, apierror.InvalidArgument("several languageCodes are only available in the global location and the us and eu multi-regions, not %q", loc)
		}
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
	if samples, err = denoise(ctx, cfg, samples); err != nil {
		return none, nil, cfg, err
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
	res, cfg, err := transcribeBestFit(ctx, samples, opt, cfg)
	if err != nil {
		return none, nil, cfg, err
	}
	normalize(res.Segments, cfg, resultLanguage(cfg, res))
	if err := s.translateSegments(ctx, res.Segments, cfg, resultLanguage(cfg, res)); err != nil {
		return none, nil, cfg, err
	}
	return res, samples, cfg, nil
}

// transcribeBestFit transcribes samples in the config's language or, for
// several languageCodes (#1799), in the best fit of them, which it sets as
// the config's LanguageCode: the language whisper detects when it is one of
// them, else the one whose transcript whisper is most confident of (its
// segments' mean token probability, weighted by their length).
//
// UNVERIFIED (re-read 2026-11-20: the multiple-languages page says "the
// best-fit language from the list", not how it is chosen): this choice,
// which is whisper's, not Google's.
func transcribeBestFit(ctx context.Context, samples []float32, opt speech.Options, cfg speechconfig.Config) (speech.Result, speechconfig.Config, error) {
	if len(cfg.Candidates) == 0 {
		res, err := speech.Transcribe(ctx, samples, opt)
		if err != nil {
			return res, cfg, speechError(err)
		}
		return res, cfg, nil
	}
	opt.Language = ""
	res, err := speech.Transcribe(ctx, samples, opt)
	if err != nil {
		return res, cfg, speechError(err)
	}
	for _, c := range cfg.Candidates {
		if strings.EqualFold(c.Whisper, res.Language) {
			cfg.LanguageCode, cfg.Language = c.Code, c.Whisper
			return res, cfg, nil
		}
	}
	var (
		best      speech.Result
		bestScore = -1.0
	)
	for _, c := range cfg.Candidates {
		known, err := speech.LanguageKnown(ctx, c.Whisper)
		if err != nil {
			return res, cfg, speechError(err)
		}
		if !known {
			continue
		}
		opt.Language = c.Whisper
		r, err := speech.Transcribe(ctx, samples, opt)
		if err != nil {
			return res, cfg, speechError(err)
		}
		var sum, weight float64
		for _, seg := range r.Segments {
			w := max((seg.End - seg.Start).Seconds(), 0.01)
			sum += seg.Confidence * w
			weight += w
		}
		score := 0.0
		if weight > 0 {
			score = sum / weight
		}
		if score > bestScore {
			best, bestScore = r, score
			cfg.LanguageCode, cfg.Language = c.Code, c.Whisper
		}
	}
	if bestScore < 0 {
		return res, cfg, apierror.Unimplemented("Not implemented here: none of the languageCodes is a language CloudBurrow's local Whisper model " +
			"recognises, and CloudBurrow never calls Google")
	}
	return best, cfg, nil
}

// denoise is samples denoised when the config's denoiserConfig asks for it
// (denoiseAudio, #1799; internal/speech's Denoise, RNNoise).
func denoise(ctx context.Context, cfg speechconfig.Config, samples []float32) ([]float32, error) {
	if !cfg.Denoise {
		return samples, nil
	}
	out, err := speech.Denoise(ctx, samples)
	if err != nil {
		return nil, apierror.Internal(err, "denoising the audio failed: %v", err)
	}
	return out, nil
}

// resultLanguage is the language code results are labelled with: the
// config's, else the detected one.
func resultLanguage(cfg speechconfig.Config, res speech.Result) string {
	if cfg.LanguageCode != "" {
		return cfg.LanguageCode
	}
	return strings.ToLower(res.Language)
}

// translateSegments translates each segment's transcript to the config's
// translationConfig.targetLanguage (#1799) with the served Translation
// API's model (translate.go's translateOne): Chirp 2's speech translation,
// from the spoken language. Its words and confidences are dropped, as
// Chirp 2's page says of translation ("confidence scores are not
// returned").
//
// UNVERIFIED (re-read 2026-11-20: Chirp 2's page shows the translated
// transcript only): that each result is translated on its own, that words
// are dropped, and that results keep the spoken languageCode.
func (s *Server) translateSegments(ctx context.Context, segs []speech.Segment, cfg speechconfig.Config, code string) error {
	if cfg.TranslateTo == "" {
		return nil
	}
	src, tgt := translationCode(code), translationCode(cfg.TranslateTo)
	for i := range segs {
		tr, err := s.translateOne(ctx, segs[i].Text, src, tgt)
		if err != nil {
			return err
		}
		segs[i].Text, segs[i].Words = tr, nil
	}
	return nil
}

// translationCode is a Speech-to-Text language code as Translation's:
// cmn-Hans-CN as zh-CN, a dialect (ar-x-gulf, es-419) as its language.
func translationCode(code string) string {
	c := strings.ToLower(code)
	switch {
	case strings.HasPrefix(c, "cmn-hans") || c == "cmn":
		return "zh-CN"
	case strings.HasPrefix(c, "cmn-hant"):
		return "zh-TW"
	}
	if LanguageName(code) != "" {
		return code
	}
	p, _, _ := strings.Cut(c, "-")
	return p
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
		if ch, err = denoise(ctx, cfg, ch); err != nil {
			return none, nil, cfg, err
		}
		res, chCfg, err := transcribeBestFit(ctx, ch, opt, cfg)
		if err != nil {
			return none, nil, cfg, err
		}
		if c == 0 {
			cfg = chCfg
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
	normalize(out.Segments, cfg, resultLanguage(cfg, out))
	if err := s.translateSegments(ctx, out.Segments, cfg, resultLanguage(cfg, out)); err != nil {
		return none, nil, cfg, err
	}
	return out, samples, cfg, nil
}

// normalize applies the config's spoken punctuation and emojis, profanity
// filter and transcriptNormalization to each final segment's transcript
// (speechconfig's Format), recognised in the language code, and the
// profanity filter to its words.
func normalize(segs []speech.Segment, cfg speechconfig.Config, code string) {
	for i := range segs {
		segs[i].Text = cfg.Format(segs[i].Text, code)
		if cfg.ProfanityFilter {
			for j := range segs[i].Words {
				segs[i].Words[j].Text = speechconfig.MaskWord(segs[i].Words[j].Text)
			}
		}
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
		alt := map[string]any{"transcript": seg.Text}
		if f.TranslateTo == "" {
			alt["confidence"] = math.Round(seg.Confidence*1e6) / 1e6
		}
		if (f.WordOffsets || f.WordConfidence) && f.TranslateTo == "" {
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
