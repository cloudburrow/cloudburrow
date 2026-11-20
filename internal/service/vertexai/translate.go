package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/langid"
)

// Cloud Translation v3's translateText and detectLanguage (#1799), for
// BigQuery ML's ML.TRANSLATE over a CLOUD_AI_TRANSLATE_V3 remote model,
// which the BigQuery front sends here, through cloudburrow-host, instead of
// to translate.googleapis.com. It is BigQuery's internal path, not a served
// Translation API: POST /v3/projects/{project}/locations/{location}:translateText
// and :detectLanguage, with v3's TranslateTextRequest and
// DetectLanguageRequest and their responses
// (https://docs.cloud.google.com/translate/docs/reference/rest/v3/projects.locations/translateText
// and .../detectLanguage, read 2026-11-18), for what local models answer:
//
//   - detectLanguage, and translateText's detectedLanguageCode when no
//     sourceLanguageCode is given, run MediaPipe's Language Detector
//     (internal/langid; Google Research's, Apache-2.0, the 110 languages of
//     CLD3) in pure Go. Its most probable language is answered, zh as
//     zh-CN.
//   - translateText runs the instance's local Gemma model (the LiteRT-LM
//     generator AI.GENERATE uses, `up --local-ai-model`) with
//     TranslateGemma's prompt, greedily (temperature 0, top-k 1), so a
//     text always translates to the same words. TranslateGemma itself is
//     published for LiteRT only as MediaPipe web (GPU) bundles, which the
//     CPU runtime does not run. With no local model, translateText is
//     UNIMPLEMENTED. A text whose language is already the target is
//     answered unchanged.
//   - targetLanguageCode and sourceLanguageCode are checked against the
//     Neural Machine Translation model's languages
//     (https://docs.cloud.google.com/translate/docs/languages, read
//     2026-11-18).
//   - mimeType text/html, model, glossaryConfig, transliterationConfig and
//     detectLanguage's model are UNIMPLEMENTED.
//
// UNVERIFIED (re-read 2026-11-18: the references define the fields, not
// these choices): the words of a translation, which Gemma's are and NMT's
// are not; the detector's confidences against Translation's; the texts of
// the errors.

// translateLanguages are the NMT model's language codes and names, by
// lower-case code.
var translateLanguages = func() map[string]string {
	m := map[string]string{}
	for k, v := range nmtLanguages {
		m[strings.ToLower(k)] = v
	}
	return m
}()

// LanguageName is a supported Translation code's language name, or "".
func LanguageName(code string) string { return translateLanguages[strings.ToLower(code)] }

func (s *Server) translation(w http.ResponseWriter, r *http.Request) {
	loc, verb, _ := strings.Cut(r.PathValue("location"), ":")
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("unreadable request body"))
		return
	}
	var resp any
	switch {
	case loc == "" || r.PathValue("project") == "":
		err = apierror.NotFound("%s %s is not served", r.Method, r.URL.Path)
	case verb == "translateText":
		resp, err = s.translateText(r, body)
	case verb == "detectLanguage":
		resp, err = detectLanguage(body)
	default:
		err = apierror.NotFound("%s %s is not served: this endpoint serves Translation v3's :translateText and :detectLanguage", r.Method, r.URL.Path)
	}
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// translateMime checks a request's mimeType.
func translateMime(m string) error {
	switch strings.ToLower(m) {
	case "", "text/plain":
		return nil
	case "text/html":
		return apierror.Unimplemented("mimeType text/html: the local translation model translates plain text only")
	}
	return apierror.InvalidArgument("Unsupported mime type: %s", m)
}

// detected is the detector's most probable language for text, as a
// Translation code, and its probability; "und" when it has none.
func detected(text string) (string, float64, error) {
	ps, err := langid.Detect(text)
	if err != nil {
		return "", 0, apierror.Internal(err, "the local language detector failed")
	}
	if len(ps) == 0 {
		return "und", 0, nil
	}
	code := ps[0].Code
	if code == "zh" {
		code = "zh-CN"
	}
	return code, ps[0].Probability, nil
}

