package embedding

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"

	"github.com/gen2brain/gav1d/avif"
)

// AVIF images (#1729) are decoded with github.com/gen2brain/gav1d (#1736):
// a pure Go port of dav1d with an AVIF reader, BSD 2-Clause, no cgo, no
// WebAssembly and no dependencies, so the Linux CLI stays statically linked
// and every AVIF Google takes decodes, 8 to 12 bits up to 16384 x 16384.
//
// The picture is libavif's (with libyuv, its default build): an 8-bit
// image's YUV is converted here, 4:2:0 and 4:2:2 chroma upsampled
// nearest-neighbour as libyuv does for libavif's default (AUTOMATIC)
// upsampling; a 10- or 12-bit image is gav1d's 16-bit RGB, whose 4:2:0 and
// 4:2:2 chroma is upsampled bilinearly, as libavif's own converter does.
// The primary item's clean aperture, rotation and mirror (clap, irot, imir)
// are applied in that order (MIAF 7.3.6.7), as libheif's heif-dec and
// ImageMagick apply them, as for HEIC (heic.go). An image sequence (AVIS)
// gives its first frame, as a HEIC sequence or an animated WebP does
// (webpanim.go).

// avifOptions bound a frame at Google's largest image.
var avifOptions = avif.Options{ToYCbCr: true, FrameSizeLimit: MaxImageSide * MaxImageSide}

func avifConfig(r io.Reader) (image.Config, error) {
	return avif.DecodeConfig(r)
}

func avifDecode(r io.Reader) (img image.Image, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// Guard the decoder, so no input can take the server down.
	defer func() {
		if p := recover(); p != nil {
			img, err = nil, fmt.Errorf("the AVIF decoder failed: %v", p)
		}
	}()
	img, ci, err := avif.DecodeColor(bytes.NewReader(data), avifOptions)
	if err != nil {
		return nil, err
	}
	if b := img.Bounds(); b.Dx() <= 0 || b.Dy() <= 0 || b.Dx() > MaxImageSide || b.Dy() > MaxImageSide {
		return nil, fmt.Errorf("the image is %dx%d; each side must be 1 to %d pixels", b.Dx(), b.Dy(), MaxImageSide)
	}
	img = avifRGB(img, ci)
	return avifTransform(img, avifPrimaryProps(data))
}

// avifRGB converts gav1d's native 8-bit planes to RGB as libyuv does for
// libavif: nearest-neighbour chroma, the matrix and range the file signals
// (unspecified is BT.601, as libavif reads it). A deep image is already RGB.
func avifRGB(img image.Image, ci avif.ColorInfo) image.Image {
	var (
		yc    *image.YCbCr
		alpha []uint8
		astr  int
	)
	switch m := img.(type) {
	case *image.YCbCr:
		yc = m
	case *image.NYCbCrA:
		yc, alpha, astr = &m.YCbCr, m.A, m.AStride
	case *image.Gray:
		b := m.Bounds()
		out := image.NewNRGBA(b)
		conv := newAVIFConv(ci)
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				r, g, bb := conv.rgb(m.Pix[y*m.Stride+x], 128, 128)
				o := out.Pix[y*out.Stride+4*x:]
				o[0], o[1], o[2], o[3] = r, g, bb, 255
			}
		}
		return out
	default: // *image.NRGBA64 above 8 bits
		return img
	}
	b := yc.Bounds()
	out := image.NewNRGBA(b)
	conv := newAVIFConv(ci)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			yi, ci := yc.YOffset(x, y), yc.COffset(x, y)
			r, g, bb := conv.rgb(yc.Y[yi], yc.Cb[ci], yc.Cr[ci])
			o := out.Pix[(y-b.Min.Y)*out.Stride+4*(x-b.Min.X):]
			o[0], o[1], o[2], o[3] = r, g, bb, 255
			if alpha != nil {
				o[3] = alpha[(y-b.Min.Y)*astr+(x-b.Min.X)]
			}
		}
	}
	return out
}

// avifConv is one file's YUV to RGB conversion.
type avifConv struct {
	identity, ycgco bool
	full            bool
	kr, kb          float64
}

