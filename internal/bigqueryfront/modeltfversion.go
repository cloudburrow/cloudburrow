package bigqueryfront

import (
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// The create-tensorflow page's version limits (#1775, read 2026-11-20:
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tensorflow):
// "Models trained using a version of GraphDef earlier than version 20
// aren't supported" and "Models trained using an unreleased version of
// TensorFlow aren't supported". They are read from the SavedModel's
// saved_model.pb (or .pbtxt) before anything converts it: each
// MetaGraphDef's graph_def.versions.producer, the GraphDef version
// (tensorflow/core/framework/versions.proto), and its
// meta_info_def.tensorflow_version, the TensorFlow release that wrote it
// (tensorflow/core/protobuf/meta_graph.proto).

// savedModelVersion is one MetaGraphDef's GraphDef producer version and
// TensorFlow version; producer is -1 when the graph names none.
type savedModelVersion struct {
	producer  int64
	tfVersion string
}

// savedModelVersions reads saved_model.pb's MetaGraphDefs' versions: a
// SavedModel is {saved_model_schema_version = 1; repeated MetaGraphDef
// meta_graphs = 2}, a MetaGraphDef {MetaInfoDef meta_info_def = 1;
// GraphDef graph_def = 2; ...}, a MetaInfoDef's tensorflow_version is
// field 5, a GraphDef's VersionDef versions field 4 and its producer
// field 1. It answers nil for bytes that are not such a message.
func savedModelVersions(pb []byte) []savedModelVersion {
	var out []savedModelVersion
	ok := eachField(pb, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) bool {
		if num != 2 || typ != protowire.BytesType {
			return true
		}
		mv := savedModelVersion{producer: -1}
		ok := eachField(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) bool {
			switch {
			case num == 1 && typ == protowire.BytesType: // meta_info_def
				return eachField(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) bool {
					if num == 5 && typ == protowire.BytesType {
						mv.tfVersion = string(v)
					}
					return true
				})
			case num == 2 && typ == protowire.BytesType: // graph_def
				return eachField(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) bool {
					if num != 4 || typ != protowire.BytesType {
						return true
					}
					return eachField(v, func(num protowire.Number, typ protowire.Type, _ []byte, n uint64) bool {
						if num == 1 && typ == protowire.VarintType {
							mv.producer = int64(int32(n))
						}
						return true
					})
				})
			}
			return true
		})
		if ok {
			out = append(out, mv)
		}
		return ok
	})
	if !ok {
		return nil
	}
	return out
}

// eachField calls fn on each field of the message b (v a length-delimited
// field's bytes, n a varint's value) until fn answers false; it answers
// false for bytes that do not parse.
func eachField(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, n uint64) bool) bool {
	for len(b) > 0 {
		num, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return false
		}
		b = b[l:]
		var v []byte
		var n uint64
		switch typ {
		case protowire.BytesType:
			v, l = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			n, l = protowire.ConsumeVarint(b)
		default:
			l = protowire.ConsumeFieldValue(num, typ, b)
		}
		if l < 0 {
			return false
		}
		b = b[l:]
		if !fn(num, typ, v, n) {
			return false
		}
	}
	return true
}

var (
	pbtxtProducer  = regexp.MustCompile(`(?s)versions\s*\{[^}]*?producer\s*:\s*(-?\d+)`)
	pbtxtTFVersion = regexp.MustCompile(`tensorflow_version\s*:\s*"([^"]*)"`)
)

// savedModelTextVersions reads the same from a saved_model.pbtxt.
func savedModelTextVersions(txt []byte) []savedModelVersion {
	mv := savedModelVersion{producer: -1}
	if m := pbtxtProducer.FindSubmatch(txt); m != nil {
		mv.producer, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	if m := pbtxtTFVersion.FindSubmatch(txt); m != nil {
		mv.tfVersion = string(m[1])
	}
	return []savedModelVersion{mv}
}

// unreleasedTensorFlow reports whether a tensorflow_version is a build no
// release carries: a nightly (tf-nightly writes "2.21.0-dev20251120") or
// a source build's development version.
func unreleasedTensorFlow(v string) bool {
	v = strings.ToLower(v)
	return strings.Contains(v, "dev") || strings.Contains(v, "nightly")
}

// checkSavedModelVersions holds a SavedModel to the page's version limits:
// 400 with the page's words, or 0.
func checkSavedModelVersions(uri string, files map[string][]byte) (int, string) {
	var vs []savedModelVersion
	if pb, ok := files["saved_model.pb"]; ok {
		vs = savedModelVersions(pb)
	} else if txt, ok := files["saved_model.pbtxt"]; ok {
		vs = savedModelTextVersions(txt)
	}
	for _, v := range vs {
		if v.producer >= 0 && v.producer < 20 {
			// The page's own sentence, as BigQuery's error (#1775).
			return 400, "Models trained using a version of GraphDef earlier than version 20 aren't supported. The TensorFlow model " + uri +
				" is GraphDef version " + strconv.FormatInt(v.producer, 10) + "."
		}
		if unreleasedTensorFlow(v.tfVersion) {
			return 400, "Models trained using an unreleased version of TensorFlow aren't supported. The TensorFlow model " + uri +
				" was written by TensorFlow " + v.tfVersion + "."
		}
	}
	return 0, ""
}
