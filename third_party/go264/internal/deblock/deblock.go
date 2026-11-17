package deblock

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"

func clip3(lo, hi, x int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func clip1(x int) int {
	return clip3(0, 255, x)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func IndexA(qpAv, filterOffsetA int) int {
	return clip3(0, 51, qpAv+filterOffsetA)
}

func IndexB(qpAv, filterOffsetB int) int {
	return clip3(0, 51, qpAv+filterOffsetB)
}

func AverageQP(qpP, qpQ int) int {
	return (qpP + qpQ + 1) >> 1
}

func filterLumaSample(plane []byte, base, dir int, bS uint8, alpha, beta, indexA int) {
	p0i, p1i, p2i, p3i := base-dir, base-2*dir, base-3*dir, base-4*dir
	q0i, q1i, q2i, q3i := base, base+dir, base+2*dir, base+3*dir

	p0, p1, p2, p3 := int(plane[p0i]), int(plane[p1i]), int(plane[p2i]), int(plane[p3i])
	q0, q1, q2, q3 := int(plane[q0i]), int(plane[q1i]), int(plane[q2i]), int(plane[q3i])

	if abs(p0-q0) >= alpha || abs(p1-p0) >= beta || abs(q1-q0) >= beta {
		return
	}

	if bS < 4 {
		tc0 := int(tc0Table[bS-1][indexA])
		ap := abs(p2 - p0)
		aq := abs(q2 - q0)
		tc := tc0
		if ap < beta {
			tc++
		}
		if aq < beta {
			tc++
		}
		delta := clip3(-tc, tc, (((q0-p0)<<2)+(p1-q1)+4)>>3)
		newP0 := clip1(p0 + delta)
		newQ0 := clip1(q0 - delta)
		var newP1, newQ1 int
		filterP1 := ap < beta
		filterQ1 := aq < beta
		if filterP1 {
			newP1 = p1 + clip3(-tc0, tc0, (p2+((p0+q0+1)>>1)-(p1<<1))>>1)
		}
		if filterQ1 {
			newQ1 = q1 + clip3(-tc0, tc0, (q2+((p0+q0+1)>>1)-(q1<<1))>>1)
		}
		plane[p0i] = byte(newP0)
		plane[q0i] = byte(newQ0)
		if filterP1 {
			plane[p1i] = byte(newP1)
		}
		if filterQ1 {
			plane[q1i] = byte(newQ1)
		}
		return
	}

	ap := abs(p2 - p0)
	aq := abs(q2 - q0)
	small := abs(p0-q0) < ((alpha >> 2) + 2)
	filterP := ap < beta && small
	filterQ := aq < beta && small

	var newP0, newP1, newP2 int
	if filterP {
		newP0 = (p2 + 2*p1 + 2*p0 + 2*q0 + q1 + 4) >> 3
		newP1 = (p2 + p1 + p0 + q0 + 2) >> 2
		newP2 = (2*p3 + 3*p2 + p1 + p0 + q0 + 4) >> 3
	} else {
		newP0 = (2*p1 + p0 + q1 + 2) >> 2
	}

	var newQ0, newQ1, newQ2 int
	if filterQ {
		newQ0 = (q2 + 2*q1 + 2*q0 + 2*p0 + p1 + 4) >> 3
		newQ1 = (q2 + q1 + q0 + p0 + 2) >> 2
		newQ2 = (2*q3 + 3*q2 + q1 + q0 + p0 + 4) >> 3
	} else {
		newQ0 = (2*q1 + q0 + p1 + 2) >> 2
	}

	plane[p0i] = byte(newP0)
	plane[q0i] = byte(newQ0)
	if filterP {
		plane[p1i] = byte(newP1)
		plane[p2i] = byte(newP2)
	}
	if filterQ {
		plane[q1i] = byte(newQ1)
		plane[q2i] = byte(newQ2)
	}
}

func filterChromaSample(plane []byte, base, dir int, bS uint8, alpha, beta, indexA int) {
	p0i, p1i := base-dir, base-2*dir
	q0i, q1i := base, base+dir

	p0, p1 := int(plane[p0i]), int(plane[p1i])
	q0, q1 := int(plane[q0i]), int(plane[q1i])

	if abs(p0-q0) >= alpha || abs(p1-p0) >= beta || abs(q1-q0) >= beta {
		return
	}

	if bS < 4 {
		tc := int(tc0Table[bS-1][indexA]) + 1
		delta := clip3(-tc, tc, (((q0-p0)<<2)+(p1-q1)+4)>>3)
		plane[p0i] = byte(clip1(p0 + delta))
		plane[q0i] = byte(clip1(q0 - delta))
		return
	}

	newP0 := (2*p1 + p0 + q1 + 2) >> 2
	newQ0 := (2*q1 + q0 + p1 + 2) >> 2
	plane[p0i] = byte(newP0)
	plane[q0i] = byte(newQ0)
}

func filterEdge(plane []byte, offset, lineStep, sampleStep, numLines, groupSize int, bS [4]uint8, indexA, indexB int, chroma bool) {
	alpha := int(alphaTable[indexA])
	beta := int(betaTable[indexB])
	if alpha == 0 || beta == 0 {
		return
	}
	for line := 0; line < numLines; line++ {
		b := bS[line/groupSize]
		if b == 0 {
			continue
		}
		base := offset + line*lineStep
		if chroma {
			filterChromaSample(plane, base, sampleStep, b, alpha, beta, indexA)
		} else {
			filterLumaSample(plane, base, sampleStep, b, alpha, beta, indexA)
		}
	}
}

func FilterLumaEdgeVertical(plane []byte, stride, offset int, bS [4]uint8, indexA, indexB int) {
	if acceleratedLumaEdgeVertical(plane, offset, stride, bS, indexA, indexB) {
		return
	}
	filterEdge(plane, offset, stride, 1, 16, 4, bS, indexA, indexB, false)
}

func acceleratedLumaEdgeVertical(plane []byte, offset, stride int, bS [4]uint8, indexA, indexB int) bool {
	alpha := int(alphaTable[indexA])
	beta := int(betaTable[indexB])
	if alpha == 0 || beta == 0 {
		return true
	}
	tc0, bs, kind := edgeStrengths(bS, indexA)
	switch kind {
	case edgeIdle:
		return true
	case edgeNormal:
		return simd.DeblockLumaVerticalNormal(plane, offset, stride, &tc0, &bs, alpha, beta)
	}
	return false
}

type edgeKind int

const (
	edgeIdle edgeKind = iota
	edgeNormal
	edgeStrong
	edgeMixed
)

func edgeStrengths(bS [4]uint8, indexA int) (tc0, bs [16]uint8, kind edgeKind) {
	strong, weak := 0, 0
	for g, b := range bS {
		switch {
		case b == 0:
		case b == 4:
			strong++
		default:
			weak++
		}
		for i := 0; i < 4; i++ {
			bs[g*4+i] = b
			if b >= 1 && b <= 3 {
				tc0[g*4+i] = tc0Table[b-1][indexA]
			}
		}
	}
	switch {
	case strong+weak == 0:
		kind = edgeIdle
	case strong == 4:
		kind = edgeStrong
	case strong == 0:
		kind = edgeNormal
	default:
		kind = edgeMixed
	}
	return
}

func FilterLumaEdgeHorizontal(plane []byte, stride, offset int, bS [4]uint8, indexA, indexB int) {
	if acceleratedLumaEdge(plane, offset, stride, bS, indexA, indexB) {
		return
	}
	filterEdge(plane, offset, 1, stride, 16, 4, bS, indexA, indexB, false)
}

func acceleratedLumaEdge(plane []byte, offset, stride int, bS [4]uint8, indexA, indexB int) bool {
	alpha := int(alphaTable[indexA])
	beta := int(betaTable[indexB])
	if alpha == 0 || beta == 0 {
		return true
	}
	tc0, bs, kind := edgeStrengths(bS, indexA)
	switch kind {
	case edgeIdle:
		return true
	case edgeStrong:
		return simd.DeblockLumaStrong(plane, offset, stride, alpha, beta)
	case edgeNormal:
		return simd.DeblockLumaNormal(plane, offset, stride, &tc0, &bs, alpha, beta)
	}
	return false
}

func FilterChromaEdgeVertical(plane []byte, stride, offset int, bS [4]uint8, indexA, indexB int) {
	filterEdge(plane, offset, stride, 1, 8, 2, bS, indexA, indexB, true)
}

func FilterChromaEdgeHorizontal(plane []byte, stride, offset int, bS [4]uint8, indexA, indexB int) {
	filterEdge(plane, offset, 1, stride, 8, 2, bS, indexA, indexB, true)
}
