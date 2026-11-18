package vertexai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Natural Language v1's documents methods (#1799), for BigQuery ML's
// ML.UNDERSTAND_TEXT over a CLOUD_AI_NATURAL_LANGUAGE_V1 remote model, which
// the BigQuery front sends here, through cloudburrow-host, instead of to
// language.googleapis.com. It is BigQuery's internal path, not a served
// Natural Language API: POST /v1/documents:analyzeEntities,
// :analyzeEntitySentiment, :analyzeSentiment, :classifyText and
// :analyzeSyntax, with v1's request and response types
// (https://docs.cloud.google.com/natural-language/docs/reference/rest/v1/documents,
// read 2026-11-18).
//
//   - Entities, sentiment, entity sentiment and classification are asked of
//     the instance's local Gemma model (`up --local-ai-model`), decoded
//     greedily (temperature 0, top-k 1) under a JSON-schema constraint
//     (responseJsonSchema, #1779), so a text always answers the same JSON.
//     Classification's names are constrained to the 620 Version 1 content
//     categories (languagecats.go), the V1 model being classifyText's
//     default ("If this field is not set, then the v1_model will be used",
//     ClassificationModelOptions). With no local model each is
//     UNIMPLEMENTED.
//   - Sentences are split, and every offset counted, here, not by the
//     model: a mention the model names is kept only where its words occur
//     in the text, so no entity is answered that the text does not contain.
//     beginOffset is -1 for encodingType NONE (as BigQuery's tutorial shows
//     it), else the offset in UTF-8 bytes, UTF-16 code units or code points.
//   - The language is the request's, else MediaPipe's Language Detector's
//     (internal/langid), and is checked against each feature's languages
//     (https://docs.cloud.google.com/natural-language/docs/languages, read
//     2026-11-18). classifyText's V1 model requires "at least twenty tokens
//     (words)" (classifying-text page); a token here is a word or a
//     punctuation mark, which the BigQuery page's example sentence (19
//     words and a full stop) needs to pass.
//   - analyzeSyntax is UNIMPLEMENTED: no local, permissive part-of-speech
//     tagger and dependency parser (Google's SyntaxNet needs TensorFlow) is
//     available, and a language model's guesses at tags, lemmas and
//     dependency edges would not be the API's. HTML documents,
//     gcsContentUri and the V2 classification model are UNIMPLEMENTED too.
//
// UNVERIFIED (2026-11-18, the references define the fields, not these
// values): the calibration of every score, magnitude, salience and
// confidence, which are Gemma's estimates rounded and normalised here
// (sentiment to one decimal, as the API's samples show; salience summing to
// 1); which categories Gemma picks out of the taxonomy and their
// confidences; entity metadata (no Knowledge Graph mid or wikipedia_url is
// answered); the texts of the errors.

// nlEntityTypes are v1's Entity.Type values a model may answer.
var nlEntityTypes = []string{"PERSON", "LOCATION", "ORGANIZATION", "EVENT", "WORK_OF_ART", "CONSUMER_GOOD", "OTHER",
	"PHONE_NUMBER", "ADDRESS", "DATE", "NUMBER", "PRICE"}

// nlRequest is the documents methods' common request.
type nlRequest struct {
	Document *struct {
		Type          string  `json:"type"`
		Content       *string `json:"content"`
		GCSContentURI string  `json:"gcsContentUri"`
		Language      string  `json:"language"`
	} `json:"document"`
	EncodingType               string          `json:"encodingType"`
	ClassificationModelOptions json.RawMessage `json:"classificationModelOptions"`
}

func (s *Server) language(w http.ResponseWriter, r *http.Request) {
	verb := strings.TrimPrefix(r.URL.Path, "/v1/documents:")
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("unreadable request body"))
		return
	}
	var resp any
	switch verb {
	case "analyzeEntities", "analyzeEntitySentiment", "analyzeSentiment", "classifyText":
		resp, err = s.understand(r, verb, body)
	case "analyzeSyntax":
		err = apierror.Unimplemented("analyzeSyntax: CloudBurrow has no local part-of-speech tagger and dependency parser " +
			"that answers tokens, lemmas, morphology and dependency edges as the Natural Language API does")
	case "annotateText", "moderateText":
		err = apierror.Unimplemented("%s: CloudBurrow serves analyzeEntities, analyzeEntitySentiment, analyzeSentiment and classifyText, "+
			"which BigQuery's ML.UNDERSTAND_TEXT sends", verb)
	default:
		err = apierror.NotFound("%s %s is not served: this endpoint serves Natural Language v1's analyzeEntities, "+
			"analyzeEntitySentiment, analyzeSentiment and classifyText", r.Method, r.URL.Path)
	}
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

