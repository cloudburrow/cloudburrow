package pred

func Intra16x16(plane []byte, stride, offset, mode int, avail Availability) {
	p := func(x, y int) int {
		return int(plane[offset+y*stride+x])
	}

	haveTop := avail.Has(AvailTop)
	haveLeft := avail.Has(AvailLeft)
	haveTopLeft := avail.Has(AvailTopLeft)

	var top [16]int
	if haveTop {
		for i := 0; i < 16; i++ {
			top[i] = p(i, -1)
		}
	}

	var left [16]int
	if haveLeft {
		for i := 0; i < 16; i++ {
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
	case I16x16Vertical:
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				set(x, y, top[x])
			}
		}
	case I16x16Horizontal:
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				set(x, y, left[y])
			}
		}
	case I16x16DC:
		var dc int
		switch {
		case haveLeft && haveTop:
			sum := 0
			for i := 0; i < 16; i++ {
				sum += top[i] + left[i]
			}
			dc = (sum + 16) >> 5
		case haveTop:
			sum := 0
			for i := 0; i < 16; i++ {
				sum += top[i]
			}
			dc = (sum + 8) >> 4
		case haveLeft:
			sum := 0
			for i := 0; i < 16; i++ {
				sum += left[i]
			}
			dc = (sum + 8) >> 4
		default:
			dc = 128
		}
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				set(x, y, dc)
			}
		}
	case I16x16Plane:
		H := 0
		for i := 0; i < 8; i++ {
			H += (i + 1) * (topAt(8+i) - topAt(6-i))
		}
		V := 0
		for j := 0; j < 8; j++ {
			V += (j + 1) * (leftAt(8+j) - leftAt(6-j))
		}
		a := 16 * (leftAt(15) + topAt(15))
		b := (5*H + 32) >> 6
		c := (5*V + 32) >> 6
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				v := (a + b*(x-7) + c*(y-7) + 16) >> 5
				set(x, y, v)
			}
		}
	}
}
