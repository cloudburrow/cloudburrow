package nal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

type Type uint8

const (
	TypeUnspecified     Type = 0
	TypeSliceNonIDR     Type = 1
	TypeSliceDataPartA  Type = 2
	TypeSliceDataPartB  Type = 3
	TypeSliceDataPartC  Type = 4
	TypeSliceIDR        Type = 5
	TypeSEI             Type = 6
	TypeSPS             Type = 7
	TypePPS             Type = 8
	TypeAccessUnitDelim Type = 9
	TypeEndOfSequence   Type = 10
	TypeEndOfStream     Type = 11
	TypeFillerData      Type = 12
	TypeSPSExtension    Type = 13
	TypePrefix          Type = 14
	TypeSubsetSPS       Type = 15
	TypeAuxSliceNoPart  Type = 19
)

var typeNames = map[Type]string{
	TypeUnspecified:     "unspecified",
	TypeSliceNonIDR:     "slice",
	TypeSliceDataPartA:  "slice-data-a",
	TypeSliceDataPartB:  "slice-data-b",
	TypeSliceDataPartC:  "slice-data-c",
	TypeSliceIDR:        "idr",
	TypeSEI:             "sei",
	TypeSPS:             "sps",
	TypePPS:             "pps",
	TypeAccessUnitDelim: "aud",
	TypeEndOfSequence:   "end-of-sequence",
	TypeEndOfStream:     "end-of-stream",
	TypeFillerData:      "filler",
	TypeSPSExtension:    "sps-extension",
	TypePrefix:          "prefix",
	TypeSubsetSPS:       "subset-sps",
	TypeAuxSliceNoPart:  "aux-slice",
}

func (t Type) String() string {
	if s, ok := typeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("reserved(%d)", uint8(t))
}

func (t Type) IsSlice() bool {
	return t == TypeSliceNonIDR || t == TypeSliceIDR || t == TypeAuxSliceNoPart
}

func (t Type) IsVCL() bool { return t >= 1 && t <= 5 }

var (
	ErrEmptyUnit       = errors.New("go264/nal: empty NAL unit")
	ErrForbiddenBit    = errors.New("go264/nal: forbidden_zero_bit is set")
	ErrLengthSize      = errors.New("go264/nal: length prefix size must be 1, 2 or 4")
	ErrTruncatedLength = errors.New("go264/nal: truncated length-prefixed unit")
)

type Header struct {
	RefIDC uint8
	Type   Type
}

func (h Header) Byte() byte { return h.RefIDC&3<<5 | byte(h.Type)&0x1F }

type Unit struct {
	Header
	RBSP []byte
}

func ParseHeader(b byte) (Header, error) {
	if b&0x80 != 0 {
		return Header{}, ErrForbiddenBit
	}
	return Header{RefIDC: b >> 5 & 3, Type: Type(b & 0x1F)}, nil
}

func Parse(ebsp []byte) (Unit, error) {
	if len(ebsp) == 0 {
		return Unit{}, ErrEmptyUnit
	}
	h, err := ParseHeader(ebsp[0])
	if err != nil {
		return Unit{}, err
	}
	return Unit{Header: h, RBSP: Unescape(nil, ebsp[1:])}, nil
}

func Unescape(dst, src []byte) []byte {
	if dst == nil {
		dst = make([]byte, 0, len(src))
	}
	zeros := 0
	for _, b := range src {
		if zeros >= 2 && b == 0x03 {
			zeros = 0
			continue
		}
		dst = append(dst, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

func Escape(dst, src []byte) []byte {
	if dst == nil {
		dst = make([]byte, 0, len(src)+len(src)/64+1)
	}
	zeros := 0
	for _, b := range src {
		if zeros >= 2 && b <= 0x03 {
			dst = append(dst, 0x03)
			zeros = 0
		}
		dst = append(dst, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

func AppendAnnexB(dst []byte, u Unit, longStartCode bool) []byte {
	if longStartCode {
		dst = append(dst, 0x00, 0x00, 0x00, 0x01)
	} else {
		dst = append(dst, 0x00, 0x00, 0x01)
	}
	dst = append(dst, u.Header.Byte())
	return Escape(dst, u.RBSP)
}

func AppendAVCC(dst []byte, u Unit, lengthSize int) ([]byte, error) {
	if lengthSize != 1 && lengthSize != 2 && lengthSize != 4 {
		return dst, ErrLengthSize
	}
	body := Escape(nil, u.RBSP)
	n := len(body) + 1
	switch lengthSize {
	case 1:
		dst = append(dst, byte(n))
	case 2:
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	case 4:
		dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	}
	dst = append(dst, u.Header.Byte())
	return append(dst, body...), nil
}

func SplitAVCC(data []byte, lengthSize int) ([][]byte, error) {
	if lengthSize != 1 && lengthSize != 2 && lengthSize != 4 {
		return nil, ErrLengthSize
	}
	var out [][]byte
	for i := 0; i < len(data); {
		if i+lengthSize > len(data) {
			return out, ErrTruncatedLength
		}
		var n int
		switch lengthSize {
		case 1:
			n = int(data[i])
		case 2:
			n = int(binary.BigEndian.Uint16(data[i:]))
		case 4:
			v := binary.BigEndian.Uint32(data[i:])
			if uint64(v) > uint64(len(data)) {
				return out, ErrTruncatedLength
			}
			n = int(v)
		}
		i += lengthSize
		if i+n > len(data) {
			return out, ErrTruncatedLength
		}
		if n > 0 {
			out = append(out, data[i:i+n])
		}
		i += n
	}
	return out, nil
}
