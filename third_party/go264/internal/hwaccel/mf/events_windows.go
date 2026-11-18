package mf

import (
	"strconv"
	"time"
	"unsafe"
)

const (
	transformEventUnknown = 600 + iota
	transformEventNeedInput
	transformEventHaveOutput
	transformEventDrainComplete
	transformEventMarker
	transformEventInputStreamStateChanged
)

const eventFlagNoWait = 1

const (
	mfENoEventsAvailable   HRESULT = 0xc00d3e80
	mfEShutdown            HRESULT = 0xc00d3e85
	mfETransformTypeNotSet HRESULT = 0xc00d6d60
)

type eventGenerator struct {
	obj       unknown
	shutdown  unknown
	abandoned bool
}

func (t *Transform) eventGenerator() (*eventGenerator, error) {
	obj, err := t.obj.queryInterface(&IIDIMFMediaEventGenerator)
	if err != nil {
		return nil, err
	}
	g := &eventGenerator{obj: obj}
	if s, err := t.obj.queryInterface(&IIDIMFShutdown); err == nil {
		g.shutdown = s
	}
	return g, nil
}

func (g *eventGenerator) release() {
	if g == nil || g.abandoned {
		return
	}
	g.obj.release()
	g.obj = unknown{}
	if g.shutdown.p != nil {
		g.shutdown.release()
		g.shutdown = unknown{}
	}
}

func (g *eventGenerator) shutdownTransform() {
	if g.shutdown.p != nil {
		hr(g.shutdown.p, shutdownShutdown)
	}
}

type transformEvent struct {
	kind   uint32
	status HRESULT
}

var eventTimeout = 10 * time.Second

const eventPause = 20 * time.Microsecond

func (g *eventGenerator) next(wait bool) (transformEvent, bool, error) {
	if !wait {
		return g.get(eventFlagNoWait)
	}
	if g.shutdown.p == nil {
		return pollForEvent(func() (transformEvent, bool, error) { return g.get(eventFlagNoWait) }, eventTimeout)
	}
	ev, ok, err, settled := waitWithWatchdog(func() (transformEvent, bool, error) { return g.get(0) }, func() {
		hr(g.shutdown.p, shutdownShutdown)
	}, eventTimeout)
	if !settled {
		g.abandoned = true
	}
	return ev, ok, err
}

func timedOut(timeout time.Duration) error {
	return &Error{Op: "the transform sent no event within " + timeout.String(), Code: mfENoEventsAvailable}
}

func waitWithWatchdog(wait func() (transformEvent, bool, error), stop func(), timeout time.Duration) (transformEvent, bool, error, bool) {
	stopped := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		stop()
		close(stopped)
	})
	ev, ok, err := wait()
	if timer.Stop() {
		return ev, ok, err, true
	}
	select {
	case <-stopped:
		return transformEvent{}, false, timedOut(timeout), true
	case <-time.After(timeout):
		return transformEvent{}, false, timedOut(timeout), false
	}
}

func pollForEvent(poll func() (transformEvent, bool, error), timeout time.Duration) (transformEvent, bool, error) {
	start := time.Now()
	for {
		ev, ok, err := poll()
		if err != nil || ok {
			return ev, ok, err
		}
		if time.Since(start) >= timeout {
			return transformEvent{}, false, timedOut(timeout)
		}
		time.Sleep(eventPause)
	}
}

func (g *eventGenerator) get(flags uintptr) (transformEvent, bool, error) {
	var raw unsafe.Pointer
	code := hr(g.obj.p, eventGeneratorGetEvent, flags, uintptr(unsafe.Pointer(&raw)))
	if code == mfENoEventsAvailable {
		return transformEvent{}, false, nil
	}
	if err := check("IMFMediaEventGenerator::GetEvent", code); err != nil {
		return transformEvent{}, false, err
	}
	ev := unknown{raw}
	defer ev.release()

	var e transformEvent
	if code := hr(ev.p, mediaEventGetType, uintptr(unsafe.Pointer(&e.kind))); code.Failed() {
		return transformEvent{}, false, check("IMFMediaEvent::GetType", code)
	}
	if code := hr(ev.p, mediaEventGetStatus, uintptr(unsafe.Pointer(&e.status))); code.Failed() {
		return transformEvent{}, false, check("IMFMediaEvent::GetStatus", code)
	}
	if e.status.Failed() {
		return transformEvent{}, false, &Error{Op: "the transform's event " + strconv.Itoa(int(e.kind)), Code: e.status}
	}
	return e, true, nil
}
