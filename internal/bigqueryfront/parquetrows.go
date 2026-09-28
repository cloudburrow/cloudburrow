package bigqueryfront

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apache/arrow/go/v15/parquet"
	"github.com/apache/arrow/go/v15/parquet/file"
)

// A Parquet file's rows, read by the front (#1004, #1005, #1006).
//
// A load the emulator would not carry out as BigQuery does (parquetload.go)
// is read here, with Apache Arrow's Parquet reader for Go (the page
// encodings and compressions of a data page), and each row is written as
// a GoogleSQL literal of its table's row type, which the front inserts
// itself. The reader gives each leaf column's values with their
// definition and repetition levels; the rows are put back together from
// those by the file's schema (the record assembly of the Dremel paper,
// which the Parquet format adopts:
// https://parquet.apache.org/docs/file-format/nestedencoding/), every group
// a RECORD and every repeated node a list, and then read as BigQuery reads
// them (parquetshape.go).
//
// Each value is converted exactly, or refused (pqValueError):
//
//   - INT32 and INT64 as they are; an unsigned INT(32) as unsigned; an
//     unsigned INT(64) above 9,223,372,036,854,775,807 is an error, as
//     BigQuery documents ("an error will be returned if the unsigned value
//     exceeds the maximum INTEGER value").
//   - FLOAT and DOUBLE as FLOAT64, a FLOAT widened exactly. NaN is 501:
//     the emulator's engine stores a NaN as NULL (measured: an INSERT of
//     IEEE_DIVIDE(0, 0), and of CAST(x AS FLOAT64) of 'nan', read back
//     NULL), and would lose it.
//   - DATE from days since 1970-01-01; TIME(MILLIS) and TIME(MICROS) from
//     the time since midnight; TIMESTAMP(MILLIS) and TIMESTAMP(MICROS) from
//     the time since the Unix epoch; INT96 from its nanoseconds of the day
//     and Julian day, which is 501 when it has a part of a microsecond
//     (BigQuery's TIMESTAMP holds microseconds, and its documentation does
//     not say whether it truncates or rounds). A value outside BigQuery's
//     range for its type is an error.
//   - DECIMAL: the unscaled integer (big-endian two's complement for the
//     byte arrays) at the column's scale; out of its type's range an
//     error, and 501 when it would have to be rounded to fit.
//   - BYTE_ARRAY and FIXED_LEN_BYTE_ARRAY BYTES as they are; STRING (and
//     ENUM with enumAsString) as text, 501 when it is not UTF-8.

// pqGroup is a group's value: its children's values, by position.
type pqGroup struct{ f []any }

// pqList is a repeated node's values.
type pqList struct{ items []any }

// pqValueError is a value the front does not load: status is 400
// (BigQuery refuses it: the job fails, "invalid") or 501.
type pqValueError struct {
	status int
	msg    string
}

func (e *pqValueError) Error() string { return e.msg }

func valueInvalid(format string, a ...any) error {
	return &pqValueError{status: 400, msg: fmt.Sprintf(format, a...)}
}

func valueNotHere(format string, a ...any) error {
	return &pqValueError{status: 501, msg: fmt.Sprintf(format, a...)}
}

// pqLeaf is one leaf column's levels and values in a row group, and how
// far they have been read.
type pqLeaf struct {
	node       *pqNode
	path       []*pqNode // from a top-level column down to node
	pos        []int     // each node's position among its parent's children
	defs, reps []int16
	vals       []any
	at, vat    int // the next level, and the next value
}

