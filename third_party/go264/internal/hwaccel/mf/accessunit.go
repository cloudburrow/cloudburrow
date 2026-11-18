package mf

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/nal"

func startsPicture(unit []byte) bool {
	return len(unit) >= 2 && unit[1]&0x80 != 0
}

func SplitAccessUnits(annexB []byte) [][]byte {
	units := nal.SplitAnnexB(annexB)
	var out [][]byte
	var current []byte
	var coded bool
	for _, u := range units {
		if len(u) == 0 {
			continue
		}
		t := nal.Type(u[0] & 0x1f)
		vcl := t.IsVCL()
		if coded && (!vcl || startsPicture(u)) {
			out = append(out, current)
			current = nil
			coded = false
		}
		current = append(current, 0, 0, 0, 1)
		current = append(current, u...)
		if vcl {
			coded = true
		}
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}
