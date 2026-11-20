package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Runtime is what an execution reaches outside itself: HTTP, connectors,
// logging and tokens. The server supplies one per execution.
type Runtime interface {
	// Do sends an http.* step or a connector's request (http.go).
	HTTP() *HTTPCaller
	// Log writes a sys.log entry.
	Log(severity string, payload any)
}

// StepEntry is one entry of an execution's history (the stepEntries
// resource of workflowexecutions v1).
type StepEntry struct {
	EntryID    int64  `json:"entryId,string"`
	Step       string `json:"step"`
	Routine    string `json:"routine"`
	StepType   string `json:"stepType"`
	State      string `json:"state"`
	CreateTime string `json:"createTime"`
	UpdateTime string `json:"updateTime"`
	ThreadID   string `json:"threadId,omitempty"`
	Exception  string `json:"exception,omitempty"`
	Parent     int64  `json:"parent,omitempty,string"`
	// ProgressType and ProgressNumber are a for loop's progress
	// (stepEntryMetadata): PROGRESS_TYPE_FOR and the iteration it is on,
	// counting from 1. ExpectedIteration is how many it will run, recorded
	// with detailed execution history only.
	ProgressType      string `json:"progressType,omitempty"`
	ProgressNumber    int64  `json:"progressNumber,omitempty,string"`
	ExpectedIteration int64  `json:"expectedIteration,omitempty,string"`
	// Variables are the variables in scope when the step finished, each as
	// JSON (variableData), recorded with detailed execution history only.
	Variables map[string]json.RawMessage `json:"variables,omitempty"`
}

type ctrlKind int

const (
	cNone ctrlKind = iota
	cNext
	cEnd
	cReturn
	cBreak
	cContinue
)

type ctrl struct {
	kind   ctrlKind
	target string
	val    any
}

// machine runs one execution of a program.
type machine struct {
	ctx  context.Context
	prog *program
	rt   Runtime
	env  map[string]any
	now  func() time.Time

	steps     atomic.Int64
	unhandled atomic.Int64
	mu        sync.Mutex // variables
	// assignMu makes an assign step in a parallel branch atomic, as
	// Google's are: total: ${total + v} from every branch adds them all.
	assignMu   sync.Mutex
	sem        chan struct{}
	callbacks  *callbackSet
	historyMu  sync.Mutex
	history    []*StepEntry
	nextEntry  int64
	currentMu  sync.Mutex
	current    [2]string // routine, step
	maxHistory int
	// detailed is the execution's history level: EXECUTION_HISTORY_DETAILED
	// also records each entry's in-scope variables and a for loop's
	// expected iterations.
	detailed bool

	// The journal of effects (journal.go).
	journalMu sync.Mutex
	journal   []journalEntry
	replay    map[string]journalEntry
	seq       map[string]int
	onJournal func()

	// callLevel is the execution's call log level (calllog.go); emit
	// writes a log entry to logID.
	callLevel string
	emit      func(logID, severity string, payload any)
	execID    string
	// children starts and waits for experimental.executions' executions.
	children ChildRunner
}

type scope struct {
	vars   map[string]any
	parent *scope
	// isolate marks a parallel branch or iteration: the parent's variables
	// are read-only through it except those in shared.
	isolate bool
	shared  map[string]bool
}

type frame struct {
	x        *machine
	sc       *scope
	routine  *routine
	depth    int
	parDepth int
	thread   string
	parent   int64
	// locked is set while this frame holds the machine's assignMu.
	locked bool
	// caller is the call step that called this frame's routine, when it is
	// call-logged.
	caller *loggedCall
	// pendingCall is the call step being run, which a routine it calls
	// takes as its caller.
	pendingCall *loggedCall
}

func (f *frame) child(isolate bool, shared []string) *frame {
	g := *f
	g.sc = &scope{vars: map[string]any{}, parent: f.sc, isolate: isolate}
	if isolate {
		g.sc.shared = map[string]bool{}
		for _, s := range shared {
			g.sc.shared[s] = true
		}
	}
	return &g
}

