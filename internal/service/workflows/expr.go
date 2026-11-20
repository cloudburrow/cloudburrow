package workflows

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// The expression language of Google's syntax reference
// (docs/reference/syntax/expressions and datatypes): numbers, double-quoted
// strings, true/false/null in their three spellings, variables, map field
// and list element access, function calls and the operators below, in
// Google's precedence from lowest to highest:
//
//	or; and; <= >= < > == != in; + -; * / // %; unary - ; not; [] ; . ; ()
//
// An expression is a whole YAML value of the form ${...}; text around it is
// not interpolated, as Google's is not.

// MaxExpressionLength is "Expression length: 400 characters" (quotas).
const MaxExpressionLength = 400

type node interface {
	eval(f *frame) (any, error)
}

type (
	litNode   struct{ v any }
	varNode   struct{ name string }
	fieldNode struct {
		x    node
		name string
	}
	indexNode struct{ x, i node }
	callNode  struct {
		fn   string
		args []node
	}
	unaryNode struct {
		op string
		x  node
	}
	binNode struct {
		op   string
		l, r node
	}
	listNode struct{ items []node }
	mapNode  struct {
		keys []node
		vals []node
	}
)

// isExpr reports whether s is a whole ${...} expression.
func isExpr(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "${") && strings.HasSuffix(t, "}")
}

// parseExpr parses the text of a ${...} value.
func parseExpr(src string) (node, error) {
	t := strings.TrimSpace(src)
	if len(t) > MaxExpressionLength {
		return nil, fmt.Errorf("expression is %d characters long; the limit is %d", len(t), MaxExpressionLength)
	}
	body := t[2 : len(t)-1]
	toks, err := lex(body)
	if err != nil {
		return nil, fmt.Errorf("parse error in expression %q: %v", t, err)
	}
	p := &parser{toks: toks}
	n, err := p.expr()
	if err == nil && p.pos < len(p.toks) {
		err = fmt.Errorf("unexpected %q", p.toks[p.pos].text)
	}
	if err != nil {
		return nil, fmt.Errorf("parse error in expression %q: %v", t, err)
	}
	return n, nil
}

type token struct {
	kind byte // 'n' number, 's' string, 'i' identifier, 'o' operator/punctuation
	text string
	val  any
}

func lex(s string) ([]token, error) {
	var out []token
	r := []rune(s)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '"' || c == '\'':
			q := c
			var b strings.Builder
			j := i + 1
			for ; j < len(r) && r[j] != q; j++ {
				if r[j] == '\\' && j+1 < len(r) {
					j++
					switch r[j] {
					case 'n':
						b.WriteRune('\n')
					case 't':
						b.WriteRune('\t')
					case 'r':
						b.WriteRune('\r')
					case 'u':
						if j+4 < len(r) {
							if v, err := strconv.ParseUint(string(r[j+1:j+5]), 16, 32); err == nil {
								b.WriteRune(rune(v))
								j += 4
								continue
							}
						}
						b.WriteRune('u')
					default:
						b.WriteRune(r[j])
					}
					continue
				}
				b.WriteRune(r[j])
			}
			if j >= len(r) {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, token{kind: 's', text: string(r[i : j+1]), val: b.String()})
			i = j + 1
		case unicode.IsDigit(c) || (c == '.' && i+1 < len(r) && unicode.IsDigit(r[i+1])):
			j := i
			isFloat := false
			for j < len(r) && (unicode.IsDigit(r[j]) || r[j] == '.' || r[j] == 'e' || r[j] == 'E' ||
				((r[j] == '+' || r[j] == '-') && j > i && (r[j-1] == 'e' || r[j-1] == 'E'))) {
				if r[j] == '.' || r[j] == 'e' || r[j] == 'E' {
					isFloat = true
				}
				j++
			}
			txt := string(r[i:j])
			var v any
			if isFloat {
				f, err := strconv.ParseFloat(txt, 64)
				if err != nil {
					return nil, fmt.Errorf("bad number %q", txt)
				}
				v = f
			} else {
				n, err := strconv.ParseInt(txt, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("integer %s is out of range", txt)
				}
				v = n
			}
			out = append(out, token{kind: 'n', text: txt, val: v})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
				j++
			}
			out = append(out, token{kind: 'i', text: string(r[i:j])})
			i = j
		default:
			two := ""
			if i+1 < len(r) {
				two = string(r[i : i+2])
			}
			switch two {
			case "==", "!=", "<=", ">=", "//":
				out = append(out, token{kind: 'o', text: two})
				i += 2
				continue
			}
			if strings.ContainsRune("+-*/%<>()[]{}.,:", c) {
				out = append(out, token{kind: 'o', text: string(c)})
				i++
				continue
			}
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	return out, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() *token {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *parser) isOp(s string) bool {
	t := p.peek()
	return t != nil && t.kind == 'o' && t.text == s
}

