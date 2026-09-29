package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// fieldSelection is tabledata.list's selectedFields (#1038), parsed
// against the table's schema: for each chosen top-level field (by its
// index in the schema), nil for the whole field, or the selection of its
// subfields when only some were named ("for nested fields, list e.g.
// a.b", https://cloud.google.com/bigquery/docs/reference/rest/v2/tabledata/list#query-parameters).
type fieldSelection map[int]fieldSelection

// parseSelectedFields reads selectedFields, a comma separated list of
// (dotted) field names, against fields. Names are case-insensitive, as
// BigQuery's column names are. A name the schema does not have is an
// error. The cells come back in the schema's order, whatever the
// parameter's order (not measured against BigQuery; the Python and Go
// clients read the cells by the schema they were given).
func parseSelectedFields(fields []field, param string) (fieldSelection, error) {
	sel := fieldSelection{}
	for _, name := range strings.Split(param, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := sel.add(fields, strings.Split(name, "."), name); err != nil {
			return nil, err
		}
	}
	if len(sel) == 0 {
		return nil, nil
	}
	return sel, nil
}

func (s fieldSelection) add(fields []field, path []string, whole string) error {
	for i, fl := range fields {
		if !strings.EqualFold(fl.Name, path[0]) {
			continue
		}
		if len(path) == 1 {
			s[i] = nil
			return nil
		}
		if len(fl.Fields) == 0 {
			return fmt.Errorf("Invalid field selection %q: field %s is not a RECORD", whole, fl.Name)
		}
		sub, seen := s[i]
		if seen && sub == nil {
			return nil // the whole record was already chosen
		}
		if sub == nil {
			sub = fieldSelection{}
			s[i] = sub
		}
		return sub.add(fl.Fields, path[1:], whole)
	}
	return fmt.Errorf("Invalid field selection %q: field %s not found in the table's schema", whole, path[0])
}

// selectRows keeps, of each row ({"f":[{"v":...}]}, in fields' order),
// the cells sel chose.
func selectRows(fields []field, sel fieldSelection, rows []json.RawMessage) []json.RawMessage {
	if sel == nil {
		return rows
	}
	for i, raw := range rows {
		var row any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&row) != nil {
			continue
		}
		if b, err := json.Marshal(selectStruct(fields, sel, row)); err == nil {
			rows[i] = b
		}
	}
	return rows
}

// selectStruct keeps sel's cells of a struct value {"f":[...]}.
func selectStruct(fields []field, sel fieldSelection, v any) any {
	m, _ := v.(map[string]any)
	cells, _ := m["f"].([]any)
	if m == nil || len(cells) != len(fields) {
		return v
	}
	kept := make([]any, 0, len(sel))
	for i, fl := range fields {
		sub, ok := sel[i]
		if !ok {
			continue
		}
		cell := cells[i]
		if sub != nil {
			if c, _ := cell.(map[string]any); c != nil {
				if strings.EqualFold(fl.Mode, "REPEATED") {
					if elems, ok := c["v"].([]any); ok {
						for j, e := range elems {
							if em, _ := e.(map[string]any); em != nil {
								em["v"] = selectStruct(fl.Fields, sub, em["v"])
								elems[j] = em
							}
						}
					}
				} else {
					c["v"] = selectStruct(fl.Fields, sub, c["v"])
				}
			}
		}
		kept = append(kept, cell)
	}
	m["f"] = kept
	return m
}
