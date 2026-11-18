package bigqueryfront

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scritchley/orc"
	orcproto "github.com/scritchley/orc/proto"
)

// ORC loads (#1713).
//
// The pinned emulator has no ORC load (it answered one 500, a nil pointer
// dereference: loadtable.go). So an ORC load is the front's own job,
// carried out as an Avro load is (avroload.go: the rows go first to a
// scratch table, then into the table, so a load that fails leaves it as
// it was), with the file read by orcfile.go and its schema by "Loading ORC
// data from Cloud Storage"
// (https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-orc,
// read 2026-11-17):
//
//   - "ORC files are self-describing": the table's schema is the file's,
//     every column NULLABLE ("all fields ... are NULLABLE") but a list's or
//     map's, REPEATED.
//   - ORC conversions: boolean BOOLEAN; byte, short, int and long
//     INTEGER; float and double FLOAT; string, varchar and char STRING
//     ("only UTF-8 encoding is supported": a value that is not is 400);
//     binary BYTES; date DATE; timestamp TIMESTAMP ("ORC supports
//     nanosecond precision, but BigQuery converts sub-microsecond values to
//     microseconds when the data is read": truncated); a date or
//     timestamp outside BigQuery's range fails the job ("invalid date
//     value"). decimal NUMERIC, BIGNUMERIC or STRING by decimalTargetTypes,
//     as for Parquet and Avro (pqOptions.decimal).
//   - Complex types: struct RECORD ("All fields are NULLABLE. The field
//     order is ignored. A field name must be a valid column name": one
//     that is not is 400); map<K,V> "a REPEATED RECORD with key and value
//     fields, both NULLABLE"; list "repeated fields. Nested lists and
//     lists of maps are not supported" (400) and "NULL list elements are
//     ignored during load"; union: "a single-variant union becomes a
//     NULLABLE field", otherwise "a RECORD with a list of NULLABLE fields.
//     The NULLABLE fields have suffixes such as field_0, field_1, and so
//     on. Only one of these fields is assigned a value when the data is
//     read".
//   - Compression: "Zlib, Snappy, LZO, LZ4, and ZSTD" (orcfile.go).
//   - Several files: as for Avro, files whose columns differ are 501
//     (BigQuery takes "the alphabetically last file").
//   - Write dispositions, schemaUpdateOptions, a job's schema, a
//     referenceFileSchemaUri and hivePartitioningOptions are read as for
//     Avro (mergeParquet, givenFits, referencefile.go, loadvariants.go).
//
// UNVERIFIED (docs re-read 2026-11-17:
// https://docs.cloud.google.com/bigquery/docs/loading-data-cloud-storage-orc):
// an ORC TIMESTAMP's seconds count from 2015-01-01 00:00:00 in
// the writer's time zone (the stripe's writerTimezone); the front reads
// the wall-clock time there, as the Apache ORC tools print it, as UTC
// (the page does not say how BigQuery reads the zone). A TIMESTAMP_INSTANT
// is an instant, read as UTC. A writer's zone this machine's time zone
// database does not have is 501.
//
// Not implemented (501), each for its reason: a decimal decimalTargetTypes
// makes STRING (how BigQuery writes it is not documented); a dry run; a
// partition decorator; another project's table; files whose columns
// differ from each other's or the reference file's.

// orcNode is a column's type as the front reads it from an ORC file.
type orcNode struct {
	kind     orcproto.Type_Kind
	bq       string
	repeated bool // a list or a map
	// elem is a list's element; fields a struct's, a map's entry (key,
	// value) or a union's (field_0, ...); single a single-variant union's
	// one type.
	elem   *orcNode
	fields []orcField
	single *orcNode
	scalar pqScalar // a decimal's target type
}

type orcField struct {
	name string
	node *orcNode
}

// orcColumns reads the file's schema as BigQuery's columns.
func orcColumns(types []*orcproto.Type, opts pqOptions) ([]orcField, error) {
	root, err := orcNodeOf(types, 0, "", opts)
	if err != nil {
		return nil, err
	}
	return root.fields, nil
}

