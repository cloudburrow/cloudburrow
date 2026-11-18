package mf

import "unsafe"

var procMFCreateMediaType = modmfplat.NewProc("MFCreateMediaType")

type MediaType struct {
	obj unknown
}

func NewMediaType() (*MediaType, error) {
	var obj unsafe.Pointer
	r, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&obj)))
	if err := check("MFCreateMediaType", HRESULT(r)); err != nil {
		return nil, err
	}
	return &MediaType{obj: unknown{obj}}, nil
}

func (m *MediaType) Release() {
	if m == nil {
		return
	}
	m.obj.release()
	m.obj = unknown{}
}

func (m *MediaType) SetGUID(key *GUID, value GUID) error {
	code := hr(m.obj.p, attributesSetGUID,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&value)))
	return check("IMFAttributes::SetGUID", code)
}

func (m *MediaType) GUID(key *GUID) (GUID, error) {
	var v GUID
	code := hr(m.obj.p, attributesGetGUID,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	if err := check("IMFAttributes::GetGUID", code); err != nil {
		return GUID{}, err
	}
	return v, nil
}

func (m *MediaType) SetUint32(key *GUID, v uint32) error {
	code := hr(m.obj.p, attributesSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v))
	return check("IMFAttributes::SetUINT32", code)
}

func (m *MediaType) Uint32(key *GUID) (uint32, error) {
	var v uint32
	code := hr(m.obj.p, attributesGetUINT32,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	if err := check("IMFAttributes::GetUINT32", code); err != nil {
		return 0, err
	}
	return v, nil
}

func (m *MediaType) SetUint64(key *GUID, v uint64) error {
	code := hr(m.obj.p, attributesSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(v))
	return check("IMFAttributes::SetUINT64", code)
}

func (m *MediaType) Uint64(key *GUID) (uint64, error) {
	var v uint64
	code := hr(m.obj.p, attributesGetUINT64,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	if err := check("IMFAttributes::GetUINT64", code); err != nil {
		return 0, err
	}
	return v, nil
}

func pack2UInt32AsUInt64(hi, lo uint32) uint64 {
	return uint64(hi)<<32 | uint64(lo)
}

func unpack2UInt32AsUInt64(v uint64) (hi, lo uint32) {
	return uint32(v >> 32), uint32(v)
}

func (m *MediaType) SetRatio(key *GUID, numerator, denominator uint32) error {
	return m.SetUint64(key, pack2UInt32AsUInt64(numerator, denominator))
}

func (m *MediaType) Ratio(key *GUID) (numerator, denominator uint32, err error) {
	v, err := m.Uint64(key)
	if err != nil {
		return 0, 0, err
	}
	numerator, denominator = unpack2UInt32AsUInt64(v)
	return numerator, denominator, nil
}

func (m *MediaType) SetSize(key *GUID, width, height uint32) error {
	return m.SetUint64(key, pack2UInt32AsUInt64(width, height))
}

func (m *MediaType) Size(key *GUID) (width, height uint32, err error) {
	v, err := m.Uint64(key)
	if err != nil {
		return 0, 0, err
	}
	width, height = unpack2UInt32AsUInt64(v)
	return width, height, nil
}

func (m *MediaType) Blob(key *GUID) ([]byte, error) {
	var ptr unsafe.Pointer
	var n uint32
	code := hr(m.obj.p, attributesGetAllocatedBlob,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&n)))
	if err := check("IMFAttributes::GetAllocatedBlob", code); err != nil {
		return nil, err
	}
	if ptr == nil || n == 0 {
		return nil, nil
	}
	defer coTaskMemFree(ptr)
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(ptr), n))
	return out, nil
}

type EncoderFormat struct {
	Width, Height        int
	FPSNum, FPSDen       int
	BitrateBitsPerSecond int
	Profile              uint32
	QP                   int
}

func (f EncoderFormat) baseType(subtype GUID) (*MediaType, error) {
	mt, err := NewMediaType()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*MediaType, error) {
		mt.Release()
		return nil, err
	}
	if err := mt.SetGUID(&MFMTMajorType, MFMediaTypeVideo); err != nil {
		return fail(err)
	}
	if err := mt.SetGUID(&MFMTSubtype, subtype); err != nil {
		return fail(err)
	}
	if err := mt.SetSize(&MFMTFrameSize, uint32(f.Width), uint32(f.Height)); err != nil {
		return fail(err)
	}
	if err := mt.SetRatio(&MFMTFrameRate, uint32(f.FPSNum), uint32(f.FPSDen)); err != nil {
		return fail(err)
	}
	if err := mt.SetUint32(&MFMTInterlaceMode, MFVideoInterlaceProgressive); err != nil {
		return fail(err)
	}
	if err := mt.SetRatio(&MFMTPixelAspectRatio, 1, 1); err != nil {
		return fail(err)
	}
	return mt, nil
}

func (f EncoderFormat) OutputType() (*MediaType, error) {
	mt, err := f.baseType(MFVideoFormatH264)
	if err != nil {
		return nil, err
	}
	if err := mt.SetUint32(&MFMTAvgBitrate, uint32(f.BitrateBitsPerSecond)); err != nil {
		mt.Release()
		return nil, err
	}
	if err := mt.SetUint32(&MFMTMpeg2Profile, f.Profile); err != nil {
		mt.Release()
		return nil, err
	}
	return mt, nil
}

func (f EncoderFormat) InputType() (*MediaType, error) {
	return f.baseType(MFVideoFormatNV12)
}

type VideoOffset struct {
	Fract uint16
	Value int16
}

type VideoArea struct {
	OffsetX VideoOffset
	OffsetY VideoOffset
	Width   int32
	Height  int32
}

func (m *MediaType) VideoArea(key *GUID) (VideoArea, bool) {
	blob, err := m.Blob(key)
	if err != nil || len(blob) < int(unsafe.Sizeof(VideoArea{})) {
		return VideoArea{}, false
	}
	area := *(*VideoArea)(unsafe.Pointer(&blob[0]))
	if area.Width <= 0 || area.Height <= 0 {
		return VideoArea{}, false
	}
	return area, true
}