func detectLanguage(body []byte) (any, error) {
	var req struct {
		Content  *string         `json:"content"`
		MimeType string          `json:"mimeType"`
		Model    string          `json:"model"`
		Labels   json.RawMessage `json:"labels"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, apierror.InvalidArgument("malformed request body: expected a DetectLanguageRequest")
	}
	if req.Model != "" {
		return nil, apierror.Unimplemented("DetectLanguageRequest.model: the local language detector is the only model")
	}
	if err := translateMime(req.MimeType); err != nil {
		return nil, err
	}
	if req.Content == nil {
		return nil, apierror.InvalidArgument("Content is required.")
	}
	code, p, err := detected(*req.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"languages": []map[string]any{{"languageCode": code, "confidence": p}}}, nil
}

func (s *Server) translateText(r *http.Request, body []byte) (any, error) {
	var req struct {
		Contents              []string        `json:"contents"`
		MimeType              string          `json:"mimeType"`
		SourceLanguageCode    string          `json:"sourceLanguageCode"`
		TargetLanguageCode    string          `json:"targetLanguageCode"`
		Model                 string          `json:"model"`
		GlossaryConfig        json.RawMessage `json:"glossaryConfig"`
		TransliterationConfig json.RawMessage `json:"transliterationConfig"`
		Labels                json.RawMessage `json:"labels"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, apierror.InvalidArgument("malformed request body: expected a TranslateTextRequest")
	}
	switch {
	case req.Model != "":
		return nil, apierror.Unimplemented("TranslateTextRequest.model: the local Gemma model is the only translation model")
	case len(req.GlossaryConfig) > 0 && string(req.GlossaryConfig) != "null":
		return nil, apierror.Unimplemented("TranslateTextRequest.glossaryConfig: no glossaries are served")
	case len(req.TransliterationConfig) > 0 && string(req.TransliterationConfig) != "null":
		return nil, apierror.Unimplemented("TranslateTextRequest.transliterationConfig: the local model does not transliterate")
	}
	if err := translateMime(req.MimeType); err != nil {
		return nil, err
	}
	if req.TargetLanguageCode == "" {
		return nil, apierror.InvalidArgument("Target language code is required.")
	}
	if LanguageName(req.TargetLanguageCode) == "" {
		return nil, apierror.InvalidArgument("Target language is invalid.")
	}
	if req.SourceLanguageCode != "" && LanguageName(req.SourceLanguageCode) == "" {
		return nil, apierror.InvalidArgument("Source language is invalid.")
	}
	if len(req.Contents) == 0 {
		return nil, apierror.InvalidArgument("Contents are required.")
	}
	var out []map[string]any
	for _, text := range req.Contents {
		src := req.SourceLanguageCode
		t := map[string]any{}
		if src == "" {
			code, _, err := detected(text)
			if err != nil {
				return nil, err
			}
			src = code
			t["detectedLanguageCode"] = code
		}
		tr, err := s.translateOne(r.Context(), text, src, req.TargetLanguageCode)
		if err != nil {
			return nil, err
		}
		t["translatedText"] = tr
		out = append(out, t)
	}
	return map[string]any{"translations": out}, nil
}

// sameLanguage reports whether two codes name one language: zh-CN and zh,
// iw and he, jw and jv, tl and fil.
func sameLanguage(a, b string) bool {
	norm := func(c string) string {
		c = strings.ToLower(c)
		switch c {
		case "zh", "zh-cn", "zh-hans":
			return "zh-cn"
		case "iw":
			return "he"
		case "jw":
			return "jv"
		case "tl":
			return "fil"
		}
		return c
	}
	return norm(a) == norm(b)
}

// translateOne translates one text with the local Gemma model and
// TranslateGemma's prompt (its chat template's, as
// litert-community/TranslateGemma-4B-IT's model card spells it).
func (s *Server) translateOne(ctx context.Context, text, src, tgt string) (string, error) {
	if strings.TrimSpace(text) == "" || sameLanguage(src, tgt) {
		return text, nil
	}
	if s.gen == nil {
		return "", apierror.Unimplemented("translateText: this instance has no local generation model, which translates " +
			"(start it with `up --local-ai-model`, see docs/generation.md); detectLanguage needs none")
	}
	sn, tn := LanguageName(src), LanguageName(tgt)
	if sn == "" {
		sn = "the source language"
	}
	prompt := "You are a professional " + sn + " (" + src + ") to " + tn + " (" + tgt + ") translator. Your goal is to accurately convey the " +
		"meaning and nuances of the original " + sn + " text while adhering to " + tn + " grammar, vocabulary, and cultural sensitivities.\n\n" +
		"Produce only the " + tn + " translation, without any additional explanations or commentary. Please translate the following " +
		sn + " text into " + tn + ":\n\n\n" + text
	gr := GenerateContentRequest{Contents: []Content{{Role: "user", Parts: []Part{{Text: prompt}}}}}
	if honoursOptions(s.gen) {
		// Greedy decoding, so a text always translates to the same words,
		// as Translation's do: temperature 0, and top-k 1 for a sampler
		// that does not read temperature 0 as greedy.
		zero, one := float32(0), float32(1)
		gr.GenerationConfig = &GenerationConfig{Temperature: &zero, TopK: &one}
	}
	req, _ := json.Marshal(gr)
	resp, err := s.generateUnary(ctx, s.gen.Model(), bytes.NewReader(req))
	if err != nil {
		return "", err
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", apierror.Internal(nil, "the local model answered no translation")
	}
	return strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text), nil
}
