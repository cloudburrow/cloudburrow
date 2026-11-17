package embedding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"

	"golang.org/x/image/webp"
)

// An animated WebP (#1724) is embedded as its first frame, as an animated
// HEIC or HEIF image sequence is (github.com/gen2brain/h265's heic.Decode
// returns a sequence's first frame). Google's multimodal embeddings page
// (re-read 2026-11-17) lists WebP for gemini-embedding-2 and says nothing
// of animation; it documents no refusal, so CloudBurrow does not refuse
// one, and embeds the one picture a still-image reader shows (sharp, and so
// the Transformers.js reference, reads page 0 by default).
//
// golang.org/x/image/webp decodes no animation, so the first ANMF frame's
// bitstream (VP8 or VP8L, with its ALPH chunk) is rewrapped as a still WebP
// and decoded, then placed at its offset on the canvas. The rest of the
// canvas is transparent: the background colour in ANIM is only a hint
// (WebP container specification, "Canvas assembly from frames"), and
// libwebp's WebPAnimDecoder starts from a transparent canvas too.

// webpRIFF is the RIFF header of a WebP file.
func webpRIFF(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

// webpChunk is one RIFF chunk: its FourCC and payload.
type webpChunk struct {
	fourCC string
	data   []byte
}

// webpChunks splits data, a run of RIFF chunks, into chunks. It fails on a
// chunk that runs past the end.
func webpChunks(data []byte) ([]webpChunk, error) {
	var out []webpChunk
	for off := 0; off < len(data); {
		if off+8 > len(data) {
			return nil, errors.New("a chunk header is truncated")
		}
		n := uint64(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		if n > uint64(len(data)-off-8) {
			return nil, fmt.Errorf("the %q chunk is truncated", data[off:off+4])
		}
		out = append(out, webpChunk{string(data[off : off+4]), data[off+8 : off+8+int(n)]})
		off += 8 + int(n) + int(n&1)
	}
	return out, nil
}

// webpAnimated reports whether data is a WebP whose VP8X header sets the
// animation flag or which holds ANIM or ANMF chunks.
func webpAnimated(data []byte) bool {
	if !webpRIFF(data) {
		return false
	}
	for off := 12; off+8 <= len(data); {
		fourCC := string(data[off : off+4])
		n := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		switch fourCC {
		case "ANIM", "ANMF":
			return true
		case "VP8X":
			if off+9 <= len(data) && data[off+8]&(1<<1) != 0 {
				return true
			}
		}
		if n < 0 || n > len(data) {
			return false
		}
		off += 8 + n + n&1
	}
	return false
}

// le24 is a 24-bit little-endian field.
func le24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }

// decodeWebP decodes a still or animated WebP; an animated one gives its
// first frame on the canvas. A lossy picture's colours are libwebp's
// (webpRGB).
func decodeWebP(data []byte) (image.Image, error) {
	if !webpAnimated(data) {
		img, err := webp.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if rgb := webpRGB(img); rgb != nil {
			return rgb, nil
		}
		return img, nil
	}
	return webpFirstFrame(data)
}

// webpFirstFrame is an animated WebP's first frame on its canvas.
func webpFirstFrame(data []byte) (image.Image, error) {
	riffLen := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if riffLen < 4 || riffLen > uint64(len(data)-8) {
		return nil, errors.New("webp: the RIFF size does not match the data")
	}
	chunks, err := webpChunks(data[12 : 8+riffLen])
	if err != nil {
		return nil, fmt.Errorf("webp: %v", err)
	}
	if len(chunks) == 0 || chunks[0].fourCC != "VP8X" || len(chunks[0].data) != 10 {
		return nil, errors.New("webp: an animated WebP must start with a VP8X chunk")
	}
	canvasW, canvasH := le24(chunks[0].data[4:7])+1, le24(chunks[0].data[7:10])+1
	for _, c := range chunks[1:] {
		if c.fourCC != "ANMF" {
			continue
		}
		if len(c.data) < 16 {
			return nil, errors.New("webp: the first ANMF chunk is truncated")
		}
		x, y := 2*le24(c.data[0:3]), 2*le24(c.data[3:6])
		w, h := le24(c.data[6:9])+1, le24(c.data[9:12])+1
		if x+w > canvasW || y+h > canvasH {
			return nil, fmt.Errorf("webp: the first frame (%dx%d at %d,%d) does not fit the %dx%d canvas", w, h, x, y, canvasW, canvasH)
		}
		frame, err := webpChunks(c.data[16:])
		if err != nil {
			return nil, fmt.Errorf("webp: the first frame: %v", err)
		}
		still, err := webpStill(frame, w, h)
		if err != nil {
			return nil, err
		}
		img, err := webp.Decode(bytes.NewReader(still))
		if err != nil {
			return nil, fmt.Errorf("the first frame: %v", err)
		}
		if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
			return nil, fmt.Errorf("webp: the first frame is %dx%d; its ANMF header says %dx%d", b.Dx(), b.Dy(), w, h)
		}
		if rgb := webpRGB(img); rgb != nil {
			img = rgb
		}
		if x == 0 && y == 0 && w == canvasW && h == canvasH {
			return img, nil
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, canvasW, canvasH))
		draw.Draw(canvas, image.Rect(x, y, x+w, y+h), img, img.Bounds().Min, draw.Src)
		return canvas, nil
	}
	return nil, errors.New("webp: the animated WebP has no frame")
}

// webpStill wraps a frame's chunks (ALPH, then VP8 or VP8L) as a still
// WebP file of w x h pixels.
func webpStill(frame []webpChunk, w, h int) ([]byte, error) {
	var alph, bits *webpChunk
	for i := range frame {
		switch frame[i].fourCC {
		case "ALPH":
			if alph == nil && bits == nil {
				alph = &frame[i]
			}
		case "VP8 ", "VP8L":
			if bits == nil {
				bits = &frame[i]
			}
		}
	}
	if bits == nil {
		return nil, errors.New("webp: the first frame has no VP8 or VP8L bitstream")
	}
	var body bytes.Buffer
	body.WriteString("WEBP")
	put := func(c *webpChunk) {
		var hdr [8]byte
		copy(hdr[:4], c.fourCC)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(len(c.data)))
		body.Write(hdr[:])
		body.Write(c.data)
		if len(c.data)&1 == 1 {
			body.WriteByte(0)
		}
	}
	if alph != nil && bits.fourCC == "VP8 " {
		// A lossy frame's alpha needs the extended format.
		vp8x := make([]byte, 10)
		vp8x[0] = 1 << 4 // alpha
		vp8x[4], vp8x[5], vp8x[6] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
		vp8x[7], vp8x[8], vp8x[9] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
		put(&webpChunk{"VP8X", vp8x})
		put(alph)
	}
	put(bits)
	out := make([]byte, 8, 8+body.Len())
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(body.Len()))
	return append(out, body.Bytes()...), nil
}