// understand checks a request and answers it with the local model.
func (s *Server) understand(r *http.Request, verb string, body []byte) (any, error) {
	var req nlRequest
	if json.Unmarshal(body, &req) != nil {
		return nil, apierror.InvalidArgument("malformed request body: expected a %s request", verb)
	}
	d := req.Document
	switch {
	case d == nil:
		return nil, apierror.InvalidArgument("document is required.")
	case d.GCSContentURI != "":
		return nil, apierror.Unimplemented("document.gcsContentUri: CloudBurrow reads the document's content only")
	case strings.EqualFold(d.Type, "HTML"):
		return nil, apierror.Unimplemented("document.type HTML: the local model analyses plain text only")
	case !strings.EqualFold(d.Type, "PLAIN_TEXT"):
		return nil, apierror.InvalidArgument("document.type must be PLAIN_TEXT or HTML.")
	case d.Content == nil || strings.TrimSpace(*d.Content) == "":
		return nil, apierror.InvalidArgument("document.content is empty.")
	}
	enc := strings.ToUpper(req.EncodingType)
	switch enc {
	case "", "NONE", "UTF8", "UTF16", "UTF32":
	default:
		return nil, apierror.InvalidArgument("Invalid encodingType %q: expected NONE, UTF8, UTF16 or UTF32.", req.EncodingType)
	}
	if verb == "classifyText" && len(req.ClassificationModelOptions) > 0 && string(req.ClassificationModelOptions) != "null" {
		var o map[string]json.RawMessage
		_ = json.Unmarshal(req.ClassificationModelOptions, &o)
		if _, v2 := o["v2Model"]; v2 {
			return nil, apierror.Unimplemented("classificationModelOptions.v2Model: the local classifier answers the V1 model's categories only")
		}
	}
	text := *d.Content
	lang := d.Language
	if lang == "" {
		code, _, err := detected(text)
		if err != nil {
			return nil, err
		}
		lang = code
	}
	feature := map[string]string{"analyzeEntities": "entities", "analyzeEntitySentiment": "entitySentiment",
		"analyzeSentiment": "sentiment", "classifyText": "classify"}[verb]
	if !nlSupports(feature, lang) {
		what := map[string]string{"entities": "entity analysis", "entitySentiment": "entity sentiment analysis",
			"sentiment": "sentiment analysis", "classify": "text classification"}[feature]
		return nil, apierror.InvalidArgument("The language %s is not supported for %s.", lang, what)
	}
	if verb == "classifyText" && nlTokens(text) < 20 {
		return nil, apierror.InvalidArgument("Invalid text content: too few tokens (words) to process.")
	}
	if s.gen == nil || !honoursOptions(s.gen) {
		return nil, apierror.Unimplemented("%s: this instance has no local generation model with constrained decoding, which "+
			"analyses the text (start it with `up --local-ai-model`, see docs/generation.md)", verb)
	}
	off := offsetFunc(text, enc)
	switch verb {
	case "analyzeSentiment":
		return s.analyzeSentiment(r, text, lang, off)
	case "classifyText":
		return s.classifyText(r, text)
	}
	return s.analyzeEntities(r, text, lang, off, verb == "analyzeEntitySentiment")
}

// nlSupports reports whether a feature serves a language: its code, or
// its base (pt-BR as pt), zh-CN and zh-Hans as zh, zh-TW and zh-HK as zh-Hant.
func nlSupports(feature, lang string) bool {
	l := strings.ToLower(lang)
	switch l {
	case "zh-cn", "zh-hans":
		l = "zh"
	case "zh-tw", "zh-hk", "zh-hant":
		l = "zh-hant"
	}
	base, _, _ := strings.Cut(l, "-")
	for _, c := range nlLanguages[feature] {
		c = strings.ToLower(c)
		if c == l || (c == base && !strings.HasPrefix(l, "zh-")) {
			return true
		}
	}
	return false
}

