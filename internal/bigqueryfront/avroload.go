package bigqueryfront

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/linkedin/goavro/v2"
)

// Avro loads (#1457).
//
// The pinned emulator has no Avro load: it answers one "not support
// sourceFormat: AVRO" (handler.go). So an Avro load is the front's own
// job, carried out as a Parquet load it carries out itself is
// (parquetload.go: the rows go first to a scratch table, then into the
// table, so a load that fails leaves it as it was), with the file's schema
// read by BigQuery's "Avro conversions"
// (https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-avro#avro_conversions):
//
//   - null: "ignored", a field of type null is no column; boolean
//     BOOLEAN; int and long INTEGER; float and double FLOAT; bytes BYTES;
//     string STRING; enum STRING ("the symbol value"); fixed BYTES.
//   - record RECORD, its doc the column's description ("doc text becomes
//     a field description"); array REPEATED ("Arrays of arrays are
//     unsupported": 400); a union of null and one type NULLABLE; a field
//     of one type REQUIRED.
//   - Logical types: by default "BigQuery ignores most logicalType
//     attributes and uses the underlying Avro type" (a date INTEGER, its
//     days since the epoch; a time or timestamp INTEGER of its millis or
//     micros); with useAvroLogicalTypes date DATE, time-millis and
//     time-micros TIME, timestamp-millis and timestamp-micros TIMESTAMP,
//     local-timestamp-millis and -micros DATETIME. decimal NUMERIC,
//     BIGNUMERIC or STRING either way, by decimalTargetTypes as for
//     Parquet (pqOptions.decimal); duration BYTES.
//   - Several files: "all schemas must be compatible"; files whose
//     columns differ are 501, as BigQuery then takes "the alphabetically
//     last file", which the front does not reproduce.
//   - Appending, WRITE_TRUNCATE and WRITE_TRUNCATE_DATA, schemaUpdateOptions
//     and a job's schema are checked as for Parquet (mergeParquet,
//     givenFits).
//
// Not implemented (501), each for its reason: a union of several types
// other than null (BigQuery makes "a RECORD of nullable fields", but does
// not document their names); a map (its entries' order is not documented,
// and the decoder does not keep the file's); a recursive record ("dropped
// beyond the first nesting level", not measured); an array whose items
// may be null; a file compressed with zstandard (the Avro decoder in
// go.mod, goavro, reads null, deflate and snappy); referenceFileSchemaUri;
// a decimal decimalTargetTypes makes STRING; a dry run; a partition
// decorator; another project's table.

// avNode is a column's type as the front reads it from an Avro schema.
type avNode struct {
	// kind is the Avro type: a primitive's name, "record", "enum", "fixed"
	// or "array".
	kind    string
	logical string
	// nullable is a union of null and this type, whose values the decoder
	// gives as a map of the branch's name to the value.
	nullable bool
	items    *avNode   // an array's
	fields   []avField // a record's
	scalar   pqScalar  // a decimal's target type
	bq       string    // the column's BigQuery type
}

// avField is a record's field that is a column (a null one is not).
type avField struct {
	name, doc string
	node      *avNode
}

// avBuilder reads an Avro schema's types.
type avBuilder struct {
	logical bool
	opts    pqOptions
	named   map[string]*avNode
	// building are the records being read, for a recursive one.
	building map[string]bool
}

// avNotHere and avInvalid are why a schema cannot be loaded: 501, 400.
type avSchemaError struct {
	status int
	msg    string
}

func (e *avSchemaError) Error() string { return e.msg }

func avNotHere(format string, a ...any) error {
	return &avSchemaError{status: http.StatusNotImplemented, msg: fmt.Sprintf(format, a...)}
}