func (f *frame) lookup(name string) (any, bool) {
	f.x.mu.Lock()
	defer f.x.mu.Unlock()
	for s := f.sc; s != nil; s = s.parent {
		if v, ok := s.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// variables are the frame's variables in scope, each as JSON, for a step
// entry's variableData; nil unless the execution keeps detailed history.
func (f *frame) variables() map[string]json.RawMessage {
	if !f.x.detailed {
		return nil
	}
	f.x.mu.Lock()
	defer f.x.mu.Unlock()
	out := map[string]json.RawMessage{}
	for s := f.sc; s != nil; s = s.parent {
		for k, v := range s.vars {
			if _, shadowed := out[k]; shadowed {
				continue
			}
			if b, err := encodeJSON(v, false); err == nil {
				out[k] = b
			}
		}
	}
	return out
}

// progress records a for loop's iteration on the step entry id, the for
// step's: the iteration it is on, and with detailed history how many it
// runs.
func (x *machine) progress(id int64, n, of int) {
	x.historyMu.Lock()
	defer x.historyMu.Unlock()
	if id <= 0 || int(id) > len(x.history) || x.history[id-1].EntryID != id {
		return
	}
	e := x.history[id-1]
	e.ProgressType, e.ProgressNumber = "PROGRESS_TYPE_FOR", int64(n)
	if x.detailed {
		e.ExpectedIteration = int64(of)
	}
}

func (f *frame) isFunction(name string) bool {
	if f.x.prog.routines[name] != nil {
		return true
	}
	_, ok := stdlib[name]
	return ok
}

// set assigns a variable: where it already is in reach, or in the
// innermost scope. A parallel branch writes its parent's variables only
// when they are shared.
func (f *frame) set(name string, update func(old any, exists bool) (any, error)) error {
	f.x.mu.Lock()
	defer f.x.mu.Unlock()
	crossed := false
	allowed := true
	for s := f.sc; s != nil; s = s.parent {
		if old, ok := s.vars[name]; ok {
			if crossed && !allowed {
				return raiseErr("TypeError", "variable '%s' belongs to the parent of a parallel step and is not shared", name)
			}
			v, err := update(old, true)
			if err != nil {
				return err
			}
			s.vars[name] = v
			return nil
		}
		if s.isolate {
			if !crossed {
				allowed = s.shared[name]
			} else {
				allowed = allowed && s.shared[name]
			}
			crossed = true
		}
	}
	v, err := update(nil, false)
	if err != nil {
		return err
	}
	f.sc.vars[name] = v
	return nil
}

// memory is the size of the variables in reach of f.
func (f *frame) memory() int {
	f.x.mu.Lock()
	defer f.x.mu.Unlock()
	n := 0
	for s := f.sc; s != nil; s = s.parent {
		for k, v := range s.vars {
			n += len(k) + sizeOf(v)
		}
	}
	return n
}

// assignTo writes v at target, copying the maps and lists on its path so
// no other reference sees the change.
func (f *frame) assignTo(target node, v any) error {
	var path []node
	x := target
	for {
		switch t := x.(type) {
		case *varNode:
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			// Evaluate index expressions before taking the lock.
			keys := make([]any, len(path))
			for i, p := range path {
				switch pn := p.(type) {
				case *fieldNode:
					keys[i] = pn.name
				case *indexNode:
					k, err := pn.i.eval(f)
					if err != nil {
						return err
					}
					keys[i] = k
				}
			}
			return f.set(t.name, func(old any, exists bool) (any, error) {
				if len(keys) == 0 {
					return v, nil
				}
				if !exists {
					old = nil
				}
				return setPath(old, keys, v)
			})
		case *fieldNode:
			path = append(path, t)
			x = t.x
		case *indexNode:
			path = append(path, t)
			x = t.x
		default:
			return raiseErr("TypeError", "cannot assign to this expression")
		}
	}
}

func setPath(cur any, keys []any, v any) (any, error) {
	if len(keys) == 0 {
		return v, nil
	}
	switch k := keys[0].(type) {
	case string:
		var m map[string]any
		switch c := cur.(type) {
		case map[string]any:
			m = make(map[string]any, len(c)+1)
			for a, b := range c {
				m[a] = b
			}
		case nil:
			// A missing map on the path is created, as Google's
			// assignment to a nested key does.
			m = map[string]any{}
		default:
			return nil, raiseErr("TypeError", "cannot set key '%s' of a value of type %s", k, typeName(cur))
		}
		next, err := setPath(m[k], keys[1:], v)
		if err != nil {
			return nil, err
		}
		m[k] = next
		return m, nil
	case int64:
		l, ok := cur.([]any)
		if !ok {
			return nil, raiseErr("TypeError", "cannot set an element of a value of type %s", typeName(cur))
		}
		if k < 0 || k >= int64(len(l)) {
			return nil, raiseErr("IndexError", "list assignment index out of range")
		}
		n := append([]any(nil), l...)
		next, err := setPath(n[k], keys[1:], v)
		if err != nil {
			return nil, err
		}
		n[k] = next
		return n, nil
	}
	return nil, raiseErr("TypeError", "a key must be a string or an integer, not %s", typeName(keys[0]))
}

// callExpr calls a function from an expression.
func (f *frame) callExpr(name string, args []any) (any, error) {
	if r := f.x.prog.routines[name]; r != nil {
		return f.callRoutine(r, args, nil)
	}
	d := stdlib[name]
	if d == nil {
		return nil, raiseErr("KeyError", "function '%s' not found", name)
	}
	bound, err := d.bind(name, args, nil)
	if err != nil {
		return nil, err
	}
	return d.call(f, bound)
}

// callValue calls a function named by a value, such as a retry predicate.
func (f *frame) callValue(fnv any, args []any) (any, error) {
	ref, ok := fnv.(funcRef)
	if !ok {
		if s, isStr := fnv.(string); isStr {
			ref = funcRef{name: s}
		} else {
			return nil, raiseErr("TypeError", "a predicate must be a function or a subworkflow, not %s", typeName(fnv))
		}
	}
	return f.callExpr(ref.name, args)
}

func (f *frame) callRoutine(r *routine, positional []any, named map[string]any) (any, error) {
	if f.depth+1 > MaxCallDepth {
		return nil, raiseErr("RecursionError", "maximum call stack depth of %d exceeded", MaxCallDepth)
	}
	g := &frame{x: f.x, routine: r, depth: f.depth + 1, parDepth: f.parDepth, thread: f.thread, parent: f.parent, locked: f.locked,
		sc: &scope{vars: map[string]any{}}, caller: f.pendingCall}
	if named == nil && len(positional) > len(r.params) {
		return nil, raiseErr("TypeError", "%s takes %d arguments, not %d", r.name, len(r.params), len(positional))
	}
	known := map[string]bool{}
	for _, p := range r.params {
		known[p.name] = true
	}
	for k := range named {
		if !known[k] {
			return nil, raiseErr("TypeError", "%s got an unexpected argument %q", r.name, k)
		}
	}
	for i, p := range r.params {
		var v any
		var ok bool
		if named != nil {
			v, ok = named[p.name]
		} else if i < len(positional) {
			v, ok = positional[i], true
		}
		if !ok {
			if !p.hasDef {
				return nil, raiseErr("TypeError", "%s is missing the argument %q", r.name, p.name)
			}
			d, err := p.def.eval(g)
			if err != nil {
				return nil, err
			}
			v = d
		}
		g.sc.vars[p.name] = v
	}
	c, err := g.runSteps(r.steps)
	if err != nil {
		return nil, err
	}
	switch c.kind {
	case cReturn:
		return c.val, nil
	case cNone, cEnd:
		return nil, nil
	}
	return nil, raiseErr("SystemError", "jump %q left workflow %s", c.target, r.name)
}

func (f *frame) runSteps(steps []*step) (ctrl, error) {
	for i := 0; i < len(steps); {
		c, err := f.runStep(steps[i])
		if err != nil {
			return ctrl{}, err
		}
		switch c.kind {
		case cNone:
			i++
		case cNext:
			j := indexOf(steps, c.target)
			if j < 0 {
				return c, nil
			}
			i = j
		default:
			return c, nil
		}
	}
	return ctrl{}, nil
}

func indexOf(steps []*step, name string) int {
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	return -1
}

func (s *step) stepType() string {
	switch {
	case s.assign != nil:
		return "STEP_ASSIGN"
	case s.call != "":
		switch {
		case strings.HasPrefix(s.call, "googleapis."):
			return "STEP_CONNECTOR_CALL"
		case strings.Contains(s.call, "."):
			return "STEP_STD_LIB_CALL"
		}
		return "STEP_SUBWORKFLOW_CALL"
	case s.switchCases != nil:
		return "STEP_SWITCH"
	case s.loop != nil:
		return "STEP_FOR"
	case s.parallel != nil:
		if s.parallel.loop != nil {
			return "STEP_PARALLEL_FOR"
		}
		return "STEP_PARALLEL_BRANCH"
	case s.try != nil:
		return "STEP_TRY_RETRY_EXCEPT"
	case s.hasRet:
		return "STEP_RETURN"
	case s.raise != nil:
		return "STEP_RAISE"
	case s.steps != nil:
		return "STEP_TYPE_UNSPECIFIED"
	}
	return "STEP_GOTO"
}

func (x *machine) record(f *frame, s *step, typ string) *StepEntry {
	x.historyMu.Lock()
	defer x.historyMu.Unlock()
	x.nextEntry++
	ts := x.now().UTC().Format(time.RFC3339Nano)
	e := &StepEntry{EntryID: x.nextEntry, Step: s.name, Routine: f.routine.name, StepType: typ, State: "STATE_IN_PROGRESS",
		CreateTime: ts, UpdateTime: ts, ThreadID: f.thread, Parent: f.parent}
	if x.maxHistory <= 0 || len(x.history) < x.maxHistory {
		x.history = append(x.history, e)
	}
	return e
}

func (x *machine) finish(e *StepEntry, err error, vars map[string]json.RawMessage) {
	x.historyMu.Lock()
	defer x.historyMu.Unlock()
	e.UpdateTime = x.now().UTC().Format(time.RFC3339Nano)
	if vars != nil {
		e.Variables = vars
	}
	switch {
	case err == nil:
		e.State = "STATE_SUCCEEDED"
	case errors.Is(err, context.Canceled):
		e.State = "STATE_CANCELLED"
	default:
		e.State = "STATE_FAILED"
		var we *wfError
		if errors.As(err, &we) {
			if b, jerr := encodeJSON(we.val, false); jerr == nil {
				e.Exception = string(b)
			}
		}
	}
}

// History is a copy of the step entries so far.
func (x *machine) History() []StepEntry {
	x.historyMu.Lock()
	defer x.historyMu.Unlock()
	out := make([]StepEntry, len(x.history))
	for i, e := range x.history {
		out[i] = *e
	}
	return out
}

func (x *machine) Current() (routine, step string) {
	x.currentMu.Lock()
	defer x.currentMu.Unlock()
	return x.current[0], x.current[1]
}

func (f *frame) runStep(s *step) (c ctrl, err error) {
	if err := f.x.ctx.Err(); err != nil {
		return ctrl{}, err
	}
	if n := f.x.steps.Add(1); n > MaxSteps {
		return ctrl{}, f.at(s, raiseErr("ResourceLimitError", "the execution exceeded the limit of %d steps", MaxSteps))
	}
	f.x.currentMu.Lock()
	f.x.current = [2]string{f.routine.name, s.name}
	f.x.currentMu.Unlock()
	entry := f.x.record(f, s, s.stepType())
	g := *f
	g.parent = entry.EntryID
	defer func() { f.x.finish(entry, err, g.variables()) }()
	c, err = g.body(s)
	if err != nil {
		return ctrl{}, f.at(s, err)
	}
	if c.kind == cNone && s.next != "" {
		return jump(s.next), nil
	}
	return c, nil
}

func jump(next string) ctrl {
	switch next {
	case "end":
		return ctrl{kind: cEnd}
	case "break":
		return ctrl{kind: cBreak}
	case "continue":
		return ctrl{kind: cContinue}
	}
	return ctrl{kind: cNext, target: next}
}

// at records where an error was first raised.
func (f *frame) at(s *step, err error) error {
	var we *wfError
	if errors.As(err, &we) && we.step == "" {
		we.step, we.routine, we.line, we.col = s.name, f.routine.name, s.line, s.col
	}
	return err
}

// body runs a step's action, without its next.
func (f *frame) body(s *step) (ctrl, error) {
	switch {
	case s.assign != nil:
		if f.parDepth > 0 && !f.locked {
			f.x.assignMu.Lock()
			defer f.x.assignMu.Unlock()
			g := *f
			g.locked = true
			f = &g
		}
		for _, a := range s.assign {
			v, err := a.value.eval(f)
			if err != nil {
				return ctrl{}, err
			}
			if err := f.assignTo(a.target, v); err != nil {
				return ctrl{}, err
			}
		}
		if f.memory() > MaxMemoryBytes {
			return ctrl{}, raiseErr("ResourceLimitError", "Memory usage limit exceeded: variables are limited to %d bytes", MaxMemoryBytes)
		}
	case s.call != "":
		v, err := f.call(s)
		if err != nil {
			return ctrl{}, err
		}
		if s.result != "" {
			target, perr := parseTarget(s.result)
			if perr != nil {
				return ctrl{}, raiseErr("TypeError", "%v", perr)
			}
			if err := f.assignTo(target, v); err != nil {
				return ctrl{}, err
			}
			if f.memory() > MaxMemoryBytes {
				return ctrl{}, raiseErr("ResourceLimitError", "Memory usage limit exceeded: variables are limited to %d bytes", MaxMemoryBytes)
			}
		}
	case s.switchCases != nil:
		for _, sc := range s.switchCases {
			cv, err := sc.cond.eval(f)
			if err != nil {
				return ctrl{}, err
			}
			b, ok := cv.(bool)
			if !ok && cv != nil {
				return ctrl{}, raiseErr("TypeError", "a switch condition must be a boolean, not %s", typeName(cv))
			}
			if b {
				c, err := f.body(sc.body)
				if err != nil {
					return ctrl{}, err
				}
				if c.kind == cNone && sc.body.next != "" {
					return jump(sc.body.next), nil
				}
				return c, nil
			}
		}
	case s.hasRet:
		v, err := s.ret.eval(f)
		if err != nil {
			return ctrl{}, err
		}
		return ctrl{kind: cReturn, val: v}, nil
	case s.raise != nil:
		v, err := s.raise.eval(f)
		if err != nil {
			return ctrl{}, err
		}
		return ctrl{}, &wfError{val: v}
	case s.loop != nil:
		return f.runLoop(s.loop)
	case s.parallel != nil:
		return ctrl{}, f.runParallel(s.parallel)
	case s.try != nil:
		return f.runTry(s)
	case s.steps != nil:
		return f.runSteps(s.steps)
	}
	return ctrl{}, nil
}

// iterations are a for's values: a list, or a range's integers or
// doubles from start to end inclusive.
func (f *frame) iterations(l *loopSpec) ([]any, error) {
	if l.in != nil {
		v, err := l.in.eval(f)
		if err != nil {
			return nil, err
		}
		list, ok := v.([]any)
		if !ok {
			return nil, raiseErr("TypeError", "for in needs a list, not %s", typeName(v))
		}
		return list, nil
	}
	v, err := l.rng.eval(f)
	if err != nil {
		return nil, err
	}
	r, ok := v.([]any)
	if !ok || len(r) != 2 {
		return nil, raiseErr("TypeError", "range must be a list of a start and an end")
	}
	si, sInt := r[0].(int64)
	ei, eInt := r[1].(int64)
	if sInt && eInt {
		if ei-si >= MaxSteps {
			return nil, raiseErr("ResourceLimitError", "the range has more than %d values", MaxSteps)
		}
		var out []any
		for i := si; i <= ei; i++ {
			out = append(out, i)
		}
		return out, nil
	}
	sf, ok1 := toFloat(r[0])
	ef, ok2 := toFloat(r[1])
	if !ok1 || !ok2 {
		return nil, raiseErr("TypeError", "range bounds must be numbers")
	}
	if ef-sf >= MaxSteps {
		return nil, raiseErr("ResourceLimitError", "the range has more than %d values", MaxSteps)
	}
	var out []any
	for x := sf; x <= ef; x++ {
		out = append(out, x)
	}
	return out, nil
}

func (f *frame) runLoop(l *loopSpec) (ctrl, error) {
	items, err := f.iterations(l)
	if err != nil {
		return ctrl{}, err
	}
	for i, it := range items {
		f.x.progress(f.parent, i+1, len(items))
		g := f.child(false, nil)
		g.sc.vars[l.value] = it
		if l.index != "" {
			g.sc.vars[l.index] = int64(i)
		}
		c, err := g.runSteps(l.steps)
		if err != nil {
			return ctrl{}, err
		}
		switch c.kind {
		case cBreak:
			return ctrl{}, nil
		case cNone, cContinue:
		case cNext:
			return ctrl{}, raiseErr("SystemError", "jump %q leaves a for loop", c.target)
		default:
			return c, nil
		}
	}
	return ctrl{}, nil
}

func (f *frame) runParallel(p *parallelSpec) error {
	if f.parDepth+1 > MaxParallelDepth {
		return raiseErr("ParallelNestingError", "parallel steps nest deeper than %d", MaxParallelDepth)
	}
	for _, s := range p.shared {
		if _, ok := f.lookup(s); !ok {
			return raiseErr("KeyError", "shared variable '%s' is not defined before the parallel step", s)
		}
	}
	type job struct {
		id    string
		steps []*step
		vars  map[string]any
	}
	var jobs []job
	if p.loop != nil {
		items, err := f.iterations(p.loop)
		if err != nil {
			return err
		}
		for i, it := range items {
			vars := map[string]any{p.loop.value: it}
			if p.loop.index != "" {
				vars[p.loop.index] = int64(i)
			}
			jobs = append(jobs, job{id: strconv.Itoa(i), steps: p.loop.steps, vars: vars})
		}
	} else {
		for _, b := range p.branches {
			jobs = append(jobs, job{id: b.name, steps: b.steps})
		}
	}
	limit := len(jobs)
	if p.concurrency != nil {
		v, err := p.concurrency.eval(f)
		if err != nil {
			return err
		}
		n, ok := v.(int64)
		if !ok || n < 1 {
			return raiseErr("ValueError", "concurrency_limit must be a positive integer, not %v", v)
		}
		if int(n) < limit {
			limit = int(n)
		}
	}
	local := make(chan struct{}, max(limit, 1))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []any
		stop     atomic.Bool
		fatal    error
	)
	for _, j := range jobs {
		if stop.Load() {
			break
		}
		select {
		case local <- struct{}{}:
		case <-f.x.ctx.Done():
			wg.Wait()
			return f.x.ctx.Err()
		}
		select {
		case f.x.sem <- struct{}{}:
		case <-f.x.ctx.Done():
			<-local
			wg.Wait()
			return f.x.ctx.Err()
		}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-f.x.sem; <-local }()
			g := f.child(true, p.shared)
			g.parDepth = f.parDepth + 1
			g.thread = strings.TrimPrefix(f.thread+"."+j.id, ".")
			for k, v := range j.vars {
				g.sc.vars[k] = v
			}
			c, err := g.runSteps(j.steps)
			if err == nil {
				switch c.kind {
				case cBreak:
					stop.Store(true)
				case cReturn:
					err = raiseErr("SystemError", "return is not allowed in a parallel branch or iteration; set a shared variable")
				case cNext:
					err = raiseErr("SystemError", "jump %q leaves a parallel branch", c.target)
				}
			}
			if err == nil {
				return
			}
			var we *wfError
			if !errors.As(err, &we) {
				mu.Lock()
				fatal = err
				mu.Unlock()
				return
			}
			if f.x.unhandled.Add(1) > MaxUnhandledExceptions {
				mu.Lock()
				fatal = raiseErr("ResourceLimitError", "more than %d unhandled exceptions in parallel branches", MaxUnhandledExceptions)
				mu.Unlock()
				return
			}
			mu.Lock()
			failures = append(failures, map[string]any{"id": j.id, "error": map[string]any{"context": we.context(), "payload": we.val}})
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	if fatal != nil {
		return fatal
	}
	if len(failures) > 0 {
		return &wfError{val: map[string]any{
			"message":   "UnhandledBranchError: One or more branches or iterations encountered an unhandled runtime error",
			"tags":      []any{"UnhandledBranchError"},
			"branches":  failures,
			"truncated": false,
		}}
	}
	return nil
}

// runTry runs try with its retry policy and except.
func (f *frame) runTry(s *step) (ctrl, error) {
	var policy map[string]any
	if s.retry != nil {
		v, err := s.retry.eval(f)
		if err != nil {
			return ctrl{}, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return ctrl{}, raiseErr("TypeError", "retry must be a policy map, not %s", typeName(v))
		}
		policy = m
	}
	maxRetries, delay, maxDelay, mult := int64(0), 1.0, 60.0, 1.25
	var predicate any
	if policy != nil {
		predicate = policy["predicate"]
		if predicate == nil {
			return ctrl{}, raiseErr("ValueError", "a retry policy needs a predicate")
		}
		n, ok := policy["max_retries"].(int64)
		if !ok || n < 0 {
			return ctrl{}, raiseErr("ValueError", "max_retries must be a non-negative integer")
		}
		maxRetries = n
		if b, ok := policy["backoff"].(map[string]any); ok {
			if d, ok := toFloat(b["initial_delay"]); ok {
				delay = d
			}
			if d, ok := toFloat(b["max_delay"]); ok {
				maxDelay = d
			}
			if m, ok := toFloat(b["multiplier"]); ok {
				mult = m
			}
		} else if policy["backoff"] != nil {
			return ctrl{}, raiseErr("TypeError", "backoff must be a map of initial_delay, max_delay and multiplier")
		}
	}
	for attempt := int64(0); ; attempt++ {
		body := *s.try
		body.next = ""
		c, err := f.body(&body)
		if err == nil {
			if s.try.next != "" && c.kind == cNone {
				return jump(s.try.next), nil
			}
			return c, nil
		}
		var we *wfError
		if !errors.As(err, &we) {
			return ctrl{}, err
		}
		if policy != nil && attempt < maxRetries {
			ok, perr := f.callValue(predicate, []any{we.val})
			if perr != nil {
				return ctrl{}, perr
			}
			if b, _ := ok.(bool); b {
				wait := math.Min(delay*math.Pow(mult, float64(attempt)), maxDelay)
				if err := f.sleepFor(time.Duration(wait * float64(time.Second))); err != nil {
					return ctrl{}, err
				}
				continue
			}
		}
		if s.except == nil {
			return ctrl{}, err
		}
		f.logHandled(we)
		if s.except.as != "" {
			if err := f.set(s.except.as, func(any, bool) (any, error) { return we.val, nil }); err != nil {
				return ctrl{}, err
			}
		}
		return f.runSteps(s.except.steps)
	}
}

func (x *machine) sleep(d time.Duration) error {
	if d <= 0 {
		return x.ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-x.ctx.Done():
		return x.ctx.Err()
	}
}

// call runs a call step, call-logged at the execution's level.
func (f *frame) call(s *step) (any, error) {
	named := map[string]any{}
	for _, a := range s.args {
		v, err := a.value.eval(f)
		if err != nil {
			return nil, err
		}
		named[a.name] = v
	}
	lc := f.logBegun(s, named)
	g := *f
	g.pendingCall = lc
	v, err := g.callBody(s, named)
	f.logEnd(lc, v, err)
	return v, err
}

func (f *frame) callBody(s *step, named map[string]any) (any, error) {
	name := s.call
	if r := f.x.prog.routines[name]; r != nil {
		return f.callRoutine(r, nil, named)
	}
	switch {
	case strings.HasPrefix(name, "http."):
		if stdlib[name] != nil {
			break
		}
		return f.x.effect(f, "http", func() (any, error) {
			return f.x.rt.HTTP().httpStep(f.x.ctx, strings.TrimPrefix(name, "http."), named)
		})
	case name == "googleapis.workflowexecutions.v1.projects.locations.workflows.executions.run":
		// The connector's run helper, which experimental.executions.run's
		// page points to: start an execution and wait for its result.
		args := map[string]any{}
		for k, v := range named {
			if k != "connector_params" {
				args[k] = v
			}
		}
		return f.experimental("run", args)
	case strings.HasPrefix(name, "googleapis."):
		return f.x.effect(f, "connector", func() (any, error) {
			return f.x.rt.HTTP().connector(f.x.ctx, name, named)
		})
	case strings.HasPrefix(name, "experimental.executions."):
		return f.experimental(strings.TrimPrefix(name, "experimental.executions."), named)
	}
	switch name {
	case "sys.sleep":
		sec, ok := toFloat(named["seconds"])
		if !ok || sec < 0 {
			return nil, raiseErr("TypeError", "sys.sleep needs seconds, a non-negative number")
		}
		return nil, f.sleepFor(time.Duration(sec * float64(time.Second)))
	case "sys.sleep_until":
		ts, err := str(named["time"], "time")
		if err != nil {
			return nil, err
		}
		t, perr := time.Parse(time.RFC3339Nano, ts)
		if perr != nil {
			return nil, raiseErr("ValueError", "%q is not an RFC 3339 timestamp", ts)
		}
		return nil, f.x.sleep(time.Until(t))
	case "sys.log":
		sev, _ := named["severity"].(string)
		if sev == "" {
			sev = "DEFAULT"
		}
		payload := named["data"]
		if payload == nil {
			if t, ok := named["text"]; ok {
				payload = t
			} else {
				payload = named["json"]
			}
		}
		if err := checkEncodable(payload); err != nil {
			return nil, err
		}
		_, err := f.x.effect(f, "log", func() (any, error) {
			f.x.rt.Log(sev, payload)
			return nil, nil
		})
		return nil, err
	case "events.await_callback":
		return f.x.effect(f, "await", func() (any, error) { return f.x.awaitCallback(named) })
	}
	d := stdlib[name]
	if d == nil {
		return nil, raiseErr("KeyError", "function '%s' not found", name)
	}
	bound, err := d.bind(name, nil, named)
	if err != nil {
		return nil, err
	}
	return d.call(f, bound)
}

// Run executes the program's main with the argument (JSON, "" for none)
// and returns the result as JSON, or the error.
func (x *machine) run(argument string) (string, error) {
	main := x.prog.routines["main"]
	var args []any
	if len(main.params) == 1 {
		var arg any
		if strings.TrimSpace(argument) != "" {
			v, err := decodeJSON([]byte(argument))
			if err != nil {
				return "", raiseErr("ValueError", "the argument is not valid JSON: %v", err)
			}
			arg = v
		}
		args = []any{arg}
	}
	f := &frame{x: x, sc: &scope{vars: map[string]any{}}}
	v, err := f.callRoutine(main, args, nil)
	if err != nil {
		return "", err
	}
	b, err := encodeJSON(v, false)
	if err != nil {
		return "", err
	}
	if len(b) > MaxMemoryBytes {
		return "", raiseErr("ResourceLimitError", "the result is %d bytes; the limit is %d", len(b), MaxMemoryBytes)
	}
	return string(b), nil
}

func errorPayload(err error) (payload, context string) {
	var we *wfError
	if errors.As(err, &we) {
		b, jerr := encodeJSON(we.val, false)
		if jerr != nil {
			b = []byte(fmt.Sprintf("%q", we.Error()))
		}
		return string(b), we.context()
	}
	return fmt.Sprintf("%q", err.Error()), "RuntimeError: " + err.Error()
}

// newMachine prepares one execution of prog. base is the execution's
// resource name; callbackURL makes a callback endpoint's URL from its name.
func newMachine(ctx context.Context, prog *program, rt Runtime, env map[string]any, base string, callbackURL func(string) string) *machine {
	if callbackURL == nil {
		callbackURL = func(name string) string { return "http://localhost/v1/" + name }
	}
	x := &machine{ctx: ctx, prog: prog, rt: rt, env: env, now: time.Now, sem: make(chan struct{}, MaxConcurrentBranches),
		maxHistory: 10000}
	x.callbacks = &callbackSet{byID: map[string]*callback{}, base: base, url: callbackURL, now: x.now}
	return x
}