func orcNodeOf(types []*orcproto.Type, id uint32, path string, opts pqOptions) (*orcNode, error) {
	t := types[id]
	n := &orcNode{kind: t.GetKind()}
	at := path
	if at == "" {
		at = "the file's root"
	}
	sub := t.GetSubtypes()
	switch k := t.GetKind(); k {
	case orcproto.Type_BOOLEAN:
		n.bq = "BOOLEAN"
	case orcproto.Type_BYTE, orcproto.Type_SHORT, orcproto.Type_INT, orcproto.Type_LONG:
		n.bq = "INTEGER"
	case orcproto.Type_FLOAT, orcproto.Type_DOUBLE:
		n.bq = "FLOAT"
	case orcproto.Type_STRING, orcproto.Type_VARCHAR, orcproto.Type_CHAR:
		n.bq = "STRING"
	case orcproto.Type_BINARY:
		n.bq = "BYTES"
	case orcproto.Type_DATE:
		n.bq = "DATE"
	case orcproto.Type_TIMESTAMP, orcTimestampInstant:
		n.bq = "TIMESTAMP"
	case orcproto.Type_DECIMAL:
		s, why := opts.decimal(pqElement{Precision: int(t.GetPrecision()), Scale: int(t.GetScale())})
		if why != "" {
			if s.bq == "" {
				return nil, avInvalid("Error while reading data: the column %s: %s.", at, why)
			}
			return nil, avNotHere("of a file whose column %s is %s", at, why)
		}
		n.scalar, n.bq = s, s.bq
	case orcproto.Type_STRUCT:
		n.bq = "RECORD"
		names := t.GetFieldNames()
		if len(names) != len(sub) {
			return nil, avInvalid("Error while reading data: the ORC file's struct %s names %d fields of %d.", at,
				len(names), len(sub))
		}
		for i, c := range sub {
			child, err := orcNodeOf(types, c, strings.TrimPrefix(path+"."+names[i], "."), opts)
			if err != nil {
				return nil, err
			}
			n.fields = append(n.fields, orcField{name: names[i], node: child})
		}
		if len(n.fields) == 0 {
			return nil, avInvalid("Error while reading data: the ORC file's struct %s has no fields, and a RECORD has at "+
				"least one.", at)
		}
	case orcproto.Type_LIST:
		if len(sub) != 1 {
			return nil, avInvalid("Error while reading data: the ORC file's list %s has %d element types.", at, len(sub))
		}
		elem, err := orcNodeOf(types, sub[0], path, opts)
		if err != nil {
			return nil, err
		}
		switch {
		case elem.kind == orcproto.Type_MAP || (elem.single != nil && elem.single.kind == orcproto.Type_MAP):
			return nil, avInvalid("Error while reading data: the column %s is a list of maps: \"Nested lists and lists of "+
				"maps are not supported\".", at)
		case elem.repeated:
			return nil, avInvalid("Error while reading data: the column %s is a list of lists: \"Nested lists and lists of "+
				"maps are not supported\".", at)
		}
		n.repeated, n.elem, n.bq = true, elem, elem.bq
	case orcproto.Type_MAP:
		if len(sub) != 2 {
			return nil, avInvalid("Error while reading data: the ORC file's map %s has %d subtypes.", at, len(sub))
		}
		key, err := orcNodeOf(types, sub[0], path+".key", opts)
		if err != nil {
			return nil, err
		}
		val, err := orcNodeOf(types, sub[1], path+".value", opts)
		if err != nil {
			return nil, err
		}
		n.repeated, n.bq = true, "RECORD"
		n.fields = []orcField{{name: "key", node: key}, {name: "value", node: val}}
	case orcproto.Type_UNION:
		if len(sub) == 0 {
			return nil, avInvalid("Error while reading data: the ORC file's union %s has no types.", at)
		}
		if len(sub) == 1 {
			one, err := orcNodeOf(types, sub[0], path, opts)
			if err != nil {
				return nil, err
			}
			c := *one
			c.kind, c.single = orcproto.Type_UNION, one
			return &c, nil
		}
		n.bq = "RECORD"
		for i, c := range sub {
			name := "field_" + strconv.Itoa(i)
			child, err := orcNodeOf(types, c, path+"."+name, opts)
			if err != nil {
				return nil, err
			}
			n.fields = append(n.fields, orcField{name: name, node: child})
		}
	default:
		return nil, avInvalid("Error while reading data: the ORC file's column %s is of a type kind (%d) ORC does not "+
			"define.", at, k)
	}
	return n, nil
}

