package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestDecimalParamsAndFits (#957): a parameterized NUMERIC's or
// BIGNUMERIC's precision and scale, read from tables.get's strings (or
// numbers), written back as strings, and the values that fit them.
func TestDecimalParamsAndFits(t *testing.T) {
	var fs []field
	if err := json.Unmarshal([]byte(`[{"name":"a","type":"NUMERIC","precision":"5","scale":"2"},
		{"name":"b","type":"BIGNUMERIC","precision":40},{"name":"c","type":"NUMERIC"},{"name":"d","type":"BIGNUMERIC"},
		{"name":"e","type":"STRING"}]`), &fs); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		p, s  int
		param bool
	}{{5, 2, true}, {40, 0, true}, {38, 9, false}, {76, 38, false}, {0, 0, false}} {
		if p, s, param := decimalParams(fs[i]); p != want.p || s != want.s || param != want.param {
			t.Errorf("%s: decimalParams = %d, %d, %v, want %v", fs[i].Name, p, s, param, want)
		}
	}
	b, _ := json.Marshal(fs[:2])
	if got := string(b); got != `[{"name":"a","type":"NUMERIC","mode":"","fields":null,"precision":"5","scale":"2"},`+
		`{"name":"b","type":"BIGNUMERIC","mode":"","fields":null,"precision":"40"}]` {
		t.Errorf("marshalled %s", got)
	}
	for _, c := range []struct {
		v    string
		p, s int
		ok   bool
	}{
		{"-123.45", 5, 2, true}, {"999.99", 5, 2, true}, {"1.234", 5, 2, false}, {"1000", 5, 2, false},
		{"42", 10, 0, true}, {"0.5", 10, 0, false}, {"12345678901234567890123456789012345.12345", 40, 5, true},
	} {
		if err := decimalFits(c.v, c.p, c.s); (err == nil) != c.ok {
			t.Errorf("decimalFits(%s, %d, %d) = %v, want ok %v", c.v, c.p, c.s, err, c.ok)
		}
	}
}