// readParquetRows reads the Parquet file r, of schema elems, whose columns
// BigQuery reads as cols, and calls row with each row: the value of each
// of cols, as rowValues makes it.
func readParquetRows(r parquet.ReaderAtSeeker, elems []pqElement, cols []*pqCol, row func([]any) error) (n int64, err error) {
	// The reader can panic on a malformed file; that is an error here, not
	// the front's end.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the Parquet reader failed: %v", p)
		}
	}()
	root, leaves, err := parquetTree(elems)
	if err != nil {
		return 0, err
	}
	rd, err := file.NewParquetReader(r)
	if err != nil {
		return 0, err
	}
	defer rd.Close()
	if rd.MetaData().Schema.NumColumns() != len(leaves) {
		return 0, fmt.Errorf("the file has %d leaf columns, and its schema %d", rd.MetaData().Schema.NumColumns(), len(leaves))
	}
	for g := range rd.NumRowGroups() {
		rg := rd.RowGroup(g)
		data := make([]*pqLeaf, len(leaves))
		for i, lf := range leaves {
			cr, err := rg.Column(i)
			if err != nil {
				return n, err
			}
			d := cr.Descriptor()
			if int(d.MaxDefinitionLevel()) != lf.def || int(d.MaxRepetitionLevel()) != lf.rep {
				return n, fmt.Errorf("column %s has levels %d and %d, and its schema %d and %d", d.Path(),
					d.MaxDefinitionLevel(), d.MaxRepetitionLevel(), lf.def, lf.rep)
			}
			if data[i], err = readLeaf(cr, lf); err != nil {
				return n, fmt.Errorf("column %s: %w", d.Path(), err)
			}
			data[i].path = nodePath(root, lf)
			for k, pn := range data[i].path {
				parent := root
				if k > 0 {
					parent = data[i].path[k-1]
				}
				data[i].pos = append(data[i].pos, childIndex(parent, pn))
			}
		}
		rows := rg.NumRows()
		for range rows {
			rec := &pqGroup{f: make([]any, len(root.kids))}
			for _, lf := range data {
				if err := lf.assemble(rec); err != nil {
					return n, err
				}
			}
			vals := make([]any, len(cols))
			for i, c := range cols {
				v, err := c.value(rec.f[i])
				if err != nil {
					return n, err
				}
				vals[i] = v
			}
			if err := row(vals); err != nil {
				return n, err
			}
			n++
		}
		for _, lf := range data {
			if lf.at != len(lf.defs) {
				return n, fmt.Errorf("column %s has more values than the row group's %d rows", lf.node.e.Name, rows)
			}
		}
	}
	return n, nil
}

// readLeaf reads all of a column chunk's levels and values.
func readLeaf(cr file.ColumnChunkReader, n *pqNode) (*pqLeaf, error) {
	const batch = 4096
	lf := &pqLeaf{node: n}
	defs, reps := make([]int16, batch), make([]int16, batch)
	add := func(total int64) {
		for i := range int(total) {
			d, r := int16(0), int16(0)
			if n.def > 0 {
				d = defs[i]
			}
			if n.rep > 0 {
				r = reps[i]
			}
			lf.defs, lf.reps = append(lf.defs, d), append(lf.reps, r)
		}
	}
	for cr.HasNext() {
		var total int64
		var err error
		switch c := cr.(type) {
		case *file.BooleanColumnChunkReader:
			v := make([]bool, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, x)
			}
		case *file.Int32ColumnChunkReader:
			v := make([]int32, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, x)
			}
		case *file.Int64ColumnChunkReader:
			v := make([]int64, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, x)
			}
		case *file.Int96ColumnChunkReader:
			v := make([]parquet.Int96, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, [12]byte(x))
			}
		case *file.Float32ColumnChunkReader:
			v := make([]float32, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, x)
			}
		case *file.Float64ColumnChunkReader:
			v := make([]float64, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, x)
			}
		case *file.ByteArrayColumnChunkReader:
			v := make([]parquet.ByteArray, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, append([]byte{}, x...))
			}
		case *file.FixedLenByteArrayColumnChunkReader:
			v := make([]parquet.FixedLenByteArray, batch)
			var got int
			total, got, err = c.ReadBatch(batch, v, defs, reps)
			for _, x := range v[:got] {
				lf.vals = append(lf.vals, append([]byte{}, x...))
			}
		default:
			return nil, fmt.Errorf("a column of physical type %s", cr.Type())
		}
		if err != nil {
			return nil, err
		}
		if total == 0 {
			break
		}
		add(total)
	}
	if err := cr.Err(); err != nil {
		return nil, err
	}
	return lf, nil
}

