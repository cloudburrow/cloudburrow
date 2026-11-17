package mf

import "unsafe"

type Transform struct {
	obj unknown
}

func (t *Transform) Release() {
	if t == nil {
		return
	}
	t.obj.release()
	t.obj = unknown{}
}

func (t *Transform) StreamIDs() (in, out uint32, err error) {
	var inCount, outCount uint32
	code := hr(t.obj.p, transformGetStreamCount,
		uintptr(unsafe.Pointer(&inCount)), uintptr(unsafe.Pointer(&outCount)))
	if err := check("IMFTransform::GetStreamCount", code); err != nil {
		return 0, 0, err
	}
	if inCount == 0 || outCount == 0 {
		return 0, 0, &Error{Op: "IMFTransform reported no streams", Code: sOK}
	}
	inIDs := make([]uint32, inCount)
	outIDs := make([]uint32, outCount)
	code = hr(t.obj.p, transformGetStreamIDs,
		uintptr(inCount), uintptr(unsafe.Pointer(&inIDs[0])),
		uintptr(outCount), uintptr(unsafe.Pointer(&outIDs[0])))
	if code.Failed() {
		return 0, 0, nil
	}
	return inIDs[0], outIDs[0], nil
}

func (t *Transform) ProcessMessage(message uint32, param uintptr) error {
	code := hr(t.obj.p, transformProcessMessage, uintptr(message), param)
	return check("IMFTransform::ProcessMessage", code)
}

func (t *Transform) OutputStreamInfo(stream uint32) (MFTOutputStreamInfo, error) {
	var info MFTOutputStreamInfo
	code := hr(t.obj.p, transformGetOutputStreamInfo,
		uintptr(stream), uintptr(unsafe.Pointer(&info)))
	return info, check("IMFTransform::GetOutputStreamInfo", code)
}

func (t *Transform) InputStreamInfo(stream uint32) (MFTInputStreamInfo, error) {
	var info MFTInputStreamInfo
	code := hr(t.obj.p, transformGetInputStreamInfo,
		uintptr(stream), uintptr(unsafe.Pointer(&info)))
	return info, check("IMFTransform::GetInputStreamInfo", code)
}

func (t *Transform) Attributes() (unknown, error) {
	var out unsafe.Pointer
	code := hr(t.obj.p, transformGetAttributes, uintptr(unsafe.Pointer(&out)))
	if err := check("IMFTransform::GetAttributes", code); err != nil {
		return unknown{}, err
	}
	return unknown{out}, nil
}

const (
	outputStreamProvidesSamples   = 0x100
	outputStreamCanProvideSamples = 0x200
)

func setAttributeUint32(u unknown, key *GUID, v uint32) error {
	code := hr(u.p, attributesSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v))
	return check("IMFAttributes::SetUINT32", code)
}

func (t *Transform) Unlock() error {
	attrs, err := t.Attributes()
	if err != nil {
		return err
	}
	defer attrs.release()
	if v, ok := attributeUint32(attrs, &MFTransformAsync); !ok || v == 0 {
		return nil
	}
	return setAttributeUint32(attrs, &MFTransformAsyncUnlock, 1)
}

func (t *Transform) IsAsync() bool {
	attrs, err := t.Attributes()
	if err != nil {
		return false
	}
	defer attrs.release()
	v, ok := attributeUint32(attrs, &MFTransformAsync)
	return ok && v != 0
}

func (t *Transform) SetOutputType(stream uint32, mediaType unsafe.Pointer, flags uint32) error {
	code := hr(t.obj.p, transformSetOutputType, uintptr(stream), uintptr(mediaType), uintptr(flags))
	return check("IMFTransform::SetOutputType", code)
}

func (t *Transform) SetInputType(stream uint32, mediaType unsafe.Pointer, flags uint32) error {
	code := hr(t.obj.p, transformSetInputType, uintptr(stream), uintptr(mediaType), uintptr(flags))
	return check("IMFTransform::SetInputType", code)
}

func (t *Transform) ProcessInput(stream uint32, sample unsafe.Pointer) HRESULT {
	return hr(t.obj.p, transformProcessInput, uintptr(stream), uintptr(sample), 0)
}

func (t *Transform) ProcessOutput(buffers []MFTOutputDataBuffer) (uint32, HRESULT) {
	if len(buffers) == 0 {
		return 0, MFEInvalidMediaType
	}
	var status uint32
	code := hr(t.obj.p, transformProcessOutput,
		0, uintptr(len(buffers)), uintptr(unsafe.Pointer(&buffers[0])), uintptr(unsafe.Pointer(&status)))
	return status, code
}

func (t *Transform) OutputAvailableType(stream, index uint32) (unsafe.Pointer, HRESULT) {
	var out unsafe.Pointer
	code := hr(t.obj.p, transformGetOutputAvailableType,
		uintptr(stream), uintptr(index), uintptr(unsafe.Pointer(&out)))
	return out, code
}

func (t *Transform) OutputCurrentType(stream uint32) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	code := hr(t.obj.p, transformGetOutputCurrentType, uintptr(stream), uintptr(unsafe.Pointer(&out)))
	if err := check("IMFTransform::GetOutputCurrentType", code); err != nil {
		return nil, err
	}
	return out, nil
}
