package mf

import "unsafe"

var procMFCreateDXGIDeviceManager = modmfplat.NewProc("MFCreateDXGIDeviceManager")

var (
	IIDIMFDXGIDeviceManager = GUID{Data1: 0xeb533d5d, Data2: 0x2db6, Data3: 0x40f8, Data4: [8]byte{0x97, 0xa9, 0x49, 0x46, 0x92, 0x01, 0x4f, 0x07}}
	IIDIMFDXGIBuffer        = GUID{Data1: 0xe7174cfa, Data2: 0x1c9e, Data3: 0x48b1, Data4: [8]byte{0x88, 0x66, 0x62, 0x62, 0x26, 0xbf, 0xc2, 0x58}}
)

const (
	dxgiManagerCloseDeviceHandle = 3
	dxgiManagerGetVideoService   = 4
	dxgiManagerLockDevice        = 5
	dxgiManagerOpenDeviceHandle  = 6
	dxgiManagerResetDevice       = 7
	dxgiManagerTestDevice        = 8
	dxgiManagerUnlockDevice      = 9
)

const (
	dxgiBufferGetResource         = 3
	dxgiBufferGetSubresourceIndex = 4
)

type deviceManager struct {
	obj    unknown
	token  uint32
	device *d3dDevice
}

func newDeviceManager() (*deviceManager, error) {
	device, err := newD3DDevice()
	if err != nil {
		return nil, err
	}
	var token uint32
	var raw unsafe.Pointer
	r, _, _ := procMFCreateDXGIDeviceManager.Call(
		uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&raw)))
	if err := check("MFCreateDXGIDeviceManager", HRESULT(r)); err != nil {
		device.release()
		return nil, err
	}
	m := &deviceManager{obj: unknown{raw}, token: token, device: device}
	code := hr(m.obj.p, dxgiManagerResetDevice, uintptr(device.device.p), uintptr(token))
	if err := check("IMFDXGIDeviceManager::ResetDevice", code); err != nil {
		m.release()
		return nil, err
	}
	return m, nil
}

func (m *deviceManager) release() {
	if m == nil {
		return
	}
	m.obj.release()
	m.obj = unknown{}
	m.device.release()
	m.device = nil
}

func dxgiTextureOfSample(sample *Sample) (tex unknown, subresource uint32, ok bool, err error) {
	buf, err := sample.BufferByIndex(0)
	if err != nil {
		return unknown{}, 0, false, err
	}
	defer buf.Release()

	dxgi, qerr := buf.obj.queryInterface(&IIDIMFDXGIBuffer)
	if qerr != nil {
		return unknown{}, 0, false, nil
	}
	defer dxgi.release()

	var raw unsafe.Pointer
	code := hr(dxgi.p, dxgiBufferGetResource,
		uintptr(unsafe.Pointer(&IIDID3D11Texture2D)), uintptr(unsafe.Pointer(&raw)))
	if code.Failed() || raw == nil {
		return unknown{}, 0, false, nil
	}
	var index uint32
	code = hr(dxgi.p, dxgiBufferGetSubresourceIndex, uintptr(unsafe.Pointer(&index)))
	if code.Failed() {
		(unknown{raw}).release()
		return unknown{}, 0, false, check("IMFDXGIBuffer::GetSubresourceIndex", code)
	}
	return unknown{raw}, index, true, nil
}

type textureReader struct {
	device  *d3dDevice
	staging unknown
	width   uint32
	height  uint32
	format  uint32
}

func newTextureReader(device *d3dDevice) *textureReader {
	return &textureReader{device: device}
}

func (r *textureReader) release() {
	if r == nil {
		return
	}
	r.staging.release()
	r.staging = unknown{}
	r.device = nil
}

func (r *textureReader) ensureStaging(desc d3d11Texture2DDesc) error {
	if r.staging.p != nil && r.width == desc.Width && r.height == desc.Height && r.format == desc.Format {
		return nil
	}
	tex, err := r.device.createStagingTexture(desc.Width, desc.Height, desc.Format)
	if err != nil {
		return err
	}
	r.staging.release()
	r.staging = tex
	r.width, r.height, r.format = desc.Width, desc.Height, desc.Format
	return nil
}

func (r *textureReader) withMappedNV12(sample *Sample, width, height int, fn func(src []byte, pitch, height int)) (bool, error) {
	tex, subresource, ok, err := dxgiTextureOfSample(sample)
	if err != nil || !ok {
		return false, err
	}
	defer tex.release()

	desc := textureDesc(tex)
	if desc.Format != dxgiFormatNV12 {
		return false, &Error{Op: "the adapter returned a surface that is not NV12", Code: sOK}
	}
	if int(desc.Width) < width || int(desc.Height) < height {
		return false, &Error{Op: "the adapter returned a surface smaller than the picture", Code: sOK}
	}
	if err := r.ensureStaging(desc); err != nil {
		return false, err
	}
	r.device.copySubresource(r.staging, tex, subresource)
	mapped, err := r.device.mapRead(r.staging)
	if err != nil {
		return false, err
	}
	pitch := int(mapped.RowPitch)
	total := pitch*int(desc.Height) + pitch*(int(desc.Height)/2)
	fn(unsafe.Slice((*byte)(mapped.Data), total), pitch, int(desc.Height))
	r.device.unmap(r.staging)
	return true, nil
}
