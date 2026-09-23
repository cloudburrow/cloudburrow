//go:build localai

// Package localai exercises the local generation endpoint against the real
// runtime and a real model, driven by the official Google Gen AI SDK.
//
// It is tagged separately from `compat` because it needs two things no CI
// runner has: the runtime image from `make litert-lm`, and a multi-gigabyte
// model artifact. Everything that can be tested without them is tested in
// internal/service/vertexai, deterministically.
package localai

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// EnvModel is the host path to a .litertlm artifact.
const EnvModel = "CLOUDBURROW_TEST_MODEL"

// EnvImage overrides the runtime image.
const EnvImage = "CLOUDBURROW_TEST_LOCALAI_IMAGE"

const staticToken = "cloudburrow-local-not-a-credential"

type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: staticToken, Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

// refuseCloudCredentials fails rather than skips when the environment would
// let a client reach real Google Cloud, matching the compat harness.
func refuseCloudCredentials(t *testing.T) {
	t.Helper()
	for _, v := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if os.Getenv(v) != "" {
			t.Fatalf("%s is set; this suite refuses to run with cloud credentials in the environment", v)
		}
	}

}

// endpoint starts the real generation server in process and returns its URL.
//
// The generator is built the way `cloudburrow up` builds it, through the same
// exported configuration, so this exercises the shipped command line rather
// than one written for the test.
func endpoint(t *testing.T) (string, string) {
	t.Helper()
	refuseCloudCredentials(t)

	model := strings.TrimSpace(os.Getenv(EnvModel))
	if model == "" {
		t.Skipf("%s is not set; point it at a .litertlm artifact (see docs/generation.md)", EnvModel)
	}
	abs, err := filepath.Abs(model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("%s: %v", EnvModel, err)
	}

	image := os.Getenv(EnvImage)
	if image == "" {
		image = config.DefaultLocalAIImage
	}
	dir, file := filepath.Split(abs)
	modelID := strings.TrimSuffix(file, filepath.Ext(file))

	gen := vertexai.NewDockerGenerator(image, filepath.Clean(dir), file, modelID)
	srv := httptest.NewServer(vertexai.NewServer(gen).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, modelID
}

func client(t *testing.T, baseURL string) *genai.Client {
	t.Helper()
	// Explicit credentials so the SDK never calls DetectDefault. On the Vertex
	// backend it otherwise mints a real access token from Application Default
	// Credentials even when the base URL is local.
	//
	// HOME is moved for the construction only. It is not moved for the whole
	// test because this suite shells out to docker, which reads its own
	// configuration from the home directory.
	var cl *genai.Client
	var err error
	withoutADC(t, func() {
		cl, err = genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend:  genai.BackendVertexAI,
			Project:  "cloudburrow-local",
			Location: "us-central1",
			Credentials: auth.NewCredentials(&auth.CredentialsOptions{
				TokenProvider: staticTokenProvider{},
			}),
			HTTPOptions: genai.HTTPOptions{BaseURL: baseURL},
		})
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	return cl
}

// TestRealGenerationThroughTheOfficialSDK runs the model.
func TestRealGenerationThroughTheOfficialSDK(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	resp, err := cl.Models.GenerateContent(ctx, model,
		genai.Text("Name the capital city of France. Answer in one word."), nil)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(resp.Candidates))
	}
	c := resp.Candidates[0]
	if c.Content == nil || len(c.Content.Parts) == 0 || strings.TrimSpace(c.Content.Parts[0].Text) == "" {
		t.Fatal("the model produced no text")
	}
	if c.FinishReason != genai.FinishReasonStop {
		t.Errorf("finishReason = %q, want STOP", c.FinishReason)
	}
	if resp.ModelVersion != model {
		t.Errorf("modelVersion = %q, want %q", resp.ModelVersion, model)
	}
	// Recorded, not asserted. What the model says is not this project's claim.
	t.Logf("model output (recorded, not asserted): %q", strings.TrimSpace(c.Content.Parts[0].Text))
}

