package console

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file measures two accessibility properties of console.css rather than
// asserting that particular strings are present in it:
//
//   - every text/background pairing the stylesheet uses meets the WCAG 2.x
//     contrast minimum in the light theme, the explicit dark theme and the
//     device-selected dark theme (TestTokensMeetContrast);
//   - every rule that suppresses the focus outline has a :focus-visible rule
//     that puts a visible one back (TestOutlineSuppressionHasAFocusVisibleRule).
//
// The CSS reader below is deliberately small: comments, quoted strings, nested
// at-rules and top-level-comma splitting are all this stylesheet needs. It is
// not a general CSS parser, and a construct it does not understand fails the
// test rather than being skipped.

// --- a small CSS reader ------------------------------------------------------

type cssDecl struct{ prop, value string }

type cssRule struct {
	index     int      // source order, for cascade comparisons
	at        []string // enclosing @media/@supports preludes, outermost first
	selectors []string // the selector list, split at top-level commas
	decls     []cssDecl
}

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

func parseCSS(src string) ([]cssRule, error) {
	src = cssComment.ReplaceAllString(src, " ")
	var rules []cssRule
	err := parseCSSBlock(src, nil, &rules)
	return rules, err
}

func parseCSSBlock(src string, at []string, out *[]cssRule) error {
	i := 0
	for {
		// The prelude runs to the next '{' outside quotes.
		start := i
		open := -1
		for j := i; j < len(src); j++ {
			switch src[j] {
			case '"', '\'':
				j = skipCSSString(src, j)
			case '{':
				open = j
			case '}':
				return fmt.Errorf("unbalanced '}' at offset %d", j)
			}
			if open >= 0 {
				break
			}
		}
		if open < 0 {
			if strings.TrimSpace(src[start:]) != "" {
				return fmt.Errorf("trailing text with no block: %q", strings.TrimSpace(src[start:]))
			}
			return nil
		}
		prelude := collapseSpace(src[start:open])
		closeAt, err := matchingBrace(src, open)
		if err != nil {
			return err
		}
		body := src[open+1 : closeAt]
		i = closeAt + 1

		switch {
		case strings.HasPrefix(prelude, "@media"), strings.HasPrefix(prelude, "@supports"):
			nested := append(append([]string(nil), at...), prelude)
			if err := parseCSSBlock(body, nested, out); err != nil {
				return err
			}
		case strings.HasPrefix(prelude, "@keyframes"):
			// Animation frames are not selectors and set no colours or outlines.
		case strings.HasPrefix(prelude, "@"):
			return fmt.Errorf("unhandled at-rule %q", prelude)
		default:
			*out = append(*out, cssRule{
				index:     len(*out),
				at:        at,
				selectors: splitTopLevel(prelude, ','),
				decls:     parseDecls(body),
			})
		}
	}
}

func skipCSSString(s string, j int) int {
	q := s[j]
	for k := j + 1; k < len(s); k++ {
		if s[k] == '\\' {
			k++
			continue
		}
		if s[k] == q {
			return k
		}
	}
	return len(s) - 1
}

func matchingBrace(s string, open int) (int, error) {
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '"', '\'':
			j = skipCSSString(s, j)
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j, nil
			}
		}
	}
	return 0, fmt.Errorf("unclosed '{' at offset %d", open)
}

func parseDecls(body string) []cssDecl {
	var decls []cssDecl
	for _, part := range splitTopLevel(body, ';') {
		prop, value, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		decls = append(decls, cssDecl{
			prop:  strings.ToLower(strings.TrimSpace(prop)),
			value: collapseSpace(value),
		})
	}
	return decls
}

// splitTopLevel splits on sep outside parentheses, brackets and quotes, so
// `:not(:has(+ .a, .b))` and `color-mix(in srgb, …)` stay whole.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, last := 0, 0
	for j := 0; j < len(s); j++ {
		switch c := s[j]; {
		case c == '"' || c == '\'':
			j = skipCSSString(s, j)
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == sep && depth == 0:
			if p := collapseSpace(s[last:j]); p != "" {
				parts = append(parts, p)
			}
			last = j + 1
		}
	}
	if p := collapseSpace(s[last:]); p != "" {
		parts = append(parts, p)
	}
	return parts
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- theme tokens ------------------------------------------------------------

