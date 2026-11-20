package speechconfig

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed" // the spoken punctuation and emoji inventories
	"io"
	"strings"
	"sync"
	"unicode"
)

// Spoken punctuation and spoken emojis (features.enableSpokenPunctuation
// and enableSpokenEmojis, #1799): a spoken form in the transcript ("question
// mark", "smiling face with heart-eyes") is replaced by its symbol. The
// forms are Google's own inventories, by locale: the tables of "Supported
// spoken punctuation" and "Supported spoken emoji"
// (https://docs.cloud.google.com/speech-to-text/docs/spoken-punctuation and
// .../spoken-emoji, whose data the pages load from
// /speech-to-text/docs/common/spoken_punctuation_inventories.json and
// spoken_emoji_inventories.json, read 2026-11-20; Google's documentation,
// CC BY 4.0), stored here as locale, symbol and |-separated forms a line
// (data/, the emoji table gzipped). A language with no table of its own
// uses its first locale's (en as en_us when it is there); a language with
// none (spoken emoji are listed for English and French only) is left as it
// was recognised.
//
// UNVERIFIED (re-read 2026-11-20: the pages list the forms, not how the
// result is spaced): a
// closing mark (. , ? ! : ; ) ] } and the like) is written against the
// word before it, an opening one ( [ { against the word after it, new line
// and new paragraph as "\n" and "\n\n", and an emoji as a word of its own;
// a form listed for two symbols (en_us "paren", "quote") opens first and
// closes the next time; the recogniser's own punctuation around a spoken
// form is dropped.

//go:embed data/spoken_punctuation.tsv
var spokenPunctuationTSV []byte

//go:embed data/spoken_emoji.tsv.gz
var spokenEmojiTSVGz []byte

// spokenTable is one locale's forms: each form (normalised words) to its
// symbols, and the longest form's word count.
type spokenTable struct {
	forms   map[string][]string
	longest int
}

var (
	spokenOnce               sync.Once
	spokenPunct, spokenEmoji map[string]*spokenTable
)

func loadSpoken() {
	spokenPunct = parseSpoken(bytes.NewReader(spokenPunctuationTSV))
	zr, err := gzip.NewReader(bytes.NewReader(spokenEmojiTSVGz))
	if err != nil {
		spokenEmoji = map[string]*spokenTable{}
		return
	}
	spokenEmoji = parseSpoken(zr)
}

func parseSpoken(r io.Reader) map[string]*spokenTable {
	out := map[string]*spokenTable{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		t := out[parts[0]]
		if t == nil {
			t = &spokenTable{forms: map[string][]string{}}
			out[parts[0]] = t
		}
		for _, f := range strings.Split(parts[2], "|") {
			words := spokenWords(f)
			if len(words) == 0 {
				continue
			}
			key := strings.Join(words, " ")
			t.forms[key] = append(t.forms[key], parts[1])
			t.longest = max(t.longest, len(words))
		}
	}
	return out
}

// spokenWords are text's words in lower case, punctuation trimmed, empty
// words dropped.
func spokenWords(text string) []string {
	var out []string
	for _, w := range strings.Fields(text) {
		if w = trimMarks(strings.ToLower(w)); w != "" {
			out = append(out, w)
		}
	}
	return out
}

func trimMarks(w string) string {
	return strings.TrimFunc(w, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) })
}

// spokenLocale is the inventory locale for a language code ("en-US" as
// en_us, "en" as en_us, "fr" as fr_fr), "" when the table has none.
func spokenLocale(tables map[string]*spokenTable, code string) string {
	code = strings.ReplaceAll(strings.ToLower(code), "-", "_")
	if _, ok := tables[code]; ok {
		return code
	}
	primary, _, _ := strings.Cut(code, "_")
	if _, ok := tables[primary+"_"+primary]; ok {
		return primary + "_" + primary
	}
	if primary == "en" {
		if _, ok := tables["en_us"]; ok {
			return "en_us"
		}
	}
	best := ""
	for loc := range tables {
		if strings.HasPrefix(loc, primary+"_") && (best == "" || loc < best) {
			best = loc
		}
	}
	return best
}

