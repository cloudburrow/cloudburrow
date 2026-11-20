package bigqueryfront

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// savedModelPB writes a saved_model.pb of one MetaGraphDef with the given
// GraphDef producer and tensorflow_version.
func savedModelPB(producer int64, tfVersion string) []byte {
	var versions, graph, info, meta, sm []byte
	versions = protowire.AppendTag(versions, 1, protowire.VarintType)
	versions = protowire.AppendVarint(versions, uint64(producer))
	graph = protowire.AppendTag(graph, 1, protowire.BytesType) // a node, skipped
	graph = protowire.AppendBytes(graph, []byte{0x0a, 0x01, 'x'})
	graph = protowire.AppendTag(graph, 4, protowire.BytesType)
	graph = protowire.AppendBytes(graph, versions)
	info = protowire.AppendTag(info, 4, protowire.BytesType)
	info = protowire.AppendString(info, "serve")
	info = protowire.AppendTag(info, 5, protowire.BytesType)
	info = protowire.AppendString(info, tfVersion)
	meta = protowire.AppendTag(meta, 1, protowire.BytesType)
	meta = protowire.AppendBytes(meta, info)
	meta = protowire.AppendTag(meta, 2, protowire.BytesType)
	meta = protowire.AppendBytes(meta, graph)
	sm = protowire.AppendTag(sm, 1, protowire.VarintType)
	sm = protowire.AppendVarint(sm, 1)
	sm = protowire.AppendTag(sm, 2, protowire.BytesType)
	return protowire.AppendBytes(sm, meta)
}

// The create-tensorflow page's version limits (#1775): GraphDef versions
// before 20 and unreleased TensorFlow builds are 400 in its words.
func TestSavedModelVersionLimits(t *testing.T) {
	for _, tc := range []struct {
		files map[string][]byte
		code  int
		text  string
	}{
		{map[string][]byte{"saved_model.pb": savedModelPB(2238, "2.20.0")}, 0, ""},
		{map[string][]byte{"saved_model.pb": savedModelPB(20, "1.15.5")}, 0, ""},
		{map[string][]byte{"saved_model.pb": savedModelPB(19, "2.20.0")}, 400, "earlier than version 20 aren't supported"},
		{map[string][]byte{"saved_model.pb": savedModelPB(2238, "2.21.0-dev20251101")}, 400, "unreleased version of TensorFlow"},
		{map[string][]byte{"saved_model.pbtxt": []byte("meta_graphs {\n meta_info_def { tensorflow_version: \"2.20.0\" }\n graph_def { versions { producer: 12 } }\n}")}, 400, "GraphDef version 12"},
		{map[string][]byte{"saved_model.pbtxt": []byte("meta_graphs { graph_def { versions { producer: 1500 min_consumer: 12 } } }")}, 0, ""},
		{map[string][]byte{"saved_model.pb": []byte("not a protobuf \xff\xff\xff")}, 0, ""}, // the converter answers it
	} {
		code, msg := checkSavedModelVersions("gs://b/m/", tc.files)
		if code != tc.code || !strings.Contains(msg, tc.text) {
			t.Errorf("%v: %d %q", tc.files, code, msg)
		}
	}
	if vs := savedModelVersions(savedModelPB(1234, "2.20.0")); len(vs) != 1 || vs[0].producer != 1234 || vs[0].tfVersion != "2.20.0" {
		t.Errorf("%+v", vs)
	}
}
