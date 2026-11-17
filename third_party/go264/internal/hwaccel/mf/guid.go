package mf

import "encoding/hex"

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g GUID) String() string {
	var b [38]byte
	b[0] = '{'
	b[9] = '-'
	b[14] = '-'
	b[19] = '-'
	b[24] = '-'
	b[37] = '}'
	put32(b[1:9], g.Data1)
	put16(b[10:14], g.Data2)
	put16(b[15:19], g.Data3)
	hex.Encode(b[20:24], g.Data4[:2])
	hex.Encode(b[25:37], g.Data4[2:])
	upper(b[1:37])
	return string(b[:])
}

func put32(dst []byte, v uint32) {
	hex.Encode(dst, []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func put16(dst []byte, v uint16) {
	hex.Encode(dst, []byte{byte(v >> 8), byte(v)})
}

func upper(b []byte) {
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
}

var mediaSubtypeBase = [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}

func fourCCGUID(cc string) GUID {
	var v uint32
	for i := 0; i < 4 && i < len(cc); i++ {
		v |= uint32(cc[i]) << uint(8*i)
	}
	return GUID{Data1: v, Data2: 0x0000, Data3: 0x0010, Data4: mediaSubtypeBase}
}

var (
	MFMediaTypeVideo  = fourCCGUID("vids")
	MFVideoFormatH264 = fourCCGUID("H264")
	MFVideoFormatNV12 = fourCCGUID("NV12")
	MFVideoFormatIYUV = fourCCGUID("IYUV")
)
