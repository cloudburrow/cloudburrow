package workflows

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// A program is a deployed workflow's source, parsed as Google's syntax
// reference (docs/reference/syntax) describes it: a list of steps, which is
// the main workflow, or a map of main and subworkflows, each with params and
// steps. YAML and JSON are both accepted, as JSON is YAML.
type program struct {
	routines map[string]*routine
}

type routine struct {
	name   string
	params []param
	steps  []*step
}

type param struct {
	name   string
	def    node
	hasDef bool
}

type assignment struct {
	target node // a variable, field or element: the left-hand side
	text   string
	value  node
}

type step struct {
	name      string
	line, col int

	assign []assignment
	call   string
	args   []namedArg
	result string

	switchCases []*switchCase
	next        string

	ret    node
	hasRet bool
	raise  node

	loop     *loopSpec
	parallel *parallelSpec

	try    *step
	retry  node
	except *exceptSpec

	steps []*step
}

type namedArg struct {
	name  string
	value node
}

type switchCase struct {
	cond node
	body *step // the case's next, steps, assign, call, return or raise
	line int
}

type loopSpec struct {
	value, index string
	in           node
	rng          node
	steps        []*step
}

type parallelSpec struct {
	shared      []string
	concurrency node
	policy      string
	branches    []*branch
	loop        *loopSpec
}

type branch struct {
	name  string
	steps []*step
}

type exceptSpec struct {
	as    string
	steps []*step
}

// SyntaxError is a source Google would refuse to deploy.
type SyntaxError struct {
	Line, Col int
	Msg       string
}

func (e *SyntaxError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("parse error: in line %d, column %d: %s", e.Line, e.Col, e.Msg)
	}
	return "parse error: " + e.Msg
}

func synErr(n *yaml.Node, format string, args ...any) error {
	e := &SyntaxError{Msg: fmt.Sprintf(format, args...)}
	if n != nil {
		e.Line, e.Col = n.Line, n.Column
	}
	return e
}

// Parse parses and checks a workflow's source.
func Parse(src string) (*program, error) {
	if len(src) > MaxSourceBytes {
		return nil, &SyntaxError{Msg: fmt.Sprintf("the source is %d bytes; the limit is %d", len(src), MaxSourceBytes)}
	}
	if strings.TrimSpace(src) == "" {
		return nil, &SyntaxError{Msg: "the source code is empty; a workflow needs at least one step"}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, &SyntaxError{Msg: err.Error()}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, &SyntaxError{Msg: "the source is not a list of steps or a map of workflows"}
	}
	root := doc.Content[0]
	p := &program{routines: map[string]*routine{}}
	switch root.Kind {
	case yaml.SequenceNode:
		steps, err := parseSteps(root)
		if err != nil {
			return nil, err
		}
		p.routines["main"] = &routine{name: "main", steps: steps, params: nil}
	case yaml.MappingNode:
		for i := 0; i+1 < len(root.Content); i += 2 {
			name, body := root.Content[i].Value, root.Content[i+1]
			r, err := parseRoutine(name, body)
			if err != nil {
				return nil, err
			}
			p.routines[name] = r
		}
		if p.routines["main"] == nil {
			return nil, synErr(root, "the workflow has no main: a map of workflows must define main")
		}
		if len(p.routines["main"].params) > 1 {
			return nil, synErr(root, "main takes at most one parameter, the execution's argument")
		}
	default:
		return nil, synErr(root, "the source is not a list of steps or a map of workflows")
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	return p, nil
}

func parseRoutine(name string, n *yaml.Node) (*routine, error) {
	if n.Kind != yaml.MappingNode {
		return nil, synErr(n, "workflow %q must be a map with params and steps", name)
	}
	r := &routine{name: name}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "params":
			if v.Kind != yaml.SequenceNode {
				return nil, synErr(v, "params of %q must be a list", name)
			}
			for _, pn := range v.Content {
				switch pn.Kind {
				case yaml.ScalarNode:
					r.params = append(r.params, param{name: pn.Value})
				case yaml.MappingNode:
					if len(pn.Content) != 2 {
						return nil, synErr(pn, "a parameter with a default is one name: value")
					}
					def, err := compileValue(pn.Content[1])
					if err != nil {
						return nil, err
					}
					r.params = append(r.params, param{name: pn.Content[0].Value, def: def, hasDef: true})
				default:
					return nil, synErr(pn, "a parameter is a name, or a name and its default")
				}
			}
		case "steps":
			steps, err := parseSteps(v)
			if err != nil {
				return nil, err
			}
			r.steps = steps
		default:
			return nil, synErr(n.Content[i], "unexpected key %q in workflow %q: expected params or steps", k, name)
		}
	}
	if len(r.steps) == 0 {
		return nil, synErr(n, "workflow %q has no steps", name)
	}
	return r, nil
}

