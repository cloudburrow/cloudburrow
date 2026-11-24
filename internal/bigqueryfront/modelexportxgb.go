package bigqueryfront

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The XGBoost Booster export of a boosted tree or random forest model
// (EXPORT MODEL, #2210). "Export models"
// (https://docs.cloud.google.com/bigquery/docs/exporting-models, read
// 2026-11-24) gives BOOSTED_TREE_CLASSIFIER, BOOSTED_TREE_REGRESSOR and
// the random forests the format "XGBoost Booster" of "XGBoost 0.82", and
// its files: model.bst, assets/ (0.txt, 1.txt, ... and
// model_metadata.json, which maps the generic feature names f0, f1, ...
// to the real ones), main.py "for local runs" and
// xgboost_predictor-0.1.tar.gz, which holds predictor.py.
//
// model.bst is XGBoost 0.82's binary Booster format, the only one 0.82
// writes (src/learner.cc's LearnerImpl::Save, src/gbm/gbtree_model.h,
// src/tree/tree_model.cc of the v0.82 tag; XGBoost 1.x still loads it):
// LearnerModelParam (base_score, the starting margin, num_feature, num_class,
// contain_extra_attrs, contain_eval_metrics and 29 reserved ints), the
// objective's and booster's names (each a uint64 length and its bytes),
// GBTreeModelParam (num_trees, num_roots, num_feature, a pad, a deprecated
// int64, num_output_group, size_leaf_vector, 32 reserved ints), each tree
// (TreeParam's 37 ints, its nodes, 20 bytes each, then each node's stats,
// 16 bytes) and tree_info, the output group of each tree, little-endian.
// The trees are the model's own (modeltree.go): node f's split goes left
// when the encoded column is below the threshold, as XGBoost's does; a
// leaf's value is its weight. The encoded columns, f0, f1, ..., are the
// model's preprocessing of its features (encodeFeatureMode), which
// predictor.py carries out from model_metadata.json.
//
// UNVERIFIED (the page above, re-read 2026-11-24, does not say): what
// 0.txt, 1.txt and model_metadata.json hold, and the code of main.py and
// predictor.py. CloudBurrow writes each categorical feature's vocabulary,
// one category a line, as assets/<n>.txt for the feature's index n, the
// encoding in model_metadata.json, and its own main.py and predictor.py,
// which encode a JSON instance as the model does and answer the
// predictions ML.PREDICT names.

// xgbObjective is the XGBoost objective of a tree model t of type typ.
func xgbObjective(typ string, t mlTrained) string {
	switch {
	case len(t.Classes) > 2:
		return "multi:softprob"
	case len(t.Classes) == 2:
		return "binary:logistic"
	}
	return "reg:linear" // XGBoost 0.82's name of squared error
}

// xgbBooster is model.bst, XGBoost 0.82's binary Booster of t's trees.
func xgbBooster(typ string, t mlTrained) []byte {
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) {
		le(uint64(len(s)))
		b.WriteString(s)
	}
	numFeature := 0
	for _, ft := range t.Features {
		numFeature += ft.dims()
	}
	groups := 1
	if len(t.Classes) > 2 {
		groups = len(t.Classes)
	}
	obj := xgbObjective(typ, t)
	// A saved Booster's base_score is the starting margin itself: 0.82
	// applied ProbToMargin when training began, before saving, and loads
	// it as it is (measured: XGBoost 1.7.6 predicts a binary:logistic
	// model of base_score 0.5 from a margin of 0.5).
	base := float32(t.BaseScore)
	// LearnerModelParam.
	le(base)
	le(uint32(numFeature))
	numClass := int32(0)
	if groups > 1 {
		numClass = int32(groups)
	}
	le(numClass)
	le(int32(0)) // contain_extra_attrs
	le(int32(0)) // contain_eval_metrics
	le(make([]int32, 29))
	str(obj)
	str("gbtree")
	// GBTreeModelParam.
	le(int32(len(t.Trees)))
	le(int32(1)) // num_roots
	le(int32(numFeature))
	le(int32(0)) // pad_32bit
	le(int64(0)) // num_pbuffer_deprecated
	le(int32(groups))
	le(int32(0)) // size_leaf_vector
	le(make([]int32, 32))
	for _, tr := range t.Trees {
		xgbTree(&b, tr, numFeature)
	}
	info := make([]int32, len(t.Trees))
	for j := range info {
		if j < len(t.TreeClass) {
			info[j] = int32(t.TreeClass[j])
		}
	}
	le(info)
	return b.Bytes()
}

