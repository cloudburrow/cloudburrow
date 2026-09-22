package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/identity-wael/cloudburrow/internal/console"
)

// Every create form shipped to the browser.
func createForms(t *testing.T) map[string][]console.Field {
	t.Helper()
	out := map[string][]console.Field{}
	for _, p := range []console.Provider{
		storageProvider{}, pubsubProvider{}, tasksProvider{}, runProvider{},
	} {
		creator, ok := p.(console.Creator)
		if !ok {
			t.Fatalf("%s does not offer a create form", p.ID())
		}
		_, fields := creator.CreateForm()
		out[p.ID()] = fields
	}
	return out
}

// An HTML `pattern` attribute is compiled with the RegExp `v` flag, where `-`
// must be escaped inside a character class even in trailing position. An
// unescaped one makes the pattern invalid and the browser then **silently
// skips validation** — the form appears to validate and does not.
//
// Found by driving a real browser: every create form had shipped with a
// pattern the browser refused.
func TestCreateFormPatternsAreValidInTheBrowser(t *testing.T) {
	t.Parallel()

	for service, fields := range createForms(t) {
		for _, f := range fields {
			if f.Pattern == "" {
				continue
			}
			// `pattern` is an attribute of <input>, and only of the input
			// types that hold text. A pattern on a checkbox or a textarea is
			// not enforced by anything, so the form would appear to constrain
			// a value it does not constrain.
			if !patternable(f.Type) {
				t.Errorf("%s/%s is a %q field carrying pattern %q, which nothing enforces",
					service, f.Name, f.Type, f.Pattern)
				continue
			}
			// Only a *literal* hyphen needs escaping. One between two
			// characters is a range and is fine — flagging `a-z` would make
			// the test useless. What the browser rejects, and what shipped,
			// is a hyphen at the start or end of a class.
			for _, class := range characterClasses(f.Pattern) {
				if pos, bad := unescapedLiteralHyphen(class); bad {
					t.Errorf("%s/%s pattern %q has an unescaped literal '-' at position %d "+
						"of character class [%s]; the browser compiles this with the v flag, "+
						"rejects it, and then validates nothing",
						service, f.Name, f.Pattern, pos, class)
				}
			}
			// It must also be a valid Go regexp, or the two would disagree
			// about what they accept.
			if _, err := regexp.Compile(f.Pattern); err != nil {
				t.Errorf("%s/%s pattern %q does not compile: %v", service, f.Name, f.Pattern, err)
			}
		}
	}
}

// A pattern must actually accept the default it ships with, or the form
// refuses its own suggestion.
func TestCreateFormDefaultsSatisfyTheirPatterns(t *testing.T) {
	t.Parallel()

	for service, fields := range createForms(t) {
		for _, f := range fields {
			if f.Pattern == "" || f.Default == "" {
				continue
			}
			re, err := regexp.Compile(f.Pattern)
			if err != nil {
				continue // reported by the test above
			}
			if !re.MatchString(f.Default) {
				t.Errorf("%s/%s default %q does not satisfy its own pattern %q",
					service, f.Name, f.Default, f.Pattern)
			}
		}
	}
}

// A required field with no help text leaves a rejected user guessing.
func TestRequiredFieldsExplainTheirConstraint(t *testing.T) {
	t.Parallel()

	for service, fields := range createForms(t) {
		for _, f := range fields {
			if f.Required && f.Help == "" {
				t.Errorf("%s/%s is required but carries no help text, so a "+
					"rejected value leaves the user guessing", service, f.Name)
			}
			if f.Label == "" {
				t.Errorf("%s/%s has no label", service, f.Name)
			}
		}
	}
}

// patternable reports whether a field type can carry an HTML `pattern`.
//
// The empty type is text, which is what the client falls back to.
func patternable(fieldType string) bool {
	switch fieldType {
	case "", "text", "search", "url", "tel", "email", "password":
		return true
	default:
		return false
	}
}

// characterClasses returns the contents of each [...] in a pattern.
func characterClasses(pattern string) []string {
	var out []string
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '[' || (i > 0 && pattern[i-1] == '\\') {
			continue
		}
		end := strings.IndexByte(pattern[i+1:], ']')
		if end < 0 {
			continue
		}
		out = append(out, pattern[i+1:i+1+end])
		i += end
	}
	return out
}

// unescapedLiteralHyphen reports a hyphen that is literal rather than a range
// operator and is not escaped.
//
// A hyphen between two characters is a range, which needs no escaping. A
// leading or trailing one is literal, and the `v` flag requires it escaped.
func unescapedLiteralHyphen(class string) (int, bool) {
	if class == "" {
		return 0, false
	}
	// Leading, allowing for a leading negation.
	start := 0
	if class[0] == '^' {
		start = 1
	}
	if start < len(class) && class[start] == '-' {
		return start, true
	}
	// Trailing, unless escaped.
	last := len(class) - 1
	if class[last] == '-' && (last == 0 || class[last-1] != '\\') {
		return last, true
	}
	return 0, false
}
