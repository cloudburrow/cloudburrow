package vp9hbd

import "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/tables"

// probs is one frame context: every adaptive probability table (spec 10.5).
type probs struct {
	tx8          [2][1]uint8
	tx16         [2][2]uint8
	tx32         [2][3]uint8
	skip         [3]uint8
	coef         [4][2][2][6][6][3]uint8
	interMode    [7][3]uint8
	interpFilter [4][2]uint8
	isInter      [4]uint8
	compMode     [5]uint8
	singleRef    [5][2]uint8
	compRef      [5]uint8
	yMode        [4][9]uint8
	uvMode       [10][9]uint8
	partition    [16][3]uint8
	mvJoint      [3]uint8
	mvSign       [2]uint8
	mvClass      [2][10]uint8
	mvClass0Bit  [2]uint8
	mvBits       [2][10]uint8
	mvClass0Fr   [2][2][3]uint8
	mvFr         [2][3]uint8
	mvClass0Hp   [2]uint8
	mvHp         [2]uint8
}

func defaultProbs() probs {
	var p probs
	p.tx8 = tables.DefaultTxProbsP8x8
	p.tx16 = tables.DefaultTxProbsP16x16
	p.tx32 = tables.DefaultTxProbsP32x32
	p.skip = tables.DefaultSkipProbs
	p.coef[0] = tables.DefaultCoefProbs4x4
	p.coef[1] = tables.DefaultCoefProbs8x8
	p.coef[2] = tables.DefaultCoefProbs16x16
	p.coef[3] = tables.DefaultCoefProbs32x32
	p.interMode = tables.DefaultInterModeProbs
	p.interpFilter = tables.DefaultSwitchableInterpProb
	p.isInter = tables.DefaultIntraInter
	p.compMode = tables.DefaultCompInter
	p.singleRef = tables.DefaultSingleRef
	p.compRef = tables.DefaultCompRef
	p.yMode = tables.DefaultIfYProbs
	p.uvMode = tables.DefaultIfUvProbs
	p.partition = tables.DefaultPartitionProbs
	p.mvJoint = tables.DefaultNmvJoints
	for i := range 2 {
		c := tables.DefaultNmvComps[i]
		p.mvSign[i] = c.Sign
		p.mvClass[i] = c.Classes
		p.mvClass0Bit[i] = c.Class0[0]
		p.mvBits[i] = c.Bits
		p.mvClass0Fr[i] = c.Class0Fp
		p.mvFr[i] = c.Fp
		p.mvClass0Hp[i] = c.Class0Hp
		p.mvHp[i] = c.Hp
	}
	return p
}

// counts are the symbol counts of a frame (spec 8.3).
type counts struct {
	intraMode    [4][10]int
	uvMode       [10][10]int
	partition    [16][4]int
	interpFilter [4][3]int
	interMode    [7][4]int
	tx8          [2][2]int
	tx16         [2][3]int
	tx32         [2][4]int
	isInter      [4][2]int
	compMode     [5][2]int
	singleRef    [5][2][2]int
	compRef      [5][2]int
	skip         [3][2]int
	mvJoint      [4]int
	mvSign       [2][2]int
	mvClass      [2][11]int
	mvClass0Bit  [2][2]int
	mvClass0Fr   [2][2][4]int
	mvClass0Hp   [2][2]int
	mvBits       [2][10][2]int
	mvFr         [2][4]int
	mvHp         [2][2]int
	token        [4][2][2][6][6][3]int
	moreCoefs    [4][2][2][6][6][2]int
}

// Trees (spec 9.3.1); leaves are negated values.
var (
	partitionTree    = []int8{0, 2, -1, 4, -2, -3}
	intraModeTree    = []int8{-dcPred, 2, -tmPred, 4, -vPred, 6, 8, 12, -hPred, 10, -d135Pred, -d117Pred, -d45Pred, 14, -d63Pred, 16, -d153Pred, -d207Pred}
	segmentTree      = []int8{2, 4, 6, 8, 10, 12, 0, -1, -2, -3, -4, -5, -6, -7}
	tx32Tree         = []int8{0, 2, -1, 4, -2, -3}
	tx16Tree         = []int8{0, 2, -1, -2}
	tx8Tree          = []int8{0, -1}
	interModeTree    = []int8{-(zeroMv - nearestMv), 2, -(nearestMv - nearestMv), 4, -(nearMv - nearestMv), -(newMv - nearestMv)}
	interpFilterTree = []int8{0, 2, -1, -2}
	mvJointTree      = []int8{0, 2, -1, 4, -2, -3}
	mvClassTree      = []int8{0, 2, -1, 4, 6, 8, -2, -3, 10, 12, -4, -5, -6, 14, 16, 18, -7, -8, -9, -10}
	mvFrTree         = []int8{0, 2, -1, 4, -2, -3}
	smallTokenTree   = []int8{0, 0, 0, 4, -1, -2}
	binaryTree       = []int8{0, -1}
)

