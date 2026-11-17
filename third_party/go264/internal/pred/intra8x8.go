package pred

const (
	I8x8Vertical = iota
	I8x8Horizontal
	I8x8DC
	I8x8DiagonalDownLeft
	I8x8DiagonalDownRight
	I8x8VerticalRight
	I8x8HorizontalDown
	I8x8VerticalLeft
	I8x8HorizontalUp
)

func Intra8x8ModeAvailable(mode int, avail Availability) bool {
	return Intra4x4ModeAvailable(mode, avail)
}

type refs8x8 struct {
	top  [16]int
	left [8]int
	tl   int
}

func gather8x8(plane []byte, stride, offset int, avail Availability) refs8x8 {
	p := func(x, y int) int {
		return int(plane[offset+y*stride+x])
	}
	haveTop := avail.Has(AvailTop)
	haveLeft := avail.Has(AvailLeft)
	haveTopLeft := avail.Has(AvailTopLeft)
	haveTopRight := avail.Has(AvailTopRight)

	var tRaw [16]int
	if haveTop {
		for i := 0; i < 8; i++ {
			tRaw[i] = p(i, -1)
		}
		if haveTopRight {
			for i := 8; i < 16; i++ {
				tRaw[i] = p(i, -1)
			}
		}
	}
	var lRaw [8]int
	if haveLeft {
		for i := 0; i < 8; i++ {
			lRaw[i] = p(-1, i)
		}
	}
	tlRaw := 0
	if haveTopLeft {
		tlRaw = p(-1, -1)
	}

	var r refs8x8
	r.tl = (lRaw[0] + 2*tlRaw + tRaw[0] + 2) >> 2

	first := lRaw[0]
	if haveTopLeft {
		first = tlRaw
	}
	r.left[0] = (first + 2*lRaw[0] + lRaw[1] + 2) >> 2
	for y := 1; y < 7; y++ {
		r.left[y] = (lRaw[y-1] + 2*lRaw[y] + lRaw[y+1] + 2) >> 2
	}
	r.left[7] = (lRaw[6] + 3*lRaw[7] + 2) >> 2

	first = tRaw[0]
	if haveTopLeft {
		first = tlRaw
	}
	r.top[0] = (first + 2*tRaw[0] + tRaw[1] + 2) >> 2
	for x := 1; x < 7; x++ {
		r.top[x] = (tRaw[x-1] + 2*tRaw[x] + tRaw[x+1] + 2) >> 2
	}
	last := tRaw[7]
	if haveTopRight {
		last = tRaw[8]
	}
	r.top[7] = (last + 2*tRaw[7] + tRaw[6] + 2) >> 2

	if haveTopRight {
		for x := 8; x < 15; x++ {
			r.top[x] = (tRaw[x-1] + 2*tRaw[x] + tRaw[x+1] + 2) >> 2
		}
		r.top[15] = (tRaw[14] + 3*tRaw[15] + 2) >> 2
	} else {
		for x := 8; x < 16; x++ {
			r.top[x] = tRaw[7]
		}
	}
	return r
}

func Intra8x8(plane []byte, stride, offset, mode int, avail Availability) {
	r := gather8x8(plane, stride, offset, avail)
	t, l, lt := &r.top, &r.left, r.tl

	topAt := func(i int) int {
		if i < 0 {
			return lt
		}
		return t[i]
	}
	leftAt := func(i int) int {
		if i < 0 {
			return lt
		}
		return l[i]
	}
	set := func(x, y, v int) {
		plane[offset+y*stride+x] = clip1(v)
	}

	switch mode {
	case I8x8Vertical:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				set(x, y, t[x])
			}
		}
	case I8x8Horizontal:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				set(x, y, l[y])
			}
		}
	case I8x8DC:
		haveTop := avail.Has(AvailTop)
		haveLeft := avail.Has(AvailLeft)
		var dc int
		switch {
		case haveTop && haveLeft:
			sum := 8
			for i := 0; i < 8; i++ {
				sum += t[i] + l[i]
			}
			dc = sum >> 4
		case haveLeft:
			sum := 4
			for i := 0; i < 8; i++ {
				sum += l[i]
			}
			dc = sum >> 3
		case haveTop:
			sum := 4
			for i := 0; i < 8; i++ {
				sum += t[i]
			}
			dc = sum >> 3
		default:
			dc = 128
		}
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				set(x, y, dc)
			}
		}
	case I8x8DiagonalDownLeft:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				var v int
				if x == 7 && y == 7 {
					v = (t[14] + 3*t[15] + 2) >> 2
				} else {
					s := x + y
					v = (t[s] + 2*t[s+1] + t[s+2] + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I8x8DiagonalDownRight:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				var v int
				switch {
				case x > y:
					d := x - y
					v = (topAt(d-2) + 2*topAt(d-1) + topAt(d) + 2) >> 2
				case x < y:
					d := y - x
					v = (leftAt(d-2) + 2*leftAt(d-1) + leftAt(d) + 2) >> 2
				default:
					v = (l[0] + 2*lt + t[0] + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I8x8VerticalRight:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				zVR := 2*x - y
				var v int
				switch {
				case zVR >= 0 && zVR&1 == 0:
					i := x - (y >> 1)
					v = (topAt(i-1) + topAt(i) + 1) >> 1
				case zVR >= 0:
					i := x - (y >> 1)
					v = (topAt(i-2) + 2*topAt(i-1) + topAt(i) + 2) >> 2
				case zVR == -1:
					v = (l[0] + 2*lt + t[0] + 2) >> 2
				default:
					d := y - 2*x
					v = (leftAt(d-1) + 2*leftAt(d-2) + leftAt(d-3) + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I8x8HorizontalDown:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				zHD := 2*y - x
				var v int
				switch {
				case zHD >= 0 && zHD&1 == 0:
					i := y - (x >> 1)
					v = (leftAt(i-1) + leftAt(i) + 1) >> 1
				case zHD >= 0:
					i := y - (x >> 1)
					v = (leftAt(i-2) + 2*leftAt(i-1) + leftAt(i) + 2) >> 2
				case zHD == -1:
					v = (l[0] + 2*lt + t[0] + 2) >> 2
				default:
					d := x - 2*y
					v = (topAt(d-1) + 2*topAt(d-2) + topAt(d-3) + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I8x8VerticalLeft:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				i := x + (y >> 1)
				var v int
				if y&1 == 0 {
					v = (t[i] + t[i+1] + 1) >> 1
				} else {
					v = (t[i] + 2*t[i+1] + t[i+2] + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I8x8HorizontalUp:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				zHU := x + 2*y
				i := y + (x >> 1)
				var v int
				switch {
				case zHU < 13 && zHU&1 == 0:
					v = (l[i] + l[i+1] + 1) >> 1
				case zHU < 13:
					v = (l[i] + 2*l[i+1] + l[i+2] + 2) >> 2
				case zHU == 13:
					v = (l[6] + 3*l[7] + 2) >> 2
				default:
					v = l[7]
				}
				set(x, y, v)
			}
		}
	}
}