func newAVIFConv(ci avif.ColorInfo) avifConv {
	c := avifConv{full: ci.FullRange, kr: 0.299, kb: 0.114}
	switch ci.Matrix {
	case 0:
		c.identity = true
	case 1:
		c.kr, c.kb = 0.2126, 0.0722
	case 4:
		c.kr, c.kb = 0.30, 0.11
	case 7:
		c.kr, c.kb = 0.212, 0.087
	case 8:
		c.ycgco = true
	case 9, 10:
		c.kr, c.kb = 0.2627, 0.0593
	}
	return c
}

func (c avifConv) rgb(y8, cb8, cr8 uint8) (uint8, uint8, uint8) {
	yy, cb, cr := float64(y8), float64(cb8)-128, float64(cr8)-128
	if !c.full {
		yy, cb, cr = (yy-16)*255/219, cb*255/224, cr*255/224
	}
	var r, g, b float64
	switch {
	case c.identity:
		g, b, r = float64(y8), float64(cb8), float64(cr8)
		if !c.full {
			g, b, r = (g-16)*255/219, (b-16)*255/219, (r-16)*255/219
		}
	case c.ycgco:
		t := yy - cb
		g, b, r = yy+cb, t-cr, t+cr
	default:
		kg := 1 - c.kr - c.kb
		r = yy + 2*(1-c.kr)*cr
		b = yy + 2*(1-c.kb)*cb
		g = yy - (2*c.kr*(1-c.kr)*cr+2*c.kb*(1-c.kb)*cb)/kg
	}
	return clamp8(r), clamp8(g), clamp8(b)
}

func clamp8(v float64) uint8 {
	return uint8(math.Round(min(max(v, 0), 255)))
}

// avifProps are the transformative properties of the primary item.
type avifProps struct {
	clap       []byte // 32 bytes: width, height, horizontal and vertical offset, each N/D
	irot, imir int    // -1 when absent
}

// avifPrimaryProps reads the primary item's clap, irot and imir from the
// meta box. A file it cannot follow has none: the decoder has already parsed
// and decoded it, so the picture stands untransformed.
func avifPrimaryProps(data []byte) avifProps {
	p := avifProps{irot: -1, imir: -1}
	meta := isoBox(data, "meta")
	if len(meta) < 4 {
		return p
	}
	meta = meta[4:] // full box
	primary := -1
	if pitm := isoBox(meta, "pitm"); len(pitm) >= 6 {
		if pitm[0] == 0 {
			primary = int(binary.BigEndian.Uint16(pitm[4:]))
		} else if len(pitm) >= 8 {
			primary = int(binary.BigEndian.Uint32(pitm[4:]))
		}
	}
	iprp := isoBox(meta, "iprp")
	ipco, ipma := isoBox(iprp, "ipco"), isoBox(iprp, "ipma")
	if primary < 0 || ipco == nil || len(ipma) < 8 {
		return p
	}
	var props [][2][]byte // type, payload
	isoBoxes(ipco, func(typ string, payload []byte) bool {
		props = append(props, [2][]byte{[]byte(typ), payload})
		return true
	})
	version, flags := ipma[0], ipma[3]
	n := int(binary.BigEndian.Uint32(ipma[4:]))
	off := 8
	for e := 0; e < n; e++ {
		var id int
		if version < 1 {
			if off+2 > len(ipma) {
				return p
			}
			id, off = int(binary.BigEndian.Uint16(ipma[off:])), off+2
		} else {
			if off+4 > len(ipma) {
				return p
			}
			id, off = int(binary.BigEndian.Uint32(ipma[off:])), off+4
		}
		if off >= len(ipma) {
			return p
		}
		count := int(ipma[off])
		off++
		for a := 0; a < count; a++ {
			var idx int
			if flags&1 != 0 {
				if off+2 > len(ipma) {
					return p
				}
				idx, off = int(binary.BigEndian.Uint16(ipma[off:])&0x7fff), off+2
			} else {
				if off >= len(ipma) {
					return p
				}
				idx, off = int(ipma[off]&0x7f), off+1
			}
			if id != primary || idx < 1 || idx > len(props) {
				continue
			}
			typ, payload := string(props[idx-1][0]), props[idx-1][1]
			switch {
			case typ == "clap" && len(payload) >= 32:
				p.clap = payload[:32]
			case typ == "irot" && len(payload) >= 1:
				p.irot = int(payload[0] & 3)
			case typ == "imir" && len(payload) >= 1:
				p.imir = int(payload[0] & 1)
			}
		}
	}
	return p
}

