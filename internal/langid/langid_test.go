package langid

import (
	"testing"
)

// TestDetect: the detector's most probable language for sentences in a few
// scripts.
func TestDetect(t *testing.T) {
	for text, want := range map[string]string{
		"hello world, how are you today?":             "en",
		"Bonjour tout le monde, comment allez-vous ?": "fr",
		"Guten Morgen, wie geht es dir?":              "de",
		"¿Dónde está la biblioteca?":                  "es",
		"Привет, как дела?":                           "ru",
		"こんにちは世界":                                     "ja",
		"你好，世界。今天天气很好。":                               "zh",
	} {
		ps, err := Detect(text)
		if err != nil || len(ps) == 0 || ps[0].Code != want {
			t.Errorf("%s: %v %v", text, ps, err)
			continue
		}
		sum := 0.0
		for _, p := range ps {
			sum += p.Probability
		}
		if ps[0].Probability < 0.5 || sum > 1.01 {
			t.Errorf("%s: %v (sum %v)", text, ps[0], sum)
		}
	}
	if ps, err := Detect(""); err != nil || ps != nil {
		t.Errorf("empty: %v %v", ps, err)
	}
}

// TestTokenize: ngram_hash_ops_utils.cc's tokens: "^", each rune, a space
// for each non-letter, "$", within max_splits.
func TestTokenize(t *testing.T) {
	s, toks := tokenize("ab1é", 128)
	if s != "^ab é$" || len(toks) != 6 || toks[4] != [2]int{4, 2} {
		t.Errorf("%q %v", s, toks)
	}
	if s, toks := tokenize("abcdef", 4); s != "^ab$" || len(toks) != 4 {
		t.Errorf("max_splits: %q %v", s, toks)
	}
}

// TestMurmur64: MurmurHash64WithSeed's values, worked by hand from
// murmur.cc for the empty string and checked for the tail and block paths.
func TestMurmur64(t *testing.T) {
	if got := murmur64(nil, 0); got != 0 {
		t.Errorf("empty, seed 0: %#x", got)
	}
	a, b := murmur64([]byte("abcdefgh"), 1), murmur64([]byte("abcdefghi"), 1)
	if a == b || a == murmur64([]byte("abcdefgh"), 2) {
		t.Error("seed or length ignored")
	}
}

// TestFlexMap: the custom_options of the model's NGramHash operator.
func TestFlexMap(t *testing.T) {
	once.Do(load)
	if errMod != nil {
		t.Fatal(errMod)
	}
	var found bool
	for _, n := range model.Uses() {
		found = found || n == "NGramHash"
	}
	if !found {
		t.Fatalf("uses %v", model.Uses())
	}
	if len(labels) != 111 || labels[0] != "unknown" {
		t.Errorf("labels: %d %v", len(labels), labels[:3])
	}
}
