package pred

func Intra4x4(plane []byte, stride, offset, mode int, avail Availability) {
	p := func(x, y int) int {
		return int(plane[offset+y*stride+x])
	}

	haveTop := avail.Has(AvailTop)
	haveLeft := avail.Has(AvailLeft)
	haveTopLeft := avail.Has(AvailTopLeft)
	haveTopRight := avail.Has(AvailTopRight)

	var top [8]int
	if haveTop {
		for i := 0; i < 4; i++ {
			top[i] = p(i, -1)
		}
		if haveTopRight {
			for i := 4; i < 8; i++ {
				top[i] = p(i, -1)
			}
		} else {
			for i := 4; i < 8; i++ {
				top[i] = top[3]
			}
		}
	}

	var left [4]int
	if haveLeft {
		for i := 0; i < 4; i++ {
			left[i] = p(-1, i)
		}
	}

	var tl int
	if haveTopLeft {
		tl = p(-1, -1)
	}

	topAt := func(i int) int {
		if i == -1 {
			return tl
		}
		return top[i]
	}
	leftAt := func(j int) int {
		if j == -1 {
			return tl
		}
		return left[j]
	}

	set := func(x, y, v int) {
		plane[offset+y*stride+x] = clip1(v)
	}

	switch mode {
	case I4x4Vertical:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				set(x, y, top[x])
			}
		}
	case I4x4Horizontal:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				set(x, y, left[y])
			}
		}
	case I4x4DC:
		var dc int
		switch {
		case haveLeft && haveTop:
			sum := top[0] + top[1] + top[2] + top[3] + left[0] + left[1] + left[2] + left[3]
			dc = (sum + 4) >> 3
		case haveLeft:
			sum := left[0] + left[1] + left[2] + left[3]
			dc = (sum + 2) >> 2
		case haveTop:
			sum := top[0] + top[1] + top[2] + top[3]
			dc = (sum + 2) >> 2
		default:
			dc = 128
		}
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				set(x, y, dc)
			}
		}
	case I4x4DiagonalDownLeft:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				var v int
				if x == 3 && y == 3 {
					v = (top[6] + 3*top[7] + 2) >> 2
				} else {
					s := x + y
					v = (top[s] + 2*top[s+1] + top[s+2] + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I4x4DiagonalDownRight:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				var v int
				switch {
				case x > y:
					d := x - y
					v = (topAt(d-2) + 2*topAt(d-1) + topAt(d) + 2) >> 2
				case x < y:
					d := y - x
					v = (leftAt(d-2) + 2*leftAt(d-1) + leftAt(d) + 2) >> 2
				default:
					v = (top[0] + 2*tl + left[0] + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I4x4VerticalRight:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				zVR := 2*x - y
				var v int
				switch {
				case zVR == 0 || zVR == 2 || zVR == 4 || zVR == 6:
					idx := x - (y >> 1) - 1
					v = (topAt(idx) + topAt(idx+1) + 1) >> 1
				case zVR == 1 || zVR == 3 || zVR == 5:
					idx := x - (y >> 1) - 2
					v = (topAt(idx) + 2*topAt(idx+1) + topAt(idx+2) + 2) >> 2
				case zVR == -1:
					v = (leftAt(0) + 2*tl + top[0] + 2) >> 2
				default:
					v = (leftAt(y-1) + 2*leftAt(y-2) + leftAt(y-3) + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I4x4HorizontalDown:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				zHD := 2*y - x
				var v int
				switch {
				case zHD == 0 || zHD == 2 || zHD == 4 || zHD == 6:
					idx := y - (x >> 1) - 1
					v = (leftAt(idx) + leftAt(idx+1) + 1) >> 1
				case zHD == 1 || zHD == 3 || zHD == 5:
					idx := y - (x >> 1) - 2
					v = (leftAt(idx) + 2*leftAt(idx+1) + leftAt(idx+2) + 2) >> 2
				case zHD == -1:
					v = (leftAt(0) + 2*tl + top[0] + 2) >> 2
				default:
					v = (topAt(x-1) + 2*topAt(x-2) + topAt(x-3) + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I4x4VerticalLeft:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				var v int
				if y == 0 || y == 2 {
					idx := x + (y >> 1)
					v = (top[idx] + top[idx+1] + 1) >> 1
				} else {
					idx := x + (y >> 1)
					v = (top[idx] + 2*top[idx+1] + top[idx+2] + 2) >> 2
				}
				set(x, y, v)
			}
		}
	case I4x4HorizontalUp:
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				zHU := x + 2*y
				var v int
				switch {
				case zHU == 0 || zHU == 2 || zHU == 4:
					idx := y + (x >> 1)
					v = (left[idx] + left[idx+1] + 1) >> 1
				case zHU == 1 || zHU == 3:
					idx := y + (x >> 1)
					v = (left[idx] + 2*left[idx+1] + left[idx+2] + 2) >> 2
				case zHU == 5:
					v = (left[2] + 3*left[3] + 2) >> 2
				default:
					v = left[3]
				}
				set(x, y, v)
			}
		}
	}
}
