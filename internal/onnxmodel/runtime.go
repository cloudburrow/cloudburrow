//go:build onnx

package onnxmodel

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Available reports whether this binary runs ONNX models.
const Available = true

var envOnce sync.Once
var envErr error

// InitRuntime loads ONNX Runtime once for the process, from ONNXRUNTIME_LIB
// when it is set; internal/embedding shares it.
func InitRuntime() error {
	envOnce.Do(func() {
		if ort.IsInitialized() {
			return
		}
		if lib := os.Getenv("ONNXRUNTIME_LIB"); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		envErr = ort.InitializeEnvironment()
	})
	if envErr != nil {
		return fmt.Errorf("initialise ONNX Runtime (set ONNXRUNTIME_LIB to libonnxruntime): %w", envErr)
	}
	return nil
}

func port(info ort.InputOutputInfo) Port {
	p := Port{Name: info.Name, Kind: info.OrtValueType.String()}
	if info.OrtValueType == ort.ONNXTypeTensor {
		p.Elem = strings.TrimPrefix(info.DataType.String(), "ONNX_TENSOR_ELEMENT_DATA_TYPE_")
		p.Dims = append([]int64{}, info.Dimensions...)
		for i, d := range p.Dims {
			if d < 0 {
				p.Dims[i] = -1
			}
		}
	}
	return p
}

// Inspect loads model and returns its inputs and outputs.
func Inspect(model []byte) (inputs, outputs []Port, err error) {
	if err := InitRuntime(); err != nil {
		return nil, nil, err
	}
	in, out, err := ort.GetInputOutputInfoWithONNXData(model)
	if err != nil {
		return nil, nil, err
	}
	for _, i := range in {
		inputs = append(inputs, port(i))
	}
	for _, o := range out {
		outputs = append(outputs, port(o))
	}
	return inputs, outputs, nil
}

// Run runs model on in, one Tensor for each of inputs, and returns one
// Tensor for each of outputs.
func Run(model []byte, inputs []Port, in []Tensor, outputs []Port) ([]Tensor, error) {
	if err := InitRuntime(); err != nil {
		return nil, err
	}
	if len(in) != len(inputs) {
		return nil, fmt.Errorf("%d tensors for %d inputs", len(in), len(inputs))
	}
	var inNames, outNames []string
	for _, p := range inputs {
		inNames = append(inNames, p.Name)
	}
	for _, p := range outputs {
		outNames = append(outNames, p.Name)
	}
	sess, err := ort.NewDynamicAdvancedSessionWithONNXData(model, inNames, outNames, nil)
	if err != nil {
		return nil, err
	}
	defer sess.Destroy()
	vals := make([]ort.Value, len(in))
	defer func() {
		for _, v := range vals {
			if v != nil {
				_ = v.Destroy()
			}
		}
	}()
	for i, t := range in {
		v, err := tensorValue(inputs[i].Elem, t)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", inputs[i].Name, err)
		}
		vals[i] = v
	}
	outs := make([]ort.Value, len(outputs))
	if err := sess.Run(vals, outs); err != nil {
		return nil, err
	}
	defer func() {
		for _, v := range outs {
			if v != nil {
				_ = v.Destroy()
			}
		}
	}()
	res := make([]Tensor, len(outs))
	for i, v := range outs {
		t, err := goTensor(v)
		if err != nil {
			return nil, fmt.Errorf("output %s: %w", outputs[i].Name, err)
		}
		res[i] = t
	}
	return res, nil
}

func newTensor[T ort.TensorData](shape []int64, n int, at func(int) T) (ort.Value, error) {
	data := make([]T, n)
	for i := range data {
		data[i] = at(i)
	}
	return ort.NewTensor(ort.NewShape(shape...), data)
}

// tensorValue is t as a tensor of element type elem.
func tensorValue(elem string, t Tensor) (ort.Value, error) {
	n := 1
	for _, d := range t.Shape {
		n *= int(d)
	}
	switch elem {
	case "STRING":
		if len(t.S) != n {
			return nil, fmt.Errorf("%d strings for shape %v", len(t.S), t.Shape)
		}
		st, err := ort.NewStringTensor(ort.NewShape(t.Shape...))
		if err != nil {
			return nil, err
		}
		if err := st.SetContents(t.S); err != nil {
			_ = st.Destroy()
			return nil, err
		}
		return st, nil
	case "BOOL":
		if len(t.B) != n {
			return nil, fmt.Errorf("%d booleans for shape %v", len(t.B), t.Shape)
		}
		return newTensor(t.Shape, n, func(i int) bool { return t.B[i] })
	}
	if IsFloat(elem) && len(t.F) != n || IsInt(elem) && len(t.I) != n {
		return nil, fmt.Errorf("%d values for shape %v", max(len(t.F), len(t.I)), t.Shape)
	}
	switch elem {
	case "FLOAT":
		return newTensor(t.Shape, n, func(i int) float32 { return float32(t.F[i]) })
	case "DOUBLE":
		return newTensor(t.Shape, n, func(i int) float64 { return t.F[i] })
	case "FLOAT16", "BFLOAT16":
		b := make([]byte, 2*n)
		for i, f := range t.F {
			var h uint16
			if elem == "FLOAT16" {
				h = toHalf(float32(f))
			} else {
				h = uint16(math.Float32bits(float32(f)) >> 16)
			}
			b[2*i], b[2*i+1] = byte(h), byte(h>>8)
		}
		dt := ort.TensorElementDataType(ort.TensorElementDataTypeFloat16)
		if elem == "BFLOAT16" {
			dt = ort.TensorElementDataTypeBFloat16
		}
		return ort.NewCustomDataTensor(ort.NewShape(t.Shape...), b, dt)
	case "INT8":
		return newTensor(t.Shape, n, func(i int) int8 { return int8(t.I[i]) })
	case "UINT8":
		return newTensor(t.Shape, n, func(i int) uint8 { return uint8(t.I[i]) })
	case "INT16":
		return newTensor(t.Shape, n, func(i int) int16 { return int16(t.I[i]) })
	case "UINT16":
		return newTensor(t.Shape, n, func(i int) uint16 { return uint16(t.I[i]) })
	case "INT32":
		return newTensor(t.Shape, n, func(i int) int32 { return int32(t.I[i]) })
	case "UINT32":
		return newTensor(t.Shape, n, func(i int) uint32 { return uint32(t.I[i]) })
	case "INT64":
		return newTensor(t.Shape, n, func(i int) int64 { return t.I[i] })
	case "UINT64":
		return newTensor(t.Shape, n, func(i int) uint64 { return uint64(t.I[i]) })
	}
	return nil, fmt.Errorf("unsupported element type %s", elem)
}

