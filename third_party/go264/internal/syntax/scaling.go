package syntax

var zigzagScan4x4 = [16]uint8{
	0, 1, 4, 8, 5, 2, 3, 6,
	9, 12, 13, 10, 7, 11, 14, 15,
}

var zigzagScan8x8 = [64]uint8{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

var rasterDefaultScalingList4x4Intra = [16]uint8{
	6, 13, 20, 28,
	13, 20, 28, 32,
	20, 28, 32, 37,
	28, 32, 37, 42,
}

var rasterDefaultScalingList4x4Inter = [16]uint8{
	10, 14, 20, 24,
	14, 20, 24, 27,
	20, 24, 27, 30,
	24, 27, 30, 34,
}

var rasterDefaultScalingList8x8Intra = [64]uint8{
	6, 10, 13, 16, 18, 23, 25, 27,
	10, 11, 16, 18, 23, 25, 27, 29,
	13, 16, 18, 23, 25, 27, 29, 31,
	16, 18, 23, 25, 27, 29, 31, 33,
	18, 23, 25, 27, 29, 31, 33, 36,
	23, 25, 27, 29, 31, 33, 36, 38,
	25, 27, 29, 31, 33, 36, 38, 40,
	27, 29, 31, 33, 36, 38, 40, 42,
}

var rasterDefaultScalingList8x8Inter = [64]uint8{
	9, 13, 15, 17, 19, 21, 22, 24,
	13, 13, 17, 19, 21, 22, 24, 25,
	15, 17, 19, 21, 22, 24, 25, 27,
	17, 19, 21, 22, 24, 25, 27, 28,
	19, 21, 22, 24, 25, 27, 28, 30,
	21, 22, 24, 25, 27, 28, 30, 32,
	22, 24, 25, 27, 28, 30, 32, 33,
	24, 25, 27, 28, 30, 32, 33, 35,
}

var flatScalingList4x4 = [16]uint8{
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
}

var flatScalingList8x8 = [64]uint8{
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
	16, 16, 16, 16, 16, 16, 16, 16,
}

func toScanOrder4x4(raster [16]uint8) [16]uint8 {
	var out [16]uint8
	for i, p := range zigzagScan4x4 {
		out[i] = raster[p]
	}
	return out
}

func toScanOrder8x8(raster [64]uint8) [64]uint8 {
	var out [64]uint8
	for i, p := range zigzagScan8x8 {
		out[i] = raster[p]
	}
	return out
}

var (
	scanDefaultScalingList4x4Intra = toScanOrder4x4(rasterDefaultScalingList4x4Intra)
	scanDefaultScalingList4x4Inter = toScanOrder4x4(rasterDefaultScalingList4x4Inter)
	scanDefaultScalingList8x8Intra = toScanOrder8x8(rasterDefaultScalingList8x8Intra)
	scanDefaultScalingList8x8Inter = toScanOrder8x8(rasterDefaultScalingList8x8Inter)
)

func (s *SPS) ScalingListsPresent() bool {
	return profileHasChromaFormat(s.ProfileIDC) && s.SeqScalingMatrixPresent
}

func (s *SPS) ResolvedScalingLists() (list4x4 [6][16]uint8, list8x8 [6][64]uint8) {
	if !s.ScalingListsPresent() {
		for i := range list4x4 {
			list4x4[i] = flatScalingList4x4
		}
		for i := range list8x8 {
			list8x8[i] = flatScalingList8x8
		}
		return
	}
	for i := 0; i < 6; i++ {
		switch {
		case s.ScalingList4x4Present[i] && s.UseDefaultScaling4x4[i]:
			if i < 3 {
				list4x4[i] = scanDefaultScalingList4x4Intra
			} else {
				list4x4[i] = scanDefaultScalingList4x4Inter
			}
		case s.ScalingList4x4Present[i]:
			list4x4[i] = s.ScalingList4x4[i]
		case i == 0:
			list4x4[0] = scanDefaultScalingList4x4Intra
		case i == 3:
			list4x4[3] = scanDefaultScalingList4x4Inter
		default:
			list4x4[i] = list4x4[i-1]
		}
	}
	n8 := 2
	if s.ChromaFormatIDC == Chroma444 {
		n8 = 6
	}
	for i := 0; i < n8; i++ {
		switch {
		case s.ScalingList8x8Present[i] && s.UseDefaultScaling8x8[i]:
			if i%2 == 0 {
				list8x8[i] = scanDefaultScalingList8x8Intra
			} else {
				list8x8[i] = scanDefaultScalingList8x8Inter
			}
		case s.ScalingList8x8Present[i]:
			list8x8[i] = s.ScalingList8x8[i]
		case i == 0:
			list8x8[0] = scanDefaultScalingList8x8Intra
		case i == 1:
			list8x8[1] = scanDefaultScalingList8x8Inter
		default:
			list8x8[i] = list8x8[i-2]
		}
	}
	for i := n8; i < 6; i++ {
		list8x8[i] = flatScalingList8x8
	}
	return
}

func (p *PPS) ResolvedScalingLists(sps *SPS) (list4x4 [6][16]uint8, list8x8 [6][64]uint8) {
	seqPresent := sps != nil && sps.ScalingListsPresent()
	var spsList4x4 [6][16]uint8
	var spsList8x8 [6][64]uint8
	if seqPresent {
		spsList4x4, spsList8x8 = sps.ResolvedScalingLists()
	}
	if !p.PicScalingMatrixPresent {
		if seqPresent {
			return spsList4x4, spsList8x8
		}
		for i := range list4x4 {
			list4x4[i] = flatScalingList4x4
		}
		for i := range list8x8 {
			list8x8[i] = flatScalingList8x8
		}
		return
	}
	fallback4Intra, fallback4Inter := scanDefaultScalingList4x4Intra, scanDefaultScalingList4x4Inter
	fallback8Intra, fallback8Inter := scanDefaultScalingList8x8Intra, scanDefaultScalingList8x8Inter
	if seqPresent {
		fallback4Intra, fallback4Inter = spsList4x4[0], spsList4x4[3]
		fallback8Intra, fallback8Inter = spsList8x8[0], spsList8x8[3]
	}
	for i := 0; i < 6; i++ {
		switch {
		case p.ScalingList4x4Present[i] && p.UseDefaultScaling4x4[i]:
			if i < 3 {
				list4x4[i] = scanDefaultScalingList4x4Intra
			} else {
				list4x4[i] = scanDefaultScalingList4x4Inter
			}
		case p.ScalingList4x4Present[i]:
			list4x4[i] = p.ScalingList4x4[i]
		case i == 0:
			list4x4[0] = fallback4Intra
		case i == 3:
			list4x4[3] = fallback4Inter
		default:
			list4x4[i] = list4x4[i-1]
		}
	}
	n8 := 0
	if p.Transform8x8Mode {
		n8 = 2
		if sps != nil && sps.ChromaFormatIDC == Chroma444 {
			n8 = 6
		}
	}
	for i := 0; i < n8; i++ {
		switch {
		case p.ScalingList8x8Present[i] && p.UseDefaultScaling8x8[i]:
			if i%2 == 0 {
				list8x8[i] = scanDefaultScalingList8x8Intra
			} else {
				list8x8[i] = scanDefaultScalingList8x8Inter
			}
		case p.ScalingList8x8Present[i]:
			list8x8[i] = p.ScalingList8x8[i]
		case i == 0:
			list8x8[0] = fallback8Intra
		case i == 1:
			list8x8[1] = fallback8Inter
		default:
			list8x8[i] = list8x8[i-2]
		}
	}
	for i := n8; i < 6; i++ {
		list8x8[i] = flatScalingList8x8
	}
	return
}
