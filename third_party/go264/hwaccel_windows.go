package go264

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/hwaccel"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/hwaccel/mf"
)

const mediaFoundationBackend = "mediafoundation"

func init() {
	hwaccel.Register(hwaccel.Backend{
		Name:        mediaFoundationBackend,
		ProbeEncode: openMediaFoundationEncoder,
	})
}

type mediaFoundationEncoder struct {
	enc *mf.Encoder
}

func defaultBitrateBitsPerSecond(p hwaccel.EncoderParams) int {
	if p.BitrateKbps > 0 {
		return p.BitrateKbps * 1000
	}
	num, den := p.FPSNum, p.FPSDen
	if num <= 0 || den <= 0 {
		num, den = 30, 1
	}
	pixels := p.Width * p.Height
	rate := pixels * num / den / 10
	if rate < 100000 {
		rate = 100000
	}
	return rate
}

func openMediaFoundationEncoder(p hwaccel.EncoderParams) (hwaccel.Encoder, bool) {
	if !mf.Loaded() {
		return nil, false
	}
	num, den := p.FPSNum, p.FPSDen
	if num <= 0 || den <= 0 {
		num, den = 30, 1
	}
	format := mf.EncoderFormat{
		Width:                p.Width,
		Height:               p.Height,
		FPSNum:               num,
		FPSDen:               den,
		BitrateBitsPerSecond: defaultBitrateBitsPerSecond(p),
		Profile:              mf.AVEncH264VProfileMain,
	}
	if p.BitrateKbps <= 0 {
		format.QP = hwaccel.ConstantQP(p.QP)
	}
	enc, err := mf.OpenEncoder(format, true)
	if err != nil {
		return nil, false
	}
	return &mediaFoundationEncoder{enc: enc}, true
}

func joinPackets(packets [][]byte) []byte {
	n := 0
	for _, p := range packets {
		n += len(p)
	}
	if n == 0 {
		return nil
	}
	out := make([]byte, 0, n)
	for _, p := range packets {
		out = append(out, p...)
	}
	return out
}

func (e *mediaFoundationEncoder) Encode(i420 []byte) ([]byte, error) {
	packets, err := e.enc.Encode(i420)
	if err != nil {
		return nil, err
	}
	return joinPackets(packets), nil
}

func (e *mediaFoundationEncoder) Drain() ([]byte, error) {
	packets, err := e.enc.Drain()
	if err != nil {
		return nil, err
	}
	return joinPackets(packets), nil
}

func (e *mediaFoundationEncoder) Close() error {
	return e.enc.Close()
}
