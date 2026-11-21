package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"cloud.google.com/go/documentai/apiv1/documentaipb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Generative custom processors (#2124). Google's Custom Extractor is
// generative: "the default model type is a foundation model that performs
// zero-shot prediction, that is, without training", from the schema and
// its document prompt, and labeled documents improve it
// (https://docs.cloud.google.com/document-ai/docs/workbench/build-custom-processor,
// read 2026-11-21). Where this instance serves a local generation model
// with constrained decoding (the local Gemma runtime Vertex AI's
// generateContent runs on, `up --local-ai-model`, docs/generation.md), a
// trained version is generative:
//
//   - Extractor: zero-shot from the schema (each field's name, value type
//     and occurrence, and the schema's description as the document
//     prompt) when the training split has no labeled documents, few-shot
//     with up to docAIShots labeled training documents as examples
//     otherwise. The model answers each field's value and a confidence,
//     as JSON valid against a schema built from the document schema; a
//     value is anchored to the first place it occurs in the text (case and
//     whitespace ignored), and a value that does not occur is dropped.
//   - Classifier: the labeled training documents, up to docAIShots of each
//     class, are the examples; the model answers the class and its
//     confidence, and every other class shares the rest.
//   - Splitter: the model answers each page's class and whether it starts
//     a subdocument, with up to docAIShots labeled training documents'
//     pages as examples; pages are grouped as the counting splitter
//     groups them.
//
// Every version also keeps the counting models (documentaimodels.go),
// which answer when no local model is installed: a version trained
// without one is never generative, and a generative version answers
// with its counting model when the instance later runs without a model.
// Decoding is greedy (temperature 0, top-k 1), so a document is always
// answered the same way. Texts are cut to docAIGenChars (the target) and
// docAIShotChars (each example), as the local model's context is small.
//
// UNVERIFIED (re-read 2026-11-21, the page above, which does not say):
// Google's foundation model's prompt, its confidence scores, and how many
// labeled documents its few-shot versions read.

const (
	docAIShots     = 3
	docAIShotChars = 1500
	docAIGenChars  = 6000
)

// fewShot is a labeled training document kept as an example.
type fewShot struct {
	Text string `json:"text"`
	// Fields are an extractor example's values, by type.
	Fields []shotField `json:"fields,omitempty"`
	// Class is a classifier example's class.
	Class string `json:"class,omitempty"`
	// Pages are a splitter example's pages.
	Pages []shotPage `json:"pages,omitempty"`
}

type shotField struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type shotPage struct {
	Text   string `json:"text"`
	Class  string `json:"class"`
	Starts bool   `json:"starts"`
}

func cutText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + " …"
}

// generativeAvailable reports whether the instance serves a local model
// that answers JSON valid against a schema.
func (s *Server) generativeAvailable() bool {
	return s.gen != nil && honoursOptions(s.gen)
}

// keepShots records the training documents a generative version reads as
// examples.
func keepShots(m *docModel, docs []*documentaipb.Document) {
	perClass := map[string]int{}
	for _, d := range docs {
		if len(d.GetEntities()) == 0 {
			continue
		}
		switch m.Kind {
		case "classifier":
			c := docLabel(d)
			if c == "" || perClass[c] >= docAIShots {
				continue
			}
			perClass[c]++
			m.Shots = append(m.Shots, fewShot{Text: cutText(d.GetText(), docAIShotChars), Class: c})
		case "splitter":
			if len(m.Shots) >= docAIShots {
				continue
			}
			texts := pageTexts(d)
			labels, starts := pageLabels(d, len(texts))
			sh := fewShot{}
			for i, t := range texts {
				if labels[i] == "" {
					continue
				}
				sh.Pages = append(sh.Pages, shotPage{Text: cutText(t, docAIShotChars/2), Class: labels[i], Starts: starts[i]})
			}
			if len(sh.Pages) > 0 {
				m.Shots = append(m.Shots, sh)
			}
		default:
			if len(m.Shots) >= docAIShots {
				continue
			}
			text := []rune(d.GetText())
			sh := fewShot{Text: cutText(d.GetText(), docAIShotChars)}
			for _, e := range d.GetEntities() {
				v := e.GetMentionText()
				if st, en, ok := mentionSpan(e); v == "" && ok && st >= 0 && en <= len(text) && st < en {
					v = string(text[st:en])
				}
				if v = strings.TrimSpace(v); v != "" {
					sh.Fields = append(sh.Fields, shotField{e.GetType(), v})
				}
			}
			m.Shots = append(m.Shots, sh)
		}
	}
}

