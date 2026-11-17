package cabac

const (
	offMBSkipP          = 11
	offMBSkipB          = 24
	offMBTypeB          = 27
	offMBTypeIinB       = 32
	offSubMBTypeB       = 36
	offMBTypeP          = 14
	offMBTypeIinP       = 17
	offSubMBTypeP       = 21
	offMVD              = 40
	offRefIdx           = 54
	offMBTypeI          = 3
	offMBQPDelta        = 60
	offIntraChromaPred  = 64
	offPrevIntraPred    = 68
	offRemIntraPred     = 69
	offCodedBlockPatLum = 73
	offCodedBlockPatChr = 77
)

const (
	CBPUnavailable = 0x0F
	CBPPCM         = 0x2F
)

const (
	MVDHorizontal = offMVD
	MVDVertical   = offMVD + 7
)

func (d *Decoder) intraMBTypeSuffix(base, spread int) int {
	if d.DecodeTerminate() == 1 {
		return 25
	}
	mbType := 1
	if d.DecodeDecision(base+1) == 1 {
		mbType += 12
	}
	if d.DecodeDecision(base+2) == 1 {
		mbType += 4 + 4*int(d.DecodeDecision(base+2+spread))
	}
	mbType += 2 * int(d.DecodeDecision(base+3+spread))
	mbType += int(d.DecodeDecision(base + 3 + 2*spread))
	return mbType
}

func (d *Decoder) IntraMBType(inc int) int {
	if d.DecodeDecision(offMBTypeI+inc) == 0 {
		return 0
	}
	return d.intraMBTypeSuffix(offMBTypeI+2, 1)
}

func (d *Decoder) MBSkipFlagP(inc int) bool {
	return d.DecodeDecision(offMBSkipP+inc) == 1
}

func (d *Decoder) MBSkipFlagB(inc int) bool {
	return d.DecodeDecision(offMBSkipB+inc) == 1
}

func (d *Decoder) intraMBTypeIn(base int) int {
	if d.DecodeDecision(base) == 0 {
		return 0
	}
	return d.intraMBTypeSuffix(base, 0)
}

func (d *Decoder) MBTypeB(inc int) (mbType int, intra bool) {
	if d.DecodeDecision(offMBTypeB+inc) == 0 {
		return 0, false
	}
	if d.DecodeDecision(offMBTypeB+3) == 0 {
		return 1 + int(d.DecodeDecision(offMBTypeB+5)), false
	}
	bits := int(d.DecodeDecision(offMBTypeB+4)) << 3
	bits += int(d.DecodeDecision(offMBTypeB+5)) << 2
	bits += int(d.DecodeDecision(offMBTypeB+5)) << 1
	bits += int(d.DecodeDecision(offMBTypeB + 5))
	switch {
	case bits < 8:
		return bits + 3, false
	case bits == 13:
		return d.intraMBTypeIn(offMBTypeIinB), true
	case bits == 14:
		return 11, false
	case bits == 15:
		return 22, false
	}
	bits = bits<<1 + int(d.DecodeDecision(offMBTypeB+5))
	return bits - 4, false
}

func (d *Decoder) SubMBTypeB() int {
	if d.DecodeDecision(offSubMBTypeB) == 0 {
		return 0
	}
	if d.DecodeDecision(offSubMBTypeB+1) == 0 {
		return 1 + int(d.DecodeDecision(offSubMBTypeB+3))
	}
	t := 3
	if d.DecodeDecision(offSubMBTypeB+2) == 1 {
		if d.DecodeDecision(offSubMBTypeB+3) == 1 {
			return 11 + int(d.DecodeDecision(offSubMBTypeB+3))
		}
		t += 4
	}
	t += 2 * int(d.DecodeDecision(offSubMBTypeB+3))
	t += int(d.DecodeDecision(offSubMBTypeB + 3))
	return t
}

func (d *Decoder) MBTypeP() (mbType int, intra bool) {
	if d.DecodeDecision(offMBTypeP) == 1 {
		return d.intraMBTypeIn(offMBTypeIinP), true
	}
	if d.DecodeDecision(offMBTypeP+1) == 0 {
		return 3 * int(d.DecodeDecision(offMBTypeP+2)), false
	}
	return 2 - int(d.DecodeDecision(offMBTypeP+3)), false
}

