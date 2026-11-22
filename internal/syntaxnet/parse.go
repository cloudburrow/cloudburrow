package syntaxnet

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// state is a ParserState (parser_state.cc): the input position, the
// stack, and each token's head, label and tag. Token -1 is the root.
type state struct {
	n      int
	next   int
	stack  []int
	head   []int
	label  []int
	tag    []int
	score  float64
	parent *state
}

func newState(n int) *state {
	s := &state{n: n, head: make([]int, n), label: make([]int, n), tag: make([]int, n)}
	for i := range n {
		s.head[i], s.label[i], s.tag[i] = -1, -1, -1
	}
	return s
}

func (s *state) clone() *state {
	c := *s
	c.stack = append([]int(nil), s.stack...)
	c.head = append([]int(nil), s.head...)
	c.label = append([]int(nil), s.label...)
	c.tag = append([]int(nil), s.tag...)
	return &c
}

func (s *state) input(off int) int {
	i := s.next + off
	if i >= -1 && i < s.n {
		return i
	}
	return -2
}

func (s *state) stackAt(p int) int {
	i := len(s.stack) - 1 - p
	if p < 0 || i < 0 {
		return -2
	}
	return s.stack[i]
}

func (s *state) headOf(i int) int {
	if i == -1 {
		return -1
	}
	return s.head[i]
}

func (s *state) leftmostChild(i, n int) int {
	for ; n > 0; n-- {
		j := -1
		for ; j < i; j++ {
			if s.headOf(j) == i {
				break
			}
		}
		if j == i {
			return -2
		}
		i = j
	}
	return i
}

func (s *state) rightmostChild(i, n int) int {
	for ; n > 0; n-- {
		j := s.n - 1
		for ; j > i; j-- {
			if s.headOf(j) == i {
				break
			}
		}
		if j == i {
			return -2
		}
		i = j
	}
	return i
}

func (s *state) leftSibling(i, n int) int {
	if i == -1 && n > 0 {
		return -2
	}
	j := i
	for n > 0 {
		j--
		if j == -1 {
			return -2
		}
		if s.headOf(j) == s.headOf(i) {
			n--
		}
	}
	return j
}

func (s *state) rightSibling(i, n int) int {
	if i == -1 && n > 0 {
		return -2
	}
	j := i
	for n > 0 {
		j++
		if j == s.n {
			return -2
		}
		if s.headOf(j) == s.headOf(i) {
			n--
		}
	}
	return j
}

// step is one locator of a feature: input, stack, child or sibling, and
// its argument.
type step struct {
	op  string
	arg int
}

// feature is a parsed feature descriptor such as
// stack(1).child(-1).sibling(1).label: its locators and its function.
type feature struct {
	steps []step
	fn    string
	arg   int // prefix/suffix length
}

func parseFeatures(spec string) [][]feature {
	var groups [][]feature
	for _, g := range strings.Split(spec, ";") {
		var fs []feature
		for _, d := range strings.Fields(g) {
			var f feature
			parts := strings.Split(d, ".")
			for _, p := range parts {
				name, arg := p, 0
				if i := strings.IndexByte(p, '('); i >= 0 {
					name = p[:i]
					a := strings.TrimSuffix(p[i+1:], ")")
					a = strings.TrimSuffix(strings.TrimPrefix(a, `length="`), `"`)
					arg, _ = strconv.Atoi(a)
				}
				switch name {
				case "input", "stack", "child", "sibling":
					f.steps = append(f.steps, step{name, arg})
				case "token":
				default:
					f.fn, f.arg = name, arg
				}
			}
			fs = append(fs, f)
		}
		groups = append(groups, fs)
	}
	return groups
}