func avInvalid(format string, a ...any) error {
	return &avSchemaError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

// fullName is a named type's full name in namespace ns.
func fullName(name, ns string) string {
	if strings.Contains(name, ".") || ns == "" {
		return name
	}
	return ns + "." + name
}

// node reads the type s, in namespace ns, of the column at path. It
// returns nil for a type that is no column (null, a union of nulls).
func (b *avBuilder) node(s any, ns, path string) (*avNode, error) {
	switch t := s.(type) {
	case string:
		switch t {
		case "null":
			return nil, nil
		case "boolean", "int", "long", "float", "double", "bytes", "string":
			return b.primitive(t, "", nil, path)
		}
		if b.building[fullName(t, ns)] || b.building[t] {
			return nil, avNotHere("of a file whose column %s is a recursive record, which BigQuery drops \"beyond the first nesting "+
				"level\" by a rule it does not document further", path)
		}
		n, ok := b.named[fullName(t, ns)]
		if !ok {
			n, ok = b.named[t]
		}
		if !ok {
			return nil, avInvalid("Error while reading data: the type %s of %s is not defined.", t, path)
		}
		c := *n
		return &c, nil
	case []any:
		var kept []any
		for _, u := range t {
			if u != "null" {
				kept = append(kept, u)
			}
		}
		switch len(kept) {
		case 0:
			return nil, nil
		case 1:
			if len(kept) == len(t) {
				return b.node(kept[0], ns, path)
			}
			n, err := b.node(kept[0], ns, path)
			if n == nil {
				return nil, err
			}
			c := *n // not the named type's own
			c.nullable = true
			return &c, nil
		}
		return nil, avNotHere("of a file whose column %s is a union of several types, which BigQuery makes a RECORD of nullable "+
			"fields whose names it does not document", path)
	case map[string]any:
		typ, _ := t["type"].(string)
		logical, _ := t["logicalType"].(string)
		name, _ := t["name"].(string)
		if n, _ := t["namespace"].(string); n != "" {
			ns = n
		}
		switch typ {
		case "record", "error":
			full := fullName(name, ns)
			if strings.Contains(name, ".") {
				ns = name[:strings.LastIndex(name, ".")]
			}
			b.building[full] = true
			defer delete(b.building, full)
			n := &avNode{kind: "record", bq: "RECORD"}
			list, _ := t["fields"].([]any)
			for _, fl := range list {
				m, _ := fl.(map[string]any)
				fname, _ := m["name"].(string)
				child, err := b.node(m["type"], ns, strings.TrimPrefix(path+"."+fname, "."))
				if err != nil {
					return nil, err
				}
				if child == nil {
					continue
				}
				doc, _ := m["doc"].(string)
				n.fields = append(n.fields, avField{name: fname, doc: doc, node: child})
			}
			if len(n.fields) == 0 {
				return nil, avNotHere("of a file whose column %s is a record with no field but nulls, which BigQuery's documentation "+
					"does not say how it loads", path)
			}
			b.named[full] = n
			return n, nil
		case "enum":
			n := &avNode{kind: "enum", bq: "STRING"}
			b.named[fullName(name, ns)] = n
			return n, nil
		case "fixed":
			n, err := b.primitive("fixed", logical, t, path)
			if err == nil {
				b.named[fullName(name, ns)] = n
			}
			return n, err
		case "array":
			items, err := b.node(t["items"], ns, path)
			if err != nil {
				return nil, err
			}
			if items == nil {
				return nil, avNotHere("of a file whose column %s is an array of nulls, which BigQuery ignores by a rule it does not "+
					"document further", path)
			}
			if items.kind == "array" {
				return nil, avInvalid("Error while reading data: the column %s is an array of arrays, and \"Arrays of "+
					"arrays are unsupported\".", path)
			}
			if items.nullable {
				return nil, avNotHere("of a file whose column %s is an array whose items may be null, which BigQuery's documentation "+
					"does not say how it loads", path)
			}
			return &avNode{kind: "array", items: items, bq: items.bq}, nil
		case "map":
			return nil, avNotHere("of a file whose column %s is a map, which BigQuery loads as a REPEATED RECORD of its entries, "+
				"in an order it does not document", path)
		case "":
			return b.node(t["type"], ns, path)
		default:
			return b.primitive(typ, logical, t, path)
		}
	}
	return nil, avInvalid("the type of %s is not an Avro type", path)
}

// primitive reads a primitive or fixed type with its logical type.
func (b *avBuilder) primitive(typ, logical string, t map[string]any, path string) (*avNode, error) {
	n := &avNode{kind: typ, logical: logical}
	base := map[string]string{"boolean": "BOOLEAN", "int": "INTEGER", "long": "INTEGER", "float": "FLOAT",
		"double": "FLOAT", "bytes": "BYTES", "string": "STRING", "fixed": "BYTES"}[typ]
	if base == "" {
		return nil, avInvalid("the type %s of %s is not an Avro type", typ, path)
	}
	n.bq = base
	logicalOf := map[string]string{"int.date": "DATE", "int.time-millis": "TIME", "long.time-micros": "TIME",
		"long.timestamp-millis": "TIMESTAMP", "long.timestamp-micros": "TIMESTAMP",
		"long.local-timestamp-millis": "DATETIME", "long.local-timestamp-micros": "DATETIME"}
	switch {
	case logical == "decimal" && (typ == "bytes" || typ == "fixed"):
		precision, scale := avInt(t["precision"]), avInt(t["scale"])
		s, why := b.opts.decimal(pqElement{Precision: precision, Scale: scale})
		if why != "" {
			if s.bq == "" {
				return nil, avInvalid("Error while reading data: the column %s: %s.", path, why)
			}
			return nil, avNotHere("of a file whose column %s is %s", path, why)
		}
		n.scalar, n.bq = s, s.bq
	case logical == "duration" && typ == "fixed":
		n.bq = "BYTES"
	case logicalOf[typ+"."+logical] != "":
		if b.logical {
			n.bq = logicalOf[typ+"."+logical]
		}
	default:
		n.logical = "" // "ignores most logicalType attributes"
	}
	return n, nil
}

func avInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	}
	return 0
}

