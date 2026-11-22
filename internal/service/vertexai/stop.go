package vertexai

import (
	"context"
	"strings"
)

// applyStops ends a generation at the first of stops, as Vertex AI's
// generationConfig.stopSequences does: the text before the sequence is
// returned, the sequence itself is not, and the finish reason is STOP.
//
// The runtime has no stop-sequence control, so this is done on the stream:
// up to the longest sequence's length minus one byte is held back, because a
// sequence can arrive split across chunks. On a match the generation is
// cancelled (cancel stops the runtime; ExecGenerator kills its container) so
// the model does not keep decoding text nobody will read.
//
// cancel is called whenever the source ends, so the context it belongs to is
// always released.
func applyStops(ctx context.Context, cancel context.CancelFunc, in <-chan Chunk, stops []string) <-chan Chunk {
	out := make(chan Chunk)
	hold := 0
	for _, s := range stops {
		if len(s)-1 > hold {
			hold = len(s) - 1
		}
	}
	go func() {
		defer close(out)
		defer cancel()
		var lp logprobQueue
		// send forwards c, carrying the logprob steps of the text sent so
		// far (all of them for a chunk that is not text).
		send := func(c Chunk) bool {
			switch {
			case c.Text != "" && len(c.ToolCalls) == 0 && !c.Last:
				c.Logprobs = lp.take(len(c.Text))
			case c.Last || len(c.ToolCalls) > 0:
				c.Logprobs = lp.takeAll()
			}
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		var pending string
		var usage *Usage // the prompt's count, for a generation cut short
		for c := range in {
			// A decode step's log-probabilities wait for the text its
			// token spells (#2107): a stop sequence may yet cut it.
			if len(c.Logprobs) > 0 && c.Text == "" && !c.Last && len(c.ToolCalls) == 0 && c.Thought == "" && c.Usage == nil {
				lp.add(c.Logprobs)
				continue
			}
			// Thoughts pass through: stop sequences end the answer, not
			// the thinking before it (#2107).
			if !c.Last && (c.Usage != nil || len(c.ToolCalls) > 0 || c.Thought != "") {
				// Not text: the held text goes first, then the chunk as is.
				if c.Usage != nil {
					usage = c.Usage
				}
				if pending != "" && !send(Chunk{Text: pending}) {
					drain(in)
					return
				}
				pending = ""
				c.Text = ""
				if !send(c) {
					drain(in)
					return
				}
				continue
			}
			if c.Last {
				if pending != "" && !send(Chunk{Text: pending}) {
					drain(in)
					return
				}
				pending = ""
				if !send(c) {
					drain(in)
					return
				}
				continue
			}
			pending += c.Text
			if at := firstStop(pending, stops); at >= 0 {
				if at > 0 && !send(Chunk{Text: pending[:at]}) {
					drain(in)
					return
				}
				cancel()
				drain(in)
				// The runtime was stopped before it counted what it
				// decoded, so only the prompt's count is known.
				var u *Usage
				if usage != nil {
					cp := *usage
					cp.CandidateTokens = -1
					u = &cp
				}
				// The steps of the tokens after the cut are not part of
				// the answer.
				lp.drop()
				send(Chunk{Last: true, FinishReason: FinishReasonStop, Usage: u})
				return
			}
			// Everything but the tail that could begin a sequence is safe.
			safe := len(pending) - hold
			for safe > 0 && safe < len(pending) && !isRuneStart(pending[safe]) {
				safe--
			}
			if safe > 0 {
				if !send(Chunk{Text: pending[:safe]}) {
					drain(in)
					return
				}
				pending = pending[safe:]
			}
		}
		if pending != "" {
			send(Chunk{Text: pending})
		}
	}()
	return out
}

// logprobQueue holds decode steps until the text their tokens spell is
// sent. sent counts the bytes of answer text sent, and each held step's
// token begins at the byte offset after the steps before it, so the steps
// a chunk of text carries are those whose token begins inside it. The
// runtime writes a step before the text it decodes to, so a step is always
// held by the time its text arrives.
type logprobQueue struct {
	steps []LogprobStep
	start []int // each held step's token's offset in the answer text
	next  int   // the offset after the last held step's token
	sent  int
}

func (q *logprobQueue) add(steps []LogprobStep) {
	for _, s := range steps {
		q.steps = append(q.steps, s)
		q.start = append(q.start, q.next)
		q.next += len(s.Chosen.Token)
	}
}

// take returns the held steps whose token begins in the next n bytes of
// text.
func (q *logprobQueue) take(n int) []LogprobStep {
	q.sent += n
	i := 0
	for i < len(q.steps) && q.start[i] < q.sent {
		i++
	}
	out := q.steps[:i:i]
	q.steps, q.start = q.steps[i:], q.start[i:]
	return out
}

// takeAll returns every held step, for a chunk that is not text (a function
// call, the end), and starts the text offsets over after them.
func (q *logprobQueue) takeAll() []LogprobStep {
	out := q.steps
	q.steps, q.start = nil, nil
	q.sent = q.next
	return out
}

func (q *logprobQueue) drop() { q.steps, q.start = nil, nil }

// firstStop is the byte offset of the earliest stop sequence in s, or -1.
func firstStop(s string, stops []string) int {
	at := -1
	for _, st := range stops {
		if i := strings.Index(s, st); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	return at
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