// nlTokens counts a text's tokens: each run of letters, digits and inner
// apostrophes is one, and each other non-space character is one.
func nlTokens(text string) int {
	n, inWord := 0, false
	for _, c := range text {
		switch {
		case unicode.IsLetter(c) || unicode.IsDigit(c) || unicode.Is(unicode.Mn, c) || (inWord && (c == '\'' || c == '’')):
			if !inWord {
				n++
			}
			inWord = true
		case unicode.IsSpace(c):
			inWord = false
		default:
			n++
			inWord = false
		}
	}
	return n
}

// offsetFunc converts a byte offset in text to beginOffset in an encoding.
func offsetFunc(text, enc string) func(int) int {
	return func(b int) int {
		switch enc {
		case "UTF8":
			return b
		case "UTF16":
			n := 0
			for _, c := range text[:b] {
				n += len(utf16.Encode([]rune{c}))
			}
			return n
		case "UTF32":
			return utf8.RuneCountInString(text[:b])
		}
		return -1
	}
}

// span is a piece of the text and its byte offset.
type span struct {
	text  string
	start int
}

// splitSentences splits text after each sentence-final mark (. ! ? and
// their full-width and other scripts' forms, with any closing quotes or
// brackets) that is followed by a space or the end, or that is
// full-width; and at each blank line.
func splitSentences(text string) []span {
	var out []span
	start := -1
	emit := func(end int) {
		if start >= 0 {
			if t := strings.TrimRightFunc(text[start:end], unicode.IsSpace); t != "" {
				out = append(out, span{t, start})
			}
		}
		start = -1
	}
	for i := 0; i < len(text); {
		c, n := utf8.DecodeRuneInString(text[i:])
		if start < 0 && !unicode.IsSpace(c) {
			start = i
		}
		i += n
		if c == '\n' && strings.HasPrefix(strings.TrimLeft(text[i:], " \t\r"), "\n") {
			emit(i)
			continue
		}
		if !strings.ContainsRune(".!?。！？।؟…", c) {
			continue
		}
		j := i
		for j < len(text) {
			d, m := utf8.DecodeRuneInString(text[j:])
			if !strings.ContainsRune(".!?。！？\"'”’)]»」』", d) {
				break
			}
			j += m
		}
		next, _ := utf8.DecodeRuneInString(text[j:])
		if j == len(text) || unicode.IsSpace(next) || strings.ContainsRune("。！？", c) {
			i = j
			emit(j)
		}
	}
	emit(len(text))
	return out
}

// nlGenerate asks the local model for JSON valid against schema, greedily,
// and decodes it into out.
func (s *Server) nlGenerate(r *http.Request, prompt string, schema map[string]any, out any) error {
	sb, _ := json.Marshal(schema)
	zero, one := float32(0), float32(1)
	gr := GenerateContentRequest{
		Contents:         []Content{{Role: "user", Parts: []Part{{Text: prompt}}}},
		GenerationConfig: &GenerationConfig{Temperature: &zero, TopK: &one, ResponseMIMEType: "application/json", ResponseJSONSchema: sb},
	}
	req, _ := json.Marshal(gr)
	resp, err := s.generateUnary(r.Context(), s.gen.Model(), bytes.NewReader(req))
	if err != nil {
		return err
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return apierror.Internal(nil, "the local model answered nothing")
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text)), out); err != nil {
		return apierror.Internal(err, "the local model answered JSON that does not match the schema")
	}
	return nil
}

// round1 rounds to one decimal.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// f32 is a proto float as the API's JSON prints it: float32, to eight
// significant digits (the BigQuery page's sample confidence 0.51999998).
func f32(v float64) json.Number {
	return json.Number(strconv.FormatFloat(float64(float32(v)), 'g', 8, 64))
}
func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// sentimentJSON is a v1 Sentiment.
func sentimentJSON(score, magnitude float64) map[string]any {
	return map[string]any{"score": round1(clamp(score, -1, 1)), "magnitude": round1(math.Max(0, magnitude))}
}

