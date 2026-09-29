package bigqueryfront

import "testing"

// TestDatasetIDVariable (#1137): each @@dataset_id of a query with a
// default dataset is sent as the dataset's ID, but in a string, a
// comment, a query that sets the variable, and the body of a CREATE
// FUNCTION or VIEW; with no default dataset nothing changes.
func TestDatasetIDVariable(t *testing.T) {
	for _, c := range []struct {
		sql, dataset, want string
		changed            bool
	}{
		{"SELECT @@dataset_id", "two", "SELECT 'two'", true},
		{"SELECT @@DATASET_ID AS d, x FROM t WHERE s = @@dataset_id", "d_1", "SELECT 'd_1' AS d, x FROM t WHERE s = 'd_1'", true},
		{"SELECT '@@dataset_id', @@dataset_id -- @@dataset_id", "two", "SELECT '@@dataset_id', 'two' -- @@dataset_id", true},
		{"SELECT @@dataset_project_id, @@project_id, @dataset_id", "two", "SELECT @@dataset_project_id, @@project_id, @dataset_id", false},
		{"SELECT @@dataset_id", "", "SELECT @@dataset_id", false},
		{"SET @@dataset_id = 'one'; SELECT @@dataset_id", "two", "SET @@dataset_id = 'one'; SELECT @@dataset_id", false},
		{"CREATE VIEW two.v AS SELECT @@dataset_id AS d; SELECT @@dataset_id", "two",
			"CREATE VIEW two.v AS SELECT @@dataset_id AS d; SELECT 'two'", true},
		{"IF @@dataset_id = 'two' THEN SELECT @@dataset_id; END IF", "two", "IF 'two' = 'two' THEN SELECT 'two'; END IF", true},
	} {
		got, changed := datasetIDVariable(c.sql, c.dataset)
		if got != c.want || changed != c.changed {
			t.Errorf("%q with %q: %q %v, want %q %v", c.sql, c.dataset, got, changed, c.want, c.changed)
		}
	}
}
