package aac

// Parametric stereo tables — ISO/IEC 14496-3 §8.6.4 and Annex 8.B. See
// PROVENANCE.md (#1754) for where each comes from and how it was checked
// against the ISO/IEC 14496-26 conformance streams.

var (
	// The hybrid filters' prototypes, 13 taps, g[6] = 1/Q: the 20-band
	// configuration's 8-band (QMF band 0) and real 2-band (bands 1, 2)
	// filters, and the 34-band configuration's 12-band (band 0), 8-band
	// (band 1) and 4-band (bands 2 to 4) filters.
	psG8 = [13]float64{0.00746082949812, 0.02270420949825, 0.04546865930473, 0.07266113929591,
		0.09885108575264, 0.11793710567217, 0.125, 0.11793710567217, 0.09885108575264,
		0.07266113929591, 0.04546865930473, 0.02270420949825, 0.00746082949812}
	psG2 = [13]float64{0, 0.01899487526049, 0, -0.07293139167538, 0, 0.30596630545168, 0.5,
		0.30596630545168, 0, -0.07293139167538, 0, 0.01899487526049, 0}
	psG12 = [13]float64{0.04081179924692, 0.03812810994926, 0.05144908135699, 0.06399831151592,
		0.07428313801106, 0.08100347892914, 0.08333333333333, 0.08100347892914, 0.07428313801106,
		0.06399831151592, 0.05144908135699, 0.03812810994926, 0.04081179924692}
	psG8In34 = [13]float64{0.01565675600122, 0.03752716391991, 0.05417891378782, 0.08417044116767,
		0.10307344158036, 0.12222452249753, 0.125, 0.12222452249753, 0.10307344158036,
		0.08417044116767, 0.05417891378782, 0.03752716391991, 0.01565675600122}
	psG4 = [13]float64{-0.05908211155639, -0.04871498374946, 0, 0.07778723915851,
		0.16486303567403, 0.23279856662996, 0.25, 0.23279856662996, 0.16486303567403,
		0.07778723915851, 0, -0.04871498374946, -0.05908211155639}

	// The 34-band configuration's 32 hybrid sub-subbands (QMF band 0's
	// 12, band 1's 8, and 4 each of bands 2 to 4, in filter output
	// order): each one's stereo parameter band and centre frequency, in
	// QMF bands (band q's centre is q + 1/2). Each Q-band filter's output
	// k is centred at (k + 1/2) 2/Q QMF bands from its band's lower edge,
	// wrapped into [-1/2, 3/2); the odd QMF bands' outputs come out a
	// whole QMF band higher.
	psKToI34Hybrid = [32]int{
		0, 1, 2, 3, 4, 5, 6, 6, 7, 2, 1, 0,
		10, 10, 4, 5, 6, 7, 8, 9,
		10, 11, 12, 9,
		14, 11, 12, 13,
		14, 15, 16, 13,
	}
	psFCenter34Hybrid = [32]float64{
		1.0 / 12, 3.0 / 12, 5.0 / 12, 7.0 / 12, 9.0 / 12, 11.0 / 12, 13.0 / 12, 15.0 / 12, 17.0 / 12, -5.0 / 12, -3.0 / 12, -1.0 / 12,
		17.0 / 8, 19.0 / 8, 5.0 / 8, 7.0 / 8, 9.0 / 8, 11.0 / 8, 13.0 / 8, 15.0 / 8,
		9.0 / 4, 11.0 / 4, 13.0 / 4, 7.0 / 4,
		17.0 / 4, 11.0 / 4, 13.0 / 4, 15.0 / 4,
		17.0 / 4, 19.0 / 4, 21.0 / 4, 15.0 / 4,
	}

	// psMap20To34 gives each of the 34 stereo bands its 20-band one;
	// bands 1 and 4 straddle two (0 and 1, 2 and 3), whose indices IID
	// and ICC average.
	psMap20To34 = [34]int{0, 0, 1, 2, 2, 3, 4, 4, 5, 5, 6, 7, 8, 8, 9, 9, 10, 11, 12, 13, 14, 14,
		15, 15, 16, 16, 17, 17, 18, 18, 18, 18, 19, 19}
	// psMap34To20 picks one of the 34 bands for each of the 20, for the
	// last mixing coefficients when a stream changes configuration.
	psMap34To20 = [20]int{0, 2, 3, 5, 6, 8, 10, 11, 12, 14, 16, 17, 18, 19, 20, 22, 24, 26, 28, 32}
)

// psQMFToI34 is the 34-band configuration's stereo band of QMF band q,
// 5 <= q < 64.
func psQMFToI34(q int) int {
	switch {
	case q < 11:
		return q + 11 // 5..10 -> 16..21
	case q < 21:
		return 22 + (q-11)/2 // 11..20 -> 22..26, two each
	case q < 33:
		return 27 + (q-21)/3 // 21..32 -> 27..30, three each
	case q < 41:
		return 31 + (q-33)/4 // 33..40 -> 31, 32, four each
	default:
		return 33
	}
}

// The IPD and OPD Huffman codes (Annex 8.B), symbol i the index (delta,
// modulo 8) i: frequency- and time-differential.
var (
	huffPSIPDDf = psHuffFromCodes("1", "000", "0110", "0100", "0010", "0011", "0101", "0111")
	huffPSIPDDt = psHuffFromCodes("1", "010", "0010", "00011", "00010", "0000", "0011", "011")
	huffPSOPDDf = psHuffFromCodes("1", "001", "0110", "0100", "01111", "01110", "0101", "000")
	huffPSOPDDt = psHuffFromCodes("1", "010", "0001", "00111", "00110", "0000", "0010", "011")
)

// psHuffFromCodes builds a decoding tree (sbrHuffman's form) from each
// symbol's code word, written as bits.
func psHuffFromCodes(codes ...string) sbrHuffman {
	const free = 127
	tree := [][2]int8{{free, free}}
	for sym, code := range codes {
		node := 0
		for j := range len(code) {
			bit := code[j] - '0'
			if j == len(code)-1 {
				if tree[node][bit] != free {
					panic("ps: Huffman code is not prefix-free")
				}
				tree[node][bit] = int8(sym - 64)
				break
			}
			next := tree[node][bit]
			if next == free {
				tree = append(tree, [2]int8{free, free})
				next = int8(len(tree) - 1)
				tree[node][bit] = next
			} else if next <= 0 {
				panic("ps: Huffman code is not prefix-free")
			}
			node = int(next)
		}
	}
	for _, n := range tree {
		if n[0] == free || n[1] == free {
			panic("ps: Huffman code is incomplete")
		}
	}
	return sbrHuffman{tree: tree}
}
