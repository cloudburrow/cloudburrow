package aac

// sbrHuffman is one of SBR's and PS's Huffman tables as a binary tree: a
// pair per node, the entry for a 0 bit then a 1 bit; a positive entry is
// the next node, any other the symbol -(value+64).
type sbrHuffman struct {
	tree [][2]int8
}

func (h *sbrHuffman) decode(b *bitr) (int, error) {
	node := 0
	for range 32 {
		v := int(h.tree[node][b.u(1)])
		if b.err != nil {
			return 0, b.err
		}
		if v <= 0 {
			return v + 64, nil
		}
		node = v
	}
	return 0, errSBR
}