// avColumns reads an Avro file's schema as BigQuery's columns.
func avColumns(schema string, logical bool, opts pqOptions) ([]avField, error) {
	var s any
	d := json.NewDecoder(strings.NewReader(schema))
	if err := d.Decode(&s); err != nil {
		return nil, avInvalid("Error while reading data: the Avro file's schema is not JSON: %v.", err)
	}
	b := &avBuilder{logical: logical, opts: opts, named: map[string]*avNode{}, building: map[string]bool{}}
	root, err := b.node(s, "", "")
	if err != nil {
		return nil, err
	}
	if root == nil || root.kind != "record" || root.nullable {
		return nil, avInvalid("Error while reading data: the Avro file's schema is not a record, which BigQuery " +
			"loads as a row.")
	}
	return root.fields, nil
}

// avTableFields are the columns' BigQuery fields.
func avTableFields(fields []avField) []field {
	out := make([]field, len(fields))
	for i, fl := range fields {
		n := fl.node
		f := field{Name: fl.name, Type: n.bq, Mode: "REQUIRED"}
		switch {
		case n.kind == "array":
			f.Mode = "REPEATED"
			if n.items.kind == "record" {
				f.Fields = avTableFields(n.items.fields)
			}
		case n.nullable:
			f.Mode = "NULLABLE"
		}
		if n.kind == "record" {
			f.Fields = avTableFields(n.fields)
		}
		out[i] = f
	}
	return out
}

// avDescribe sets the description of each column of raw (rawFields) whose
// field has a doc.
func avDescribe(raw []any, fields []avField) {
	byName := map[string]avField{}
	for _, fl := range fields {
		byName[strings.ToLower(fl.name)] = fl
	}
	for _, r := range raw {
		m, _ := r.(map[string]any)
		name, _ := m["name"].(string)
		fl, ok := byName[strings.ToLower(name)]
		if !ok {
			continue
		}
		if fl.doc != "" {
			m["description"] = fl.doc
		}
		kids, _ := m["fields"].([]any)
		n := fl.node
		if n.kind == "array" {
			n = n.items
		}
		if len(kids) > 0 && n.kind == "record" {
			avDescribe(kids, n.fields)
		}
	}
}

// avRow is a record's values as literalFor takes them: a map of each
// column's lower-case name to its value.
func avRow(fields []avField, rec map[string]any, path string) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for _, fl := range fields {
		v, err := fl.node.value(rec[fl.name], path+fl.name)
		if err != nil {
			return nil, err
		}
		out[strings.ToLower(fl.name)] = v
	}
	return out, nil
}

// value is v, the decoder's value of a column of type n, as literalFor
// takes it.
func (n *avNode) value(v any, col string) (any, error) {
	if n.nullable {
		if v == nil {
			return nil, nil
		}
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return nil, fmt.Errorf("column %s's union value is not one branch", col)
		}
		for _, x := range m {
			v = x
		}
		c := *n
		c.nullable = false
		return c.value(v, col)
	}
	switch n.kind {
	case "array":
		items, _ := v.([]any)
		out := make([]any, len(items))
		for i, it := range items {
			x, err := n.items.value(it, col)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case "record":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("column %s's value is not a record", col)
		}
		return avRow(n.fields, m, col+".")
	}
	return n.literal(v, col)
}

