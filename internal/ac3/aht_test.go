package ac3

import (
	"math"
	"slices"
	"testing"
)

// bitsOf is a bit reader over the n-bit codes given, MSB first.
func bitsOf(codes []int, n []int) *bitReader {
	var b []byte
	pos := 0
	for i, c := range codes {
		for k := n[i] - 1; k >= 0; k-- {
			if pos/8 >= len(b) {
				b = append(b, 0)
			}
			if c>>k&1 != 0 {
				b[pos/8] |= 1 << (7 - pos%8)
			}
			pos++
		}
	}
	return &bitReader{b: b}
}

// TestGAQQuantizers: the AHT's scalar quantizers (Tables E.2.5 and E.2.6),
// read through gaqMantissa, are symmetric with the levels and steps Table
// E.2.5 gives: for gain 1, 2^m - 1 points 2/(2^m - 1) apart; for gains 2
// and 4, small mantissas 1/2^(m-1) apart and below 1/2 (gain 2) or 1/4
// (gain 4), and large mantissas, after the tag, symmetric beyond them. No
// stream ffmpeg writes uses the AHT, so these check its arithmetic.
func TestGAQQuantizers(t *testing.T) {
	for hebap := 8; hebap <= 19; hebap++ {
		m := hebapBits[hebap]
		var vals []float64
		for c := -(1 << (m - 1)) + 1; c < 1<<(m-1); c++ {
			vals = append(vals, gaqMantissa(bitsOf([]int{c & (1<<m - 1)}, []int{m}), hebap, m, 1))
		}
		step := 2 / float64(int(1)<<m-1)
		for i, v := range vals {
			if want := float64(i-(len(vals)-1)/2) * step; math.Abs(v-want) > 1e-4 && hebap < 17 {
				t.Fatalf("hebap %d, gain 1: level %d is %g, want %g", hebap, i, v, want)
			}
			if math.Abs(v+vals[len(vals)-1-i]) > 2e-4 {
				t.Fatalf("hebap %d, gain 1: not symmetric: %g, %g", hebap, v, vals[len(vals)-1-i])
			}
		}
		if hebap > 16 {
			continue
		}
		for _, gain := range []int{2, 4} {
			small, large := m-1, m-1
			if gain == 4 {
				small, large = m-2, m
			}
			tag := 1 << (small - 1)
			var big []float64
			for c := -(1 << (large - 1)); c < 1<<(large-1); c++ {
				big = append(big, gaqMantissa(bitsOf([]int{tag, c & (1<<large - 1)}, []int{small, large}), hebap, m, gain))
			}
			slices.Sort(big)
			for i, v := range big {
				if math.Abs(v+big[len(big)-1-i]) > 2e-4 {
					t.Fatalf("hebap %d, gain %d: large levels not symmetric: %v", hebap, gain, big)
				}
				if math.Abs(v) < 1/float64(gain)-1e-4 || math.Abs(v) > 1 {
					t.Fatalf("hebap %d, gain %d: a large level %g is not beyond the small range", hebap, gain, v)
				}
			}
			if small > 1 {
				v := gaqMantissa(bitsOf([]int{1}, []int{small}), hebap, m, gain)
				if want := 1 / float64(int(1)<<(m-1)); math.Abs(v-want) > 1e-12 {
					t.Fatalf("hebap %d, gain %d: the smallest step is %g, want %g", hebap, gain, v, want)
				}
			}
		}
	}
}

// TestAHTInverseDCT: E.2.4.5's inverse DCT, with its sqrt(2) and R_0,
// takes a single AHT coefficient X(k,0) = x to C(k,m) = x in every block,
// and is orthogonal: every basis vector has the same energy, 6.
func TestAHTInverseDCT(t *testing.T) {
	f := &frame{}
	f.ahtMant[0][0][5] = 0.25
	for blk := range 6 {
		if c := f.ahtCoef(0, 5, blk); math.Abs(c-0.25) > 1e-12 {
			t.Fatalf("block %d: %g", blk, c)
		}
	}
	for j := 1; j < 6; j++ {
		g := &frame{}
		g.ahtMant[0][j][0] = 1
		e := 0.0
		for blk := range 6 {
			e += g.ahtCoef(0, 0, blk) * g.ahtCoef(0, 0, blk)
		}
		if math.Abs(e-6) > 1e-9 {
			t.Fatalf("basis %d has energy %g", j, e)
		}
	}
}

// TestVQTables: Tables E.3.1 to E.3.7 have 2^bits vectors for each hebap.
func TestVQTables(t *testing.T) {
	for hebap := 1; hebap <= 7; hebap++ {
		if len(vqTables[hebap]) != 1<<hebapBits[hebap] {
			t.Errorf("hebap %d: %d vectors", hebap, len(vqTables[hebap]))
		}
	}
}