// mergeProb is spec 8.4.1.
func mergeProb(pre uint8, ct0, ct1, countSat, maxUpdateFactor int) uint8 {
	den := ct0 + ct1
	prob := 128
	if den != 0 {
		prob = min(255, max(1, (ct0*256+(den>>1))/den))
	}
	count := min(den, countSat)
	factor := maxUpdateFactor * count / countSat
	return uint8((int(pre)*(256-factor) + prob*factor + 128) >> 8)
}

// mergeProbs is spec 8.4.2.
func mergeProbs(tree []int8, i int, p []uint8, c []int, countSat, maxUpdateFactor int) int {
	s := int(tree[i])
	var left, right int
	if s <= 0 {
		left = c[-s]
	} else {
		left = mergeProbs(tree, s, p, c, countSat, maxUpdateFactor)
	}
	r := int(tree[i+1])
	if r <= 0 {
		right = c[-r]
	} else {
		right = mergeProbs(tree, r, p, c, countSat, maxUpdateFactor)
	}
	p[i>>1] = mergeProb(p[i>>1], left, right, countSat, maxUpdateFactor)
	return left + right
}

const (
	countSat        = 20
	maxUpdateFactor = 128
)

func adaptProbs(tree []int8, p []uint8, c []int) {
	mergeProbs(tree, 0, p, c, countSat, maxUpdateFactor)
}

func adaptProb(p uint8, c [2]int) uint8 {
	return mergeProb(p, c[0], c[1], countSat, maxUpdateFactor)
}

// adaptCoefProbs is spec 8.4.3.
func (d *Decoder) adaptCoefProbs() {
	f := &d.fc
	cnt := &d.counts
	uf := 112
	if !d.h.frameIsIntra && d.h.lastFrameType == keyFrame {
		uf = 128
	}
	for t := range 4 {
		for i := range 2 {
			for j := range 2 {
				for k := range 6 {
					maxL := 6
					if k == 0 {
						maxL = 3
					}
					for l := range maxL {
						p := f.coef[t][i][j][k][l][:]
						mergeProbs(smallTokenTree, 2, p, cnt.token[t][i][j][k][l][:], 24, uf)
						mergeProbs(binaryTree, 0, p, cnt.moreCoefs[t][i][j][k][l][:], 24, uf)
					}
				}
			}
		}
	}
}

// adaptNoncoefProbs is spec 8.4.4.
func (d *Decoder) adaptNoncoefProbs() {
	f := &d.fc
	c := &d.counts
	for i := range 4 {
		f.isInter[i] = adaptProb(f.isInter[i], c.isInter[i])
	}
	for i := range 5 {
		f.compMode[i] = adaptProb(f.compMode[i], c.compMode[i])
		f.compRef[i] = adaptProb(f.compRef[i], c.compRef[i])
		for j := range 2 {
			f.singleRef[i][j] = adaptProb(f.singleRef[i][j], c.singleRef[i][j])
		}
	}
	for i := range 7 {
		adaptProbs(interModeTree, f.interMode[i][:], c.interMode[i][:])
	}
	for i := range 4 {
		adaptProbs(intraModeTree, f.yMode[i][:], c.intraMode[i][:])
	}
	for i := range 10 {
		adaptProbs(intraModeTree, f.uvMode[i][:], c.uvMode[i][:])
	}
	for i := range 16 {
		adaptProbs(partitionTree, f.partition[i][:], c.partition[i][:])
	}
	for i := range 3 {
		f.skip[i] = adaptProb(f.skip[i], c.skip[i])
	}
	if d.h.interpFilter == switchable {
		for i := range 4 {
			adaptProbs(interpFilterTree, f.interpFilter[i][:], c.interpFilter[i][:])
		}
	}
	if d.h.txMode == txModeSelect {
		for i := range 2 {
			adaptProbs(tx8Tree, f.tx8[i][:], c.tx8[i][:])
			adaptProbs(tx16Tree, f.tx16[i][:], c.tx16[i][:])
			adaptProbs(tx32Tree, f.tx32[i][:], c.tx32[i][:])
		}
	}
	adaptProbs(mvJointTree, f.mvJoint[:], c.mvJoint[:])
	for i := range 2 {
		f.mvSign[i] = adaptProb(f.mvSign[i], c.mvSign[i])
		adaptProbs(mvClassTree, f.mvClass[i][:], c.mvClass[i][:])
		f.mvClass0Bit[i] = adaptProb(f.mvClass0Bit[i], c.mvClass0Bit[i])
		for j := range 10 {
			f.mvBits[i][j] = adaptProb(f.mvBits[i][j], c.mvBits[i][j])
		}
		for j := range 2 {
			adaptProbs(mvFrTree, f.mvClass0Fr[i][j][:], c.mvClass0Fr[i][j][:])
		}
		adaptProbs(mvFrTree, f.mvFr[i][:], c.mvFr[i][:])
		if d.h.allowHighPrecisionMv {
			f.mvClass0Hp[i] = adaptProb(f.mvClass0Hp[i], c.mvClass0Hp[i])
			f.mvHp[i] = adaptProb(f.mvHp[i], c.mvHp[i])
		}
	}
}
