package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// field is the part of a TableFieldSchema the checks read.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#TableFieldSchema
type field struct {
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Mode   string  `json:"mode"`
	Fields []field `json:"fields"`
	// RangeElementType is a RANGE field's element type (#1111).
	RangeElementType *struct {
		Type string `json:"type"`
	} `json:"rangeElementType,omitempty"`
	// DefaultValueExpression is a column's default (#1340,
	// columndefaults.go).
	DefaultValueExpression string `json:"defaultValueExpression,omitempty"`
	// Precision and Scale are a parameterized NUMERIC's or BIGNUMERIC's
	// (#957, extracts): int64s, which REST gives as strings.
	Precision int64String `json:"precision,omitempty"`
	Scale     int64String `json:"scale,omitempty"`
}

// int64String is a REST int64: a JSON string of digits, also read from a
// JSON number, and written as a string, as the REST API writes one.
type int64String string

func (n *int64String) UnmarshalJSON(b []byte) error {
	var v json.Number
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*n = int64String(v)
	return nil
}

func (n int64String) MarshalJSON() ([]byte, error) { return json.Marshal(string(n)) }

// tableSchema is a Table's schema.
type tableSchema struct {
	Fields []field `json:"fields"`
}

// fieldTypes are the TableFieldSchema.type values the REST reference lists,
// legacy names and their GoogleSQL aliases alike.
var fieldTypes = map[string]bool{
	"STRING": true, "BYTES": true, "INTEGER": true, "INT64": true, "FLOAT": true, "FLOAT64": true,
	"NUMERIC": true, "BIGNUMERIC": true, "BOOLEAN": true, "BOOL": true, "TIMESTAMP": true,
	"DATE": true, "TIME": true, "DATETIME": true, "GEOGRAPHY": true, "RECORD": true, "STRUCT": true,
	"JSON": true, "INTERVAL": true, "RANGE": true,
}

// fieldModes are TableFieldSchema.mode's values; empty means NULLABLE.
var fieldModes = map[string]bool{"": true, "NULLABLE": true, "REQUIRED": true, "REPEATED": true}

// checkSchema returns why a schema BigQuery would refuse is invalid, or "".
// It checks each name, that no two fields at one level share a name
// ("Duplicate column names are not allowed even if the case differs",
// https://cloud.google.com/bigquery/docs/schemas), and each type and mode.
//
// The emulator checks only duplicates, and answers them 500 (measured,
// #861); a 500 is retried by the Go client until its deadline.
func checkSchema(fields []field, prefix string) string {
	seen := map[string]string{}
	for _, f := range fields {
		if msg := checkColumnName(f.Name); msg != "" {
			return msg
		}
		key := strings.ToLower(f.Name)
		if prev, dup := seen[key]; dup {
			return fmt.Sprintf("Field %s%s already exists in schema (as %s%s); field names are case-insensitive", prefix, f.Name, prefix, prev)
		}
		seen[key] = f.Name
		typ := strings.ToUpper(f.Type)
		if !fieldTypes[typ] {
			return fmt.Sprintf("Invalid field type %q for field %s%s", f.Type, prefix, f.Name)
		}
		if !fieldModes[strings.ToUpper(f.Mode)] {
			return fmt.Sprintf("Invalid field mode %q for field %s%s", f.Mode, prefix, f.Name)
		}
		isRecord := typ == "RECORD" || typ == "STRUCT"
		if isRecord && len(f.Fields) == 0 {
			return fmt.Sprintf("Field %s%s is type %s but has no schema", prefix, f.Name, typ)
		}
		if !isRecord && len(f.Fields) > 0 {
			return fmt.Sprintf("Field %s%s is type %s and cannot have subfields", prefix, f.Name, typ)
		}
		if isRecord {
			if msg := checkSchema(f.Fields, prefix+f.Name+"."); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// decimalParams is the precision and scale of a NUMERIC or BIGNUMERIC
// field, and whether they are its own (a parameterized NUMERIC(P[, S]) or
// BIGNUMERIC(P[, S]), whose scale defaults to 0) rather than the type's
// (38, 9) or (76, 38). Other types give (0, 0, false).
func decimalParams(fl field) (precision, scale int, parameterized bool) {
	switch strings.ToUpper(fl.Type) {
	case "NUMERIC":
		precision, scale = 38, 9
	case "BIGNUMERIC":
		precision, scale = 76, 38
	default:
		return 0, 0, false
	}
	if fl.Precision == "" {
		return precision, scale, false
	}
	p, err := strconv.Atoi(string(fl.Precision))
	if err != nil {
		return precision, scale, false
	}
	sc := 0
	if fl.Scale != "" {
		if sc, err = strconv.Atoi(string(fl.Scale)); err != nil {
			return precision, scale, false
		}
	}
	return p, sc, true
}

// decimalFits reports, as an error, a decimal text s with more than scale
// digits after the point or more than precision-scale before it: a value
// that a NUMERIC or BIGNUMERIC of that precision and scale cannot hold
// as it is, and that the front therefore cannot write as BigQuery would.
func decimalFits(s string, precision, scale int) error {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return fmt.Errorf("unreadable decimal %q", s)
	}
	ten := func(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }
	r.Mul(r, new(big.Rat).SetInt(ten(scale)))
	if !r.IsInt() {
		return fmt.Errorf("%s has more than %d digits after the point, which its NUMERIC(%d, %d) or "+
			"BIGNUMERIC(%d, %d) type cannot hold (BigQuery would not hold this value)", s, scale, precision, scale, precision, scale)
	}
	if new(big.Int).Abs(r.Num()).Cmp(ten(precision)) >= 0 {
		return fmt.Errorf("%s has more than %d digits, which its NUMERIC(%d, %d) or BIGNUMERIC(%d, %d) type "+
			"cannot hold (BigQuery would not hold this value)", s, precision, precision, scale, precision, scale)
	}
	return nil
}
