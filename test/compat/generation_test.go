//go:build compat

package compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"
)

// EnvLocalAI points at the local generation endpoint.
const EnvLocalAI = "CLOUDBURROW_TEST_LOCALAI"

// localModel is the model a default instance serves. A test that hard-codes it
// is correct here: the point is that the *configured* model is the one that
// answers, and no other.
const localModel = "gemma-4-e2b-it-community"

// staticToken is the bearer token the tests inject.
//
// It is not a credential and authenticates nothing. It exists so the official
// SDK never reaches for Application Default Credentials — which it does by
// default on the Vertex backend, minting a real access token against a real
// Google endpoint even when the base URL is local. That is not hypothetical:
// it happened while this endpoint was being built, on a developer machine with
// gcloud configured, and the harness's environment-variable check did not stop
// it because ADC was on disk rather than in the environment.
const staticToken = "cloudburrow-local-not-a-credential"

type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: staticToken, Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

// genaiClient returns an official SDK client pointed at the local endpoint,
// with credentials that cannot reach Google.
func genaiClient(t *testing.T, h *Harness) *genai.Client {
	t.Helper()
	addr := h.Endpoint(EnvLocalAI)

	var cl *genai.Client
	var err error
	// Explicit credentials mean DetectDefault should never be called; the
	// wrapper makes sure it could find nothing if it were.
	WithoutADC(t, func() {
		cl, err = genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend:  genai.BackendVertexAI,
			Project:  h.Project(),
			Location: "us-central1",
			Credentials: auth.NewCredentials(&auth.CredentialsOptions{
				TokenProvider: staticTokenProvider{},
			}),
			HTTPOptions: genai.HTTPOptions{BaseURL: "http://" + addr},
		})
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	return cl
}

// TestGenerateContentThroughTheOfficialSDK is the claim that matters: the
// endpoint is driven by Google's own client, unmodified.
func TestGenerateContentThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	defer cancel()

	resp, err := cl.Models.GenerateContent(ctx, localModel,
		genai.Text("Reply with the single word: ready."), nil)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	// Protocol assertions only. What the model said is not asserted beyond
	// being present: asserting its words would make the test a measure of the
	// model's mood.
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(resp.Candidates))
	}
	c := resp.Candidates[0]
	if c.Content == nil || len(c.Content.Parts) == 0 {
		t.Fatal("the candidate carries no content")
	}
	if strings.TrimSpace(c.Content.Parts[0].Text) == "" {
		t.Error("the model produced no text")
	}
	if c.FinishReason != genai.FinishReasonStop {
		t.Errorf("finishReason = %q, want STOP", c.FinishReason)
	}
	// The model that ran must be named, so a substitution could not be silent.
	if resp.ModelVersion != localModel {
		t.Errorf("modelVersion = %q, want %q", resp.ModelVersion, localModel)
	}
	t.Logf("model output (not asserted): %q", strings.TrimSpace(c.Content.Parts[0].Text))
}

// TestStreamGenerateContentThroughTheOfficialSDK drives the SSE path with the
// SDK's own iterator, which is the part a hand-rolled client would not catch.
func TestStreamGenerateContentThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	defer cancel()

	var events int
	var text strings.Builder
	var finish genai.FinishReason

	for resp, err := range cl.Models.GenerateContentStream(ctx, localModel,
		genai.Text("Count from one to five in words, one per line."), nil) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		events++
		if len(resp.Candidates) == 0 {
			continue
		}
		c := resp.Candidates[0]
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				text.WriteString(p.Text)
			}
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}

	if events < 2 {
		t.Errorf("got %d events; a stream that arrives in one piece is not streaming", events)
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Error("the stream carried no text")
	}
	if finish != genai.FinishReasonStop {
		t.Errorf("finishReason = %q, want STOP", finish)
	}
	t.Logf("%d events, %d bytes (content not asserted)", events, text.Len())
}

// TestGenerationNeverUsesApplicationDefaultCredentials is the regression test
// for the leak described above.
//
// It asserts the endpoint saw the injected token. If the SDK had fallen back
// to ADC, a real `ya29.` access token would have arrived instead — at a local
// address, from a developer's own Google account.
func TestGenerationNeverUsesApplicationDefaultCredentials(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvLocalAI)

	seen := make(chan string, 1)
	// A proxy in front of the real endpoint, so the assertion is on what the
	// SDK actually put on the wire rather than on how it was configured.
	proxy := newRecordingProxy(t, addr, seen)

	var cl *genai.Client
	var err error
	WithoutADC(t, func() {
		cl, err = genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend:  genai.BackendVertexAI,
			Project:  h.Project(),
			Location: "us-central1",
			Credentials: auth.NewCredentials(&auth.CredentialsOptions{
				TokenProvider: staticTokenProvider{},
			}),
			HTTPOptions: genai.HTTPOptions{BaseURL: proxy},
		})
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	defer cancel()
	if _, err := cl.Models.GenerateContent(ctx, localModel, genai.Text("hello"), nil); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	select {
	case got := <-seen:
		if !strings.Contains(got, staticToken) {
			t.Fatalf("Authorization header was not the injected token")
		}
		if strings.Contains(got, "ya29.") {
			t.Fatal("a real Google access token was sent to a local endpoint: the SDK used ADC")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no request reached the proxy")
	}
}

// newRecordingProxy forwards to target and reports the Authorization header of
// the first request it sees.
func newRecordingProxy(t *testing.T, target string, seen chan<- string) string {
	t.Helper()
	director := func(r *httputil.ProxyRequest) {
		r.SetURL(&url.URL{Scheme: "http", Host: target})
	}
	var once sync.Once
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { seen <- r.Header.Get("Authorization") })
		(&httputil.ReverseProxy{Rewrite: director}).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
