package vertexai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/speech"
)

func recognizeCall(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(NewServer(nil).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const recognizePath = "/v2/projects/p/locations/us/recognizers/_:recognize"

// TestRecognizeRefusals: what the route refuses before transcribing.
func TestRecognizeRefusals(t *testing.T) {
	speech.SetModelSource(nil)
	wav := base64.StdEncoding.EncodeToString([]byte("RIFF\x24\x00\x00\x00WAVEfmt "))
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/v2/projects/p/locations/us/recognizers/mine:recognize", `{}`, 404},
		{recognizePath, `{"content": "` + wav + `"}`, 400},
		{recognizePath, `{"config": {"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}}, "uri": "gs://b/o"}`, 400}, // no Cloud Storage
		{recognizePath, `{"config": {"model": "medical_conversation", "languageCodes": ["en-US"], "autoDecodingConfig": {}}, "content": ""}`, 501},
		// Ogg Opus is decoded since #1799: a stream that ends early is bad
		// data; AMR has no local decoder.
		{recognizePath, `{"config": {"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}}, "content": "` +
			base64.StdEncoding.EncodeToString([]byte("OggS\x00\x02")) + `"}`, 400},
		{recognizePath, `{"config": {"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}}, "content": "` +
			base64.StdEncoding.EncodeToString([]byte("#!AMR\n\x3c")) + `"}`, 501},
		// FLAC is decoded since #1799: a stream that ends early is bad data.
		{recognizePath, `{"config": {"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}}, "content": "` +
			base64.StdEncoding.EncodeToString([]byte("fLaC\x00\x00")) + `"}`, 400},
		{recognizePath, `{"config": {"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}}, "content": "` +
			base64.StdEncoding.EncodeToString([]byte("not audio")) + `"}`, 400},
		// A valid request with no model installed.
		{recognizePath, `{"config": {"model": "chirp", "languageCodes": ["en-US"], "explicitDecodingConfig": {"encoding": "LINEAR16", ` +
			`"sampleRateHertz": 16000, "audioChannelCount": 1}}, "content": "AAAAAA=="}`, 501},
	} {
		if code, out := recognizeCall(t, tc.path, tc.body); code != tc.code {
			t.Errorf("%s %s: %d %v, want %d", tc.path, tc.body, code, out, tc.code)
		}
	}
}

// TestRecognize transcribes whisper.cpp's sample through the route, with
// word offsets and confidences.
func TestRecognize(t *testing.T) {
	if testing.Short() {
		t.Skip("fetches the whisper model and transcribes")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	cache := localai.Cache{Dir: filepath.Join(dir, "cloudburrow-test", "models")}
	speech.SetModelSource(func(ctx context.Context) (string, error) {
		p, err := cache.FetchPinnedFile(ctx, localai.Whisper, localai.Whisper.Pin.Files[0])
		if err != nil {
			t.Skipf("fetching the whisper model: %v", err)
		}
		return p, nil
	})
	t.Cleanup(func() { speech.SetModelSource(nil) })
	wav, err := os.ReadFile("../../speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	code, out := recognizeCall(t, recognizePath, `{"config": {"model": "chirp", "language_codes": ["en-US"], "auto_decoding_config": {}, `+
		`"features": {"enable_word_time_offsets": true, "enable_word_confidence": true}}, "content": "`+base64.StdEncoding.EncodeToString(wav)+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	b, _ := json.Marshal(out)
	s := string(b)
	for _, want := range []string{`"totalBilledDuration":"11s"`, `"languageCode":"en-us"`, `ask not what your country can do for you`,
		`"resultEndOffset":"11s"`, `"word":"And"`, `"startOffset":"0.32s"`} {
		if !strings.Contains(s, want) {
			t.Errorf("response lacks %s: %s", want, s)
		}
	}
}
