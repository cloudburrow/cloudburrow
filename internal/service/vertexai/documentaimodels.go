package vertexai

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
)

// The models a custom processor version is trained into (#2106). Google's
// Custom Extractor, Custom Classifier and Custom Splitter train Google's own
// models, which CloudBurrow has no copy of; these are CloudBurrow's, trained
// on this machine from the labeled documents, and every one is made of
// counts, so uptraining (base_processor_version) adds the new documents'
// counts to the base version's:
//
//   - Classifier: multinomial naive Bayes over the document's words, a
//     class a document type of the schema. A document is answered with an
//     entity for every class, its confidence the posterior, most probable
//     first.
//   - Splitter: the same over each page's words, the page's class the
//     labeled subdocument holding it, plus a second naive Bayes deciding
//     whether a page starts a subdocument. Pages are grouped into
//     subdocuments where a page starts one or its class changes; each is
//     answered as an entity with its type, confidence and page_anchor.
//   - Extractor: for each entity type, the words that precede a labeled
//     value on its line (its anchor, "Invoice number:") and the value's
//     shape (letters, digits, punctuation) and length in words. A
//     document's values are what follows a learnt anchor, cut to the
//     learnt length, its confidence the share of the type's training
//     values that anchor preceded.

// docModel is a trained version's model.
type docModel struct {
	Kind string `json:"kind"` // "classifier", "splitter" or "extractor"
	// Class and Start are the classifier's (documents) or the splitter's
	// (pages) naive Bayes.
	Class *naiveBayes `json:"class,omitempty"`
	Start *naiveBayes `json:"start,omitempty"`
	// Fields are the extractor's, by entity type.
	Fields map[string]*fieldModel `json:"fields,omitempty"`
}

// naiveBayes is a multinomial naive Bayes classifier's counts.
type naiveBayes struct {
	Docs  map[string]int            `json:"docs"`
	Words map[string]map[string]int `json:"words"`
	Total map[string]int            `json:"total"`
}

func newNaiveBayes() *naiveBayes {
	return &naiveBayes{Docs: map[string]int{}, Words: map[string]map[string]int{}, Total: map[string]int{}}
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

func docWords(text string) []string {
	return wordRE.FindAllString(strings.ToLower(text), -1)
}

func (nb *naiveBayes) add(class, text string) {
	nb.Docs[class]++
	if nb.Words[class] == nil {
		nb.Words[class] = map[string]int{}
	}
	for _, w := range docWords(text) {
		nb.Words[class][w]++
		nb.Total[class]++
	}
}

func (nb *naiveBayes) merge(o *naiveBayes) {
	if o == nil {
		return
	}
	for c, n := range o.Docs {
		nb.Docs[c] += n
	}
	for c, ws := range o.Words {
		if nb.Words[c] == nil {
			nb.Words[c] = map[string]int{}
		}
		for w, n := range ws {
			nb.Words[c][w] += n
		}
	}
	for c, n := range o.Total {
		nb.Total[c] += n
	}
}

// scored is a class and its posterior.
type scored struct {
	class string
	p     float64
}

// predict is each class's posterior for text, most probable first, over
// the classes named (every trained one when none is).
func (nb *naiveBayes) predict(text string, classes []string) []scored {
	if len(classes) == 0 {
		for c := range nb.Docs {
			classes = append(classes, c)
		}
	}
	vocab := map[string]bool{}
	docs := 0
	for _, ws := range nb.Words {
		for w := range ws {
			vocab[w] = true
		}
	}
	for _, n := range nb.Docs {
		docs += n
	}
	words := docWords(text)
	logs := make([]float64, len(classes))
	for i, c := range classes {
		l := math.Log(float64(nb.Docs[c]+1) / float64(docs+len(classes)))
		for _, w := range words {
			l += math.Log(float64(nb.Words[c][w]+1) / float64(nb.Total[c]+len(vocab)+1))
		}
		logs[i] = l
	}
	maxL := math.Inf(-1)
	for _, l := range logs {
		maxL = math.Max(maxL, l)
	}
	sum := 0.0
	out := make([]scored, len(classes))
	for i, l := range logs {
		out[i] = scored{classes[i], math.Exp(l - maxL)}
		sum += out[i].p
	}
	for i := range out {
		out[i].p /= sum
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].p != out[j].p {
			return out[i].p > out[j].p
		}
		return out[i].class < out[j].class
	})
	return out
}

