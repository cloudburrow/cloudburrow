package syntaxnet

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Span is a token of a sentence: its bytes [Start, End) and the form the
// models read.
type Span struct {
	Start, End int
	Word       string
}

// Tokenize splits one sentence as SyntaxNet's english-text format does
// (text_formats.cc's EnglishTextFormat, Penn Treebank rules), keeping
// each token's offset: , ; : @ # $ % & ? ! -- ... and brackets are tokens
// of their own; a sentence-final period is split; double quotes are “
// when they open and ” otherwise; 's 'm 'd 'll 're 've n't and a
// closing single quote are split off; cannot, gonna, gotta, wanna, gimme
// and lemme are split in two; brackets read as -LRB- -RRB- -LSB- -LCB-
// -RCB-. The preprocessing's typographic quotes count as straight ones
// (the single ones as apostrophes, so don’t splits as don't does) and
// its discarded bullets are dropped.
func Tokenize(s string) []Span {
	type chunk struct{ a, b int }
	var chunks []chunk
	start := -1
	for i, c := range s {
		if unicode.IsSpace(c) {
			if start >= 0 {
				chunks = append(chunks, chunk{start, i})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		chunks = append(chunks, chunk{start, len(s)})
	}
	var out []Span
	for ci, ch := range chunks {
		out = append(out, tokenizeChunk(s, ch.a, ch.b, ci == len(chunks)-1)...)
	}
	return out
}

// normal is a character as the rules see it: typographic quotes and
// brackets as ASCII, discarded bullets as 0.
func normal(c rune) rune {
	switch c {
	case '″', '“', '„', '”', '«', '»', '‹', '›':
		return '"'
	case '’', '‘', '′':
		return '\''
	case '【', '（':
		return '('
	case '】', '）':
		return ')'
	case '|', '·', '•', '●', '▪', '■', '□', '❑', '◆', '★', '＊', '♦':
		return 0
	}
	return c
}

func tokenizeChunk(s string, a, b int, last bool) []Span {
	var out []Span
	wordStart := -1
	flush := func(end int) {
		if wordStart >= 0 && end > wordStart {
			out = append(out, Span{wordStart, end, ""})
		}
		wordStart = -1
	}
	for i := a; i < b; {
		c, n := utf8.DecodeRuneInString(s[i:])
		nc := normal(c)
		switch {
		case nc == 0:
			flush(i)
		case strings.HasPrefix(s[i:b], "..."):
			flush(i)
			out = append(out, Span{i, i + 3, "..."})
			i += 3
			continue
		case strings.HasPrefix(s[i:b], "--"):
			flush(i)
			out = append(out, Span{i, i + 2, "--"})
			i += 2
			continue
		case strings.ContainsRune(",;:@#$%&?!()[]{}<>", nc):
			flush(i)
			out = append(out, Span{i, i + n, string(nc)})
		case nc == '"':
			flush(i)
			out = append(out, Span{i, i + n, "\""})
		default:
			if wordStart < 0 {
				wordStart = i
			}
		}
		i += n
	}
	flush(b)
	// The sentence-final period: the last word's, when only closing
	// brackets and quotes follow it.
	if last {
		j := len(out) - 1
		for j >= 0 && out[j].Word != "" && strings.Contains(")]}>\"", out[j].Word) {
			j--
		}
		if j >= 0 && out[j].Word == "" {
			w := s[out[j].Start:out[j].End]
			if len(w) >= 2 && w[len(w)-1] == '.' && w[len(w)-2] != '.' {
				p := Span{out[j].End - 1, out[j].End, "."}
				out[j].End--
				out = append(out[:j+1], append([]Span{p}, out[j+1:]...)...)
			}
		}
	}
	var res []Span
	for k, sp := range out {
		if sp.Word != "" {
			res = append(res, sp)
			continue
		}
		res = append(res, splitWord(s, sp.Start, sp.End)...)
		_ = k
	}
	// Forms: quotes by position, brackets as the parsed-file symbols.
	for k := range res {
		switch res[k].Word {
		case "\"":
			open := k == 0 || res[k-1].End < res[k].Start || strings.Contains("([{<", res[k-1].Word) ||
				res[k-1].Word == "-LRB-" || res[k-1].Word == "-LCB-"
			if open {
				res[k].Word = "``"
			} else {
				res[k].Word = "''"
			}
		case "(":
			res[k].Word = "-LRB-"
		case ")":
			res[k].Word = "-RRB-"
		case "]":
			res[k].Word = "-LSB-"
		case "{":
			res[k].Word = "-LCB-"
		case "}":
			res[k].Word = "-RCB-"
		}
	}
	return res
}

// contractions are the suffixes split off a word.
var contractions = []string{"n't", "N'T", "'ll", "'re", "'ve", "'LL", "'RE", "'VE", "'s", "'S", "'m", "'M", "'d", "'D"}

// splits are the words split in two, at the byte given.
var splits = map[string]int{"cannot": 3, "gonna": 3, "gotta": 3, "wanna": 3, "gimme": 3, "lemme": 3}

// splitWord splits a word's contraction or closing quote off; the
// apostrophe may be typographic.
func splitWord(s string, a, b int) []Span {
	w := s[a:b]
	norm := strings.NewReplacer("’", "'", "‘", "'", "′", "'").Replace(w)
	if norm != w {
		// Map back by rune: a typographic apostrophe is 3 bytes.
		return splitNormalized(s, a, b)
	}
	if k, ok := splits[strings.ToLower(w)]; ok && (w[0] == strings.ToLower(w)[0] || unicode.IsUpper(rune(w[0]))) &&
		w[1:] == strings.ToLower(w)[1:] {
		return []Span{{a, a + k, w[:k]}, {a + k, b, w[k:]}}
	}
	for _, c := range contractions {
		if len(w) > len(c) && strings.HasSuffix(w, c) {
			return []Span{{a, b - len(c), w[:len(w)-len(c)]}, {b - len(c), b, c}}
		}
	}
	if len(w) > 1 && w[len(w)-1] == '\'' && w[len(w)-2] != '\'' {
		return []Span{{a, b - 1, w[:len(w)-1]}, {b - 1, b, "'"}}
	}
	return []Span{{a, b, w}}
}

// splitNormalized splits a word whose apostrophes are typographic: as
// its normalized form splits, with the offsets of the original bytes.
func splitNormalized(s string, a, b int) []Span {
	var offs []int // original byte offset of each normalized byte
	var nb strings.Builder
	for i := a; i < b; {
		c, n := utf8.DecodeRuneInString(s[i:])
		if c == '’' || c == '‘' || c == '′' {
			c = '\''
		}
		for range utf8.RuneLen(c) {
			offs = append(offs, i)
		}
		nb.WriteRune(c)
		i += n
	}
	offs = append(offs, b)
	norm := nb.String()
	parts := splitWord(norm, 0, len(norm))
	out := make([]Span, len(parts))
	for i, p := range parts {
		out[i] = Span{offs[p.Start], offs[p.End], p.Word}
	}
	return out
}