// xgbOrder renumbers tr's nodes as XGBoost allocates them: the root 0,
// and each split's children the next two numbers in breadth-first order,
// its right child after its left. XGBoost's predictors rely on it: they
// go to LeftChild() + !(value < split_cond), never reading cright.
func xgbOrder(tr []mlNode) []mlNode {
	if len(tr) == 0 {
		return tr
	}
	order := []int{0}
	for i := 0; i < len(order); i++ {
		if nd := tr[order[i]]; nd.Feat >= 0 {
			order = append(order, nd.Left, nd.Right)
		}
	}
	id := make(map[int]int, len(order))
	for n, old := range order {
		id[old] = n
	}
	out := make([]mlNode, len(order))
	for n, old := range order {
		nd := tr[old]
		if nd.Feat >= 0 {
			nd.Left, nd.Right = id[nd.Left], id[nd.Right]
		}
		out[n] = nd
	}
	return out
}

// xgbTree writes one RegTree: its TreeParam, nodes and node stats.
func xgbTree(b *bytes.Buffer, tr []mlNode, numFeature int) {
	tr = xgbOrder(tr)
	le := func(v any) { _ = binary.Write(b, binary.LittleEndian, v) }
	parent := make([]int32, len(tr))
	left := make([]bool, len(tr))
	parent[0] = -1
	depth := make([]int, len(tr))
	maxDepth := 0
	for j, nd := range tr {
		if nd.Feat < 0 {
			continue
		}
		parent[nd.Left], parent[nd.Right] = int32(j), int32(j)
		left[nd.Left] = true
		depth[nd.Left], depth[nd.Right] = depth[j]+1, depth[j]+1
		maxDepth = max(maxDepth, depth[j]+1)
	}
	// TreeParam.
	le(int32(1)) // num_roots
	le(int32(len(tr)))
	le(int32(0)) // num_deleted
	le(int32(maxDepth))
	le(int32(numFeature))
	le(int32(0)) // size_leaf_vector
	le(make([]int32, 31))
	for j, nd := range tr {
		p := uint32(parent[j])
		if j > 0 && left[j] {
			p |= 1 << 31
		}
		le(p)
		if nd.Feat < 0 {
			le(int32(-1))
			le(int32(-1))
			le(uint32(0))
			le(float32(nd.Value))
			continue
		}
		le(int32(nd.Left))
		le(int32(nd.Right))
		// The split's feature; a missing value (none: the encoded columns
		// are never NaN) goes left.
		le(uint32(nd.Feat) | 1<<31)
		le(float32(nd.Thr))
	}
	for _, nd := range tr {
		// RTreeNodeStat: loss_chg, sum_hess, base_weight, leaf_child_cnt.
		le(float32(nd.Gain))
		le(float32(nd.Cover))
		le(float32(nd.Value))
		le(int32(0))
	}
}