// genJSON asks the local model for JSON valid against schema, greedily.
func (s *Server) genJSON(ctx context.Context, prompt string, schema map[string]any, out any) error {
	sb, _ := json.Marshal(schema)
	zero, one := float32(0), float32(1)
	gr := GenerateContentRequest{
		Contents:         []Content{{Role: "user", Parts: []Part{{Text: prompt}}}},
		GenerationConfig: &GenerationConfig{Temperature: &zero, TopK: &one, ResponseMIMEType: "application/json", ResponseJSONSchema: sb},
	}
	req, _ := json.Marshal(gr)
	resp, err := s.generateUnary(ctx, s.gen.Model(), bytes.NewReader(req))
	if err != nil {
		return err
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return apierror.Internal(nil, "the local model answered nothing")
	}
	text := strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text)
	if err := json.Unmarshal([]byte(text), out); err != nil {
		// An answer cut off by the model's context keeps its complete
		// array elements.
		if fixed, ok := truncatedJSON(text); ok && json.Unmarshal([]byte(fixed), out) == nil {
			return nil
		}
		return apierror.Internal(err, "the local model answered JSON that does not match the schema (%d characters, finish reason %s: %v; it ends %q)",
			len(text), resp.Candidates[0].FinishReason, err, text[max(0, len(text)-120):])
	}
	return nil
}

// docPredict is what version model m answers for d: generatively when m is
// generative and the instance serves a local model, else with its
// counting model.
func (s *Server) docPredict(ctx context.Context, m *docModel, d *documentaipb.Document, schema *documentaipb.DocumentSchema) ([]*documentaipb.Document_Entity, error) {
	if !m.Generative || !s.generativeAvailable() {
		return m.predictEntities(d, schema), nil
	}
	switch m.Kind {
	case "classifier":
		return s.genClassify(ctx, m, d.GetText(), classifierTypes(schema))
	case "splitter":
		return s.genSplit(ctx, m, d, classifierTypes(schema))
	}
	return s.genExtract(ctx, m, d.GetText(), schema)
}

func confSchema() map[string]any { return map[string]any{"type": "number", "minimum": 0, "maximum": 1} }

func (s *Server) genExtract(ctx context.Context, m *docModel, text string, schema *documentaipb.DocumentSchema) ([]*documentaipb.Document_Entity, error) {
	types := extractorTypes(schema)
	if len(types) == 0 {
		return nil, nil
	}
	props := map[string]*documentaipb.DocumentSchema_EntityType_Property{}
	for _, et := range schema.GetEntityTypes() {
		for _, p := range et.GetProperties() {
			props[p.GetName()] = p
		}
	}
	var names []string
	var b strings.Builder
	b.WriteString("Extract the fields below from the document. Answer each field's value exactly as it is written in the document, " +
		"and your confidence from 0 to 1 that it is right. Leave out a field the document does not have; answer a field that " +
		"occurs more than once once for each value.\n")
	if d := strings.TrimSpace(schema.GetDescription()); d != "" {
		b.WriteString("\nThe documents: " + d + "\n")
	}
	b.WriteString("\nFields:\n")
	for _, t := range types {
		names = append(names, t.name)
		line := "- " + t.name
		if p := props[t.name]; p != nil {
			if p.GetDisplayName() != "" && p.GetDisplayName() != t.name {
				line += " (" + p.GetDisplayName() + ")"
			}
			if vt := p.GetValueType(); vt != "" {
				line += ", a " + vt
			}
		}
		if t.multiple {
			line += ", any number of values"
		} else {
			line += ", one value"
		}
		b.WriteString(line + "\n")
	}
	for i, sh := range m.Shots {
		ans, _ := json.Marshal(map[string]any{"entities": shotEntities(sh.Fields)})
		fmt.Fprintf(&b, "\nExample %d.\nDocument:\n%s\nAnswer:\n%s\n", i+1, sh.Text, ans)
	}
	b.WriteString("\nDocument:\n" + cutText(text, docAIGenChars) + "\nAnswer:\n")
	item := map[string]any{"type": "object", "properties": map[string]any{
		"type": map[string]any{"enum": names}, "value": map[string]any{"type": "string"}, "confidence": confSchema(),
	}, "required": []string{"type", "value", "confidence"}, "additionalProperties": false}
	js := map[string]any{"type": "object", "properties": map[string]any{"entities": map[string]any{"type": "array", "items": item}},
		"required": []string{"entities"}, "additionalProperties": false}
	var got struct {
		Entities []struct {
			Type       string  `json:"type"`
			Value      string  `json:"value"`
			Confidence float64 `json:"confidence"`
		} `json:"entities"`
	}
	if err := s.genJSON(ctx, b.String(), js, &got); err != nil {
		return nil, err
	}
	multiple := map[string]bool{}
	for _, t := range types {
		multiple[t.name] = t.multiple
	}
	runes := []rune(text)
	seen := map[string]bool{}
	var out []*documentaipb.Document_Entity
	for _, e := range got.Entities {
		v := strings.TrimSpace(e.Value)
		if v == "" || (!multiple[e.Type] && seen[e.Type]) {
			continue
		}
		st, en, ok := findFuzzy(runes, v)
		if !ok {
			continue
		}
		seen[e.Type] = true
		out = append(out, &documentaipb.Document_Entity{Type: e.Type, MentionText: string(runes[st:en]),
			Confidence: float32(clamp(e.Confidence, 0, 1)), TextAnchor: anchor(int64(st), int64(en))})
	}
	return out, nil
}

