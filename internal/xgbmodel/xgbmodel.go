// Package xgbmodel reads and runs XGBoost models in pure Go (#1711), for
// BigQuery ML's imported XGBoost models (CREATE MODEL … OPTIONS
// (MODEL_TYPE='XGBOOST'),
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-xgboost,
// re-read 2026-11-17: "BigQuery ML uses XGBoost 3.1.0", models in "BST,
// UBJ, or JSON" format).
//
// It reads the model document XGBoost's save_model writes, as XGBoost
// documents it (https://xgboost.readthedocs.io/en/stable/tutorials/saving_model.html
// and its model schema, doc/model.schema): JSON (.json), or the same
// document in Universal Binary JSON (.ubj, and a .bst saved by XGBoost 2.1
// or later, which writes UBJSON whatever the extension). XGBoost's older
// binary format, which 3.x no longer writes, is refused.
//
// Prediction is XGBoost's (src/predictor/predict_fn.h, src/gbm,
// src/objective): each tree is walked from its root, a missing value (NaN)
// taking the node's default direction, a numerical split going left when
// the value is below the split condition, a categorical one going right
// when the value's category is in the node's set; the leaves' values are
// summed in float32 per output group (a class, or a target) onto the base
// margin, a DART tree's scaled by its weight_drop, and the objective's
// transform gives the prediction. gblinear models are their weights' dot
// product plus bias. It matches XGBoost 3.1.0's Booster.predict on the
// models in testdata (xgbmodel_test.go).
package xgbmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Model is a loaded XGBoost model.
type Model struct {
	// FeatureNames and FeatureTypes are the model's feature_names and
	// feature_types, empty when it was saved without them.
	FeatureNames []string
	FeatureTypes []string
	// NumFeature is learner_model_param.num_feature.
	NumFeature int
	// Objective is the objective's name, such as reg:squarederror.
	Objective string
	// Booster is gbtree, dart or gblinear.
	Booster string
	// Version is the XGBoost version that saved it.
	Version [3]int

	groups    int
	base      []float32 // the base margin of each group
	trees     []tree
	group     []int     // each tree's output group
	weight    []float32 // each tree's weight (DART), 1 otherwise
	linear    []float32 // gblinear: (NumFeature+1)*groups weights
	leafWidth int       // a multi-target tree's leaf vector size
}

type tree struct {
	left, right []int32
	feat        []int32
	cond        []float32
	defLeft     []bool
	categorical []bool
	cats        map[int32]map[int32]bool
	leaf        []float32 // base_weights, for a vector leaf
	gain, cover []float64
}

// Load reads a model saved by XGBoost's save_model as JSON or UBJSON.
func Load(data []byte) (*Model, error) {
	var doc any
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	switch {
	case len(trimmed) > 0 && trimmed[0] == '{' && len(trimmed) > 1 && (trimmed[1] == '"' || trimmed[1] == ' ' || trimmed[1] == '\n' || trimmed[1] == '\r' || trimmed[1] == '\t' || trimmed[1] == '}'):
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("the model is not valid JSON: %v", err)
		}
	case len(data) > 0 && data[0] == '{':
		v, err := decodeUBJ(data)
		if err != nil {
			return nil, fmt.Errorf("the model is not valid UBJSON: %v", err)
		}
		doc = v
	default:
		return nil, errors.New("the model is in XGBoost's old binary format, which XGBoost 3.x does not read; save it with XGBoost 2.0 or later as JSON or UBJ")
	}
	return parse(doc)
}

type obj map[string]any

func asObj(v any) obj {
	m, _ := v.(map[string]any)
	return m
}

func (o obj) obj(k string) obj { return asObj(o[k]) }

func (o obj) list(k string) []any {
	l, _ := o[k].([]any)
	return l
}

func (o obj) str(k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return ""
}

func (o obj) int(k string) int {
	s := o.str(k)
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseFloat(s, 64)
	return int(n)
}

func nums(l []any) ([]float64, error) {
	out := make([]float64, len(l))
	for i, v := range l {
		switch x := v.(type) {
		case float64:
			out[i] = x
		case bool:
			if x {
				out[i] = 1
			}
		default:
			return nil, fmt.Errorf("a non-numeric value %v", v)
		}
	}
	return out, nil
}