// orcTableFields are the columns' BigQuery fields.
func orcTableFields(fields []orcField) []field {
	out := make([]field, len(fields))
	for i, fl := range fields {
		n := fl.node
		f := field{Name: fl.name, Type: n.bq, Mode: "NULLABLE"}
		if n.repeated {
			f.Mode = "REPEATED"
		}
		inner := n
		if n.elem != nil {
			inner = n.elem
		}
		if inner.bq == "RECORD" {
			f.Fields = orcTableFields(inner.fields)
		}
		out[i] = f
	}
	return out
}

// value is v, the reader's value of a column of type n, as literalFor
// takes it: nil, a literal, a []any of a REPEATED field's values, or a
// RECORD's map of its fields' lower-case names to values.
func (n *orcNode) value(v any, col string) (any, error) {
	if v == nil {
		if n.repeated {
			return []any{}, nil // a REPEATED column is never NULL
		}
		return nil, nil
	}
	if n.single != nil {
		u, ok := v.(orcUnion)
		if !ok {
			return nil, fmt.Errorf("column %s's union value is not a union's", col)
		}
		return n.single.value(u.value, col)
	}
	switch n.kind {
	case orcproto.Type_LIST:
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("column %s's value is not a list", col)
		}
		out := make([]any, 0, len(items))
		for _, it := range items {
			if it == nil {
				continue // "NULL list elements are ignored during load"
			}
			x, err := n.elem.value(it, col)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	case orcproto.Type_MAP:
		entries, ok := v.([]orcMapEntry)
		if !ok {
			return nil, fmt.Errorf("column %s's value is not a map", col)
		}
		out := make([]any, len(entries))
		for i, e := range entries {
			k, err := n.fields[0].node.value(e.key, col+".key")
			if err != nil {
				return nil, err
			}
			x, err := n.fields[1].node.value(e.value, col+".value")
			if err != nil {
				return nil, err
			}
			out[i] = map[string]any{"key": k, "value": x}
		}
		return out, nil
	case orcproto.Type_STRUCT:
		vals, ok := v.([]any)
		if !ok || len(vals) != len(n.fields) {
			return nil, fmt.Errorf("column %s's value is not a struct of its fields", col)
		}
		return orcRow(n.fields, vals, col+".")
	case orcproto.Type_UNION:
		u, ok := v.(orcUnion)
		if !ok || u.tag >= len(n.fields) {
			return nil, fmt.Errorf("column %s's value is not one of its union's", col)
		}
		out := make(map[string]any, len(n.fields))
		x, err := n.fields[u.tag].node.value(u.value, col+"."+n.fields[u.tag].name)
		if err != nil {
			return nil, err
		}
		out[n.fields[u.tag].name] = x
		return out, nil
	}
	return n.literal(v, col)
}

// orcRow is a struct's values as literalFor takes them.
func orcRow(fields []orcField, vals []any, path string) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for i, fl := range fields {
		v, err := fl.node.value(vals[i], path+fl.name)
		if err != nil {
			return nil, err
		}
		out[strings.ToLower(fl.name)] = v
	}
	return out, nil
}

