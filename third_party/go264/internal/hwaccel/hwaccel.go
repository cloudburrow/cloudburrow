package hwaccel

import "sync"

type EncoderParams struct {
	Width   int
	Height  int
	FPSNum  int
	FPSDen  int
	GOPSize int
	QP      int

	BitrateKbps int
}

const DefaultQP = 26

func ConstantQP(qp int) int {
	if qp <= 0 {
		return DefaultQP
	}
	return min(qp, 51)
}

type Encoder interface {
	Encode(i420 []byte) ([]byte, error)
	Drain() ([]byte, error)
	Close() error
}

type KeyFrameForcer interface {
	ForceKeyFrame()
}

type DecoderParams struct {
	Width  int
	Height int
}

type Decoder interface {
	Decode(annexB []byte) ([]*Picture, error)
	Flush() ([]*Picture, error)
	Close() error
}

type Picture struct {
	Y       []byte
	Cb      []byte
	Cr      []byte
	StrideY int
	StrideC int
	Width   int
	Height  int
}

type Backend struct {
	Name        string
	ProbeEncode func(EncoderParams) (Encoder, bool)
	ProbeDecode func(DecoderParams) (Decoder, bool)
}

var (
	mu       sync.RWMutex
	backends []Backend
	disabled bool
)

func Register(b Backend) {
	mu.Lock()
	defer mu.Unlock()
	for _, have := range backends {
		if have.Name == b.Name {
			return
		}
	}
	backends = append(backends, b)
}

func Disable() {
	mu.Lock()
	defer mu.Unlock()
	disabled = true
}

func Enable() {
	mu.Lock()
	defer mu.Unlock()
	disabled = false
}

func snapshot() []Backend {
	mu.RLock()
	defer mu.RUnlock()
	if disabled {
		return nil
	}
	out := make([]Backend, len(backends))
	copy(out, backends)
	return out
}

func Available() []string {
	var out []string
	for _, b := range snapshot() {
		out = append(out, b.Name)
	}
	return out
}

func OpenEncoder(p EncoderParams) (Encoder, string, bool) {
	for _, b := range snapshot() {
		if b.ProbeEncode == nil {
			continue
		}
		if enc, ok := b.ProbeEncode(p); ok {
			return enc, b.Name, true
		}
	}
	return nil, "", false
}

func OpenDecoder(p DecoderParams) (Decoder, string, bool) {
	for _, b := range snapshot() {
		if b.ProbeDecode == nil {
			continue
		}
		if dec, ok := b.ProbeDecode(p); ok {
			return dec, b.Name, true
		}
	}
	return nil, "", false
}
