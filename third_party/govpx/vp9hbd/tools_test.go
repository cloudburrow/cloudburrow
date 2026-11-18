package vp9hbd

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file are not checks: they are the tools that made the
// scaled-* and intraonly-* fixtures, by rewriting headers of libvpx streams,
// since libvpx through ffmpeg makes neither reference scaling nor intra-only
// frames. They run only when their environment variable is set.

// TestRewriteResize, with VP9HBD_RESIZE="in.ivf out.ivf idx:WxH,...", gives
// each listed inter frame an explicit frame size (found_ref = 0), so it and
// the frames after it predict from scaled references. The tile data is
// unchanged: the frames decode to deterministic, if odd-looking, pictures.
func TestRewriteResize(t *testing.T) {
	a := strings.Fields(os.Getenv("VP9HBD_RESIZE"))
	if len(a) != 3 {
		t.Skip()
	}
	edits := map[int][2]int{}
	for _, e := range strings.Split(a[2], ",") {
		p := strings.Split(e, ":")
		i, _ := strconv.Atoi(p[0])
		wh := strings.Split(p[1], "x")
		w, _ := strconv.Atoi(wh[0])
		h, _ := strconv.Atoi(wh[1])
		edits[i] = [2]int{w, h}
	}
	raw, _ := os.ReadFile(a[0])
	hl := int(binary.LittleEndian.Uint16(raw[6:]))
	out := append([]byte(nil), raw[:hl]...)
	d := NewDecoder()
	for i, fr := range readIVF(t, a[0]) {
		if e, ok := edits[i]; ok {
			tmp := *d
			r := &bitReader{data: fr}
			if err := tmp.readUncompressedHeader(r); err != nil {
				t.Fatal(err)
			}
			end := r.pos
			// prefix: marker 2, profile 2 (+1 if 3), show_existing, type, show, er
			p := &bitReader{data: fr}
			p.f(2)
			prof := p.f(1) | p.f(1)<<1
			if prof == 3 {
				p.f(1)
			}
			p.f(1)
			p.f(1)
			show := p.f(1)
			er := p.f(1)
			if show == 0 && p.f(1) == 1 {
				t.Fatal("intra-only")
			}
			if er == 0 {
				p.f(2)
			}
			p.f(8)
			p.f(12)
			start := p.pos
			found := false
			for range 3 {
				if p.f(1) == 1 {
					found = true
					break
				}
			}
			if !found {
				p.f(32)
			}
			if p.f(1) == 1 {
				p.f(32)
			}
			rest := p.pos
			var bits []int
			get := func(k int) int { return int(fr[k>>3]>>(7-uint(k&7))) & 1 }
			for k := range start {
				bits = append(bits, get(k))
			}
			put := func(v, n int) {
				for k := n - 1; k >= 0; k-- {
					bits = append(bits, v>>uint(k)&1)
				}
			}
			put(0, 3)
			put(e[0]-1, 16)
			put(e[1]-1, 16)
			put(0, 1)
			for k := rest; k < end; k++ {
				bits = append(bits, get(k))
			}
			for len(bits)%8 != 0 {
				bits = append(bits, 0)
			}
			nb := make([]byte, len(bits)/8)
			for k, v := range bits {
				nb[k>>3] |= byte(v << (7 - uint(k&7)))
			}
			fr = append(nb, fr[(end+7)>>3:]...)
		}
		if _, err := d.Decode(fr); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		var hdr [12]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(fr)))
		binary.LittleEndian.PutUint64(hdr[4:], uint64(i))
		out = append(out, hdr[:]...)
		out = append(out, fr...)
	}
	os.WriteFile(a[1], out, 0o644)
}

// TestRewriteIntraOnly, with VP9HBD_INTRAONLY="in.ivf out.ivf idx reset",
// rewrites keyframe idx as a hidden intra-only frame (reset_frame_context
// reset, refresh_frame_flags 0xff) followed, in the same superframe, by a
// show_existing_frame of slot 0.
func TestRewriteIntraOnly(t *testing.T) {
	a := strings.Fields(os.Getenv("VP9HBD_INTRAONLY"))
	if len(a) != 4 {
		t.Skip()
	}
	idx, _ := strconv.Atoi(a[2])
	reset, _ := strconv.Atoi(a[3])
	raw, _ := os.ReadFile(a[0])
	hl := int(binary.LittleEndian.Uint16(raw[6:]))
	out := append([]byte(nil), raw[:hl]...)
	d := NewDecoder()
	for i, fr := range readIVF(t, a[0]) {
		if i == idx {
			tmp := *d
			r := &bitReader{data: fr}
			if err := tmp.readUncompressedHeader(r); err != nil {
				t.Fatal(err)
			}
			end := r.pos
			p := &bitReader{data: fr}
			get := func(k int) int { return int(fr[k>>3]>>(7-uint(k&7))) & 1 }
			var bits []int
			put := func(v, n int) {
				for k := n - 1; k >= 0; k-- {
					bits = append(bits, v>>uint(k)&1)
				}
			}
			copyTo := func(e int) {
				for k := p.pos; k < e; k++ {
					bits = append(bits, get(k))
				}
				p.pos = e
			}
			p.f(2)
			prof := p.f(1) | p.f(1)<<1
			if prof == 3 {
				p.f(1)
			}
			copyTo(0)
			bits = nil
			p.pos = 0
			copyTo(2 + 2 + btoi(prof == 3))
			if p.f(1) != 0 || p.f(1) != 0 {
				t.Fatal("not a keyframe")
			}
			p.f(1) // show_frame
			er := p.f(1)
			put(0, 1) // show_existing_frame
			put(1, 1) // frame_type
			put(0, 1) // show_frame
			put(er, 1)
			put(1, 1) // intra_only
			if er == 0 {
				put(reset, 2)
			}
			copyTo(p.pos + 24) // sync code
			ccStart := p.pos
			if prof >= 2 {
				p.f(1)
			}
			if p.f(3) != 7 {
				p.f(1)
				if prof == 1 || prof == 3 {
					p.f(3)
				}
			} else if prof == 1 || prof == 3 {
				p.f(1)
			}
			if prof > 0 {
				e := p.pos
				p.pos = ccStart
				copyTo(e)
			}
			put(0xff, 8) // refresh_frame_flags
			copyTo(end)
			for len(bits)%8 != 0 {
				bits = append(bits, 0)
			}
			nb := make([]byte, len(bits)/8)
			for k, v := range bits {
				nb[k>>3] |= byte(v << (7 - uint(k&7)))
			}
			hidden := append(nb, fr[(end+7)>>3:]...)
			// show_existing_frame of slot 0
			var se []int
			se = append(se, 1, 0, prof&1, prof>>1)
			if prof == 3 {
				se = append(se, 0)
			}
			se = append(se, 1, 0, 0, 0)
			var show []byte
			for len(se)%8 != 0 {
				se = append(se, 0)
			}
			for k := 0; k < len(se); k += 8 {
				var v byte
				for j := range 8 {
					v |= byte(se[k+j] << (7 - j))
				}
				show = append(show, v)
			}
			fr = append(append([]byte(nil), hidden...), show...)
			fr = append(fr, 0xd9)
			for _, n := range []int{len(hidden), len(show)} {
				fr = binary.LittleEndian.AppendUint32(fr, uint32(n))
			}
			fr = append(fr, 0xd9)
		}
		if _, err := d.Decode(fr); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		var hdr [12]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(fr)))
		binary.LittleEndian.PutUint64(hdr[4:], uint64(i))
		out = append(out, hdr[:]...)
		out = append(out, fr...)
	}
	os.WriteFile(a[1], out, 0o644)
}
