package cabac

const CatLuma8x8 = 5

const (
	offSignificant8x8     = 402
	offLastSignificant8x8 = 417
	offAbsLevel8x8        = 426
)

var significantCoeffFlagOffset8x8 = [63]uint8{
	0, 1, 2, 3, 4, 5, 5, 4,
	4, 3, 3, 4, 4, 4, 5, 5,
	4, 4, 4, 4, 3, 3, 6, 7,
	7, 7, 8, 9, 10, 9, 8, 7,
	7, 6, 11, 12, 13, 11, 6, 7,
	8, 9, 14, 10, 9, 8, 6, 11,
	12, 13, 11, 6, 9, 14, 10, 9,
	11, 12, 13, 11, 14, 10, 12,
}

var lastCoeffFlagOffset8x8 = [63]uint8{
	0, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	2, 2, 2, 2, 2, 2, 2, 2,
	2, 2, 2, 2, 2, 2, 2, 2,
	3, 3, 3, 3, 3, 3, 3, 3,
	4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 6, 6, 6, 6,
	7, 7, 7, 7, 8, 8, 8,
}

const offTransformSize8x8 = 399

func (d *Decoder) TransformSize8x8Flag(inc int) bool {
	return d.DecodeDecision(offTransformSize8x8+inc) == 1
}

func (d *Decoder) ResidualBlock8x8(coeffs *[64]int32) int {
	for i := range coeffs {
		coeffs[i] = 0
	}
	var significant [64]bool
	numCoeff := 64
	for i := 0; i < numCoeff-1; i++ {
		if d.DecodeDecision(offSignificant8x8+int(significantCoeffFlagOffset8x8[i])) == 1 {
			significant[i] = true
			if d.DecodeDecision(offLastSignificant8x8+int(lastCoeffFlagOffset8x8[i])) == 1 {
				numCoeff = i + 1
			}
		}
	}
	significant[numCoeff-1] = true

	numGt1, numEq1 := 0, 0
	total := 0
	for i := numCoeff - 1; i >= 0; i-- {
		if !significant[i] {
			continue
		}
		level := d.boundedLevel(d.absLevelMinus1At(offAbsLevel8x8, 4, numGt1, numEq1) + 1)
		if level > 1 {
			numGt1++
		} else {
			numEq1++
		}
		if d.DecodeBypass() == 1 {
			coeffs[i] = int32(-level)
		} else {
			coeffs[i] = int32(level)
		}
		total++
	}
	return total
}

func (e *Encoder) absLevelMinus1At(base, limit, v, numGt1, numEq1 int) {
	prefix := v
	if prefix > 14 {
		prefix = 14
	}
	for i := 0; i < prefix; i++ {
		e.EncodeDecision(absLevelIncAt(base, limit, i, numGt1, numEq1), 1)
	}
	if prefix < 14 {
		e.EncodeDecision(absLevelIncAt(base, limit, prefix, numGt1, numEq1), 0)
		return
	}
	rest := v - 14
	k := 0
	for rest >= 1<<uint(k) {
		e.EncodeBypass(1)
		rest -= 1 << uint(k)
		k++
		if k > 24 {
			break
		}
	}
	e.EncodeBypass(0)
	for k--; k >= 0; k-- {
		e.EncodeBypass(uint32(rest>>uint(k)) & 1)
	}
}

func (e *Encoder) TransformSize8x8Flag(inc int, flag bool) {
	e.EncodeDecision(offTransformSize8x8+inc, boolToUint32(flag))
}

func (e *Encoder) ResidualBlock8x8(coeffs *[64]int32) {
	numCoeff := 0
	for i := 0; i < 64; i++ {
		if coeffs[i] != 0 {
			numCoeff = i + 1
		}
	}
	if numCoeff == 0 {
		return
	}
	for i := 0; i < numCoeff-1; i++ {
		significant := coeffs[i] != 0
		e.EncodeDecision(offSignificant8x8+int(significantCoeffFlagOffset8x8[i]), boolToUint32(significant))
		if significant {
			e.EncodeDecision(offLastSignificant8x8+int(lastCoeffFlagOffset8x8[i]), 0)
		}
	}
	if numCoeff < 64 {
		e.EncodeDecision(offSignificant8x8+int(significantCoeffFlagOffset8x8[numCoeff-1]), 1)
		e.EncodeDecision(offLastSignificant8x8+int(lastCoeffFlagOffset8x8[numCoeff-1]), 1)
	}

	numGt1, numEq1 := 0, 0
	for i := numCoeff - 1; i >= 0; i-- {
		v := coeffs[i]
		if v == 0 {
			continue
		}
		level := v
		sign := uint32(0)
		if level < 0 {
			level = -level
			sign = 1
		}
		e.absLevelMinus1At(offAbsLevel8x8, 4, int(level)-1, numGt1, numEq1)
		if level > 1 {
			numGt1++
		} else {
			numEq1++
		}
		e.EncodeBypass(sign)
	}
}
