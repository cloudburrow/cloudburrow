package tflitemodel

import (
	"fmt"
	"math"

	flatbuffers "github.com/google/flatbuffers/go"
)

// LiteRT's fused recurrent kernels (#1775): LSTM (its FULL kernel),
// UNIDIRECTIONAL_SEQUENCE_LSTM, BIDIRECTIONAL_SEQUENCE_LSTM, RNN,
// UNIDIRECTIONAL_SEQUENCE_RNN and BIDIRECTIONAL_SEQUENCE_RNN, in float32
// as tensorflow/lite/kernels/lstm_eval.cc (LstmStepFloat) and
// basic_rnn.cc / rnn_batch_step compute them: CIFG, peephole, projection,
// cell and projection clipping, layer normalization, diagonal recurrent
// weights, the auxiliary input of the bidirectional kernels, time-major and
// batch-major layouts. Their state tensors are the model's variable
// tensors, zero at the start of each Run. Weights stored quantized (the
// hybrid kernels) are dequantized at load, as for every operator here.
const (
	opLSTM                       = 16
	opRNN                        = 24
	opUnidirectionalSequenceRNN  = 35
	opUnidirectionalSequenceLSTM = 44
	opBidirectionalSequenceRNN   = 46
	opBidirectionalSequenceLSTM  = 52
)

func init() {
	for code, name := range map[int32]string{opLSTM: "LSTM", opRNN: "RNN", opUnidirectionalSequenceRNN: "UNIDIRECTIONAL_SEQUENCE_RNN",
		opUnidirectionalSequenceLSTM: "UNIDIRECTIONAL_SEQUENCE_LSTM", opBidirectionalSequenceRNN: "BIDIRECTIONAL_SEQUENCE_RNN",
		opBidirectionalSequenceLSTM: "BIDIRECTIONAL_SEQUENCE_LSTM"} {
		opNames[code] = name
	}
}

func (o op) boolD(slot int, d bool) bool {
	if o.opts == nil {
		return d
	}
	return o.opts.GetBoolSlot(flatbuffers.VOffsetT(slot), d)
}

// floats is an optional float tensor's elements, or nil.
func floats(t *Tensor) ([]float32, error) {
	if t == nil {
		return nil, nil
	}
	if !t.Type.IsFloat() {
		return nil, &UnsupportedError{"recurrent kernels with " + t.Type.String() + " weights are not supported"}
	}
	return t.F, nil
}

// matAcc is tensor_utils::MatrixBatchVectorMultiplyAccumulate for one
// batch row: out[r] += dot(w[r, :], v).
func matAcc(w []float32, rows, cols int, v, out []float32) {
	for r := range rows {
		var dot float32
		row := w[r*cols : (r+1)*cols]
		for c, x := range v[:cols] {
			dot += row[c] * x
		}
		out[r] += dot
	}
}

func sigmoid32(v float32) float32 { return float32(1 / (1 + math.Exp(-float64(v)))) }

// lstmDir is one direction's weights, each nil when the model omits it:
// w are input weights, r recurrent, p peephole, b biases, ln the
// layer-norm coefficients and a the auxiliary input's weights, by gate
// (input, forget, cell, output; peephole has no cell entry).
type lstmDir struct {
	w, r, b, ln, a  [4][]float32
	p               [3][]float32
	proj, projB     []float32
	nIn, nAux       int
	nCell, nOut     int
	diag            bool
	act             int8
	cellClip, pClip float32
}

