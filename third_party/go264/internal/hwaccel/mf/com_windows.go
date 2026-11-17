package mf

import (
	"syscall"
	"unsafe"
)

const ptrSize = unsafe.Sizeof(uintptr(0))

//go:uintptrescapes
func vtblCall(this unsafe.Pointer, index int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(this)
	fn := *(*uintptr)(unsafe.Pointer(uintptr(vtbl) + uintptr(index)*ptrSize))
	call := make([]uintptr, 0, len(args)+1)
	call = append(call, uintptr(this))
	call = append(call, args...)
	r, _, _ := syscall.SyscallN(fn, call...)
	return r
}

//go:uintptrescapes
func hr(this unsafe.Pointer, index int, args ...uintptr) HRESULT {
	return HRESULT(vtblCall(this, index, args...))
}

const (
	unknownQueryInterface = 0
	unknownAddRef         = 1
	unknownRelease        = 2
)

type unknown struct{ p unsafe.Pointer }

func (u unknown) addRef() uint32 {
	return uint32(vtblCall(u.p, unknownAddRef))
}

func (u unknown) release() uint32 {
	if u.p == nil {
		return 0
	}
	return uint32(vtblCall(u.p, unknownRelease))
}

func (u unknown) queryInterface(iid *GUID) (unknown, error) {
	var out unsafe.Pointer
	code := hr(u.p, unknownQueryInterface,
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if err := check("QueryInterface", code); err != nil {
		return unknown{}, err
	}
	return unknown{out}, nil
}