// The models' feature descriptors, from Parsey McParseface's context.pbtxt.
const parserFeatureSpec = `stack.child(1).label stack.child(1).sibling(-1).label stack.child(-1).label stack.child(-1).sibling(1).label
stack.child(2).label stack.child(-2).label stack(1).child(1).label stack(1).child(1).sibling(-1).label stack(1).child(-1).label
stack(1).child(-1).sibling(1).label stack(1).child(2).label stack(1).child(-2).label;
input.token.tag input(1).token.tag input(2).token.tag input(3).token.tag stack.token.tag stack.child(1).token.tag
stack.child(1).sibling(-1).token.tag stack.child(-1).token.tag stack.child(-1).sibling(1).token.tag stack.child(2).token.tag
stack.child(-2).token.tag stack(1).token.tag stack(1).child(1).token.tag stack(1).child(1).sibling(-1).token.tag
stack(1).child(-1).token.tag stack(1).child(-1).sibling(1).token.tag stack(1).child(2).token.tag stack(1).child(-2).token.tag
stack(2).token.tag stack(3).token.tag;
input.token.word input(1).token.word input(2).token.word input(3).token.word stack.token.word stack.child(1).token.word
stack.child(1).sibling(-1).token.word stack.child(-1).token.word stack.child(-1).sibling(1).token.word stack.child(2).token.word
stack.child(-2).token.word stack(1).token.word stack(1).child(1).token.word stack(1).child(1).sibling(-1).token.word
stack(1).child(-1).token.word stack(1).child(-1).sibling(1).token.word stack(1).child(2).token.word stack(1).child(-2).token.word
stack(2).token.word stack(3).token.word`

const taggerFeatureSpec = `input.digit input.hyphen;
input.prefix(length="2") input(1).prefix(length="2") input(2).prefix(length="2") input(3).prefix(length="2")
input(-1).prefix(length="2") input(-2).prefix(length="2") input(-3).prefix(length="2") input(-4).prefix(length="2");
input.prefix(length="3") input(1).prefix(length="3") input(2).prefix(length="3") input(3).prefix(length="3")
input(-1).prefix(length="3") input(-2).prefix(length="3") input(-3).prefix(length="3") input(-4).prefix(length="3");
input.suffix(length="2") input(1).suffix(length="2") input(2).suffix(length="2") input(3).suffix(length="2")
input(-1).suffix(length="2") input(-2).suffix(length="2") input(-3).suffix(length="2") input(-4).suffix(length="2");
input.suffix(length="3") input(1).suffix(length="3") input(2).suffix(length="3") input(3).suffix(length="3")
input(-1).suffix(length="3") input(-2).suffix(length="3") input(-3).suffix(length="3") input(-4).suffix(length="3");
input.token.word input(1).token.word input(2).token.word input(3).token.word
input(-1).token.word input(-2).token.word input(-3).token.word input(-4).token.word`

// Model is Parsey McParseface: its tagger and parser networks and maps.
type Model struct {
	words, tags, labels *termMap
	category            map[string]string
	prefixes, suffixes  map[string]int
	tagger, parser      *network
	taggerFeats         [][]feature
	parserFeats         [][]feature
}

// Files are the model's files, as the parsey_mcparseface directory names
// them.
var Files = []string{"parser-params", "tagger-params", "word-map", "tag-map", "label-map",
	"fine-to-universal.map", "prefix-table", "suffix-table"}

// Open loads the model from dir, which holds Files.
func Open(dir string) (*Model, error) {
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	m := &Model{category: map[string]string{}, taggerFeats: parseFeatures(taggerFeatureSpec),
		parserFeats: parseFeatures(parserFeatureSpec)}
	for name, dst := range map[string]**termMap{"word-map": &m.words, "tag-map": &m.tags, "label-map": &m.labels} {
		b, err := read(name)
		if err != nil {
			return nil, err
		}
		if *dst, err = readTermMap(b); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	b, err := read("fine-to-universal.map")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			m.category[f[0]] = f[1]
		}
	}
	for name, dst := range map[string]*map[string]int{"prefix-table": &m.prefixes, "suffix-table": &m.suffixes} {
		b, err := read(name)
		if err != nil {
			return nil, err
		}
		if *dst, err = readAffixTable(b); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if m.tagger, err = loadNetwork(filepath.Join(dir, "tagger-params"), 6, 1); err != nil {
		return nil, err
	}
	if m.parser, err = loadNetwork(filepath.Join(dir, "parser-params"), 3, 2); err != nil {
		return nil, err
	}
	if got, want := m.parser.softW.shape[1], 1+2*len(m.labels.terms); got != want {
		return nil, fmt.Errorf("parser-params: %d actions, want %d", got, want)
	}
	if got, want := m.tagger.softW.shape[1], len(m.tags.terms); got != want {
		return nil, fmt.Errorf("tagger-params: %d actions, want %d", got, want)
	}
	return m, nil
}