// path returns the nodes from root's child down to n.
func nodePath(root, n *pqNode) []*pqNode {
	var walk func(at *pqNode) []*pqNode
	walk = func(at *pqNode) []*pqNode {
		if at == n {
			return []*pqNode{at}
		}
		for _, k := range at.kids {
			if p := walk(k); p != nil {
				return append([]*pqNode{at}, p...)
			}
		}
		return nil
	}
	p := walk(root)
	if len(p) > 0 {
		p = p[1:]
	}
	return p
}

// assemble puts the leaf's values for the next row into rec, the row's
// root group: each of its levels from a repetition level of 0 up to the
// next one.
func (lf *pqLeaf) assemble(rec *pqGroup) error {
	path := lf.path
	idx := make([]int, len(path))
	first := true
	for lf.at < len(lf.defs) && (first || lf.reps[lf.at] > 0) {
		d, r := int(lf.defs[lf.at]), int(lf.reps[lf.at])
		if first && r != 0 {
			return fmt.Errorf("column %s starts a row at repetition level %d", lf.node.e.Name, r)
		}
		first = false
		lf.at++
		// Which list each repeated node on the path adds to.
		for i, n := range path {
			if n.e.Repetition != pqRepeated {
				continue
			}
			switch {
			case r == 0 || n.rep > r:
				idx[i] = 0
			case n.rep == r:
				idx[i]++
			}
		}
		parent := rec
		for i, n := range path {
			slot := &parent.f[lf.pos[i]]
			if d < n.def {
				// Not defined here: a NULL, or an empty list.
				if n.e.Repetition == pqRepeated && *slot == nil {
					*slot = &pqList{}
				}
				break
			}
			leaf := i == len(path)-1
			var val any
			if leaf {
				if lf.vat >= len(lf.vals) {
					return fmt.Errorf("column %s has fewer values than its levels", lf.node.e.Name)
				}
				val = lf.vals[lf.vat]
				lf.vat++
			}
			if n.e.Repetition == pqRepeated {
				l, _ := (*slot).(*pqList)
				if l == nil {
					l = &pqList{}
					*slot = l
				}
				if idx[i] > len(l.items) {
					return fmt.Errorf("column %s skips an element of %s", lf.node.e.Name, n.e.Name)
				}
				if idx[i] == len(l.items) {
					if leaf {
						l.items = append(l.items, val)
					} else {
						l.items = append(l.items, &pqGroup{f: make([]any, len(n.kids))})
					}
				} else if leaf {
					return fmt.Errorf("column %s repeats an element of %s", lf.node.e.Name, n.e.Name)
				}
				if !leaf {
					parent = l.items[idx[i]].(*pqGroup)
				}
				continue
			}
			if leaf {
				*slot = val
				break
			}
			g, _ := (*slot).(*pqGroup)
			if g == nil {
				g = &pqGroup{f: make([]any, len(n.kids))}
				*slot = g
			}
			parent = g
		}
	}
	if first {
		return fmt.Errorf("column %s has fewer rows than its row group", lf.node.e.Name)
	}
	return nil
}

func childIndex(parent, n *pqNode) int {
	for i, k := range parent.kids {
		if k == n {
			return i
		}
	}
	return -1
}