// literal writes a scalar as a GoogleSQL literal of its column's type.
func (n *orcNode) literal(v any, col string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("column %s's value %v (%T) is not of its type %s", col, v, v, n.kind)
	}
	switch n.bq {
	case "BOOLEAN":
		b, ok := v.(bool)
		if !ok {
			return bad()
		}
		return strings.ToUpper(strconv.FormatBool(b)), nil
	case "INTEGER":
		switch x := v.(type) {
		case int8: // an ORC byte is signed
			return strconv.Itoa(int(x)), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		}
		return bad()
	case "FLOAT":
		switch x := v.(type) {
		case orc.Float:
			return floatLiteral(col, float64(x))
		case orc.Double:
			return floatLiteral(col, float64(x))
		}
		return bad()
	case "STRING":
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		if !utf8.ValidString(s) {
			return "", valueInvalid("Error while reading data: column %s's STRING value is not UTF-8 (\"only UTF-8 "+
				"encoding is supported\").", col)
		}
		return sqlString(s), nil
	case "BYTES":
		b, ok := v.([]byte)
		if !ok {
			return bad()
		}
		return sqlBytes(b), nil
	case "DATE":
		days, ok := v.(int64)
		if !ok {
			return bad()
		}
		if days < -719162 || days > 2932896 { // 0001-01-01, 9999-12-31
			return "", valueInvalid("Error while reading data: column %s: invalid date value: %d days from 1970-01-01 "+
				"is outside BigQuery's range of 0001-01-01 to 9999-12-31.", col, days)
		}
		return "DATE '" + time.Unix(days*86400, 0).UTC().Format("2006-01-02") + "'", nil
	case "TIMESTAMP":
		t, ok := v.(time.Time)
		if !ok {
			return bad()
		}
		t = t.Add(-time.Duration(t.Nanosecond() % 1000)) // "converts sub-microsecond values to microseconds"
		if t.Before(minTimestamp) || t.After(maxTimestamp) {
			return "", valueInvalid("Error while reading data: column %s: invalid date value: the TIMESTAMP is outside "+
				"BigQuery's range of 0001-01-01 00:00:00 to 9999-12-31 23:59:59.999999 UTC.", col)
		}
		return timestampLiteral(col, t)
	case "NUMERIC", "BIGNUMERIC":
		d, ok := v.(orcDecimal)
		if !ok {
			return bad()
		}
		want := int64(n.scalar.scale)
		u := new(big.Int).Set(d.unscaled)
		switch {
		case d.scale < want:
			u.Mul(u, new(big.Int).Exp(big.NewInt(10), big.NewInt(want-d.scale), nil))
		case d.scale > want:
			div := new(big.Int).Exp(big.NewInt(10), big.NewInt(d.scale-want), nil)
			q, m := new(big.Int).QuoRem(u, div, new(big.Int))
			if m.Sign() != 0 {
				return "", valueInvalid("Error while reading data: column %s's DECIMAL value has a scale of %d, more than "+
					"its type's %d.", col, d.scale, want)
			}
			u = q
		}
		return decimalLiteral(col, n.bq, u, int(want))
	}
	return bad()
}

// orcInput is one file of an ORC load.
type orcInput struct {
	uri    string
	obj    gsObject
	fields []orcField
}

// orcLoadJob is the part of an ORC load's job the front reads beyond jobBody.
type orcLoadJob struct {
	Configuration struct {
		DryRun bool `json:"dryRun"`
		Load   struct {
			ReferenceFileSchemaURI string   `json:"referenceFileSchemaUri"`
			CreateDisposition      string   `json:"createDisposition"`
			DecimalTargetTypes     []string `json:"decimalTargetTypes"`
		} `json:"load"`
	} `json:"configuration"`
}

// orcOpenFile opens one file of the load and reads its tail; cleanup
// removes what it made. A file that is not ORC is an avSchemaError, 400.
func (f front) orcOpenFile(r *http.Request, upload *spooled, uri string, obj gsObject) (*orcFile, int64, func(), error) {
	data, size, cleanup, err := f.parquetData(r, upload, pqFile{uri: uri, obj: obj})
	if err != nil {
		return nil, 0, nil, err
	}
	o, err := openORC(data, size)
	if err != nil {
		cleanup()
		return nil, 0, nil, orcFileError(err, uri)
	}
	return o, size, cleanup, nil
}

// orcFileError is err, reading the file at uri, as the load answers it.
func orcFileError(err error, uri string) error {
	where := ""
	if uri != "" {
		where = " " + uri
	}
	var ce *orcCodecError
	if errors.Is(err, errNotORC) || errors.As(err, &ce) {
		return avInvalid("Error while reading data%s: the data is not an ORC file BigQuery can read: %v.", where, err)
	}
	return err
}

