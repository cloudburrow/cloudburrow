package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cavlc"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/pred"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/transform"
)

func residual8x8(dst *transform.Block8x8, src []byte, srcStride, srcOff int, ref []byte, refStride, refOff int) {
	for y := 0; y < 8; y++ {
		s := src[srcOff+y*srcStride:]
		r := ref[refOff+y*refStride:]
		for x := 0; x < 8; x++ {
			dst[y*8+x] = int32(s[x]) - int32(r[x])
		}
	}
}

func satdBlock8x8Transform(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff int) int {
	var block transform.Block8x8
	residual8x8(&block, src, srcStride, srcOff, ref, refStride, refOff)
	transform.Forward8x8(&block)
	total := int32(0)
	for _, c := range block {
		total += abs32(c)
	}
	return int((total + 2) >> 2)
}

func block8x8ToScan(dst *[64]int32, b *transform.Block8x8) {
	for i := 0; i < 64; i++ {
		dst[i] = b[transform.ZigZagScan8x8[i]]
	}
}

func scanToBlock8x8(dst *transform.Block8x8, scan *[64]int32) {
	for i := 0; i < 64; i++ {
		dst[transform.ZigZagScan8x8[i]] = scan[i]
	}
}

func rasterOrder8x8(scan [64]uint8) [64]uint8 {
	var out [64]uint8
	for i := 0; i < 64; i++ {
		out[transform.ZigZagScan8x8[i]] = scan[i]
	}
	return out
}

func (e *Encoder) quantScale8x8(intra bool) *transform.QuantScale8x8 {
	if intra {
		return &e.quant8x8[0]
	}
	return &e.quant8x8[1]
}

func (e *Encoder) levelScale8x8(intra bool) *transform.LevelScale8x8 {
	if intra {
		return &e.level8x8[0]
	}
	return &e.level8x8[1]
}

func luma8x8Availability(i8 int, n neighbours) pred.Availability {
	var a pred.Availability
	if i8%2 == 1 || n.left != nil {
		a |= pred.AvailLeft
	}
	if i8/2 == 1 || n.top != nil {
		a |= pred.AvailTop
	}
	switch i8 {
	case 0:
		if n.topLeft != nil {
			a |= pred.AvailTopLeft
		}
		if n.top != nil {
			a |= pred.AvailTopRight
		}
	case 1:
		if n.top != nil {
			a |= pred.AvailTopLeft
		}
		if n.topRight != nil {
			a |= pred.AvailTopRight
		}
	case 2:
		if n.left != nil {
			a |= pred.AvailTopLeft
		}
		a |= pred.AvailTopRight
	default:
		a |= pred.AvailTopLeft
	}
	return a
}

func (s *mbEncoder) luma8x8Offset(i8 int) int {
	return s.e.rec.LumaOffset(s.mbx*16+i8%2*8, s.mby*16+i8/2*8)
}

func (s *mbEncoder) source8x8Offset(i8 int) int {
	return s.e.src.LumaOffset(s.mbx*16+i8%2*8, s.mby*16+i8/2*8)
}

func (s *mbEncoder) setIntra8x8Mode(i8, mode int) {
	for k := 0; k < 4; k++ {
		s.cur.intra4Modes[i8*4+k] = int8(mode)
	}
}

func (s *mbEncoder) setNz8x8(i8 int) {
	scan := &s.luma8x8Scan[i8]
	if s.cb != nil {
		n := uint8(countNonZero(scan[:]))
		for i4 := 0; i4 < 4; i4++ {
			s.cur.NzY[i8*4+i4] = n
		}
		return
	}
	for i4 := 0; i4 < 4; i4++ {
		n := 0
		for k := 0; k < 16; k++ {
			if scan[4*k+i4] != 0 {
				n++
			}
		}
		s.cur.NzY[i8*4+i4] = uint8(n)
	}
}

func (s *mbEncoder) sub4x4Of8x8(i8, i4 int) [16]int32 {
	var out [16]int32
	for k := 0; k < 16; k++ {
		out[k] = s.luma8x8Scan[i8][4*k+i4]
	}
	return out
}