func shotEntities(fs []shotField) []map[string]any {
	out := []map[string]any{}
	for _, f := range fs {
		out = append(out, map[string]any{"type": f.Type, "value": f.Value, "confidence": 1})
	}
	return out
}

// findFuzzy is where v first occurs in text, case and runs of whitespace
// ignored, as a span of the text's runes. A one-word value may sit inside
// a longer token ("$1,200.00," holds "$1,200.00").
func findFuzzy(text []rune, v string) (int, int, bool) {
	want := strings.Fields(v)
	if len(want) == 0 {
		return 0, 0, false
	}
	type word struct{ s, e int }
	var words []word
	for i := 0; i < len(text); {
		for i < len(text) && isSpace(text[i]) {
			i++
		}
		j := i
		for j < len(text) && !isSpace(text[j]) {
			j++
		}
		if j > i {
			words = append(words, word{i, j})
		}
		i = j
	}
	lower := func(w word) []rune { return []rune(strings.ToLower(string(text[w.s:w.e]))) }
	if len(want) == 1 {
		needle := []rune(strings.ToLower(want[0]))
		for _, w := range words {
			tok := lower(w)
			if len(tok) != w.e-w.s {
				continue
			}
			if at := runeIndex(tok, needle); at >= 0 {
				return w.s + at, w.s + at + len(needle), true
			}
		}
		return 0, 0, false
	}
	for i := 0; i+len(want) <= len(words); i++ {
		ok := true
		for k, w := range want {
			if !strings.EqualFold(string(text[words[i+k].s:words[i+k].e]), w) {
				ok = false
				break
			}
		}
		if ok {
			return words[i].s, words[i+len(want)-1].e, true
		}
	}
	return 0, 0, false
}

func runeIndex(hay, needle []rune) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == '\f' || r == '\v'
}

func (s *Server) genClassify(ctx context.Context, m *docModel, text string, classes []string) ([]*documentaipb.Document_Entity, error) {
	if len(classes) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("Classify the document as one of these document types: " + strings.Join(classes, ", ") +
		". Answer the type and your confidence from 0 to 1.\n")
	for i, sh := range m.Shots {
		fmt.Fprintf(&b, "\nExample %d.\nDocument:\n%s\nAnswer:\n{\"class\":%q,\"confidence\":1}\n", i+1, sh.Text, sh.Class)
	}
	b.WriteString("\nDocument:\n" + cutText(text, docAIGenChars) + "\nAnswer:\n")
	js := map[string]any{"type": "object", "properties": map[string]any{"class": map[string]any{"enum": classes}, "confidence": confSchema()},
		"required": []string{"class", "confidence"}, "additionalProperties": false}
	var got struct {
		Class      string  `json:"class"`
		Confidence float64 `json:"confidence"`
	}
	if err := s.genJSON(ctx, b.String(), js, &got); err != nil {
		return nil, err
	}
	return classEntities(got.Class, got.Confidence, classes), nil
}

// classEntities answers an entity per class, the chosen one first with
// its confidence, the others sharing the rest.
func classEntities(chosen string, conf float64, classes []string) []*documentaipb.Document_Entity {
	conf = clamp(conf, 0, 1)
	if len(classes) > 1 {
		// The chosen class is never less likely than another.
		conf = math.Max(conf, 1/float64(len(classes)))
	}
	out := []*documentaipb.Document_Entity{{Type: chosen, Confidence: float32(conf)}}
	rest := 0.0
	if len(classes) > 1 {
		rest = (1 - conf) / float64(len(classes)-1)
	}
	for _, c := range classes {
		if c != chosen {
			out = append(out, &documentaipb.Document_Entity{Type: c, Confidence: float32(rest)})
		}
	}
	return out
}