// value converts a node's value, as assembled, to what c loads: a
// GoogleSQL literal (a string), nil for NULL, a []any for a REPEATED
// field and a map of lower-cased field names to values for a RECORD.
func (c *pqCol) value(v any) (any, error) {
	switch c.how {
	case pqHowValue:
		if v == nil {
			return nil, nil
		}
		return c.scalar.literal(c.Name, v)
	case pqHowGroup:
		g, _ := v.(*pqGroup)
		if g == nil {
			return nil, nil
		}
		m := make(map[string]any, len(c.kids))
		for i, k := range c.kids {
			x, err := k.value(g.f[i])
			if err != nil {
				return nil, err
			}
			m[strings.ToLower(k.Name)] = x
		}
		return m, nil
	case pqHowRepeated:
		l, _ := v.(*pqList)
		out := []any{}
		if l == nil {
			return out, nil
		}
		for _, it := range l.items {
			x, err := c.elem.value(it)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	case pqHowList, pqHowMapArray:
		out := []any{}
		g, _ := v.(*pqGroup)
		if g == nil {
			return out, nil // a NULL list or map: an empty REPEATED field
		}
		l, _ := g.f[0].(*pqList)
		if l == nil {
			return out, nil
		}
		for _, it := range l.items {
			var x any
			var err error
			if c.how == pqHowList {
				x, err = c.elem.value(it.(*pqGroup).f[0])
				if err == nil && x == nil {
					return nil, valueNotHere("a NULL element in the LIST column %s, read with enableListInference: BigQuery's "+
						"arrays hold no NULL, and its documentation does not say how a load reads one", c.Name)
				}
			} else {
				x, err = c.elem.value(it)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	}
	return nil, fmt.Errorf("column %s: no way to read it", c.Name)
}

// BigQuery's ranges (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types).
var (
	minTimestamp = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTimestamp = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	maxNumeric   = mustRat("99999999999999999999999999999.999999999")
	// BIGNUMERIC's range is ±5.7896044618658097711785492504343953926634992332820282019728792003956564819967E+38
	// with 38 fractional digits: the 256-bit integers at scale 38.
	maxBignumeric = new(big.Rat).SetFrac(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1)),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(38), nil))
	minBignumeric = new(big.Rat).SetFrac(new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 255)),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(38), nil))
)

func mustRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

// julianUnixEpoch is the Julian day number of 1970-01-01, which an INT96
// timestamp counts days from.
const julianUnixEpoch = 2440588

// literal writes a primitive value, as the reader gives it, as a
// GoogleSQL literal of s's type.
func (s pqScalar) literal(col string, v any) (string, error) {
	bad := func() (string, error) { return "", fmt.Errorf("column %s: a value of type %T", col, v) }
	switch s.conv {
	case pqToBool:
		b, ok := v.(bool)
		if !ok {
			return bad()
		}
		return strings.ToUpper(strconv.FormatBool(b)), nil
	case pqToInt32:
		x, ok := v.(int32)
		if !ok {
			return bad()
		}
		return strconv.FormatInt(int64(x), 10), nil
	case pqToUint32:
		x, ok := v.(int32)
		if !ok {
			return bad()
		}
		return strconv.FormatUint(uint64(uint32(x)), 10), nil
	case pqToInt64:
		x, ok := v.(int64)
		if !ok {
			return bad()
		}
		return strconv.FormatInt(x, 10), nil
	case pqToUint64:
		x, ok := v.(int64)
		if !ok {
			return bad()
		}
		if x < 0 {
			return "", valueInvalid("Error while reading data: column %s's unsigned INT64 value %d exceeds the maximum "+
				"INTEGER value of 9,223,372,036,854,775,807.", col, uint64(x))
		}
		return strconv.FormatInt(x, 10), nil
	case pqToFloat32, pqToFloat64:
		var f float64
		switch x := v.(type) {
		case float32:
			f = float64(x)
		case float64:
			f = x
		default:
			return bad()
		}
		return floatLiteral(col, f)
	case pqToDateDays:
		x, ok := v.(int32)
		if !ok {
			return bad()
		}
		t := time.Unix(0, 0).UTC().AddDate(0, 0, int(x))
		if t.Year() < 1 || t.Year() > 9999 {
			return "", valueInvalid("Error while reading data: column %s's DATE value, %d days from 1970-01-01, is "+
				"outside BigQuery's range of 0001-01-01 to 9999-12-31.", col, x)
		}
		return "DATE '" + t.Format("2006-01-02") + "'", nil
	case pqToTimeMillis32, pqToTimeMicros64:
		var us int64
		switch x := v.(type) {
		case int32:
			us = int64(x) * 1000
		case int64:
			us = x
		default:
			return bad()
		}
		if us < 0 || us >= 86400*1e6 {
			return "", valueInvalid("Error while reading data: column %s's TIME value, %d microseconds after midnight, is "+
				"outside a day.", col, us)
		}
		t := time.UnixMicro(us).UTC()
		return "TIME '" + t.Format("15:04:05.000000") + "'", nil
	case pqToTsMillis, pqToTsMicros:
		x, ok := v.(int64)
		if !ok {
			return bad()
		}
		us := x
		if s.conv == pqToTsMillis {
			if x > math.MaxInt64/1000 || x < math.MinInt64/1000 {
				return tsOutOfRange(col)
			}
			us = x * 1000
		}
		return timestampLiteral(col, time.UnixMicro(us).UTC())
	case pqToInt96:
		b, ok := v.([12]byte)
		if !ok {
			return bad()
		}
		nanos := binary.LittleEndian.Uint64(b[:8])
		day := int64(binary.LittleEndian.Uint32(b[8:]))
		if nanos >= 86400*1e9 {
			return "", valueInvalid("Error while reading data: column %s's INT96 value has %d nanoseconds in a day.", col, nanos)
		}
		if nanos%1000 != 0 {
			return "", valueNotHere("column %s's INT96 value has a part of a microsecond (%d nanoseconds into its day): "+
				"BigQuery's TIMESTAMP holds microseconds, and its documentation does not say whether a load truncates or "+
				"rounds", col, nanos)
		}
		days := day - julianUnixEpoch
		if days < -800000 || days > 3000000 {
			return tsOutOfRange(col)
		}
		t := time.Unix(days*86400, int64(nanos)).UTC()
		return timestampLiteral(col, t)
	case pqToDecimal:
		var unscaled *big.Int
		switch x := v.(type) {
		case int32:
			unscaled = big.NewInt(int64(x))
		case int64:
			unscaled = big.NewInt(x)
		case []byte:
			unscaled = twosComplement(x)
		default:
			return bad()
		}
		return decimalLiteral(col, s.bq, unscaled, s.scale)
	case pqToBytes:
		b, ok := v.([]byte)
		if !ok {
			return bad()
		}
		return sqlBytes(b), nil
	case pqToString:
		b, ok := v.([]byte)
		if !ok {
			return bad()
		}
		if !utf8.Valid(b) {
			return "", valueNotHere("column %s has a STRING value that is not UTF-8, which BigQuery's documentation does "+
				"not say how a load reads", col)
		}
		return sqlString(string(b)), nil
	}
	return bad()
}