// isoBoxes calls fn with each box in b, until it returns false.
func isoBoxes(b []byte, fn func(typ string, payload []byte) bool) {
	for off := 0; off+8 <= len(b); {
		size := uint64(binary.BigEndian.Uint32(b[off:]))
		typ := string(b[off+4 : off+8])
		hdr := uint64(8)
		switch size {
		case 0:
			size = uint64(len(b) - off)
		case 1:
			if off+16 > len(b) {
				return
			}
			size, hdr = binary.BigEndian.Uint64(b[off+8:]), 16
		}
		if size < hdr || size > uint64(len(b)-off) {
			return
		}
		if !fn(typ, b[off+int(hdr):off+int(size)]) {
			return
		}
		off += int(size)
	}
}

// isoBox is the payload of the first box of type typ in b, or nil.
func isoBox(b []byte, typ string) []byte {
	var out []byte
	isoBoxes(b, func(t string, payload []byte) bool {
		if t == typ {
			out = payload
			return false
		}
		return true
	})
	return out
}

// avifCrop is the clean aperture's rectangle in a w x h picture, as
// libavif's avifCropRectFromCleanApertureBox derives it: the aperture is
// centred on (w-1)/2 + horizOff, (h-1)/2 + vertOff, and must be whole
// pixels inside the picture. An invalid one is not applied, as libavif
// reports it invalid and heif-dec does not crop.
func avifCrop(clap []byte, w, h int) (image.Rectangle, bool) {
	u := func(i int) int64 { return int64(binary.BigEndian.Uint32(clap[4*i:])) }
	s := func(i int) int64 { return int64(int32(binary.BigEndian.Uint32(clap[4*i:]))) }
	wN, wD, hN, hD := u(0), u(1), u(2), u(3)
	xN, xD, yN, yD := s(4), u(5), s(6), u(7)
	if wD == 0 || hD == 0 || xD == 0 || yD == 0 || wN%wD != 0 || hN%hD != 0 {
		return image.Rectangle{}, false
	}
	cw, ch := wN/wD, hN/hD
	// left = (w - cw)/2 + xN/xD, which must be an integer: in units of 2*xD.
	left2 := (int64(w)-cw)*xD + 2*xN
	top2 := (int64(h)-ch)*yD + 2*yN
	if left2%(2*xD) != 0 || top2%(2*yD) != 0 {
		return image.Rectangle{}, false
	}
	left, top := left2/(2*xD), top2/(2*yD)
	if cw <= 0 || ch <= 0 || left < 0 || top < 0 || left+cw > int64(w) || top+ch > int64(h) {
		return image.Rectangle{}, false
	}
	return image.Rect(int(left), int(top), int(left+cw), int(top+ch)), true
}

// avifTransform applies p to img: crop, then rotate irot x 90 degrees
// anticlockwise, then mirror (imir 0 exchanges top and bottom, 1 left and
// right, as ISO/IEC 23008-12:2022 and libavif define it).
func avifTransform(img image.Image, p avifProps) (image.Image, error) {
	if p.clap == nil && p.irot <= 0 && p.imir < 0 {
		return img, nil
	}
	src := img
	if p.clap != nil {
		b := img.Bounds()
		if r, ok := avifCrop(p.clap, b.Dx(), b.Dy()); ok {
			src = cropImage(img, r.Add(b.Min))
		}
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if p.irot == 1 || p.irot == 3 {
		dw, dh = h, w
	}
	dst := newLike(img, dw, dh)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Rotate (x, y) anticlockwise irot quarter turns.
			dx, dy := x, y
			switch p.irot {
			case 1:
				dx, dy = y, w-1-x
			case 2:
				dx, dy = w-1-x, h-1-y
			case 3:
				dx, dy = h-1-y, x
			}
			switch p.imir {
			case 0:
				dy = dh - 1 - dy
			case 1:
				dx = dw - 1 - dx
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst, nil
}

// cropImage is the part r of img, copied.
func cropImage(img image.Image, r image.Rectangle) image.Image {
	dst := newLike(img, r.Dx(), r.Dy())
	draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
	return dst
}

// newLike is a w x h image of img's kind: 16 bits a channel for a deep
// image, 8 otherwise, both unpremultiplied as the decoder's are.
func newLike(img image.Image, w, h int) draw.Image {
	if _, deep := img.(*image.NRGBA64); deep {
		return image.NewNRGBA64(image.Rect(0, 0, w, h))
	}
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}