// fieldModel is the extractor's model of one entity type.
type fieldModel struct {
	Anchors map[string]int `json:"anchors"`
	Shapes  map[string]int `json:"shapes"`
	Words   int            `json:"words"` // the longest value, in words
	Values  int            `json:"values"`
}

// anchorWords is how many words before a value make its anchor.
const anchorWords = 3

// anchorOf is the words before offset start on its line (or the line
// above, when the value starts its line), lower-cased.
func anchorOf(text []rune, start int) string {
	i := start
	for i > 0 && text[i-1] != '\n' {
		i--
	}
	before := string(text[i:start])
	if strings.TrimSpace(before) == "" && i > 0 {
		j := i - 1
		for j > 0 && text[j-1] != '\n' {
			j--
		}
		before = string(text[j : i-1])
	}
	w := docWords(before)
	if len(w) > anchorWords {
		w = w[len(w)-anchorWords:]
	}
	return strings.Join(w, " ")
}

// shapeOf is a value's shape: each run of letters a, digits 9, anything
// else kept.
func shapeOf(v string) string {
	var b strings.Builder
	last := rune(0)
	for _, r := range v {
		c := r
		switch {
		case unicode.IsLetter(r):
			c = 'a'
		case unicode.IsDigit(r):
			c = '9'
		case unicode.IsSpace(r):
			c = ' '
		}
		if c != last || (c != 'a' && c != '9' && c != ' ') {
			b.WriteRune(c)
		}
		last = c
	}
	return b.String()
}

func (f *fieldModel) merge(o *fieldModel) {
	for a, n := range o.Anchors {
		f.Anchors[a] += n
	}
	for s, n := range o.Shapes {
		f.Shapes[s] += n
	}
	f.Words = max(f.Words, o.Words)
	f.Values += o.Values
}

// mentionSpan is an entity's span in Document.text, in runes.
func mentionSpan(e *documentaipb.Document_Entity) (int, int, bool) {
	segs := e.GetTextAnchor().GetTextSegments()
	if len(segs) == 0 {
		return 0, 0, false
	}
	return int(segs[0].GetStartIndex()), int(segs[len(segs)-1].GetEndIndex()), true
}

// trainExtractor learns each type's anchors and shapes from the labeled
// documents' entities.
func trainExtractor(m *docModel, docs []*documentaipb.Document) {
	if m.Fields == nil {
		m.Fields = map[string]*fieldModel{}
	}
	for _, d := range docs {
		text := []rune(d.GetText())
		for _, e := range d.GetEntities() {
			start, end, ok := mentionSpan(e)
			if !ok || start < 0 || end > len(text) || start >= end {
				continue
			}
			f := m.Fields[e.GetType()]
			if f == nil {
				f = &fieldModel{Anchors: map[string]int{}, Shapes: map[string]int{}}
				m.Fields[e.GetType()] = f
			}
			v := strings.TrimSpace(string(text[start:end]))
			f.Anchors[anchorOf(text, start)]++
			f.Shapes[shapeOf(v)]++
			f.Words = max(f.Words, len(strings.Fields(v)))
			f.Values++
		}
	}
}

// extract finds each schema type's values in text.
func (m *docModel) extract(text string, schema *documentaipb.DocumentSchema) []*documentaipb.Document_Entity {
	runes := []rune(text)
	lower := []rune(strings.ToLower(text))
	var out []*documentaipb.Document_Entity
	types := extractorTypes(schema)
	for _, t := range types {
		f := m.Fields[t.name]
		if f == nil || f.Values == 0 {
			continue
		}
		type hit struct {
			start, end int
			conf       float64
		}
		var hits []hit
		for a, n := range f.Anchors {
			if a == "" {
				continue
			}
			for _, at := range anchorPositions(lower, a) {
				start, end := valueAfter(runes, at, f.Words)
				if start >= end {
					continue
				}
				conf := float64(n) / float64(f.Values)
				if f.Shapes[shapeOf(string(runes[start:end]))] == 0 {
					conf /= 2
				}
				hits = append(hits, hit{start, end, conf})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].conf != hits[j].conf {
				return hits[i].conf > hits[j].conf
			}
			return hits[i].start < hits[j].start
		})
		seen := map[int]bool{}
		for _, h := range hits {
			if seen[h.start] {
				continue
			}
			seen[h.start] = true
			out = append(out, &documentaipb.Document_Entity{Type: t.name, MentionText: string(runes[h.start:h.end]),
				Confidence: float32(h.conf), TextAnchor: anchor(int64(h.start), int64(h.end))})
			if !t.multiple {
				break
			}
		}
	}
	return out
}

