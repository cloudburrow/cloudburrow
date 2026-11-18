package mf

import "runtime"

const (
	coinitMultithreaded    = 0x0
	coinitDisableOLE1DDE   = 0x4
	rpcEChangedMode        = HRESULT(0x80010106)
	comAlreadyInitialized  = sFalse
	comInitialisationFlags = coinitMultithreaded | coinitDisableOLE1DDE
)

var (
	modole32Init      = modole32
	procCoInitializeE = modole32Init.NewProc("CoInitializeEx")
	procCoUninitial   = modole32Init.NewProc("CoUninitialize")
)

type comThread struct {
	calls  chan func()
	closed chan struct{}
}

func newCOMThread() *comThread {
	t := &comThread{calls: make(chan func()), closed: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(t.closed)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		owned := initialiseCOM()
		close(ready)
		for fn := range t.calls {
			fn()
		}
		if owned {
			procCoUninitial.Call()
		}
	}()
	<-ready
	return t
}

func initialiseCOM() bool {
	r, _, _ := procCoInitializeE.Call(0, comInitialisationFlags)
	switch HRESULT(r) {
	case sOK:
		return true
	case comAlreadyInitialized, rpcEChangedMode:
		return false
	}
	return false
}

func (t *comThread) run(fn func()) {
	if t == nil {
		fn()
		return
	}
	done := make(chan struct{})
	t.calls <- func() {
		defer close(done)
		fn()
	}
	<-done
}

func (t *comThread) stop() {
	if t == nil || t.calls == nil {
		return
	}
	close(t.calls)
	t.calls = nil
	<-t.closed
}