func (d *Decoder) SubMBTypeP() int {
	if d.DecodeDecision(offSubMBTypeP) == 1 {
		return 0
	}
	if d.DecodeDecision(offSubMBTypeP+1) == 0 {
		return 1
	}
	if d.DecodeDecision(offSubMBTypeP+2) == 1 {
		return 2
	}
	return 3
}

func (d *Decoder) RefIdx(inc int) int {
	ref := 0
	ctx := inc
	for d.DecodeDecision(offRefIdx+ctx) == 1 {
		ref++
		ctx = ctx>>2 + 4
		if ref >= 32 {
			break
		}
	}
	return ref
}

func (d *Decoder) MVD(base, absSum int) int {
	inc := 0
	if absSum > 2 {
		inc++
	}
	if absSum > 32 {
		inc++
	}
	if d.DecodeDecision(base+inc) == 0 {
		return 0
	}
	v := 1
	ctx := base + 3
	for v < 9 && d.DecodeDecision(ctx) == 1 {
		if v < 4 {
			ctx++
		}
		v++
	}
	if v >= 9 {
		k := 3
		for d.DecodeBypass() == 1 {
			v += 1 << uint(k)
			k++
			if k > 24 {
				break
			}
		}
		for k--; k >= 0; k-- {
			v += int(d.DecodeBypass()) << uint(k)
		}
	}
	if d.DecodeBypass() == 1 {
		return -v
	}
	return v
}

func (d *Decoder) IntraChromaPredMode(inc int) int {
	if d.DecodeDecision(offIntraChromaPred+inc) == 0 {
		return 0
	}
	if d.DecodeDecision(offIntraChromaPred+3) == 0 {
		return 1
	}
	if d.DecodeDecision(offIntraChromaPred+3) == 0 {
		return 2
	}
	return 3
}

func (d *Decoder) Intra4x4PredMode(predMode int) int {
	if d.DecodeDecision(offPrevIntraPred) == 1 {
		return predMode
	}
	mode := int(d.DecodeDecision(offRemIntraPred))
	mode += 2 * int(d.DecodeDecision(offRemIntraPred))
	mode += 4 * int(d.DecodeDecision(offRemIntraPred))
	if mode >= predMode {
		mode++
	}
	return mode
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *Decoder) CodedBlockPatternLuma(leftCBP, topCBP int) int {
	cbp := 0
	ctx := boolToInt(leftCBP&0x02 == 0) + 2*boolToInt(topCBP&0x04 == 0)
	cbp |= int(d.DecodeDecision(offCodedBlockPatLum + ctx))
	ctx = boolToInt(cbp&0x01 == 0) + 2*boolToInt(topCBP&0x08 == 0)
	cbp |= int(d.DecodeDecision(offCodedBlockPatLum+ctx)) << 1
	ctx = boolToInt(leftCBP&0x08 == 0) + 2*boolToInt(cbp&0x01 == 0)
	cbp |= int(d.DecodeDecision(offCodedBlockPatLum+ctx)) << 2
	ctx = boolToInt(cbp&0x04 == 0) + 2*boolToInt(cbp&0x02 == 0)
	cbp |= int(d.DecodeDecision(offCodedBlockPatLum+ctx)) << 3
	return cbp
}

func (d *Decoder) CodedBlockPatternChroma(leftCBP, topCBP int) int {
	a := leftCBP >> 4 & 3
	b := topCBP >> 4 & 3
	ctx := 0
	if a > 0 {
		ctx++
	}
	if b > 0 {
		ctx += 2
	}
	if d.DecodeDecision(offCodedBlockPatChr+ctx) == 0 {
		return 0
	}
	ctx = 4
	if a == 2 {
		ctx++
	}
	if b == 2 {
		ctx += 2
	}
	return 1 + int(d.DecodeDecision(offCodedBlockPatChr+ctx))
}

func (d *Decoder) MBQPDelta(prevNonZero bool) int {
	if d.DecodeDecision(offMBQPDelta+boolToInt(prevNonZero)) == 0 {
		return 0
	}
	val := 1
	ctx := 2
	for d.DecodeDecision(offMBQPDelta+ctx) == 1 {
		ctx = 3
		val++
		if val > 102 {
			break
		}
	}
	if val&1 == 1 {
		return (val + 1) >> 1
	}
	return -((val + 1) >> 1)
}

func (d *Decoder) EndOfSlice() bool { return d.DecodeTerminate() == 1 }