type schemaType struct {
	name     string
	multiple bool
}

// extractorTypes are the schema's extractable entity types: the
// properties of its document-level entity type (base_types "document"),
// or its entity types when none has properties.
func extractorTypes(schema *documentaipb.DocumentSchema) []schemaType {
	var out []schemaType
	for _, et := range schema.GetEntityTypes() {
		for _, p := range et.GetProperties() {
			m := p.GetOccurrenceType()
			out = append(out, schemaType{p.GetName(), m == documentaipb.DocumentSchema_EntityType_Property_OPTIONAL_MULTIPLE ||
				m == documentaipb.DocumentSchema_EntityType_Property_REQUIRED_MULTIPLE})
		}
	}
	if len(out) == 0 {
		for _, et := range schema.GetEntityTypes() {
			out = append(out, schemaType{name: et.GetName()})
		}
	}
	return out
}

// classifierTypes are the schema's classes: its entity types, other than
// a document-level type that only holds properties.
func classifierTypes(schema *documentaipb.DocumentSchema) []string {
	var out []string
	for _, et := range schema.GetEntityTypes() {
		if len(et.GetProperties()) == 0 {
			out = append(out, et.GetName())
		}
	}
	sort.Strings(out)
	return out
}

// anchorPositions are the rune offsets just after each whole-word
// occurrence of anchor's words in lower, separated by anything but a
// newline.
func anchorPositions(lower []rune, a string) []int {
	words := strings.Fields(a)
	var pat strings.Builder
	pat.WriteString(`(?:^|[^\p{L}\p{N}])`)
	for i, w := range words {
		if i > 0 {
			pat.WriteString(`[^\p{L}\p{N}\n]+`)
		}
		pat.WriteString(regexp.QuoteMeta(w))
	}
	pat.WriteString(`(?:[^\p{L}\p{N}]|$)`)
	re := regexp.MustCompile(pat.String())
	s := string(lower)
	var out []int
	for _, loc := range re.FindAllStringIndex(s, -1) {
		end := loc[1]
		// Back off the trailing separator the pattern consumed.
		if end > loc[0] && end <= len(s) {
			r := []rune(s[:end])
			n := len(r)
			if n > 0 && !unicode.IsLetter(r[n-1]) && !unicode.IsDigit(r[n-1]) {
				n--
			}
			out = append(out, n)
		}
	}
	return out
}

// valueAfter is the span of up to words words after offset at, on at's
// line (or the next line, when nothing follows on it), past separators.
func valueAfter(text []rune, at, words int) (int, int) {
	i := at
	for i < len(text) && text[i] != '\n' && !unicode.IsLetter(text[i]) && !unicode.IsDigit(text[i]) && text[i] != '$' && text[i] != '€' && text[i] != '£' {
		i++
	}
	if i < len(text) && text[i] == '\n' {
		i++
		for i < len(text) && unicode.IsSpace(text[i]) && text[i] != '\n' {
			i++
		}
	}
	start := i
	n := 0
	end := start
	for i < len(text) && text[i] != '\n' {
		for i < len(text) && unicode.IsSpace(text[i]) && text[i] != '\n' {
			i++
		}
		if i >= len(text) || text[i] == '\n' {
			break
		}
		for i < len(text) && !unicode.IsSpace(text[i]) {
			i++
		}
		n++
		end = i
		if n >= max(words, 1) {
			break
		}
	}
	return start, end
}

// --- splitter ------------------------------------------------------------

// pageTexts are each page's text: its layout's anchor in Document.text,
// or, for a document whose pages carry none, form-feed separated text.
func pageTexts(d *documentaipb.Document) []string {
	text := []rune(d.GetText())
	var out []string
	for _, p := range d.GetPages() {
		var b strings.Builder
		for _, s := range p.GetLayout().GetTextAnchor().GetTextSegments() {
			st, en := int(s.GetStartIndex()), int(s.GetEndIndex())
			if st >= 0 && en <= len(text) && st < en {
				b.WriteString(string(text[st:en]))
			}
		}
		out = append(out, b.String())
	}
	if len(out) == 0 {
		out = strings.Split(d.GetText(), "\f")
	}
	return out
}

// pageLabels are each page's class and whether it starts a subdocument,
// from the splitter's labels (entities with page_anchor page_refs).
func pageLabels(d *documentaipb.Document, n int) ([]string, []bool) {
	classes := make([]string, n)
	starts := make([]bool, n)
	for _, e := range d.GetEntities() {
		refs := e.GetPageAnchor().GetPageRefs()
		first := -1
		for _, r := range refs {
			pg := int(r.GetPage())
			if pg < 0 || pg >= n {
				continue
			}
			classes[pg] = e.GetType()
			if first < 0 || pg < first {
				first = pg
			}
		}
		if first >= 0 {
			starts[first] = true
		}
	}
	return classes, starts
}