func (s *Server) genSplit(ctx context.Context, m *docModel, d *documentaipb.Document, classes []string) ([]*documentaipb.Document_Entity, error) {
	pages := pageTexts(d)
	if len(pages) == 0 || len(classes) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("The file below holds one or more documents, each of one of these types: " + strings.Join(classes, ", ") +
		". For each page, in order, answer its document type, whether it starts a new document, and your confidence from 0 to 1.\n")
	writePages := func(ps []shotPage) {
		for i, p := range ps {
			fmt.Fprintf(&b, "Page %d:\n%s\n", i+1, p.Text)
		}
	}
	for i, sh := range m.Shots {
		fmt.Fprintf(&b, "\nExample %d.\n", i+1)
		writePages(sh.Pages)
		var ans []map[string]any
		for _, p := range sh.Pages {
			ans = append(ans, map[string]any{"class": p.Class, "starts": p.Starts, "confidence": 1})
		}
		a, _ := json.Marshal(map[string]any{"pages": ans})
		fmt.Fprintf(&b, "Answer:\n%s\n", a)
	}
	b.WriteString("\nFile:\n")
	per := max(docAIGenChars/len(pages), 200)
	var target []shotPage
	for _, t := range pages {
		target = append(target, shotPage{Text: cutText(t, per)})
	}
	writePages(target)
	b.WriteString("Answer:\n")
	item := map[string]any{"type": "object", "properties": map[string]any{"class": map[string]any{"enum": classes},
		"starts": map[string]any{"type": "boolean"}, "confidence": confSchema()},
		"required": []string{"class", "starts", "confidence"}, "additionalProperties": false}
	js := map[string]any{"type": "object", "properties": map[string]any{"pages": map[string]any{"type": "array", "items": item,
		"minItems": len(pages), "maxItems": len(pages)}}, "required": []string{"pages"}, "additionalProperties": false}
	var got struct {
		Pages []struct {
			Class      string  `json:"class"`
			Starts     bool    `json:"starts"`
			Confidence float64 `json:"confidence"`
		} `json:"pages"`
	}
	if err := s.genJSON(ctx, b.String(), js, &got); err != nil {
		return nil, err
	}
	if len(got.Pages) != len(pages) {
		return nil, apierror.Internal(nil, "the local model answered %d pages of %d", len(got.Pages), len(pages))
	}
	var out []*documentaipb.Document_Entity
	var cur *documentaipb.Document_Entity
	var sum float64
	n := 0
	flush := func() {
		if cur != nil {
			cur.Confidence = float32(sum / float64(n))
			out = append(out, cur)
		}
	}
	for i, p := range got.Pages {
		if cur == nil || p.Starts || cur.GetType() != p.Class {
			flush()
			cur, sum, n = &documentaipb.Document_Entity{Type: p.Class, PageAnchor: &documentaipb.Document_PageAnchor{}}, 0, 0
		}
		cur.PageAnchor.PageRefs = append(cur.PageAnchor.PageRefs, &documentaipb.Document_PageAnchor_PageRef{Page: int64(i)})
		sum += clamp(p.Confidence, 0, 1)
		n++
	}
	flush()
	return out, nil
}

// truncatedJSON closes JSON that ends early, dropping the array element or
// object member being written: the text up to the last complete element
// of an array, its open arrays and objects closed. ok is false when no
// array element completed.
func truncatedJSON(text string) (string, bool) {
	var stack []byte
	inStr, esc := false, false
	cut, cutDepth := -1, 0
	var cutStack []byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
			if len(stack) > 0 && stack[len(stack)-1] == '[' {
				cut, cutDepth, cutStack = i+1, len(stack), append([]byte(nil), stack...)
			}
		case ',':
			if len(stack) > 0 && stack[len(stack)-1] == '[' {
				cut, cutDepth, cutStack = i, len(stack), append([]byte(nil), stack...)
			}
		}
	}
	if cut < 0 || cutDepth == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(text[:cut])
	for i := len(cutStack) - 1; i >= 0; i-- {
		if cutStack[i] == '[' {
			b.WriteByte(']')
		} else {
			b.WriteByte('}')
		}
	}
	return b.String(), true
}
