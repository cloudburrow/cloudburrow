package bigqueryfront

import (
	"reflect"
	"testing"
)

func TestDatasetOptions(t *testing.T) {
	ds := map[string]any{
		"description":              "the dataset",
		"friendlyName":             "Data",
		"defaultTableExpirationMs": "129600000",
		"maxTimeTravelHours":       "96",
		"labels":                   map[string]any{"z": "1", "a": "2"},
		"location":                 "US",
	}
	want := [][3]string{
		{"default_table_expiration_days", "FLOAT64", "1.5"},
		{"description", "STRING", `"the dataset"`},
		{"friendly_name", "STRING", `"Data"`},
		{"labels", "ARRAY<STRUCT<STRING, STRING>>", `[STRUCT("a", "2"), STRUCT("z", "1")]`},
		{"max_time_travel_hours", "INT64", "96"},
	}
	if got := datasetOptions(ds); !reflect.DeepEqual(got, want) {
		t.Errorf("datasetOptions:\n got %v\nwant %v", got, want)
	}
	if got := datasetOptions(map[string]any{}); len(got) != 0 {
		t.Errorf("an empty dataset's options: %v", got)
	}
	for _, v := range []string{"SCHEMATA_OPTIONS", "MATERIALIZED_VIEWS"} {
		if unbuiltInfoSchema[v] || !regionViews[v] || regionColumns[v] == "" || infoSchemaBuilders[v] == nil {
			t.Errorf("%s is not registered as a dataset and region view", v)
		}
	}
	if !unbuiltInfoSchema["SCHEMATA_LINKS"] || !unbuiltInfoSchema["OBJECT_PRIVILEGES"] {
		t.Error("SCHEMATA_LINKS and OBJECT_PRIVILEGES should stay 501")
	}
}