func parse(doc any) (*Model, error) {
	root := asObj(doc)
	learner := root.obj("learner")
	if learner == nil {
		return nil, errors.New("the model has no learner: it is not an XGBoost model document")
	}
	m := &Model{}
	for i, v := range root.list("version") {
		if f, ok := v.(float64); ok && i < 3 {
			m.Version[i] = int(f)
		}
	}
	for _, v := range learner.list("feature_names") {
		s, _ := v.(string)
		m.FeatureNames = append(m.FeatureNames, s)
	}
	for _, v := range learner.list("feature_types") {
		s, _ := v.(string)
		m.FeatureTypes = append(m.FeatureTypes, s)
	}
	param := learner.obj("learner_model_param")
	m.NumFeature = param.int("num_feature")
	numClass, numTarget := param.int("num_class"), param.int("num_target")
	m.groups = max(1, numClass, numTarget)
	m.Objective = learner.obj("objective").str("name")
	if m.Objective == "" {
		return nil, errors.New("the model names no objective")
	}
	if _, ok := transforms[m.Objective]; !ok {
		return nil, fmt.Errorf("unsupported objective %s", m.Objective)
	}
	base, err := parseBaseScore(param.str("base_score"))
	if err != nil {
		return nil, err
	}
	m.base = make([]float32, m.groups)
	for g := range m.base {
		b := base[0]
		if len(base) == m.groups {
			b = base[g]
		} else if len(base) != 1 {
			return nil, fmt.Errorf("base_score has %d values for %d output groups", len(base), m.groups)
		}
		m.base[g] = probToMargin(m.Objective, b)
	}
	gb := learner.obj("gradient_booster")
	m.Booster = gb.str("name")
	switch m.Booster {
	case "gbtree":
		err = m.parseTrees(gb.obj("model"), nil)
	case "dart":
		w, e := nums(gb.list("weight_drop"))
		if e != nil {
			return nil, fmt.Errorf("weight_drop: %v", e)
		}
		err = m.parseTrees(gb.obj("gbtree").obj("model"), w)
	case "gblinear":
		w, e := nums(gb.obj("model").list("weights"))
		if e != nil {
			return nil, fmt.Errorf("weights: %v", e)
		}
		if len(w) != (m.NumFeature+1)*m.groups {
			return nil, fmt.Errorf("gblinear has %d weights for %d features and %d groups", len(w), m.NumFeature, m.groups)
		}
		for _, x := range w {
			m.linear = append(m.linear, float32(x))
		}
	default:
		return nil, fmt.Errorf("unsupported booster %q", m.Booster)
	}
	if err != nil {
		return nil, err
	}
	if len(m.FeatureNames) > 0 && len(m.FeatureNames) != m.NumFeature {
		return nil, fmt.Errorf("the model has %d feature_names for %d features", len(m.FeatureNames), m.NumFeature)
	}
	return m, nil
}