func floats[T float32 | float64](d []T) []float64 {
	out := make([]float64, len(d))
	for i, v := range d {
		out[i] = float64(v)
	}
	return out
}

func ints[T int8 | uint8 | int16 | uint16 | int32 | uint32 | int64](d []T) []int64 {
	out := make([]int64, len(d))
	for i, v := range d {
		out[i] = int64(v)
	}
	return out
}

// goTensor copies an output tensor's data.
func goTensor(v ort.Value) (Tensor, error) {
	t := Tensor{Shape: append([]int64{}, v.GetShape()...)}
	switch x := v.(type) {
	case *ort.Tensor[float32]:
		t.F = floats(x.GetData())
	case *ort.Tensor[float64]:
		t.F = floats(x.GetData())
	case *ort.Tensor[int8]:
		t.I = ints(x.GetData())
	case *ort.Tensor[uint8]:
		t.I = ints(x.GetData())
	case *ort.Tensor[int16]:
		t.I = ints(x.GetData())
	case *ort.Tensor[uint16]:
		t.I = ints(x.GetData())
	case *ort.Tensor[int32]:
		t.I = ints(x.GetData())
	case *ort.Tensor[uint32]:
		t.I = ints(x.GetData())
	case *ort.Tensor[int64]:
		t.I = ints(x.GetData())
	case *ort.Tensor[uint64]:
		for _, u := range x.GetData() {
			if u > math.MaxInt64 {
				return t, fmt.Errorf("UINT64 value %d does not fit an INT64", u)
			}
			t.I = append(t.I, int64(u))
		}
	case *ort.Tensor[bool]:
		t.B = append([]bool{}, x.GetData()...)
	case *ort.StringTensor:
		s, err := x.GetContents()
		if err != nil {
			return t, err
		}
		t.S = s
	case *ort.CustomDataTensor:
		b := x.GetData()
		dt := ort.TensorElementDataType(x.DataType())
		if dt != ort.TensorElementDataTypeFloat16 && dt != ort.TensorElementDataTypeBFloat16 {
			return t, fmt.Errorf("unsupported element type %s", dt)
		}
		for i := 0; i+1 < len(b); i += 2 {
			h := uint16(b[i]) | uint16(b[i+1])<<8
			if dt == ort.TensorElementDataTypeFloat16 {
				t.F = append(t.F, float64(fromHalf(h)))
			} else {
				t.F = append(t.F, float64(math.Float32frombits(uint32(h)<<16)))
			}
		}
	default:
		return t, fmt.Errorf("unsupported ONNX value %T (%s)", v, v.GetONNXType())
	}
	return t, nil
}

// toHalf is f as IEEE 754 half precision, rounded to nearest even.
func toHalf(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int((b>>23)&0xff) - 127 + 15
	mant := b & 0x7fffff
	switch {
	case (b>>23)&0xff == 0xff: // Inf or NaN
		if mant != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	case exp >= 0x1f:
		return sign | 0x7c00
	case exp <= 0:
		if exp < -10 {
			return sign
		}
		mant |= 0x800000
		shift := uint(14 - exp)
		h := uint16(mant >> shift)
		rem := mant & (1<<shift - 1)
		half := uint32(1) << (shift - 1)
		if rem > half || rem == half && h&1 == 1 {
			h++
		}
		return sign | h
	}
	h := sign | uint16(exp)<<10 | uint16(mant>>13)
	rem := mant & 0x1fff
	if rem > 0x1000 || rem == 0x1000 && h&1 == 1 {
		h++
	}
	return h
}

// fromHalf is IEEE 754 half precision h as a float32.
func fromHalf(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch {
	case exp == 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	case exp == 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		f := float32(mant) / 1024 / 16384
		if sign != 0 {
			return -f
		}
		return f
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | mant<<13)
}