func parseSteps(n *yaml.Node) ([]*step, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, synErr(n, "steps must be a list")
	}
	if len(n.Content) == 0 {
		return nil, synErr(n, "a list of steps cannot be empty")
	}
	var out []*step
	seen := map[string]bool{}
	for _, sn := range n.Content {
		if sn.Kind != yaml.MappingNode || len(sn.Content) != 2 {
			return nil, synErr(sn, "each step must be a map of its one name to its body")
		}
		name := sn.Content[0].Value
		if seen[name] {
			return nil, synErr(sn.Content[0], "duplicate step name %q", name)
		}
		seen[name] = true
		switch name {
		case "end", "break", "continue":
			return nil, synErr(sn.Content[0], "%q is reserved and cannot name a step", name)
		}
		s, err := parseStepBody(name, sn.Content[1])
		if err != nil {
			return nil, err
		}
		s.line, s.col = sn.Content[0].Line, sn.Content[0].Column
		out = append(out, s)
	}
	return out, nil
}

func parseStepBody(name string, n *yaml.Node) (*step, error) {
	s := &step{name: name, line: n.Line, col: n.Column}
	if n.Kind != yaml.MappingNode {
		return nil, synErr(n, "step %q must be a map", name)
	}
	keys := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if _, dup := keys[k]; dup {
			return nil, synErr(n.Content[i], "duplicate key %q in step %q", k, name)
		}
		keys[k] = n.Content[i+1]
	}
	kinds := 0
	for k, v := range keys {
		var err error
		switch k {
		case "assign":
			kinds++
			err = s.parseAssign(v)
		case "call":
			kinds++
			s.call = v.Value
			if v.Kind != yaml.ScalarNode || s.call == "" {
				err = synErr(v, "call in step %q must name a function or subworkflow", name)
			}
		case "args":
			if keys["call"] == nil {
				return nil, synErr(v, "args in step %q needs call", name)
			}
			if v.Kind != yaml.MappingNode {
				return nil, synErr(v, "args in step %q must be a map", name)
			}
			for i := 0; i+1 < len(v.Content); i += 2 {
				val, err := compileValue(v.Content[i+1])
				if err != nil {
					return nil, err
				}
				s.args = append(s.args, namedArg{name: v.Content[i].Value, value: val})
			}
		case "result":
			if keys["call"] == nil {
				return nil, synErr(v, "result in step %q needs call", name)
			}
			s.result = v.Value
		case "switch":
			kinds++
			err = s.parseSwitch(v)
		case "next":
			s.next = v.Value
			if v.Kind != yaml.ScalarNode || s.next == "" {
				err = synErr(v, "next in step %q must name a step", name)
			}
		case "return":
			kinds++
			s.hasRet = true
			s.ret, err = compileValue(v)
		case "raise":
			kinds++
			s.raise, err = compileValue(v)
		case "for":
			kinds++
			s.loop, err = parseLoop(v)
		case "parallel":
			kinds++
			s.parallel, err = parseParallel(v)
		case "try":
			kinds++
			s.try, err = parseTryBody(name, v)
		case "retry":
			if keys["try"] == nil {
				return nil, synErr(v, "retry in step %q needs try", name)
			}
			s.retry, err = compileValue(v)
		case "except":
			if keys["try"] == nil {
				return nil, synErr(v, "except in step %q needs try", name)
			}
			s.except, err = parseExcept(v)
		case "steps":
			kinds++
			s.steps, err = parseSteps(v)
		default:
			return nil, synErr(n, "unexpected key %q in step %q", k, name)
		}
		if err != nil {
			return nil, err
		}
	}
	if kinds > 1 {
		return nil, synErr(n, "step %q has more than one of assign, call, switch, return, raise, for, parallel, try and steps", name)
	}
	if kinds == 0 && s.next == "" {
		return nil, synErr(n, "step %q does nothing: it needs assign, call, switch, return, raise, for, parallel, try, steps or next", name)
	}
	return s, nil
}

