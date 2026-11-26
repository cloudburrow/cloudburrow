package vertexai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/hostmount"
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
		ArgsFor: func(ctx context.Context, id string) ([]string, error) {
			// On a remote compute host the model directory is carried
			// into a volume there, and the XNNPACK cache the runtime
			// writes stays in it (internal/hostmount).
			models, err := hostmount.Bind(ctx, modelDir, "/models", hostmount.Cache)
			if err != nil {
				return nil, err
			}
			args := []string{"run", "--rm", "-i", "--name", id, "--label", ContainerLabel}
			args = append(args, models.Args()...)
			return append(args,
				// cloudburrow_generate rather than the image's
				// litert_lm_main: the same engine, with the sampler and
				// the token limit set per request (#1797).
				"--entrypoint", RunnerPath,
				image,
				"--backend=cpu",
				"--model_path=/models/"+modelFile,
			), nil
		},
		OnCancel: func(id string) {
			// Detached from the cancelled context on purpose: the cleanup
			// must outlive what caused it.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "docker", "kill", id).Run()
		},
		ModelID: modelID,
		Runner:  true,
	}
}

// RunnerPath is cloudburrow_generate inside the runtime image
// (deploy/litert-lm/runner).
const RunnerPath = "/usr/local/bin/cloudburrow_generate"

// LiteRT-LM's executor backends, as cloudburrow_generate's --backend takes
// them.
const (
	BackendCPU = "cpu"
	BackendGPU = "gpu"
)