// locate is a feature's focus: its locators applied in turn.
func locate(s *state, f feature) int {
	focus := 0
	for i, st := range f.steps {
		if i > 0 && (focus < -1 || focus >= s.n) {
			return -2
		}
		switch st.op {
		case "input":
			focus = s.input(st.arg)
		case "stack":
			focus = s.stackAt(st.arg)
		case "child":
			if st.arg < 0 {
				focus = s.leftmostChild(focus, -st.arg)
			} else {
				focus = s.rightmostChild(focus, st.arg)
			}
		case "sibling":
			if st.arg < 0 {
				focus = s.leftSibling(focus, -st.arg)
			} else {
				focus = s.rightSibling(focus, st.arg)
			}
		}
	}
	return focus
}

// sentence is a sentence's tokens, as the features see them.
type sentence struct {
	words []string
	// precomputed per-token values
	word, digit, hyphen, pre2, pre3, suf2, suf3 []int
}

func (m *Model) prepare(words []string) *sentence {
	s := &sentence{words: words}
	for _, w := range words {
		s.word = append(s.word, m.words.lookup(w, len(m.words.terms)))
		s.digit = append(s.digit, digitValue(w))
		h := 0
		if strings.Contains(w, "-") {
			h = 1
		}
		s.hyphen = append(s.hyphen, h)
		s.pre2 = append(s.pre2, affix(m.prefixes, w, 2, true))
		s.pre3 = append(s.pre3, affix(m.prefixes, w, 3, true))
		s.suf2 = append(s.suf2, affix(m.suffixes, w, 2, false))
		s.suf3 = append(s.suf3, affix(m.suffixes, w, 3, false))
	}
	return s
}

// digitValue is Digit's value: NO_DIGIT 0, SOME_DIGIT 1, ALL_DIGIT 2,
// byte by byte as isdigit sees them.
func digitValue(w string) int {
	if w == "" {
		return 0
	}
	isd := func(c byte) bool { return c >= '0' && c <= '9' }
	has, all := isd(w[0]), isd(w[0])
	for i := 1; i < len(w); i++ {
		d := isd(w[i])
		all = all && d
		has = has || d
		if !all && has {
			return 1
		}
	}
	if !all {
		return 0
	}
	return 2
}

// affix is AffixTableFeature's value: the affix's ID, or the table's
// size for an unknown affix or a word shorter than n characters.
func affix(table map[string]int, w string, n int, prefix bool) int {
	if utf8.RuneCountInString(w) < n {
		return len(table)
	}
	rs := []rune(w)
	a := string(rs[:n])
	if !prefix {
		a = string(rs[len(rs)-n:])
	}
	if id, ok := table[a]; ok {
		return id
	}
	return len(table)
}

// value is a feature's value in s for sentence t.
func (m *Model) value(s *state, t *sentence, f feature) int {
	focus := locate(s, f)
	inside := focus >= 0 && focus < s.n
	// A sentence feature's value outside the sentence is its NumValues;
	// a parser feature's (label, digit, hyphen, prefix, suffix) at the
	// root is the value after its domain.
	tokenValue := func(vals []int, numValues int, root bool) int {
		switch {
		case inside:
			return vals[focus]
		case root && focus == -1:
			return numValues + 1
		}
		return numValues
	}
	switch f.fn {
	case "label":
		nv := len(m.labels.terms) + 1
		switch {
		case focus == -1:
			return nv + 1
		case !inside:
			return nv
		case s.label[focus] == -1:
			return nv + 1
		}
		return s.label[focus]
	case "tag":
		if !inside {
			return len(m.tags.terms) + 1
		}
		return s.tag[focus]
	case "word":
		return tokenValue(t.word, len(m.words.terms)+1, false)
	case "digit":
		return tokenValue(t.digit, 3, true)
	case "hyphen":
		return tokenValue(t.hyphen, 2, true)
	case "prefix":
		vals := t.pre2
		if f.arg == 3 {
			vals = t.pre3
		}
		return tokenValue(vals, len(m.prefixes)+1, true)
	case "suffix":
		vals := t.suf2
		if f.arg == 3 {
			vals = t.suf3
		}
		return tokenValue(vals, len(m.suffixes)+1, true)
	}
	return noFeature
}

