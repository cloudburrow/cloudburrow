package mf

import (
	"syscall"
	"unsafe"
)

var (
	procMFTEnumEx     = modmfplat.NewProc("MFTEnumEx")
	modole32          = syscall.NewLazyDLL("ole32.dll")
	procCoTaskMemFree = modole32.NewProc("CoTaskMemFree")
)

func coTaskMemFree(p unsafe.Pointer) {
	if p != nil {
		procCoTaskMemFree.Call(uintptr(p))
	}
}

type TransformDescription struct {
	Name     string
	Hardware bool
	Async    bool
}

func attributeString(u unknown, key *GUID) (string, bool) {
	var ptr unsafe.Pointer
	var n uint32
	code := hr(u.p, attributesGetAllocatedString,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&n)))
	if code.Failed() || ptr == nil {
		return "", false
	}
	defer coTaskMemFree(ptr)
	return syscall.UTF16ToString(unsafe.Slice((*uint16)(ptr), n)), true
}

func attributeUint32(u unknown, key *GUID) (uint32, bool) {
	var v uint32
	code := hr(u.p, attributesGetUINT32,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	if code.Failed() {
		return 0, false
	}
	return v, true
}

func describeActivate(act unknown) TransformDescription {
	var d TransformDescription
	d.Name, _ = attributeString(act, &MFTFriendlyNameAttribute)
	_, d.Hardware = attributeString(act, &MFTEnumHardwareURLAttribute)
	if v, ok := attributeUint32(act, &MFTransformAsync); ok {
		d.Async = v != 0
	}
	return d
}

const enumAllTransforms = MFTEnumFlagSyncMFT | MFTEnumFlagAsyncMFT | MFTEnumFlagHardware | MFTEnumFlagSortAndFilter

func enumActivates(category GUID, input, output *MFTRegisterTypeInfo, visit func(unknown) bool) error {
	var array unsafe.Pointer
	var count uint32
	r, _, _ := procMFTEnumEx.Call(
		uintptr(unsafe.Pointer(&category)),
		uintptr(enumAllTransforms),
		uintptr(unsafe.Pointer(input)),
		uintptr(unsafe.Pointer(output)),
		uintptr(unsafe.Pointer(&array)),
		uintptr(unsafe.Pointer(&count)),
	)
	if err := check("MFTEnumEx", HRESULT(r)); err != nil {
		return err
	}
	if array == nil {
		return nil
	}
	defer coTaskMemFree(array)

	stop := false
	for i := uint32(0); i < count; i++ {
		slot := *(*unsafe.Pointer)(unsafe.Pointer(uintptr(array) + uintptr(i)*ptrSize))
		if slot == nil {
			continue
		}
		act := unknown{slot}
		if !stop && !visit(act) {
			stop = true
		}
		act.release()
	}
	return nil
}

func ListTransforms(category GUID, input, output *MFTRegisterTypeInfo) ([]TransformDescription, error) {
	if err := Startup(); err != nil {
		return nil, err
	}
	defer Shutdown()

	var out []TransformDescription
	err := enumActivates(category, input, output, func(act unknown) bool {
		out = append(out, describeActivate(act))
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func videoTypeInfo(subtype GUID) MFTRegisterTypeInfo {
	return MFTRegisterTypeInfo{GuidMajorType: MFMediaTypeVideo, GuidSubtype: subtype}
}

func H264EncoderTypes() (in, out MFTRegisterTypeInfo) {
	return videoTypeInfo(MFVideoFormatNV12), videoTypeInfo(MFVideoFormatH264)
}

func H264DecoderTypes() (in, out MFTRegisterTypeInfo) {
	return videoTypeInfo(MFVideoFormatH264), videoTypeInfo(MFVideoFormatNV12)
}

func ListH264Encoders() ([]TransformDescription, error) {
	_, out := H264EncoderTypes()
	return ListTransforms(MFTCategoryVideoEncoder, nil, &out)
}

func ListH264Decoders() ([]TransformDescription, error) {
	in, _ := H264DecoderTypes()
	return ListTransforms(MFTCategoryVideoDecoder, &in, nil)
}

func openTransform(category GUID, input, output *MFTRegisterTypeInfo, accept func(TransformDescription) bool) (*Transform, TransformDescription, error) {
	var opened *Transform
	var chosen TransformDescription
	err := enumActivates(category, input, output, func(act unknown) bool {
		d := describeActivate(act)
		if accept != nil && !accept(d) {
			return true
		}
		var obj unsafe.Pointer
		code := hr(act.p, activateActivateObject,
			uintptr(unsafe.Pointer(&IIDIMFTransform)), uintptr(unsafe.Pointer(&obj)))
		if code.Failed() {
			return true
		}
		opened = &Transform{obj: unknown{obj}}
		chosen = d
		return false
	})
	if err != nil {
		return nil, TransformDescription{}, err
	}
	if opened == nil {
		return nil, TransformDescription{}, &Error{Op: "MFTEnumEx found no usable transform", Code: sOK}
	}
	return opened, chosen, nil
}
