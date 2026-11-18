package mf

import "unsafe"

var (
	procMFCreateSample              = modmfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer        = modmfplat.NewProc("MFCreateMemoryBuffer")
	procMFCreateAlignedMemoryBuffer = modmfplat.NewProc("MFCreateAlignedMemoryBuffer")
)

type Sample struct {
	obj unknown
}

type Buffer struct {
	obj unknown
}

func NewSample() (*Sample, error) {
	var out unsafe.Pointer
	r, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&out)))
	if err := check("MFCreateSample", HRESULT(r)); err != nil {
		return nil, err
	}
	return &Sample{obj: unknown{out}}, nil
}

func NewMemoryBuffer(size int) (*Buffer, error) {
	var out unsafe.Pointer
	r, _, _ := procMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&out)))
	if err := check("MFCreateMemoryBuffer", HRESULT(r)); err != nil {
		return nil, err
	}
	return &Buffer{obj: unknown{out}}, nil
}

func NewAlignedMemoryBuffer(size, alignment int) (*Buffer, error) {
	var out unsafe.Pointer
	r, _, _ := procMFCreateAlignedMemoryBuffer.Call(uintptr(size), uintptr(alignment), uintptr(unsafe.Pointer(&out)))
	if err := check("MFCreateAlignedMemoryBuffer", HRESULT(r)); err != nil {
		return nil, err
	}
	return &Buffer{obj: unknown{out}}, nil
}

func (s *Sample) Release() {
	if s == nil {
		return
	}
	s.obj.release()
	s.obj = unknown{}
}

func (b *Buffer) Release() {
	if b == nil {
		return
	}
	b.obj.release()
	b.obj = unknown{}
}

func (s *Sample) AddBuffer(b *Buffer) error {
	code := hr(s.obj.p, sampleAddBuffer, uintptr(b.obj.p))
	return check("IMFSample::AddBuffer", code)
}

func (s *Sample) BufferByIndex(i int) (*Buffer, error) {
	var out unsafe.Pointer
	code := hr(s.obj.p, sampleGetBufferByIndex, uintptr(i), uintptr(unsafe.Pointer(&out)))
	if err := check("IMFSample::GetBufferByIndex", code); err != nil {
		return nil, err
	}
	return &Buffer{obj: unknown{out}}, nil
}

func (s *Sample) ConvertToContiguousBuffer() (*Buffer, error) {
	var out unsafe.Pointer
	code := hr(s.obj.p, sampleConvertToContiguousBuffer, uintptr(unsafe.Pointer(&out)))
	if err := check("IMFSample::ConvertToContiguousBuffer", code); err != nil {
		return nil, err
	}
	return &Buffer{obj: unknown{out}}, nil
}

func (s *Sample) BufferCount() (int, error) {
	var n uint32
	code := hr(s.obj.p, sampleGetBufferCount, uintptr(unsafe.Pointer(&n)))
	if err := check("IMFSample::GetBufferCount", code); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *Sample) SetTime(hundredNanoseconds int64) error {
	code := hr(s.obj.p, sampleSetSampleTime, uintptr(hundredNanoseconds))
	return check("IMFSample::SetSampleTime", code)
}

func (s *Sample) SetDuration(hundredNanoseconds int64) error {
	code := hr(s.obj.p, sampleSetSampleDuration, uintptr(hundredNanoseconds))
	return check("IMFSample::SetSampleDuration", code)
}

func (s *Sample) Time() (int64, error) {
	var t int64
	code := hr(s.obj.p, sampleGetSampleTime, uintptr(unsafe.Pointer(&t)))
	if err := check("IMFSample::GetSampleTime", code); err != nil {
		return 0, err
	}
	return t, nil
}

func (s *Sample) Duration() (int64, error) {
	var d int64
	code := hr(s.obj.p, sampleGetSampleDuration, uintptr(unsafe.Pointer(&d)))
	if err := check("IMFSample::GetSampleDuration", code); err != nil {
		return 0, err
	}
	return d, nil
}

func (b *Buffer) SetCurrentLength(n int) error {
	code := hr(b.obj.p, bufferSetCurrentLength, uintptr(n))
	return check("IMFMediaBuffer::SetCurrentLength", code)
}

func (b *Buffer) CurrentLength() (int, error) {
	var n uint32
	code := hr(b.obj.p, bufferGetCurrentLength, uintptr(unsafe.Pointer(&n)))
	if err := check("IMFMediaBuffer::GetCurrentLength", code); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (b *Buffer) MaxLength() (int, error) {
	var n uint32
	code := hr(b.obj.p, bufferGetMaxLength, uintptr(unsafe.Pointer(&n)))
	if err := check("IMFMediaBuffer::GetMaxLength", code); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (b *Buffer) lock() (unsafe.Pointer, uint32, uint32, error) {
	var ptr unsafe.Pointer
	var maxLen, curLen uint32
	code := hr(b.obj.p, bufferLock,
		uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&curLen)))
	if err := check("IMFMediaBuffer::Lock", code); err != nil {
		return nil, 0, 0, err
	}
	return ptr, maxLen, curLen, nil
}

func (b *Buffer) unlock() error {
	code := hr(b.obj.p, bufferUnlock)
	return check("IMFMediaBuffer::Unlock", code)
}

func (b *Buffer) WithLocked(fn func(data []byte, maxLen int) error) error {
	ptr, maxLen, curLen, err := b.lock()
	if err != nil {
		return err
	}
	data := unsafe.Slice((*byte)(ptr), maxLen)[:curLen:maxLen]
	err = fn(data, int(maxLen))
	if unlockErr := b.unlock(); err == nil {
		err = unlockErr
	}
	return err
}

func (b *Buffer) Write(src []byte) error {
	return b.WithLocked(func(data []byte, maxLen int) error {
		if len(src) > maxLen {
			return &Error{Op: "IMFMediaBuffer::Write: source exceeds buffer capacity", Code: sOK}
		}
		copy(data[:maxLen], src)
		return b.SetCurrentLength(len(src))
	})
}

func (b *Buffer) Read(dst []byte) (int, error) {
	var n int
	err := b.WithLocked(func(data []byte, maxLen int) error {
		n = copy(dst, data)
		return nil
	})
	return n, err
}