// NewHostGenerator returns a generator that runs a native
// cloudburrow_generate (runner) on the host, one process per request as in
// the container, with LiteRT-LM's backend: gpu is the Apple GPU through
// WebGPU on Metal, on a Mac (#2159). Each request's process is a host
// process: cancelling the request ends it directly, with no daemon between.
func NewHostGenerator(runner, modelPath, modelID, backend string) *ExecGenerator {
	return &ExecGenerator{
		Command: runner,
		Args:    []string{"--backend=" + backend, "--model_path=" + modelPath},
		ModelID: modelID,
		Runner:  true,
		Backend: backend,
		Host:    true,
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
	ArgsFor func(ctx context.Context, id string) ([]string, error)
	// OnCancel, when set, runs if the context is cancelled before the process
	// exits. It exists for commands that are clients for work happening
	// elsewhere, where killing the client leaves the work running.
	OnCancel func(id string)
	// ModelID is what this generator reports as the model that ran.
	ModelID string
	// Runner marks the command as cloudburrow_generate: it takes the
	// request's options as flags and writes only the model's text to
	// standard output. Without it the command is litert_lm_main, whose
	// output is parsed for its prompt echo, and options are refused.
	Runner bool
	// Backend is the LiteRT-LM backend the runner was given (BackendCPU
	// when empty), and Host marks a runner running natively on the host
	// rather than in the runtime container (NewHostGenerator).
	Backend string
	Host    bool
}

// Accelerator names where generation runs, for `up`'s summary.
func (g *ExecGenerator) Accelerator() string {
	switch {
	case g.Host && g.Backend == BackendGPU:
		return "the Apple GPU (LiteRT-LM GPU backend, WebGPU on Metal), a host process"
	case g.Host:
		return "CPU, a host process"
	}
	return "CPU, in the runtime container"
}

// Model implements Generator.
func (g *ExecGenerator) Model() string { return g.ModelID }

// HonoursOptions implements OptionsGenerator.
func (g *ExecGenerator) HonoursOptions() bool { return g.Runner }

// The sampling a request gets for each of temperature, topP and topK it
// leaves unset (#2107): Google's documented configuration for the served
// model, Gemma 4 ("Use the following standardized sampling configuration
// across all use cases: temperature=1.0, top_p=0.95, top_k=64", the Gemma 4
// model card), which are also the Gemini 2.5 models' Vertex defaults. Vertex
// samples by default; the model file's own sampler has top-k 1 (measured:
// "sampler type=2 k=1 p=0.95"), which is greedy, so without these every
// unconfigured request, and every one of candidateCount's candidates, would
// be the same answer. Temperature 0 stays greedy (the runner decodes it as
// GREEDY whatever top-k and top-p say).
const (
	defaultTemperature = float32(1.0)
	defaultTopP        = float32(0.95)
	defaultTopK        = int32(64)
)

// withSamplingDefaults is req with Google's default sampling for each field
// it leaves unset, and a random seed when it sets none, as Vertex draws one
// ("By default, a random seed value is used", content generation
// parameters), so that repeated unseeded requests are independent samples.
// A request's own values are kept.
func withSamplingDefaults(req Request) Request {
	if req.Temperature == nil {
		v := defaultTemperature
		req.Temperature = &v
	}
	if req.TopP == nil {
		v := defaultTopP
		req.TopP = &v
	}
	if req.TopK == nil {
		v := defaultTopK
		req.TopK = &v
	}
	if req.Seed == nil {
		var b [4]byte
		_, _ = rand.Read(b[:])
		v := int32(binary.LittleEndian.Uint32(b[:]) & 0x7fffffff)
		req.Seed = &v
	}
	return req
}

// runnerFlags are a request's options as cloudburrow_generate's flags, which
// are LiteRT-LM's own shared flag names.
func runnerFlags(req Request) []string {
	var out []string
	f32 := func(v float32) string { return strconv.FormatFloat(float64(v), 'g', -1, 32) }
	if req.MaxOutputTokens != nil {
		out = append(out, "--max_output_tokens="+strconv.Itoa(int(*req.MaxOutputTokens)))
	}
	if req.Temperature != nil {
		out = append(out, "--sampler_temperature="+f32(*req.Temperature))
	}
	if req.TopP != nil {
		out = append(out, "--sampler_top_p="+f32(*req.TopP))
	}
	if req.TopK != nil {
		out = append(out, "--sampler_top_k="+strconv.Itoa(int(*req.TopK)))
	}
	if req.Seed != nil {
		out = append(out, "--sampler_seed="+strconv.Itoa(int(*req.Seed)))
	}
	if req.JSONSchema != "" {
		out = append(out, "--constraint_json_schema="+req.JSONSchema)
	}
	if req.PresencePenalty != nil {
		out = append(out, "--presence_penalty="+f32(*req.PresencePenalty))
	}
	if req.FrequencyPenalty != nil {
		out = append(out, "--frequency_penalty="+f32(*req.FrequencyPenalty))
	}
	if req.ResponseLogprobs {
		out = append(out, "--response_logprobs")
		if req.TopLogprobs > 0 {
			out = append(out, "--logprobs="+strconv.Itoa(int(req.TopLogprobs)))
		}
	}
	if req.Thinking != nil {
		out = append(out, "--enable_thinking="+strconv.FormatBool(*req.Thinking))
		if *req.Thinking {
			out = append(out, "--thinking_budget="+strconv.Itoa(int(req.ThinkingBudget)))
		}
	}
	if req.Conversation != nil {
		out = append(out, "--request_stdin")
		if req.Vision {
			// The vision encoder is loaded only for a request with an
			// image: it costs memory and start-up time otherwise.
			out = append(out, "--vision_backend=cpu")
		}
	}
	return out
}

// decodedMarker is cloudburrow_generate's last line on standard error.
const decodedMarker = "cloudburrow_generate: decoded_tokens="

// thoughtMarker is cloudburrow_generate's count of the thought channel's
// tokens, written with thinking on (#2107).
const thoughtMarker = "cloudburrow_generate: thought_tokens="

// decodedTokens is the count cloudburrow_generate reported, or -1.
func decodedTokens(stderr string) int { return markerCount(stderr, decodedMarker) }

// markerCount is the number after the last marker on standard error, or -1.
func markerCount(stderr, marker string) int {
	i := strings.LastIndex(stderr, marker)
	if i < 0 {
		return -1
	}
	rest := stderr[i+len(marker):]
	if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return -1
	}
	return n
}

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
		var err error
		if args, err = g.ArgsFor(ctx, id); err != nil {
			return nil, nil, err
		}
	}
	conversation := g.Runner && req.Conversation != nil
	if !g.Runner {
		req.Conversation = nil
	}
	if g.Runner {
		args = append(args, runnerFlags(withSamplingDefaults(req))...)
	}
	var stdin []byte
	if conversation {
		var err error
		if stdin, err = json.Marshal(req.Conversation); err != nil {
			return nil, nil, fmt.Errorf("encoding the conversation: %w", err)
		}
	} else {
		args = append(args, "--input_prompt="+req.Prompt)
	}

	cmd := exec.CommandContext(ctx, g.Command, args...)
	if conversation {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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

	usage := &Usage{PromptTokens: -1, CandidateTokens: -1, Images: req.Vision}
	go func() {
		defer close(finished)
		defer close(out)
		if conversation {
			streamJSONLines(ctx, stdout, out, usage)
		} else if g.Runner {
			streamRaw(ctx, stdout, out)
		} else {
			scanGeneration(ctx, stdout, out, req.Prompt)
		}

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
		finish := FinishReasonStop
		decoded := decodedTokens(stderr.String())
		// The limit was reached when the engine decoded as many tokens as
		// it was allowed: Vertex's MAX_TOKENS.
		if g.Runner && req.MaxOutputTokens != nil {
			if decoded >= int(*req.MaxOutputTokens) {
				finish = FinishReasonMaxTokens
			}
		}
		var last *Usage
		if conversation && usage.PromptTokens >= 0 {
			u := *usage
			u.CandidateTokens = decoded
			if t := markerCount(stderr.String(), thoughtMarker); t > 0 && decoded >= 0 {
				u.ThoughtTokens = min(t, decoded)
			}
			last = &u
		}
		select {
		case out <- Chunk{Last: true, FinishReason: finish, Usage: last}:
		case <-ctx.Done():
		}
	}()

	return out, func() error { return genErr }, nil
}