// isWord matches a keyword in all lower or all upper case, as Google's
// logical operators and literals may be written.
func (p *parser) isWord(w string) bool {
	t := p.peek()
	return t != nil && t.kind == 'i' && (t.text == w || t.text == strings.ToUpper(w))
}

func (p *parser) expect(s string) error {
	if !p.isOp(s) {
		if t := p.peek(); t != nil {
			return fmt.Errorf("expected %q, found %q", s, t.text)
		}
		return fmt.Errorf("expected %q at the end", s)
	}
	p.pos++
	return nil
}

func (p *parser) expr() (node, error) { return p.or() }

func (p *parser) or() (node, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.isWord("or") {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: "or", l: l, r: r}
	}
	return l, nil
}

func (p *parser) and() (node, error) {
	l, err := p.cmp()
	if err != nil {
		return nil, err
	}
	for p.isWord("and") {
		p.pos++
		r, err := p.cmp()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: "and", l: l, r: r}
	}
	return l, nil
}

func (p *parser) cmp() (node, error) {
	l, err := p.add()
	if err != nil {
		return nil, err
	}
	for {
		op := ""
		switch {
		case p.isOp("=="), p.isOp("!="), p.isOp("<"), p.isOp(">"), p.isOp("<="), p.isOp(">="):
			op = p.peek().text
		case p.isWord("in"):
			op = "in"
		default:
			return l, nil
		}
		p.pos++
		r, err := p.add()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: op, l: l, r: r}
	}
}

func (p *parser) add() (node, error) {
	l, err := p.mul()
	if err != nil {
		return nil, err
	}
	for p.isOp("+") || p.isOp("-") {
		op := p.peek().text
		p.pos++
		r, err := p.mul()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *parser) mul() (node, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.isOp("*") || p.isOp("/") || p.isOp("//") || p.isOp("%") {
		op := p.peek().text
		p.pos++
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *parser) unary() (node, error) {
	if p.isOp("-") {
		p.pos++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		if lit, ok := x.(*litNode); ok {
			switch v := lit.v.(type) {
			case int64:
				return &litNode{v: -v}, nil
			case float64:
				return &litNode{v: -v}, nil
			}
		}
		return &unaryNode{op: "-", x: x}, nil
	}
	if p.isWord("not") {
		p.pos++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "not", x: x}, nil
	}
	return p.postfix()
}

func (p *parser) postfix() (node, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.isOp("."):
			p.pos++
			t := p.peek()
			if t == nil || t.kind != 'i' {
				return nil, fmt.Errorf("expected a field name after '.'")
			}
			p.pos++
			x = &fieldNode{x: x, name: t.text}
		case p.isOp("["):
			p.pos++
			i, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			x = &indexNode{x: x, i: i}
		case p.isOp("("):
			name, ok := dottedName(x)
			if !ok {
				return nil, fmt.Errorf("only named functions can be called")
			}
			p.pos++
			var args []node
			for !p.isOp(")") {
				a, err := p.expr()
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.isOp(",") {
					p.pos++
					continue
				}
				if !p.isOp(")") {
					return nil, fmt.Errorf("expected ',' or ')' in the arguments of %s", name)
				}
			}
			p.pos++
			if blocking[name] {
				return nil, fmt.Errorf("%s is a blocking function and can only be called from a call step", name)
			}
			x = &callNode{fn: name, args: args}
		default:
			return x, nil
		}
	}
}

// dottedName is the name a.b.c a call's callee spells.
func dottedName(n node) (string, bool) {
	switch v := n.(type) {
	case *varNode:
		return v.name, true
	case *fieldNode:
		base, ok := dottedName(v.x)
		return base + "." + v.name, ok
	}
	return "", false
}

