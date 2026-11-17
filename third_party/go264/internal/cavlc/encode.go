package cavlc

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

func writeTotalZeros(w *bits.Writer, totalCoeff, totalZeros, maxCoeff int) error {
	var e encoding
	if maxCoeff == 4 {
		if totalCoeff < 1 || totalCoeff > 3 || totalZeros >= len(chromaDCTotalZerosEnc[totalCoeff]) {
			return fmt.Errorf("%w: chroma total_zeros %d for %d coefficients", ErrTotalZeros, totalZeros, totalCoeff)
		}
		e = chromaDCTotalZerosEnc[totalCoeff][totalZeros]
	} else {
		if totalCoeff < 1 || totalCoeff > 15 || totalZeros >= len(totalZerosEnc[totalCoeff]) {
			return fmt.Errorf("%w: total_zeros %d for %d coefficients", ErrTotalZeros, totalZeros, totalCoeff)
		}
		e = totalZerosEnc[totalCoeff][totalZeros]
	}
	if !e.valid {
		return ErrTotalZeros
	}
	w.WriteBits(e.bits, int(e.length))
	return nil
}

func writeRunBefore(w *bits.Writer, run, zerosLeft int) error {
	idx := zerosLeft
	if idx > 7 {
		idx = 7
	}
	if idx < 1 || run >= len(runBeforeEnc[idx]) {
		return fmt.Errorf("%w: run_before %d with %d zeros left", ErrInvalidCode, run, zerosLeft)
	}
	e := runBeforeEnc[idx][run]
	if !e.valid {
		return ErrInvalidCode
	}
	w.WriteBits(e.bits, int(e.length))
	return nil
}

func escapedLevelBase(prefix, suffixLength int) int {
	base := 15 << uint(suffixLength)
	if suffixLength == 0 {
		base += 15
	}
	if prefix >= 16 {
		base += 1<<uint(prefix-3) - 4096
	}
	return base
}

func writeEscapedLevel(w *bits.Writer, levelCode, suffixLength int) error {
	for prefix := 15; prefix <= maxLevelPrefix; prefix++ {
		suffixSize := prefix - 3
		if suffixSize > 30 {
			break
		}
		rest := levelCode - escapedLevelBase(prefix, suffixLength)
		if rest >= 0 && rest < 1<<uint(suffixSize) {
			w.WriteBits(1, prefix+1)
			writeWideBits(w, uint32(rest), suffixSize)
			return nil
		}
	}
	return ErrLevelRange
}

func writeWideBits(w *bits.Writer, v uint32, n int) {
	for n > 16 {
		n -= 16
		w.WriteBits(v>>uint(n)&0xFFFF, 16)
	}
	if n > 0 {
		w.WriteBits(v&(1<<uint(n)-1), n)
	}
}

func writeLevel(w *bits.Writer, levelCode, suffixLength int) error {
	if levelCode < 0 {
		return ErrLevelRange
	}
	if suffixLength == 0 {
		switch {
		case levelCode < 14:
			w.WriteBits(1, levelCode+1)
			return nil
		case levelCode < 30:
			w.WriteBits(1, 15)
			w.WriteBits(uint32(levelCode-14), 4)
			return nil
		}
		return writeEscapedLevel(w, levelCode, suffixLength)
	}
	prefix := levelCode >> uint(suffixLength)
	if prefix < 15 {
		w.WriteBits(1, prefix+1)
		w.WriteBits(uint32(levelCode)&(1<<uint(suffixLength)-1), suffixLength)
		return nil
	}
	return writeEscapedLevel(w, levelCode, suffixLength)
}

func WriteBlock(w *bits.Writer, coeffs []int32, nC int) error {
	maxCoeff := len(coeffs)
	var positions [16]int
	totalCoeff := 0
	for i, v := range coeffs {
		if v != 0 {
			if totalCoeff == 16 {
				return ErrTooManyCoeff
			}
			positions[totalCoeff] = i
			totalCoeff++
		}
	}

	trailingOnes := 0
	for i := totalCoeff - 1; i >= 0 && trailingOnes < 3; i-- {
		v := coeffs[positions[i]]
		if v != 1 && v != -1 {
			break
		}
		trailingOnes++
	}

	if err := writeCoeffToken(w, nC, trailingOnes, totalCoeff); err != nil {
		return err
	}
	if totalCoeff == 0 {
		return w.Err()
	}

	suffixLength := 0
	if totalCoeff > 10 && trailingOnes < 3 {
		suffixLength = 1
	}
	for i := 0; i < totalCoeff; i++ {
		level := int(coeffs[positions[totalCoeff-1-i]])
		if i < trailingOnes {
			if level > 0 {
				w.WriteBit(0)
			} else {
				w.WriteBit(1)
			}
			continue
		}
		var levelCode int
		if level > 0 {
			levelCode = 2*level - 2
		} else {
			levelCode = -2*level - 1
		}
		if i == trailingOnes && trailingOnes < 3 {
			levelCode -= 2
		}
		if err := writeLevel(w, levelCode, suffixLength); err != nil {
			return err
		}
		if suffixLength == 0 {
			suffixLength = 1
		}
		abs := level
		if abs < 0 {
			abs = -abs
		}
		if abs > 3<<uint(suffixLength-1) && suffixLength < 6 {
			suffixLength++
		}
	}

	totalZeros := positions[totalCoeff-1] + 1 - totalCoeff
	if totalCoeff < maxCoeff {
		if err := writeTotalZeros(w, totalCoeff, totalZeros, maxCoeff); err != nil {
			return err
		}
	} else if totalZeros != 0 {
		return ErrTotalZeros
	}

	zerosLeft := totalZeros
	for i := 0; i < totalCoeff-1; i++ {
		if zerosLeft <= 0 {
			break
		}
		idx := totalCoeff - 1 - i
		run := positions[idx] - positions[idx-1] - 1
		if err := writeRunBefore(w, run, zerosLeft); err != nil {
			return err
		}
		zerosLeft -= run
	}
	return w.Err()
}
