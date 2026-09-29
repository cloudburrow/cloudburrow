package bigqueryfront

import (
	"errors"
	"fmt"
	"strings"
)

// How a Parquet file's schema becomes a BigQuery schema (#1004, #1005).
//
// BigQuery's rules, from
// https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-parquet
// and the load job's options
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad,
// ParquetOptions):
//
//   - A column's type by the "Type conversions" table (parquetType,
//     pqScalar). "Other combinations of Parquet types and converted types
//     are not supported", so any other is 501 here.
//   - "Nested groups are converted into STRUCT types": a group is a RECORD
//     of its fields, a REQUIRED one REQUIRED and an OPTIONAL one NULLABLE.
//     A repeated column or group is REPEATED. That includes a LIST (or
//     MAP) group read without inference: its repeated group is a REPEATED
//     RECORD inside it, as written (for pyarrow's LIST, l RECORD { list
//     REPEATED RECORD { element } }).
//   - parquetOptions.enableListInference: a LIST "in the standard form"
//     (a group annotated LIST whose one field is a repeated group named
//     list whose one field is named element) "is treated as if the node
//     has the following schema: repeated <element-type> <name>". The
//     backward-compatible forms BigQuery also infers are 501 here: no
//     writer the front is tested against writes them. An element that is
//     itself REPEATED (a list of lists) is 501: BigQuery has no arrays of
//     arrays, and the page does not say what it does. A NULL element is
//     501 when read (BigQuery's arrays hold no NULL, and the page does not
//     say what a load makes of one); a NULL list is an empty one, as a
//     REPEATED column holds no NULL.
//   - parquetOptions.mapTargetType: unset, "the map will have the
//     following schema: struct map_field_name { repeated struct key_value
//     { key value } }", the nested-group rule; ARRAY_OF_STRUCT, "repeated
//     struct map_field_name { key value }", a NULL map an empty one.
//   - parquetOptions.enumAsString: an ENUM column is STRING, else BYTES
//     ("Indicates whether to infer Parquet ENUM logical type as STRING
//     instead of BYTES by default").
//   - decimalTargetTypes: a DECIMAL(precision, scale) is "In the order of
//     NUMERIC, BIGNUMERIC, and STRING, a type is picked if it is in the
//     specified list and if it supports the precision and the scale ... If
//     none of the listed types supports the precision and the scale, the
//     type supporting the widest range in the specified list is picked,
//     and if a value exceeds the supported range when reading the data, an
//     error will be thrown", by default ["NUMERIC"]. NUMERIC holds 29
//     integer and 9 fractional digits, BIGNUMERIC 38 and 38. STRING is 501:
//     the page does not say how a decimal is written as text.
//   - Column names, at every depth, by BigQuery's rule without flexible
//     column names (bqColumnName); names that differ only in case at one
//     level are 400.

// pqNode is a Parquet schema element with its children, and the
// definition and repetition levels a value at it has when it is present.
type pqNode struct {
	e        pqElement
	kids     []*pqNode
	def, rep int
	leaf     int // the leaf column's index, for a primitive
}

