package speechconfig

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/speaker"
)

// TestSupportMatrix (#2127): cells of Google's supported languages tables,
// read 2026-11-21, supported and not.
func TestSupportMatrix(t *testing.T) {
	if len(v2Rows) != 1563 || len(v1Rows) != 867 {
		t.Errorf("rows: v2 %d, v1 %d; the tables had 1563 and 867", len(v2Rows), len(v1Rows))
	}
	cfg := func(model, features string, codes ...string) Config {
		c, code, err := Parse([]byte(`{"model":"` + model + `","languageCodes":["` + strings.Join(codes, `","`) +
			`"],"autoDecodingConfig":{},"features":{` + features + `}}`))
		if code != 0 {
			t.Fatalf("Parse %s %s: %v", model, features, err)
		}
		return c
	}
	for _, c := range []struct {
		model, loc, features string
		codes                []string
		ok                   bool
		msg                  string
	}{
		{"chirp", "us-central1", `"enableAutomaticPunctuation":true`, []string{"en-US"}, true, ""},
		{"chirp", "us-central1", `"profanityFilter":true`, []string{"en-US"}, false, "profanityFilter"},
		{"chirp", "europe-west4", `"enableWordConfidence":true`, []string{"en-US"}, false, "enableWordConfidence"},
		{"chirp", "asia-southeast1", `"enableSpokenPunctuation":true`, []string{"en-US"}, false, "enableSpokenPunctuation"},
		{"chirp", "us-central1", `"enableSpokenEmojis":true`, []string{"en-US"}, false, "enableSpokenEmojis"},
		{"chirp", "us", ``, []string{"en-US"}, false, `not supported in the location "us"`},
		{"chirp", "global", ``, []string{"en-US"}, false, `not supported in the location "global"`},
		{"chirp_2", "us-central1", `"profanityFilter":true,"enableWordConfidence":true`, []string{"en-US"}, true, ""},
		{"chirp_2", "us-central1", `"enableSpokenPunctuation":true`, []string{"en-US"}, false, "enableSpokenPunctuation"},
		{"chirp_3", "us", `"diarizationConfig":{}`, []string{"en-US"}, true, ""},
		{"chirp_3", "us", `"enableWordConfidence":true`, []string{"en-US"}, false, "enableWordConfidence"},
		{"chirp_3", "eu", `"profanityFilter":true`, []string{"auto"}, true, ""},
		{"chirp_telephony", "us-central1", `"enableAutomaticPunctuation":true`, []string{"en-US"}, false, "enableAutomaticPunctuation"},
		{"long", "global", `"enableSpokenPunctuation":true,"enableSpokenEmojis":true,"profanityFilter":true`, []string{"en-US"}, true, ""},
		{"long", "global", `"diarizationConfig":{}`, []string{"en-US"}, false, "diarizationConfig"},
		{"long", "us-central1", ``, []string{"en-US"}, false, "location"},
		{"short", "eu", ``, []string{"xx-XX"}, false, `The language "xx-XX"`},
		{"long", "global", `"enableSpokenEmojis":true`, []string{"en-US", "de-DE"}, false, `"de-DE"`},
		{"telephony_short", "asia-south1", `"enableSpokenEmojis":true`, []string{"en-US"}, true, ""},
	} {
		err := cfg(c.model, c.features, c.codes...).CheckV2(c.loc)
		if (err == nil) != c.ok || (err != nil && !strings.Contains(err.Error(), c.msg)) {
			t.Errorf("%s in %s %v {%s}: %v, want ok %v (%q)", c.model, c.loc, c.codes, c.features, err, c.ok, c.msg)
		}
	}
	for _, c := range []struct {
		model, lang string
		single      bool
		want        Feature
		ok          bool
	}{
		{"", "en-US", false, ProfanityFilter | WordConfidence, true},
		{"default", "en-US", false, SpokenPunctuationFeature, false},
		{"latest_long", "en-US", true, SpokenPunctuationFeature | SpokenEmojiFeature, true},
		{"latest_short", "en-US", false, SpokenPunctuationFeature, false},
		{"default", "en-US", true, 0, false},
		{"command_and_search", "af-ZA", false, ModelAdaptation | ProfanityFilter, true},
		{"command_and_search", "af-ZA", false, AutomaticPunctuation, false},
		{"nope", "en-US", false, 0, false},
	} {
		if err := CheckV1(c.model, c.lang, c.single, c.want); (err == nil) != c.ok {
			t.Errorf("v1 %q %s single %v features %b: %v, want ok %v", c.model, c.lang, c.single, c.want, err, c.ok)
		}
	}
}

// TestUnserved: diarization, which the table allows on chirp_3, is
// UNIMPLEMENTED in the default build and served by internal/speaker in the
// onnx build (#2131), so the test holds under both tags.
func TestUnserved(t *testing.T) {
	c, _, _ := Parse([]byte(`{"model":"chirp_3","languageCodes":["en-US"],"autoDecodingConfig":{},"features":{"diarizationConfig":{}}}`))
	if err := c.CheckV2("us"); err != nil {
		t.Fatalf("chirp_3 diarization: CheckV2: %v", err)
	}
	err := c.Unserved()
	if speaker.Available != (err == nil) {
		t.Errorf("chirp_3 diarization with speaker.Available %v: Unserved %v", speaker.Available, err)
	}
	if err != nil && !strings.Contains(err.Error(), "diarizationConfig") {
		t.Errorf("Unserved %q does not name diarizationConfig", err)
	}
}

func TestStripPunctuation(t *testing.T) {
	for in, want := range map[string]string{
		"Hello, world. Isn't it 3.5 or 1,000? Well-known!": "Hello world Isn't it 3.5 or 1,000 Well-known",
		" And so, my fellow Americans... ":                 "And so my fellow Americans",
	} {
		if got := StripPunctuation(in); got != want {
			t.Errorf("StripPunctuation(%q) = %q, want %q", in, got, want)
		}
	}
	if (Config{Model: "long"}).Format("Hi, there.", "en-us") != "Hi there" {
		t.Errorf("long without enableAutomaticPunctuation kept punctuation")
	}
	if got := (Config{Model: "long", Options: Options{AutomaticPunctuation: true}}).Format("Hi, there.", "en-us"); got != "Hi, there." {
		t.Errorf("long with enableAutomaticPunctuation: %q", got)
	}
	if got := (Config{Model: "chirp"}).Format("Hi, there.", "en-us"); got != "Hi, there." {
		t.Errorf("chirp: %q", got)
	}
}