// newLSTMDir reads a direction's 17 weight inputs at w (input_to_input
// .. projection_bias), its 4 layer-norm coefficients at ln (or nil) and
// its 4 aux weights at aux (or nil).
func newLSTMDir(o op, w, ln, aux []*Tensor, act int8, cellClip, projClip float32, diag bool) (*lstmDir, error) {
	d := &lstmDir{act: act, cellClip: cellClip, pClip: projClip, diag: diag}
	get := func(ts []*Tensor, i int) ([]float32, error) {
		if i >= len(ts) {
			return nil, nil
		}
		return floats(ts[i])
	}
	var err error
	for g := range 4 {
		if d.w[g], err = get(w, g); err != nil {
			return nil, err
		}
		if d.r[g], err = get(w, 4+g); err != nil {
			return nil, err
		}
		if d.b[g], err = get(w, 11+g); err != nil {
			return nil, err
		}
		if d.ln[g], err = get(ln, g); err != nil {
			return nil, err
		}
		if d.a[g], err = get(aux, g); err != nil {
			return nil, err
		}
	}
	for g := range 3 {
		if d.p[g], err = get(w, 8+g); err != nil {
			return nil, err
		}
	}
	if d.proj, err = get(w, 15); err != nil {
		return nil, err
	}
	if d.projB, err = get(w, 16); err != nil {
		return nil, err
	}
	if d.w[1] == nil || d.w[2] == nil || d.w[3] == nil || d.r[1] == nil || d.r[2] == nil || d.r[3] == nil {
		return nil, fmt.Errorf("operator %s lacks its forget, cell or output weights", o.name)
	}
	fw := w[1]
	if len(fw.Shape) != 2 {
		return nil, fmt.Errorf("operator %s: input_to_forget_weights has shape %v", o.name, fw.Shape)
	}
	d.nCell, d.nIn = fw.Shape[0], fw.Shape[1]
	d.nOut = d.nCell
	if d.diag {
		if len(d.r[1]) != d.nCell {
			return nil, fmt.Errorf("operator %s: diagonal recurrent weights of %d values for %d cells", o.name, len(d.r[1]), d.nCell)
		}
	} else {
		if w[5] == nil || len(w[5].Shape) != 2 || w[5].Shape[0] != d.nCell {
			return nil, fmt.Errorf("operator %s: recurrent_to_forget_weights must be [%d, n_output]", o.name, d.nCell)
		}
		d.nOut = w[5].Shape[1]
	}
	if d.proj != nil {
		if len(d.proj) != d.nOut*d.nCell {
			return nil, fmt.Errorf("operator %s: projection weights of %d values, not %d", o.name, len(d.proj), d.nOut*d.nCell)
		}
	} else if d.nOut != d.nCell {
		return nil, fmt.Errorf("operator %s: %d outputs and %d cells without a projection", o.name, d.nOut, d.nCell)
	}
	check := func(v []float32, n int, what string) error {
		if v != nil && len(v) != n {
			return fmt.Errorf("operator %s: %s holds %d values, not %d", o.name, what, len(v), n)
		}
		return nil
	}
	for g := range 4 {
		if err := check(d.w[g], d.nCell*d.nIn, "an input weight"); err != nil {
			return nil, err
		}
		rn := d.nCell * d.nOut
		if d.diag {
			rn = d.nCell
		}
		if err := check(d.r[g], rn, "a recurrent weight"); err != nil {
			return nil, err
		}
		if err := check(d.b[g], d.nCell, "a gate bias"); err != nil {
			return nil, err
		}
		if err := check(d.ln[g], d.nCell, "a layer-norm coefficient"); err != nil {
			return nil, err
		}
	}
	for g := range 3 {
		if err := check(d.p[g], d.nCell, "a peephole weight"); err != nil {
			return nil, err
		}
	}
	if err := check(d.projB, d.nOut, "the projection bias"); err != nil {
		return nil, err
	}
	if d.a[1] != nil {
		d.nAux = len(d.a[1]) / d.nCell
	}
	return d, nil
}

// gate is CalculateLstmGateFloat for one batch row.
func (d *lstmDir) gate(g int, x, aux, h, c []float32, peep []float32, sig bool) []float32 {
	out := make([]float32, d.nCell)
	ln := d.ln[g] != nil
	if !ln && d.b[g] != nil {
		copy(out, d.b[g])
	}
	matAcc(d.w[g], d.nCell, d.nIn, x, out)
	if d.a[g] != nil && aux != nil {
		matAcc(d.a[g], d.nCell, d.nAux, aux, out)
	}
	if d.diag {
		for i := range out {
			out[i] += d.r[g][i] * h[i]
		}
	} else {
		matAcc(d.r[g], d.nCell, d.nOut, h, out)
	}
	if peep != nil {
		for i := range out {
			out[i] += peep[i] * c[i]
		}
	}
	if ln {
		meanStddevNormalize(out)
		for i := range out {
			out[i] *= d.ln[g][i]
			if d.b[g] != nil {
				out[i] += d.b[g][i]
			}
		}
	}
	for i, v := range out {
		if sig {
			out[i] = sigmoid32(v)
		} else {
			out[i] = activation(d.act, v)
		}
	}
	return out
}