type theme struct {
	name   string
	tokens map[string]string
}

// themeTokens returns the three token sets the console can render with: the
// light block on :root, the explicit dark block, and the dark block chosen by
// prefers-color-scheme when no theme is forced. The dark sets are the light
// set overlaid with the dark declarations, which is what the cascade does to
// any token a dark block does not redefine.
func themeTokens(rules []cssRule) ([]theme, error) {
	find := func(selector, media string) (map[string]string, error) {
		var found map[string]string
		for _, r := range rules {
			if len(r.selectors) != 1 || r.selectors[0] != selector {
				continue
			}
			inMedia := len(r.at) == 1 && strings.Contains(r.at[0], media)
			if (media == "" && len(r.at) != 0) || (media != "" && !inMedia) {
				continue
			}
			if found != nil {
				return nil, fmt.Errorf("two token blocks for %s %s", media, selector)
			}
			found = map[string]string{}
			for _, d := range r.decls {
				if strings.HasPrefix(d.prop, "--") {
					found[d.prop] = d.value
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("no token block for %s %s", media, selector)
		}
		return found, nil
	}
	light, err := find(":root", "")
	if err != nil {
		return nil, err
	}
	dark, err := find(`:root[data-theme="dark"]`, "")
	if err != nil {
		return nil, err
	}
	device, err := find(`:root:not([data-theme="light"])`, "prefers-color-scheme: dark")
	if err != nil {
		return nil, err
	}
	overlay := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range light {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	return []theme{
		{"light", light},
		{"dark", overlay(dark)},
		{"dark (device)", overlay(device)},
	}, nil
}

// --- colour ------------------------------------------------------------------

// rgba is a colour in gamma-encoded sRGB, every channel 0..1.
type rgba struct{ r, g, b, a float64 }

func resolveColor(expr string, tokens map[string]string, depth int) (rgba, error) {
	if depth > 16 {
		return rgba{}, fmt.Errorf("token cycle at %q", expr)
	}
	e := strings.TrimSpace(expr)
	lower := strings.ToLower(e)
	switch {
	case lower == "transparent":
		return rgba{}, nil
	case strings.HasPrefix(e, "#"):
		return parseHex(e)
	case strings.HasPrefix(lower, "var(") && strings.HasSuffix(e, ")"):
		args := splitTopLevel(e[4:len(e)-1], ',')
		if len(args) == 0 {
			return rgba{}, fmt.Errorf("empty var() in %q", e)
		}
		if v, ok := tokens[args[0]]; ok {
			return resolveColor(v, tokens, depth+1)
		}
		if len(args) > 1 {
			return resolveColor(strings.Join(args[1:], ","), tokens, depth+1)
		}
		return rgba{}, fmt.Errorf("token %s is not defined", args[0])
	case strings.HasPrefix(lower, "rgb(") || strings.HasPrefix(lower, "rgba("):
		inner := e[strings.Index(e, "(")+1 : len(e)-1]
		f := strings.FieldsFunc(inner, func(r rune) bool { return r == ',' || r == ' ' || r == '/' })
		if len(f) != 3 && len(f) != 4 {
			return rgba{}, fmt.Errorf("cannot read %q", e)
		}
		var ch [4]float64
		ch[3] = 1
		for k, s := range f {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
			if err != nil {
				return rgba{}, fmt.Errorf("cannot read %q: %v", e, err)
			}
			switch {
			case strings.HasSuffix(s, "%"):
				v /= 100
			case k < 3:
				v /= 255
			}
			ch[k] = v
		}
		return rgba{ch[0], ch[1], ch[2], ch[3]}, nil
	case strings.HasPrefix(lower, "color-mix(") && strings.HasSuffix(e, ")"):
		return resolveColorMix(e, tokens, depth)
	}
	return rgba{}, fmt.Errorf("unsupported colour %q", e)
}

func parseHex(e string) (rgba, error) {
	h := strings.TrimPrefix(e, "#")
	if len(h) == 3 || len(h) == 4 {
		var b strings.Builder
		for _, c := range h {
			b.WriteRune(c)
			b.WriteRune(c)
		}
		h = b.String()
	}
	if len(h) == 6 {
		h += "ff"
	}
	if len(h) != 8 {
		return rgba{}, fmt.Errorf("bad hex colour %q", e)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return rgba{}, fmt.Errorf("bad hex colour %q", e)
	}
	c := func(shift uint) float64 { return float64(v>>shift&0xff) / 255 }
	return rgba{c(24), c(16), c(8), c(0)}, nil
}

// resolveColorMix implements color-mix(in srgb, A p%, B [q%]) as CSS Color 5
// defines it: premultiplied interpolation, so mixing with transparent keeps
// the hue and scales the alpha.
func resolveColorMix(e string, tokens map[string]string, depth int) (rgba, error) {
	args := splitTopLevel(e[len("color-mix("):len(e)-1], ',')
	if len(args) != 3 || strings.ToLower(args[0]) != "in srgb" {
		return rgba{}, fmt.Errorf("only color-mix(in srgb, a, b) is supported: %q", e)
	}
	split := func(arg string) (string, float64, bool, error) {
		f := strings.Fields(arg)
		last := f[len(f)-1]
		if strings.HasSuffix(last, "%") {
			p, err := strconv.ParseFloat(strings.TrimSuffix(last, "%"), 64)
			return strings.Join(f[:len(f)-1], " "), p / 100, true, err
		}
		return arg, 0, false, nil
	}
	ca, pa, hasA, err := split(args[1])
	if err != nil {
		return rgba{}, err
	}
	cb, pb, hasB, err := split(args[2])
	if err != nil {
		return rgba{}, err
	}
	switch {
	case hasA && !hasB:
		pb = 1 - pa
	case hasB && !hasA:
		pa = 1 - pb
	case !hasA && !hasB:
		pa, pb = .5, .5
	}
	a, err := resolveColor(ca, tokens, depth+1)
	if err != nil {
		return rgba{}, err
	}
	b, err := resolveColor(cb, tokens, depth+1)
	if err != nil {
		return rgba{}, err
	}
	// Normalise if the percentages do not sum to 100%.
	sum := pa + pb
	pa, pb = pa/sum, pb/sum
	alpha := a.a*pa + b.a*pb
	if alpha == 0 {
		return rgba{}, nil
	}
	mix := func(x, y float64) float64 { return (x*a.a*pa + y*b.a*pb) / alpha }
	return rgba{mix(a.r, b.r), mix(a.g, b.g), mix(a.b, b.b), alpha}, nil
}

func over(top, bottom rgba) rgba {
	f := func(t, b float64) float64 { return t*top.a + b*(1-top.a) }
	return rgba{f(top.r, bottom.r), f(top.g, bottom.g), f(top.b, bottom.b), 1}
}

// relativeLuminance is the WCAG 2.x definition. The linearisation threshold is
// the sRGB standard's 0.04045; WCAG's text quotes 0.03928, and no 8-bit channel
// value falls between the two.
func relativeLuminance(c rgba) float64 {
	lin := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

func contrastRatio(a, b rgba) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// --- the pairings --------------------------------------------------------------

const (
	textMin    = 4.5 // WCAG 1.4.3, normal-size text
	nonTextMin = 3.0 // WCAG 1.4.11, UI components and graphical objects
)

type pairing struct {
	fg   string   // the foreground colour, as the stylesheet writes it
	bg   []string // background layers, bottom first; the first must be opaque
	min  float64
	uses string // where the stylesheet puts this foreground on this background
}

// Derived by reading console.css rule by rule. Every foreground colour the
// stylesheet assigns with `color:` appears here or in colorExemptions — the
// coverage check in TestTokensMeetContrast holds that — so a new text colour
// cannot arrive without its pairings being written down.
//
// Hover is measured; :active is not. A pressed state lasts as long as the
// pointer is held down, and nothing is read in it; hover is where a pointer
// rests while the row under it is read.
//
// Badge text is held to 4.5:1, not 3:1: the LOCAL badge and the notification
// count are 10px, and WCAG's 3:1 allowance is for large text only.
var contrastPairings = []pairing{
	// Body and content text.
	{"var(--text)", []string{"var(--bg)"}, textMin,
		"body text and page titles on the page background"},
	{"var(--text)", []string{"var(--surface)"}, textMin,
		"cards, tables, panels, menus, dialogs, form fields, the navigation drawer"},
	{"var(--text)", []string{"var(--surface-alt)"}, textMin,
		".search input, kbd, .reveal-value, .state pre, .pg-output"},
	{"var(--text)", []string{"var(--surface)", "var(--state-hover)"}, textMin,
		"a hovered row, nav item, menu item, picker item, tab or search hit"},
	{"var(--text)", []string{"var(--surface)", "var(--nav-selected)"}, textMin,
		"tbody tr.is-selected and tr.is-inspected rows"},
	{"var(--text)", []string{"var(--surface)", "color-mix(in srgb, var(--accent) 8%, transparent)"}, textMin,
		".suggest-row:hover / .is-active in the search suggestions"},
	{"var(--text)", []string{"var(--surface)", "color-mix(in srgb, var(--warn) 12%, transparent)"}, textMin,
		".discard-prompt inside a dialog"},

	// Secondary text.
	{"var(--text-muted)", []string{"var(--bg)"}, textMin,
		".subtitle, .breadcrumb, .table-footer, .still-loading on the page background"},
	{"var(--text-muted)", []string{"var(--surface)"}, textMin,
		"th, dt, form labels and help, .nav-section, .product-nav a, .tab, .confirm-detail, stale meter text"},
	{"var(--text-muted)", []string{"var(--surface-alt)"}, textMin,
		"tr.is-operating td"},
	{"var(--text-muted)", []string{"var(--surface)", "var(--state-hover)"}, textMin,
		".picker-item-id, .search-hit-detail and muted cells inside a hovered item or row"},
	{"var(--text-muted)", []string{"var(--surface)", "color-mix(in srgb, var(--accent) 8%, transparent)"}, textMin,
		".suggest-kind in the highlighted suggestion"},

	// Links and accent-coloured labels.
	{"var(--accent)", []string{"var(--bg)"}, textMin,
		".breadcrumb a, secondary buttons in .page-actions and .action-bar"},
	{"var(--accent)", []string{"var(--surface)"}, textMin,
		"td a, .search-hit-name, .tab.is-selected, .nav-all, .sort-button.is-active, dialog secondary buttons"},
	{"var(--accent)", []string{"var(--surface)", "var(--nav-selected)"}, textMin,
		".product-nav a.is-current, links in a selected or inspected row"},
	{"var(--accent)", []string{"var(--bg)", "color-mix(in srgb, var(--accent) 6%, transparent)"}, textMin,
		"button.secondary:hover and a.button(-link).secondary:hover on the page"},
	{"var(--accent)", []string{"var(--surface)", "color-mix(in srgb, var(--accent) 6%, transparent)"}, textMin,
		"button.secondary:hover inside a dialog or card"},

	// Filled controls.
	{"var(--accent-contrast)", []string{"var(--accent)"}, textMin,
		"button.primary, .avatar, the skip link"},
	{"var(--accent-contrast)", []string{"color-mix(in srgb, var(--state-ink) 8%, var(--accent))"}, textMin,
		"button.primary:hover"},
	{"var(--nav-selected-text)", []string{"var(--nav-selected)"}, textMin,
		"#nav a[aria-current=page], .chip, .picker-item.is-selected and its .picker-item-id " +
			"(which inherits the colour; at the opacity .8 it once had, 3.35:1 in light)"},

	// Error and warning text.
	{"var(--error)", []string{"var(--surface)"}, textMin,
		".state.error h2, .form-field-error in a dialog, button.danger, .overflow-item.is-destructive, a.op-detail, .required-mark"},
	{"var(--error)", []string{"var(--bg)"}, textMin,
		".form-field-error on the full-page create form, button.danger in the action bar"},
	{"var(--error)", []string{"var(--surface)", "color-mix(in srgb, var(--error) 12%, transparent)"}, textMin,
		".form-error inside a dialog"},
	{"var(--error)", []string{"var(--bg)", "color-mix(in srgb, var(--error) 12%, transparent)"}, textMin,
		".form-error on the full-page create form"},
	{"var(--warn)", []string{"var(--bg)"}, textMin,
		".table-freshness.is-stale in the filter bar"},
	{"var(--warn)", []string{"var(--surface)"}, textMin,
		".metrics-stale in the dashboard card"},

	// Badges and bars on a container colour.
	{"var(--on-warn)", []string{"var(--warn)"}, textMin,
		".local-badge (10px bold: small text, so 4.5:1)"},
	{"var(--on-error)", []string{"var(--error)"}, textMin,
		".badge (10px), .shell-error, button.primary.danger, .snackbar.is-error"},
	{"var(--error)", []string{"var(--on-error)"}, textMin,
		".shell-error button"},
	{"#fff", []string{"#323232"}, textMin,
		".snackbar, the same fixed colours in both themes"},

	// Non-text: focus indicators (WCAG 1.4.11 / 2.4.11) against what they sit on.
	{"var(--accent)", []string{"var(--bg)"}, nonTextMin,
		":focus-visible ring on the page background"},
	{"var(--accent)", []string{"var(--surface)"}, nonTextMin,
		":focus-visible ring on cards, tables, dialogs, the toolbar and the drawer"},
	{"var(--accent)", []string{"var(--surface-alt)"}, nonTextMin,
		"row focus ring on tr.is-operating; .meter-fill against .meter-track"},
	{"var(--accent)", []string{"var(--surface)", "var(--nav-selected)"}, nonTextMin,
		"inset row focus ring on a selected or inspected row"},
	{"var(--error)", []string{"var(--surface)"}, nonTextMin,
		".form-row.is-invalid input:focus ring, #project-button.is-invalid"},
	{"var(--error)", []string{"var(--surface-alt)"}, nonTextMin,
		".meter-fill.is-high against .meter-track"},

	// Non-text: status marks. Each carries a label too, but the dot is the
	// part that is scanned for.
	{"var(--ok)", []string{"var(--surface)"}, nonTextMin, ".status[data-state=ok]::before, .timeline-bar"},
	{"var(--warn)", []string{"var(--surface)"}, nonTextMin, ".status[data-state=warn]::before, .timeline-bar.is-warn"},
	{"var(--error)", []string{"var(--surface)"}, nonTextMin, ".status[data-state=error]::before, .timeline-bar.is-error"},
}

// Foreground colours the stylesheet uses that are deliberately not held to a
// minimum, each with the reason WCAG gives.
var colorExemptions = map[string]string{
	"var(--disabled-label)": "the label of a disabled control: WCAG 1.4.3 exempts inactive components",
}

// Not measured, and why:
//   - --border and the dividers built from it are decorative separators;
//     input boundaries would need 3:1 under a strict reading of 1.4.11, which
//     is a palette decision (#dadce0 measures ~1.4:1 on white), not a test fix.
//   - opacity on disabled table-footer buttons and on a row being dragged:
//     inactive and transient respectively.
//   - .timeline-bar.is-muted uses --border on purpose: an empty bucket.

func contrastFailures(css string) ([]string, []string, error) {
	rules, err := parseCSS(css)
	if err != nil {
		return nil, nil, err
	}
	themes, err := themeTokens(rules)
	if err != nil {
		return nil, nil, err
	}
	var failures, report []string
	for _, th := range themes {
		for _, p := range contrastPairings {
			fg, err := resolveColor(p.fg, th.tokens, 0)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %v", th.name, err)
			}
			var bg rgba
			for k, layer := range p.bg {
				c, err := resolveColor(layer, th.tokens, 0)
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %v", th.name, err)
				}
				if k == 0 {
					if c.a < 1 {
						return nil, nil, fmt.Errorf("%s: base layer %s is not opaque", th.name, layer)
					}
					bg = c
					continue
				}
				bg = over(c, bg)
			}
			ratio := contrastRatio(over(fg, bg), bg)
			line := fmt.Sprintf("%-14s %5.2f:1 (min %.1f)  %s on %s  — %s",
				th.name, ratio, p.min, p.fg, strings.Join(p.bg, " + "), p.uses)
			report = append(report, line)
			if ratio < p.min {
				failures = append(failures, line)
			}
		}
	}
	return failures, report, nil
}

var cssVarName = regexp.MustCompile(`var\((--[a-z0-9-]+)`)

// TestTokensMeetContrast resolves both theme token blocks (and the device dark
// block that duplicates the explicit one) and computes the WCAG 2.x contrast
// ratio of every pairing in contrastPairings. Nothing here was measured before
// it existed: docs/console-parity.md left the box unchecked for that reason.
func TestTokensMeetContrast(t *testing.T) {
	css := consoleAsset(t, "console.css")
	failures, report, err := contrastFailures(css)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range report {
		t.Log(line)
	}
	for _, f := range failures {
		t.Errorf("below the contrast minimum: %s", f)
	}

	// Coverage: every `color:` the stylesheet sets is a foreground above, or
	// exempt with a reason. A colour outside the table would be unmeasured.
	rules, err := parseCSS(css)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, p := range contrastPairings {
		covered[p.fg] = true
		for _, m := range cssVarName.FindAllStringSubmatch(p.fg, -1) {
			covered["var("+m[1]+")"] = true
		}
	}
	for fg := range colorExemptions {
		covered[fg] = true
	}
	var missing []string
	for _, r := range rules {
		for _, d := range r.decls {
			if d.prop != "color" {
				continue
			}
			switch strings.ToLower(d.value) {
			case "inherit", "currentcolor":
				continue
			}
			if !covered[d.value] {
				missing = append(missing, fmt.Sprintf("%s { color: %s }", strings.Join(r.selectors, ", "), d.value))
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("foreground colour with no pairing in contrastPairings: %s", m)
	}
}

// TestContrastCheckCatchesALightenedMutedText holds the check to the number it
// exists for: a light --text-muted lightened to #9aa0a6 measures about 2.6:1
// on white, and the light theme must fail on it.
func TestContrastCheckCatchesALightenedMutedText(t *testing.T) {
	css := consoleAsset(t, "console.css")
	const light = "--text-muted: #5f6368;"
	if !strings.Contains(css, light) {
		t.Fatalf("console.css no longer declares %q; update this test with the new light value", light)
	}
	mutated := strings.Replace(css, light, "--text-muted: #9aa0a6;", 1)
	failures, _, err := contrastFailures(mutated)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range failures {
		if strings.HasPrefix(f, "light ") && strings.Contains(f, "var(--text-muted) on var(--surface) ") {
			return
		}
	}
	t.Errorf("a light --text-muted of #9aa0a6 on white passed the contrast check; failures were %q", failures)
}

func TestContrastRatioMatchesTheWCAGReferenceValues(t *testing.T) {
	black, white := rgba{0, 0, 0, 1}, rgba{1, 1, 1, 1}
	if got := contrastRatio(black, white); math.Abs(got-21) > 1e-9 {
		t.Errorf("black on white = %.4f, want 21", got)
	}
	grey, _ := parseHex("#777777")
	if got := contrastRatio(grey, white); math.Abs(got-4.48) > 0.01 {
		t.Errorf("#777 on white = %.4f, want 4.48", got)
	}
	mixed, err := resolveColor("color-mix(in srgb, #000000 50%, transparent)", nil, 0)
	if err != nil || math.Abs(mixed.a-.5) > 1e-9 || mixed.r != 0 {
		t.Errorf("color-mix with transparent = %+v, %v; want black at alpha .5", mixed, err)
	}
}

// --- focus indicators -----------------------------------------------------------

var focusPseudo = regexp.MustCompile(`:focus\b(?:[^-]|$)`)

func suppressesOutline(d cssDecl) bool {
	v := strings.ToLower(d.value)
	switch d.prop {
	case "outline":
		return v == "none" || v == "0" || v == "0px" || strings.HasPrefix(v, "none ") || strings.HasPrefix(v, "0 ")
	case "outline-style":
		return v == "none"
	case "outline-width":
		return v == "0" || v == "0px"
	}
	return false
}

func drawsOutline(decls []cssDecl) bool {
	styles := map[string]bool{"solid": true, "dashed": true, "dotted": true, "double": true,
		"groove": true, "ridge": true, "inset": true, "outset": true, "auto": true}
	for _, d := range decls {
		switch d.prop {
		case "outline":
			visible := false
			for _, f := range strings.Fields(strings.ToLower(d.value)) {
				if f == "0" || f == "0px" || f == "none" {
					visible = false
					break
				}
				if styles[f] {
					visible = true
				}
			}
			if visible {
				return true
			}
		case "outline-style":
			if styles[strings.ToLower(d.value)] {
				return true
			}
		}
	}
	return false
}

// atPrefix reports whether a rule inside `outer` applies wherever a rule
// inside `inner` does: the same conditions, or fewer.
func atPrefix(outer, inner []string) bool {
	if len(outer) > len(inner) {
		return false
	}
	for k := range outer {
		if outer[k] != inner[k] {
			return false
		}
	}
	return true
}

// focusFailures returns every selector that suppresses the outline without a
// :focus-visible rule that restores one, and a line per selector checked.
func focusFailures(css string) ([]string, []string, error) {
	rules, err := parseCSS(css)
	if err != nil {
		return nil, nil, err
	}
	global := false
	for _, r := range rules {
		if len(r.at) == 0 && len(r.selectors) == 1 && r.selectors[0] == ":focus-visible" && drawsOutline(r.decls) {
			global = true
		}
	}
	var failures, report []string
	if !global {
		failures = append(failures, "no global :focus-visible rule draws an outline")
	}
	for _, r := range rules {
		suppressing := false
		for _, d := range r.decls {
			if suppressesOutline(d) {
				suppressing = true
			}
		}
		if !suppressing {
			continue
		}
		for _, s := range r.selectors {
			switch {
			case strings.Contains(s, ":not(:focus-visible)"):
				// Only the non-keyboard case is suppressed; the global rule
				// draws the ring whenever :focus-visible matches.
				if global {
					report = append(report, fmt.Sprintf("%s → the global :focus-visible rule", s))
				} else {
					failures = append(failures, s+" relies on a global :focus-visible rule that does not exist")
				}
				continue
			case strings.Contains(s, ":focus-visible"):
				failures = append(failures, s+" suppresses the outline on :focus-visible itself")
				continue
			}
			// The partner is the same selector with :focus-visible where
			// :focus was, or appended when the rule has no :focus. Replacing
			// keeps the specificity equal, so the partner must also come later.
			want, sameSpecificity := s+":focus-visible", false
			if loc := focusPseudo.FindStringIndex(s); loc != nil {
				want, sameSpecificity = s[:loc[0]]+":focus-visible"+s[loc[0]+len(":focus"):], true
			}
			found := false
			for _, r2 := range rules {
				if !atPrefix(r2.at, r.at) || !drawsOutline(r2.decls) {
					continue
				}
				if sameSpecificity && r2.index < r.index {
					continue
				}
				for _, s2 := range r2.selectors {
					if s2 == want {
						found = true
					}
				}
			}
			if found {
				report = append(report, fmt.Sprintf("%s → %s", s, want))
			} else {
				failures = append(failures, fmt.Sprintf("%s sets outline: none with no later %s rule drawing an outline", s, want))
			}
		}
	}
	return failures, report, nil
}

// TestOutlineSuppressionHasAFocusVisibleRule enumerates every selector that
// sets outline: none (or 0) and requires a :focus-visible rule for the same
// selector that draws a visible outline and wins the cascade. `.search
// input:focus` outranked the global :focus-visible rule, so the toolbar's
// search field — the most-used control in the console — had no focus ring.
func TestOutlineSuppressionHasAFocusVisibleRule(t *testing.T) {
	failures, report, err := focusFailures(consoleAsset(t, "console.css"))
	if err != nil {
		t.Fatal(err)
	}
	if len(report) == 0 && len(failures) == 0 {
		t.Log("no rule suppresses the focus outline")
	}
	for _, line := range report {
		t.Log(line)
	}
	for _, f := range failures {
		t.Error(f)
	}
}

func TestOutlineCheckCatchesASuppressedRing(t *testing.T) {
	css := consoleAsset(t, "console.css") + "\n.probe input:focus { outline: none; }\n"
	failures, _, err := focusFailures(css)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || !strings.Contains(failures[0], ".probe input:focus ") {
		t.Errorf("an outline: none with no :focus-visible partner was not reported alone; got %q", failures)
	}
	// And a partner that comes first loses the cascade, so it does not count.
	css = consoleAsset(t, "console.css") +
		"\n.probe:focus-visible { outline: 2px solid red; }\n.probe:focus { outline: none; }\n"
	if failures, _, _ := focusFailures(css); len(failures) != 1 {
		t.Errorf("a :focus-visible partner declared before the suppression was accepted; got %q", failures)
	}
}
