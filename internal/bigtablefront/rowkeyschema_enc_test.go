package bigtablefront

// appendOrderedInt64 appends x in OrderedCode's signed-integer format (the
// format of Google's ordered code, as github.com/google/orderedcode
// documents it): the value's bytes after a unary length prefix of 1 bits
// ended by a 0 bit, the sign bit first, so one to ten bytes that sort as
// the values do, a negative value the bitwise inverse of its complement's.
func appendOrderedInt64(s []byte, x int64) []byte {
	if x >= -64 && x < 64 {
		return append(s, uint8(x)^0x80)
	}
	neg := x < 0
	if neg {
		x = ^x
	}
	n := 1
	var buf [10]byte
	i := 9
	for x > 0 {
		buf[i] = byte(x)
		n, i, x = n+1, i-1, x>>8
	}
	ffByte := n > 7
	if ffByte {
		n -= 7
	}
	if buf[i+1] < 1<<uint(8-n) {
		n--
		i++
	}
	buf[i] |= ^byte(0xff >> uint(n))
	if ffByte {
		i--
		buf[i] = 0xff
	}
	if neg {
		for j := i; j < len(buf); j++ {
			buf[j] ^= 0xff
		}
	}
	return append(s, buf[i:]...)
}