func (s *step) parseAssign(v *yaml.Node) error {
	if v.Kind != yaml.SequenceNode || len(v.Content) == 0 {
		return synErr(v, "assign in step %q must be a list of name: value", s.name)
	}
	if len(v.Content) > MaxAssignments {
		return synErr(v, "step %q makes %d assignments; the limit is %d", s.name, len(v.Content), MaxAssignments)
	}
	for _, a := range v.Content {
		if a.Kind != yaml.MappingNode || len(a.Content) != 2 {
			return synErr(a, "each assignment in step %q is one name: value", s.name)
		}
		lhs := a.Content[0].Value
		target, err := parseTarget(lhs)
		if err != nil {
			return synErr(a.Content[0], "%v", err)
		}
		val, err := compileValue(a.Content[1])
		if err != nil {
			return err
		}
		s.assign = append(s.assign, assignment{target: target, text: lhs, value: val})
	}
	return nil
}

// parseTarget is an assignment's left-hand side: a variable, then fields
// and indexes, such as my_map.key, my_map["key"] or my_list[i].
func parseTarget(lhs string) (node, error) {
	src := lhs
	if isExpr(src) {
		src = strings.TrimSpace(src)
		src = src[2 : len(src)-1]
	}
	toks, err := lex(src)
	if err != nil {
		return nil, fmt.Errorf("bad assignment target %q: %v", lhs, err)
	}
	p := &parser{toks: toks}
	n, err := p.postfix()
	if err == nil && p.pos < len(p.toks) {
		err = fmt.Errorf("unexpected %q", p.toks[p.pos].text)
	}
	if err != nil {
		return nil, fmt.Errorf("bad assignment target %q: %v", lhs, err)
	}
	for x := n; ; {
		switch t := x.(type) {
		case *varNode:
			return n, nil
		case *fieldNode:
			x = t.x
		case *indexNode:
			x = t.x
		default:
			return nil, fmt.Errorf("bad assignment target %q", lhs)
		}
	}
}

func (s *step) parseSwitch(v *yaml.Node) error {
	if v.Kind != yaml.SequenceNode || len(v.Content) == 0 {
		return synErr(v, "switch in step %q must be a list of conditions", s.name)
	}
	if len(v.Content) > MaxConditions {
		return synErr(v, "step %q has %d conditions; the limit is %d", s.name, len(v.Content), MaxConditions)
	}
	for _, c := range v.Content {
		if c.Kind != yaml.MappingNode {
			return synErr(c, "each condition in step %q must be a map", s.name)
		}
		sc := &switchCase{line: c.Line}
		body := &yaml.Node{Kind: yaml.MappingNode, Line: c.Line, Column: c.Column}
		for i := 0; i+1 < len(c.Content); i += 2 {
			if c.Content[i].Value == "condition" {
				cond, err := compileValue(c.Content[i+1])
				if err != nil {
					return err
				}
				sc.cond = cond
				continue
			}
			body.Content = append(body.Content, c.Content[i], c.Content[i+1])
		}
		if sc.cond == nil {
			return synErr(c, "a condition in step %q has no condition", s.name)
		}
		b, err := parseStepBody(s.name, body)
		if err != nil {
			return err
		}
		sc.body = b
		s.switchCases = append(s.switchCases, sc)
	}
	return nil
}

func parseLoop(v *yaml.Node) (*loopSpec, error) {
	if v.Kind != yaml.MappingNode {
		return nil, synErr(v, "for must be a map of value, in or range, and steps")
	}
	l := &loopSpec{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, w := v.Content[i].Value, v.Content[i+1]
		var err error
		switch k {
		case "value":
			l.value = w.Value
		case "index":
			l.index = w.Value
		case "in":
			l.in, err = compileValue(w)
		case "range":
			l.rng, err = compileValue(w)
		case "steps":
			l.steps, err = parseSteps(w)
		default:
			return nil, synErr(v.Content[i], "unexpected key %q in for", k)
		}
		if err != nil {
			return nil, err
		}
	}
	if l.value == "" {
		return nil, synErr(v, "for needs value")
	}
	if (l.in == nil) == (l.rng == nil) {
		return nil, synErr(v, "for needs exactly one of in and range")
	}
	if l.rng != nil && l.index != "" {
		return nil, synErr(v, "for over a range takes no index")
	}
	if len(l.steps) == 0 {
		return nil, synErr(v, "for needs steps")
	}
	return l, nil
}

