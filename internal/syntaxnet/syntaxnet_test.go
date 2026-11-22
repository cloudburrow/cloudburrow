package syntaxnet

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func words(spans []Span) string {
	var w []string
	for _, s := range spans {
		w = append(w, s.Word)
	}
	return strings.Join(w, " ")
}

func TestTokenize(t *testing.T) {
	for in, want := range map[string]string{
		"Bob brought the pizza to Alice.":         "Bob brought the pizza to Alice .",
		`He said "I can't go (today)," and left.`: "He said `` I ca n't go -LRB- today -RRB- , '' and left .",
		"Mr. Smith's dogs aren't here!":           "Mr. Smith 's dogs are n't here !",
		"I cannot wait... really":                 "I can not wait ... really",
		"We don’t know.":                          "We do n't know .",
		"Costs rose 5% to $1,000 -- a record.":    "Costs rose 5 % to $ 1 , 000 -- a record .",
		"The players' coach left.":                "The players ' coach left .",
	} {
		spans := Tokenize(in)
		if got := words(spans); got != want {
			t.Errorf("Tokenize(%q) = %q, want %q", in, got, want)
		}
		for _, s := range spans {
			if s.Start < 0 || s.End > len(in) || s.Start >= s.End {
				t.Fatalf("Tokenize(%q): bad span %+v", in, s)
			}
		}
	}
	// Offsets name the original bytes, typographic apostrophes included.
	in := "We don’t know."
	var got []string
	for _, s := range Tokenize(in) {
		got = append(got, in[s.Start:s.End])
	}
	if strings.Join(got, "|") != "We|do|n’t|know|." {
		t.Errorf("contents = %q", got)
	}
}

func TestParseyParse(t *testing.T) {
	dir := os.Getenv("PARSEY_DIR")
	if dir == "" {
		t.Skip("PARSEY_DIR (a directory of Parsey McParseface's files) is not set")
	}
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// SyntaxNet's README parses this sentence so.
	toks := m.Parse(strings.Fields("Bob brought the pizza to Alice ."))
	var got []string
	for _, tk := range toks {
		got = append(got, tk.Word+"/"+tk.Tag+"/"+tk.Label+"/"+strconv.Itoa(tk.Head))
	}
	want := "Bob/NNP/nsubj/1 brought/VBD/ROOT/-1 the/DT/det/3 pizza/NN/dobj/1 to/IN/prep/1 Alice/NNP/pobj/4 ././punct/1"
	if strings.Join(got, " ") != want {
		t.Errorf("Parse = %s\nwant    %s", strings.Join(got, " "), want)
	}
}
