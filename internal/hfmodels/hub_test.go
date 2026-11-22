package hfmodels

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata/hub are huggingface.co's own answers, recorded
// 2026-11-22 (dataset and arXiv tags trimmed): a search for MiniLM sorted
// by downloads with its Link header, sentence-transformers'
// paraphrase-MiniLM-L3-v2 and google/gemma-3-4b-it (gated) with
// ?blobs=true, and the MiniLM model's README.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hub", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const fakeToken = "hf_fakeTokenForTestsOnly0123456789"

// hubStub serves the fixtures and records what it was asked.
func hubStub(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		link := strings.TrimSpace(string(fixture(t, "search.link")))
		w.Header().Set("Link", strings.ReplaceAll(link, "https://huggingface.co", "http://"+r.Host))
		_, _ = w.Write(fixture(t, "search.json"))
	})
	mux.HandleFunc("GET /api/models/sentence-transformers/paraphrase-MiniLM-L3-v2", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		_, _ = w.Write(fixture(t, "model-minilm.json"))
	})
	mux.HandleFunc("GET /api/models/google/gemma-3-4b-it", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		_, _ = w.Write(fixture(t, "model-gated.json"))
	})
	mux.HandleFunc("GET /sentence-transformers/paraphrase-MiniLM-L3-v2/resolve/{rev}/README.md", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		readme := string(fixture(t, "readme-minilm.md")) + "\n<script>alert(1)</script>\n<div align=\"center\"><b>kept</b></div>\n"
		_, _ = w.Write([]byte(readme))
	})
	// A gated file opens only with the fake token.
	mux.HandleFunc("HEAD /google/gemma-3-4b-it/resolve/{rev}/config.json", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", "https://cdn.example/x")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestSearchReadsTheHubAnonymously(t *testing.T) {
	srv, seen := hubStub(t)
	m := &Manager{Base: srv.URL, Token: func() string { return fakeToken }}
	page, err := m.Search(context.Background(), SearchQuery{Search: "MiniLM", Sort: "downloads", Task: "sentence-similarity",
		Library: "sentence-transformers", License: "apache-2.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Models) != 3 || page.Models[0].ID != "sentence-transformers/all-MiniLM-L6-v2" {
		t.Fatalf("models = %+v", page.Models)
	}
	first := page.Models[0]
	if first.License != "apache-2.0" || first.Task != "sentence-similarity" || first.Library != "sentence-transformers" ||
		first.Downloads == 0 || first.Likes == 0 || first.Author != "sentence-transformers" || first.Gated != "" {
		t.Errorf("first = %+v", first)
	}
	if page.Next == "" || strings.Contains(page.Next, "&") {
		t.Errorf("next cursor = %q", page.Next)
	}
	q := (*seen)[0].URL.Query()
	for k, want := range map[string]string{"search": "MiniLM", "sort": "downloads", "direction": "-1", "pipeline_tag": "sentence-similarity",
		"library": "sentence-transformers", "filter": "license:apache-2.0", "limit": "30"} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(strings.Join(q["expand[]"], ","), "gated") {
		t.Errorf("the search does not ask for gated: %v", q["expand[]"])
	}
	if a := (*seen)[0].Header.Get("Authorization"); a != "" {
		t.Errorf("browsing sent a token: %q", a)
	}
	// The next page passes the cursor back.
	if _, err := m.Search(context.Background(), SearchQuery{Cursor: page.Next}); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[1].URL.Query().Get("cursor"); got != page.Next {
		t.Errorf("cursor sent = %q, want %q", got, page.Next)
	}
}

func TestSearchRefusesWhatTheHubWould(t *testing.T) {
	m := &Manager{Base: "http://127.0.0.1:1"}
	for _, q := range []SearchQuery{{Sort: "random"}, {Task: "a b"}, {License: "x&y"}, {Cursor: "a b"}, {Limit: 1000},
		{Search: strings.Repeat("x", 201)}} {
		if _, err := m.Search(context.Background(), q); !errors.Is(err, ErrBadID) {
			t.Errorf("Search(%+v) = %v, want ErrBadID", q, err)
		}
	}
}

func TestCardReadsFactsSizesAndASanitisedREADME(t *testing.T) {
	srv, seen := hubStub(t)
	m := &Manager{Base: srv.URL, Token: func() string { return fakeToken }}
	c, err := m.Card(context.Background(), "sentence-transformers/paraphrase-MiniLM-L3-v2")
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision != "4ca70771034acceecb2e72475f72050fcdde4ddc" || c.License != "apache-2.0" || c.Task != "sentence-similarity" ||
		c.Library != "sentence-transformers" || c.Gated != "" {
		t.Errorf("card = %+v", c.HubModel)
	}
	// The weights are model.safetensors alone: not the ONNX, OpenVINO,
	// PyTorch and TensorFlow copies beside it.
	if c.WeightsBytes != 69569488 || c.Bytes <= c.WeightsBytes {
		t.Errorf("weights %d of %d bytes", c.WeightsBytes, c.Bytes)
	}
	if strings.HasPrefix(c.README, "---") || !strings.HasPrefix(c.README, "# sentence-transformers/paraphrase-MiniLM-L3-v2") {
		t.Errorf("README starts %q", c.README[:min(60, len(c.README))])
	}
	if strings.Contains(c.README, "<script") || strings.Contains(c.README, "alert(1)") || strings.Contains(c.README, "<div") ||
		!strings.Contains(c.README, "kept") {
		t.Errorf("README is not sanitised: %q", c.README[len(c.README)-80:])
	}
	for _, r := range *seen {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("%s sent a token", r.URL.Path)
		}
	}
}

