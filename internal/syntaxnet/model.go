package syntaxnet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// termMap is a SyntaxNet TermFrequencyMap: a count line, then "term
// frequency" lines; a term's value is its line's position.
type termMap struct {
	terms []string
	index map[string]int
}

func readTermMap(b []byte) (*termMap, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	if !sc.Scan() {
		return nil, errors.New("empty term map")
	}
	total, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil {
		return nil, fmt.Errorf("term map size: %w", err)
	}
	m := &termMap{index: map[string]int{}}
	for i := 0; i < total && sc.Scan(); i++ {
		line := sc.Text()
		sp := strings.LastIndexByte(line, ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("term map line %d: no frequency", i+2)
		}
		term := line[:sp]
		m.index[term] = len(m.terms)
		m.terms = append(m.terms, term)
	}
	return m, nil
}

func (m *termMap) lookup(term string, unknown int) int {
	if i, ok := m.index[term]; ok {
		return i
	}
	return unknown
}

// readAffixTable reads an AffixTable from a SyntaxNet record file: each
// record a little-endian uint64 length, a uint32 masked CRC and the
// AffixTableEntry (form, length, shorter_id per affix). Its value is the
// affix's position.
func readAffixTable(b []byte) (map[string]int, error) {
	if len(b) < 12 {
		return nil, errors.New("short affix table")
	}
	n := binary.LittleEndian.Uint64(b)
	if uint64(len(b)) < 12+n {
		return nil, errors.New("truncated affix table")
	}
	entry := b[12 : 12+n]
	out := map[string]int{}
	err := fields(entry, func(num protowire.Number, _ protowire.Type, a []byte, _ uint64) {
		if num != 3 {
			return
		}
		_ = fields(a, func(num protowire.Number, _ protowire.Type, v []byte, _ uint64) {
			if num == 1 {
				out[string(v)] = len(out)
			}
		})
	})
	return out, err
}

// network is a SyntaxNet feed-forward network (graph_builder.py's
// _BuildNetwork): per feature group an embedding matrix, the group's
// features' embeddings concatenated in order, ReLU layers and a linear
// softmax layer whose outputs are the actions' scores.
type network struct {
	embeddings []*tensor // [ids, dim] per group
	weights    []*tensor // [in, out] per hidden layer
	biases     []*tensor
	softW      *tensor
	softB      *tensor
}

func loadNetwork(path string, groups, layers int) (*network, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ts, err := readCheckpoint(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	get := func(name string) (*tensor, error) {
		t := ts[name+"_avg_var"]
		if t == nil {
			return nil, fmt.Errorf("%s: no tensor %s", filepath.Base(path), name)
		}
		return t, nil
	}
	n := &network{}
	for i := range groups {
		t, err := get(fmt.Sprintf("embedding_matrix_%d", i))
		if err != nil {
			return nil, err
		}
		n.embeddings = append(n.embeddings, t)
	}
	for i := range layers {
		w, err := get(fmt.Sprintf("weights_%d", i))
		if err != nil {
			return nil, err
		}
		bb, err := get(fmt.Sprintf("bias_%d", i))
		if err != nil {
			return nil, err
		}
		n.weights, n.biases = append(n.weights, w), append(n.biases, bb)
	}
	if n.softW, err = get("softmax_weight"); err != nil {
		return nil, err
	}
	if n.softB, err = get("softmax_bias"); err != nil {
		return nil, err
	}
	return n, nil
}

// noFeature is a feature with no value, whose embedding is left out (a
// zero vector), as SyntaxNet's kNone.
const noFeature = -1

// scores runs the network on feats, a value per feature, grouped.
func (n *network) scores(feats [][]int) []float32 {
	var in []float32
	for g, ids := range feats {
		e := n.embeddings[g]
		dim := e.shape[1]
		for _, id := range ids {
			if id < 0 || id >= e.shape[0] {
				in = append(in, make([]float32, dim)...)
				continue
			}
			in = append(in, e.data[id*dim:(id+1)*dim]...)
		}
	}
	for l, w := range n.weights {
		out := make([]float32, w.shape[1])
		copy(out, n.biases[l].data)
		cols := w.shape[1]
		for i, v := range in {
			if v == 0 {
				continue
			}
			row := w.data[i*cols : (i+1)*cols]
			for j, x := range row {
				out[j] += v * x
			}
		}
		for j := range out {
			if out[j] < 0 {
				out[j] = 0
			}
		}
		in = out
	}
	cols := n.softW.shape[1]
	out := make([]float32, cols)
	copy(out, n.softB.data)
	for i, v := range in {
		if v == 0 {
			continue
		}
		row := n.softW.data[i*cols : (i+1)*cols]
		for j, x := range row {
			out[j] += v * x
		}
	}
	return out
}