// meanStddevNormalize is tensor_utils::MeanStddevNormalization of one row.
func meanStddevNormalize(v []float32) {
	var sum, sumSq float32
	for _, x := range v {
		sum += x
		sumSq += x * x
	}
	n := float32(len(v))
	mean := sum / n
	variance := sumSq/n - mean*mean
	inv := float32(1 / math.Sqrt(1e-8))
	if variance != 0 {
		inv = 1 / float32(math.Sqrt(float64(variance)))
	}
	for i, x := range v {
		v[i] = (x - mean) * inv
	}
}

// step is LstmStepFloat for one batch row: it updates h and c.
func (d *lstmDir) step(x, aux, h, c []float32) {
	cifg := d.w[0] == nil
	var in []float32
	if !cifg {
		in = d.gate(0, x, aux, h, c, d.p[0], true)
	}
	f := d.gate(1, x, aux, h, c, d.p[1], true)
	g := d.gate(2, x, aux, h, c, nil, false)
	for i := range c {
		c[i] *= f[i]
		if cifg {
			c[i] += g[i] * (1 - f[i])
		} else {
			c[i] += g[i] * in[i]
		}
		if d.cellClip > 0 {
			c[i] = min(max(c[i], -d.cellClip), d.cellClip)
		}
	}
	o := d.gate(3, x, aux, h, c, d.p[2], true)
	s := make([]float32, d.nCell)
	for i := range s {
		s[i] = o[i] * activation(d.act, c[i])
	}
	if d.proj == nil {
		copy(h, s)
		return
	}
	for i := range h {
		h[i] = 0
		if d.projB != nil {
			h[i] = d.projB[i]
		}
	}
	matAcc(d.proj, d.nOut, d.nCell, s, h)
	if d.pClip > 0 {
		for i := range h {
			h[i] = min(max(h[i], -d.pClip), d.pClip)
		}
	}
}

// seqLayout is a sequence input's time steps, batch and features, and
// where element (t, b) starts.
type seqLayout struct {
	steps, batch, width int
	timeMajor           bool
}

func layout(o op, x *Tensor, timeMajor bool) (seqLayout, error) {
	switch len(x.Shape) {
	case 2: // LSTM and RNN: one step
		return seqLayout{1, x.Shape[0], x.Shape[1], true}, nil
	case 3:
		if timeMajor {
			return seqLayout{x.Shape[0], x.Shape[1], x.Shape[2], true}, nil
		}
		return seqLayout{x.Shape[1], x.Shape[0], x.Shape[2], false}, nil
	}
	return seqLayout{}, fmt.Errorf("operator %s: input of shape %v is not [time, batch, features]", o.name, x.Shape)
}

func (l seqLayout) at(t, b int) int {
	if l.timeMajor {
		return (t*l.batch + b) * l.width
	}
	return (b*l.steps + t) * l.width
}

// shape is the layout's shape with width w.
func (l seqLayout) shape(w int, rank int) []int {
	switch {
	case rank == 2:
		return []int{l.batch, w}
	case l.timeMajor:
		return []int{l.steps, l.batch, w}
	}
	return []int{l.batch, l.steps, w}
}

// state is an initial state's values for batch row b, copied.
func state(t *Tensor, b, n int) []float32 {
	s := make([]float32, n)
	if t != nil && t.F != nil && len(t.F) >= (b+1)*n {
		copy(s, t.F[b*n:(b+1)*n])
	}
	return s
}

