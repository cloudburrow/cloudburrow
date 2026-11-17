package mf

import (
	"syscall"
	"unsafe"
)

var (
	modd3d11              = syscall.NewLazyDLL("d3d11.dll")
	procD3D11CreateDevice = modd3d11.NewProc("D3D11CreateDevice")
)

var (
	IIDID3D11Device      = GUID{Data1: 0xdb6f6ddb, Data2: 0xac77, Data3: 0x4e88, Data4: [8]byte{0x82, 0x53, 0x81, 0x9d, 0xf9, 0xbb, 0xf1, 0x40}}
	IIDID3D11Texture2D   = GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
	IIDID3D10Multithread = GUID{Data1: 0x9b7e4e00, Data2: 0x342c, Data3: 0x4106, Data4: [8]byte{0xa1, 0x9f, 0x4f, 0x27, 0x04, 0xf6, 0x89, 0xf0}}
)

const (
	d3dDriverTypeHardware = 1
	d3d11SDKVersion       = 7
)

const (
	d3d11CreateDeviceBGRASupport  = 0x20
	d3d11CreateDeviceVideoSupport = 0x800
)

const (
	featureLevel110 = 0xb000
	featureLevel111 = 0xb100
	featureLevel101 = 0xa100
	featureLevel100 = 0xa000
)

const (
	d3d11UsageDefault  = 0
	d3d11UsageStaging  = 3
	d3d11CPUAccessRead = 0x20000
	d3d11MapRead       = 1
	dxgiFormatNV12     = 0x67
)

const (
	deviceCreateTexture2D     = 5
	deviceGetImmediateContext = 40
)

const (
	contextMap                   = 14
	contextUnmap                 = 15
	contextCopySubresourceRegion = 46
	contextFlush                 = 111
)

const textureGetDesc = 10

const multithreadSetProtected = 5

type dxgiSampleDesc struct {
	Count   uint32
	Quality uint32
}

type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleDesc     dxgiSampleDesc
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

type d3d11MappedSubresource struct {
	Data       unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

type d3dDevice struct {
	device       unknown
	context      unknown
	featureLevel uint32
}

func d3d11Available() bool { return modd3d11.Load() == nil }

var d3d11FeatureLevelSets = [][]uint32{
	{featureLevel111, featureLevel110, featureLevel101, featureLevel100},
	{featureLevel110, featureLevel101, featureLevel100},
}

func newD3DDevice() (*d3dDevice, error) {
	if err := modd3d11.Load(); err != nil {
		return nil, &Error{Op: "LoadLibrary(d3d11.dll)", Code: sOK}
	}
	last := sOK
	for _, levels := range d3d11FeatureLevelSets {
		var device, context unsafe.Pointer
		var obtained uint32
		r, _, _ := procD3D11CreateDevice.Call(
			0,
			d3dDriverTypeHardware,
			0,
			d3d11CreateDeviceVideoSupport|d3d11CreateDeviceBGRASupport,
			uintptr(unsafe.Pointer(&levels[0])),
			uintptr(len(levels)),
			d3d11SDKVersion,
			uintptr(unsafe.Pointer(&device)),
			uintptr(unsafe.Pointer(&obtained)),
			uintptr(unsafe.Pointer(&context)),
		)
		last = HRESULT(r)
		if last.Failed() {
			continue
		}
		if device == nil || context == nil {
			(unknown{device}).release()
			(unknown{context}).release()
			continue
		}
		d := &d3dDevice{device: unknown{device}, context: unknown{context}, featureLevel: obtained}
		d.protectMultithreaded()
		return d, nil
	}
	if !last.Failed() {
		last = sOK
	}
	return nil, &Error{Op: "D3D11CreateDevice", Code: last}
}

func (d *d3dDevice) protectMultithreaded() bool {
	for _, holder := range []unknown{d.device, d.context} {
		mt, err := holder.queryInterface(&IIDID3D10Multithread)
		if err != nil {
			continue
		}
		vtblCall(mt.p, multithreadSetProtected, 1)
		mt.release()
		return true
	}
	return false
}

func (d *d3dDevice) release() {
	if d == nil {
		return
	}
	d.context.release()
	d.context = unknown{}
	d.device.release()
	d.device = unknown{}
}

func (d *d3dDevice) createStagingTexture(width, height, format uint32) (unknown, error) {
	desc := d3d11Texture2DDesc{
		Width:          width,
		Height:         height,
		MipLevels:      1,
		ArraySize:      1,
		Format:         format,
		SampleDesc:     dxgiSampleDesc{Count: 1},
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CPUAccessRead,
	}
	var tex unsafe.Pointer
	code := hr(d.device.p, deviceCreateTexture2D,
		uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&tex)))
	if err := check("ID3D11Device::CreateTexture2D", code); err != nil {
		return unknown{}, err
	}
	return unknown{tex}, nil
}

func textureDesc(tex unknown) d3d11Texture2DDesc {
	var desc d3d11Texture2DDesc
	vtblCall(tex.p, textureGetDesc, uintptr(unsafe.Pointer(&desc)))
	return desc
}

func (d *d3dDevice) copySubresource(dst unknown, src unknown, subresource uint32) {
	vtblCall(d.context.p, contextCopySubresourceRegion,
		uintptr(dst.p), 0, 0, 0, 0, uintptr(src.p), uintptr(subresource), 0)
}

func (d *d3dDevice) mapRead(tex unknown) (d3d11MappedSubresource, error) {
	var mapped d3d11MappedSubresource
	code := hr(d.context.p, contextMap,
		uintptr(tex.p), 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(&mapped)))
	if err := check("ID3D11DeviceContext::Map", code); err != nil {
		return d3d11MappedSubresource{}, err
	}
	return mapped, nil
}

func (d *d3dDevice) unmap(tex unknown) {
	vtblCall(d.context.p, contextUnmap, uintptr(tex.p), 0)
}
