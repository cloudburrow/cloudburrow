package trace

// The v1 ListTraces filter, as docs.cloud.google.com/trace/docs/trace-filters
// documents it (read 2026-11-21):
//
//   - terms are separated by spaces and ANDed; string comparisons are
//     case-sensitive;
//   - an unquoted value matches [a-zA-Z0-9./_][a-zA-Z0-9./_-]*, anything
//     else is double-quoted with \" and \\ escapes;
//   - root:PREFIX (or a bare PREFIX) matches the root span's name by
//     prefix, +root:NAME (or +NAME) exactly;
//   - span:PREFIX matches any span's name by prefix, +span:NAME exactly;
//   - latency:DURATION matches traces whose overall latency is at least
//     DURATION (s, ms or ns; no unit is milliseconds);
//   - label:KEY matches a span holding label KEY, KEY:VALUE a span whose
//     label KEY starts with VALUE, +KEY:VALUE exactly;
//   - method: and url: are /http/method: and /http/url:;
//   - ^ before a term restricts it to the root span;
//   - root, span, label, method, latency and url are reserved keys.

import (
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

type filterTerm struct {
	exact, rootOnly bool
	key, value      string
}

// Filter is a parsed ListTraces filter.
type Filter []filterTerm

// ParseFilter parses a ListTraces filter.
func ParseFilter(s string) (Filter, error) {
	var f Filter
	i := 0
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for i < len(s) {
		if isSpace(s[i]) {
			i++
			continue
		}
		start := i
		var t filterTerm
		for i < len(s) && (s[i] == '+' || s[i] == '^') {
			if s[i] == '+' {
				t.exact = true
			} else {
				t.rootOnly = true
			}
			i++
		}
		if i < len(s) && s[i] == '"' {
			// A quoted bare value: the root span's name.
			v, n, err := unquote(s[i:])
			if err != nil {
				return nil, err
			}
			i += n
			t.key, t.value = "root", v
		} else {
			ks := i
			for i < len(s) && !isSpace(s[i]) && s[i] != ':' {
				i++
			}
			word := s[ks:i]
			if i < len(s) && s[i] == ':' {
				if word == "" {
					return nil, apierror.InvalidArgument("filter term %q has no key before ':'", s[start:i+1])
				}
				i++
				t.key = word
				if i < len(s) && s[i] == '"' {
					v, n, err := unquote(s[i:])
					if err != nil {
						return nil, err
					}
					i += n
					t.value = v
				} else {
					vs := i
					for i < len(s) && !isSpace(s[i]) {
						i++
					}
					t.value = s[vs:i]
				}
			} else {
				t.key, t.value = "root", word
			}
		}
		if i < len(s) && !isSpace(s[i]) {
			return nil, apierror.InvalidArgument("filter term %q: a space must follow it", s[start:])
		}
		switch t.key {
		case "method":
			t.key = "/http/method"
		case "url":
			t.key = "/http/url"
		case "latency":
			if _, err := parseLatency(t.value); err != nil {
				return nil, err
			}
		}
		f = append(f, t)
	}
	return f, nil
}

// unquote reads a double-quoted string at the start of s, returning it and
// the bytes it took.
func unquote(s string) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", 0, apierror.InvalidArgument("filter: unterminated escape in %s", s)
			}
			i++
			b.WriteByte(s[i])
		case '"':
			return b.String(), i + 1, nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, apierror.InvalidArgument("filter: unterminated quoted string %s", s)
}

// parseLatency reads a latency: duration; no unit is milliseconds.
func parseLatency(v string) (time.Duration, error) {
	units := []struct {
		suffix string
		unit   time.Duration
	}{{"ms", time.Millisecond}, {"ns", time.Nanosecond}, {"s", time.Second}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(v, u.suffix); ok {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil || f < 0 {
				return 0, apierror.InvalidArgument("filter: latency %q is not a duration", v)
			}
			return time.Duration(f * float64(u.unit)), nil
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, apierror.InvalidArgument("filter: latency %q is not a duration", v)
	}
	return time.Duration(f * float64(time.Millisecond)), nil
}

// Match is whether a trace passes every term.
func (f Filter) Match(t Trace) bool {
	root := t.Root()
	for _, term := range f {
		if !term.match(t, root) {
			return false
		}
	}
	return true
}

func (term filterTerm) cmp(got string) bool {
	if term.exact {
		return got == term.value
	}
	return strings.HasPrefix(got, term.value)
}

func (term filterTerm) match(t Trace, root *Span) bool {
	spans := t.Spans
	if term.rootOnly {
		spans = nil
		if root != nil {
			spans = []*Span{root}
		}
	}
	switch term.key {
	case "root":
		return root != nil && term.cmp(root.Name())
	case "span":
		for _, sp := range spans {
			if term.cmp(sp.Name()) {
				return true
			}
		}
		return false
	case "latency":
		d, _ := parseLatency(term.value)
		return t.End().Sub(t.Start()) >= d
	case "label":
		for _, sp := range spans {
			if _, ok := sp.V2.GetAttributes().GetAttributeMap()[term.value]; ok {
				return true
			}
		}
		return false
	}
	for _, sp := range spans {
		v, ok := sp.V2.GetAttributes().GetAttributeMap()[term.key]
		if ok && term.cmp(AttrString(v)) {
			return true
		}
	}
	return false
}
