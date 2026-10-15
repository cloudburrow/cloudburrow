package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/linkedin/goavro/v2"
)

// encodeAvroExtract writes rows as an Avro object container file (#957):
// see writeExtract for the form and why it is BigQuery's. It returns why a
// row cannot be written, or "".
func encodeAvroExtract(x writtenExtract, rows [][]any) ([]byte, string) {
	schema, err := json.Marshal(map[string]any{"type": "record", "name": "Root", "fields": avroFields(x.fields, x.avroLogical, "Root")})
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	codec := x.avroCodec
	if codec == "" {
		codec = goavro.CompressionNullLabel
	}
	var buf bytes.Buffer
	ocf, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &buf, Schema: string(schema), CompressionName: codec})
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	records := make([]any, 0, len(rows))
	for i, row := range rows {
		rec, why := avroRecord(x.fields, row, x.avroLogical, "Root")
		if why != "" {
			return nil, fmt.Sprintf("row %d, %s", i+1, why)
		}
		records = append(records, rec)
	}
	if len(records) > 0 {
		if err := ocf.Append(records); err != nil {
			return nil, err.Error()
		}
	}
	return buf.Bytes(), ""
}

// avroExportSchema is the Avro type BigQuery's documentation gives a column of
// type typ ("Avro export details",
// https://cloud.google.com/bigquery/docs/exporting-data#avro_export_details),
// for an extract job with useAvroLogicalTypes logical or not, and its name
// in a union; "" for a type the front does not write (avroExportType).
// A RECORD is not here (avroFields).
func avroExportSchema(typ string, logical bool) (any, string) {
	switch strings.ToUpper(typ) {
	case "STRING":
		return "string", "string"
	case "INTEGER", "INT64":
		return "long", "long"
	case "FLOAT", "FLOAT64":
		return "double", "double"
	case "BOOLEAN", "BOOL":
		return "boolean", "boolean"
	case "BYTES":
		return "bytes", "bytes"
	case "TIMESTAMP":
		// "in Extract jobs, it always uses the Avro logical type".
		return map[string]any{"type": "long", "logicalType": "timestamp-micros"}, "long.timestamp-micros"
	case "DATE":
		// "represented as string type by default in Extract jobs".
		if logical {
			return map[string]any{"type": "int", "logicalType": "date"}, "int.date"
		}
		return "string", "string"
	case "TIME":
		if logical {
			return map[string]any{"type": "long", "logicalType": "time-micros"}, "long.time-micros"
		}
	case "NUMERIC":
		return map[string]any{"type": "bytes", "logicalType": "decimal", "precision": 38, "scale": 9}, "bytes.decimal"
	case "BIGNUMERIC":
		return map[string]any{"type": "bytes", "logicalType": "decimal", "precision": 77, "scale": 38}, "bytes.decimal"
	}
	return nil, ""
}

// avroExportType reports whether the front writes a column of type typ to
// Avro (avroExportSchema), RECORDs included.
func avroExportType(typ string, logical bool) bool {
	if t := strings.ToUpper(typ); t == "RECORD" || t == "STRUCT" {
		return true
	}
	_, name := avroExportSchema(typ, logical)
	return name != ""
}

// avroFields is the Avro record fields of a schema's fields: each of the
// type avroExportSchema gives, a RECORD as a record of its fields ("Nested data is
// represented by nested record objects"), named by its path below the
// root record, which the documentation does not give (UNVERIFIED against
// BigQuery); a NULLABLE one as a union with "null", a REPEATED one as an
// array.
func avroFields(fields []field, logical bool, path string) []any {
	out := make([]any, 0, len(fields))
	for _, fl := range fields {
		var typ any
		if t := strings.ToUpper(fl.Type); t == "RECORD" || t == "STRUCT" {
			typ = map[string]any{"type": "record", "name": path + "__" + fl.Name, "fields": avroFields(fl.Fields, logical, path+"__"+fl.Name)}
		} else {
			typ, _ = avroExportSchema(fl.Type, logical)
		}
		switch strings.ToUpper(fl.Mode) {
		case "REQUIRED":
		case "REPEATED":
			typ = map[string]any{"type": "array", "items": typ}
		default:
			typ = []any{"null", typ}
		}
		out = append(out, map[string]any{"name": fl.Name, "type": typ})
	}
	return out
}

