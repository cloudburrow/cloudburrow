package ac3

import "math"

// Tables from ETSI TS 102 366 V1.4.1 (A/52 Annex E is its Annex E); each
// names the table it copies.

// frmSizeTab is Table 4.13: words per syncframe by frmsizecod>>1 and fscod
// (48, 44.1 and 32 kHz); 44.1 kHz adds frmsizecod&1.
var frmSizeTab = [19][3]int{
	{64, 69, 96}, {80, 87, 120}, {96, 104, 144}, {112, 121, 168}, {128, 139, 192},
	{160, 174, 240}, {192, 208, 288}, {224, 243, 336}, {256, 278, 384}, {320, 348, 480},
	{384, 417, 576}, {448, 487, 672}, {512, 557, 768}, {640, 696, 960}, {768, 835, 1152},
	{896, 975, 1344}, {1024, 1114, 1536}, {1152, 1253, 1728}, {1280, 1393, 1920},
}

var sampleRates = [3]int{48000, 44100, 32000}

// Tables 6.6 to 6.11.
var (
	slowdec  = [4]int{0x0f, 0x11, 0x13, 0x15}
	fastdec  = [4]int{0x3f, 0x53, 0x67, 0x7b}
	slowgain = [4]int{0x540, 0x4d8, 0x478, 0x410}
	dbpbtab  = [4]int{0x000, 0x700, 0x900, 0xb00}
	floortab = [8]int{0x2f0, 0x2b0, 0x270, 0x230, 0x1f0, 0x170, 0x0f0, -0x800} // 0xf800 as a 16-bit signed value
	fastgain = [8]int{0x080, 0x100, 0x180, 0x200, 0x280, 0x300, 0x380, 0x400}
)

// bndtab and bndsz are Table 6.12.
var (
	bndtab = [50]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24,
		25, 26, 27, 28, 31, 34, 37, 40, 43, 46, 49, 55, 61, 67, 73, 79, 85, 97, 109, 121, 133, 157, 181, 205, 229}
	bndsz = [50]int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 3, 3, 3, 3, 3, 3, 3, 6, 6, 6, 6, 6, 6, 12, 12, 12, 12, 24, 24, 24, 24, 24}
)

// baptab is Table 6.16.
var baptab = [64]int{0, 1, 1, 1, 1, 1, 2, 2, 3, 3, 3, 4, 4, 5, 5, 6, 6, 6, 6, 7, 7, 7, 7, 8, 8, 8, 8, 9, 9, 9, 9, 10,
	10, 10, 10, 11, 11, 11, 11, 12, 12, 12, 12, 13, 13, 13, 13, 14, 14, 14, 14, 14, 14, 14, 14, 15, 15, 15, 15, 15, 15, 15, 15, 15}

// hebaptab is Table E.2.1.
var hebaptab = [64]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 8, 8, 8, 9, 9, 9, 10, 10, 10, 10, 11, 11, 11, 11, 12, 12, 12, 12, 13, 13, 13, 13, 14,
	14, 14, 14, 15, 15, 15, 15, 16, 16, 16, 16, 17, 17, 17, 17, 18, 18, 18, 18, 18, 18, 18, 18, 19, 19, 19, 19, 19, 19, 19, 19, 19}

// qntztab is the mantissa length by bap (Table 6.18), asymmetric quantizers.
var qntztab = [16]int{0, 0, 0, 3, 0, 4, 5, 6, 7, 8, 9, 10, 11, 12, 14, 16}

// hebapBits is the mantissa length by hebap (Table E.2.2): the VQ index
// length for hebap 1 to 7, the mantissa length m above.
var hebapBits = [20]int{0, 2, 3, 4, 5, 7, 8, 9, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 14, 16}

// gaqRemap is Table E.2.6, by hebap-8 and gain (1, 2, 4): a, b for x >= 0,
// b for x < 0, as 16-bit fractions.
var gaqRemap = [12][3][3]int16{
	{{0x1249, 0x0000, 0x0000}, {-0x2aab, 0x4000, -0x1555}, {-0x1249, 0x2000, -0x0492}},
	{{0x0889, 0x0000, 0x0000}, {-0x36db, 0x4000, -0x2db7}, {-0x199a, 0x2000, -0x1333}},
	{{0x0421, 0x0000, 0x0000}, {-0x3bbc, 0x4000, -0x3777}, {-0x1ce7, 0x2000, -0x19ce}},
	{{0x0208, 0x0000, 0x0000}, {-0x3def, 0x4000, -0x3bdf}, {-0x1e7a, 0x2000, -0x1cf4}},
	{{0x0102, 0x0000, 0x0000}, {-0x3efc, 0x4000, -0x3df8}, {-0x1f3e, 0x2000, -0x1e7d}},
	{{0x0081, 0x0000, 0x0000}, {-0x3f7f, 0x4000, -0x3efe}, {-0x1fa0, 0x2000, -0x1f3f}},
	{{0x0040, 0x0000, 0x0000}, {-0x3fc0, 0x4000, -0x3f7f}, {-0x1fd0, 0x2000, -0x1fa0}},
	{{0x0020, 0x0000, 0x0000}, {-0x3fe0, 0x4000, -0x3fc0}, {-0x1fe8, 0x2000, -0x1fd0}},
	{{0x0010, 0x0000, 0x0000}, {-0x3ff0, 0x4000, -0x3fe0}, {-0x1ff4, 0x2000, -0x1fe8}},
	{{0x0008, 0x0000, 0x0000}},
	{{0x0002, 0x0000, 0x0000}},
	{{0x0000, 0x0000, 0x0000}},
}

