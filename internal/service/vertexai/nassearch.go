package vertexai

// Neural Architecture Search's search space and controller (#2214,
// nasjobs.go runs the trials). A NasJob's search_space_spec is a PyGlove
// search space as JSON: Google's NAS client builds it with
// `search_space.to_json_str(hide_default_values=True)` (vertex_nas_cli.py,
// github.com/google/vertex-ai-nas, Apache-2.0, read 2026-11-24). Its
// decision points are PyGlove's hyper primitives, serialized with the
// "_type" keys "hyper.OneOf", "hyper.ManyOf" and "hyper.Float"
// (pyglove/core/hyper/categorical.py and numerical.py: candidates,
// num_choices, choices_distinct, choices_sorted; min_value, max_value,
// scale), lists as arrays and tuples as ["__tuple__", ...].
//
// Each trial gets the sampled architecture in --nas_params_str: "The ...
// Neural Architecture Search Controller will provide each trial with a
// suggestion for a new architecture candidate through the FLAG
// --nas_params_str" (pre-built-trainer); the NAS library decodes it with
// json.loads and pg.materialize. Its keys are the decision points' IDs
// (PyGlove's DNASpec.id: a field path, a.b[0], with a conditional step
// [=i/n] inside a chosen candidate), its values PyGlove's
// DNA.to_dict(value_type='choice_and_literal') form, '{index}/{count}
// ({literal})', a float's its number, and a ManyOf's choices keyed
// ID[j].
//
// UNVERIFIED (Google's controller is not public; re-read 2026-11-24): the
// reinforcement learning controller (here REINFORCE over a softmax per
// decision, and a Gaussian over a float's unit range, with a moving
// average baseline, seeded by the job's name); a grid's order (the
// decisions' product, the first slowest; floats are refused); a
// candidate's literal (Python's repr of a number, string or boolean, an
// object's compact form) and the parameters' order (depth first).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// nasDecision is one decision point of a search space.
type nasDecision struct {
	id   string
	kind string // "oneof", "manyof" or "float"
	// candidates are a categorical decision's, their literals and, for
	// each, the decisions inside it.
	candidates []any
	literals   []string
	children   [][]*nasDecision
	num        int
	distinct   bool
	sorted     bool
	lo, hi     float64
	scale      string
}

func hyperType(m map[string]any) string {
	t, _ := m["_type"].(string)
	switch {
	case strings.HasSuffix(t, "hyper.OneOf"):
		return "oneof"
	case strings.HasSuffix(t, "hyper.ManyOf"):
		return "manyof"
	case strings.HasSuffix(t, "hyper.Float"):
		return "float"
	case strings.Contains(t, "hyper.") && !strings.HasSuffix(t, "hyper.ObjectTemplate"):
		return "unsupported:" + t
	}
	return ""
}

// parseSearchSpace reads a search_space_spec's decision points.
func parseSearchSpace(spec string) ([]*nasDecision, error) {
	var root any
	d := json.NewDecoder(strings.NewReader(spec))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		return nil, apierror.InvalidArgument("nas_job_spec.search_space_spec is not JSON (a PyGlove search space's to_json_str()): %v", err)
	}
	ds, err := walkSpace(root, "")
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		return nil, apierror.InvalidArgument("nas_job_spec.search_space_spec has no decision points: no pg.oneof, pg.manyof or pg.floatv")
	}
	return ds, nil
}

func joinPath(base, key string) string {
	if base == "" || strings.HasSuffix(base, "]") && strings.HasPrefix(key, "[") {
		return base + key
	}
	if strings.HasPrefix(key, "[") {
		return base + key
	}
	return base + "." + key
}

