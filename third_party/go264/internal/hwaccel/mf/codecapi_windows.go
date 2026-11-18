package mf

import "unsafe"

var (
	CodecAPIAVEncCommonRateControlMode  = GUID{Data1: 0x1c0608e9, Data2: 0x370c, Data3: 0x4710, Data4: [8]byte{0x8a, 0x58, 0xcb, 0x61, 0x81, 0xc4, 0x24, 0x23}}
	CodecAPIAVEncVideoEncodeQP          = GUID{Data1: 0x2cb5696b, Data2: 0x23fb, Data3: 0x4ce1, Data4: [8]byte{0xa0, 0xf9, 0xef, 0x5b, 0x90, 0xfd, 0x55, 0xca}}
	CodecAPIAVEncVideoEncodeFrameTypeQP = GUID{Data1: 0xaa70b610, Data2: 0xe03f, Data3: 0x450c, Data4: [8]byte{0xad, 0x07, 0x07, 0x31, 0x4e, 0x63, 0x9c, 0xe7}}
	CodecAPIAVEncVideoMinQP             = GUID{Data1: 0x0ee22c6a, Data2: 0xa37c, Data3: 0x4568, Data4: [8]byte{0xb5, 0xf1, 0x9d, 0x4c, 0x2b, 0x3a, 0xb8, 0x86}}
	CodecAPIAVEncVideoMaxQP             = GUID{Data1: 0x3daf6f66, Data2: 0xa6a7, Data3: 0x45e0, Data4: [8]byte{0xa8, 0xe5, 0xf2, 0x74, 0x3f, 0x46, 0xa3, 0xa2}}
)

const AVEncCommonRateControlModeQuality = 3

const (
	codecAPISetValue = 9

	vtUI4 = 19
	vtUI8 = 21
)

type variant struct {
	vt        uint16
	reserved1 uint16
	reserved2 uint16
	reserved3 uint16
	value     uint64
	record    uint64
}

type codecAPI struct {
	obj unknown
}

func (t *Transform) codecAPI() (*codecAPI, error) {
	u, err := t.obj.queryInterface(&IIDICodecAPI)
	if err != nil {
		return nil, err
	}
	return &codecAPI{obj: u}, nil
}

func (c *codecAPI) Release() {
	c.obj.release()
	c.obj = unknown{}
}

func (c *codecAPI) set(api *GUID, vt uint16, v uint64) error {
	val := variant{vt: vt, value: v}
	return check("ICodecAPI::SetValue", hr(c.obj.p, codecAPISetValue,
		uintptr(unsafe.Pointer(api)), uintptr(unsafe.Pointer(&val))))
}

func (c *codecAPI) setUint32(api *GUID, v uint32) error {
	if err := c.set(api, vtUI4, uint64(v)); err == nil {
		return nil
	}
	return c.set(api, vtUI8, uint64(v))
}

func (t *Transform) setConstantQuantiser(qp int) error {
	api, err := t.codecAPI()
	if err != nil {
		return err
	}
	defer api.Release()
	if err := api.setUint32(&CodecAPIAVEncCommonRateControlMode, AVEncCommonRateControlModeQuality); err != nil {
		return err
	}
	if err := api.setUint32(&CodecAPIAVEncVideoEncodeQP, uint32(qp)); err != nil {
		return err
	}
	q := uint64(qp)
	if api.set(&CodecAPIAVEncVideoEncodeFrameTypeQP, vtUI8, q|q<<16|q<<32) == nil {
		return nil
	}
	if err := api.setUint32(&CodecAPIAVEncVideoMinQP, uint32(qp)); err != nil {
		return err
	}
	return api.setUint32(&CodecAPIAVEncVideoMaxQP, uint32(qp))
}