// runSeq runs a recurrent step over x's sequence, forward or backward,
// for each batch row, writing each step's output at offset off of rows of
// width w in out. step(x, aux, h) updates h (the output) and its own state.
func runSeq(l seqLayout, x, aux []float32, auxW int, forward bool, out []float32, off, w, nOut int, step func(b int, x, aux, h []float32)) {
	h := make([]float32, nOut)
	for b := range l.batch {
		for k := range l.steps {
			t := k
			if !forward {
				t = l.steps - 1 - k
			}
			xs := x[l.at(t, b) : l.at(t, b)+l.width]
			var as []float32
			if aux != nil {
				a := l
				a.width = auxW
				as = aux[a.at(t, b) : a.at(t, b)+auxW]
			}
			step(b, xs, as, h)
			o := l
			o.width = w
			copy(out[o.at(t, b)+off:], h)
		}
	}
}

// evalLSTM runs LSTM and UNIDIRECTIONAL_SEQUENCE_LSTM: inputs 1..17 the
// weights, 18 and 19 the output and cell state, 20..23 the layer-norm
// coefficients.
func evalLSTM(o op, a []*Tensor) (*Tensor, error) {
	x := a[0]
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	if len(a) < 20 {
		return nil, fmt.Errorf("operator %s takes at least 20 inputs, not %d", o.name, len(a))
	}
	timeMajor, diag := true, false
	if o.code == opLSTM {
		if o.i8(10, 0) != 0 {
			return nil, &UnsupportedError{"operator LSTM with the BASIC kernel is not supported"}
		}
	} else {
		timeMajor, diag = o.bool(10), o.bool(14)
	}
	var ln []*Tensor
	if len(a) >= 24 {
		ln = a[20:24]
	}
	d, err := newLSTMDir(o, a[1:18], ln, nil, o.i8(4, 0), o.f32(6, 0), o.f32(8, 0), diag)
	if err != nil {
		return nil, err
	}
	l, err := layout(o, x, timeMajor)
	if err != nil {
		return nil, err
	}
	if l.width != d.nIn {
		return nil, fmt.Errorf("operator %s: inputs of %d features for weights of %d", o.name, l.width, d.nIn)
	}
	r := &Tensor{Type: Float32, Shape: l.shape(d.nOut, len(x.Shape))}
	r.F = make([]float32, prod(r.Shape))
	d.seq(l, x.F, nil, 0, true, r.F, 0, d.nOut, a[18], a[19])
	return r, nil
}

// seq runs the direction over a sequence from the given initial states.
func (d *lstmDir) seq(l seqLayout, x, aux []float32, auxW int, forward bool, out []float32, off, w int, h0, c0 *Tensor) {
	hs := make([][]float32, l.batch)
	cs := make([][]float32, l.batch)
	for b := range l.batch {
		hs[b], cs[b] = state(h0, b, d.nOut), state(c0, b, d.nCell)
	}
	runSeq(l, x, aux, auxW, forward, out, off, w, d.nOut, func(b int, x, aux, h []float32) {
		d.step(x, aux, hs[b], cs[b])
		copy(h, hs[b])
	})
}

// evalBiLSTM runs BIDIRECTIONAL_SEQUENCE_LSTM: 1..17 the forward weights,
// 18..34 the backward ones, 35..38 the forward and backward output and
// cell states, 39 the auxiliary input, 40..43 and 44..47 its forward and
// backward weights.
func evalBiLSTM(o op, a []*Tensor, nOut int) ([]*Tensor, error) {
	x := a[0]
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	if len(a) < 39 {
		return nil, fmt.Errorf("operator %s takes at least 39 inputs, not %d", o.name, len(a))
	}
	get := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	aux := get(39)
	var fa, ba []*Tensor
	if len(a) >= 48 {
		fa, ba = a[40:44], a[44:48]
	}
	useAux := len(fa) > 1 && fa[1] != nil
	if !useAux {
		fa, ba = nil, nil
	}
	act, cc, pc := o.i8(4, 0), o.f32(6, 0), o.f32(8, 0)
	fw, err := newLSTMDir(o, a[1:18], nil, fa, act, cc, pc, false)
	if err != nil {
		return nil, err
	}
	bw, err := newLSTMDir(o, a[18:35], nil, ba, act, cc, pc, false)
	if err != nil {
		return nil, err
	}
	return biSeq(o, x, aux, useAux, nOut, fw.nIn, fw.nOut, bw.nOut,
		func(l seqLayout, x, aux []float32, auxW int, out []float32, off, w int) {
			fw.seq(l, x, aux, auxW, true, out, off, w, a[35], a[36])
		},
		func(l seqLayout, x, aux []float32, auxW int, out []float32, off, w int) {
			bw.seq(l, x, aux, auxW, false, out, off, w, a[37], a[38])
		}, o.boolD(12, true), o.bool(10))
}