func numberedSentences(ss []span) string {
	var b strings.Builder
	for i, s := range ss {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s.text)
	}
	return b.String()
}

func (s *Server) analyzeSentiment(r *http.Request, text, lang string, off func(int) int) (any, error) {
	ss := splitSentences(text)
	item := map[string]any{"type": "object", "properties": map[string]any{
		"score":     map[string]any{"type": "number", "minimum": -1, "maximum": 1},
		"magnitude": map[string]any{"type": "number", "minimum": 0},
	}, "required": []string{"score", "magnitude"}, "additionalProperties": false}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"sentences": map[string]any{"type": "array", "items": item, "minItems": len(ss), "maxItems": len(ss)},
	}, "required": []string{"sentences"}, "additionalProperties": false}
	prompt := "Analyze the sentiment of each numbered sentence below. For each sentence, in order, give a score from -1.0 " +
		"(clearly negative) through 0.0 (neutral) to 1.0 (clearly positive), and a magnitude from 0.0 upwards for how much " +
		"emotion it carries regardless of its direction (0.0 for none). Answer one entry per sentence, " +
		fmt.Sprint(len(ss)) + " in all.\n\n" + numberedSentences(ss)
	var got struct {
		Sentences []struct {
			Score     float64 `json:"score"`
			Magnitude float64 `json:"magnitude"`
		} `json:"sentences"`
	}
	if err := s.nlGenerate(r, prompt, schema, &got); err != nil {
		return nil, err
	}
	if len(got.Sentences) != len(ss) {
		return nil, apierror.Internal(nil, "the local model rated %d sentences of %d", len(got.Sentences), len(ss))
	}
	sentences := make([]map[string]any, len(ss))
	var sum, mag float64
	for i, sp := range ss {
		g := got.Sentences[i]
		sentences[i] = map[string]any{"text": map[string]any{"content": sp.text, "beginOffset": off(sp.start)}, "sentiment": sentimentJSON(g.Score, g.Magnitude)}
		sum += clamp(g.Score, -1, 1)
		mag += math.Max(0, g.Magnitude)
	}
	return map[string]any{"documentSentiment": sentimentJSON(sum/float64(len(ss)), mag), "language": lang, "sentences": sentences}, nil
}

