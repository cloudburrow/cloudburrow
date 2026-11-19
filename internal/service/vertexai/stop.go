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
		send := func(c Chunk) bool {
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
			if !c.Last && (c.Usage != nil || len(c.ToolCalls) > 0) {
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
