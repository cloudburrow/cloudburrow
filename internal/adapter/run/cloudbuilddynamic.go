package run

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Dynamic substitutions (#1449): with options.dynamicSubstitutions, a
// user substitution's value may name other substitutions, built-in or
// user, and every substitution may be written with the bash parameter
// expansions Cloud Build documents: ${VAR:offset}, ${VAR:offset:length},
// ${VAR#pattern}, ${VAR##pattern}, ${VAR%pattern}, ${VAR%%pattern},
// ${VAR/pattern/replacement} and ${VAR//pattern/replacement}, where a
// pattern is a shell glob (*, ? and [...]).

// dynamicKey marks a substitution map as dynamic. The empty key is never a
// substitution's name, so it never reaches a step.
const dynamicKey = ""

var dynamicRE = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)([^}]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandDynamic applies substitutions with bash parameter expansion; a
// variable with no substitution is left as written, as substitute does.
func expandDynamic(s string, subs map[string]string) string {
	return dynamicRE.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		g := dynamicRE.FindStringSubmatch(m)
		name, op := g[1], g[2]
		if name == "" {
			name = g[3]
		}
		v, ok := subs[name]
		if !ok || name == dynamicKey {
			return m
		}
		out, ok := paramOp(v, op)
		if !ok {
			return m
		}
		return out
	})
}

// paramOp applies one bash expansion operator to v.
func paramOp(v, op string) (string, bool) {
	switch {
	case op == "":
		return v, true
	case strings.HasPrefix(op, "##"):
		return trimPrefixGlob(v, op[2:], true), true
	case strings.HasPrefix(op, "#"):
		return trimPrefixGlob(v, op[1:], false), true
	case strings.HasPrefix(op, "%%"):
		return trimSuffixGlob(v, op[2:], true), true
	case strings.HasPrefix(op, "%"):
		return trimSuffixGlob(v, op[1:], false), true
	case strings.HasPrefix(op, "//"):
		pat, rep, _ := strings.Cut(op[2:], "/")
		return replaceGlob(v, pat, rep, true), true
	case strings.HasPrefix(op, "/"):
		pat, rep, _ := strings.Cut(op[1:], "/")
		return replaceGlob(v, pat, rep, false), true
	case strings.HasPrefix(op, ":"):
		return substring(v, op[1:])
	}
	return "", false
}

// substring is ${VAR:offset[:length]}; a negative offset counts from the
// end and, as in bash, is written with a space or in parentheses.
func substring(v, spec string) (string, bool) {
	offS, lenS, hasLen := strings.Cut(spec, ":")
	num := func(s string) (int, bool) {
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "()"))
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	r := []rune(v)
	off, ok := num(offS)
	if !ok {
		return "", false
	}
	if off < 0 {
		off += len(r)
	}
	if off < 0 || off > len(r) {
		return "", true
	}
	end := len(r)
	if hasLen {
		n, ok := num(lenS)
		if !ok {
			return "", false
		}
		if n < 0 {
			end = len(r) + n
		} else {
			end = off + n
		}
		if end > len(r) {
			end = len(r)
		}
		if end < off {
			return "", false
		}
	}
	return string(r[off:end]), true
}

// globRE is a shell glob as an anchored regular expression.
func globRE(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^(?s:")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			if j := strings.IndexByte(pat[i+1:], ']'); j > 0 {
				cls := pat[i+1 : i+1+j]
				if strings.HasPrefix(cls, "!") {
					cls = "^" + cls[1:]
				}
				b.WriteString("[" + cls + "]")
				i += j + 1
				continue
			}
			b.WriteString(`\[`)
		case '\\':
			if i+1 < len(pat) {
				i++
				b.WriteString(regexp.QuoteMeta(pat[i : i+1]))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(")$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(`^\b$`) // matches nothing
	}
	return re
}

func trimPrefixGlob(v, pat string, longest bool) string {
	re := globRE(pat)
	if longest {
		for i := len(v); i >= 0; i-- {
			if re.MatchString(v[:i]) {
				return v[i:]
			}
		}
		return v
	}
	for i := 0; i <= len(v); i++ {
		if re.MatchString(v[:i]) {
			return v[i:]
		}
	}
	return v
}

func trimSuffixGlob(v, pat string, longest bool) string {
	re := globRE(pat)
	if longest {
		for i := 0; i <= len(v); i++ {
			if re.MatchString(v[i:]) {
				return v[:i]
			}
		}
		return v
	}
	for i := len(v); i >= 0; i-- {
		if re.MatchString(v[i:]) {
			return v[:i]
		}
	}
	return v
}

// replaceGlob replaces the longest match of pat at the first place it
// matches, or at every place with all, as bash does.
func replaceGlob(v, pat, rep string, all bool) string {
	if pat == "" {
		return v
	}
	re := globRE(pat)
	var b strings.Builder
	for i := 0; i < len(v); {
		matched := -1
		for j := len(v); j > i; j-- {
			if re.MatchString(v[i:j]) {
				matched = j
				break
			}
		}
		if matched < 0 {
			b.WriteByte(v[i])
			i++
			continue
		}
		b.WriteString(rep)
		if !all {
			b.WriteString(v[matched:])
			return b.String()
		}
		i = matched
	}
	return b.String()
}

// resolveDynamic expands each user substitution's value against the
// others, built-in and user, until none changes, and marks subs dynamic.
func resolveDynamic(subs map[string]string) {
	keys := make([]string, 0, len(subs))
	for k := range subs {
		if strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for pass := 0; pass <= len(keys); pass++ {
		changed := false
		for _, k := range keys {
			// $$ stays escaped until the value is used.
			v := subs[k]
			nv := expandDynamic(strings.ReplaceAll(v, "$$", "\x00"), subs)
			nv = strings.ReplaceAll(nv, "\x00", "$$")
			if nv != v {
				subs[k], changed = nv, true
			}
		}
		if !changed {
			break
		}
	}
	subs[dynamicKey] = "1"
}
