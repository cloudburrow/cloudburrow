//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// covers: google.cloud.aiplatform.v1.PredictionService/StreamGenerateContent, google.cloud.aiplatform.v1.PredictionService/GenerateContent
//
// TestGenerateContentOnTheAppleGPU (#2159): on a Mac with Apple silicon,
// an instance started with --local-ai-accelerator=metal (the default
// there) generates Gemini content with CloudBurrow's macOS
// cloudburrow_generate on LiteRT-LM's GPU backend, as a host process: while
// the official SDK's stream is open, the host runs
// `cloudburrow_generate --backend=gpu`, and no runtime container runs. The
// rest of the generation tests run unchanged against the same instance.
// The test logs the stream's decode rate.
func TestGenerateContentOnTheAppleGPU(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skipf("the Apple GPU (Metal) is on macOS on Apple silicon only; this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if os.Getenv(EnvMetal) == "" {
		t.Skipf("set %s=1, with an instance on --local-ai-accelerator=metal and the macOS runner installed", EnvMetal)
	}
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skipf("%s is not set", EnvLocalAI)
	}
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	max := int32(128)
	temp := float32(0)
	cfg := &genai.GenerateContentConfig{MaxOutputTokens: max, Temperature: &temp}
	start := time.Now()
	var first time.Time
	var text strings.Builder
	var host, container string
	var tokens int32
	for resp, err := range cl.Models.GenerateContentStream(ctx, localModel, genai.Text("Write a long story about a fox who learns to fly."), cfg) {
		if err != nil {
			t.Fatalf("GenerateContentStream: %v", err)
		}
		if first.IsZero() && strings.TrimSpace(respText(resp)) != "" {
			// The first chunk with text: the stream's first carries only
			// the prompt's token count, before the model has loaded.
			first = time.Now()
			out, _ := exec.Command("ps", "-axo", "command").Output()
			for _, l := range strings.Split(string(out), "\n") {
				if strings.Contains(l, "cloudburrow_generate") && strings.Contains(l, "--model_path=") {
					host = l
				}
			}
			ps, _ := exec.Command("docker", "ps", "--filter", "label=cloudburrow.localai=1", "-q").Output()
			container = strings.TrimSpace(string(ps))
		}
		text.WriteString(respText(resp))
		if resp.UsageMetadata != nil && resp.UsageMetadata.CandidatesTokenCount > 0 {
			tokens = resp.UsageMetadata.CandidatesTokenCount
		}
	}
	if host == "" && first.IsZero() {
		t.Fatal("the stream carried no text")
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Error("the model produced no text")
	}
	if !strings.Contains(host, "--backend=gpu") {
		t.Errorf("no host cloudburrow_generate --backend=gpu ran during the stream (found %q): the instance did not generate on the Apple GPU", host)
	} else {
		t.Logf("host process: %s", strings.Fields(host)[0])
	}
	if container != "" {
		t.Errorf("a runtime container (%s) ran during the stream; want the host process only", container)
	}
	if tokens > 1 && !first.IsZero() {
		t.Logf("Apple GPU stream: %d tokens in %.2f s, %.1f tokens/s after the first text (%.2f s to it)",
			tokens, time.Since(start).Seconds(), float64(tokens-1)/time.Since(first).Seconds(), first.Sub(start).Seconds())
	}
}

// respText is a streamed response's text.
func respText(resp *genai.GenerateContentResponse) string {
	var b strings.Builder
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, p := range resp.Candidates[0].Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
