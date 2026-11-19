package vertexai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// Speech-to-Text v2's recognize (#1799), for BigQuery ML's ML.TRANSCRIBE
// over a CLOUD_AI_SPEECH_TO_TEXT_V2 remote model, which the BigQuery front
// sends here, through cloudburrow-host, instead of to speech.googleapis.com.
// It is BigQuery's internal path, not a served Speech-to-Text API: the
// request is v2's RecognizeRequest (config and content) on the default
// recognizer, projects/{project}/locations/{location}/recognizers/_, and the
// answer its RecognizeResponse
// (https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers/recognize,
// read 2026-11-18), for what a local model can answer:
//
//   - model "chirp", the one ML.TRANSCRIBE takes, is read with whisper.cpp
//     and OpenAI's multilingual Whisper base model (internal/speech), not
//     Google's Chirp: the words recognised and the confidences differ.
//     Other models are UNIMPLEMENTED.
//   - languageCodes: one BCP-47 code, whose language whisper knows (its
//     primary subtag, cmn as zh, iw as he, fil as tl, nb as no), or "auto"
//     to detect it. Several codes are UNIMPLEMENTED.
//   - features: enableWordTimeOffsets (each word's startOffset and
//     endOffset, from whisper's token timestamps), enableWordConfidence,
//     maxAlternatives (one alternative is returned, which the reference
//     allows) and enableAutomaticPunctuation (whisper punctuates
//     regardless) are read; profanityFilter, enableSpokenPunctuation,
//     enableSpokenEmojis, diarizationConfig and SEPARATE_RECOGNITION_PER_CHANNEL
//     are UNIMPLEMENTED, as are adaptation, transcriptNormalization,
//     translationConfig and denoiserConfig.
//   - decoding (speech/audio.go): WAV, headerless LINEAR16, MULAW and ALAW,
//     MP3, and AAC in MP4, M4A or MOV; FLAC, Opus and AMR are UNIMPLEMENTED.
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

// recognize serves POST /v2/projects/{p}/locations/{l}/recognizers/{r}:recognize.
func (s *Server) recognize(w http.ResponseWriter, r *http.Request) {
	rec, verb, _ := strings.Cut(r.PathValue("recognizer"), ":")
	if verb != "recognize" {
		apierror.WriteJSON(w, apierror.NotFound("%s %s is not served", r.Method, r.URL.Path))
		return
	}
	if rec != "_" {
		apierror.WriteJSON(w, apierror.NotFound("Recognizer projects/%s/locations/%s/recognizers/%s not found: CloudBurrow serves "+
			"the default recognizer _ alone", r.PathValue("project"), r.PathValue("location"), rec))
		return
	}
	var req struct {
		Config  json.RawMessage `json:"config"`
		Content string          `json:"content"`
		URI     string          `json:"uri"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil || json.Unmarshal(body, &req) != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("malformed request body: expected a RecognizeRequest"))
		return
	}
	if req.URI != "" {
		apierror.WriteJSON(w, apierror.Unimplemented("RecognizeRequest.uri: send the audio as content"))
		return
	}
	if len(req.Config) == 0 {
		apierror.WriteJSON(w, apierror.InvalidArgument("RecognizeRequest.config is required for the default recognizer"))
		return
	}
	cfg, code, err := speechconfig.Parse(req.Config)
	if code == 501 {
		apierror.WriteJSON(w, apierror.Unimplemented("%v", err))
		return
	} else if code != 0 {
		apierror.WriteJSON(w, apierror.InvalidArgument("%v", err))
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("RecognizeRequest.content is not valid base64"))
		return
	}
	samples, err := speech.Decode(cfg.Decoding, data)
	switch {
	case errors.Is(err, speech.ErrUnsupported):
		apierror.WriteJSON(w, apierror.Unimplemented("Not implemented here: %v", err))
		return
	case errors.Is(err, speech.ErrTooLong):
		apierror.WriteJSON(w, apierror.InvalidArgument("%v", err))
		return
	case err != nil:
		apierror.WriteJSON(w, apierror.InvalidArgument("Audio data does not appear to be in a supported encoding: %v", err))
		return
	}
	if lang := cfg.Language; lang != "" {
		known, err := speech.LanguageKnown(r.Context(), lang)
		if err != nil {
			s.speechError(w, err)
			return
		}
		if !known {
			apierror.WriteJSON(w, apierror.Unimplemented("Not implemented here: the language %s: CloudBurrow's local Whisper model "+
				"does not recognise it, and CloudBurrow never calls Google", lang))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	res, err := speech.Transcribe(ctx, samples, speech.Options{Language: cfg.Language, Words: cfg.Words})
	if err != nil {
		s.speechError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(recognizeResponse(res, samples, cfg))
}

func (s *Server) speechError(w http.ResponseWriter, err error) {
	if errors.Is(err, speech.ErrNoModel) {
		apierror.WriteJSON(w, apierror.Unimplemented("Not implemented here: %v", err))
		return
	}
	apierror.WriteJSON(w, apierror.Internal(err, "speech recognition failed: %v", err))
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
		results = append(results, map[string]any{"alternatives": []any{alt}, "resultEndOffset": protoDuration(seg.End), "languageCode": code})
	}
	billed := time.Duration(math.Ceil(float64(len(samples))/speech.SampleRate)) * time.Second
	out := map[string]any{"metadata": map[string]any{"totalBilledDuration": protoDuration(billed)}}
	if len(results) > 0 {
		out["results"] = results
	}
	return out
}
