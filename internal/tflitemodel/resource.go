package tflitemodel

import (
	"fmt"
)

// LiteRT's resource operators (#1775), which text models' vocabularies and
// TFLiteConverter's resource variables use: HASHTABLE, HASHTABLE_IMPORT,
// HASHTABLE_FIND and HASHTABLE_SIZE (kernels/hashtable*.cc, a
// StaticHashtable that imports once, keeping a repeated key's first
// value), VAR_HANDLE, READ_VARIABLE and ASSIGN_VARIABLE, and CALL_ONCE,
// which runs its initialization subgraph once. A resource lives for one
// Run: each Run starts from the model's initial state, as a fresh LiteRT
// interpreter does.

// runState is one Run's resources.
type runState struct {
	tables map[int]*hashtable
	vars   map[int]*Tensor
	varIDs map[[2]string]int
	once   map[int]bool
}

func newRunState() *runState {
	return &runState{tables: map[int]*hashtable{}, vars: map[int]*Tensor{}, varIDs: map[[2]string]int{}, once: map[int]bool{}}
}

type hashtable struct {
	keyType, valType Type
	byS              map[string]int
	byI              map[int64]int
	vals             *Tensor
	ready            bool
}

// handle is a resource tensor's id.
func handle(o op, t *Tensor) (int, error) {
	if t == nil || t.Len() != 1 || t.I == nil {
		return 0, fmt.Errorf("operator %s takes a resource handle", o.name)
	}
	return int(t.I[0]), nil
}

func resourceTensor(id int) *Tensor {
	return &Tensor{Type: Resrc, Shape: []int{1}, I: []int64{int64(id)}}
}

func (st *runState) table(o op, t *Tensor) (*hashtable, error) {
	id, err := handle(o, t)
	if err != nil {
		return nil, err
	}
	h := st.tables[id]
	if h == nil {
		return nil, fmt.Errorf("operator %s: no hash table has id %d", o.name, id)
	}
	return h, nil
}

func keyKind(t Type) bool { return t == String || t.IsInt() }

func (m *Model) evalResource(o op, a []*Tensor, depth int, st *runState) ([]*Tensor, error) {
	arg := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	switch o.code {
	case opCallOnce:
		k := int(o.i32(4, -1))
		if k < 0 || k >= len(m.graphs) {
			return nil, fmt.Errorf("operator CALL_ONCE names subgraph %d, which the model does not have", k)
		}
		if !st.once[k] {
			st.once[k] = true
			if _, err := m.call(k, nil, depth, st); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case opHashtable:
		id := int(o.i32(4, 0))
		kt, vt := Type(o.i8(6, 0)), Type(o.i8(8, 0))
		if !keyKind(kt) || !(keyKind(vt) || vt.IsFloat()) {
			return nil, &UnsupportedError{fmt.Sprintf("HASHTABLE of %s keys and %s values is not supported", kt, vt)}
		}
		if st.tables[id] == nil {
			st.tables[id] = &hashtable{keyType: kt, valType: vt, byS: map[string]int{}, byI: map[int64]int{}}
		}
		return []*Tensor{resourceTensor(id)}, nil
	case opHashtableImport:
		h, err := st.table(o, arg(0))
		if err != nil {
			return nil, err
		}
		keys, vals := arg(1), arg(2)
		if keys == nil || vals == nil || keys.Len() != vals.Len() {
			return nil, fmt.Errorf("operator HASHTABLE_IMPORT takes as many values as keys")
		}
		if h.ready {
			return nil, nil
		}
		h.ready = true
		h.vals = vals
		for i := range keys.Len() {
			if keys.S != nil {
				if _, ok := h.byS[keys.S[i]]; !ok {
					h.byS[keys.S[i]] = i
				}
			} else if keys.I != nil {
				if _, ok := h.byI[keys.I[i]]; !ok {
					h.byI[keys.I[i]] = i
				}
			}
		}
		return nil, nil
	case opHashtableFind:
		h, err := st.table(o, arg(0))
		if err != nil {
			return nil, err
		}
		keys, def := arg(1), arg(2)
		if keys == nil || def == nil || def.Len() != 1 {
			return nil, fmt.Errorf("operator HASHTABLE_FIND takes keys and one default value")
		}
		r := alloc(def, keys.Shape)
		r.Type = def.Type
		for i := range keys.Len() {
			j, ok := -1, false
			switch {
			case keys.S != nil:
				j, ok = h.byS[keys.S[i]]
			case keys.I != nil:
				j, ok = h.byI[keys.I[i]]
			default:
				return nil, fmt.Errorf("operator HASHTABLE_FIND: keys of type %s", keys.Type)
			}
			if ok && h.vals != nil {
				at(r, i, h.vals, j)
			} else {
				at(r, i, def, 0)
			}
		}
		return []*Tensor{r}, nil
	case opHashtableSize:
		h, err := st.table(o, arg(0))
		if err != nil {
			return nil, err
		}
		return []*Tensor{{Type: Int64, Shape: []int{1}, I: []int64{int64(len(h.byS) + len(h.byI))}}}, nil
	case opVarHandle:
		key := [2]string{"", ""}
		if o.opts != nil {
			key = [2]string{str(o.opts, 4), str(o.opts, 6)}
		}
		id, ok := st.varIDs[key]
		if !ok {
			id = len(st.varIDs)
			st.varIDs[key] = id
		}
		return []*Tensor{resourceTensor(id)}, nil
	case opReadVariable:
		id, err := handle(o, arg(0))
		if err != nil {
			return nil, err
		}
		v := st.vars[id]
		if v == nil {
			return nil, fmt.Errorf("operator READ_VARIABLE reads a variable no operator has assigned")
		}
		c := *v
		return []*Tensor{&c}, nil
	case opAssignVariable:
		id, err := handle(o, arg(0))
		if err != nil {
			return nil, err
		}
		if arg(1) == nil {
			return nil, fmt.Errorf("operator ASSIGN_VARIABLE takes a value")
		}
		st.vars[id] = arg(1)
		return nil, nil
	}
	return nil, &UnsupportedError{"operator " + o.name + " is not supported"}
}
