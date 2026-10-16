package run

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ListBuilds' filter (#1382), the syntax "Filtering build results"
// documents: field OP "value", with = and != for every field and >, >=, <
// and <= for the times, combined with AND, OR, NOT (or a leading -) and
// parentheses. As in Google's list filters, OR binds tighter than AND, and
// juxtaposed terms are ANDed. A repeated field (tags, images,
// results.images.*) matches = when any value does and != when none does.
// field=(a OR b) is field=a OR field=b. Times are RFC 3339.

// buildFilterFields maps each documented field to its values in a build.
var buildFilterFields = map[string]func(*cbpb.Build) []string{
	"status":     func(b *cbpb.Build) []string { return []string{b.GetStatus().String()} },
	"build_id":   func(b *cbpb.Build) []string { return []string{b.GetId()} },
	"id":         func(b *cbpb.Build) []string { return []string{b.GetId()} },
	"project_id": func(b *cbpb.Build) []string { return []string{b.GetProjectId()} },
	"trigger_id": func(b *cbpb.Build) []string { return []string{b.GetBuildTriggerId()} },
	"source.storage_source.bucket": func(b *cbpb.Build) []string {
		return []string{b.GetSource().GetStorageSource().GetBucket()}
	},
	"source.storage_source.object": func(b *cbpb.Build) []string {
		return []string{b.GetSource().GetStorageSource().GetObject()}
	},
	"source.repo_source.repo_name": func(b *cbpb.Build) []string {
		return []string{b.GetSource().GetRepoSource().GetRepoName()}
	},
	"source.repo_source.branch_name": func(b *cbpb.Build) []string {
		return []string{b.GetSource().GetRepoSource().GetBranchName()}
	},
	"source.repo_source.tag_name": func(b *cbpb.Build) []string {
		return []string{b.GetSource().GetRepoSource().GetTagName()}
	},
	"source.repo_source.commit_sha": func(b *cbpb.Build) []string {
		return []string{b.GetSource().GetRepoSource().GetCommitSha()}
	},
	"source_provenance.resolved_repo_source.commit_sha": func(b *cbpb.Build) []string {
		return []string{b.GetSourceProvenance().GetResolvedRepoSource().GetCommitSha()}
	},
	"results.images.name": func(b *cbpb.Build) []string {
		var out []string
		for _, im := range b.GetResults().GetImages() {
			out = append(out, im.GetName())
		}
		return out
	},
	"results.images.digest": func(b *cbpb.Build) []string {
		var out []string
		for _, im := range b.GetResults().GetImages() {
			out = append(out, im.GetDigest())
		}
		return out
	},
	"options.requested_verify_option": func(b *cbpb.Build) []string {
		return []string{b.GetOptions().GetRequestedVerifyOption().String()}
	},
	"tags":   func(b *cbpb.Build) []string { return b.GetTags() },
	"images": func(b *cbpb.Build) []string { return b.GetImages() },
}

// buildTimeFields are the fields compared as times.
var buildTimeFields = map[string]func(*cbpb.Build) (time.Time, bool){
	"create_time": func(b *cbpb.Build) (time.Time, bool) { return b.GetCreateTime().AsTime(), b.GetCreateTime() != nil },
	"start_time":  func(b *cbpb.Build) (time.Time, bool) { return b.GetStartTime().AsTime(), b.GetStartTime() != nil },
	"finish_time": func(b *cbpb.Build) (time.Time, bool) { return b.GetFinishTime().AsTime(), b.GetFinishTime() != nil },
}

// buildFilter is a parsed filter: it reports whether a build matches.
type buildFilter func(*cbpb.Build) bool

type filterToken struct {
	kind string // "(", ")", "op", "word", "string"
	text string
}

func lexBuildFilter(s string) ([]filterToken, error) {
	var out []filterToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(' || c == ')':
			out = append(out, filterToken{kind: string(c), text: string(c)})
			i++
		case c == '"' || c == '\'':
			j := i + 1
			var sb strings.Builder
			for ; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				sb.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string at offset %d", i)
			}
			out = append(out, filterToken{kind: "string", text: sb.String()})
			i = j + 1
		case strings.ContainsRune("=!<>:", rune(c)):
			op := string(c)
			if i+1 < len(s) && s[i+1] == '=' && c != '=' && c != ':' {
				op += "="
			}
			if op == "!" {
				return nil, fmt.Errorf("unexpected ! at offset %d", i)
			}
			out = append(out, filterToken{kind: "op", text: op})
			i += len(op)
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune("()=!<>:\"'", rune(s[j])) {
				j++
			}
			out = append(out, filterToken{kind: "word", text: s[i:j]})
			i = j
		}
	}
	return out, nil
}

type filterParser struct {
	toks []filterToken
	pos  int
}

func (p *filterParser) peek() (filterToken, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return filterToken{}, false
}

func (p *filterParser) isWord(w string) bool {
	t, ok := p.peek()
	return ok && t.kind == "word" && t.text == w
}

