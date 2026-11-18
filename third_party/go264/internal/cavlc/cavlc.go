package cavlc

import (
	"errors"
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

var (
	ErrInvalidCode  = errors.New("go264/cavlc: no matching variable length code")
	ErrTooManyCoeff = errors.New("go264/cavlc: coefficient count exceeds block capacity")
	ErrTotalZeros   = errors.New("go264/cavlc: total_zeros out of range")
	ErrLevelRange   = errors.New("go264/cavlc: coefficient level out of range")
)

const maxLevelPrefix = 30

const vlcAbsent = 0xFFFF

type vlc struct {
	lookup []uint16
	maxLen int
}

func newVLC() *vlc { return &vlc{} }

func (v *vlc) grow(n int) {
	if n <= len(v.lookup) {
		return
	}
	grown := make([]uint16, n)
	for i := range grown {
		grown[i] = vlcAbsent
	}
	copy(grown, v.lookup)
	v.lookup = grown
}

func (v *vlc) add(pattern string, value uint16) {
	if pattern == "" {
		return
	}
	if value == vlcAbsent {
		panic(fmt.Sprintf("go264/cavlc: code %q carries the absent marker", pattern))
	}
	key := uint32(1)
	for _, c := range pattern {
		key <<= 1
		if c == '1' {
			key |= 1
		}
	}
	v.grow(int(key) + 1)
	if v.lookup[key] != vlcAbsent {
		panic(fmt.Sprintf("go264/cavlc: duplicate code %q", pattern))
	}
	v.lookup[key] = value
	if len(pattern) > v.maxLen {
		v.maxLen = len(pattern)
	}
}

func (v *vlc) read(r *bits.Reader) (uint16, error) {
	key := uint32(1)
	for i := 0; i < v.maxLen; i++ {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		key = key<<1 | b
		if int(key) < len(v.lookup) {
			if val := v.lookup[key]; val != vlcAbsent {
				return val, nil
			}
		}
	}
	return 0, ErrInvalidCode
}

type encoding struct {
	bits   uint32
	length uint8
	valid  bool
}

func encodingOf(pattern string) encoding {
	if pattern == "" {
		return encoding{}
	}
	var b uint32
	for _, c := range pattern {
		b <<= 1
		if c == '1' {
			b |= 1
		}
	}
	return encoding{bits: b, length: uint8(len(pattern)), valid: true}
}

var (
	coeffTokenVLC [5]*vlc
	coeffTokenEnc [5][4][17]encoding

	totalZerosVLC [16]*vlc
	totalZerosEnc [16][]encoding

	chromaDCTotalZerosVLC [4]*vlc
	chromaDCTotalZerosEnc [4][]encoding

	runBeforeVLC [8]*vlc
	runBeforeEnc [8][]encoding
)

const chromaDCTable = 4

func tokenValue(trailingOnes, totalCoeff int) uint16 {
	return uint16(trailingOnes)<<8 | uint16(totalCoeff)
}

func init() {
	sources := [4]*[4][17]string{&coeffTokenNC0, &coeffTokenNC2, &coeffTokenNC4, nil}
	for tbl := 0; tbl < 3; tbl++ {
		v := newVLC()
		src := sources[tbl]
		for t1 := 0; t1 < 4; t1++ {
			for tc := 0; tc < 17; tc++ {
				pattern := src[t1][tc]
				v.add(pattern, tokenValue(t1, tc))
				coeffTokenEnc[tbl][t1][tc] = encodingOf(pattern)
			}
		}
		coeffTokenVLC[tbl] = v
	}

	v := newVLC()
	for t1 := 0; t1 < 4; t1++ {
		for tc := 0; tc < 17; tc++ {
			if tc == 0 && t1 == 0 {
				coeffTokenEnc[3][0][0] = encoding{bits: 3, length: 6, valid: true}
				continue
			}
			if tc == 0 || t1 > tc {
				continue
			}
			coeffTokenEnc[3][t1][tc] = encoding{bits: uint32((tc-1)<<2 | t1), length: 6, valid: true}
		}
	}
	coeffTokenVLC[3] = v

	v = newVLC()
	for t1 := 0; t1 < 4; t1++ {
		for tc := 0; tc < 5; tc++ {
			pattern := coeffTokenChromaDC420[t1][tc]
			v.add(pattern, tokenValue(t1, tc))
			coeffTokenEnc[chromaDCTable][t1][tc] = encodingOf(pattern)
		}
	}
	coeffTokenVLC[chromaDCTable] = v

	for i := 1; i < 16; i++ {
		v := newVLC()
		totalZerosEnc[i] = make([]encoding, len(totalZeros4x4[i]))
		for tz, pattern := range totalZeros4x4[i] {
			v.add(pattern, uint16(tz))
			totalZerosEnc[i][tz] = encodingOf(pattern)
		}
		totalZerosVLC[i] = v
	}

	for i := 1; i < 4; i++ {
		v := newVLC()
		chromaDCTotalZerosEnc[i] = make([]encoding, len(totalZerosChromaDC420[i]))
		for tz, pattern := range totalZerosChromaDC420[i] {
			v.add(pattern, uint16(tz))
			chromaDCTotalZerosEnc[i][tz] = encodingOf(pattern)
		}
		chromaDCTotalZerosVLC[i] = v
	}

	for i := 1; i < 8; i++ {
		v := newVLC()
		runBeforeEnc[i] = make([]encoding, len(runBefore[i]))
		for run, pattern := range runBefore[i] {
			v.add(pattern, uint16(run))
			runBeforeEnc[i][run] = encodingOf(pattern)
		}
		runBeforeVLC[i] = v
	}
}

func coeffTokenTable(nC int) int {
	switch {
	case nC < 0:
		return chromaDCTable
	case nC < 2:
		return 0
	case nC < 4:
		return 1
	case nC < 8:
		return 2
	default:
		return 3
	}
}

func readCoeffToken(r *bits.Reader, nC int) (trailingOnes, totalCoeff int, err error) {
	tbl := coeffTokenTable(nC)
	if tbl == 3 {
		v, err := r.ReadBits(6)
		if err != nil {
			return 0, 0, err
		}
		if v == 3 {
			return 0, 0, nil
		}
		tc := int(v>>2) + 1
		t1 := int(v & 3)
		if t1 > tc || tc > 16 {
			return 0, 0, ErrInvalidCode
		}
		return t1, tc, nil
	}
	val, err := coeffTokenVLC[tbl].read(r)
	if err != nil {
		return 0, 0, err
	}
	return int(val >> 8), int(val & 0xFF), nil
}

func writeCoeffToken(w *bits.Writer, nC, trailingOnes, totalCoeff int) error {
	tbl := coeffTokenTable(nC)
	e := coeffTokenEnc[tbl][trailingOnes][totalCoeff]
	if !e.valid {
		return fmt.Errorf("%w: coeff_token nC=%d T1s=%d total=%d", ErrInvalidCode, nC, trailingOnes, totalCoeff)
	}
	w.WriteBits(e.bits, int(e.length))
	return nil
}

func ReadBlock(r *bits.Reader, coeffs []int32, nC int) (int, error) {
	maxCoeff := len(coeffs)
	for i := range coeffs {
		coeffs[i] = 0
	}
	trailingOnes, totalCoeff, err := readCoeffToken(r, nC)
	if err != nil {
		return 0, err
	}
	if totalCoeff > maxCoeff {
		return 0, ErrTooManyCoeff
	}
	if totalCoeff == 0 {
		return 0, nil
	}

	var levels [16]int32
	suffixLength := 0
	if totalCoeff > 10 && trailingOnes < 3 {
		suffixLength = 1
	}
	for i := 0; i < totalCoeff; i++ {
		if i < trailingOnes {
			b, err := r.ReadBit()
			if err != nil {
				return 0, err
			}
			levels[i] = 1 - 2*int32(b)
			continue
		}
		prefix := 0
		for {
			b, err := r.ReadBit()
			if err != nil {
				return 0, err
			}
			if b == 1 {
				break
			}
			prefix++
			if prefix > maxLevelPrefix {
				return 0, ErrInvalidCode
			}
		}
		suffixSize := suffixLength
		if prefix == 14 && suffixLength == 0 {
			suffixSize = 4
		} else if prefix >= 15 {
			suffixSize = prefix - 3
		}
		var suffix uint32
		if suffixSize > 0 {
			if suffixSize > 32 {
				return 0, ErrLevelRange
			}
			if suffix, err = r.ReadBits(suffixSize); err != nil {
				return 0, err
			}
		}
		levelCode := int64(min(15, prefix))<<uint(suffixLength) + int64(suffix)
		if prefix >= 15 && suffixLength == 0 {
			levelCode += 15
		}
		if prefix >= 16 {
			levelCode += int64(1)<<uint(prefix-3) - 4096
		}
		if i == trailingOnes && trailingOnes < 3 {
			levelCode += 2
		}
		var level int64
		if levelCode%2 == 0 {
			level = (levelCode + 2) >> 1
		} else {
			level = (-levelCode - 1) >> 1
		}
		if level < -32768 || level > 32767 {
			return 0, ErrLevelRange
		}
		levels[i] = int32(level)
		if suffixLength == 0 {
			suffixLength = 1
		}
		abs := level
		if abs < 0 {
			abs = -abs
		}
		if abs > int64(3)<<uint(suffixLength-1) && suffixLength < 6 {
			suffixLength++
		}
	}

	zerosLeft := 0
	if totalCoeff < maxCoeff {
		v, err := readTotalZeros(r, totalCoeff, maxCoeff)
		if err != nil {
			return 0, err
		}
		zerosLeft = v
		if zerosLeft > maxCoeff-totalCoeff {
			return 0, ErrTotalZeros
		}
	}

	var runs [16]int
	for i := 0; i < totalCoeff-1; i++ {
		if zerosLeft <= 0 {
			break
		}
		run, err := readRunBefore(r, zerosLeft)
		if err != nil {
			return 0, err
		}
		if run > zerosLeft {
			return 0, ErrTotalZeros
		}
		runs[i] = run
		zerosLeft -= run
	}
	runs[totalCoeff-1] = zerosLeft

	pos := -1
	for i := totalCoeff - 1; i >= 0; i-- {
		pos += runs[i] + 1
		if pos >= maxCoeff {
			return 0, ErrTooManyCoeff
		}
		coeffs[pos] = levels[i]
	}
	return totalCoeff, nil
}

func readTotalZeros(r *bits.Reader, totalCoeff, maxCoeff int) (int, error) {
	if maxCoeff == 4 {
		if totalCoeff < 1 || totalCoeff > 3 {
			return 0, ErrTotalZeros
		}
		v, err := chromaDCTotalZerosVLC[totalCoeff].read(r)
		return int(v), err
	}
	if totalCoeff < 1 || totalCoeff > 15 {
		return 0, ErrTotalZeros
	}
	v, err := totalZerosVLC[totalCoeff].read(r)
	return int(v), err
}

func readRunBefore(r *bits.Reader, zerosLeft int) (int, error) {
	idx := zerosLeft
	if idx > 7 {
		idx = 7
	}
	v, err := runBeforeVLC[idx].read(r)
	return int(v), err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