// parseBaseScore reads base_score, a number ("5E-1") or, since XGBoost
// 3.0, a vector of one per output group ("[5E-1]").
func parseBaseScore(s string) ([]float64, error) {
	if s == "" {
		return []float64{0.5}, nil
	}
	var out []float64
	for _, p := range strings.Split(strings.Trim(s, "[]"), ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("bad base_score %q", s)
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *Model) parseTrees(model obj, weights []float64) error {
	if model == nil {
		return errors.New("the booster has no model")
	}
	if cats := model.obj("cats"); cats != nil && len(cats.list("enc")) > 0 {
		return errors.New("the model re-codes string categories (it was trained on a DataFrame of category columns); BigQuery ML reads categorical features as integers")
	}
	trees := model.list("trees")
	info, err := nums(model.list("tree_info"))
	if err != nil {
		return fmt.Errorf("tree_info: %v", err)
	}
	if len(info) != len(trees) {
		return fmt.Errorf("the model has %d trees and %d tree_info entries", len(trees), len(info))
	}
	if weights != nil && len(weights) != len(trees) {
		return fmt.Errorf("the DART model has %d trees and %d weight_drop entries", len(trees), len(weights))
	}
	for i, tv := range trees {
		t, width, err := parseTree(asObj(tv), m.NumFeature)
		if err != nil {
			return fmt.Errorf("tree %d: %v", i, err)
		}
		g := int(info[i])
		if width > 1 {
			if width != m.groups {
				return fmt.Errorf("tree %d has leaves of %d values for %d targets", i, width, m.groups)
			}
			m.leafWidth = width
		} else if g < 0 || g >= m.groups {
			return fmt.Errorf("tree %d is of output group %d of %d", i, g, m.groups)
		}
		m.trees = append(m.trees, t)
		m.group = append(m.group, g)
		w := float32(1)
		if weights != nil {
			w = float32(weights[i])
		}
		m.weight = append(m.weight, w)
	}
	return nil
}

func parseTree(o obj, numFeature int) (tree, int, error) {
	if o == nil {
		return tree{}, 0, errors.New("not an object")
	}
	n := o.obj("tree_param").int("num_nodes")
	width := max(1, o.obj("tree_param").int("size_leaf_vector"))
	get := func(k string) ([]float64, error) {
		v, err := nums(o.list(k))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", k, err)
		}
		return v, nil
	}
	var t tree
	fields := map[string]*[]float64{}
	var left, right, feat, cond, def, stype, gain, cover, bw []float64
	for k, p := range map[string]*[]float64{"left_children": &left, "right_children": &right, "split_indices": &feat,
		"split_conditions": &cond, "default_left": &def, "split_type": &stype, "loss_changes": &gain, "sum_hessian": &cover,
		"base_weights": &bw} {
		fields[k] = p
		v, err := get(k)
		if err != nil {
			return t, 0, err
		}
		*p = v
	}
	for _, k := range []string{"left_children", "right_children", "split_indices", "split_conditions", "default_left"} {
		if len(*fields[k]) != n {
			return t, 0, fmt.Errorf("%s has %d values for %d nodes", k, len(*fields[k]), n)
		}
	}
	if n == 0 {
		return t, 0, errors.New("a tree of no nodes")
	}
	t.left, t.right, t.feat = make([]int32, n), make([]int32, n), make([]int32, n)
	t.cond, t.defLeft, t.categorical = make([]float32, n), make([]bool, n), make([]bool, n)
	for i := 0; i < n; i++ {
		t.left[i], t.right[i], t.feat[i] = int32(left[i]), int32(right[i]), int32(feat[i])
		t.cond[i] = float32(cond[i])
		t.defLeft[i] = def[i] != 0
		if i < len(stype) && stype[i] == 1 {
			t.categorical[i] = true
		}
		if t.left[i] != -1 {
			if t.left[i] <= 0 || int(t.left[i]) >= n || t.right[i] <= 0 || int(t.right[i]) >= n {
				return t, 0, fmt.Errorf("node %d has children %d and %d of %d nodes", i, t.left[i], t.right[i], n)
			}
			if t.feat[i] < 0 || int(t.feat[i]) >= numFeature {
				return t, 0, fmt.Errorf("node %d splits on feature %d of %d", i, t.feat[i], numFeature)
			}
		}
	}
	// A node's children come after it, so a walk always ends.
	for i := 0; i < n; i++ {
		if t.left[i] != -1 && (int(t.left[i]) <= i || int(t.right[i]) <= i) {
			return t, 0, fmt.Errorf("node %d's children come before it", i)
		}
	}
	if len(gain) == n {
		t.gain = gain
	}
	if len(cover) == n {
		t.cover = cover
	}
	if width > 1 {
		if len(bw) != n*width {
			return t, 0, fmt.Errorf("base_weights has %d values for %d nodes of %d", len(bw), n, width)
		}
		for _, v := range bw {
			t.leaf = append(t.leaf, float32(v))
		}
	}
	catNodes, err := get("categories_nodes")
	if err != nil {
		return t, 0, err
	}
	if len(catNodes) > 0 {
		cats, err1 := get("categories")
		segs, err2 := get("categories_segments")
		sizes, err3 := get("categories_sizes")
		if err := errors.Join(err1, err2, err3); err != nil {
			return t, 0, err
		}
		if len(segs) != len(catNodes) || len(sizes) != len(catNodes) {
			return t, 0, errors.New("categories_nodes, categories_segments and categories_sizes differ in length")
		}
		t.cats = map[int32]map[int32]bool{}
		for k, nd := range catNodes {
			lo, sz := int(segs[k]), int(sizes[k])
			if lo < 0 || sz < 0 || lo+sz > len(cats) || int(nd) < 0 || int(nd) >= n {
				return t, 0, errors.New("a categorical split's categories are out of range")
			}
			set := map[int32]bool{}
			for _, c := range cats[lo : lo+sz] {
				set[int32(c)] = true
			}
			t.cats[int32(nd)] = set
		}
	}
	return t, width, nil
}

// OutputWidth is the number of values Predict returns for a row: the
// classes of multi:softprob, the targets of a multi-target model, else 1.
func (m *Model) OutputWidth() int {
	if m.Objective == "multi:softmax" {
		return 1
	}
	return m.groups
}

// leafOf walks t for row x and returns the leaf's node.
func (t *tree) leafOf(x []float32) int32 {
	nid := int32(0)
	for t.left[nid] != -1 {
		v := x[t.feat[nid]]
		switch {
		case v != v: // missing
			if t.defLeft[nid] {
				nid = t.left[nid]
			} else {
				nid = t.right[nid]
			}
		case t.categorical[nid]:
			// XGBoost's common::Decision: a category in the set goes
			// right; any other value, an invalid one included, left.
			c := int32(v)
			if v >= 0 && v < 1<<24 && float32(c) == v && t.cats[nid][c] {
				nid = t.right[nid]
			} else {
				nid = t.left[nid]
			}
		case v < t.cond[nid]:
			nid = t.left[nid]
		default:
			nid = t.right[nid]
		}
	}
	return nid
}

