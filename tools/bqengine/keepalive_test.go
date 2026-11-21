package main

import (
	"strings"
	"testing"
)

// keepAliveSrc has the shapes of the generated bindings the
// handle-keepalive rewrite knows (#2134).
const keepAliveSrc = `package googlesql

import "runtime"

type Kind int32

type TypeNode interface{ rawPtr() uint64 }

type Base struct {
	ptr       uint64
	keepAlive []any
}

type Mid struct {
	*Base
}

type Outer struct {
	*Mid
}

type Options struct {
	Language *Base
}

func newBaseNoFinalizer(ptr uint64) *Base { return &Base{ptr: ptr} }

func newBase(ptr uint64) *Base {
	h := &Base{ptr: ptr}
	runtime.SetFinalizer(h, (*Base).free)
	return h
}

func newMidNoFinalizer(ptr uint64) *Mid { return &Mid{Base: newBaseNoFinalizer(ptr)} }

func newOuterNoFinalizer(ptr uint64) *Outer { return &Outer{Mid: newMidNoFinalizer(ptr)} }

func newOuter(ptr uint64) *Outer {
	h := &Outer{
		Mid: newMidNoFinalizer(ptr),
	}
	runtime.SetFinalizer(h, (*Outer).free)
	return h
}

func NewOuterFromImpl(id int32) (*Outer, error) {
	h := newOuter(uint64(id))
	runtime.SetFinalizer(h, nil)
	runtime.SetFinalizer(h, func(h *Outer) { h.free(); UnregisterCallback(id) })
	return h, nil
}

func UnregisterCallback(int32) {}

func (h *Base) free()  {}
func (h *Outer) free() {}

func invokeMethod(svc, mid int32, req []byte) ([]byte, error) { return nil, nil }

func (h *Mid) Set(kind Kind, name string, data []byte, t TypeNode, opts *Options, list []*Base) error {
	_, err := invokeMethod(1, 2, nil)
	return err
}

func NewThing(name string) error {
	_, err := invokeMethod(1, 3, nil)
	return err
}
`

func TestRewriteKeepAlive(t *testing.T) {
	out, err := rewriteKeepAlive([]byte(keepAliveSrc))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		// The receiver and every argument that can hold a handle, not the
		// scalars, right after the call.
		"_, err := invokeMethod(1, 2, nil)\n\truntime.KeepAlive(h)\n\truntime.KeepAlive(t)\n\truntime.KeepAlive(opts)\n\truntime.KeepAlive(list)\n\treturn err",
		// Nothing to keep.
		"_, err := invokeMethod(1, 3, nil)\n\treturn err",
		// Finalizers on the pointer's holder.
		"runtime.SetFinalizer(h.Mid.Base, func(b *Base) { newOuterNoFinalizer(b.ptr).free() })",
		"runtime.SetFinalizer(h.Mid.Base, nil)",
		"runtime.SetFinalizer(h.Mid.Base, func(b *Base) { newOuterNoFinalizer(b.ptr).free(); UnregisterCallback(id) })",
		// A wrapper that holds the pointer keeps its own.
		"runtime.SetFinalizer(h, (*Base).free)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten source lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"KeepAlive(kind)", "KeepAlive(name)", "KeepAlive(data)"} {
		if strings.Contains(got, bad) {
			t.Errorf("rewritten source has %s", bad)
		}
	}
}

// A shape the rewrite does not know fails the build.
func TestRewriteKeepAliveRefusesUnknownShapes(t *testing.T) {
	for name, edit := range map[string][2]string{
		"call not a statement": {"_, err := invokeMethod(1, 3, nil)\n\treturn err", "return func() error { _, err := invokeMethod(1, 3, nil); return err }()"},
		"finalizer not on h":   {"runtime.SetFinalizer(h, (*Outer).free)", "runtime.SetFinalizer(h.Mid, (*Outer).free)"},
		"other finalizer":      {"runtime.SetFinalizer(h, (*Outer).free)", "runtime.SetFinalizer(h, (*Outer).release)"},
		"closure uses h":       {"h.free(); UnregisterCallback(id)", "h.free(); UnregisterCallback(int32(h.ptr))"},
		"no NoFinalizer":       {"func newOuterNoFinalizer(ptr uint64) *Outer { return &Outer{Mid: newMidNoFinalizer(ptr)} }", ""},
	} {
		src := strings.Replace(keepAliveSrc, edit[0], edit[1], 1)
		if src == keepAliveSrc {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, err := rewriteKeepAlive([]byte(src)); err == nil {
			t.Errorf("%s: rewritten without an error", name)
		}
	}
}