func (p *parser) primary() (node, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	switch t.kind {
	case 'n', 's':
		p.pos++
		return &litNode{v: t.val}, nil
	case 'i':
		p.pos++
		switch t.text {
		case "true", "True", "TRUE":
			return &litNode{v: true}, nil
		case "false", "False", "FALSE":
			return &litNode{v: false}, nil
		case "null":
			return &litNode{v: nil}, nil
		}
		return &varNode{name: t.text}, nil
	}
	switch t.text {
	case "(":
		p.pos++
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		return x, p.expect(")")
	case "[":
		p.pos++
		l := &listNode{}
		for !p.isOp("]") {
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			l.items = append(l.items, x)
			if p.isOp(",") {
				p.pos++
			} else if !p.isOp("]") {
				return nil, fmt.Errorf("expected ',' or ']' in a list")
			}
		}
		p.pos++
		return l, nil
	case "{":
		p.pos++
		m := &mapNode{}
		for !p.isOp("}") {
			k, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.expr()
			if err != nil {
				return nil, err
			}
			m.keys, m.vals = append(m.keys, k), append(m.vals, v)
			if p.isOp(",") {
				p.pos++
			} else if !p.isOp("}") {
				return nil, fmt.Errorf("expected ',' or '}' in a map")
			}
		}
		p.pos++
		return m, nil
	}
	return nil, fmt.Errorf("unexpected %q", t.text)
}

// ---- evaluation ----

func (n *litNode) eval(*frame) (any, error) { return n.v, nil }

func (n *varNode) eval(f *frame) (any, error) {
	if v, ok := f.lookup(n.name); ok {
		return v, nil
	}
	if c, ok := constant(n.name); ok {
		return c, nil
	}
	if f.isFunction(n.name) {
		return funcRef{name: n.name}, nil
	}
	return nil, raiseErr("KeyError", "variable '%s' not found", n.name)
}

func (n *fieldNode) eval(f *frame) (any, error) {
	if name, ok := dottedName(n); ok {
		root := strings.SplitN(name, ".", 2)[0]
		if _, isVar := f.lookup(root); !isVar {
			if c, ok := constant(name); ok {
				return c, nil
			}
			if f.isFunction(name) {
				return funcRef{name: name}, nil
			}
		}
	}
	x, err := n.x.eval(f)
	if err != nil {
		return nil, err
	}
	m, ok := x.(map[string]any)
	if !ok {
		return nil, raiseErr("TypeError", "cannot access field '%s' of a value of type %s", n.name, typeName(x))
	}
	v, ok := m[n.name]
	if !ok {
		return nil, raiseErr("KeyError", "key not found: %s", n.name)
	}
	return v, nil
}

func (n *indexNode) eval(f *frame) (any, error) {
	x, err := n.x.eval(f)
	if err != nil {
		return nil, err
	}
	i, err := n.i.eval(f)
	if err != nil {
		return nil, err
	}
	return index(x, i)
}

func index(x, i any) (any, error) {
	switch c := x.(type) {
	case map[string]any:
		k, ok := i.(string)
		if !ok {
			return nil, raiseErr("TypeError", "map keys must be strings, not %s", typeName(i))
		}
		v, ok := c[k]
		if !ok {
			return nil, raiseErr("KeyError", "key not found: %s", k)
		}
		return v, nil
	case []any:
		k, ok := i.(int64)
		if !ok {
			return nil, raiseErr("TypeError", "list indices must be integers, not %s", typeName(i))
		}
		if k < 0 || k >= int64(len(c)) {
			return nil, raiseErr("IndexError", "list index out of range")
		}
		return c[k], nil
	}
	return nil, raiseErr("TypeError", "a value of type %s cannot be indexed", typeName(x))
}

func (n *callNode) eval(f *frame) (any, error) {
	// if() evaluates every argument, as Google's does.
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(f)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return f.callExpr(n.fn, args)
}

func (n *unaryNode) eval(f *frame) (any, error) {
	x, err := n.x.eval(f)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "not":
		b, ok := x.(bool)
		if !ok {
			return nil, raiseErr("TypeError", "'not' needs a boolean, not %s", typeName(x))
		}
		return !b, nil
	default:
		switch v := x.(type) {
		case int64:
			if v == math.MinInt64 {
				return nil, raiseErr("ValueError", "integer overflow")
			}
			return -v, nil
		case float64:
			return -v, nil
		}
		return nil, raiseErr("TypeError", "unary '-' needs a number, not %s", typeName(x))
	}
}

