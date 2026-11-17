package pred

func IntraChroma8x8(plane []byte, stride, offset, mode int, avail Availability) {
	p := func(x, y int) int {
		return int(plane[offset+y*stride+x])
	}

	haveTop := avail.Has(AvailTop)
	haveLeft := avail.Has(AvailLeft)
	haveTopLeft := avail.Has(AvailTopLeft)

	var top [8]int
	if haveTop {
		for i := 0; i < 8; i++ {
			top[i] = p(i, -1)
		}
	}

	var left [8]int
	if haveLeft {
		for i := 0; i < 8; i++ {
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
	case ChromaDC:
		positions := [4][2]int{{0, 0}, {4, 0}, {0, 4}, {4, 4}}
		for _, pos := range positions {
			xO, yO := pos[0], pos[1]
			sumTop := 0
			for i := 0; i < 4; i++ {
				sumTop += top[xO+i]
			}
			sumLeft := 0
			for j := 0; j < 4; j++ {
				sumLeft += left[yO+j]
			}
			var dc int
			switch {
			case (xO == 0 && yO == 0) || (xO > 0 && yO > 0):
				switch {
				case haveTop && haveLeft:
					dc = (sumTop + sumLeft + 4) >> 3
				case haveTop:
					dc = (sumTop + 2) >> 2
				case haveLeft:
					dc = (sumLeft + 2) >> 2
				default:
					dc = 128
				}
			case xO > 0 && yO == 0:
				switch {
				case haveTop:
					dc = (sumTop + 2) >> 2
				case haveLeft:
					dc = (sumLeft + 2) >> 2
				default:
					dc = 128
				}
			default:
				switch {
				case haveLeft:
					dc = (sumLeft + 2) >> 2
				case haveTop:
					dc = (sumTop + 2) >> 2
				default:
					dc = 128
				}
			}
			for y := 0; y < 4; y++ {
				for x := 0; x < 4; x++ {
					set(xO+x, yO+y, dc)
				}
			}
		}
	case ChromaHorizontal:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				set(x, y, left[y])
			}
		}
	case ChromaVertical:
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				set(x, y, top[x])
			}
		}
	case ChromaPlane:
		H := 0
		for i := 0; i < 4; i++ {
			H += (i + 1) * (topAt(4+i) - topAt(2-i))
		}
		V := 0
		for j := 0; j < 4; j++ {
			V += (j + 1) * (leftAt(4+j) - leftAt(2-j))
		}
		a := 16 * (leftAt(7) + topAt(7))
		b := (34*H + 32) >> 6
		c := (34*V + 32) >> 6
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				v := (a + b*(x-3) + c*(y-3) + 16) >> 5
				set(x, y, v)
			}
		}
	}
}
