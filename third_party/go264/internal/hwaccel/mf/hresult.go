//go:build windows

package mf

import "strconv"

type HRESULT uint32

const (
	sOK    HRESULT = 0x00000000
	sFalse HRESULT = 0x00000001
)

func (h HRESULT) Failed() bool { return h&0x80000000 != 0 }

func (h HRESULT) String() string {
	return "0x" + strconv.FormatUint(uint64(h), 16)
}

type Error struct {
	Op   string
	Code HRESULT
}

func (e *Error) Error() string {
	return "go264/mf: " + e.Op + " returned " + e.Code.String()
}

func check(op string, h HRESULT) error {
	if h.Failed() {
		return &Error{Op: op, Code: h}
	}
	return nil
}