// literal writes a scalar as a GoogleSQL literal of its column's type.
func (n *avNode) literal(v any, col string) (string, error) {
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
	case "FLOAT":
		switch x := v.(type) {
		case float32:
			return floatLiteral(col, float64(x))
		case float64:
			return floatLiteral(col, x)
		}
		return bad()
	case "STRING":
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		return sqlString(s), nil
	case "BYTES":
		b, ok := v.([]byte)
		if !ok {
			return bad()
		}
		return sqlBytes(b), nil
	case "NUMERIC", "BIGNUMERIC":
		var unscaled *big.Int
		switch x := v.(type) {
		case *big.Rat:
			r := new(big.Rat).Mul(x, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.scalar.scale)), nil)))
			if !r.IsInt() {
				return bad()
			}
			unscaled = r.Num()
		case []byte:
			unscaled = twosComplement(x)
		default:
			return bad()
		}
		return decimalLiteral(col, n.bq, unscaled, n.scalar.scale)
	}
	// INTEGER, DATE, TIME, TIMESTAMP, DATETIME: the decoder gives a
	// logical type it knows as a time.Time or time.Duration.
	var i int64
	switch x := v.(type) {
	case int32:
		i = int64(x)
	case int64:
		i = x
	case time.Time:
		switch n.logical {
		case "date":
			i = x.Unix() / 86400
			if x.Unix() < 0 && x.Unix()%86400 != 0 {
				i--
			}
		case "timestamp-millis":
			i = x.UnixMilli()
		default:
			i = x.UnixMicro()
		}
	case time.Duration:
		if n.logical == "time-millis" {
			i = x.Milliseconds()
		} else {
			i = x.Microseconds()
		}
	default:
		return bad()
	}
	us := i
	if strings.HasSuffix(n.logical, "-millis") {
		us = i * 1000
	}
	switch n.bq {
	case "DATE":
		t := time.Unix(0, 0).UTC().AddDate(0, 0, int(i))
		if t.Year() < 1 || t.Year() > 9999 {
			return "", valueInvalid("Error while reading data: column %s's DATE value, %d days from 1970-01-01, is "+
				"outside BigQuery's range of 0001-01-01 to 9999-12-31.", col, i)
		}
		return "DATE '" + t.Format("2006-01-02") + "'", nil
	case "TIME":
		if us < 0 || us >= 86400*1e6 {
			return "", valueInvalid("Error while reading data: column %s's TIME value, %d microseconds after midnight, is "+
				"outside a day.", col, us)
		}
		return "TIME '" + time.UnixMicro(us).UTC().Format("15:04:05.000000") + "'", nil
	case "TIMESTAMP":
		return timestampLiteral(col, time.UnixMicro(us).UTC())
	case "DATETIME":
		t := time.UnixMicro(us).UTC()
		if t.Before(minTimestamp) || t.After(maxTimestamp) {
			return "", valueInvalid("Error while reading data: column %s's DATETIME value is outside BigQuery's range of "+
				"0001-01-01 00:00:00 to 9999-12-31 23:59:59.999999.", col)
		}
		return "DATETIME '" + t.Format("2006-01-02 15:04:05.000000") + "'", nil
	}
	return strconv.FormatInt(i, 10), nil
}

// avFile is one file of an Avro load.
type avFile struct {
	uri    string
	obj    gsObject
	fields []avField
}

// avroJob is the part of an Avro load's job the front reads beyond jobBody.
type avroJob struct {
	Configuration struct {
		DryRun bool `json:"dryRun"`
		Load   struct {
			ReferenceFileSchemaURI string   `json:"referenceFileSchemaUri"`
			CreateDisposition      string   `json:"createDisposition"`
			DecimalTargetTypes     []string `json:"decimalTargetTypes"`
			UseAvroLogicalTypes    bool     `json:"useAvroLogicalTypes"`
		} `json:"load"`
	} `json:"configuration"`
}

// avroOpen opens one file's data: the upload's, or the object's.
func (f front) avroOpen(r *http.Request, upload *spooled, af avFile) (io.ReadCloser, error) {
	if af.uri == "" {
		if upload == nil {
			return nil, errors.New("the upload's data was not kept")
		}
		return io.NopCloser(io.NewSectionReader(upload.file, 0, upload.size)), nil
	}
	return f.storage.open(r.Context(), af.obj)
}