// runnerLine is one line of cloudburrow_generate's --request_stdin output.
type runnerLine struct {
	PromptTokens *int              `json:"prompt_tokens"`
	Text         *string           `json:"text"`
	Thought      *string           `json:"thought"`
	ToolCalls    []json.RawMessage `json:"tool_calls"`
	Logprob      *struct {
		runnerToken
		Top []runnerToken `json:"top"`
	} `json:"logprob"`
	Compute []struct {
		IDs    []int64  `json:"ids"`
		Tokens []string `json:"tokens"`
	} `json:"compute"`
}

// runnerToken is one token of a logprob line: its bytes in base64, its id
// and its log-probability.
type runnerToken struct {
	Token string  `json:"token"`
	ID    int32   `json:"id"`
	Score float32 `json:"score"`
}

func (t runnerToken) candidate() (LogprobsCandidate, error) {
	tok, err := base64.StdEncoding.DecodeString(t.Token)
	if err != nil {
		return LogprobsCandidate{}, err
	}
	return LogprobsCandidate{Token: string(tok), TokenID: t.ID, LogProbability: t.Score}, nil
}

// streamJSONLines forwards cloudburrow_generate's JSON lines as chunks:
// the prompt's token count first, then text and tool calls as they decode.
// A line that is not JSON is a runtime fault, reported as text would hide it,
// so it ends the stream (the process's exit status then carries the error).
func streamJSONLines(ctx context.Context, r io.Reader, out chan<- Chunk, usage *Usage) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var l runnerLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return
		}
		var c Chunk
		switch {
		case l.PromptTokens != nil:
			usage.PromptTokens = *l.PromptTokens
			u := *usage
			c.Usage = &u
		case l.Text != nil:
			c.Text = *l.Text
		case l.Logprob != nil:
			chosen, err := l.Logprob.candidate()
			if err != nil {
				return
			}
			step := LogprobStep{Chosen: chosen}
			for _, t := range l.Logprob.Top {
				tc, err := t.candidate()
				if err != nil {
					return
				}
				step.Top = append(step.Top, tc)
			}
			c.Logprobs = []LogprobStep{step}
		case l.Thought != nil:
			c.Thought = *l.Thought
		case len(l.ToolCalls) > 0:
			c.ToolCalls = parseToolCalls(l.ToolCalls)
		default:
			continue
		}
		select {
		case out <- c:
		case <-ctx.Done():
			return
		}
	}
}

// parseToolCalls converts LiteRT-LM's OpenAI-style tool calls to Gemini's
// functionCall: {"type":"function","function":{"name","arguments"}}, the
// arguments an object (or, from some processors, a JSON string).
func parseToolCalls(raw []json.RawMessage) []FunctionCall {
	var out []FunctionCall
	for _, r := range raw {
		var tc struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(r, &tc) != nil || tc.Function.Name == "" {
			continue
		}
		args := map[string]any{}
		a := tc.Function.Arguments
		var s string
		if json.Unmarshal(a, &s) == nil {
			a = json.RawMessage(s)
		}
		_ = json.Unmarshal(a, &args)
		out = append(out, FunctionCall{Name: tc.Function.Name, Args: args})
	}
	return out
}

