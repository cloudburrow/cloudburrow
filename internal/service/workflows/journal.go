package workflows

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Executions survive a restart of the instance, as Google's survive its
// own: an execution that was ACTIVE when the instance stopped carries on
// when it starts again, from where it was.
//
// The interpreter records every effect an execution has on the world, or
// that the world has on it, in the execution's journal as it happens: the
// result of each http.* step and connector, each sys.log and call log
// entry written, each callback endpoint made and request it received, each
// child execution started, the time each sleep ends, sys.now and
// uuid.generate. The journal is saved with the execution. After a restart
// the execution runs its workflow again from the start with the same
// argument; each effect already in the journal is not done again but
// answers what it answered before, so the execution reaches the point it
// had reached with the same variables, and goes on from there for real. A
// sleep that was under way sleeps only for what remains of it.
//
// An entry is keyed by the thread (a parallel branch or iteration has its
// own) and the effect's number in that thread, so parallel branches replay
// in any order. A step whose effect had begun but was not yet journaled
// when the instance stopped (an HTTP request in flight) is done again:
// effects are at least once, which is also what Google's documents for
// retried steps.

// journalEntry is one effect.
type journalEntry struct {
	Key   string          `json:"key"`
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value,omitempty"`
	Error json.RawMessage `json:"error,omitempty"`
}

// errNotJournaled marks an effect whose outcome cannot be replayed (the
// instance stopping, or a value that cannot be journaled): it is done
// again on resume.
var errNotJournaled = errors.New("not journaled")

// effect runs do once: on a resumed execution, an effect already in the
// journal answers from it instead.
func (x *machine) effect(f *frame, kind string, do func() (any, error)) (any, error) {
	x.journalMu.Lock()
	if x.seq == nil {
		x.seq = map[string]int{}
	}
	n := x.seq[f.thread]
	x.seq[f.thread] = n + 1
	key := f.thread + "#" + strconv.Itoa(n)
	e, replay := x.replay[key]
	x.journalMu.Unlock()
	if replay && e.Kind == kind {
		if len(e.Error) > 0 {
			v, err := decodeTyped(e.Error)
			if err == nil {
				return nil, &wfError{val: v}
			}
		} else {
			v, err := decodeTyped(e.Value)
			if err == nil {
				return v, nil
			}
		}
	}
	v, err := do()
	entry := journalEntry{Key: key, Kind: kind}
	var we *wfError
	switch {
	case err == nil:
		b, jerr := encodeTyped(v)
		if jerr != nil {
			return v, nil
		}
		entry.Value = b
	case errors.As(err, &we):
		b, jerr := encodeTyped(we.val)
		if jerr != nil {
			return v, err
		}
		entry.Error = b
	default:
		return v, err
	}
	x.journalMu.Lock()
	x.journal = append(x.journal, entry)
	save := x.onJournal
	x.journalMu.Unlock()
	if save != nil {
		save()
	}
	return v, err
}

// Journal is a copy of the journal, for saving.
func (x *machine) Journal() []journalEntry {
	x.journalMu.Lock()
	defer x.journalMu.Unlock()
	return append([]journalEntry(nil), x.journal...)
}

// resumeFrom loads a saved journal to replay.
func (x *machine) resumeFrom(j []journalEntry) {
	x.journalMu.Lock()
	defer x.journalMu.Unlock()
	x.replay = map[string]journalEntry{}
	for _, e := range j {
		x.replay[e.Key] = e
	}
	x.journal = append([]journalEntry(nil), j...)
}

// replaying is whether the thread's next effect is in the journal: what
// the execution does before it (a call log entry) was done already.
func (x *machine) replaying(f *frame) bool {
	x.journalMu.Lock()
	defer x.journalMu.Unlock()
	_, ok := x.replay[f.thread+"#"+strconv.Itoa(x.seq[f.thread])]
	return ok
}

// sleepFor sleeps d, journaling when it ends so a resumed execution only
// sleeps what is left.
func (f *frame) sleepFor(d time.Duration) error {
	v, err := f.x.effect(f, "sleep", func() (any, error) { return f.x.now().Add(d).UnixNano(), nil })
	if err != nil {
		return err
	}
	end, _ := v.(int64)
	return f.x.sleep(time.Until(time.Unix(0, end)))
}

// encodeTyped journals a value keeping its type: integers, doubles and
// bytes come back as they went in.
func encodeTyped(v any) (json.RawMessage, error) {
	t, err := toTyped(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(t)
}

func toTyped(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return map[string]any{"n": true}, nil
	case bool:
		return map[string]any{"b": x}, nil
	case string:
		return map[string]any{"s": x}, nil
	case int64:
		return map[string]any{"i": strconv.FormatInt(x, 10)}, nil
	case float64:
		return map[string]any{"f": strconv.FormatFloat(x, 'g', -1, 64)}, nil
	case []byte:
		return map[string]any{"y": base64.StdEncoding.EncodeToString(x)}, nil
	case []any:
		out := make([]any, len(x))
		for i, w := range x {
			t, err := toTyped(w)
			if err != nil {
				return nil, err
			}
			out[i] = t
		}
		return map[string]any{"l": out}, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, w := range x {
			t, err := toTyped(w)
			if err != nil {
				return nil, err
			}
			out[k] = t
		}
		return map[string]any{"m": out}, nil
	}
	return nil, fmt.Errorf("%w: a %T", errNotJournaled, v)
}

func decodeTyped(b json.RawMessage) (any, error) {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	return fromTyped(raw)
}

func fromTyped(raw any) (any, error) {
	m, ok := raw.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, fmt.Errorf("bad journal value")
	}
	for k, v := range m {
		switch k {
		case "n":
			return nil, nil
		case "b":
			b, _ := v.(bool)
			return b, nil
		case "s":
			s, _ := v.(string)
			return s, nil
		case "i":
			s, _ := v.(string)
			return strconv.ParseInt(s, 10, 64)
		case "f":
			s, _ := v.(string)
			return strconv.ParseFloat(s, 64)
		case "y":
			s, _ := v.(string)
			return base64.StdEncoding.DecodeString(s)
		case "l":
			l, _ := v.([]any)
			out := make([]any, len(l))
			for i, w := range l {
				d, err := fromTyped(w)
				if err != nil {
					return nil, err
				}
				out[i] = d
			}
			return out, nil
		case "m":
			mm, _ := v.(map[string]any)
			out := make(map[string]any, len(mm))
			for k2, w := range mm {
				d, err := fromTyped(w)
				if err != nil {
					return nil, err
				}
				out[k2] = d
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("bad journal value")
}
