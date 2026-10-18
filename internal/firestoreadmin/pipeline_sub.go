package firestoreadmin

// Subqueries and the last of Google's pipeline functions (#1613).
//
// Subqueries are expressions over a pipeline, as Google's "Perform joins
// with subqueries" guide documents them: array(pipeline) (the client's
// ToArrayExpression) is the subquery's results, an empty array when there
// are none; scalar(pipeline) (ToScalarExpression) is its one result, null
// when there is none and an error when there are more, a result with a
// single field elevated to that field's value and one with more a map.
// The subquery is evaluated once for each row of the outer pipeline: its
// field references read its own documents, and the variables the outer
// pipeline bound with let are visible to it through variable references.
//
// The subcollection stage is a subquery's input stage: Google documents it
// as "syntactic sugar" over binding the outer document's __name__ with let
// and a collection_group input filtered on parent(__name__) equal to it,
// that is, the documents of the named collection under the outer document.
//
// storage_size is the size Google's "Storage size calculations" page gives
// a value, and reference_slice slices a reference's (collection ID,
// document ID) segments as array_slice slices an array.

import (
	"strings"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkSubquery refuses a stage where it cannot stand: subcollection
// outside a subquery, and the write stages inside one.
func checkSubquery(name string, outer *row) error {
	switch {
	case name == "subcollection" && outer == nil:
		return status.Error(codes.InvalidArgument, "the subcollection stage must be used within a subquery: an array or scalar expression over the pipeline, in a stage of a parent pipeline.")
	case outer != nil && (name == "update" || name == "delete"):
		return status.Errorf(codes.InvalidArgument, "the %s stage cannot be used in a subquery.", name)
	}
	return nil
}

// withOuterVars gives each of a subquery's input rows the variables of the
// outer row.
func withOuterVars(rows []row, outer *row) []row {
	if len(outer.vars) == 0 {
		return rows
	}
	for i := range rows {
		vars := make(map[string]*firestorepb.Value, len(outer.vars)+len(rows[i].vars))
		for k, v := range outer.vars {
			vars[k] = v
		}
		for k, v := range rows[i].vars {
			vars[k] = v
		}
		rows[i].vars = vars
	}
	return rows
}

// subcollection is the documents of the collection path (a collection ID,
// or a longer collection path) under the outer row's document. An outer
// row with no document name, after a select say, has none.
func (p *pipelineRun) subcollection(st *firestorepb.Pipeline_Stage, outer *row) ([]row, error) {
	var path string
	if len(st.GetArgs()) == 1 {
		a := st.GetArgs()[0]
		path = a.GetStringValue()
		if path == "" {
			path = a.GetReferenceValue()
		}
	}
	path = strings.Trim(path, "/")
	if path == "" || strings.Count(path, "/")%2 != 0 {
		return nil, stageErr("subcollection", "a collection ID, such as reviews, is required.")
	}
	if !strings.HasPrefix(outer.name, p.db+"/documents/") {
		return nil, nil
	}
	parent := outer.name + "/" + path
	docs, err := p.all()
	if err != nil {
		return nil, err
	}
	var out []row
	for _, d := range docs {
		if i := strings.LastIndex(d.Name, "/"); i >= 0 && d.Name[:i] == parent {
			out = append(out, docRow(d))
		}
	}
	return out, nil
}

// subquery evaluates the pipeline argument of the function name for the
// outer row r.
func (p *pipelineRun) subquery(name string, args []*firestorepb.Value, r row) ([]row, error) {
	if len(args) != 1 || args[0].GetPipelineValue() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "the function %q takes one pipeline.", name)
	}
	stats := p.stats
	rows, err := p.runIn(args[0].GetPipelineValue(), &r)
	p.stats = stats
	return rows, err
}

func isSubquery(args []*firestorepb.Value) bool {
	return len(args) == 1 && args[0].GetPipelineValue() != nil
}