// Margin is the model's raw score of row x (NumFeature values, NaN for a
// missing one): one value per output group.
func (m *Model) Margin(x []float64) ([]float32, error) {
	if len(x) != m.NumFeature {
		return nil, fmt.Errorf("a row of %d values for a model of %d features", len(x), m.NumFeature)
	}
	xf := make([]float32, len(x))
	for i, v := range x {
		xf[i] = float32(v)
	}
	out := append([]float32{}, m.base...)
	if m.linear != nil {
		for g := 0; g < m.groups; g++ {
			s := float32(0)
			for f, v := range xf {
				if v == v {
					s += v * m.linear[f*m.groups+g]
				}
			}
			out[g] += s + m.linear[m.NumFeature*m.groups+g]
		}
		return out, nil
	}
	for i := range m.trees {
		t := &m.trees[i]
		nid := t.leafOf(xf)
		if m.leafWidth > 1 {
			for g := 0; g < m.leafWidth; g++ {
				out[g] += t.leaf[int(nid)*m.leafWidth+g] * m.weight[i]
			}
			continue
		}
		out[m.group[i]] += t.cond[nid] * m.weight[i]
	}
	return out, nil
}

// Predict is Booster.predict of row x: OutputWidth values.
func (m *Model) Predict(x []float64) ([]float64, error) {
	mg, err := m.Margin(x)
	if err != nil {
		return nil, err
	}
	return transforms[m.Objective](mg), nil
}

func sigmoid(v float32) float32 { return float32(1 / (1 + math.Exp(-float64(v)))) }

func each(f func(float32) float32) func([]float32) []float64 {
	return func(mg []float32) []float64 {
		out := make([]float64, len(mg))
		for i, v := range mg {
			out[i] = float64(f(v))
		}
		return out
	}
}

func identity(v float32) float32 { return v }

func expf(v float32) float32 { return float32(math.Exp(float64(v))) }

// transforms are the objectives' prediction transforms (PredTransform).
var transforms = map[string]func([]float32) []float64{
	"reg:squarederror":     each(identity),
	"reg:squaredlogerror":  each(identity),
	"reg:pseudohubererror": each(identity),
	"reg:absoluteerror":    each(identity),
	"reg:quantileerror":    each(identity),
	"reg:linear":           each(identity),
	"binary:logitraw":      each(identity),
	"rank:pairwise":        each(identity),
	"rank:ndcg":            each(identity),
	"rank:map":             each(identity),
	"reg:logistic":         each(sigmoid),
	"binary:logistic":      each(sigmoid),
	"count:poisson":        each(expf),
	"reg:gamma":            each(expf),
	"reg:tweedie":          each(expf),
	"survival:cox":         each(expf),
	"survival:aft":         each(expf),
	"binary:hinge": each(func(v float32) float32 {
		if v > 0 {
			return 1
		}
		return 0
	}),
	"multi:softprob": func(mg []float32) []float64 {
		mx := mg[0]
		for _, v := range mg {
			mx = max(mx, v)
		}
		sum := float32(0)
		e := make([]float32, len(mg))
		for i, v := range mg {
			e[i] = expf(v - mx)
			sum += e[i]
		}
		out := make([]float64, len(mg))
		for i := range e {
			out[i] = float64(e[i] / sum)
		}
		return out
	},
	"multi:softmax": func(mg []float32) []float64 {
		best := 0
		for i, v := range mg {
			if v > mg[best] {
				best = i
			}
		}
		return []float64{float64(best)}
	},
}

// probToMargin is the objective's ProbToMargin: base_score, saved in the
// prediction's space, as a margin.
func probToMargin(objective string, b float64) float32 {
	switch objective {
	case "reg:logistic", "binary:logistic", "binary:logitraw":
		return float32(-math.Log(1/b - 1))
	case "count:poisson", "reg:gamma", "reg:tweedie", "survival:cox", "survival:aft":
		return float32(math.Log(b))
	}
	return float32(b)
}

// Importance is XGBoost's get_score of one feature: the splits on it
// (weight), and their mean loss change (gain) and mean hessian sum
// (cover).
type Importance struct {
	Feature     int
	Weight      int64
	Gain, Cover float64
}

// Importances are every feature's Importance, in feature order; nil for
// a gblinear model, whose importance is its weights.
func (m *Model) Importances() []Importance {
	if m.linear != nil {
		return nil
	}
	out := make([]Importance, m.NumFeature)
	for f := range out {
		out[f].Feature = f
	}
	for _, t := range m.trees {
		for nid, l := range t.left {
			if l == -1 {
				continue
			}
			f := t.feat[nid]
			out[f].Weight++
			if t.gain != nil {
				out[f].Gain += t.gain[nid]
			}
			if t.cover != nil {
				out[f].Cover += t.cover[nid]
			}
		}
	}
	for f := range out {
		if out[f].Weight > 0 {
			out[f].Gain /= float64(out[f].Weight)
			out[f].Cover /= float64(out[f].Weight)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Feature < out[j].Feature })
	return out
}
