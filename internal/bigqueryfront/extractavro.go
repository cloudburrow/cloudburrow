package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/linkedin/goavro/v2"
)

// encodeAvroExtract writes rows as an Avro object container file (#957):
// see writeExtract for the form and why it is BigQuery's. It returns why a
// row cannot be written, or "".
func encodeAvroExtract(x writtenExtract, rows [][]any) ([]byte, string) {
	var fields []string
	for _, fl := range x.fields {
		typ := avroExportTypes[strings.ToUpper(fl.Type)]
		name := strconv.Quote(fl.Name)
		if strings.EqualFold(fl.Mode, "REQUIRED") {
			fields = append(fields, fmt.Sprintf(`{"name":%s,"type":%q}`, name, typ))
		} else {
			fields = append(fields, fmt.Sprintf(`{"name":%s,"type":["null",%q]}`, name, typ))
		}
	}
	schema := `{"type":"record","name":"Root","fields":[` + strings.Join(fields, ",") + `]}`
	codec := x.avroCodec
	if codec == "" {
		codec = goavro.CompressionNullLabel
	}
	var buf bytes.Buffer
	ocf, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &buf, Schema: schema, CompressionName: codec})
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	records := make([]any, 0, len(rows))
	for i, row := range rows {
		rec := make(map[string]any, len(x.fields))
		for j, fl := range x.fields {
			typ := avroExportTypes[strings.ToUpper(fl.Type)]
			var raw any
			if j < len(row) {
				raw = row[j]
			}
			if raw == nil {
				if strings.EqualFold(fl.Mode, "REQUIRED") {
					return nil, fmt.Sprintf("row %d, column %s: a NULL in a REQUIRED column", i+1, fl.Name)
				}
				rec[fl.Name] = nil
				continue
			}
			s, _ := raw.(string)
			v, err := avroValue(typ, s)
			if err != nil {
				return nil, fmt.Sprintf("row %d, column %s: %v", i+1, fl.Name, err)
			}
			if strings.EqualFold(fl.Mode, "REQUIRED") {
				rec[fl.Name] = v
			} else {
				rec[fl.Name] = goavro.Union(typ, v)
			}
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

// avroValue is a tabledata.list value s as the Go value goavro writes as
// the Avro type typ.
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
	}
	return s, nil
}
