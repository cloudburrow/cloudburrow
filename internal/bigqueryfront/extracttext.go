package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The text of each value in a CSV or NEWLINE_DELIMITED_JSON extract the
// front writes (#957), from the value tabledata.list gives (measured
// against the pinned emulator: a TIMESTAMP as seconds since the epoch,
// "1577934245.123456"; a DATETIME "2020-01-02T03:04:05.5"; an INTERVAL
// "1-2 3 4:5:6.789"; a RANGE<DATE> "[2020-01-01, UNBOUNDED)" and a
// RANGE<TIMESTAMP>'s bounds in microseconds; a JSON value as compact JSON
// text; a GEOGRAPHY as WKT).
//
// BigQuery's export documentation
// (https://cloud.google.com/bigquery/docs/exporting-data) gives the text
// of few types, so each is written in the canonical form the data types
// reference (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types)
// gives it:
//
//   - TIMESTAMP: "YYYY-MM-DD HH:MM:SS[.ffffff] UTC", the canonical form
//     with the time zone UTC (the emulator wrote "+00"; BigQuery writes
//     "UTC").
//   - DATETIME: "civil_date_part[time_part]", its time part after a "T"
//     ("{ |T|t}"), tabledata.list's form; DATE "YYYY-[M]M-[D]D" and TIME
//     as tabledata.list gives them.
//   - INTERVAL: "[sign]Y-M [sign]D [sign]H:M:S[.F]", as tabledata.list
//     gives it.
//   - RANGE: "[lower_bound, upper_bound)", each bound in its element
//     type's text, UNBOUNDED for none.
//   - GEOGRAPHY: "When you export spatial data from BigQuery, GEOGRAPHY
//     column values are always formatted as WKT strings" (gis-data).
//   - BYTES: base64, as the emulator's CSV (measured to be BigQuery's).
//   - FLOAT64: the shortest decimal that reads back as the value
//     (tabledata.list's "1.5", "1e+20"). NaN and infinities are not
//     written: the documentation gives no text for them in an export.
//
// Which bytes BigQuery writes for each is UNVERIFIED beyond that text.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data

// extractText is the CSV and JSON text of a scalar value s of fl, as
// tabledata.list gives it.
func extractText(fl field, s string) (string, error) {
	return extractTypeText(strings.ToUpper(fl.Type), rangeElement(fl), s)
}

func rangeElement(fl field) string {
	if fl.RangeElementType == nil {
		return ""
	}
	return strings.ToUpper(fl.RangeElementType.Type)
}

func extractTypeText(typ, elem, s string) (string, error) {
	switch typ {
	case "TIMESTAMP":
		// A column's value is seconds since the epoch ("1577934245",
		// "1.5779e9"); a RANGE bound microseconds or the engine's text
		// (parseTimestamp).
		t, err := parseTimestamp(s) // storagerows.go
		if elem != "bound" {
			t, err = timestampSeconds(s)
		}
		if err != nil {
			return "", err
		}
		return t.Format("2006-01-02 15:04:05.999999") + " UTC", nil
	case "DATETIME":
		t, err := parseDateTime(s)
		if err != nil {
			return "", err
		}
		return t.Format("2006-01-02T15:04:05.999999"), nil
	case "FLOAT", "FLOAT64":
		switch s {
		case "NaN", "Infinity", "-Infinity":
			return "", fmt.Errorf("a FLOAT64 %s, whose text in an export BigQuery's documentation does not give", s)
		}
		return s, nil
	case "RANGE":
		if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ')' {
			return "", fmt.Errorf("unreadable RANGE %q", s)
		}
		parts := strings.Split(s[1:len(s)-1], ", ")
		if len(parts) != 2 {
			return "", fmt.Errorf("unreadable RANGE %q", s)
		}
		for i, p := range parts {
			if p == "UNBOUNDED" || p == "NULL" {
				parts[i] = "UNBOUNDED"
				continue
			}
			v, err := extractTypeText(elem, "bound", p)
			if err != nil {
				return "", err
			}
			parts[i] = v
		}
		return "[" + parts[0] + ", " + parts[1] + ")", nil
	}
	return s, nil
}

// timestampSeconds reads tabledata.list's TIMESTAMP, seconds since the
// epoch as a decimal number, to the microsecond.
func timestampSeconds(s string) (time.Time, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return parseTimestamp(s)
	}
	r.Mul(r, big.NewRat(1e6, 1))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() {
		return time.Time{}, fmt.Errorf("unreadable TIMESTAMP %q", s)
	}
	return time.UnixMicro(n.Int64()).UTC(), nil
}

