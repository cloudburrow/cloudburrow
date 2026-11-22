package tflitemodel

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

// TestLSHProjectionMatchesLiteRT (#1775): lsh.tflite's four
// LSH_PROJECTIONs (SPARSE and DENSE, weighted and not, over INT32,
// FLOAT32, INT64 and UINT8 rows; testdata/gen_lsh.py) answer exactly what
// LiteRT's reference kernel does (litert_lsh.json).
func TestLSHProjectionMatchesLiteRT(t *testing.T) {
	data, err := os.ReadFile("testdata/lsh.tflite")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/litert_lsh.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Inputs  map[string]any     `json:"inputs"`
		Outputs map[string][]int64 `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	m, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Uses(), "LSH_PROJECTION") || !slices.Contains(Ops(), "LSH_PROJECTION") {
		t.Fatalf("uses %v", m.Uses())
	}
	if len(cases) != 4 {
		t.Fatalf("%d cases", len(cases))
	}
	for ci, c := range cases {
		var ins []Tensor
		for _, p := range m.Inputs {
			shape, flat := nums(c.Inputs[p.Name])
			in := Tensor{Type: p.Type, Shape: shape}
			for _, v := range flat {
				if p.Type == Float32 {
					in.F = append(in.F, float32(v))
				} else {
					in.I = append(in.I, int64(v))
				}
			}
			ins = append(ins, in)
		}
		out, err := m.Run(ins)
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range m.Outputs {
			if want := c.Outputs[p.Name]; !reflect.DeepEqual(out[i].I, want) {
				t.Errorf("case %d %s: %v, LiteRT %v", ci, p.Name, out[i].I, want)
			}
		}
	}
}

// nums flattens a nested JSON array of numbers, with its shape.
func nums(v any) ([]int, []float64) {
	a, ok := v.([]any)
	if !ok {
		return nil, []float64{v.(float64)}
	}
	var shape []int
	var flat []float64
	for _, e := range a {
		s, f := nums(e)
		shape = s
		flat = append(flat, f...)
	}
	return append([]int{len(a)}, shape...), flat
}

// TestFingerprint64 pins farmhash::Fingerprint64 on FarmHash's boundary
// lengths (0, 1-3, 4-7, 8-16, 17-32, 33-64 and over 64 bytes), the values
// of FarmHash's reference implementation for bytes i*31+7.
func TestFingerprint64(t *testing.T) {
	want := map[int]uint64{0: 0x9ae16a3b2f90404f, 1: 0x57821efdee1b7472, 3: 0x5f043801775be181, 4: 0xdcf15d9b38b260fc,
		7: 0x545ac134b57925ff, 8: 0xd14693440d28f69a, 16: 0xd9b28ec31be83978, 17: 0xffb7cd799a150d69, 32: 0x5b0213e62a3ca399,
		33: 0xd73bbb576c6c9808, 64: 0x56317ec658bfa4f9, 65: 0x5a5b9a92eb3061d6, 128: 0x62d2c1879bd73de5, 200: 0x8f2dac6e1bb7686d}
	for n, w := range want {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
		if got := fingerprint64(b); got != w {
			t.Errorf("%d bytes: %#x, want %#x", n, got, w)
		}
	}
}