func (s *mbEncoder) transform8x8Inc() int {
	inc := 0
	if m := s.nb.left; m != nil && m.Transform8x8 {
		inc++
	}
	if m := s.nb.top; m != nil && m.Transform8x8 {
		inc++
	}
	return inc
}

func (s *mbEncoder) noSubMbPartSizeLessThan8x8() bool {
	switch s.cur.kind {
	case mbTypeP8x8:
		return subsAreWhole8x8(s.subs)
	case mbTypeB8x8:
		return s.bSubsAreWhole8x8(s.bsubs)
	case mbTypeBDirect, mbTypeBSkip:
		return s.e.sps.Direct8x8Inference
	}
	return true
}

func subsAreWhole8x8(subs []subResult) bool {
	for _, sub := range subs {
		if subMbShapes[sub.subType].numParts != 1 {
			return false
		}
	}
	return true
}

func (s *mbEncoder) bSubsAreWhole8x8(subs []bSubResult) bool {
	for _, sub := range subs {
		if sub.subType == 0 {
			if !s.e.sps.Direct8x8Inference {
				return false
			}
			continue
		}
		if bSubTypes[sub.subType].numParts != 1 {
			return false
		}
	}
	return true
}

func (s *mbEncoder) mayUse8x8Transform() bool {
	return s.e.pps.Transform8x8Mode && s.cur.cbpLuma != 0 && s.noSubMbPartSizeLessThan8x8()
}

func (s *mbEncoder) writeTransformSize8x8() {
	if !s.mayUse8x8Transform() {
		return
	}
	s.w.WriteFlag(s.cur.Transform8x8)
}

func (s *mbEncoder) writeTransformSize8x8CABAC() {
	if !s.mayUse8x8Transform() {
		return
	}
	s.cb.TransformSize8x8Flag(s.transform8x8Inc(), s.cur.Transform8x8)
}

func (s *mbEncoder) allows8x8(kind int, subs []subResult) bool {
	if !s.e.pps.Transform8x8Mode {
		return false
	}
	if kind != mbTypeP8x8 {
		return true
	}
	return subsAreWhole8x8(subs)
}

func (s *mbEncoder) allowsB8x8(kind int, subs []bSubResult) bool {
	if !s.e.pps.Transform8x8Mode {
		return false
	}
	switch kind {
	case mbTypeB8x8:
		return s.bSubsAreWhole8x8(subs)
	case mbTypeBDirect:
		return s.e.sps.Direct8x8Inference
	}
	return true
}

func (s *mbEncoder) encodeIntra8x8() int {
	lambda := lambdaTable[s.qpY]
	total := 0
	var block transform.Block8x8
	s.cur.cbpLuma = 0
	for i8 := 0; i8 < 4; i8++ {
		a := luma8x8Availability(i8, s.nb)
		off := s.luma8x8Offset(i8)
		srcOff := s.source8x8Offset(i8)
		predMode := s.predIntra4x4Mode(i8 * 4)

		best := -1
		bestMode := pred.I8x8DC
		for mode := 0; mode < 9; mode++ {
			if !pred.Intra8x8ModeAvailable(mode, a) {
				continue
			}
			pred.Intra8x8(s.e.rec.Y, s.e.rec.StrideY, off, mode, a)
			c := satdBlock8x8Transform(s.e.src.Y, s.e.src.StrideY, srcOff, s.e.rec.Y, s.e.rec.StrideY, off)
			if mode != predMode {
				c += lambda * 3
			}
			if best < 0 || c < best {
				best = c
				bestMode = mode
			}
		}
		total += best
		s.setIntra8x8Mode(i8, bestMode)

		pred.Intra8x8(s.e.rec.Y, s.e.rec.StrideY, off, bestMode, a)
		residual8x8(&block, s.e.src.Y, s.e.src.StrideY, srcOff, s.e.rec.Y, s.e.rec.StrideY, off)
		transform.Forward8x8(&block)
		orig := block
		transform.Quant8x8(&block, s.qpY, s.e.quantScale8x8(true), true)
		block8x8ToScan(&s.luma8x8Scan[i8], &block)
		if s.trellis {
			s.trellisLuma8x8(i8, &orig, true)
			scanToBlock8x8(&block, &s.luma8x8Scan[i8])
		}
		s.setNz8x8(i8)
		if countNonZero(s.luma8x8Scan[i8][:]) == 0 {
			continue
		}
		s.cur.cbpLuma |= 1 << uint(i8)
		transform.Dequant8x8(&block, s.qpY, s.e.levelScale8x8(true))
		transform.Inverse8x8(&block)
		transform.AddResidual8x8(s.e.rec.Y, s.e.rec.StrideY, off, &block)
	}
	return total
}

