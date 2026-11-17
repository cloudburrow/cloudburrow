package frame

const LumaMargin = 32

const ChromaMargin = LumaMargin / 2

type Picture struct {
	Y       []byte
	Cb      []byte
	Cr      []byte
	StrideY int
	StrideC int
	Width   int
	Height  int

	CropX      int
	CropY      int
	CropWidth  int
	CropHeight int

	POC      int
	FrameNum uint32
	IDR      bool
	LongTerm bool

	Motion *Motion
}

func NewPicture(widthMBs, heightMBs int) *Picture {
	w := widthMBs * 16
	h := heightMBs * 16
	strideY := w + 2*LumaMargin
	strideC := w/2 + 2*ChromaMargin
	p := &Picture{
		StrideY:    strideY,
		StrideC:    strideC,
		Width:      w,
		Height:     h,
		CropWidth:  w,
		CropHeight: h,
	}
	p.Y = make([]byte, strideY*(h+2*LumaMargin))
	p.Cb = make([]byte, strideC*(h/2+2*ChromaMargin))
	p.Cr = make([]byte, strideC*(h/2+2*ChromaMargin))
	return p
}

func (p *Picture) OriginY() int { return LumaMargin*p.StrideY + LumaMargin }

func (p *Picture) OriginC() int { return ChromaMargin*p.StrideC + ChromaMargin }

func (p *Picture) LumaOffset(x, y int) int { return p.OriginY() + y*p.StrideY + x }

func (p *Picture) ChromaOffset(x, y int) int { return p.OriginC() + y*p.StrideC + x }

func (p *Picture) Size() int { return p.Width * p.Height * 3 / 2 }

func (p *Picture) CopyOut(dst []byte) {
	n := 0
	for y := 0; y < p.Height; y++ {
		copy(dst[n:n+p.Width], p.Y[p.LumaOffset(0, y):])
		n += p.Width
	}
	cw := p.Width / 2
	ch := p.Height / 2
	for _, plane := range [][]byte{p.Cb, p.Cr} {
		for y := 0; y < ch; y++ {
			copy(dst[n:n+cw], plane[p.ChromaOffset(0, y):])
			n += cw
		}
	}
}

func (p *Picture) CopyIn(src []byte) {
	n := 0
	for y := 0; y < p.Height; y++ {
		copy(p.Y[p.LumaOffset(0, y):p.LumaOffset(0, y)+p.Width], src[n:n+p.Width])
		n += p.Width
	}
	cw := p.Width / 2
	ch := p.Height / 2
	for _, plane := range [][]byte{p.Cb, p.Cr} {
		for y := 0; y < ch; y++ {
			copy(plane[p.ChromaOffset(0, y):p.ChromaOffset(0, y)+cw], src[n:n+cw])
			n += cw
		}
	}
}

func extendPlane(plane []byte, stride, origin, width, height, margin int) {
	for y := 0; y < height; y++ {
		row := origin + y*stride
		left := plane[row]
		right := plane[row+width-1]
		for i := 1; i <= margin; i++ {
			plane[row-i] = left
			plane[row+width-1+i] = right
		}
	}
	top := origin - margin
	bottom := origin + (height-1)*stride - margin
	rowLen := width + 2*margin
	for i := 1; i <= margin; i++ {
		copy(plane[top-i*stride:top-i*stride+rowLen], plane[top:top+rowLen])
		copy(plane[bottom+i*stride:bottom+i*stride+rowLen], plane[bottom:bottom+rowLen])
	}
}

func (p *Picture) ExtendBorders() {
	extendPlane(p.Y, p.StrideY, p.OriginY(), p.Width, p.Height, LumaMargin)
	extendPlane(p.Cb, p.StrideC, p.OriginC(), p.Width/2, p.Height/2, ChromaMargin)
	extendPlane(p.Cr, p.StrideC, p.OriginC(), p.Width/2, p.Height/2, ChromaMargin)
}

type Motion struct {
	BlocksWide int
	BlocksHigh int
	Mv         [2][][2]int16
	RefIdx     [2][]int8
	RefPOC     [2][]int32
}

func NewMotion(widthMBs, heightMBs int) *Motion {
	w := widthMBs * 4
	h := heightMBs * 4
	m := &Motion{BlocksWide: w, BlocksHigh: h}
	for l := 0; l < 2; l++ {
		m.Mv[l] = make([][2]int16, w*h)
		m.RefIdx[l] = make([]int8, w*h)
		m.RefPOC[l] = make([]int32, w*h)
		for i := range m.RefIdx[l] {
			m.RefIdx[l][i] = -1
		}
	}
	return m
}

func (m *Motion) Index(bx, by int) int { return by*m.BlocksWide + bx }
