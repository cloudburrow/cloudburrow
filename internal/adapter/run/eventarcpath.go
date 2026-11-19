package run

import (
	"fmt"
	"regexp"
	"strings"
)

// Eventarc's path patterns (#1877), as Google's "Understand path patterns"
// page documents them: the match-path-pattern operator's value is an
// optional leading /, then segments separated by /, each a capture group
// ({ID} or {ID=expression}) or an expression: *, ** or a name segment
// holding at most one * among its other characters. * matches zero or
// more characters within a segment, ** zero or more segments, at most
// once in a pattern; {ID} is {ID=*}. Capture groups carry no meaning.

// pathPatternOp is the only filter operator Google documents beyond an
// exact match.
const pathPatternOp = "match-path-pattern"

var (
	captureIDRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	// patternCharsRE is the ASCII the page allows in a pattern.
	patternCharsRE = regexp.MustCompile(`^[A-Za-z0-9\-._~%!$&'()*+,;=:@/{}]*$`)
)

// patternSegments splits and checks a pattern, returning each segment's
// expression with its capture group removed.
func patternSegments(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, fmt.Errorf("the path pattern is empty")
	}
	if !patternCharsRE.MatchString(pattern) {
		return nil, fmt.Errorf("path pattern %q has a character path patterns do not allow", pattern)
	}
	var out []string
	doubles := 0
	for _, seg := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		expr := seg
		if strings.HasPrefix(seg, "{") {
			if !strings.HasSuffix(seg, "}") {
				return nil, fmt.Errorf("path pattern %q has an unclosed capture group", pattern)
			}
			inner := seg[1 : len(seg)-1]
			id, e, hasExpr := strings.Cut(inner, "=")
			if !captureIDRE.MatchString(id) {
				return nil, fmt.Errorf("path pattern %q has a capture group whose ID is not letters, digits and underscores", pattern)
			}
			expr = "*"
			if hasExpr {
				expr = e
			}
		}
		if expr == "" {
			return nil, fmt.Errorf("path pattern %q has an empty expression", pattern)
		}
		if strings.ContainsAny(expr, "{}") {
			return nil, fmt.Errorf("path pattern %q has an unclosed capture group", pattern)
		}
		switch n := strings.Count(expr, "*"); {
		case expr == "**":
			doubles++
		case n > 1:
			return nil, fmt.Errorf("path pattern %q has more than one * in an expression", pattern)
		}
		out = append(out, expr)
	}
	if doubles > 1 {
		return nil, fmt.Errorf("path pattern %q has more than one **", pattern)
	}
	return out, nil
}

// validPathPattern refuses a pattern Google's page calls invalid.
func validPathPattern(pattern string) error {
	_, err := patternSegments(pattern)
	return err
}

// matchPathPattern reports whether path matches pattern; an invalid
// pattern matches nothing.
func matchPathPattern(pattern, path string) bool {
	segs, err := patternSegments(pattern)
	if err != nil {
		return false
	}
	return matchSegments(segs, strings.Split(strings.TrimPrefix(path, "/"), "/"))
}

func matchSegments(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if matchSegments(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 || !matchSegment(pat[0], path[0]) {
		return false
	}
	return matchSegments(pat[1:], path[1:])
}

func matchSegment(expr, seg string) bool {
	before, after, wild := strings.Cut(expr, "*")
	if !wild {
		return expr == seg
	}
	return len(seg) >= len(before)+len(after) && strings.HasPrefix(seg, before) && strings.HasSuffix(seg, after)
}