// parseBuildFilter parses a ListBuilds filter; an empty one matches every
// build.
func parseBuildFilter(s string) (buildFilter, error) {
	if strings.TrimSpace(s) == "" {
		return func(*cbpb.Build) bool { return true }, nil
	}
	toks, err := lexBuildFilter(s)
	if err != nil {
		return nil, apierror.InvalidArgument("invalid filter %q: %v", s, err)
	}
	p := &filterParser{toks: toks}
	f, err := p.and()
	if err == nil && p.pos < len(p.toks) {
		err = fmt.Errorf("unexpected %q", p.toks[p.pos].text)
	}
	if err != nil {
		return nil, apierror.InvalidArgument("invalid filter %q: %v", s, err)
	}
	return f, nil
}

// and is or { [AND] or }.
func (p *filterParser) and() (buildFilter, error) {
	left, err := p.or()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind == ")" {
			return left, nil
		}
		if p.isWord("AND") {
			p.pos++
		}
		right, err := p.or()
		if err != nil {
			return nil, err
		}
		l := left
		left = func(b *cbpb.Build) bool { return l(b) && right(b) }
	}
}

// or is unary { OR unary }.
func (p *filterParser) or() (buildFilter, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.isWord("OR") {
		p.pos++
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		l := left
		left = func(b *cbpb.Build) bool { return l(b) || right(b) }
	}
	return left, nil
}

func (p *filterParser) unary() (buildFilter, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end")
	}
	if p.isWord("NOT") || (t.kind == "word" && strings.HasPrefix(t.text, "-") && len(t.text) > 1) {
		if t.text == "NOT" {
			p.pos++
		} else {
			p.toks[p.pos].text = t.text[1:]
		}
		f, err := p.unary()
		if err != nil {
			return nil, err
		}
		return func(b *cbpb.Build) bool { return !f(b) }, nil
	}
	if t.kind == "(" {
		p.pos++
		f, err := p.and()
		if err != nil {
			return nil, err
		}
		if t, ok := p.peek(); !ok || t.kind != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return f, nil
	}
	return p.comparison()
}

func (p *filterParser) comparison() (buildFilter, error) {
	t, _ := p.peek()
	if t.kind != "word" {
		return nil, fmt.Errorf("expected a field name, got %q", t.text)
	}
	field := t.text
	p.pos++
	opTok, ok := p.peek()
	if !ok || opTok.kind != "op" {
		return nil, fmt.Errorf("expected an operator after %q", field)
	}
	op := opTok.text
	if op == ":" {
		op = "="
	}
	p.pos++
	// field=(a OR b ...): any of the values.
	if t, ok := p.peek(); ok && t.kind == "(" && (op == "=" || op == "!=") {
		p.pos++
		var fs []buildFilter
		for {
			v, ok := p.peek()
			if !ok || (v.kind != "string" && v.kind != "word") {
				return nil, fmt.Errorf("expected a value in %s%s(...)", field, op)
			}
			p.pos++
			f, err := compareField(field, "=", v.text)
			if err != nil {
				return nil, err
			}
			fs = append(fs, f)
			if p.isWord("OR") {
				p.pos++
				continue
			}
			if c, ok := p.peek(); ok && c.kind == ")" {
				p.pos++
				break
			}
			return nil, fmt.Errorf("expected OR or ) in %s%s(...)", field, op)
		}
		any := func(b *cbpb.Build) bool {
			for _, f := range fs {
				if f(b) {
					return true
				}
			}
			return false
		}
		if op == "!=" {
			return func(b *cbpb.Build) bool { return !any(b) }, nil
		}
		return any, nil
	}
	v, ok := p.peek()
	if !ok || (v.kind != "string" && v.kind != "word") {
		return nil, fmt.Errorf("expected a value after %s%s", field, op)
	}
	p.pos++
	return compareField(field, op, v.text)
}

func compareField(field, op, value string) (buildFilter, error) {
	if tf, ok := buildTimeFields[field]; ok {
		want, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an RFC 3339 time", field, value)
		}
		return func(b *cbpb.Build) bool {
			got, set := tf(b)
			if !set {
				return op == "!="
			}
			switch op {
			case "=":
				return got.Equal(want)
			case "!=":
				return !got.Equal(want)
			case ">":
				return got.After(want)
			case ">=":
				return !got.Before(want)
			case "<":
				return got.Before(want)
			default: // <=
				return !got.After(want)
			}
		}, nil
	}
	vf, ok := buildFilterFields[field]
	if !ok {
		return nil, fmt.Errorf("unknown field %q", field)
	}
	if op != "=" && op != "!=" {
		return nil, fmt.Errorf("%s supports only = and !=, not %s", field, op)
	}
	return func(b *cbpb.Build) bool {
		vals := vf(b)
		if len(vals) == 0 {
			vals = []string{""}
		}
		for _, v := range vals {
			if v == value {
				return op == "="
			}
		}
		return op == "!="
	}, nil
}