// exportEncoders are the model's features' encodings, as the export's
// preprocessing applies them (encodeFeatureMode): the JSON both the
// TensorFlow exporter and predictor.py read.
func exportEncoders(t mlTrained) []map[string]any {
	var out []map[string]any
	col := 0
	for _, ft := range t.Features {
		e := map[string]any{"name": ft.Name, "type": ft.Type}
		switch {
		case ft.numeric():
			sub, div := ft.Mean, ft.Stddev
			switch t.Scaling {
			case "raw":
				sub, div = 0, 1
			case "center":
				div = 1
			}
			e["kind"], e["sub"], e["div"], e["fill"] = "numeric", sub, div, ft.Mean
		case ft.LabelEncoded:
			e["kind"], e["vocab"] = "label", nonNil(ft.Categories)
		case ft.TargetEncoded:
			e["kind"], e["vocab"], e["targets"], e["prior"] = "target", nonNil(ft.Categories), ft.Targets, ft.TargetPrior
		default:
			e["kind"], e["vocab"], e["dropped"], e["null"] = "onehot", nonNil(ft.Categories), ft.Dropped-1, ft.NullSeen
		}
		var cols []string
		for j := 0; j < ft.dims(); j++ {
			cols = append(cols, fmt.Sprintf("f%d", col))
			col++
		}
		e["columns"] = cols
		out = append(out, e)
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// xgbFiles are the XGBoost Booster export's files of model md.
func xgbFiles(md bqModel) []exportFile {
	t := md.Trained
	enc := exportEncoders(t)
	names := map[string]string{}
	var files []exportFile
	for i, e := range enc {
		for _, c := range e["columns"].([]string) {
			names[c] = t.Features[i].Name
		}
		if v, ok := e["vocab"].([]string); ok {
			var b strings.Builder
			for _, s := range v {
				b.WriteString(strings.ReplaceAll(s, "\n", " ") + "\n")
			}
			files = append(files, exportFile{name: fmt.Sprintf("assets/%d.txt", i), contentType: "text/plain", data: []byte(b.String())})
		}
	}
	meta := map[string]any{
		"model_type":    md.Type,
		"objective":     xgbObjective(md.Type, t),
		"label":         t.Label,
		"label_type":    t.LabelType,
		"classes":       nonNil(t.Classes),
		"features":      enc,
		"feature_names": names,
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	files = append(files,
		exportFile{name: "assets/model_metadata.json", contentType: "application/json", data: mb},
		exportFile{name: "model.bst", contentType: "application/octet-stream", data: xgbBooster(md.Type, t)},
		exportFile{name: "main.py", contentType: "text/x-python", data: []byte(xgbMainPy)},
		exportFile{name: "xgboost_predictor-0.1.tar.gz", contentType: "application/gzip", data: xgbPredictorSdist()},
	)
	return files
}

// xgbPredictorSdist is xgboost_predictor-0.1.tar.gz: a source
// distribution of predictor.py, as a custom prediction routine's package
// is, with a fixed time so the same model exports the same bytes.
func xgbPredictorSdist() []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	files := []struct{ name, body string }{
		{"xgboost_predictor-0.1/PKG-INFO", "Metadata-Version: 1.0\nName: xgboost_predictor\nVersion: 0.1\nSummary: The predictor of a BigQuery ML model exported as an XGBoost Booster\n"},
		{"xgboost_predictor-0.1/setup.py", "from setuptools import setup\n\nsetup(name=\"xgboost_predictor\", version=\"0.1\", py_modules=[\"predictor\"])\n"},
		{"xgboost_predictor-0.1/predictor.py", xgbPredictorPy},
	}
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// xgbPredictorPy is predictor.py: a custom prediction routine's Predictor
// (predict(instances) and from_path(model_dir)) that encodes each JSON
// instance as the model's preprocessing does (model_metadata.json) and
// runs model.bst.
const xgbPredictorPy = `"""The predictor of a BigQuery ML model exported as an XGBoost Booster."""

import json
import os

import numpy as np
import xgboost as xgb


class Predictor(object):
    def __init__(self, booster, metadata):
        self._booster = booster
        self._meta = metadata

    def _encode(self, instance):
        row = []
        for ft in self._meta["features"]:
            v = instance.get(ft["name"])
            kind = ft["kind"]
            if kind == "numeric":
                x = ft["fill"] if v is None else float(v)
                row.append(0.0 if not ft["div"] else (x - ft["sub"]) / ft["div"])
                continue
            s = None if v is None else (str(v).lower() if isinstance(v, bool) else str(v))
            idx = ft["vocab"].index(s) if s in ft["vocab"] else -1
            if kind == "label":
                row.append(float(idx + 1))
            elif kind == "target":
                row.append(ft["targets"][idx] if idx >= 0 else ft["prior"])
            else:
                cols = [0.0] * len(ft["vocab"])
                if idx >= 0 and idx != ft.get("dropped", -1):
                    cols[idx] = 1.0
                if ft.get("null"):
                    cols.append(1.0 if v is None else 0.0)
                row.extend(cols)
        return row

    def predict(self, instances, **kwargs):
        rows = np.array([self._encode(i) for i in instances], dtype=np.float32)
        out = self._booster.predict(xgb.DMatrix(rows))
        label = self._meta["label"]
        classes = self._meta["classes"]
        answers = []
        for p in out:
            if not classes:
                answers.append({"predicted_" + label: float(p)})
                continue
            probs = [float(p), 1.0 - float(p)] if len(classes) == 2 else [float(x) for x in p]
            values = [classes[1], classes[0]] if len(classes) == 2 else list(classes)
            if len(classes) == 2:
                best = 0 if probs[0] >= 0.5 else 1
            else:
                best = int(np.argmax(probs))
            answers.append({"predicted_" + label: values[best], label + "_probs": probs, label + "_values": values})
        return answers

    @classmethod
    def from_path(cls, model_dir):
        booster = xgb.Booster(model_file=os.path.join(model_dir, "model.bst"))
        with open(os.path.join(model_dir, "assets", "model_metadata.json")) as f:
            metadata = json.load(f)
        return cls(booster, metadata)
`

// xgbMainPy is main.py, which runs the predictor on the instances given
// as its argument: python main.py '[{"feature": value, ...}]'.
const xgbMainPy = `"""Runs the exported model on instances: python main.py '[{"f": 1.0}]'."""

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# predictor.py, as xgboost_predictor-0.1.tar.gz holds it, extracted here.
sys.path[:0] = [HERE, os.path.join(HERE, "xgboost_predictor-0.1")]
from predictor import Predictor  # noqa: E402 - extracted from xgboost_predictor-0.1.tar.gz


def main():
    instances = json.loads(sys.argv[1])
    predictor = Predictor.from_path(HERE)
    print(json.dumps(predictor.predict(instances)))


if __name__ == "__main__":
    main()
`