// biSeq runs a bidirectional kernel's two directions: the backward one on
// the auxiliary input when the model gives one without aux weights, and
// their outputs merged along the last axis or as two tensors.
func biSeq(o op, x, aux *Tensor, useAux bool, nOut, nIn, fwOut, bwOut int,
	fw, bw func(l seqLayout, x, aux []float32, auxW int, out []float32, off, w int), timeMajor, merge bool) ([]*Tensor, error) {
	l, err := layout(o, x, timeMajor)
	if err != nil {
		return nil, err
	}
	if l.width != nIn {
		return nil, fmt.Errorf("operator %s: inputs of %d features for weights of %d", o.name, l.width, nIn)
	}
	bwIn := x
	var auxF []float32
	auxW := 0
	if aux != nil {
		if !aux.Type.IsFloat() || len(aux.Shape) != len(x.Shape) {
			return nil, fmt.Errorf("operator %s: auxiliary input of shape %v", o.name, aux.Shape)
		}
		if useAux {
			auxF, auxW = aux.F, aux.Shape[len(aux.Shape)-1]
		} else {
			bwIn = aux
		}
	}
	bl, err := layout(o, bwIn, timeMajor)
	if err != nil {
		return nil, err
	}
	if bl.width != nIn || bl.steps != l.steps || bl.batch != l.batch {
		return nil, fmt.Errorf("operator %s: backward input of shape %v", o.name, bwIn.Shape)
	}
	rank := len(x.Shape)
	if merge {
		if nOut != 1 {
			return nil, fmt.Errorf("operator %s merges its outputs into one, not %d", o.name, nOut)
		}
		w := fwOut + bwOut
		r := &Tensor{Type: Float32, Shape: l.shape(w, rank)}
		r.F = make([]float32, prod(r.Shape))
		fw(l, x.F, auxF, auxW, r.F, 0, w)
		bw(l, bwIn.F, auxF, auxW, r.F, fwOut, w)
		return []*Tensor{r}, nil
	}
	if nOut != 2 {
		return nil, fmt.Errorf("operator %s gives 2 outputs, not %d", o.name, nOut)
	}
	f := &Tensor{Type: Float32, Shape: l.shape(fwOut, rank)}
	f.F = make([]float32, prod(f.Shape))
	b := &Tensor{Type: Float32, Shape: l.shape(bwOut, rank)}
	b.F = make([]float32, prod(b.Shape))
	fw(l, x.F, auxF, auxW, f.F, 0, fwOut)
	bw(l, bwIn.F, auxF, auxW, b.F, 0, bwOut)
	return []*Tensor{f, b}, nil
}

// rnnDir is a basic RNN's weights.
type rnnDir struct {
	w, r, b, aux     []float32
	nIn, units, nAux int
	act              int8
}

func newRNNDir(o op, w, r, b, aux *Tensor, act int8) (*rnnDir, error) {
	if w == nil || r == nil || b == nil || len(w.Shape) != 2 {
		return nil, fmt.Errorf("operator %s lacks its weights, recurrent weights or bias", o.name)
	}
	d := &rnnDir{units: w.Shape[0], nIn: w.Shape[1], act: act}
	var err error
	if d.w, err = floats(w); err != nil {
		return nil, err
	}
	if d.r, err = floats(r); err != nil {
		return nil, err
	}
	if d.b, err = floats(b); err != nil {
		return nil, err
	}
	if d.aux, err = floats(aux); err != nil {
		return nil, err
	}
	if len(d.r) != d.units*d.units || len(d.b) != d.units {
		return nil, fmt.Errorf("operator %s: recurrent weights or bias do not match %d units", o.name, d.units)
	}
	if d.aux != nil {
		d.nAux = len(d.aux) / d.units
	}
	return d, nil
}