func tsOutOfRange(col string) (string, error) {
	return "", valueInvalid("Error while reading data: column %s's TIMESTAMP value is outside BigQuery's range of "+
		"0001-01-01 00:00:00 to 9999-12-31 23:59:59.999999 UTC.", col)
}

func timestampLiteral(col string, t time.Time) (string, error) {
	if t.Before(minTimestamp) || t.After(maxTimestamp) {
		return tsOutOfRange(col)
	}
	return "TIMESTAMP '" + t.Format("2006-01-02 15:04:05.000000") + "+00'", nil
}

// floatLiteral writes a FLOAT64. The emulator folds a CAST of a string to
// FLOAT64 in an INSERT's VALUES and refuses it ("failed to format query":
// measured, CAST('inf' AS FLOAT64)), so the infinities are IEEE_DIVIDE of
// ±1 by 0, which it takes (measured).
func floatLiteral(col string, f float64) (string, error) {
	switch {
	case math.IsNaN(f):
		return "", valueNotHere("column %s has a NaN, which the emulator behind CloudBurrow stores as NULL (measured)", col)
	case math.IsInf(f, 1):
		return "IEEE_DIVIDE(1, 0)", nil
	case math.IsInf(f, -1):
		return "IEEE_DIVIDE(-1, 0)", nil
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s, nil
}

// twosComplement reads a big-endian two's complement integer.
func twosComplement(b []byte) *big.Int {
	x := new(big.Int).SetBytes(b)
	if len(b) > 0 && b[0]&0x80 != 0 {
		x.Sub(x, new(big.Int).Lsh(big.NewInt(1), uint(8*len(b))))
	}
	return x
}

// decimalLiteral writes unscaled × 10^-scale as a NUMERIC or BIGNUMERIC
// literal.
func decimalLiteral(col, typ string, unscaled *big.Int, scale int) (string, error) {
	r := new(big.Rat).SetFrac(unscaled, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	frac, maxRat, minRat := numericFrac, maxNumeric, new(big.Rat).Neg(maxNumeric)
	if typ == "BIGNUMERIC" {
		frac, maxRat, minRat = bignumericFrac, maxBignumeric, minBignumeric
	}
	// Digits past the type's scale must be zeros: rounding them is not
	// documented.
	shifted := new(big.Rat).Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(frac)), nil)))
	if !shifted.IsInt() {
		return "", valueNotHere("column %s's DECIMAL value %s has more than the %d fractional digits of %s, and BigQuery's "+
			"documentation does not say how a load rounds it", col, r.FloatString(scale), frac, typ)
	}
	if r.Cmp(maxRat) > 0 || r.Cmp(minRat) < 0 {
		return "", valueInvalid("Error while reading data: column %s's DECIMAL value %s exceeds the range of %s.", col,
			r.FloatString(scale), typ)
	}
	digits := scale
	if digits > frac {
		digits = frac
	}
	return typ + " '" + r.FloatString(digits) + "'", nil
}