// runOnce runs cloudburrow_generate's count_only or compute mode and returns
// its one JSON line.
func (g *ExecGenerator) runOnce(ctx context.Context, rr *RunnerRequest, vision bool) (*runnerLine, error) {
	if !g.Runner {
		return nil, errNoTokenizer
	}
	id := requestID()
	args := append([]string(nil), g.Args...)
	if g.ArgsFor != nil {
		var err error
		if args, err = g.ArgsFor(ctx, id); err != nil {
			return nil, err
		}
	}
	args = append(args, runnerFlags(Request{Conversation: rr, Vision: vision})...)
	body, err := json.Marshal(rr)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, g.Command, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if g.OnCancel != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				g.OnCancel(id)
			case <-done:
			}
		}()
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s failed: %w: %s", g.Command, err, lastLines(stderr.String(), 5))
	}
	var l runnerLine
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &l); err != nil {
		return nil, fmt.Errorf("%s wrote %q, not a JSON line", g.Command, lastLines(stdout.String(), 2))
	}
	return &l, nil
}

// errNoTokenizer: only cloudburrow_generate exposes the tokenizer.
var errNoTokenizer = errors.New("this runtime exposes no tokenizer: token counts need cloudburrow_generate")

// CountTokens implements TokenCounter.
func (g *ExecGenerator) CountTokens(ctx context.Context, req Request) (int, error) {
	rr := *req.Conversation
	rr.CountOnly = true
	l, err := g.runOnce(ctx, &rr, req.Vision)
	if err != nil {
		return 0, err
	}
	if l.PromptTokens == nil {
		return 0, fmt.Errorf("%s reported no token count", g.Command)
	}
	return *l.PromptTokens, nil
}

// ComputeTokens implements TokenCounter.
func (g *ExecGenerator) ComputeTokens(ctx context.Context, texts []string) ([]TokenInfo, error) {
	rr := &RunnerRequest{Preface: []map[string]any{}, Message: map[string]any{"role": "user", "content": []any{}}, Compute: texts}
	l, err := g.runOnce(ctx, rr, false)
	if err != nil {
		return nil, err
	}
	if len(l.Compute) != len(texts) {
		return nil, fmt.Errorf("%s tokenized %d of %d texts", g.Command, len(l.Compute), len(texts))
	}
	out := make([]TokenInfo, len(texts))
	for i, c := range l.Compute {
		out[i].IDs = c.IDs
		for _, t := range c.Tokens {
			b, err := base64.StdEncoding.DecodeString(t)
			if err != nil {
				return nil, fmt.Errorf("token %q is not base64: %w", t, err)
			}
			out[i].Tokens = append(out[i].Tokens, b)
		}
	}
	return out, nil
}

// streamRaw forwards cloudburrow_generate's standard output as it arrives.
// Everything on it is the model's text, so nothing is parsed; a read may end
// inside a multi-byte character, and that tail is held for the next read so
// no chunk carries half a rune.
func streamRaw(ctx context.Context, r io.Reader, out chan<- Chunk) {
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			cut := len(pending)
			for i := 0; i < utf8.UTFMax && i < len(pending); i++ {
				b := pending[len(pending)-1-i]
				if utf8.RuneStart(b) {
					if !utf8.FullRune(pending[len(pending)-1-i:]) {
						cut = len(pending) - 1 - i
					}
					break
				}
			}
			if cut > 0 {
				select {
				case out <- Chunk{Text: string(pending[:cut])}:
				case <-ctx.Done():
					return
				}
				pending = append([]byte(nil), pending[cut:]...)
			}
		}
		if err != nil {
			if len(pending) > 0 {
				select {
				case out <- Chunk{Text: string(pending)}:
				case <-ctx.Done():
				}
			}
			return
		}
	}
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

// Probe generates a single token with g and returns what failed, if
// anything: a backend that cannot run the model (the GPU delegate, #2159)
// fails here, at startup, rather than on the first request.
func Probe(ctx context.Context, g Generator) error {
	one := int32(1)
	ch, errf, err := g.Generate(ctx, Request{Prompt: "Hi", MaxOutputTokens: &one})
	if err != nil {
		return err
	}
	for range ch {
	}
	if err := errf(); err != nil {
		return err
	}
	return ctx.Err()
}