// storageSize is the size of v, by the rules of Google's "Storage size
// calculations" page: a string is its UTF-8 bytes + 1, a boolean or null
// 1, an integer, floating-point number or timestamp 8, a geographical
// point 16, bytes their length, a vector 8 bytes a dimension, an array
// the sum of its values, a reference its document name's size, and a map
// "calculated the same way as document size": the string size of each
// field name, plus each field value's size, plus 32 bytes.
func storageSize(v *firestorepb.Value) int64 {
	switch x := v.GetValueType().(type) {
	case *firestorepb.Value_NullValue, *firestorepb.Value_BooleanValue:
		return 1
	case *firestorepb.Value_IntegerValue, *firestorepb.Value_DoubleValue, *firestorepb.Value_TimestampValue:
		return 8
	case *firestorepb.Value_GeoPointValue:
		return 16
	case *firestorepb.Value_StringValue:
		return int64(len(x.StringValue)) + 1
	case *firestorepb.Value_BytesValue:
		return int64(len(x.BytesValue))
	case *firestorepb.Value_ReferenceValue:
		return documentNameSize(x.ReferenceValue)
	case *firestorepb.Value_ArrayValue:
		var n int64
		for _, e := range x.ArrayValue.GetValues() {
			n += storageSize(e)
		}
		return n
	case *firestorepb.Value_MapValue:
		if isVector(v) {
			return 8 * int64(len(x.MapValue.GetFields()["value"].GetArrayValue().GetValues()))
		}
		var n int64 = 32
		for k, e := range x.MapValue.GetFields() {
			n += int64(len(k)) + 1 + storageSize(e)
		}
		return n
	}
	return 0
}

// splitReference splits a reference into its database's documents root
// (projects/P/databases/D/documents) and the path's segments under it.
func splitReference(ref string) (root string, segs []string) {
	const docs = "/documents"
	i := strings.Index(ref+"/", docs+"/")
	if i < 0 {
		return "", strings.Split(strings.Trim(ref, "/"), "/")
	}
	root = ref[:i+len(docs)]
	if rest := strings.Trim(ref[i+len(docs):], "/"); rest != "" {
		segs = strings.Split(rest, "/")
	}
	return root, segs
}

// documentNameSize is "the sum of the size of each collection ID and
// document ID in the path to the document", each a string, + 16 bytes.
func documentNameSize(ref string) int64 {
	_, segs := splitReference(ref)
	var n int64 = 16
	for _, s := range segs {
		n += int64(len(s)) + 1
	}
	return n
}

func init() {
	f := pipelineFunctions
	array := f["array"]
	f["array"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if !isSubquery(args) {
			return array(p, args, r)
		}
		rows, err := p.subquery("array", args, r)
		if err != nil {
			return nil, err
		}
		out := make([]*firestorepb.Value, len(rows))
		for i, sr := range rows {
			out[i] = mapValue(cloneFields(sr.fields))
		}
		return arrayValue(out), nil
	}
	f["scalar"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		rows, err := p.subquery("scalar", args, r)
		if err != nil {
			return nil, err
		}
		switch len(rows) {
		case 0:
			return nullValue(), nil
		case 1:
		default:
			return nil, evalErrf("scalar: the subquery evaluated to %d results, and a scalar subquery must evaluate to at most one", len(rows))
		}
		if len(rows[0].fields) == 1 {
			for _, v := range rows[0].fields {
				return v, nil
			}
		}
		return mapValue(cloneFields(rows[0].fields)), nil
	}
	f["storage_size"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 1 {
			return nil, status.Errorf(codes.InvalidArgument, "the function %q takes 1 arguments, not %d.", "storage_size", len(args))
		}
		v, err := p.eval(args[0], r)
		if err != nil || v == nil {
			return nil, err
		}
		return intValue(storageSize(v)), nil
	}
	f["reference_slice"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) < 2 || len(args) > 3 {
			return nil, status.Errorf(codes.InvalidArgument, "the function %q takes 2 to 3 arguments, not %d.", "reference_slice", len(args))
		}
		vs := make([]*firestorepb.Value, len(args))
		for i, a := range args {
			v, err := p.eval(a, r)
			if err != nil {
				return nil, err
			}
			vs[i] = v
		}
		if absent(vs[0]) {
			return nullValue(), nil
		}
		ref, ok := vs[0].GetValueType().(*firestorepb.Value_ReferenceValue)
		if !ok {
			return nil, evalErrf("reference_slice: %s is not a reference", typeName(vs[0]))
		}
		root, segs := splitReference(ref.ReferenceValue)
		if len(segs)%2 != 0 || root == "" {
			return nil, evalErrf("reference_slice: %q is not a document reference", ref.ReferenceValue)
		}
		n := int64(len(segs) / 2)
		off, ok := asInt(vs[1])
		if !ok {
			return nil, evalErrf("reference_slice: the offset must be an integer")
		}
		if off < 0 {
			off = max(n+off, 0)
		}
		off = min(off, n)
		end := n
		if len(vs) == 3 {
			l, ok := asInt(vs[2])
			if !ok || l < 0 {
				return nil, evalErrf("reference_slice: the length must be a non-negative integer")
			}
			end = min(off+l, end)
		}
		out := root
		if end > off {
			out += "/" + strings.Join(segs[2*off:2*end], "/")
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: out}}, nil
	}
}
