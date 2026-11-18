package decoder

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
)

func minPositive(a, b int8) int8 {
	if a >= 0 && b >= 0 {
		if a < b {
			return a
		}
		return b
	}
	if a > b {
		return a
	}
	return b
}

type colBlock struct {
	mv     [2]int16
	refIdx int8
	refPOC int32
	intra  bool
}

func (d *sliceDecoder) colocatedAt(bx4, by4 int) colBlock {
	if d.colocated == nil || d.colocated.Motion == nil {
		return colBlock{intra: true}
	}
	if d.direct8x8 {
		if bx4 < 2 {
			bx4 = 0
		} else {
			bx4 = 3
		}
		if by4 < 2 {
			by4 = 0
		} else {
			by4 = 3
		}
	}
	mo := d.colocated.Motion
	x := d.mbx*4 + bx4
	y := d.mby*4 + by4
	if x >= mo.BlocksWide || y >= mo.BlocksHigh {
		return colBlock{intra: true}
	}
	i := mo.Index(x, y)
	for list := 0; list < 2; list++ {
		if mo.RefIdx[list][i] >= 0 {
			return colBlock{
				mv:     mo.Mv[list][i],
				refIdx: mo.RefIdx[list][i],
				refPOC: mo.RefPOC[list][i],
			}
		}
	}
	return colBlock{intra: true}
}

func (d *sliceDecoder) colZero(bx4, by4 int) bool {
	if !d.colShortTerm {
		return false
	}
	c := d.colocatedAt(bx4, by4)
	if c.intra || c.refIdx != 0 {
		return false
	}
	return c.mv[0] >= -1 && c.mv[0] <= 1 && c.mv[1] >= -1 && c.mv[1] <= 1
}

func (d *sliceDecoder) spatialDirectBase() (ref [2]int8, mv [2][2]int16, zeroPred bool) {
	for list := 0; list < 2; list++ {
		a, b, c := d.neighboursFor(list, 0, 0, 16)
		ref[list] = minPositive(a.ref, minPositive(b.ref, c.ref))
	}
	if ref[0] < 0 && ref[1] < 0 {
		return [2]int8{0, 0}, [2][2]int16{}, true
	}
	for list := 0; list < 2; list++ {
		if ref[list] >= 0 {
			mv[list] = d.predictMV(list, 0, 0, 16, 16, ref[list], 0, mbTypeB16x16)
		}
	}
	return ref, mv, false
}

func (d *sliceDecoder) spatialDirect(x, y, w, h int) {
	ref, mv, zeroPred := d.spatialDirectBase()
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			blockMv := mv
			if !zeroPred && d.colZero(bx>>2, by>>2) {
				for list := 0; list < 2; list++ {
					if ref[list] == 0 {
						blockMv[list] = [2]int16{}
					}
				}
			}
			for list := 0; list < 2; list++ {
				d.storeMotion(list, bx, by, 4, 4, blockMv[list], ref[list])
			}
		}
	}
}

func (d *sliceDecoder) mapColToList0(refPOC int32) int8 {
	for i, p := range d.refList {
		if p != nil && int32(p.POC) == refPOC {
			return int8(i)
		}
	}
	return 0
}

func scaleMV(v int16, dist int) int16 {
	return int16((dist*int(v) + 128) >> 8)
}

func (d *sliceDecoder) distScaleFactor(p0 *frame.Picture) (int, bool) {
	p1 := d.refPictureIn(1, 0)
	if p0 == nil || p1 == nil || p0.LongTerm {
		return 0, false
	}
	td := clip3(-128, 127, p1.POC-p0.POC)
	if td == 0 {
		return 0, false
	}
	tb := clip3(-128, 127, d.pic.POC-p0.POC)
	abs := td / 2
	if abs < 0 {
		abs = -abs
	}
	tx := (16384 + abs) / td
	return clip3(-1024, 1023, (tb*tx+32)>>6), true
}

func (d *sliceDecoder) temporalDirect(x, y, w, h int) {
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			c := d.colocatedAt(bx>>2, by>>2)
			if c.intra {
				d.storeMotion(0, bx, by, 4, 4, [2]int16{}, 0)
				d.storeMotion(1, bx, by, 4, 4, [2]int16{}, 0)
				continue
			}
			ref0 := d.mapColToList0(c.refPOC)
			dist, scaled := d.distScaleFactor(d.refPictureIn(0, ref0))
			if !scaled {
				d.storeMotion(0, bx, by, 4, 4, c.mv, ref0)
				d.storeMotion(1, bx, by, 4, 4, [2]int16{}, 0)
				continue
			}
			mv0 := [2]int16{scaleMV(c.mv[0], dist), scaleMV(c.mv[1], dist)}
			mv1 := [2]int16{mv0[0] - c.mv[0], mv0[1] - c.mv[1]}
			d.storeMotion(0, bx, by, 4, 4, mv0, ref0)
			d.storeMotion(1, bx, by, 4, 4, mv1, 0)
		}
	}
}

func (d *sliceDecoder) directMotion(x, y, w, h int) {
	if d.directSpatial {
		d.spatialDirect(x, y, w, h)
		return
	}
	d.temporalDirect(x, y, w, h)
}

func (d *sliceDecoder) maxRefFor(list int) uint32 {
	if list == 0 {
		return uint32(d.numRefIdxActive - 1)
	}
	return uint32(d.numRefIdxL1 - 1)
}

func (d *sliceDecoder) markDirect(x, y, w, h int) {
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			d.cur.directBlk[zscanOf[by>>2][bx>>2]] = true
		}
	}
}