// step is RnnBatchStep for one row: h = act(b + W x + A aux + R h).
func (d *rnnDir) step(x, aux, h []float32) {
	out := make([]float32, d.units)
	copy(out, d.b)
	matAcc(d.w, d.units, d.nIn, x, out)
	if d.aux != nil && aux != nil {
		matAcc(d.aux, d.units, d.nAux, aux, out)
	}
	matAcc(d.r, d.units, d.units, h, out)
	for i, v := range out {
		h[i] = activation(d.act, v)
	}
}

func (d *rnnDir) seq(l seqLayout, x, aux []float32, auxW int, forward bool, out []float32, off, w int, h0 *Tensor) {
	hs := make([][]float32, l.batch)
	for b := range l.batch {
		hs[b] = state(h0, b, d.units)
	}
	runSeq(l, x, aux, auxW, forward, out, off, w, d.units, func(b int, x, aux, h []float32) {
		d.step(x, aux, hs[b])
		copy(h, hs[b])
	})
}

// evalRNN runs RNN and UNIDIRECTIONAL_SEQUENCE_RNN: input, weights,
// recurrent weights, bias, hidden state.
func evalRNN(o op, a []*Tensor) (*Tensor, error) {
	x := a[0]
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	if len(a) < 5 {
		return nil, fmt.Errorf("operator %s takes 5 inputs, not %d", o.name, len(a))
	}
	timeMajor, act := true, o.i8(4, 0)
	if o.code == opUnidirectionalSequenceRNN {
		timeMajor, act = o.bool(4), o.i8(6, 0)
	}
	d, err := newRNNDir(o, a[1], a[2], a[3], nil, act)
	if err != nil {
		return nil, err
	}
	l, err := layout(o, x, timeMajor)
	if err != nil {
		return nil, err
	}
	if l.width != d.nIn {
		return nil, fmt.Errorf("operator %s: inputs of %d features for weights of %d", o.name, l.width, d.nIn)
	}
	r := &Tensor{Type: Float32, Shape: l.shape(d.units, len(x.Shape))}
	r.F = make([]float32, prod(r.Shape))
	d.seq(l, x.F, nil, 0, true, r.F, 0, d.units, a[4])
	return r, nil
}

// evalBiRNN runs BIDIRECTIONAL_SEQUENCE_RNN: input; forward weights,
// recurrent weights, bias, hidden state; the backward ones; the auxiliary
// input and its forward and backward weights.
func evalBiRNN(o op, a []*Tensor, nOut int) ([]*Tensor, error) {
	x := a[0]
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	if len(a) < 9 {
		return nil, fmt.Errorf("operator %s takes at least 9 inputs, not %d", o.name, len(a))
	}
	get := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	useAux := get(10) != nil
	var fa, ba *Tensor
	if useAux {
		fa, ba = get(10), get(11)
	}
	act := o.i8(6, 0)
	fw, err := newRNNDir(o, a[1], a[2], a[3], fa, act)
	if err != nil {
		return nil, err
	}
	bw, err := newRNNDir(o, a[5], a[6], a[7], ba, act)
	if err != nil {
		return nil, err
	}
	return biSeq(o, x, get(9), useAux, nOut, fw.nIn, fw.units, bw.units,
		func(l seqLayout, x, aux []float32, auxW int, out []float32, off, w int) {
			fw.seq(l, x, aux, auxW, true, out, off, w, a[4])
		},
		func(l seqLayout, x, aux []float32, auxW int, out []float32, off, w int) {
			bw.seq(l, x, aux, auxW, false, out, off, w, a[8])
		}, o.bool(4), o.bool(8))
}
