package bigqueryfront

import "testing"

// TestInformationSchemaViewsReferencesAreFound: #1274.
func TestInformationSchemaViewsReferencesAreFound(t *testing.T) {
	for _, c := range []struct {
		sql      string
		datasets []string
		alias    []bool
	}{
		{"SELECT * FROM ds.INFORMATION_SCHEMA.VIEWS", []string{"ds"}, []bool{false}},
		{"SELECT v.table_name FROM `p.ds.INFORMATION_SCHEMA.VIEWS` v WHERE 1=1", []string{"ds"}, []bool{true}},
		{"SELECT * FROM ds.information_schema.views AS x JOIN e.INFORMATION_SCHEMA.VIEWS USING (table_name)", []string{"ds", "e"}, []bool{true, false}},
		{"SELECT * FROM ds.INFORMATION_SCHEMA.TABLES", nil, nil},
		{"SELECT 'ds.INFORMATION_SCHEMA.VIEWS'", nil, nil},
	} {
		refs := informationSchemaViews(c.sql)
		if len(refs) != len(c.datasets) {
			t.Fatalf("%s: %+v", c.sql, refs)
		}
		for i, ref := range refs {
			if ref.dataset != c.datasets[i] || ref.alias != c.alias[i] {
				t.Errorf("%s: ref %d %+v", c.sql, i, ref)
			}
		}
	}
}