// orcLoad carries out an ORC load (above) and answers w, reporting false;
// any other load it leaves, reporting true.
func (f front) orcLoad(w http.ResponseWriter, r *http.Request, job *jobBody) bool {
	l := job.Configuration.Load
	if l == nil || !strings.EqualFold(l.SourceFormat, "ORC") || l.DestinationTable == nil {
		return true
	}
	dest := l.DestinationTable
	name := dest.DatasetID + "." + dest.TableID
	var upload *spooled
	defer func() {
		if upload != nil {
			upload.Close()
		}
	}()
	answer := func(status int, msg string) bool {
		if status == http.StatusNotImplemented {
			writeError(w, status, "notImplemented", "Not implemented here: an ORC load "+msg+". Nothing was loaded.")
		} else {
			writeError(w, status, "invalid", msg+" Nothing was loaded.")
		}
		return false
	}
	fromErr := func(err error, where string) bool {
		var se *avSchemaError
		if errors.As(err, &se) {
			return answer(se.status, se.msg)
		}
		var le *loadDataError
		if errors.As(err, &le) {
			writeError(w, le.code, le.reason, le.msg)
			return false
		}
		return answer(http.StatusNotImplemented, "whose file"+where+" CloudBurrow could not read ("+err.Error()+")")
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); len(l.SourceURIs) == 0 && !strings.HasPrefix(mt, "multipart/") {
		return answer(http.StatusBadRequest, "An ORC load reads its data from sourceUris or from an upload, and this "+
			"job has neither.")
	}
	var extra orcLoadJob
	_ = decodeJob(r, &extra)
	el := extra.Configuration.Load
	switch {
	case strings.Contains(dest.TableID, "$"):
		return answer(http.StatusNotImplemented, "into a partition ("+name+"), which CloudBurrow does not carry out "+
			"for ORC")
	case extra.Configuration.DryRun:
		return answer(http.StatusNotImplemented, "in a dry run")
	}
	opts := pqOptions{decimalTypes: el.DecimalTargetTypes}

	// The files and their columns.
	var files []orcInput
	if len(l.SourceURIs) == 0 {
		s, err := spoolUpload(r)
		if err != nil {
			return answer(http.StatusNotImplemented, "whose data CloudBurrow could not read ("+err.Error()+")")
		}
		upload = s
		files = []orcInput{{}}
	} else {
		if f.storage == nil {
			return answer(http.StatusNotImplemented, "from Cloud Storage with no Cloud Storage for CloudBurrow to read "+
				"the files from")
		}
		objs, err := f.storage.resolve(r.Context(), l.SourceURIs)
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return false
		}
		for _, o := range objs {
			files = append(files, orcInput{uri: o.uri(), obj: o})
		}
	}
	columnsOf := func(uri string, obj gsObject) ([]orcField, error) {
		o, _, cleanup, err := f.orcOpenFile(r, upload, uri, obj)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		return orcColumns(o.types, opts)
	}
	var file []field
	for i := range files {
		where := ""
		if files[i].uri != "" {
			where = " " + files[i].uri
		}
		cols, err := columnsOf(files[i].uri, files[i].obj)
		if err != nil {
			return fromErr(err, where)
		}
		files[i].fields = cols
		got := orcTableFields(cols)
		if msg := checkSchema(got, ""); msg != "" {
			return answer(http.StatusBadRequest, "Error while reading data"+where+": the ORC file's columns are not "+
				"BigQuery's: "+msg+".")
		}
		if i == 0 {
			file = got
		} else if !sameFields(file, got) {
			return answer(http.StatusNotImplemented, "of files whose columns differ ("+files[0].uri+": "+describeFields(file)+
				"; "+files[i].uri+": "+describeFields(got)+"); BigQuery then takes the schema of the alphabetically last "+
				"file, which CloudBurrow does not reproduce")
		}
	}
	if ref := el.ReferenceFileSchemaURI; ref != "" {
		obj, status, msg := f.referenceObject(r, ref) // referencefile.go
		if status == http.StatusNotFound {
			writeError(w, status, "notFound", msg)
			return false
		} else if status != 0 {
			return answer(status, msg)
		}
		cols, err := columnsOf(obj.uri(), obj)
		if err != nil {
			return fromErr(err, " "+obj.uri())
		}
		if refFields := orcTableFields(cols); !sameFields(refFields, file) {
			return answer(http.StatusNotImplemented, "of files whose columns differ from the reference file "+ref+"'s ("+
				describeFields(refFields)+"; the files': "+describeFields(file)+")"+referenceDiffersWhy)
		}
	}
	if m := f.refuseGeneratedLoad(*dest, file); m != "" { // #1778, autoembed.go
		return answer(http.StatusBadRequest, m+".")
	}
	if l.Schema != nil && !givenFits(l.Schema.Fields, file) {
		return answer(http.StatusNotImplemented, "whose job gives a schema that is not the file's ("+describeFields(l.Schema.Fields)+
			"; the file's is "+describeFields(file)+"). BigQuery reads an ORC file by the file's schema; leave the "+
			"schema out")
	}
	if h := hiveFilesOf(r); h != nil { // #1547, loadvariants.go
		for _, k := range h.keys {
			for _, fl := range file {
				if strings.EqualFold(fl.Name, k.name) {
					return answer(http.StatusBadRequest, "The ORC file's column "+fl.Name+" is a partition key: \"Hive "+
						"partitioning keys and the columns in the underlying files cannot overlap.\"")
				}
			}
		}
		file = append(file, h.fields()...)
	}

	// The plan, as for Parquet and Avro.
	plan := &pqPlan{dest: tableRef{DatasetID: dest.DatasetID, TableID: dest.TableID},
		createNever: strings.EqualFold(el.CreateDisposition, "CREATE_NEVER")}
	write := strings.ToUpper(l.WriteDisposition)
	df := f.dataFront(r, dest) // crossload.go, #1767
	status, got := df.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
	orcWords := func(s string) string { return strings.ReplaceAll(s, "Parquet", "ORC") }
	switch {
	case status == http.StatusNotFound:
		plan.kind = pqLoadNew
		plan.props, _ = loadTableProps(r) // #1457, loadtable.go
		plan.target = file
		if l.Schema != nil {
			plan.target = l.Schema.Fields
		}
		plan.raw = rawFields(plan.target, nil)
	case status != http.StatusOK:
		return answer(http.StatusNotImplemented, fmt.Sprintf("into %s, whose schema CloudBurrow could not read (the "+
			"emulator answered HTTP %d)", name, status))
	default:
		var meta struct {
			Type   string `json:"type"`
			Schema *struct {
				Fields json.RawMessage `json:"fields"`
			} `json:"schema"`
		}
		table, ok := decodeMap(got)
		if json.Unmarshal(got, &meta) != nil || !ok {
			return answer(http.StatusNotImplemented, "into "+name+", whose schema CloudBurrow could not read")
		}
		if meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
			return answer(http.StatusNotImplemented, "into "+name+", which is a "+meta.Type)
		}
		var old []field
		var tableRaw []any
		if meta.Schema != nil && len(meta.Schema.Fields) > 0 && string(meta.Schema.Fields) != "null" {
			if json.Unmarshal(meta.Schema.Fields, &old) != nil || json.Unmarshal(meta.Schema.Fields, &tableRaw) != nil {
				return answer(http.StatusNotImplemented, "into "+name+", whose schema CloudBurrow could not read")
			}
		}
		plan.table, plan.old = table, old
		var why string
		code := 0
		switch write {
		case "WRITE_TRUNCATE":
			plan.kind, plan.target = pqLoadTruncate, old
			if !sameFields(old, file) {
				plan.kind, plan.target = pqLoadReplace, file
			}
		case "WRITE_TRUNCATE_DATA":
			plan.kind = pqLoadTruncateData
			var m pqMerge
			plan.target, code, why = mergeParquet(old, file, nil, dest, projectOf(df.base), &m)
		case "", "WRITE_APPEND", "WRITE_EMPTY":
			plan.kind, plan.writeEmpty = pqLoadAppend, write == "WRITE_EMPTY"
			var m pqMerge
			plan.target, code, why = mergeParquet(old, file, l.SchemaUpdateOptions, dest, projectOf(df.base), &m)
			plan.relaxed = m.relaxed
			if m.added {
				plan.kind = pqLoadAdd
			}
		default:
			return answer(http.StatusBadRequest, "Invalid value for writeDisposition: "+l.WriteDisposition+".")
		}
		if code != 0 {
			return answer(code, orcWords(why))
		}
		if plan.kind == pqLoadReplace {
			plan.raw = rawFields(plan.target, nil)
		} else {
			plan.raw = rawFields(plan.target, tableRaw)
		}
	}

	j, ok := f.startParquetJob(w, r, "ORC")
	if !ok {
		return false
	}
	fail := func(e rowError) bool {
		f.finishOwnJob(w, r, j, "LOAD", "load", nil, &e)
		return false
	}
	rows, err := os.CreateTemp("", "bigquery-orc-rows-*")
	if err != nil {
		return answer(http.StatusNotImplemented, "whose rows CloudBurrow could not keep ("+err.Error()+")")
	}
	defer func() { rows.Close(); os.Remove(rows.Name()) }()
	bw := bufio.NewWriter(rows)
	hive := hiveFilesOf(r) // #1547
	var n, size int64
	for _, in := range files {
		where := ""
		if in.uri != "" {
			where = " " + in.uri
		}
		o, read, cleanup, err := f.orcOpenFile(r, upload, in.uri, in.obj)
		if err != nil {
			return fromErr(err, where)
		}
		got, err := o.rows(func(vals []any) error {
			m, err := orcRow(in.fields, vals, "")
			if err != nil {
				return err
			}
			parts := make([]string, len(plan.target))
			for i, t := range plan.target {
				if hive != nil {
					if lit, ok := hive.literal(in.uri, t.Name); ok {
						parts[i] = lit
						continue
					}
				}
				if parts[i], err = literalFor(t, m[strings.ToLower(t.Name)]); err != nil {
					return err
				}
			}
			_, err = bw.WriteString("(" + strings.Join(parts, ", ") + ")\n")
			return err
		})
		cleanup()
		n += got
		size += read
		if err != nil {
			var ve *pqValueError
			switch {
			case errors.As(err, &ve) && ve.status == http.StatusBadRequest:
				return fail(rowError{Reason: "invalid", Location: in.uri, Message: ve.msg})
			case errors.As(err, &ve):
				return answer(http.StatusNotImplemented, "of a file"+where+" whose "+ve.msg)
			}
			return fail(rowError{Reason: "invalid", Location: in.uri, Message: "Error while reading data" + where + ": " +
				err.Error() + "."})
		}
	}
	if err := bw.Flush(); err != nil {
		return answer(http.StatusNotImplemented, "whose rows CloudBurrow could not keep ("+err.Error()+")")
	}

	project := projectOf(df.base)
	full := tableName(project, plan.dest)
	if plan.kind == pqLoadNew {
		if plan.createNever {
			return fail(rowError{Reason: "notFound", Message: "Not found: Table " + full})
		}
		if status, _ := df.get(r, "/datasets/"+url.PathEscape(plan.dest.DatasetID)); status == http.StatusNotFound {
			return fail(rowError{Reason: "notFound", Message: "Not found: Dataset " + project + ":" + plan.dest.DatasetID})
		}
	} else if plan.writeEmpty && !df.emptyTable(r, plan.dest.DatasetID, plan.dest.TableID, nil) {
		return fail(rowError{Reason: "duplicate", Message: "Already Exists: Table " + full})
	}
	if msg := df.carryOutParquet(r, plan, rows); msg != "" {
		return fail(rowError{Reason: "backendError", Message: "CloudBurrow could not load the ORC file's rows into " +
			full + ": " + msg})
	}
	f.finishOwnJob(w, r, j, "LOAD", "load", map[string]any{
		"inputFiles":     strconv.Itoa(len(files)),
		"inputFileBytes": strconv.FormatInt(size, 10),
		"outputRows":     strconv.FormatInt(n, 10),
		"badRecords":     "0",
	}, nil)
	return false
}