// avroRecord is a row's values, as tabledata.list gives them, as the Go
// record goavro writes for fields (avroFields).
func avroRecord(fields []field, row []any, logical bool, path string) (map[string]any, string) {
	rec := make(map[string]any, len(fields))
	for j, fl := range fields {
		var raw any
		if j < len(row) {
			raw = row[j]
		}
		mode := strings.ToUpper(fl.Mode)
		if mode == "REPEATED" {
			items, _ := raw.([]any)
			vals := make([]any, 0, len(items))
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					it = m["v"]
				}
				v, why := avroFieldValue(fl, it, logical, path)
				if why != "" {
					return nil, why
				}
				vals = append(vals, v)
			}
			rec[fl.Name] = vals
			continue
		}
		if raw == nil {
			if mode == "REQUIRED" {
				return nil, fmt.Sprintf("column %s: a NULL in a REQUIRED column", fl.Name)
			}
			rec[fl.Name] = nil
			continue
		}
		v, why := avroFieldValue(fl, raw, logical, path)
		if why != "" {
			return nil, why
		}
		if mode == "REQUIRED" {
			rec[fl.Name] = v
		} else {
			name := path + "__" + fl.Name // a record's full name
			if t := strings.ToUpper(fl.Type); t != "RECORD" && t != "STRUCT" {
				_, name = avroExportSchema(fl.Type, logical)
			}
			rec[fl.Name] = goavro.Union(name, v)
		}
	}
	return rec, ""
}

// avroFieldValue is one value (not an array) of fl.
func avroFieldValue(fl field, raw any, logical bool, path string) (any, string) {
	if t := strings.ToUpper(fl.Type); t == "RECORD" || t == "STRUCT" {
		m, _ := raw.(map[string]any)
		cells, _ := m["f"].([]any)
		vals := make([]any, len(cells))
		for i, c := range cells {
			if cm, ok := c.(map[string]any); ok {
				vals[i] = cm["v"]
			}
		}
		return avroRecord(fl.Fields, vals, logical, path+"__"+fl.Name)
	}
	if raw == nil {
		return nil, fmt.Sprintf("column %s: a NULL in an array", fl.Name)
	}
	s, _ := raw.(string)
	_, name := avroExportSchema(fl.Type, logical)
	v, err := avroValue(name, s)
	if err != nil {
		return nil, fmt.Sprintf("column %s: %v", fl.Name, err)
	}
	return v, ""
}

// avroValue is a tabledata.list value s as the Go value goavro writes as
// the Avro type named typ (avroExportSchema).
func avroValue(typ, s string) (any, error) {
	switch typ {
	case "long":
		return strconv.ParseInt(s, 10, 64)
	case "double":
		switch s {
		case "Infinity":
			s = "+Inf"
		case "-Infinity":
			s = "-Inf"
		}
		return strconv.ParseFloat(s, 64)
	case "boolean":
		return strconv.ParseBool(s)
	case "bytes":
		return base64.StdEncoding.DecodeString(s)
	case "long.timestamp-micros":
		// tabledata.list gives seconds since the epoch, as a decimal
		// number ("1.5779e9" or "1577934245.123456").
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return nil, fmt.Errorf("TIMESTAMP %q", s)
		}
		r.Mul(r, big.NewRat(1e6, 1))
		if !r.IsInt() || !r.Num().IsInt64() {
			return nil, fmt.Errorf("TIMESTAMP %q", s)
		}
		return time.UnixMicro(r.Num().Int64()).UTC(), nil
	case "int.date":
		return time.Parse("2006-01-02", s)
	case "long.time-micros":
		t, err := time.Parse("15:04:05.999999", s)
		if err != nil {
			return nil, err
		}
		return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
			time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond()), nil
	case "bytes.decimal":
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return nil, fmt.Errorf("NUMERIC %q", s)
		}
		return r, nil
	}
	return s, nil
}
