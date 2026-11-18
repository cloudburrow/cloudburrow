package mf

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/hwaccel"

const Direct3DBackend = "mediafoundation-d3d11"

const minimumAcceleratedPixels = 640 * 480

func init() {
	hwaccel.Register(hwaccel.Backend{
		Name:        Direct3DBackend,
		ProbeDecode: openDirect3DDecoder,
	})
}

func WorthAccelerating(width, height int) bool {
	return width > 0 && height > 0 && width*height >= minimumAcceleratedPixels
}

func openDirect3DDecoder(p hwaccel.DecoderParams) (hwaccel.Decoder, bool) {
	if !Loaded() || !d3d11Available() {
		return nil, false
	}
	if !WorthAccelerating(p.Width, p.Height) {
		return nil, false
	}
	dec, err := OpenDecoderWithOptions(DecoderOptions{Direct3D: true})
	if err != nil {
		return nil, false
	}
	if !dec.Direct3D() {
		dec.Close()
		return nil, false
	}
	return &direct3DDecoder{dec: dec}, true
}

type direct3DDecoder struct {
	dec *Decoder
}

func (d *direct3DDecoder) pictures(pics []*DecodedPicture) []*hwaccel.Picture {
	if len(pics) == 0 {
		return nil
	}
	out := make([]*hwaccel.Picture, 0, len(pics))
	for _, p := range pics {
		cw, ch := p.Width/2, p.Height/2
		luma := p.Width * p.Height
		out = append(out, &hwaccel.Picture{
			Y:       p.I420[:luma],
			Cb:      p.I420[luma : luma+cw*ch],
			Cr:      p.I420[luma+cw*ch:],
			StrideY: p.Width,
			StrideC: cw,
			Width:   p.Width,
			Height:  p.Height,
		})
	}
	return out
}

func (d *direct3DDecoder) Decode(annexB []byte) ([]*hwaccel.Picture, error) {
	pics, err := d.dec.Decode(annexB)
	if err != nil {
		return nil, err
	}
	return d.pictures(pics), nil
}

func (d *direct3DDecoder) Flush() ([]*hwaccel.Picture, error) {
	pics, err := d.dec.Flush()
	if err != nil {
		return nil, err
	}
	return d.pictures(pics), nil
}

func (d *direct3DDecoder) Close() error { return d.dec.Close() }