// sqlString quotes s as a GoogleSQL string literal, escaping all but
// printable ASCII.
func sqlString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, c := range s {
		switch {
		case c == '\'' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c >= 0x20 && c < 0x7f:
			b.WriteRune(c)
		default:
			fmt.Fprintf(&b, `\U%08x`, c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// sqlBytes quotes b as a GoogleSQL bytes literal.
func sqlBytes(b []byte) string {
	var s strings.Builder
	s.WriteString("b'")
	for _, c := range b {
		switch {
		case c == '\'' || c == '\\':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			s.WriteByte(c)
		default:
			fmt.Fprintf(&s, `\x%02x`, c)
		}
	}
	s.WriteByte('\'')
	return s.String()
}

// sqlType writes a field's GoogleSQL type: ARRAY<...> for REPEATED,
// STRUCT<...> for a RECORD.
func sqlType(f field) string {
	if strings.EqualFold(f.Mode, "REPEATED") {
		e := f
		e.Mode = "NULLABLE"
		return "ARRAY<" + sqlType(e) + ">"
	}
	if isRecord(f.Type) {
		parts := make([]string, len(f.Fields))
		for i, k := range f.Fields {
			parts[i] = quoteName(k.Name) + " " + sqlType(k)
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	}
	return canonicalType(f.Type)
}

// literalFor writes v, a value as pqCol.value makes it, as a literal of
// the table's field t: a RECORD's fields by t's, those v lacks NULL, and a
// REPEATED field v lacks empty.
func literalFor(t field, v any) (string, error) {
	if strings.EqualFold(t.Mode, "REPEATED") {
		items, _ := v.([]any)
		e := t
		e.Mode = "NULLABLE"
		parts := make([]string, len(items))
		for i, it := range items {
			if it == nil {
				return "", errors.New("a NULL in a REPEATED field " + t.Name)
			}
			s, err := literalFor(e, it)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return sqlType(t) + "[" + strings.Join(parts, ", ") + "]", nil
	}
	if v == nil {
		return "NULL", nil
	}
	if isRecord(t.Type) {
		m, ok := v.(map[string]any)
		if !ok {
			return "", errors.New("field " + t.Name + " is not a RECORD's value")
		}
		parts := make([]string, len(t.Fields))
		for i, k := range t.Fields {
			s, err := literalFor(k, m[strings.ToLower(k.Name)])
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return sqlType(t) + "(" + strings.Join(parts, ", ") + ")", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", errors.New("field " + t.Name + " is not a scalar's value")
	}
	return s, nil
}

// readerAtSeeker is an io.ReaderAt of a known size that also seeks, as the
// Parquet reader needs.
type readerAtSeeker struct {
	io.ReaderAt
	size, off int64
}

func (r *readerAtSeeker) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-r.off {
		p = p[:r.size-r.off]
	}
	n, err := r.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}

func (r *readerAtSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.off
	case io.SeekEnd:
		off += r.size
	default:
		return 0, errors.New("bad whence")
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	r.off = off
	return off, nil
}