func (n *listNode) eval(f *frame) (any, error) {
	out := make([]any, len(n.items))
	for i, it := range n.items {
		v, err := it.eval(f)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (n *mapNode) eval(f *frame) (any, error) {
	out := make(map[string]any, len(n.keys))
	for i := range n.keys {
		k, err := n.keys[i].eval(f)
		if err != nil {
			return nil, err
		}
		ks, ok := k.(string)
		if !ok {
			return nil, raiseErr("TypeError", "map keys must be strings, not %s", typeName(k))
		}
		v, err := n.vals[i].eval(f)
		if err != nil {
			return nil, err
		}
		out[ks] = v
	}
	return out, nil
}

func (n *binNode) eval(f *frame) (any, error) {
	l, err := n.l.eval(f)
	if err != nil {
		return nil, err
	}
	// and/or short-circuit.
	if n.op == "and" || n.op == "or" {
		lb, ok := l.(bool)
		if !ok {
			return nil, raiseErr("TypeError", "'%s' needs booleans, not %s", n.op, typeName(l))
		}
		if (n.op == "and" && !lb) || (n.op == "or" && lb) {
			return lb, nil
		}
		r, err := n.r.eval(f)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, raiseErr("TypeError", "'%s' needs booleans, not %s", n.op, typeName(r))
		}
		return rb, nil
	}
	r, err := n.r.eval(f)
	if err != nil {
		return nil, err
	}
	return binary(n.op, l, r)
}

func binary(op string, l, r any) (any, error) {
	switch op {
	case "==", "!=":
		eq, err := equal(l, r)
		if err != nil {
			return nil, err
		}
		if op == "!=" {
			return !eq, nil
		}
		return eq, nil
	case "in":
		switch c := r.(type) {
		case []any:
			for _, it := range c {
				if eq, err := equal(l, it); err == nil && eq {
					return true, nil
				}
			}
			return false, nil
		case map[string]any:
			k, ok := l.(string)
			if !ok {
				return nil, raiseErr("TypeError", "map keys are strings, not %s", typeName(l))
			}
			_, ok = c[k]
			return ok, nil
		}
		return nil, raiseErr("TypeError", "'in' needs a list or a map, not %s", typeName(r))
	case "<", ">", "<=", ">=":
		a, aok := toFloat(l)
		b, bok := toFloat(r)
		if !aok || !bok {
			// Only numbers order (datatypes: strings and booleans are an
			// error).
			return nil, raiseErr("TypeError", "'%s' is not supported between %s and %s", op, typeName(l), typeName(r))
		}
		li, lInt := l.(int64)
		ri, rInt := r.(int64)
		if lInt && rInt {
			c := 0
			if li < ri {
				c = -1
			} else if li > ri {
				c = 1
			}
			return compare(op, c), nil
		}
		c := 0
		if a < b {
			c = -1
		} else if a > b {
			c = 1
		}
		return compare(op, c), nil
	case "+":
		// Anything with a string is a string (datatypes, implicit
		// conversion of +).
		ls, lStr := l.(string)
		rs, rStr := r.(string)
		if lStr || rStr {
			if !lStr {
				s, ok := scalarString(l)
				if !ok {
					return nil, raiseErr("TypeError", "unsupported operand types for +: %s and %s", typeName(l), typeName(r))
				}
				ls = s
			}
			if !rStr {
				s, ok := scalarString(r)
				if !ok {
					return nil, raiseErr("TypeError", "unsupported operand types for +: %s and %s", typeName(l), typeName(r))
				}
				rs = s
			}
			if len(ls)+len(rs) > MaxStringBytes {
				return nil, raiseErr("ResourceLimitError", "string length limit exceeded: %d bytes", MaxStringBytes)
			}
			return ls + rs, nil
		}
		return arith(op, l, r)
	default:
		return arith(op, l, r)
	}
}

func compare(op string, c int) bool {
	switch op {
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	}
	return c >= 0
}

func arith(op string, l, r any) (any, error) {
	li, lInt := l.(int64)
	ri, rInt := r.(int64)
	a, aok := toFloat(l)
	b, bok := toFloat(r)
	if !aok || !bok {
		return nil, raiseErr("TypeError", "unsupported operand types for %s: %s and %s", op, typeName(l), typeName(r))
	}
	if lInt && rInt {
		switch op {
		case "+":
			s := li + ri
			if (s > li) != (ri > 0) {
				return nil, raiseErr("ValueError", "integer overflow")
			}
			return s, nil
		case "-":
			s := li - ri
			if (s < li) != (ri > 0) {
				return nil, raiseErr("ValueError", "integer overflow")
			}
			return s, nil
		case "*":
			if li != 0 && ri != 0 {
				p := li * ri
				if p/ri != li || (li == -1 && ri == math.MinInt64) || (ri == -1 && li == math.MinInt64) {
					return nil, raiseErr("ValueError", "integer overflow")
				}
				return p, nil
			}
			return int64(0), nil
		case "//":
			if ri == 0 {
				return nil, raiseErr("ZeroDivisionError", "division by zero")
			}
			q := li / ri
			if (li%ri != 0) && ((li < 0) != (ri < 0)) {
				q--
			}
			return q, nil
		case "%":
			if ri == 0 {
				return nil, raiseErr("ZeroDivisionError", "division by zero")
			}
			m := li % ri
			if m != 0 && ((m < 0) != (ri < 0)) {
				m += ri
			}
			return m, nil
		}
	}
	switch op {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "*":
		return a * b, nil
	case "/":
		if b == 0 {
			return nil, raiseErr("ZeroDivisionError", "division by zero")
		}
		return a / b, nil
	case "//":
		if b == 0 {
			return nil, raiseErr("ZeroDivisionError", "division by zero")
		}
		return math.Floor(a / b), nil
	case "%":
		if b == 0 {
			return nil, raiseErr("ZeroDivisionError", "division by zero")
		}
		m := math.Mod(a, b)
		if m != 0 && ((m < 0) != (b < 0)) {
			m += b
		}
		return m, nil
	}
	return nil, raiseErr("TypeError", "unknown operator %s", op)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// equal is == by Google's table: numbers with numbers, a string or a bool
// with its own type, null with anything; other mixes are a TypeError.
// Lists and maps compare element by element.
func equal(l, r any) (bool, error) {
	if l == nil || r == nil {
		return l == nil && r == nil, nil
	}
	if a, ok := toFloat(l); ok {
		b, ok := toFloat(r)
		if !ok {
			return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
		}
		li, lInt := l.(int64)
		ri, rInt := r.(int64)
		if lInt && rInt {
			return li == ri, nil
		}
		return a == b, nil
	}
	switch a := l.(type) {
	case string:
		b, ok := r.(string)
		if !ok {
			return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
		}
		return a == b, nil
	case bool:
		b, ok := r.(bool)
		if !ok {
			return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
		}
		return a == b, nil
	case []byte:
		b, ok := r.([]byte)
		if !ok {
			return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
		}
		return string(a) == string(b), nil
	case []any:
		b, ok := r.([]any)
		if !ok {
			return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
		}
		if len(a) != len(b) {
			return false, nil
		}
		for i := range a {
			if eq, err := equal(a[i], b[i]); err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	case map[string]any:
		b, ok := r.(map[string]any)
		if !ok {
			return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
		}
		if len(a) != len(b) {
			return false, nil
		}
		for k, v := range a {
			w, ok := b[k]
			if !ok {
				return false, nil
			}
			if eq, err := equal(v, w); err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	}
	return false, raiseErr("TypeError", "cannot compare %s with %s", typeName(l), typeName(r))
}

// scalarString is string() of an integer, a double or a boolean.
func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return formatDouble(x), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func formatDouble(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// typeName is get_type()'s answer.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64:
		return "integer"
	case float64:
		return "double"
	case string:
		return "string"
	case []byte:
		return "bytes"
	case []any:
		return "list"
	case map[string]any:
		return "map"
	case funcRef:
		return "function"
	}
	return fmt.Sprintf("%T", v)
}

// funcRef is a function or subworkflow named as a value, such as a retry
// predicate.
type funcRef struct{ name string }