func (m *Model) features(s *state, t *sentence, groups [][]feature) [][]int {
	out := make([][]int, len(groups))
	for g, fs := range groups {
		for _, f := range fs {
			out[g] = append(out[g], m.value(s, t, f))
		}
	}
	return out
}

// beamSize is parser_eval's default beam, which Parsey's demo runs both
// models with.
const beamSize = 8

// beam runs a transition system with beam search: each state's allowed
// actions scored by the network, path scores the sums of the raw scores
// (beam_reader_ops.cc), the best beamSize kept at each step.
func beam(start *state, final func(*state) bool, allowed func(*state, int) bool, perform func(*state, int),
	score func(*state) []float32) *state {
	items := []*state{start}
	for {
		done := true
		var next []*state
		for _, it := range items {
			if final(it) {
				next = append(next, it)
				continue
			}
			done = false
			sc := score(it)
			for a, v := range sc {
				if !allowed(it, a) {
					continue
				}
				c := it.clone()
				perform(c, a)
				c.score = it.score + float64(v)
				next = append(next, c)
			}
		}
		if done {
			break
		}
		sort.SliceStable(next, func(i, j int) bool { return next[i].score > next[j].score })
		if len(next) > beamSize {
			next = next[:beamSize]
		}
		items = next
	}
	return items[0]
}

// Token is a parsed token.
type Token struct {
	Word     string // the token as the models read it (-LRB-, ``, n't)
	Tag      string // its Penn Treebank tag
	Category string // its universal category (NOUN, VERB, ., ...)
	Head     int    // its head's index in the sentence, -1 for the root
	Label    string // its dependency label (nsubj, ..., ROOT)
}

// Parse tags and parses one tokenized sentence.
func (m *Model) Parse(words []string) []Token {
	n := len(words)
	if n == 0 {
		return nil
	}
	t := m.prepare(words)
	tagged := beam(newState(n),
		func(s *state) bool { return s.next == s.n },
		func(s *state, a int) bool { return s.next < s.n },
		func(s *state, a int) {
			s.tag[s.next] = a
			s.stack = append(s.stack, s.next)
			s.next++
		},
		func(s *state) []float32 { return m.tagger.scores(m.features(s, t, m.taggerFeats)) })
	st := newState(n)
	copy(st.tag, tagged.tag)
	st.stack = []int{-1}
	parsed := beam(st,
		func(s *state) bool { return s.next == s.n && len(s.stack) < 2 },
		func(s *state, a int) bool {
			switch {
			case a == 0:
				return s.next < s.n
			case (a-1)&1 == 0: // left arc
				return len(s.stack) > 2
			}
			return len(s.stack) > 1
		},
		func(s *state, a int) {
			if a == 0 {
				s.stack = append(s.stack, s.next)
				s.next++
				return
			}
			label := (a - 1) >> 1
			s0 := s.stack[len(s.stack)-1]
			s1 := s.stack[len(s.stack)-2]
			s.stack = s.stack[:len(s.stack)-2]
			if (a-1)&1 == 0 {
				s.head[s1], s.label[s1] = s0, label
				s.stack = append(s.stack, s0)
			} else {
				s.head[s0], s.label[s0] = s1, label
				s.stack = append(s.stack, s1)
			}
		},
		func(s *state) []float32 { return m.parser.scores(m.features(s, t, m.parserFeats)) })
	out := make([]Token, n)
	for i := range n {
		tag := m.tags.terms[tagged.tag[i]]
		label := "ROOT"
		if parsed.head[i] != -1 && parsed.label[i] >= 0 {
			label = m.labels.terms[parsed.label[i]]
		}
		out[i] = Token{Word: words[i], Tag: tag, Category: m.category[tag], Head: parsed.head[i], Label: label}
	}
	return out
}
