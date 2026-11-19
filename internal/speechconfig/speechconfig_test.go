package speechconfig

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	c, code, err := Parse([]byte(`{"language_codes": ["en-US"], "model": "chirp", "auto_decoding_config": {},
		"features": {"enable_word_time_offsets": true}}`))
	if code != 0 || err != nil {
		t.Fatalf("snake_case config: %d %v", code, err)
	}
	if !c.Decoding.Auto || c.Language != "en" || c.LanguageCode != "en-us" || !c.Words || !c.WordOffsets || c.WordConfidence || c.Model != "chirp" {
		t.Errorf("config %+v", c)
	}
	c, code, _ = Parse([]byte(`{"languageCodes": ["cmn-Hans-CN"], "model": "chirp",
		"explicitDecodingConfig": {"encoding": "MULAW", "sampleRateHertz": 8000, "audioChannelCount": 1}}`))
	if code != 0 || c.Language != "zh" || c.Decoding.Auto || c.Decoding.Encoding != "MULAW" || c.Decoding.SampleRate != 8000 {
		t.Errorf("camelCase config: %d %+v", code, c)
	}
	if c, code, _ := Parse([]byte(`{"languageCodes": ["auto"], "model": "chirp", "autoDecodingConfig": {}}`)); code != 0 || c.Language != "" || c.LanguageCode != "" {
		t.Errorf("auto: %d %+v", code, c)
	}
	for _, tc := range []struct {
		cfg  string
		code int
		want string
	}{
		{`{"languageCodes": ["en-US"], "autoDecodingConfig": {}}`, 400, "model is required"},
		{`{"model": "chirp", "autoDecodingConfig": {}}`, 400, "languageCodes is required"},
		{`{"model": "chirp", "languageCodes": ["en-US"]}`, 400, "autoDecodingConfig or explicitDecodingConfig"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "explicitDecodingConfig": {"encoding": "LINEAR16"}}`, 400, "one of"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "bogus": 1}`, 400, "unknown field"},
		{`{"model": "long", "languageCodes": ["en-US"], "autoDecodingConfig": {}}`, 501, "only chirp"},
		{`{"model": "chirp", "languageCodes": ["en-US", "de-DE"], "autoDecodingConfig": {}}`, 501, "several"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "features": {"profanityFilter": true}}`, 501, "profanityFilter"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "features": {"diarizationConfig": {}}}`, 501, "diarizationConfig"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "translationConfig": {"targetLanguage": "de"}}`, 501, "translationConfig"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "explicitDecodingConfig": {"encoding": "FLAC"}}`, 501, "FLAC"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "explicitDecodingConfig": {"encoding": "WAV"}}`, 400, "invalid explicitDecodingConfig.encoding"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "features": {"maxAlternatives": 31}}`, 400, "maxAlternatives"},
		{`[1]`, 400, "JSON object"},
	} {
		_, code, err := Parse([]byte(tc.cfg))
		if code != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %d %v, want %d %q", tc.cfg, code, err, tc.code, tc.want)
		}
	}
}