func (d *sliceDecoder) decodeBSkip() error {
	d.cur.kind = mbTypeBSkip
	d.cur.Intra = false
	d.markDirect(0, 0, 16, 16)
	d.cur.cbpLuma = 0
	d.cur.cbpChroma = 0
	d.cur.QPY = d.qpY
	d.directMotion(0, 0, 16, 16)
	d.motionCompensate()
	return nil
}

func (d *sliceDecoder) readBSubTypes() ([4]bSubTypeInfo, error) {
	var sub [4]bSubTypeInfo
	anyDirect := false
	for i := 0; i < 4; i++ {
		v, err := d.r.ReadUE()
		if err != nil {
			return sub, err
		}
		if v > 12 {
			return sub, fmt.Errorf("%w: sub_mb_type %d in a B slice", ErrCorrupt, v)
		}
		sub[i] = bSubTypes[v]
		anyDirect = anyDirect || sub[i].direct
	}
	d.noteBSubPartSizes(sub)
	d.applySubDirect(sub, anyDirect)
	return sub, nil
}

func (d *sliceDecoder) noteBSubPartSizes(sub [4]bSubTypeInfo) {
	for i := 0; i < 4; i++ {
		if sub[i].direct {
			if !d.direct8x8 {
				d.subPartsAtLeast8x8 = false
			}
			continue
		}
		if sub[i].numParts != 1 {
			d.subPartsAtLeast8x8 = false
		}
	}
}

func (d *sliceDecoder) applySubDirect(sub [4]bSubTypeInfo, anyDirect bool) {
	if !anyDirect {
		return
	}
	d.directMotion(0, 0, 16, 16)
	for i := 0; i < 4; i++ {
		if sub[i].direct {
			d.markDirect(i%2*8, i/2*8, 8, 8)
		}
	}
}

func (d *sliceDecoder) decodeB8x8(sub [4]bSubTypeInfo) error {
	var refs [2][4]int8
	for list := 0; list < 2; list++ {
		for i := 0; i < 4; i++ {
			refs[list][i] = -1
			if sub[i].direct {
				continue
			}
			if sub[i].pred&(1<<uint(list)) == 0 {
				d.storeRefIdx(list, i%2*8, i/2*8, 8, 8, -1)
				continue
			}
			r, err := d.readRefIdx(d.maxRefFor(list))
			if err != nil {
				return err
			}
			refs[list][i] = r
			d.storeRefIdx(list, i%2*8, i/2*8, 8, 8, r)
		}
	}
	for list := 0; list < 2; list++ {
		for i := 0; i < 4; i++ {
			if sub[i].direct || sub[i].pred&(1<<uint(list)) == 0 {
				if !sub[i].direct {
					d.storeMotion(list, i%2*8, i/2*8, 8, 8, [2]int16{}, -1)
				}
				continue
			}
			s := sub[i]
			ox, oy := i%2*8, i/2*8
			cols := 8 / s.w
			for p := 0; p < s.numParts; p++ {
				px := ox + p%cols*s.w
				py := oy + p/cols*s.h
				mvd, err := d.readMVD()
				if err != nil {
					return err
				}
				d.storeMVD(list, px, py, s.w, s.h, mvd)
				mvp := d.predictMV(list, px, py, s.w, s.h, refs[list][i], p, mbTypeB8x8)
				d.storeMotion(list, px, py, s.w, s.h,
					[2]int16{mvp[0] + mvd[0], mvp[1] + mvd[1]}, refs[list][i])
			}
		}
	}
	return nil
}

func (d *sliceDecoder) decodeInterMBB(info bMBTypeInfo, res *mbResidual) error {
	d.cur.kind = info.kind
	d.cur.Intra = false

	switch {
	case info.direct:
		d.subPartsAtLeast8x8 = d.direct8x8
		d.directMotion(0, 0, 16, 16)
		d.markDirect(0, 0, 16, 16)

	case info.kind == mbTypeB8x8:
		sub, err := d.readBSubTypes()
		if err != nil {
			return err
		}
		if err := d.decodeB8x8(sub); err != nil {
			return err
		}

	default:
		parts := bPartitions(info.kind)
		var refs [2][2]int8
		for list := 0; list < 2; list++ {
			for i := range parts {
				refs[list][i] = -1
				if info.pred[i]&(1<<uint(list)) == 0 {
					continue
				}
				r, err := d.readRefIdx(d.maxRefFor(list))
				if err != nil {
					return err
				}
				refs[list][i] = r
				d.storeRefIdx(list, parts[i].x, parts[i].y, parts[i].w, parts[i].h, r)
			}
		}
		for list := 0; list < 2; list++ {
			for i, p := range parts {
				if refs[list][i] < 0 {
					d.storeMotion(list, p.x, p.y, p.w, p.h, [2]int16{}, -1)
					continue
				}
				mvd, err := d.readMVD()
				if err != nil {
					return err
				}
				d.storeMVD(list, p.x, p.y, p.w, p.h, mvd)
				mvp := d.predictMV(list, p.x, p.y, p.w, p.h, refs[list][i], i, info.kind)
				d.storeMotion(list, p.x, p.y, p.w, p.h,
					[2]int16{mvp[0] + mvd[0], mvp[1] + mvd[1]}, refs[list][i])
			}
		}
	}

	if err := d.parseCBP(false); err != nil {
		return err
	}
	if err := d.readTransformSize8x8(); err != nil {
		return err
	}
	if d.cur.cbpLuma != 0 || d.cur.cbpChroma != 0 {
		if err := d.parseQPDelta(); err != nil {
			return err
		}
		if err := d.readResidual(res, false); err != nil {
			return err
		}
	}
	d.cur.QPY = d.qpY
	d.motionCompensate()
	d.addInterResidual(res)
	return nil
}