// parquetTree builds the schema's tree from its elements, depth first.
func parquetTree(elems []pqElement) (*pqNode, []*pqNode, error) {
	if len(elems) == 0 || elems[0].Children < 0 {
		return nil, nil, errors.New("its root is not a group")
	}
	var leaves []*pqNode
	i := 0
	var build func(def, rep, depth int) (*pqNode, error)
	build = func(def, rep, depth int) (*pqNode, error) {
		if i >= len(elems) {
			return nil, errors.New("a group has fewer children than it lists")
		}
		if depth > maxThriftDepth {
			return nil, errors.New("it is nested too deeply")
		}
		n := &pqNode{e: elems[i], leaf: -1}
		i++
		if depth > 0 {
			switch n.e.Repetition {
			case pqOptional:
				def++
			case pqRepeated:
				def++
				rep++
			case pqRequired:
			default:
				return nil, fmt.Errorf("column %s has no repetition", n.e.Name)
			}
		}
		n.def, n.rep = def, rep
		if n.e.Children < 0 {
			if depth == 0 {
				return nil, errors.New("its root is not a group")
			}
			n.leaf = len(leaves)
			leaves = append(leaves, n)
			return n, nil
		}
		for range n.e.Children {
			k, err := build(def, rep, depth+1)
			if err != nil {
				return nil, err
			}
			n.kids = append(n.kids, k)
		}
		return n, nil
	}
	root, err := build(0, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	if i != len(elems) {
		return nil, nil, errors.New("it has more elements than its root lists")
	}
	return root, leaves, nil
}

// How a BigQuery field reads its Parquet node's value.
const (
	pqHowValue    = iota // a primitive
	pqHowGroup           // a group: a RECORD of its fields
	pqHowRepeated        // a repeated node: REPEATED, each element by elem
	pqHowList            // a LIST group read with list inference
	pqHowMapArray        // a MAP group read as ARRAY_OF_STRUCT
)

// pqCol is a BigQuery field a Parquet node loads as: its field, how its
// value is read from the node's, and why the front does not load it,
// when it does not (a 501), found anywhere below it.
type pqCol struct {
	field
	node *pqNode
	how  int
	kids []*pqCol // pqHowGroup: the RECORD's fields, by the node's children
	elem *pqCol   // pqHowRepeated, pqHowList, pqHowMapArray: an element
	// twoLevel is a pqHowList whose repeated field is the element itself,
	// a backward-compatible LIST form (#1069), not a group around it.
	twoLevel bool
	// scalar is how a primitive's values are converted (pqHowValue).
	scalar pqScalar
	// passes reports whether the emulator's own Parquet reader loads the
	// column as BigQuery does (parquetType), for a flat column.
	passes  bool
	notHere string
}

// pqOptions are a load's options that decide how a Parquet file's schema
// is read.
type pqOptions struct {
	listInference bool
	enumAsString  bool
	mapArray      bool // mapTargetType ARRAY_OF_STRUCT
	decimalTypes  []string
}

// parquetColumns returns a Parquet schema's top-level columns, as BigQuery
// reads them with opts.
func parquetColumns(elems []pqElement, opts pqOptions) ([]*pqCol, error) {
	root, _, err := parquetTree(elems)
	if err != nil {
		return nil, err
	}
	cols, err := opts.fields(root.kids, "")
	if err != nil {
		return nil, err
	}
	return cols, nil
}

// fields maps a group's children, which must have names BigQuery tells
// apart.
func (o pqOptions) fields(kids []*pqNode, prefix string) ([]*pqCol, error) {
	var cols []*pqCol
	seen := map[string]string{}
	for _, k := range kids {
		if prev, ok := seen[strings.ToLower(k.e.Name)]; ok {
			return nil, fmt.Errorf("duplicate column names %s%s and %s%s (BigQuery's column names do not differ by case alone)",
				prefix, prev, prefix, k.e.Name)
		}
		seen[strings.ToLower(k.e.Name)] = k.e.Name
		c, err := o.column(k, prefix)
		if err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, nil
}

// column maps node n, a field of its parent.
func (o pqOptions) column(n *pqNode, prefix string) (*pqCol, error) {
	path := prefix + n.e.Name
	if !bqColumnName.MatchString(n.e.Name) {
		return &pqCol{field: field{Name: n.e.Name, Mode: "NULLABLE"}, node: n, notHere: fmt.Sprintf("%q is not a column "+
			"name BigQuery takes without flexible column names, which CloudBurrow does not implement", path)}, nil
	}
	if n.e.Repetition == pqRepeated {
		elem, err := o.single(n, prefix)
		if err != nil {
			return nil, err
		}
		c := &pqCol{field: field{Name: n.e.Name, Type: elem.Type, Mode: "REPEATED", Fields: elem.Fields}, node: n,
			how: pqHowRepeated, elem: elem, notHere: elem.notHere}
		if elem.Mode == "REPEATED" && c.notHere == "" {
			c.notHere = path + " is a repeated list, an array of arrays, which BigQuery does not have; its documentation " +
				"does not say how it loads one"
		}
		return c, nil
	}
	c, err := o.single(n, prefix)
	if err != nil {
		return nil, err
	}
	if c.Mode == "" {
		c.Mode = "NULLABLE"
		if n.e.Repetition == pqRequired {
			c.Mode = "REQUIRED"
		}
	}
	return c, nil
}

// single maps node n as one value, whatever its repetition: its field has
// no mode, but for a LIST or MAP read as an array, which is REPEATED.
func (o pqOptions) single(n *pqNode, prefix string) (*pqCol, error) {
	path := prefix + n.e.Name
	c := &pqCol{field: field{Name: n.e.Name}, node: n}
	desc := describeParquet(n.e)
	if n.e.Children < 0 {
		c.how = pqHowValue
		bq, loads := parquetType(n.e)
		s, why := o.scalarOf(n.e)
		c.Type, c.passes, c.scalar = s.bq, loads && bq != "", s
		switch {
		case why != "":
			c.notHere = path + " is " + desc + ", " + why
		case s.bq == "":
			c.notHere = path + " is " + desc + ", which BigQuery's Parquet conversion table does not list"
		}
		return c, nil
	}
	a := parquetAnnotation(n.e)
	switch {
	case len(n.kids) == 0:
		c.Type, c.how = "RECORD", pqHowGroup
		c.notHere = path + " is a group with no fields, which BigQuery has no RECORD for"
		return c, nil
	case a == "LIST" && o.listInference:
		elem, twoLevel, ok := listElement(n)
		if !ok {
			c.Type, c.Mode, c.how = "RECORD", "REPEATED", pqHowList
			c.notHere = path + " is a LIST that is neither in the standard form nor one of Parquet's backward-" +
				"compatible forms (one repeated field)"
			return c, nil
		}
		var e *pqCol
		var err error
		if twoLevel {
			// The repeated field is the element, each one present.
			e, err = o.single(elem, path+".")
			if err == nil && e.Mode == "" {
				e.Mode = "REQUIRED"
			}
		} else {
			e, err = o.column(elem, path+".")
		}
		if err != nil {
			return nil, err
		}
		c.twoLevel = twoLevel
		c.Type, c.Fields, c.Mode, c.how, c.elem, c.notHere = e.Type, e.Fields, "REPEATED", pqHowList, e, e.notHere
		if e.Mode == "REPEATED" && c.notHere == "" {
			c.notHere = path + " is a LIST of lists, which with list inference would be an array of arrays; BigQuery " +
				"has none, and its documentation does not say how it loads one"
		}
		return c, nil
	case (a == "MAP") && o.mapArray:
		kv, ok := standardMap(n)
		if !ok {
			c.Type, c.Mode, c.how = "RECORD", "REPEATED", pqHowMapArray
			c.notHere = path + " is a MAP that is not a repeated group of a key and a value, which CloudBurrow does not " +
				"read as ARRAY_OF_STRUCT"
			return c, nil
		}
		kids, err := o.fields(kv.kids, path+".")
		if err != nil {
			return nil, err
		}
		e := &pqCol{field: field{Name: n.e.Name, Type: "RECORD"}, node: kv, how: pqHowGroup, kids: kids}
		for _, k := range kids {
			e.Fields = append(e.Fields, k.field)
			if e.notHere == "" {
				e.notHere = k.notHere
			}
		}
		c.Type, c.Fields, c.Mode, c.how, c.elem, c.notHere = "RECORD", e.Fields, "REPEATED", pqHowMapArray, e, e.notHere
		return c, nil
	}
	kids, err := o.fields(n.kids, path+".")
	if err != nil {
		return nil, err
	}
	c.Type, c.how, c.kids = "RECORD", pqHowGroup, kids
	for _, k := range kids {
		c.Fields = append(c.Fields, k.field)
		if c.notHere == "" {
			c.notHere = k.notHere
		}
	}
	return c, nil
}

// listElement returns the element of a LIST as Parquet's rules give it,
// the standard form and the backward-compatible ones
// (https://github.com/apache/parquet-format/blob/master/LogicalTypes.md#backward-compatibility-rules),
// which BigQuery's list inference reads (#1069): the LIST group's one
// repeated field is the element itself (twoLevel) when it is a primitive,
// a group of several fields, or a group of one named array or
// <list name>_tuple; else it is a group of one field, the element (the
// standard form, pyarrow's item, and others).
func listElement(n *pqNode) (elem *pqNode, twoLevel, ok bool) {
	if len(n.kids) != 1 {
		return nil, false, false
	}
	r := n.kids[0]
	if r.e.Repetition != pqRepeated {
		return nil, false, false
	}
	if r.e.Children < 0 || len(r.kids) > 1 || r.e.Name == "array" || r.e.Name == n.e.Name+"_tuple" {
		if len(r.kids) == 0 && r.e.Children >= 0 {
			return nil, false, false
		}
		return r, true, true
	}
	if len(r.kids) != 1 || r.kids[0].e.Repetition == pqRepeated {
		return nil, false, false
	}
	return r.kids[0], false, true
}

// standardMap returns the key-value group of a MAP: a repeated group of a
// REQUIRED key and, optionally, a value.
func standardMap(n *pqNode) (*pqNode, bool) {
	if len(n.kids) != 1 {
		return nil, false
	}
	kv := n.kids[0]
	if kv.e.Repetition != pqRepeated || len(kv.kids) < 1 || len(kv.kids) > 2 || kv.kids[0].e.Name != "key" ||
		kv.kids[0].e.Repetition != pqRequired || (len(kv.kids) == 2 && kv.kids[1].e.Name != "value") {
		return nil, false
	}
	return kv, true
}

// Scalar conversions: how a primitive column's physical values become
// BigQuery's (pqScalar.conv).
const (
	pqToBool = iota + 1
	pqToInt32
	pqToUint32
	pqToInt64
	pqToUint64
	pqToFloat32
	pqToFloat64
	pqToDateDays
	pqToTimeMillis32
	pqToTimeMicros64
	pqToTsMillis
	pqToTsMicros
	pqToInt96
	pqToDecimal
	pqToBytes
	pqToString
)

// pqScalar is a primitive column's BigQuery type and conversion; for a
// DECIMAL, its scale.
type pqScalar struct {
	bq    string
	conv  int
	scale int
}

// scalarOf returns the BigQuery type and conversion BigQuery's table gives
// e with the load's options; or why the front does not load it, which
// completes "<column> is <its Parquet type>, ".
func (o pqOptions) scalarOf(e pqElement) (pqScalar, string) {
	a := parquetAnnotation(e)
	switch e.Type {
	case pqBoolean:
		if a == "" {
			return pqScalar{bq: "BOOLEAN", conv: pqToBool}, ""
		}
	case pqInt32:
		switch a {
		case "", "INT(8,signed)", "INT(16,signed)", "INT(32,signed)":
			return pqScalar{bq: "INTEGER", conv: pqToInt32}, ""
		case "INT(8,unsigned)", "INT(16,unsigned)", "INT(32,unsigned)":
			return pqScalar{bq: "INTEGER", conv: pqToUint32}, ""
		case "DATE":
			return pqScalar{bq: "DATE", conv: pqToDateDays}, ""
		case "TIME(MILLIS)":
			return pqScalar{bq: "TIME", conv: pqToTimeMillis32}, ""
		case "DECIMAL":
			return o.decimal(e)
		}
	case pqInt64:
		switch a {
		case "", "INT(64,signed)":
			return pqScalar{bq: "INTEGER", conv: pqToInt64}, ""
		case "INT(64,unsigned)":
			return pqScalar{bq: "INTEGER", conv: pqToUint64}, ""
		case "TIME(MICROS)":
			return pqScalar{bq: "TIME", conv: pqToTimeMicros64}, ""
		case "TIMESTAMP(MILLIS)":
			return pqScalar{bq: "TIMESTAMP", conv: pqToTsMillis}, ""
		case "TIMESTAMP(MICROS)":
			return pqScalar{bq: "TIMESTAMP", conv: pqToTsMicros}, ""
		case "DECIMAL":
			return o.decimal(e)
		}
	case pqInt96:
		if a == "" {
			return pqScalar{bq: "TIMESTAMP", conv: pqToInt96}, ""
		}
	case pqFloat:
		if a == "" {
			return pqScalar{bq: "FLOAT", conv: pqToFloat32}, ""
		}
	case pqDouble:
		if a == "" {
			return pqScalar{bq: "FLOAT", conv: pqToFloat64}, ""
		}
	case pqByteArray:
		switch a {
		case "":
			return pqScalar{bq: "BYTES", conv: pqToBytes}, ""
		case "STRING":
			return pqScalar{bq: "STRING", conv: pqToString}, ""
		case "ENUM":
			// The conversion table lists ENUM under no physical type;
			// its section gives STRING or BYTES by enumAsString.
			if o.enumAsString {
				return pqScalar{bq: "STRING", conv: pqToString}, ""
			}
			return pqScalar{bq: "BYTES", conv: pqToBytes}, ""
		}
	case pqFixedLenByteArray:
		switch a {
		case "":
			return pqScalar{bq: "BYTES", conv: pqToBytes}, ""
		case "DECIMAL":
			return o.decimal(e)
		}
	}
	return pqScalar{}, ""
}

// Decimal bounds: the integer and fractional digits NUMERIC and BIGNUMERIC
// hold (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types#decimal_types).
const (
	numericInt, numericFrac       = 29, 9
	bignumericInt, bignumericFrac = 38, 38
)

// decimal picks a DECIMAL column's type by the load's decimalTargetTypes
// (above).
func (o pqOptions) decimal(e pqElement) (pqScalar, string) {
	precision, scale := e.Precision, e.Scale
	if e.Logical != nil && e.Logical.Kind == pqLogicalDecimal {
		precision, scale = e.Logical.Precision, e.Logical.Scale
	}
	if scale == pqNone {
		scale = 0
	}
	if precision <= 0 || scale < 0 || scale > precision {
		return pqScalar{}, fmt.Sprintf("whose DECIMAL precision %d and scale %d are not a decimal's", precision, scale)
	}
	targets := o.decimalTypes
	if len(targets) == 0 {
		targets = []string{"NUMERIC"}
	}
	in := map[string]bool{}
	for _, t := range targets {
		in[strings.ToUpper(t)] = true
	}
	fits := func(intDigits, frac int) bool { return precision-scale <= intDigits && scale <= frac }
	pick := ""
	switch {
	case in["NUMERIC"] && fits(numericInt, numericFrac):
		pick = "NUMERIC"
	case in["BIGNUMERIC"] && fits(bignumericInt, bignumericFrac):
		pick = "BIGNUMERIC"
	case in["STRING"]:
		pick = "STRING"
	case in["BIGNUMERIC"]:
		pick = "BIGNUMERIC"
	case in["NUMERIC"]:
		pick = "NUMERIC"
	default:
		return pqScalar{}, "whose decimalTargetTypes (" + strings.Join(targets, ", ") + ") name none of NUMERIC, BIGNUMERIC " +
			"and STRING"
	}
	if pick == "STRING" {
		return pqScalar{bq: "STRING", conv: pqToDecimal, scale: scale}, fmt.Sprintf("a DECIMAL(%d, %d) that "+
			"decimalTargetTypes makes STRING, which BigQuery's documentation does not say how it writes", precision, scale)
	}
	return pqScalar{bq: pick, conv: pqToDecimal, scale: scale}, ""
}
