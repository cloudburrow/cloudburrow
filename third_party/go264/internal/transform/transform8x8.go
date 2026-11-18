package transform

type Block8x8 [64]int32

func dct8x1D(src [8]int32) [8]int32 {
	s07 := src[0] + src[7]
	s16 := src[1] + src[6]
	s25 := src[2] + src[5]
	s34 := src[3] + src[4]
	a0 := s07 + s34
	a1 := s16 + s25
	a2 := s07 - s34
	a3 := s16 - s25
	d07 := src[0] - src[7]
	d16 := src[1] - src[6]
	d25 := src[2] - src[5]
	d34 := src[3] - src[4]
	a4 := d16 + d25 + (d07 + (d07 >> 1))
	a5 := d07 - d34 - (d25 + (d25 >> 1))
	a6 := d07 + d34 - (d16 + (d16 >> 1))
	a7 := d16 - d25 + (d34 + (d34 >> 1))
	var out [8]int32
	out[0] = a0 + a1
	out[1] = a4 + (a7 >> 2)
	out[2] = a2 + (a3 >> 1)
	out[3] = a5 + (a6 >> 2)
	out[4] = a0 - a1
	out[5] = a6 - (a5 >> 2)
	out[6] = (a2 >> 1) - a3
	out[7] = (a4 >> 2) - a7
	return out
}

func idct8x1D(src [8]int32) [8]int32 {
	a0 := src[0] + src[4]
	a2 := src[0] - src[4]
	a4 := (src[2] >> 1) - src[6]
	a6 := (src[6] >> 1) + src[2]
	b0 := a0 + a6
	b2 := a2 + a4
	b4 := a2 - a4
	b6 := a0 - a6
	a1 := -src[3] + src[5] - src[7] - (src[7] >> 1)
	a3 := src[1] + src[7] - src[3] - (src[3] >> 1)
	a5 := -src[1] + src[7] + src[5] + (src[5] >> 1)
	a7 := src[3] + src[5] + src[1] + (src[1] >> 1)
	b1 := (a7 >> 2) + a1
	b3 := a3 + (a5 >> 2)
	b5 := (a3 >> 2) - a5
	b7 := a7 - (a1 >> 2)
	var out [8]int32
	out[0] = b0 + b7
	out[1] = b2 + b5
	out[2] = b4 + b3
	out[3] = b6 + b1
	out[4] = b6 - b1
	out[5] = b4 - b3
	out[6] = b2 - b5
	out[7] = b0 - b7
	return out
}

func Forward8x8(b *Block8x8) {
	var tmp Block8x8
	for row := 0; row < 8; row++ {
		var src [8]int32
		for col := 0; col < 8; col++ {
			src[col] = b[row*8+col]
		}
		out := dct8x1D(src)
		for col := 0; col < 8; col++ {
			tmp[row*8+col] = out[col]
		}
	}
	for col := 0; col < 8; col++ {
		var src [8]int32
		for row := 0; row < 8; row++ {
			src[row] = tmp[row*8+col]
		}
		out := dct8x1D(src)
		for row := 0; row < 8; row++ {
			b[row*8+col] = out[row]
		}
	}
}

func Inverse8x8(b *Block8x8) {
	b[0] += 32
	var tmp Block8x8
	for row := 0; row < 8; row++ {
		var src [8]int32
		for col := 0; col < 8; col++ {
			src[col] = b[row*8+col]
		}
		out := idct8x1D(src)
		for col := 0; col < 8; col++ {
			tmp[row*8+col] = out[col]
		}
	}
	for col := 0; col < 8; col++ {
		var src [8]int32
		for row := 0; row < 8; row++ {
			src[row] = tmp[row*8+col]
		}
		out := idct8x1D(src)
		for row := 0; row < 8; row++ {
			b[row*8+col] = out[row] >> 6
		}
	}
}

func AddResidual8x8(plane []byte, stride, offset int, b *Block8x8) {
	for y := 0; y < 8; y++ {
		row := plane[offset+y*stride:]
		for x := 0; x < 8; x++ {
			v := int32(row[x]) + b[y*8+x]
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			row[x] = byte(v)
		}
	}
}
