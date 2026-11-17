//go:build onnx

package bigqueryfront

import (
	"reflect"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	"github.com/cloudburrow/cloudburrow/internal/onnxmodel/onnxgen"
)

// TestRunONNXLinear (#1711): onnxgen's linear model on ONNX Runtime, from
// the input's keys as TO_JSON_STRING writes them to the outputs' literals.
func TestRunONNXLinear(t *testing.T) {
	m := onnxgen.Linear()
	in, out, err := onnxmodel.Inspect(m)
	if err != nil {
		t.Fatal(err)
	}
	imp := &onnxImport{Inputs: in, Outputs: out}
	ins := make([]onnxColumn, len(in))
	for i, p := range in {
		ins[i], _ = onnxColumnOf(p, "input")
	}
	got, code, msg := runONNX(m, imp, ins, []string{`{"c0":1,"c1":1,"c2":[0,0]}`, `{"c0":0,"c1":0.5,"c2":[2,1]}`})
	if code != 0 {
		t.Fatal(code, msg)
	}
	want := [][]string{{"6.0", "1", "[6.0, (-6.0)]"}, {"3.5", "0", "[3.5, (-3.5)]"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, code, _ := runONNX(m, imp, ins, []string{`{"c0":1,"c1":null,"c2":[0,0]}`}); code != 400 {
		t.Errorf("a NULL input: %d", code)
	}
	if _, code, _ := runONNX(m, imp, ins, []string{`{"c0":1,"c1":1,"c2":[0]}`}); code != 400 {
		t.Errorf("an array of the wrong length: %d", code)
	}
}
