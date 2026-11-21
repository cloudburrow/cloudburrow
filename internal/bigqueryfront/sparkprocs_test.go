package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

// TestSparkProcedureParse (#2104): a CREATE PROCEDURE of a procedure for
// Apache Spark, which has no BEGIN ... END, is one node of the script,
// read with its connection, options, language and inline code.
func TestSparkProcedureParse(t *testing.T) {
	sql := "CREATE OR REPLACE PROCEDURE `p.ds.sp`(word STRING, OUT n INT64) EXTERNAL SECURITY INVOKER " +
		"WITH CONNECTION `my-project.us.my-connection` OPTIONS(engine=\"SPARK\", runtime_version=\"2.2\", " +
		"py_file_uris=[\"gs://b/a.py\", \"gs://b/b.py\"], properties=[(\"spark.executor.instances\", \"3\"), (\"k\", \"v\")]) " +
		"LANGUAGE PYTHON AS r\"\"\"\nprint('a; b')\n\"\"\"; CALL `p.ds.sp`('x', v)"
	toks, _ := lex(sql)
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" {
		t.Fatal(p.err)
	}
	if len(nodes) != 2 || nodes[0].kind != nodeCreateProc || !nodes[0].spark || nodes[1].kind != nodeCall {
		t.Fatalf("nodes %+v", nodes)
	}
	h, code, msg := readSparkHeader(sql, nodes[0].toks)
	if code != 0 {
		t.Fatal(msg)
	}
	if !h.replace || !h.invoker || strings.Join(h.connection, ",") != "my-project,us,my-connection" || h.language != "PYTHON" ||
		!h.hasCode || h.code != "\nprint('a; b')\n" || len(h.args) != 2 || h.args[1].Mode != "OUT" {
		t.Errorf("header %+v", h)
	}
	if o := h.options; o["engine"].str != "SPARK" || o["runtime_version"].str != "2.2" || len(o["py_file_uris"].list) != 2 ||
		len(o["properties"].pairs) != 2 || o["properties"].pairs[1] != [2]string{"k", "v"} {
		t.Errorf("options %+v", h.options)
	}
	if _, ok := loneSparkProc(sql); ok {
		t.Error("a script of two statements is a lone CREATE PROCEDURE")
	}
	if _, ok := loneSparkProc(sql[:strings.Index(sql, "; CALL")]); !ok {
		t.Error("the CREATE PROCEDURE alone is not one")
	}
	if _, ok := loneSparkProc("CREATE PROCEDURE ds.p() BEGIN SELECT 1; END"); ok {
		t.Error("a SQL procedure is a Spark one")
	}
	for _, bad := range []string{
		"CREATE PROCEDURE ds.p() WITH CONNECTION `a.b` LANGUAGE PYTHON",
		"CREATE PROCEDURE ds.p() WITH CONNECTION `a.us.c` OPTIONS(engine=SPARK) LANGUAGE PYTHON",
		"CREATE PROCEDURE ds.p() WITH CONNECTION `a.us.c` LANGUAGE PYTHON AS 1",
	} {
		toks, _ := lex(bad)
		if _, code, _ := readSparkHeader(bad, toks); code != 400 {
			t.Errorf("%q: %d, want 400", bad, code)
		}
	}
}

// TestSparkRuntimeMinor: a runtime version's patch is left out.
func TestSparkRuntimeMinor(t *testing.T) {
	for in, want := range map[string]string{"2.2": "2.2", "2.2.30": "2.2", "1.2.5": "1.2", "4.0": "4.0", "x": "x"} {
		if got := sparkRuntimeMinor(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// TestONNXImageValues (#2104): an image STRUCT is a [1, h, w, c] tensor
// for an input of rank 4, channels last, or its values for rank 2; one of
// another size is refused naming ML.RESIZE_IMAGE.
func TestONNXImageValues(t *testing.T) {
	img := json.RawMessage(`{"f0_":[1,2,3],"f1_":[0.1,0.2,0.3,0.4,0.5,0.6]}`)
	v, why := onnxImageValues(onnxmodel.Port{Name: "input", Elem: "FLOAT", Dims: []int64{-1, 1, 2, 3}}, img)
	if why != "" || len(v.values) != 6 || string(v.values[5]) != "0.6" || len(v.shape) != 4 || v.shape[0] != 1 || v.shape[3] != 3 {
		t.Errorf("rank 4: %+v %s", v, why)
	}
	v, why = onnxImageValues(onnxmodel.Port{Name: "input", Elem: "FLOAT", Dims: []int64{-1, 6}}, img)
	if why != "" || len(v.shape) != 2 || v.shape[1] != 6 {
		t.Errorf("rank 2: %+v %s", v, why)
	}
	if _, why := onnxImageValues(onnxmodel.Port{Name: "input", Elem: "FLOAT", Dims: []int64{-1, 224, 224, 3}}, img); !strings.Contains(why, "ML.RESIZE_IMAGE") {
		t.Errorf("another size: %q", why)
	}
	if _, why := onnxImageValues(onnxmodel.Port{Name: "input", Elem: "FLOAT", Dims: []int64{-1, 2, 3}}, img); why == "" {
		t.Error("rank 3 taken as an image")
	}
}

// TestParseViewColumnOptions (#2104): ALTER VIEW ... ALTER COLUMN ... SET
// OPTIONS (description = ...) is read; another option is refused, and
// another statement left alone.
func TestParseViewColumnOptions(t *testing.T) {
	v, msg, ok := parseViewColumnOptions("ALTER VIEW IF EXISTS ds.v ALTER COLUMN IF EXISTS total SET OPTIONS (description = 'Total sales');")
	if !ok || msg != "" || !v.ifExists || !v.colIfExist || v.column != "total" || v.description == nil || *v.description != "Total sales" ||
		strings.Join(v.path, ".") != "ds.v" {
		t.Errorf("%+v %q %v", v, msg, ok)
	}
	if v, _, ok := parseViewColumnOptions("ALTER VIEW ds.v ALTER COLUMN c SET OPTIONS (description = NULL)"); !ok || v.description != nil {
		t.Errorf("NULL: %+v", v)
	}
	if _, msg, ok := parseViewColumnOptions("ALTER VIEW ds.v ALTER COLUMN c SET OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')"); !ok || msg == "" {
		t.Error("a table column's option on a view column is not refused")
	}
	for _, other := range []string{"ALTER VIEW ds.v SET OPTIONS (description = 'x')", "ALTER TABLE ds.t ALTER COLUMN c SET OPTIONS (description = 'x')",
		"ALTER VIEW ds.v ALTER COLUMN c SET OPTIONS (description = 'x'); SELECT 1"} {
		if _, _, ok := parseViewColumnOptions(other); ok {
			t.Errorf("%q read as ALTER VIEW ALTER COLUMN", other)
		}
	}
}