func walkSpace(v any, loc string) ([]*nasDecision, error) {
	switch x := v.(type) {
	case map[string]any:
		switch k := hyperType(x); {
		case strings.HasPrefix(k, "unsupported:"):
			return nil, apierror.InvalidArgument("search_space_spec at %q: the hyper primitive %s is not supported: pg.oneof, pg.manyof, pg.permutate and pg.floatv are", loc, strings.TrimPrefix(k, "unsupported:"))
		case k == "float":
			lo, _ := numberOf(x["min_value"])
			hi, _ := numberOf(x["max_value"])
			if !(lo < hi) {
				return nil, apierror.InvalidArgument("search_space_spec at %q: min_value must be less than max_value", loc)
			}
			sc, _ := x["scale"].(string)
			if (sc == "log" || sc == "rlog") && lo <= 0 {
				return nil, apierror.InvalidArgument("search_space_spec at %q: a %s scale needs min_value above 0", loc, sc)
			}
			return []*nasDecision{{id: loc, kind: "float", lo: lo, hi: hi, scale: sc}}, nil
		case k == "oneof" || k == "manyof":
			cands, _ := x["candidates"].([]any)
			cands = untuple(cands)
			if len(cands) == 0 {
				return nil, apierror.InvalidArgument("search_space_spec at %q: candidates is empty", loc)
			}
			d := &nasDecision{id: loc, kind: k, candidates: cands, num: 1, distinct: true}
			if k == "manyof" {
				n, _ := numberOf(x["num_choices"])
				d.num = int(n)
				d.distinct = x["choices_distinct"] != false
				d.sorted = x["choices_sorted"] == true
				if d.num < 1 || d.distinct && d.num > len(cands) {
					return nil, apierror.InvalidArgument("search_space_spec at %q: num_choices %d does not fit %d distinct candidates", loc, d.num, len(cands))
				}
			}
			for i, c := range cands {
				d.literals = append(d.literals, literalOf(c))
				sub, err := walkSpace(c, loc+"[="+strconv.Itoa(i)+"/"+strconv.Itoa(len(cands))+"]")
				if err != nil {
					return nil, err
				}
				d.children = append(d.children, sub)
			}
			return []*nasDecision{d}, nil
		}
		var out []*nasDecision
		keys := make([]string, 0, len(x))
		for k := range x {
			if k != "_type" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, err := walkSpace(x[k], joinPath(loc, k))
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	case []any:
		var out []*nasDecision
		for i, e := range untuple(x) {
			sub, err := walkSpace(e, joinPath(loc, "["+strconv.Itoa(i)+"]"))
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	}
	return nil, nil
}

func untuple(l []any) []any {
	if len(l) > 0 && l[0] == "__tuple__" {
		return l[1:]
	}
	return l
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

// literalOf is a candidate's literal, as Python would print it.
func literalOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return x.String()
	case string:
		return "'" + strings.ReplaceAll(strings.ReplaceAll(x, `\`, `\\`), "'", `\'`) + "'"
	case []any:
		var parts []string
		for _, e := range untuple(x) {
			parts = append(parts, literalOf(e))
		}
		if len(x) > 0 && x[0] == "__tuple__" {
			return "(" + strings.Join(parts, ", ") + ")"
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		t, _ := x["_type"].(string)
		var keys []string
		for k := range x {
			if k != "_type" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+"="+literalOf(x[k]))
		}
		if t == "" {
			return "{" + strings.Join(parts, ", ") + "}"
		}
		return t[strings.LastIndex(t, ".")+1:] + "(" + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprint(v)
}

// nasChoice is one sampled decision: a categorical's chosen candidates,
// or a float's value.
type nasChoice struct {
	d     *nasDecision
	picks []int
	value float64
}

// nasArch is a sampled architecture: its active decisions, depth first.
type nasArch []nasChoice

// params is the architecture as --nas_params_str's JSON object.
func (a nasArch) params() string {
	var b bytes.Buffer
	b.WriteByte('{')
	n := 0
	put := func(k string, v any) {
		if n > 0 {
			b.WriteString(", ")
		}
		n++
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(v)
		b.Write(kb)
		b.WriteString(": ")
		b.Write(vb)
	}
	for _, c := range a {
		switch c.d.kind {
		case "float":
			put(c.d.id, c.value)
		case "oneof":
			put(c.d.id, c.d.choiceText(c.picks[0]))
		case "manyof":
			for j, p := range c.picks {
				put(c.d.id+"["+strconv.Itoa(j)+"]", c.d.choiceText(p))
			}
		}
	}
	b.WriteByte('}')
	return b.String()
}

func (d *nasDecision) choiceText(i int) string {
	return fmt.Sprintf("%d/%d (%s)", i, len(d.candidates), d.literals[i])
}

// key identifies an architecture.
func (a nasArch) key() string { return a.params() }

// ---- controllers

// nasController suggests architectures and learns from their rewards.
type nasController interface {
	suggest() (nasArch, bool)
	observe(a nasArch, reward float64)
}

func newNASController(name string, algo aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MultiTrialAlgorithm, space []*nasDecision, maxTrials int) (nasController, error) {
	if algo == aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH {
		archs, err := gridArchs(space, maxTrials)
		if err != nil {
			return nil, err
		}
		return &gridController{archs: archs}, nil
	}
	seed := uint64(0)
	for _, c := range name {
		seed = seed*131 + uint64(c)
	}
	return &rlController{space: space, rng: rand.New(rand.NewPCG(seed, 0x2214)), logits: map[string][]float64{}, means: map[string]float64{}, seen: map[string]bool{}}, nil
}

type gridController struct {
	archs []nasArch
	next  int
}

func (g *gridController) suggest() (nasArch, bool) {
	if g.next >= len(g.archs) {
		return nil, false
	}
	g.next++
	return g.archs[g.next-1], true
}

func (g *gridController) observe(nasArch, float64) {}

// gridArchs is the product of the space's choices, up to limit.
func gridArchs(space []*nasDecision, limit int) ([]nasArch, error) {
	var out []nasArch
	var rec func(todo []*nasDecision, cur nasArch) error
	rec = func(todo []*nasDecision, cur nasArch) error {
		if len(out) >= limit {
			return nil
		}
		if len(todo) == 0 {
			out = append(out, append(nasArch(nil), cur...))
			return nil
		}
		d := todo[0]
		switch d.kind {
		case "float":
			return apierror.InvalidArgument("GRID_SEARCH needs every decision categorical: %q is a pg.floatv", d.id)
		case "manyof":
			for _, picks := range combos(len(d.candidates), d.num, d.distinct, d.sorted) {
				var next []*nasDecision
				for _, p := range picks {
					next = append(next, d.children[p]...)
				}
				if err := rec(append(next, todo[1:]...), append(cur, nasChoice{d: d, picks: picks})); err != nil {
					return err
				}
			}
		default:
			for i := range d.candidates {
				if err := rec(append(append([]*nasDecision{}, d.children[i]...), todo[1:]...), append(cur, nasChoice{d: d, picks: []int{i}})); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := rec(space, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// combos are the ways to pick k of n candidates.
func combos(n, k int, distinct, sorted bool) [][]int {
	var out [][]int
	var rec func(cur []int)
	rec = func(cur []int) {
		if len(out) > 100000 {
			return
		}
		if len(cur) == k {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := 0; i < n; i++ {
			if distinct && containsInt(cur, i) || sorted && len(cur) > 0 && i < cur[len(cur)-1] {
				continue
			}
			rec(append(cur, i))
		}
	}
	rec(nil)
	return out
}

// rlController is REINFORCE over the decisions.
type rlController struct {
	space    []*nasDecision
	rng      *rand.Rand
	logits   map[string][]float64 // by decision id (and subchoice)
	means    map[string]float64   // a float's mean, in its unit range
	baseline float64
	started  bool
	seen     map[string]bool
}

const (
	rlRate  = 0.5
	rlSigma = 0.2
)

func softmax(l []float64) []float64 {
	m := math.Inf(-1)
	for _, x := range l {
		m = math.Max(m, x)
	}
	out := make([]float64, len(l))
	t := 0.0
	for i, x := range l {
		out[i] = math.Exp(x - m)
		t += out[i]
	}
	for i := range out {
		out[i] /= t
	}
	return out
}

func (c *rlController) logitsOf(key string, n int) []float64 {
	l := c.logits[key]
	if len(l) != n {
		l = make([]float64, n)
		c.logits[key] = l
	}
	return l
}

func (c *rlController) sample() nasArch {
	var arch nasArch
	var rec func(ds []*nasDecision)
	rec = func(ds []*nasDecision) {
		for _, d := range ds {
			switch d.kind {
			case "float":
				m, ok := c.means[d.id]
				if !ok {
					m = 0.5
				}
				u := math.Min(1, math.Max(0, m+c.rng.NormFloat64()*rlSigma))
				arch = append(arch, nasChoice{d: d, value: fromUnitFloat(d, u)})
			default:
				var picks []int
				for j := 0; j < d.num; j++ {
					p := softmax(c.logitsOf(d.id+"#"+strconv.Itoa(j), len(d.candidates)))
					for {
						r, i := c.rng.Float64(), 0
						for ; i < len(p)-1 && r > p[i]; i++ {
							r -= p[i]
						}
						if !d.distinct || !containsInt(picks, i) {
							picks = append(picks, i)
							break
						}
					}
				}
				if d.sorted {
					sort.Ints(picks)
				}
				arch = append(arch, nasChoice{d: d, picks: picks})
				for _, p := range picks {
					rec(d.children[p])
				}
			}
		}
	}
	rec(c.space)
	return arch
}

func (c *rlController) suggest() (nasArch, bool) {
	var a nasArch
	for range 20 { // prefer an architecture not tried yet
		a = c.sample()
		if !c.seen[a.key()] {
			break
		}
	}
	c.seen[a.key()] = true
	return a, true
}

func (c *rlController) observe(a nasArch, reward float64) {
	if !c.started {
		c.baseline, c.started = reward, true
		return
	}
	adv := reward - c.baseline
	c.baseline = 0.9*c.baseline + 0.1*reward
	for _, ch := range a {
		if ch.d.kind == "float" {
			m, ok := c.means[ch.d.id]
			if !ok {
				m = 0.5
			}
			c.means[ch.d.id] = math.Min(1, math.Max(0, m+rlRate*adv*(toUnitFloat(ch.d, ch.value)-m)))
			continue
		}
		for j, pick := range ch.picks {
			l := c.logitsOf(ch.d.id+"#"+strconv.Itoa(j), len(ch.d.candidates))
			p := softmax(l)
			for i := range l {
				g := -p[i]
				if i == pick {
					g += 1
				}
				l[i] += rlRate * adv * g
			}
		}
	}
}

func fromUnitFloat(d *nasDecision, u float64) float64 {
	switch d.scale {
	case "log":
		return math.Exp(math.Log(d.lo) + u*(math.Log(d.hi)-math.Log(d.lo)))
	case "rlog":
		return d.hi + d.lo - math.Exp(math.Log(d.lo)+(1-u)*(math.Log(d.hi)-math.Log(d.lo)))
	}
	return d.lo + u*(d.hi-d.lo)
}

func toUnitFloat(d *nasDecision, v float64) float64 {
	switch d.scale {
	case "log":
		return (math.Log(v) - math.Log(d.lo)) / (math.Log(d.hi) - math.Log(d.lo))
	case "rlog":
		return 1 - (math.Log(d.hi+d.lo-v)-math.Log(d.lo))/(math.Log(d.hi)-math.Log(d.lo))
	}
	return (v - d.lo) / (d.hi - d.lo)
}