func parseParallel(v *yaml.Node) (*parallelSpec, error) {
	if v.Kind != yaml.MappingNode {
		return nil, synErr(v, "parallel must be a map")
	}
	p := &parallelSpec{policy: "continueAll"}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, w := v.Content[i].Value, v.Content[i+1]
		var err error
		switch k {
		case "shared":
			if w.Kind != yaml.SequenceNode {
				return nil, synErr(w, "shared must be a list of variable names")
			}
			for _, s := range w.Content {
				p.shared = append(p.shared, s.Value)
			}
		case "concurrency_limit":
			p.concurrency, err = compileValue(w)
		case "exception_policy":
			if w.Value != "continueAll" {
				return nil, synErr(w, "exception_policy %q is not supported: continueAll is the only policy", w.Value)
			}
			p.policy = w.Value
		case "branches":
			if w.Kind != yaml.SequenceNode {
				return nil, synErr(w, "branches must be a list")
			}
			for _, b := range w.Content {
				if b.Kind != yaml.MappingNode || len(b.Content) != 2 {
					return nil, synErr(b, "each branch is its one name: steps")
				}
				body := b.Content[1]
				var stepsNode *yaml.Node
				if body.Kind == yaml.MappingNode {
					for j := 0; j+1 < len(body.Content); j += 2 {
						if body.Content[j].Value == "steps" {
							stepsNode = body.Content[j+1]
						} else {
							return nil, synErr(body.Content[j], "unexpected key %q in branch %q", body.Content[j].Value, b.Content[0].Value)
						}
					}
				}
				if stepsNode == nil {
					return nil, synErr(b, "branch %q needs steps", b.Content[0].Value)
				}
				steps, err := parseSteps(stepsNode)
				if err != nil {
					return nil, err
				}
				p.branches = append(p.branches, &branch{name: b.Content[0].Value, steps: steps})
			}
		case "for":
			p.loop, err = parseLoop(w)
		default:
			return nil, synErr(v.Content[i], "unexpected key %q in parallel", k)
		}
		if err != nil {
			return nil, err
		}
	}
	if (len(p.branches) == 0) == (p.loop == nil) {
		return nil, synErr(v, "parallel needs exactly one of branches and for")
	}
	if p.loop == nil && (len(p.branches) < 2 || len(p.branches) > MaxBranches) {
		return nil, synErr(v, "a parallel step has %d branches; it needs 2 to %d", len(p.branches), MaxBranches)
	}
	return p, nil
}

func parseTryBody(name string, v *yaml.Node) (*step, error) {
	if v.Kind != yaml.MappingNode {
		return nil, synErr(v, "try in step %q must be a map", name)
	}
	return parseStepBody(name, v)
}

func parseExcept(v *yaml.Node) (*exceptSpec, error) {
	if v.Kind != yaml.MappingNode {
		return nil, synErr(v, "except must be a map of as and steps")
	}
	e := &exceptSpec{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, w := v.Content[i].Value, v.Content[i+1]
		switch k {
		case "as":
			e.as = w.Value
		case "steps":
			steps, err := parseSteps(w)
			if err != nil {
				return nil, err
			}
			e.steps = steps
		default:
			return nil, synErr(v.Content[i], "unexpected key %q in except", k)
		}
	}
	if len(e.steps) == 0 {
		return nil, synErr(v, "except needs steps")
	}
	return e, nil
}