func (s *mbEncoder) quantiseInterLuma8x8() {
	var block transform.Block8x8
	s.cur.cbpLuma = 0
	for i8 := 0; i8 < 4; i8++ {
		off := s.luma8x8Offset(i8)
		srcOff := s.source8x8Offset(i8)
		residual8x8(&block, s.e.src.Y, s.e.src.StrideY, srcOff, s.e.rec.Y, s.e.rec.StrideY, off)
		transform.Forward8x8(&block)
		orig := block
		transform.Quant8x8(&block, s.qpY, s.e.quantScale8x8(false), false)
		block8x8ToScan(&s.luma8x8Scan[i8], &block)
		if s.trellis {
			s.trellisLuma8x8(i8, &orig, false)
		}
		s.setNz8x8(i8)
		if countNonZero(s.luma8x8Scan[i8][:]) != 0 {
			s.cur.cbpLuma |= 1 << uint(i8)
		}
	}
}

func (s *mbEncoder) reconstructInterLuma8x8() {
	var block transform.Block8x8
	for i8 := 0; i8 < 4; i8++ {
		if s.cur.cbpLuma&(1<<uint(i8)) == 0 {
			continue
		}
		scanToBlock8x8(&block, &s.luma8x8Scan[i8])
		transform.Dequant8x8(&block, s.qpY, s.e.levelScale8x8(false))
		transform.Inverse8x8(&block)
		transform.AddResidual8x8(s.e.rec.Y, s.e.rec.StrideY, s.luma8x8Offset(i8), &block)
	}
}

func (s *mbEncoder) writeLuma8x8(i8 int) error {
	for i4 := 0; i4 < 4; i4++ {
		sub := s.sub4x4Of8x8(i8, i4)
		if err := cavlc.WriteBlock(s.w, sub[:], s.lumaNC(i8*4+i4)); err != nil {
			return err
		}
	}
	return nil
}

func (s *mbEncoder) writeIntra8x8Modes() {
	for i8 := 0; i8 < 4; i8++ {
		mode := int(s.cur.intra4Modes[i8*4])
		predMode := s.predIntra4x4Mode(i8 * 4)
		if mode == predMode {
			s.w.WriteFlag(true)
			continue
		}
		s.w.WriteFlag(false)
		rem := mode
		if mode > predMode {
			rem = mode - 1
		}
		s.w.WriteBits(uint32(rem), 3)
	}
}

type intraLumaState struct {
	rec     [256]byte
	scan    [4][64]int32
	nz      [16]uint8
	modes   [16]int8
	cbpLuma int
}

func (s *mbEncoder) saveIntraLuma(st *intraLumaState) {
	for y := 0; y < 16; y++ {
		off := s.e.rec.LumaOffset(s.mbx*16, s.mby*16+y)
		copy(st.rec[y*16:y*16+16], s.e.rec.Y[off:off+16])
	}
	st.scan = s.luma8x8Scan
	st.nz = s.cur.NzY
	st.modes = s.cur.intra4Modes
	st.cbpLuma = s.cur.cbpLuma
}

func (s *mbEncoder) restoreIntraLuma(st *intraLumaState) {
	for y := 0; y < 16; y++ {
		off := s.e.rec.LumaOffset(s.mbx*16, s.mby*16+y)
		copy(s.e.rec.Y[off:off+16], st.rec[y*16:y*16+16])
	}
	s.luma8x8Scan = st.scan
	s.cur.NzY = st.nz
	s.cur.intra4Modes = st.modes
	s.cur.cbpLuma = st.cbpLuma
}
