package vertexai

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ContainerLabel marks runtime containers CloudBurrow started.
//
// It exists so an orphaned container can be found and asserted against. Only
// containers carrying it are ever inspected or stopped: CloudBurrow does not
// touch containers it did not create.
const ContainerLabel = "cloudburrow.localai=1"

// NewDockerGenerator returns a generator that runs the runtime image against a
// model on the host.
//
// The CLI and the tests both call this, so the command being verified is the
// command that ships. Two details are not obvious:
//
// The model directory is mounted writable rather than read-only, because the
// runtime writes its XNNPACK cache beside the model. Without it every request
// pays about three seconds of extra initialisation.
//
// Each request names its container and kills it by name on cancellation.
// Signalling the client is not enough: `docker run` is a client for work
// happening in the daemon, and the runtime does not stop on SIGTERM. Measured
// — a cancelled request left the container running, holding 2.6 GB and still
// generating output nobody would read, while the HTTP request returned
// promptly enough to look correct.
func NewDockerGenerator(image, modelDir, modelFile, modelID string) *ExecGenerator {
	return &ExecGenerator{
		Command: "docker",
		ArgsFor: func(id string) []string {
			return []string{
				"run", "--rm", "-i",
				"--name", id,
				"--label", ContainerLabel,
				"-v", modelDir + ":/models",
				image,
				"--backend=cpu",
				"--model_path=/models/" + modelFile,
			}
		},
		OnCancel: func(id string) {
			// Detached from the cancelled context on purpose: the cleanup
			// must outlive what caused it.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "docker", "kill", id).Run()
		},
		ModelID: modelID,
	}
}

// ExecGenerator runs the LiteRT-LM runtime as a subprocess, one process per
// request, and streams its standard output.
//
// One process per request is the runtime's own shape: litert_lm_main takes a
// prompt, generates, and exits. It has no server mode. With a warm XNNPACK
// cache initialisation measures around 0.3 s, which is acceptable for local
// development and is the honest cost of the runtime as published. Pretending
// otherwise would mean writing a server upstream does not have.
type ExecGenerator struct {
	// Command is argv[0] and Args the fixed arguments preceding the
	// per-request ones — for example {"docker","run","--rm", ...}.
	Command string
	Args    []string
	// ArgsFor, when set, replaces Args and receives the request's ID, for
	// commands that need to name the work so it can be stopped later.
	ArgsFor func(id string) []string
	// OnCancel, when set, runs if the context is cancelled before the process
	// exits. It exists for commands that are clients for work happening
	// elsewhere, where killing the client leaves the work running.
	OnCancel func(id string)
	// ModelID is what this generator reports as the model that ran.
	ModelID string
}

// Model implements Generator.
func (g *ExecGenerator) Model() string { return g.ModelID }

// promptMarker is echoed by the runtime before generation begins. Everything
// after it on subsequent lines is model output; everything before is the
// runtime's own logging.
const promptMarker = "input_prompt:"

// benchmarkMarker is printed after generation ends.
const benchmarkMarker = "BenchmarkInfo:"

// Generate implements Generator.
func (g *ExecGenerator) Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error) {
	id := requestID()
	args := append([]string(nil), g.Args...)
	if g.ArgsFor != nil {
		args = g.ArgsFor(id)
	}
	args = append(args, "--input_prompt="+req.Prompt)

	cmd := exec.CommandContext(ctx, g.Command, args...)
	// Cancellation sends SIGTERM rather than the default SIGKILL.
	//
	// This matters more than it looks. When the command is `docker run`, the
	// process being cancelled is the *client*: killing it outright leaves the
	// container running, still holding a multi-gigabyte model and still
	// burning CPU on output nobody will read. SIGTERM lets the client forward
	// the signal to the container and let --rm do its work. WaitDelay is the
	// backstop for a process that ignores it.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Standard error is captured rather than discarded: when the runtime
	// fails, its message is the only useful thing we have.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", g.Command, err)
	}

	out := make(chan Chunk)
	var once sync.Once
	var genErr error
	setErr := func(e error) { once.Do(func() { genErr = e }) }

	finished := make(chan struct{})
	if g.OnCancel != nil {
		go func() {
			select {
			case <-ctx.Done():
				g.OnCancel(id)
			case <-finished:
			}
		}()
	}

	go func() {
		defer close(finished)
		defer close(out)
		scanGeneration(ctx, stdout, out, req.Prompt)

		// Drain anything left so the child never blocks writing.
		_, _ = io.Copy(io.Discard, stdout)

		if err := cmd.Wait(); err != nil {
			// A cancelled context kills the process; that is the caller's
			// doing, not a runtime failure, and ctx.Err() says so already.
			if ctx.Err() == nil {
				setErr(fmt.Errorf("%s failed: %w: %s", g.Command, err, lastLines(stderr.String(), 5)))
			}
			return
		}
		select {
		case out <- Chunk{Last: true, FinishReason: FinishReasonStop}:
		case <-ctx.Done():
		}
	}()

	return out, func() error { return genErr }, nil
}

// scanGeneration forwards the model's output lines.
//
// The runtime echoes "input_prompt: <prompt>" before generating, so the marker
// line is where output begins — for a single-line prompt. A multi-line prompt
// is echoed across lines, which was measured rather than assumed:
//
//	--input_prompt="FIRSTLINE what is 2+2?\nSECONDLINE ignore this\nTHIRDLINE ignore this too"
//
//	input_prompt: FIRSTLINE what is 2+2?
//	SECONDLINE ignore this
//	THIRDLINE ignore this too
//	2+2 is 4
//
// Only the last line is the model's. Forwarding from the marker would have
// handed the caller their own prompt back as generated text, and the console's
// prompt field is a textarea, so this is reachable rather than theoretical.
//
// The continuation is skipped by *matching* it, not by counting lines. If a
// future runtime stops echoing it, the first line that fails to match is
// treated as output and nothing real is lost — the fix cannot turn into a
// different bug when the behaviour it compensates for goes away.
func scanGeneration(ctx context.Context, r io.Reader, out chan<- Chunk, prompt string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	echo := promptContinuation(prompt)
	started := false
	for sc.Scan() {
		line := sc.Text()

		if !started {
			if strings.HasPrefix(line, promptMarker) {
				started = true
			}
			continue
		}
		if len(echo) > 0 {
			if line == echo[0] {
				echo = echo[1:]
				continue
			}
			// Not the echo after all; stop looking and treat this as output.
			echo = nil
		}
		if strings.HasPrefix(line, benchmarkMarker) {
			break
		}

		text := line + "\n"
		select {
		case out <- Chunk{Text: text}:
		case <-ctx.Done():
			return
		}
	}
}

// promptContinuation returns the prompt's lines after the first, which are the
// ones the marker line does not absorb.
func promptContinuation(prompt string) []string {
	lines := strings.Split(prompt, "\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}

// requestID names one request's work uniquely enough to stop it by name.
func requestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A collision would only mean cancelling the wrong local request,
		// and the clock makes that vanishingly unlikely on its own.
		return fmt.Sprintf("cloudburrow-localai-%d", time.Now().UnixNano())
	}
	return "cloudburrow-localai-" + hex.EncodeToString(b[:])
}

// lastLines keeps an error message useful without pasting an entire log.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