// avroReader reads an object container file's header, or says why not.
func avroReader(rd io.Reader, where string) (*goavro.OCFReader, error) {
	ocf, err := goavro.NewOCFReader(rd)
	if err == nil {
		return ocf, nil
	}
	if strings.Contains(err.Error(), "compression") || strings.Contains(err.Error(), "codec") {
		return nil, avNotHere("whose file%s is compressed by a codec the Avro decoder CloudBurrow uses does not read (%v); "+
			"it reads null, deflate and snappy", where, err)
	}
	return nil, avInvalid("Error while reading data%s: the data is not an Avro object container file BigQuery can "+
		"read: %v.", where, err)
}

// avroLoad carries out an Avro load (above) and answers w, reporting
// false; any other load it leaves, reporting true.
func (f front) avroLoad(w http.ResponseWriter, r *http.Request, job *jobBody) bool {
	l := job.Configuration.Load
	if l == nil || !strings.EqualFold(l.SourceFormat, "AVRO") || l.DestinationTable == nil {
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
			writeError(w, status, "notImplemented", "Not implemented here: an Avro load "+msg+". Nothing was loaded.")
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
		return answer(http.StatusNotImplemented, "whose file"+where+" CloudBurrow could not read ("+err.Error()+")")
	}
	var extra avroJob
	_ = decodeJob(r, &extra)
	el := extra.Configuration.Load
	switch {
	case el.ReferenceFileSchemaURI != "":
		return answer(http.StatusNotImplemented, "with referenceFileSchemaUri, which CloudBurrow does not read")
	case strings.Contains(dest.TableID, "$"):
		return answer(http.StatusNotImplemented, "into a partition ("+name+"), which CloudBurrow does not carry out "+
			"for Avro")
	case dest.ProjectID != "" && dest.ProjectID != projectOf(f.base):
		return answer(http.StatusNotImplemented, "into another project's table, "+dest.ProjectID+":"+name)
	case extra.Configuration.DryRun:
		return answer(http.StatusNotImplemented, "in a dry run")
	}
	opts := pqOptions{decimalTypes: el.DecimalTargetTypes}

	// The files and their columns.
	var files []avFile
	if len(l.SourceURIs) == 0 {
		s, err := spoolUpload(r)
		if err != nil {
			return answer(http.StatusNotImplemented, "whose data CloudBurrow could not read ("+err.Error()+")")
		}
		upload = s
		files = []avFile{{}}
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
			files = append(files, avFile{uri: o.uri(), obj: o})
		}
	}
	var file []field
	for i := range files {
		where := ""
		if files[i].uri != "" {
			where = " " + files[i].uri
		}
		rd, err := f.avroOpen(r, upload, files[i])
		if err != nil {
			var le *loadDataError
			if errors.As(err, &le) {
				writeError(w, le.code, le.reason, le.msg)
				return false
			}
			return fromErr(err, where)
		}
		ocf, err := avroReader(rd, where)
		if err != nil {
			rd.Close()
			return fromErr(err, where)
		}
		cols, err := avColumns(ocf.Codec().Schema(), el.UseAvroLogicalTypes, opts)
		rd.Close()
		if err != nil {
			return fromErr(err, where)
		}
		files[i].fields = cols
		got := avTableFields(cols)
		if i == 0 {
			file = got
		} else if !sameFields(file, got) {
			return answer(http.StatusNotImplemented, "of files whose columns differ ("+files[0].uri+": "+describeFields(file)+
				"; "+files[i].uri+": "+describeFields(got)+"); BigQuery then takes the schema of the alphabetically last "+
				"file, which CloudBurrow does not reproduce")
		}
	}
	if l.Schema != nil && !givenFits(l.Schema.Fields, file) {
		return answer(http.StatusNotImplemented, "whose job gives a schema that is not the file's ("+describeFields(l.Schema.Fields)+
			"; the file's is "+describeFields(file)+"). BigQuery reads an Avro file by the file's schema; leave the "+
			"schema out")
	}

	// The plan, as for Parquet.
	plan := &pqPlan{dest: tableRef{DatasetID: dest.DatasetID, TableID: dest.TableID},
		createNever: strings.EqualFold(el.CreateDisposition, "CREATE_NEVER")}
	write := strings.ToUpper(l.WriteDisposition)
	status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
	avroWords := func(s string) string { return strings.ReplaceAll(s, "Parquet", "Avro") }
	switch {
	case status == http.StatusNotFound:
		plan.kind = pqLoadNew
		plan.target = file
		if l.Schema != nil {
			plan.target = l.Schema.Fields
		}
		plan.raw = rawFields(plan.target, nil)
		avDescribe(plan.raw, files[0].fields)
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
			plan.target, code, why = mergeParquet(old, file, nil, dest, projectOf(f.base), &m)
		case "", "WRITE_APPEND", "WRITE_EMPTY":
			plan.kind, plan.writeEmpty = pqLoadAppend, write == "WRITE_EMPTY"
			var m pqMerge
			plan.target, code, why = mergeParquet(old, file, l.SchemaUpdateOptions, dest, projectOf(f.base), &m)
			plan.relaxed = m.relaxed
			if m.added {
				plan.kind = pqLoadAdd
			}
		default:
			return answer(http.StatusBadRequest, "Invalid value for writeDisposition: "+l.WriteDisposition+".")
		}
		if code != 0 {
			return answer(code, avroWords(why))
		}
		if plan.kind == pqLoadReplace {
			plan.raw = rawFields(plan.target, nil)
			avDescribe(plan.raw, files[0].fields)
		} else {
			plan.raw = rawFields(plan.target, tableRaw)
		}
	}

	j, ok := f.startParquetJob(w, r, "Avro")
	if !ok {
		return false
	}
	fail := func(e rowError) bool {
		f.finishOwnJob(w, r, j, "LOAD", "load", nil, &e)
		return false
	}
	rows, err := os.CreateTemp("", "bigquery-avro-rows-*")
	if err != nil {
		return answer(http.StatusNotImplemented, "whose rows CloudBurrow could not keep ("+err.Error()+")")
	}
	defer func() { rows.Close(); os.Remove(rows.Name()) }()
	bw := bufio.NewWriter(rows)
	var n, size int64
	for _, af := range files {
		where := ""
		if af.uri != "" {
			where = " " + af.uri
		}
		rd, err := f.avroOpen(r, upload, af)
		if err != nil {
			return fromErr(err, where)
		}
		var read int64
		cr := &countingReader{ReadCloser: rd, n: &read}
		ocf, err := avroReader(cr, where)
		if err != nil {
			rd.Close()
			return fromErr(err, where)
		}
		for ocf.Scan() {
			rec, err := ocf.Read()
			if err == nil {
				m, ok := rec.(map[string]any)
				if !ok {
					err = errors.New("a record is not a record")
				} else {
					var vals map[string]any
					if vals, err = avRow(af.fields, m, ""); err == nil {
						parts := make([]string, len(plan.target))
						for i, t := range plan.target {
							if parts[i], err = literalFor(t, vals[strings.ToLower(t.Name)]); err != nil {
								break
							}
						}
						if err == nil {
							_, err = bw.WriteString("(" + strings.Join(parts, ", ") + ")\n")
						}
					}
				}
			}
			if err != nil {
				rd.Close()
				var ve *pqValueError
				switch {
				case errors.As(err, &ve) && ve.status == http.StatusBadRequest:
					return fail(rowError{Reason: "invalid", Location: af.uri, Message: ve.msg})
				case errors.As(err, &ve):
					return answer(http.StatusNotImplemented, "of a file"+where+" whose "+ve.msg)
				}
				return fail(rowError{Reason: "invalid", Location: af.uri, Message: "Error while reading data" + where +
					": " + err.Error() + "."})
			}
			n++
		}
		err = ocf.Err()
		_, _ = io.Copy(io.Discard, cr)
		rd.Close()
		if err != nil {
			return fail(rowError{Reason: "invalid", Location: af.uri, Message: "Error while reading data" + where + ": " +
				err.Error() + "."})
		}
		size += read
	}
	if err := bw.Flush(); err != nil {
		return answer(http.StatusNotImplemented, "whose rows CloudBurrow could not keep ("+err.Error()+")")
	}

	project := projectOf(f.base)
	full := tableName(project, plan.dest)
	if plan.kind == pqLoadNew {
		if plan.createNever {
			return fail(rowError{Reason: "notFound", Message: "Not found: Table " + full})
		}
		if status, _ := f.get(r, "/datasets/"+url.PathEscape(plan.dest.DatasetID)); status == http.StatusNotFound {
			return fail(rowError{Reason: "notFound", Message: "Not found: Dataset " + project + ":" + plan.dest.DatasetID})
		}
	} else if plan.writeEmpty && !f.emptyTable(r, plan.dest.DatasetID, plan.dest.TableID, nil) {
		return fail(rowError{Reason: "duplicate", Message: "Already Exists: Table " + full})
	}
	if msg := f.carryOutParquet(r, plan, rows); msg != "" {
		return fail(rowError{Reason: "backendError", Message: "CloudBurrow could not load the Avro file's rows into " +
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