// Symmetric quantizer levels for bap 1 to 5 (Tables 6.19 to 6.23).
var symLevels = [6]int{0, 3, 5, 7, 11, 15}

// frmExpStr is Table E.1.9: the six blocks' strategies (0 reuse, 1 D15,
// 2 D25, 3 D45) for each frame exponent strategy code.
var frmExpStr = [32][6]int{
	{1, 0, 0, 0, 0, 0}, {1, 0, 0, 0, 0, 3}, {1, 0, 0, 0, 2, 0}, {1, 0, 0, 0, 3, 3},
	{2, 0, 0, 2, 0, 0}, {2, 0, 0, 2, 0, 3}, {2, 0, 0, 3, 2, 0}, {2, 0, 0, 3, 3, 3},
	{2, 0, 1, 0, 0, 0}, {2, 0, 2, 0, 0, 3}, {2, 0, 2, 0, 2, 0}, {2, 0, 2, 0, 3, 3},
	{2, 0, 3, 2, 0, 0}, {2, 0, 3, 2, 0, 3}, {2, 0, 3, 3, 2, 0}, {2, 0, 3, 3, 3, 3},
	{3, 1, 0, 0, 0, 0}, {3, 1, 0, 0, 0, 3}, {3, 2, 0, 0, 2, 0}, {3, 2, 0, 0, 3, 3},
	{3, 2, 0, 2, 0, 0}, {3, 2, 0, 2, 0, 3}, {3, 2, 0, 3, 2, 0}, {3, 2, 0, 3, 3, 3},
	{3, 3, 1, 0, 0, 0}, {3, 3, 2, 0, 0, 3}, {3, 3, 2, 0, 2, 0}, {3, 3, 2, 0, 3, 3},
	{3, 3, 3, 2, 0, 0}, {3, 3, 3, 2, 0, 3}, {3, 3, 3, 3, 2, 0}, {3, 3, 3, 3, 3, 3},
}

// Default band structures: Tables E.1.10, E.1.12 and E.1.13.
var (
	defSpxBndStrc  = [17]bool{8: true, 10: true, 12: true, 14: true, 16: true}
	defCplBndStrc  = [18]bool{8: true, 10: true, 11: true, 13: true, 14: true, 15: true, 16: true, 17: true}
	defEcplBndStrc = [22]bool{9: true, 11: true, 13: true, 15: true, 16: true, 17: true, 19: true, 20: true, 21: true}
)

// ecplSubbndTab is Table E.2.9.
var ecplSubbndTab = [23]int{13, 19, 25, 31, 37, 49, 61, 73, 85, 97, 109, 121, 133, 145, 157, 169, 181, 193, 205, 217, 229, 241, 253}

// Table E.2.10.
var (
	ecplAmpExpTab  = [32]int{0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 6, 6, 6, 6, 7, 7, 0}
	ecplAmpMantTab = [32]int{0x20, 0x1b, 0x17, 0x13, 0x10, 0x1b, 0x17, 0x13, 0x10, 0x1b, 0x17, 0x13, 0x10, 0x1b, 0x17, 0x13, 0x10,
		0x1b, 0x17, 0x13, 0x10, 0x1b, 0x17, 0x13, 0x10, 0x1b, 0x17, 0x13, 0x10, 0x1b, 0x17, 0x00}
)

// spxBandTable is the low transform coefficient of each spectral
// extension sub-band (Table E.2.11), with the end of the last.
func spxBandTable(sbnd int) int { return 25 + 12*sbnd }

// spxAttenTab is Table E.2.12, whose entries are 2^(-(code+1)(bin+1)/15).
func spxAtten(code, bin int) float64 { return math.Pow(2, -float64((code+1)*(bin+1))/15) }

// Rematrixing band edges (Tables 6.25 to 6.28).
var rematBands = [5]int{13, 25, 37, 61, 253}

// Mix level tables: Table 4.4 (cmixlev), Table 4.5 (surmixlev), and
// Tables D.1.2 to D.1.5 (the 3-bit Lt/Rt and Lo/Ro levels), as the powers
// of two the dB steps stand for.
var (
	cmixLevels   = [4]float64{math.Sqrt2 / 2, 0.5946035575013605, 0.5, 0.5946035575013605}
	surMixLevels = [4]float64{math.Sqrt2 / 2, 0.5, 0, 0.5}
	mixLevels3   = [8]float64{math.Sqrt2, 1.189207115002721, 1, 0.8408964152537145, math.Sqrt2 / 2, 0.5946035575013605, 0.5, 0}
)

// window is the 256-point half of the 512-point Kaiser-Bessel derived
// window, alpha 5, that Table 6.33 lists to five places.
var window = func() [256]float64 {
	const n, alpha = 512, 5.0
	var kaiser [n/2 + 1]float64
	sum := 0.0
	for j := range kaiser {
		r := 2*float64(j)/(n/2) - 1
		kaiser[j] = besselI0(math.Pi * alpha * math.Sqrt(1-r*r))
		sum += kaiser[j]
	}
	var w [256]float64
	acc := 0.0
	for i := range w {
		acc += kaiser[i]
		w[i] = math.Sqrt(acc / sum)
	}
	return w
}()

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 100; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
		if term < 1e-18*sum {
			break
		}
	}
	return sum
}
