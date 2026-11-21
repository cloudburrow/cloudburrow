package tflitemodel

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"testing"
)

// TestRngBitGeneratorMatchesLiteRT (#1775): rng.tflite's four
// STABLEHLO_RNG_BIT_GENERATORs (DEFAULT, PHILOX of a u64[3] state and
// THREEFRY, to UINT32, INT64, INT32 and UINT64 outputs;
// testdata/gen_rng.py) answer exactly LiteRT's bits and next states
// (litert_rng.json), across a 32-bit counter carry.
func TestRngBitGeneratorMatchesLiteRT(t *testing.T) {
	data, err := os.ReadFile("testdata/rng.tflite")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/litert_rng.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Inputs  map[string][]string `json:"inputs"`
		Outputs map[string][]string `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	m, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(Ops(), "STABLEHLO_RNG_BIT_GENERATOR") || len(cases) != 4 {
		t.Fatalf("ops %v, %d cases", m.Uses(), len(cases))
	}
	word := func(s string) int64 {
		u, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			i, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return i
		}
		return int64(u)
	}
	for ci, c := range cases {
		var ins []Tensor
		for _, p := range m.Inputs {
			in := Tensor{Type: p.Type, Shape: []int{len(c.Inputs[p.Name])}}
			for _, s := range c.Inputs[p.Name] {
				in.I = append(in.I, word(s))
			}
			ins = append(ins, in)
		}
		out, err := m.Run(ins)
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range m.Outputs {
			var want []int64
			for _, s := range c.Outputs[p.Name] {
				want = append(want, word(s))
			}
			if !slices.Equal(out[i].I, want) {
				t.Errorf("case %d %s: %v, LiteRT %v", ci, p.Name, out[i].I, want)
			}
		}
	}
}
