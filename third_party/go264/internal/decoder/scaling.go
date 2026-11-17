package decoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/transform"
)

var dequant4CoeffInit = [6][3]int32{
	{10, 13, 16},
	{11, 14, 18},
	{13, 16, 20},
	{14, 18, 23},
	{16, 20, 25},
	{18, 23, 29},
}

func dequant4Class(raster int) int { return raster&1 + raster>>2&1 }

const (
	scaleIntraY = 0
	scaleIntraU = 1
	scaleIntraV = 2
	scaleInterY = 3
	scaleInterU = 4
	scaleInterV = 5
)

type levelScale4x4 [6][16]int32

type scalingTables struct {
	list4x4 [6]levelScale4x4
	list8x8 [2]transform.LevelScale8x8
	flat    bool
}

func rasterOrder4x4(scan [16]uint8) [16]uint8 {
	var out [16]uint8
	for i := 0; i < 16; i++ {
		out[transform.ZigZagIndex(i)] = scan[i]
	}
	return out
}

func rasterOrder8x8(scan [64]uint8) [64]uint8 {
	var out [64]uint8
	for i := 0; i < 64; i++ {
		out[transform.ZigZagScan8x8[i]] = scan[i]
	}
	return out
}

func buildScalingTables(sps *syntax.SPS, pps *syntax.PPS) *scalingTables {
	scan4, scan8 := pps.ResolvedScalingLists(sps)
	s := &scalingTables{flat: true}
	for list := 0; list < 6; list++ {
		w := rasterOrder4x4(scan4[list])
		if w != transform.FlatWeightScale4x4 {
			s.flat = false
		}
		for m := 0; m < 6; m++ {
			for pos := 0; pos < 16; pos++ {
				s.list4x4[list][m][pos] = dequant4CoeffInit[m][dequant4Class(pos)] * int32(w[pos])
			}
		}
	}
	for list := 0; list < 2; list++ {
		w := rasterOrder8x8(scan8[list])
		if w != transform.FlatWeightScale8x8 {
			s.flat = false
		}
		s.list8x8[list] = transform.BuildLevelScale8x8(w)
	}
	return s
}

func (s *scalingTables) luma4x4(intra bool) *levelScale4x4 {
	if intra {
		return &s.list4x4[scaleIntraY]
	}
	return &s.list4x4[scaleInterY]
}

func (s *scalingTables) chroma4x4(intra bool, plane int) *levelScale4x4 {
	if intra {
		return &s.list4x4[scaleIntraU+plane]
	}
	return &s.list4x4[scaleInterU+plane]
}

func (s *scalingTables) luma8x8(intra bool) *transform.LevelScale8x8 {
	if intra {
		return &s.list8x8[0]
	}
	return &s.list8x8[1]
}

func dequant4x4(b *transform.Block, qp int, scale *levelScale4x4, skipDC bool) {
	row := &scale[qp%6]
	shift := qp / 6
	var savedDC int32
	if skipDC {
		savedDC = b[0]
	}
	if shift >= 4 {
		s := uint(shift - 4)
		for i := range b {
			b[i] = b[i] * row[i] << s
		}
	} else {
		s := uint(4 - shift)
		round := int32(1) << (s - 1)
		for i := range b {
			b[i] = (b[i]*row[i] + round) >> s
		}
	}
	if skipDC {
		b[0] = savedDC
	}
}

func dequantLumaDC(b *transform.Block, qp int, scale *levelScale4x4) {
	transform.Hadamard4x4(b)
	v := scale[qp%6][0]
	shift := qp / 6
	if shift >= 6 {
		s := uint(shift - 6)
		for i := range b {
			b[i] = b[i] * v << s
		}
		return
	}
	s := uint(6 - shift)
	round := int32(1) << (s - 1)
	for i := range b {
		b[i] = (b[i]*v + round) >> s
	}
}

func dequantChromaDC(b *transform.ChromaDC, qp int, scale *levelScale4x4) {
	transform.Hadamard2x2(b)
	v := scale[qp%6][0]
	shift := uint(qp / 6)
	for i := range b {
		b[i] = (b[i] * v << shift) >> 5
	}
}
