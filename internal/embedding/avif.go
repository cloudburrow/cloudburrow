package embedding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/avif"
)

// AVIF images (#1729) are decoded with libavif 1.4.2 and dav1d, compiled to
// WebAssembly by github.com/gen2brain/avif v0.6.0 and run on wazero: its
// WASM decode path is vendored in internal/avif without the purego loader
// of a system libavif, so the build stays cgo-free and the Linux CLI
// statically linked (third_party/avif/PROVENANCE.md).
//
// The picture is the one a viewer shows, as for HEIC (heic.go): the primary
// item's clean aperture, rotation and mirror (clap, irot, imir) are applied
// in that order (MIAF 7.3.6.7), as libheif's heif-dec and ImageMagick apply
// them; libavif itself only reports them. An image sequence (AVIS) gives its
// first frame, as a HEIC sequence or an animated WebP does (webpanim.go).

// ErrImageTooLarge is a valid image this build cannot decode for its size:
// a 10- or 12-bit AVIF larger than avifMaxDeepPixels (#1729). Google
// decodes it, so it is UNIMPLEMENTED, not INVALID_ARGUMENT.
var ErrImageTooLarge = errors.New("the image is too large for this build's decoder")

// avifMaxDeepPixels is the largest 10- or 12-bit AVIF decoded, measured on
// 2026-11-17: libavif in WebAssembly has 4 GiB of memory, which holds the
// 16-bit RGB of 13312 x 13312 pixels (the glue's copy and libavif's own)
// but not of 14336 x 14336. 8-bit AVIFs decode up to Google's 16384 x 16384.
const avifMaxDeepPixels = 13312 * 13312

func avifConfig(r io.Reader) (image.Config, error) {
	cfg, err := avif.DecodeConfig(r)
	if err != nil {
		return image.Config{}, avifError(err)
	}
	return cfg, nil
}

func avifDecode(r io.Reader) (img image.Image, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// The decoder runs in a WebAssembly sandbox; a trap is an error, but
	// guard the glue too, so no input can take the server down.
	defer func() {
		if p := recover(); p != nil {
			img, err = nil, fmt.Errorf("the AVIF decoder failed: %v", p)
		}
	}()
	img, err = avif.Decode(bytes.NewReader(data))
	if err != nil {
		if cfg, cerr := avif.DecodeConfig(bytes.NewReader(data)); cerr == nil &&
			cfg.ColorModel == color.RGBA64Model && cfg.Width*cfg.Height > avifMaxDeepPixels {
			return nil, fmt.Errorf("%w: a %dx%d AVIF of more than 8 bits a channel needs more than the 4 GiB of WebAssembly memory its decoder has (up to 13312x13312 pixels decode)",
				ErrImageTooLarge, cfg.Width, cfg.Height)
		}
		return nil, avifError(err)
	}
	if b := img.Bounds(); b.Dx() <= 0 || b.Dy() <= 0 || b.Dx() > MaxImageSide || b.Dy() > MaxImageSide {
		return nil, fmt.Errorf("the image is %dx%d; each side must be 1 to %d pixels", b.Dx(), b.Dy(), MaxImageSide)
	}
	return avifTransform(img, avifPrimaryProps(data))
}

// avifError rewords the decoder's errors, which say only "decode failed".
func avifError(err error) error {
	if err == avif.ErrDecode {
		return fmt.Errorf("libavif could not parse or decode it")
	}
	return err
}

// avifProps are the transformative properties of the primary item.
type avifProps struct {
	clap       []byte // 32 bytes: width, height, horizontal and vertical offset, each N/D
	irot, imir int    // -1 when absent
}

// avifPrimaryProps reads the primary item's clap, irot and imir from the
// meta box. A file it cannot follow has none: libavif has already parsed
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
// image, 8 otherwise, both premultiplied as the decoder's are.
func newLike(img image.Image, w, h int) draw.Image {
	if _, deep := img.(*image.RGBA64); deep {
		return image.NewRGBA64(image.Rect(0, 0, w, h))
	}
	return image.NewRGBA(image.Rect(0, 0, w, h))
}
