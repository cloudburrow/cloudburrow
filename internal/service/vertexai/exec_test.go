package vertexai

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeRuntime returns an ExecGenerator driven by a shell script that imitates
// litert_lm_main's output.
//
// The script receives the real --input_prompt= argument the generator builds,
// so the argument handling is exercised rather than assumed. echoPrompt
// selects whether the imitation echoes a multi-line prompt across lines, which
// is the behaviour in question.
func fakeRuntime(script string) *ExecGenerator {
	return &ExecGenerator{
		Command: "sh",
		// "fake" lands in $0 so the appended --input_prompt=... is $1.
		Args:    []string{"-c", script, "fake"},
		ModelID: "fake-model",
	}
}

func collect(t *testing.T, g *ExecGenerator, prompt string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	chunks, errFn, err := g.Generate(ctx, Request{Prompt: prompt})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var out strings.Builder
	for c := range chunks {
		if !c.Last {
			out.WriteString(c.Text)
		}
	}
	if err := errFn(); err != nil {
		t.Fatalf("generation failed: %v", err)
	}
	return out.String()
}

// The script imitates the runtime: some logging, the prompt echo, the
// generated text, then the benchmark block.
const echoingRuntime = `
prompt="${1#--input_prompt=}"
echo "INFO: Created TensorFlow Lite XNNPACK delegate for CPU."
echo "input_prompt: $prompt"
echo "GENERATED ONE"
echo "GENERATED TWO"
echo ""
echo "BenchmarkInfo:"
echo "  Init Total: 306.55 ms"
`

// The same, but the echo does not reproduce the prompt's newlines.
const nonEchoingRuntime = `
prompt="${1#--input_prompt=}"
first=$(printf '%s' "$prompt" | head -1)
echo "input_prompt: $first"
echo "GENERATED ONE"
echo "GENERATED TWO"
echo "BenchmarkInfo:"
`

func TestSingleLinePromptIsNotForwardedAsOutput(t *testing.T) {
	got := collect(t, fakeRuntime(echoingRuntime), "Name the capital of France.")
	want := "GENERATED ONE\nGENERATED TWO\n\n"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// A multi-line prompt is echoed across lines. Every line after the first would
// be forwarded as though the model had produced it — and the console's prompt
// field is a textarea, so this is what a user gets for pressing Enter.
func TestMultiLinePromptIsNotForwardedAsOutput(t *testing.T) {
	prompt := "Summarise this:\nthe second line\nthe third line"
	got := collect(t, fakeRuntime(echoingRuntime), prompt)

	for _, leaked := range []string{"the second line", "the third line"} {
		if strings.Contains(got, leaked) {
			t.Errorf("the prompt was echoed back as model output: %q appears in %q", leaked, got)
		}
	}
	want := "GENERATED ONE\nGENERATED TWO\n\n"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// The skip must not cost real output if the runtime does not echo the
// continuation: matching, rather than counting, is what makes that safe.
func TestOutputIsNotDroppedWhenThePromptIsNotEchoedInFull(t *testing.T) {
	prompt := "Summarise this:\nthe second line"
	got := collect(t, fakeRuntime(nonEchoingRuntime), prompt)

	want := "GENERATED ONE\nGENERATED TWO\n"
	if got != want {
		t.Errorf("output = %q, want %q — a line of real output was dropped", got, want)
	}
}

// Output that happens to begin before the marker must not leak either.
func TestLoggingBeforeTheMarkerIsNotOutput(t *testing.T) {
	got := collect(t, fakeRuntime(echoingRuntime), "hello")
	if strings.Contains(got, "XNNPACK") {
		t.Errorf("runtime logging reached the caller: %q", got)
	}
}

// A failing runtime must surface its own message rather than an empty result.
func TestRuntimeFailureIsReported(t *testing.T) {
	g := fakeRuntime(`echo "model file is corrupt" >&2; exit 3`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	chunks, errFn, err := g.Generate(ctx, Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for range chunks {
	}
	err = errFn()
	if err == nil {
		t.Fatal("a runtime that exited 3 was reported as success")
	}
	if !strings.Contains(err.Error(), "model file is corrupt") {
		t.Errorf("the runtime's own message was lost: %v", err)
	}
}
