package transform

var ZigZagScan8x8 = [64]uint8{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

var DefaultScalingList4x4Intra = [16]uint8{
	6, 13, 20, 28,
	13, 20, 28, 32,
	20, 28, 32, 37,
	28, 32, 37, 42,
}

var DefaultScalingList4x4Inter = [16]uint8{
	10, 14, 20, 24,
	14, 20, 24, 27,
	20, 24, 27, 30,
	24, 27, 30, 34,
}

var DefaultScalingList8x8Intra = [64]uint8{
	6, 10, 13, 16, 18, 23, 25, 27,
	10, 11, 16, 18, 23, 25, 27, 29,
	13, 16, 18, 23, 25, 27, 29, 31,
	16, 18, 23, 25, 27, 29, 31, 33,
	18, 23, 25, 27, 29, 31, 33, 36,
	23, 25, 27, 29, 31, 33, 36, 38,
	25, 27, 29, 31, 33, 36, 38, 40,
	27, 29, 31, 33, 36, 38, 40, 42,
}

var DefaultScalingList8x8Inter = [64]uint8{
	9, 13, 15, 17, 19, 21, 22, 24,
	13, 13, 17, 19, 21, 22, 24, 25,
	15, 17, 19, 21, 22, 24, 25, 27,
	17, 19, 21, 22, 24, 25, 27, 28,
	19, 21, 22, 24, 25, 27, 28, 30,
	21, 22, 24, 25, 27, 28, 30, 32,
	22, 24, 25, 27, 28, 30, 32, 33,
	24, 25, 27, 28, 30, 32, 33, 35,
}

var FlatWeightScale4x4 = [16]uint8{
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
}

var FlatWeightScale8x8 = [64]uint8{
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
}

var scan8x8Class = [16]uint8{
	0, 3, 4, 3, 3, 1, 5, 1, 4, 5, 2, 5, 3, 1, 5, 1,
}

var class8x8Pos [64]uint8

func init() {
	for pos := 0; pos < 64; pos++ {
		class8x8Pos[pos] = scan8x8Class[((pos>>1)&12)|(pos&3)]
	}
}

var normAdjust8x8 = [6][6]int32{
	{20, 18, 32, 19, 25, 24},
	{22, 19, 35, 21, 28, 26},
	{26, 23, 42, 24, 33, 31},
	{28, 25, 45, 26, 35, 33},
	{32, 28, 51, 30, 40, 38},
	{36, 32, 58, 34, 46, 43},
}

var quant8Scale = [6][6]int32{
	{13107, 11428, 20972, 12222, 16777, 15481},
	{11916, 10826, 19174, 11058, 14980, 14290},
	{10082, 8943, 15978, 9675, 12710, 11985},
	{9362, 8228, 14913, 8931, 11984, 11259},
	{8192, 7346, 13159, 7740, 10486, 9777},
	{7282, 6428, 11570, 6830, 9118, 8640},
}

type LevelScale8x8 [6][64]int32

type QuantScale8x8 [6][64]int32

func BuildLevelScale8x8(weightScale [64]uint8) LevelScale8x8 {
	var ls LevelScale8x8
	for m := 0; m < 6; m++ {
		for pos := 0; pos < 64; pos++ {
			ls[m][pos] = normAdjust8x8[m][class8x8Pos[pos]] * int32(weightScale[pos])
		}
	}
	return ls
}

func BuildQuantScale8x8(weightScale [64]uint8) QuantScale8x8 {
	var qs QuantScale8x8
	for m := 0; m < 6; m++ {
		for pos := 0; pos < 64; pos++ {
			base := quant8Scale[m][class8x8Pos[pos]] * 16
			w := int32(weightScale[pos])
			qs[m][pos] = (base + w/2) / w
		}
	}
	return qs
}

func Dequant8x8(b *Block8x8, qp int, ls *LevelScale8x8) {
	m := qp % 6
	shift := qp / 6
	row := &ls[m]
	if shift >= 6 {
		s := uint(shift - 6)
		for i := range b {
			b[i] = b[i] * row[i] << s
		}
		return
	}
	s := uint(6 - shift)
	round := int32(1) << (s - 1)
	for i := range b {
		b[i] = (b[i]*row[i] + round) >> s
	}
}

func Quant8x8(b *Block8x8, qp int, qs *QuantScale8x8, intra bool) {
	m := qp % 6
	qbits := uint(16 + qp/6)
	var f int32
	if intra {
		f = int32(1) << qbits / 3
	} else {
		f = int32(1) << qbits / 6
	}
	row := &qs[m]
	for i := range b {
		v := b[i]
		if v >= 0 {
			b[i] = (v*row[i] + f) >> qbits
		} else {
			b[i] = -((-v*row[i] + f) >> qbits)
		}
	}
}

type LevelScale4x4 [6][16]int32

type QuantScale4x4 [6][16]int32

func BuildLevelScale4x4(weightScale [16]uint8) LevelScale4x4 {
	var ls LevelScale4x4
	for m := 0; m < 6; m++ {
		for pos := 0; pos < 16; pos++ {
			ls[m][pos] = normAdjust[m][posClass[pos]] * int32(weightScale[pos])
		}
	}
	return ls
}

func BuildQuantScale4x4(weightScale [16]uint8) QuantScale4x4 {
	var qs QuantScale4x4
	for m := 0; m < 6; m++ {
		for pos := 0; pos < 16; pos++ {
			base := quantCoef[m][posClass[pos]] * 16
			w := int32(weightScale[pos])
			qs[m][pos] = (base + w/2) / w
		}
	}
	return qs
}
