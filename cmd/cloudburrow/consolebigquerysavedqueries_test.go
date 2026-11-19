package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBigQuerySavedQueries (#1861): Save query keeps a saved query in the
// instance's state directory, a save of the same one is a new version, a
// name is unique in its project, the region is Google's and cannot be
// changed, and Delete removes it.
func TestBigQuerySavedQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inst", bqSavedQueriesFile)
	s := bqSavedQueryStore{path: path}
	now := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	if got, err := s.list("p"); err != nil || len(got) != 0 {
		t.Fatalf("an empty store lists %v, %v", got, err)
	}
	q, err := s.save("p", bqSavedQuery{Name: " Daily ", Region: "EU", Query: "SELECT 1"}, now)
	if err != nil || q.ID == "" || q.Name != "Daily" || q.Version != 1 || q.Region != "EU" {
		t.Fatalf("Save query = %+v, %v", q, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record was not written: %v", err)
	}
	q2, err := s.save("p", bqSavedQuery{ID: q.ID, Name: "Daily", Query: "SELECT 2"}, now.Add(time.Minute))
	if err != nil || q2.Version != 2 || q2.Query != "SELECT 2" || q2.Created != q.Created || q2.Updated == q.Updated {
		t.Fatalf("a second save = %+v, %v", q2, err)
	}
	for name, in := range map[string]bqSavedQuery{
		"already exists":      {Name: "daily", Query: "x"},
		"needs a name":        {Name: "  "},
		"is not a BigQuery":   {Name: "other", Region: "mars-north1"},
		"cannot be changed":   {ID: q.ID, Name: "Daily", Region: "US"},
		"no saved query nope": {ID: "nope", Name: "x"},
	} {
		if _, err := s.save("p", in, now); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("save %+v = %v, want %q", in, err, name)
		}
	}
	// Another project's list does not hold it, and may use the name.
	if _, err := s.save("other", bqSavedQuery{Name: "Daily", Query: "SELECT 3"}, now); err != nil {
		t.Errorf("the same name in another project: %v", err)
	}
	if got, _ := s.list("p"); len(got) != 1 || got[0].ID != q.ID {
		t.Errorf("project p lists %+v", got)
	}
	if err := s.remove("p", q.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ := s.list("p"); len(got) != 0 {
		t.Errorf("after Delete project p lists %+v", got)
	}
	if err := (bqSavedQueryStore{}).remove("p", "x"); err == nil {
		t.Error("a store without a state directory accepted a delete")
	}
}

// TestBigQueryCreateDatasetLocations (#1861): the Region list is Google's,
// named "ID (City)", and a location outside it is refused.
func TestBigQueryCreateDatasetLocations(t *testing.T) {
	var region, multi bool
	for _, f := range bigqueryCreateDatasetFields() {
		switch f.Name {
		case "region":
			region = f.Type == "select" && len(f.Options) == len(bqRegionNames) && f.OptionLabels["us-central1"] == "us-central1 (Iowa)" &&
				f.OptionLabels["africa-south1"] == "africa-south1 (Johannesburg)"
		case "multiRegion":
			multi = strings.Join(f.Options, ",") == "US,EU" && f.OptionLabels["EU"] != ""
		}
	}
	if !region || !multi {
		t.Errorf("the location fields are not Google's lists: region %v, multi-region %v", region, multi)
	}
	ds, err := bigqueryDatasetFromForm("p", map[string]string{"datasetId": "d", "locationType": bqLocationRegion, "region": "me-central2"})
	if err != nil || ds.Location != "me-central2" {
		t.Errorf("region me-central2 = %v, %v", ds, err)
	}
	if _, err := bigqueryDatasetFromForm("p", map[string]string{"datasetId": "d", "locationType": bqLocationRegion, "region": "mars-north1"}); err == nil {
		t.Error("an unknown region was accepted")
	}
	if _, err := bigqueryDatasetFromForm("p", map[string]string{"datasetId": "d", "locationType": bqLocationMultiRegion, "multiRegion": "ASIA"}); err == nil {
		t.Error("an unknown multi-region was accepted")
	}
}
