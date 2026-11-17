package mf

import (
	"sync"
	"syscall"
)

const (
	mfVersion       = 0x00020070
	mfStartupNoSock = 0x1
)

var (
	modmfplat      = syscall.NewLazyDLL("mfplat.dll")
	procMFStartup  = modmfplat.NewProc("MFStartup")
	procMFShutdown = modmfplat.NewProc("MFShutdown")
)

var platform struct {
	mu    sync.Mutex
	count int
}

func Startup() error {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if platform.count > 0 {
		platform.count++
		return nil
	}
	if err := modmfplat.Load(); err != nil {
		return &Error{Op: "LoadLibrary(mfplat.dll)", Code: sOK}
	}
	r, _, _ := procMFStartup.Call(mfVersion, mfStartupNoSock)
	if err := check("MFStartup", HRESULT(r)); err != nil {
		return err
	}
	platform.count = 1
	return nil
}

func Shutdown() error {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if platform.count == 0 {
		return nil
	}
	platform.count--
	if platform.count > 0 {
		return nil
	}
	r, _, _ := procMFShutdown.Call()
	return check("MFShutdown", HRESULT(r))
}

func Loaded() bool { return modmfplat.Load() == nil }