// spokenKind is how a symbol is spaced.
func spokenKind(sym string, emoji bool) int {
	const (
		word = iota
		closing
		opening
		newline
	)
	switch {
	case emoji:
		return word
	case strings.HasPrefix(sym, "⏎"):
		return newline
	case sym == "(" || sym == "[" || sym == "{" || sym == "¿" || sym == "¡" || sym == "«" || sym == "„" || sym == "“" || sym == "‘":
		return opening
	case sym == "#" || sym == "@" || sym == "&" || sym == "*" || sym == "/" || sym == "\\" || sym == "-" || sym == "_" ||
		sym == "–" || sym == "—" || sym == "+" || sym == "=" || sym == "%" || sym == "$" || sym == "€" || sym == "£":
		return word
	}
	return closing
}

// spokenToken is a piece of the rewritten transcript.
type spokenToken struct {
	text string
	kind int // spokenKind's, word for a recognised word
}

// replaceSpoken replaces the spoken forms in text from tables' locale.
func replaceSpoken(text string, tables map[string]*spokenTable, code string, emoji bool) string {
	loc := spokenLocale(tables, code)
	if loc == "" {
		return text
	}
	t := tables[loc]
	if strings.Contains(text, "\n") {
		// A new line an earlier pass wrote is kept.
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			lines[i] = replaceLine(l, t, emoji)
		}
		return strings.Join(lines, "\n")
	}
	return replaceLine(text, t, emoji)
}

// replaceLine is replaceSpoken for one line of text.
func replaceLine(text string, t *spokenTable, emoji bool) string {
	words := strings.Fields(text)
	norm := make([]string, len(words))
	for i, w := range words {
		norm[i] = trimMarks(strings.ToLower(w))
	}
	var toks []spokenToken
	open := map[string]bool{} // the forms of two symbols that opened
	for i := 0; i < len(words); {
		matched := false
		for n := min(t.longest, len(words)-i); n >= 1; n-- {
			parts := norm[i : i+n]
			if parts[0] == "" || parts[n-1] == "" {
				continue
			}
			key := strings.Join(parts, " ")
			syms := t.forms[key]
			if len(syms) == 0 {
				continue
			}
			sym := syms[0]
			if len(syms) > 1 {
				if open[key] {
					sym = syms[1]
				}
				open[key] = !open[key]
			}
			kind := spokenKind(sym, emoji)
			// The spoken mark replaces the recogniser's own punctuation
			// after the word before it ("Hello, comma" is "Hello,").
			if kind == 1 && len(toks) > 0 && toks[len(toks)-1].kind == 0 {
				prev := &toks[len(toks)-1]
				if w := strings.TrimRightFunc(prev.text, unicode.IsPunct); w != "" {
					prev.text = w
				}
			}
			if kind == 3 {
				sym = strings.ReplaceAll(sym, "⏎", "\n")
			}
			toks = append(toks, spokenToken{text: sym, kind: kind})
			i += n
			matched = true
			break
		}
		if !matched {
			toks = append(toks, spokenToken{text: words[i]})
			i++
		}
	}
	var b strings.Builder
	prev := -1
	for _, tk := range toks {
		switch {
		case b.Len() == 0:
		case tk.kind == 1 || tk.kind == 3 || prev == 2 || prev == 3:
		default:
			b.WriteByte(' ')
		}
		b.WriteString(tk.text)
		prev = tk.kind
	}
	return b.String()
}

// SpokenPunctuation replaces the spoken punctuation in text, for the
// language code (Google's inventories, spoken.go).
func SpokenPunctuation(text, code string) string {
	spokenOnce.Do(loadSpoken)
	return replaceSpoken(text, spokenPunct, code, false)
}

// SpokenEmojis replaces the spoken emojis in text.
func SpokenEmojis(text, code string) string {
	spokenOnce.Do(loadSpoken)
	return replaceSpoken(text, spokenEmoji, code, true)
}