func (s *Server) analyzeEntities(r *http.Request, text, lang string, off func(int) int, withSentiment bool) (any, error) {
	mprops := map[string]any{"text": map[string]any{"type": "string"}, "type": map[string]any{"enum": []string{"PROPER", "COMMON"}}}
	mreq := []string{"text", "type"}
	entity := map[string]any{"type": "object", "properties": map[string]any{
		"name":     map[string]any{"type": "string"},
		"type":     map[string]any{"enum": nlEntityTypes},
		"salience": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		"mentions": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": mprops,
			"required": mreq, "additionalProperties": false}},
	}, "required": []string{"name", "type", "salience", "mentions"}, "additionalProperties": false}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"entities": map[string]any{"type": "array", "items": entity},
	}, "required": []string{"entities"}, "additionalProperties": false}
	prompt := "List the entities in the text below: the people, places, organizations, events, works of art, consumer goods, " +
		"phone numbers, addresses, dates, numbers and prices it names, and the other things it refers to by common nouns. " +
		"For each entity give its canonical name; its type (" + strings.Join(nlEntityTypes, ", ") + "); its salience, from 0 to 1, " +
		"how central it is to the text as a whole; and each of its mentions, copied exactly as it is written in the text, " +
		"PROPER for a name and COMMON for a common noun phrase.\n\nText:\n" + text
	var got struct {
		Entities []struct {
			Name     string  `json:"name"`
			Type     string  `json:"type"`
			Salience float64 `json:"salience"`
			Mentions []struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"mentions"`
		} `json:"entities"`
	}
	if err := s.nlGenerate(r, prompt, schema, &got); err != nil {
		return nil, err
	}
	type mention struct {
		at int
		m  map[string]any
	}
	type kept struct {
		m        map[string]any
		salience float64
		first    int
		mentions []mention
	}
	var ents []kept
	claimed := map[int]bool{}
	seen := map[string]bool{}
	for _, e := range got.Entities {
		key := strings.ToLower(strings.TrimSpace(e.Name)) + "\x00" + e.Type
		if strings.TrimSpace(e.Name) == "" || seen[key] {
			continue
		}
		var mentions []mention
		first := len(text)
		done := map[string]bool{}
		for _, m := range e.Mentions {
			mt := strings.TrimSpace(m.Text)
			if mt == "" || done[mt] {
				continue
			}
			done[mt] = true
			for _, at := range occurrences(text, mt) {
				if claimed[at] {
					continue
				}
				claimed[at] = true
				mj := map[string]any{"text": map[string]any{"content": text[at : at+len(mt)], "beginOffset": off(at)}, "type": m.Type}
				mentions = append(mentions, mention{at, mj})
				first = min(first, at)
			}
		}
		if len(mentions) == 0 {
			continue // the text does not contain it
		}
		seen[key] = true
		sort.SliceStable(mentions, func(i, j int) bool { return mentions[i].at < mentions[j].at })
		ms := make([]map[string]any, len(mentions))
		for i, m := range mentions {
			ms[i] = m.m
		}
		ej := map[string]any{"name": strings.TrimSpace(e.Name), "type": e.Type, "metadata": map[string]any{}, "mentions": ms}
		ents = append(ents, kept{ej, math.Max(0, e.Salience), first, mentions})
	}
	var total float64
	for _, e := range ents {
		total += e.salience
	}
	for i := range ents {
		sal := 1 / float64(len(ents))
		if total > 0 {
			sal = ents[i].salience / total
		}
		ents[i].salience = sal
		ents[i].m["salience"] = f32(sal)
	}
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].salience != ents[j].salience {
			return ents[i].salience > ents[j].salience
		}
		return ents[i].first < ents[j].first
	})
	if withSentiment && len(ents) > 0 {
		// A second pass rates each mention found, in its sentence: one
		// answer a mention, so the entities are the same as
		// analyzeEntities'.
		ss := splitSentences(text)
		sentenceOf := func(at int) string {
			for i := len(ss) - 1; i >= 0; i-- {
				if ss[i].start <= at {
					return ss[i].text
				}
			}
			return text
		}
		var list strings.Builder
		n := 0
		for _, e := range ents {
			for _, m := range e.mentions {
				n++
				fmt.Fprintf(&list, "%d. %q (the entity %q) in: %s\n", n, m.m["text"].(map[string]any)["content"], e.m["name"], sentenceOf(m.at))
			}
		}
		item := map[string]any{"type": "object", "properties": map[string]any{
			"score":     map[string]any{"type": "number", "minimum": -1, "maximum": 1},
			"magnitude": map[string]any{"type": "number", "minimum": 0},
		}, "required": []string{"score", "magnitude"}, "additionalProperties": false}
		schema := map[string]any{"type": "object", "properties": map[string]any{
			"mentions": map[string]any{"type": "array", "items": item, "minItems": n, "maxItems": n},
		}, "required": []string{"mentions"}, "additionalProperties": false}
		prompt := "For each numbered mention below, give the sentiment the sentence expresses toward that entity: a score from -1.0 " +
			"(negative) through 0.0 (neutral) to 1.0 (positive), and a magnitude from 0.0 upwards for how strongly it is felt (0.0 for none). " +
			"Answer one entry per mention, " + fmt.Sprint(n) + " in all.\n\n" + list.String()
		var rated struct {
			Mentions []struct {
				Score     float64 `json:"score"`
				Magnitude float64 `json:"magnitude"`
			} `json:"mentions"`
		}
		if err := s.nlGenerate(r, prompt, schema, &rated); err != nil {
			return nil, err
		}
		if len(rated.Mentions) != n {
			return nil, apierror.Internal(nil, "the local model rated %d mentions of %d", len(rated.Mentions), n)
		}
		k := 0
		for _, e := range ents {
			var sum, mag float64
			for _, m := range e.mentions {
				g := rated.Mentions[k]
				k++
				m.m["sentiment"] = sentimentJSON(g.Score, g.Magnitude)
				sum += clamp(g.Score, -1, 1)
				mag += math.Max(0, g.Magnitude)
			}
			e.m["sentiment"] = sentimentJSON(sum/float64(len(e.mentions)), mag)
		}
	}
	out := make([]map[string]any, len(ents))
	for i, e := range ents {
		out[i] = e.m
	}
	return map[string]any{"entities": out, "language": lang}, nil
}

// occurrences are the byte offsets of sub in text, whole words only where
// sub begins or ends with a letter or digit, matched exactly, else
// case-insensitively.
func occurrences(text, sub string) []int {
	find := func(hay, needle string) []int {
		var at []int
		for i := 0; ; {
			j := strings.Index(hay[i:], needle)
			if j < 0 {
				return at
			}
			p := i + j
			if wordEdge(text, p, true, sub) && wordEdge(text, p+len(needle), false, sub) {
				at = append(at, p)
			}
			i = p + len(needle)
		}
	}
	if at := find(text, sub); len(at) > 0 {
		return at
	}
	if lt, ls := strings.ToLower(text), strings.ToLower(sub); len(lt) == len(text) && len(ls) == len(sub) {
		return find(lt, ls)
	}
	return nil
}

// wordEdge reports whether position p of text is a word boundary for sub,
// on its left (before) or right side. Scripts written without spaces
// (Han, Hiragana, Katakana, Thai) have no boundaries to check.
func wordEdge(text string, p int, before bool, sub string) bool {
	var inner, outer rune
	if before {
		inner, _ = utf8.DecodeRuneInString(sub)
		if p == 0 {
			return true
		}
		outer, _ = utf8.DecodeLastRuneInString(text[:p])
	} else {
		inner, _ = utf8.DecodeLastRuneInString(sub)
		if p >= len(text) {
			return true
		}
		outer, _ = utf8.DecodeRuneInString(text[p:])
	}
	word := func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }
	spaceless := func(c rune) bool {
		return unicode.In(c, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai)
	}
	if !word(inner) || spaceless(inner) || spaceless(outer) {
		return true
	}
	return !word(outer)
}

func (s *Server) classifyText(r *http.Request, text string) (any, error) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"categories": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "items": map[string]any{"type": "object",
			"properties": map[string]any{
				"name":       map[string]any{"enum": nlV1Categories},
				"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			}, "required": []string{"name", "confidence"}, "additionalProperties": false}},
	}, "required": []string{"categories"}, "additionalProperties": false}
	prompt := "Classify the text below into the content categories of Google's content taxonomy (paths such as " +
		"\"/Arts & Entertainment/TV & Video/TV Shows & Programs\" or \"/Sports/Team Sports/Soccer\"). Give the most specific " +
		"categories that apply, at most three, each with a confidence from 0 to 1, the most likely first.\n\nText:\n" + text
	var got struct {
		Categories []struct {
			Name       string  `json:"name"`
			Confidence float64 `json:"confidence"`
		} `json:"categories"`
	}
	if err := s.nlGenerate(r, prompt, schema, &got); err != nil {
		return nil, err
	}
	valid := map[string]bool{}
	for _, c := range nlV1Categories {
		valid[c] = true
	}
	conf := map[string]float64{}
	var names []string
	for _, c := range got.Categories {
		if !valid[c.Name] {
			continue
		}
		if _, ok := conf[c.Name]; !ok {
			names = append(names, c.Name)
		}
		conf[c.Name] = math.Max(conf[c.Name], clamp(c.Confidence, 0, 1))
	}
	// The V1 model answers "only the most relevant categories": a category
	// whose descendant also applies is dropped.
	var out []map[string]any
	for _, n := range names {
		specific := true
		for _, o := range names {
			if strings.HasPrefix(o, n+"/") {
				specific = false
			}
		}
		if specific {
			out = append(out, map[string]any{"name": n, "confidence": f32(conf[n])})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return conf[out[i]["name"].(string)] > conf[out[j]["name"].(string)] })
	if out == nil {
		out = []map[string]any{}
	}
	return map[string]any{"categories": out}, nil
}
