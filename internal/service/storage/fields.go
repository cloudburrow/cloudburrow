package storage

import (
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A field selection is the `fields` system parameter's value
// (docs.cloud.google.com/storage/docs/json_api/v1/how-tos/performance#partial-response):
// comma-separated field names, "a/b" to select within a, "a(b,c)" to select
// several within a, and "*" for every field. A selection applies to each
// element of an array.
type selection map[string]selection // nil means "all of this field"

// parseFields parses a fields value. A malformed one is INVALID_ARGUMENT, as
// Google answers 400 invalidParameter (the reason is UNVERIFIED).
func parseFields(spec string) (selection, error) {
	p := &fieldParser{s: spec}
	sel, err := p.list(false)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, apierror.InvalidArgument("invalid field selection %q at offset %d", spec, p.i)
	}
	return sel, nil
}

type fieldParser struct {
	s string
	i int
}

// list parses name[/path|(list)] items separated by commas, up to a closing
// parenthesis when nested.
func (p *fieldParser) list(nested bool) (selection, error) {
	sel := selection{}
	for {
		if err := p.item(sel); err != nil {
			return nil, err
		}
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		if nested {
			if p.i >= len(p.s) || p.s[p.i] != ')' {
				return nil, apierror.InvalidArgument("invalid field selection %q: unclosed parenthesis", p.s)
			}
			p.i++
		}
		return sel, nil
	}
}

func (p *fieldParser) item(into selection) error {
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '/' && p.s[p.i] != '(' && p.s[p.i] != ')' {
		p.i++
	}
	name := p.s[start:p.i]
	if name == "" {
		return apierror.InvalidArgument("invalid field selection %q: empty field name at offset %d", p.s, start)
	}
	if p.i < len(p.s) {
		switch p.s[p.i] {
		case '/':
			p.i++
			sub := into[name]
			if sub == nil {
				sub = selection{}
			}
			if err := p.item(sub); err != nil {
				return err
			}
			into[name] = sub
			return nil
		case '(':
			p.i++
			sub, err := p.list(true)
			if err != nil {
				return err
			}
			into[name] = merge(into[name], sub)
			return nil
		}
	}
	into[name] = nil
	return nil
}

func merge(a, b selection) selection {
	if a == nil {
		return b
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

// apply keeps what sel selects from v, a decoded JSON value.
func (sel selection) apply(v any) any {
	if sel == nil {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, sub := range sel {
			if k == "*" {
				for kk, vv := range x {
					out[kk] = sub.apply(vv)
				}
				continue
			}
			if vv, ok := x[k]; ok {
				out[k] = sub.apply(vv)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = sel.apply(e)
		}
		return out
	default:
		return v
	}
}