const (
	startYes = "start"
	startNo  = "continue"
)

func trainSplitter(m *docModel, docs []*documentaipb.Document) {
	if m.Class == nil {
		m.Class, m.Start = newNaiveBayes(), newNaiveBayes()
	}
	for _, d := range docs {
		pages := pageTexts(d)
		classes, starts := pageLabels(d, len(pages))
		for i, t := range pages {
			if classes[i] == "" {
				continue
			}
			m.Class.add(classes[i], t)
			if starts[i] {
				m.Start.add(startYes, t)
			} else {
				m.Start.add(startNo, t)
			}
		}
	}
}

// split groups a document's pages into typed subdocuments.
func (m *docModel) split(d *documentaipb.Document, classes []string) []*documentaipb.Document_Entity {
	pages := pageTexts(d)
	var out []*documentaipb.Document_Entity
	var cur *documentaipb.Document_Entity
	var confs []float64
	flush := func() {
		if cur == nil {
			return
		}
		sum := 0.0
		for _, c := range confs {
			sum += c
		}
		cur.Confidence = float32(sum / float64(len(confs)))
		out = append(out, cur)
	}
	for i, t := range pages {
		best := m.Class.predict(t, classes)
		if len(best) == 0 {
			continue
		}
		start := i == 0
		if !start && m.Start != nil && len(m.Start.Docs) == 2 {
			start = m.Start.predict(t, []string{startYes, startNo})[0].class == startYes
		}
		if cur == nil || start || cur.GetType() != best[0].class {
			flush()
			cur = &documentaipb.Document_Entity{Type: best[0].class, PageAnchor: &documentaipb.Document_PageAnchor{}}
			confs = nil
		}
		cur.PageAnchor.PageRefs = append(cur.PageAnchor.PageRefs, &documentaipb.Document_PageAnchor_PageRef{Page: int64(i)})
		confs = append(confs, best[0].p)
	}
	flush()
	return out
}

// classify answers an entity per class, most probable first.
func (m *docModel) classify(text string, classes []string) []*documentaipb.Document_Entity {
	var out []*documentaipb.Document_Entity
	for _, s := range m.Class.predict(text, classes) {
		out = append(out, &documentaipb.Document_Entity{Type: s.class, Confidence: float32(s.p)})
	}
	return out
}

// docLabel is a classifier training document's class: its entity of the
// highest confidence (a labeled document has one, confidence 1).
func docLabel(d *documentaipb.Document) string {
	best, conf := "", float32(-1)
	for _, e := range d.GetEntities() {
		c := e.GetConfidence()
		if c == 0 {
			c = 1
		}
		if c > conf {
			best, conf = e.GetType(), c
		}
	}
	return best
}

func trainClassifier(m *docModel, docs []*documentaipb.Document) {
	if m.Class == nil {
		m.Class = newNaiveBayes()
	}
	for _, d := range docs {
		if c := docLabel(d); c != "" {
			m.Class.add(c, d.GetText())
		}
	}
}

// mergeModel adds base's counts to m (uptraining).
func mergeModel(m, base *docModel) {
	if base == nil {
		return
	}
	if base.Class != nil {
		if m.Class == nil {
			m.Class = newNaiveBayes()
		}
		m.Class.merge(base.Class)
	}
	if base.Start != nil {
		if m.Start == nil {
			m.Start = newNaiveBayes()
		}
		m.Start.merge(base.Start)
	}
	for t, f := range base.Fields {
		if m.Fields == nil {
			m.Fields = map[string]*fieldModel{}
		}
		if m.Fields[t] == nil {
			m.Fields[t] = &fieldModel{Anchors: map[string]int{}, Shapes: map[string]int{}}
		}
		m.Fields[t].merge(f)
	}
}

// predictEntities is what version model m answers for d.
func (m *docModel) predictEntities(d *documentaipb.Document, schema *documentaipb.DocumentSchema) []*documentaipb.Document_Entity {
	switch m.Kind {
	case "classifier":
		return m.classify(d.GetText(), classifierTypes(schema))
	case "splitter":
		return m.split(d, classifierTypes(schema))
	default:
		return m.extract(d.GetText(), schema)
	}
}
