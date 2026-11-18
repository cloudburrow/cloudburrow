package ac3

import "testing"

// TestReadDBA checks the delta bit allocation modes of A/52 Table 5.16:
// new info is read (cpldeltnseg, then cpldeltoffst, cpldeltlen and
// cpldeltba per segment, clauses 5.4.3.50 to 5.4.3.53), "perform no delta
// alloc" reads nothing, "reuse" keeps the previous state, and the reserved
// code is refused (clause 5.4.3.48: the decoder should not decode audio).
func TestReadDBA(t *testing.T) {
	// nseg-1 = 1 (3 bits), then (offst 5, len 4, ba 3) twice:
	// (3, 2, 5) and (1, 4, 2); 3+24 = 27 bits, padded.
	bits := "001" + "00011" + "0010" + "101" + "00001" + "0100" + "010" + "00000"
	b := make([]byte, len(bits)/8)
	for i, c := range bits {
		if c == '1' {
			b[i/8] |= 0x80 >> (i % 8)
		}
	}
	r := &bitReader{b: b}
	var d dba
	if err := readDBA(r, &d, dbaNew); err != nil {
		t.Fatal(err)
	}
	if d.mode != dbaNew || d.nseg != 2 || r.pos != 27 ||
		d.offst[0] != 3 || d.ln[0] != 2 || d.deba[0] != 5 ||
		d.offst[1] != 1 || d.ln[1] != 4 || d.deba[1] != 2 {
		t.Fatalf("new info: %+v, %d bits read", d, r.pos)
	}
	prev := d
	if err := readDBA(r, &d, dbaReuse); err != nil || d != prev || r.pos != 27 {
		t.Fatalf("reuse: %+v, %v, %d bits read", d, err, r.pos)
	}
	if err := readDBA(r, &d, dbaNone); err != nil || d.mode != dbaNone || r.pos != 27 {
		t.Fatalf("none: %+v, %v, %d bits read", d, err, r.pos)
	}
	if err := readDBA(r, &d, dbaReserved); err == nil {
		t.Fatal("the reserved mode was accepted")
	}
}