func TestCardMarksAGatedModel(t *testing.T) {
	srv, _ := hubStub(t)
	m := &Manager{Base: srv.URL}
	c, err := m.Card(context.Background(), "google/gemma-3-4b-it")
	if err != nil {
		t.Fatal(err)
	}
	if c.Gated != "manual" || c.License != "gemma" || c.Task != "image-text-to-text" || c.GatedPrompt == "" {
		t.Errorf("card = %+v", c.HubModel)
	}
}

// CheckAccess sends the token to the Hub only, and its refusal is Google's
// words, with no token in them.
func TestCheckAccessWithAFakeTokenAgainstAStub(t *testing.T) {
	srv, seen := hubStub(t)
	m := &Manager{Base: srv.URL}
	const rev = "093f9f388b31de276ce2de164bdc2081324b9767"
	if err := m.CheckAccess(context.Background(), "google/gemma-3-4b-it", rev, "config.json", fakeToken); err != nil {
		t.Fatalf("the token that opens the model: %v", err)
	}
	err := m.CheckAccess(context.Background(), "google/gemma-3-4b-it", rev, "config.json", "hf_wrongToken000")
	if !errors.Is(err, ErrGated) || !strings.Contains(err.Error(), "is a gated Hugging Face model. The token is not valid or does not have permission") {
		t.Fatalf("a refused token: %v", err)
	}
	if strings.Contains(err.Error(), "hf_wrongToken000") {
		t.Errorf("the error carries the token: %v", err)
	}
	if len(*seen) != 2 || (*seen)[0].Method != http.MethodHead {
		t.Errorf("requests: %d", len(*seen))
	}
}

func TestSanitizeREADME(t *testing.T) {
	in := "---\nlicense: mit\n---\n\n# Title\n<!-- hidden -->\n<img src=x onerror=alert(1)>text <a href=\"javascript:x\">link</a>\n<style>p{}</style>\n<script>\nbad()\n"
	got := SanitizeREADME(in)
	if got != "# Title\n\ntext link" {
		t.Errorf("SanitizeREADME = %q", got)
	}
}

func TestScrubKeepsTextWithoutAToken(t *testing.T) {
	if got := Scrub("a b", ""); got != "a b" {
		t.Errorf("Scrub with no token = %q", got)
	}
	if got := Scrub("x "+fakeToken, fakeToken); strings.Contains(got, fakeToken) {
		t.Errorf("Scrub = %q", got)
	}
}

// EnvHubLive opts in to TestHubLive, the one test that calls huggingface.co.
const EnvHubLive = "CLOUDBURROW_TEST_HF_HUB"

func TestHubLive(t *testing.T) {
	if os.Getenv(EnvHubLive) == "" {
		t.Skipf("set %s=1 to search huggingface.co's public API", EnvHubLive)
	}
	m := &Manager{Token: func() string { return "" }}
	page, err := m.Search(context.Background(), SearchQuery{Task: "sentence-similarity", Library: "sentence-transformers", Sort: "downloads", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Models) == 0 || page.Next == "" {
		t.Fatalf("page = %+v", page)
	}
	c, err := m.Card(context.Background(), page.Models[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision == "" || c.WeightsBytes == 0 || c.README == "" {
		t.Errorf("card of %s: revision %q, weights %d, README %d bytes", c.ID, c.Revision, c.WeightsBytes, len(c.README))
	}
}