// compileValue compiles a YAML value whose strings of the form ${...} are
// expressions.
func compileValue(n *yaml.Node) (node, error) {
	switch n.Kind {
	case yaml.AliasNode:
		return compileValue(n.Alias)
	case yaml.ScalarNode:
		if n.Tag == "!!str" || n.Style != 0 {
			if isExpr(n.Value) {
				e, err := parseExpr(n.Value)
				if err != nil {
					return nil, synErr(n, "%v", err)
				}
				return e, nil
			}
			return &litNode{v: n.Value}, nil
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, synErr(n, "%v", err)
		}
		switch x := v.(type) {
		case int:
			return &litNode{v: int64(x)}, nil
		case int64:
			return &litNode{v: x}, nil
		case uint64:
			return nil, synErr(n, "integer %s is out of range", n.Value)
		case float64:
			return &litNode{v: x}, nil
		case string:
			if isExpr(x) {
				e, err := parseExpr(x)
				if err != nil {
					return nil, synErr(n, "%v", err)
				}
				return e, nil
			}
		}
		return &litNode{v: v}, nil
	case yaml.SequenceNode:
		l := &listNode{}
		for _, c := range n.Content {
			v, err := compileValue(c)
			if err != nil {
				return nil, err
			}
			l.items = append(l.items, v)
		}
		return l, nil
	case yaml.MappingNode:
		m := &mapNode{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			v, err := compileValue(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m.keys = append(m.keys, &litNode{v: n.Content[i].Value})
			m.vals = append(m.vals, v)
		}
		return m, nil
	}
	return nil, synErr(n, "unexpected value")
}

// check validates calls and jumps, as Google does at deployment.
func (p *program) check() error {
	for _, r := range p.routines {
		if err := p.checkSteps(r, r.steps, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

// checkSteps checks a list of steps; outer are the names reachable by a
// jump from it (enclosing lists up to a loop, branch or routine).
func (p *program) checkSteps(r *routine, steps []*step, outer []map[string]bool, parDepth int) error {
	names := map[string]bool{}
	for _, s := range steps {
		names[s.name] = true
	}
	scopes := append([]map[string]bool{names}, outer...)
	reachable := func(target string, inLoop bool) bool {
		switch target {
		case "end":
			return true
		case "break", "continue":
			return inLoop
		}
		for _, sc := range scopes {
			if sc[target] {
				return true
			}
		}
		return false
	}
	_ = reachable
	var visit func(s *step, inLoop bool) error
	visit = func(s *step, inLoop bool) error {
		bad := func(format string, args ...any) error {
			return &SyntaxError{Line: s.line, Col: s.col, Msg: fmt.Sprintf("in workflow %q, step %q: ", r.name, s.name) + fmt.Sprintf(format, args...)}
		}
		if s.next != "" && !reachable(s.next, inLoop) {
			return bad("next %q names no step it can jump to", s.next)
		}
		if s.call != "" {
			if err := p.checkCall(s.call); err != nil {
				return bad("%v", err)
			}
		}
		for _, c := range s.switchCases {
			if err := visit(c.body, inLoop); err != nil {
				return err
			}
		}
		if s.try != nil {
			if err := visit(s.try, inLoop); err != nil {
				return err
			}
		}
		if s.except != nil {
			if err := p.checkSteps(r, s.except.steps, scopes, parDepth); err != nil {
				return err
			}
		}
		if s.steps != nil {
			if err := p.checkSteps(r, s.steps, scopes, parDepth); err != nil {
				return err
			}
		}
		if s.loop != nil {
			if err := p.checkLoop(r, s.loop.steps, parDepth); err != nil {
				return err
			}
		}
		if s.parallel != nil {
			if parDepth+1 > MaxParallelDepth {
				return bad("parallel steps nest %d deep; the limit is %d", parDepth+1, MaxParallelDepth)
			}
			if s.parallel.loop != nil {
				if err := p.checkLoop(r, s.parallel.loop.steps, parDepth+1); err != nil {
					return err
				}
			}
			for _, b := range s.parallel.branches {
				if err := p.checkBranch(r, b.steps, parDepth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	inLoop := false
	for _, sc := range outer {
		if sc["\x00loop"] {
			inLoop = true
		}
	}
	if names["\x00loop"] {
		inLoop = true
	}
	for _, s := range steps {
		if err := visit(s, inLoop || loopMarker(outer)); err != nil {
			return err
		}
	}
	return nil
}

func loopMarker(outer []map[string]bool) bool {
	for _, sc := range outer {
		if sc["\x00loop"] {
			return true
		}
	}
	return false
}

// checkLoop checks a loop's body: jumps stay inside, and break and
// continue are allowed.
func (p *program) checkLoop(r *routine, steps []*step, parDepth int) error {
	return p.checkSteps(r, steps, []map[string]bool{{"\x00loop": true}}, parDepth)
}

// checkBranch checks a branch: next: continue ends it.
func (p *program) checkBranch(r *routine, steps []*step, parDepth int) error {
	return p.checkSteps(r, steps, []map[string]bool{{"\x00loop": true}}, parDepth)
}

// checkCall is whether a call step names something callable.
func (p *program) checkCall(name string) error {
	if p.routines[name] != nil {
		return nil
	}
	if callStep[name] || stdlib[name] != nil {
		return nil
	}
	if strings.HasPrefix(name, "googleapis.") {
		if len(strings.Split(name, ".")) < 4 {
			return fmt.Errorf("%q is not a connector: a connector is googleapis.SERVICE.VERSION.RESOURCE.METHOD", name)
		}
		return nil
	}
	return fmt.Errorf("call %q names no subworkflow and no standard library function", name)
}