// TestRealStreamingThroughTheOfficialSDK proves tokens arrive incrementally
// from the runtime, through the SDK's own stream iterator.
func TestRealStreamingThroughTheOfficialSDK(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var events int
	var first, last time.Time
	var text strings.Builder
	var finish genai.FinishReason

	start := time.Now()
	for resp, err := range cl.Models.GenerateContentStream(ctx, model,
		genai.Text("Count from one to ten in words, one per line."), nil) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		events++
		if first.IsZero() {
			first = time.Now()
		}
		last = time.Now()
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
		t.Fatalf("got %d events; the response arrived in one piece, so it is not streaming", events)
	}
	if finish != genai.FinishReasonStop {
		t.Errorf("finishReason = %q, want STOP", finish)
	}
	// The spread between the first and last event is the evidence that the
	// stream tracked generation rather than being sliced after the fact.
	spread := last.Sub(first)
	if spread <= 0 {
		t.Errorf("every event arrived at the same instant (%v): the response was buffered", spread)
	}
	t.Logf("%d events over %v (time to first event %v); %d bytes, content not asserted",
		events, spread.Round(time.Millisecond), first.Sub(start).Round(time.Millisecond), text.Len())
}

// runningRuntimeContainers counts containers CloudBurrow started and has not
// cleaned up. Only labelled containers are counted, so nothing the developer
// is running themselves is inspected.
func runningRuntimeContainers(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--filter", "label="+vertexai.ContainerLabel,
		"--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	var names []string
	for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TestRealCancellationStopsTheRuntime proves a cancelled request does not
// leave the model generating into a void.
//
// The container is the part that matters. Cancelling the request kills the
// `docker run` client, and on the first implementation that left the container
// running — still holding the model, still generating, with nobody reading.
// The HTTP test passed while a 2.6 GB process burned CPU in the background,
// which is why this asserts on containers rather than on the response.
func TestRealCancellationStopsTheRuntime(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cl.Models.GenerateContent(ctx, model,
			genai.Text("Write a very long essay about distributed systems."), nil)
		done <- err
	}()

	time.Sleep(3 * time.Second)
	if got := runningRuntimeContainers(t); len(got) == 0 {
		t.Fatal("no runtime container was running, so cancellation is not being tested")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("the cancelled request returned a response")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the request did not end within a minute of cancellation")
	}

	// The client returning is not the same as the work stopping.
	deadline := time.Now().Add(30 * time.Second)
	for {
		left := runningRuntimeContainers(t)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a runtime container survived cancellation: %v — "+
				"the model is still generating with nobody reading it", left)
		}
		time.Sleep(time.Second)
	}
}

// TestRealMultiLinePromptIsNotEchoedBack is the regression test for a defect
// the unit tests could only simulate.
//
// The runtime echoes the prompt before generating, and it echoes a multi-line
// prompt across lines. Forwarding everything after the marker therefore handed
// the caller their own prompt back as generated text. The console's prompt
// field is a textarea, so any user pressing Enter hit this.
func TestRealMultiLinePromptIsNotEchoedBack(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const marker = "ZZMARKERZZ"
	prompt := "What is 2+2? Answer with the number only.\n" + marker + " ignore this line\n" +
		marker + " and this one"

	resp, err := cl.Models.GenerateContent(ctx, model, genai.Text(prompt), nil)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		t.Fatal("no content")
	}
	var got strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		got.WriteString(p.Text)
	}
	if strings.Contains(got.String(), marker) {
		t.Errorf("the prompt was returned as model output: %q", got.String())
	}
	if strings.TrimSpace(got.String()) == "" {
		t.Error("the whole response was dropped: the skip removed real output")
	}
	t.Logf("model output (recorded, not asserted): %q", strings.TrimSpace(got.String()))
}

// TestRealUnsupportedOptionIsRefusedByTheRealEndpoint checks the refusal
// survives the SDK's own encoding, which is where a field name typo would show.
func TestRealUnsupportedOptionIsRefusedByTheRealEndpoint(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := cl.Models.GenerateContent(ctx, model, genai.Text("hi"),
		&genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0.2)})
	if err == nil {
		t.Fatal("temperature was accepted; the caller would believe it was applied")
	}
	if !strings.Contains(err.Error(), "temperature") {
		t.Errorf("the error does not name the offending field: %v", err)
	}
}

// withoutADC runs fn with the well-known credentials file out of reach.
//
// Scoped to fn rather than the whole test: this suite runs docker, and docker
// reads its configuration and context from the home directory.
func withoutADC(t *testing.T, fn func()) {
	t.Helper()
	empty := t.TempDir()
	for _, v := range []string{"HOME", "APPDATA"} {
		old, had := os.LookupEnv(v)
		if err := os.Setenv(v, empty); err != nil {
			t.Fatalf("set %s: %v", v, err)
		}
		defer func(name, value string, present bool) {
			if present {
				_ = os.Setenv(name, value)
				return
			}
			_ = os.Unsetenv(name)
		}(v, old, had)
	}
	fn()
}