// jsonExtractRow writes a row of fields, as tabledata.list gives it, as
// one NEWLINE_DELIMITED_JSON object, its members in the schema's order
// with no spaces between them. A NULL is left out, as is an empty
// REPEATED column, which BigQuery does not tell from a NULL (UNVERIFIED:
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
// the documentation does not say how a NULL is written). A RECORD is a
// nested object and a REPEATED column an array ("Nested and repeated data
// are supported for Avro, JSON, and Parquet exports"); INT64, NUMERIC and
// BIGNUMERIC are JSON strings ("INT64 (integer) data types are encoded as
// JSON strings to preserve 64-bit precision", NUMERIC and BIGNUMERIC so by
// analogy, UNVERIFIED); FLOAT64 a JSON number, BOOL a JSON boolean, a JSON
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
// column its JSON value (UNVERIFIED), and the rest JSON strings of their
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
// text (extractText). "The symbols <, >, and & are converted by using the
// unicode notation": encoding/json writes them so.
func jsonExtractRow(fields []field, row []any) ([]byte, error) {
	var b bytes.Buffer
	if err := jsonExtractObject(&b, fields, row); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func jsonExtractObject(b *bytes.Buffer, fields []field, row []any) error {
	b.WriteByte('{')
	first := true
	for j, fl := range fields {
		var raw any
		if j < len(row) {
			raw = row[j]
		}
		if raw == nil {
			if strings.EqualFold(fl.Mode, "REQUIRED") {
				return fmt.Errorf("column %s: a NULL in a REQUIRED column", fl.Name)
			}
			continue
		}
		var val bytes.Buffer
		if strings.EqualFold(fl.Mode, "REPEATED") {
			items, _ := raw.([]any)
			if len(items) == 0 {
				continue
			}
			val.WriteByte('[')
			for i, it := range items {
				if i > 0 {
					val.WriteByte(',')
				}
				v := cellValue(it)
				if v == nil {
					return fmt.Errorf("column %s: a NULL in an array", fl.Name)
				}
				if err := jsonExtractValue(&val, fl, v); err != nil {
					return err
				}
			}
			val.WriteByte(']')
		} else if err := jsonExtractValue(&val, fl, raw); err != nil {
			return err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		name, _ := json.Marshal(fl.Name)
		b.Write(name)
		b.WriteByte(':')
		b.Write(val.Bytes())
	}
	b.WriteByte('}')
	return nil
}

// jsonExtractValue writes one value (not an array) of fl.
func jsonExtractValue(b *bytes.Buffer, fl field, v any) error {
	typ := strings.ToUpper(fl.Type)
	if typ == "RECORD" || typ == "STRUCT" {
		cells, err := recordCells(v) // storagerows.go
		if err != nil {
			return fmt.Errorf("column %s: %v", fl.Name, err)
		}
		vals := make([]any, len(cells))
		for i, c := range cells {
			vals[i] = cellValue(c)
		}
		return jsonExtractObject(b, fl.Fields, vals)
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("column %s: a value of %T", fl.Name, v)
	}
	switch typ {
	case "BOOLEAN", "BOOL":
		switch s {
		case "true", "false":
			b.WriteString(s)
			return nil
		}
		return fmt.Errorf("column %s: unreadable BOOL %q", fl.Name, s)
	case "FLOAT", "FLOAT64":
		t, err := extractText(fl, s)
		if err != nil {
			return fmt.Errorf("column %s: %v", fl.Name, err)
		}
		var n json.Number = json.Number(t)
		out, err := json.Marshal(n)
		if err != nil {
			return fmt.Errorf("column %s: %v", fl.Name, err)
		}
		b.Write(out)
		return nil
	case "JSON":
		var c bytes.Buffer
		if err := json.Compact(&c, []byte(s)); err != nil {
			return fmt.Errorf("column %s: %v", fl.Name, err)
		}
		// Re-encoded so <, > and & are written in unicode notation.
		var val any
		dec := json.NewDecoder(&c)
		dec.UseNumber()
		if err := dec.Decode(&val); err != nil {
			return fmt.Errorf("column %s: %v", fl.Name, err)
		}
		out, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("column %s: %v", fl.Name, err)
		}
		b.Write(out)
		return nil
	}
	t, err := extractText(fl, s)
	if err != nil {
		return fmt.Errorf("column %s: %v", fl.Name, err)
	}
	out, _ := json.Marshal(t)
	b.Write(out)
	return nil
}
