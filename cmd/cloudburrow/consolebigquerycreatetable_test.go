package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestCreateTableDestinationIsGooglesAndChoosesTheDataset (#1861): Create
// table's Destination section is Project, Dataset, Table, Table type, in
// Google's order, the Dataset prefilled with the one the form was opened
// on, and the table is made in the Dataset chosen there.
func TestCreateTableDestinationIsGooglesAndChoosesTheDataset(t *testing.T) {
	fields := createTableFields("p", "opened", []string{"a", "b"})
	var dest []string
	for _, f := range fields {
		if f.Section == "Destination" {
			dest = append(dest, f.Label)
		}
		switch f.Name {
		case bqTableProject:
			if f.Default != "p" || !reflect.DeepEqual(f.Options, []string{"p"}) {
				t.Errorf("Project is %+v", f)
			}
		case bqTableDataset:
			if f.Default != "opened" || !reflect.DeepEqual(f.Options, []string{"opened", "a", "b"}) {
				t.Errorf("Dataset is %+v", f)
			}
		}
	}
	if got := strings.Join(dest, "|"); got != "Project|Dataset|Table|Table type|View query" {
		t.Errorf("Destination is %q", got)
	}
	var adv []string
	for _, f := range fields {
		if f.Section == "Advanced options" {
			adv = append(adv, f.Label)
		}
	}
	if !strings.HasSuffix(strings.Join(adv, "|"), "Encryption") {
		t.Errorf("Advanced options are %q, want Encryption among them", adv)
	}

	path, err := createTableTarget("p", []string{"opened"}, "createtable", map[string]string{bqTableProject: "p", bqTableDataset: "b"})
	if err != nil || !reflect.DeepEqual(path, []string{"b"}) {
		t.Errorf("the chosen Dataset makes the table in %v, %v", path, err)
	}
	if path, _ := createTableTarget("p", []string{"opened"}, "createtable", map[string]string{}); !reflect.DeepEqual(path, []string{"opened"}) {
		t.Errorf("no Dataset makes the table in %v", path)
	}
	if _, err := createTableTarget("p", []string{"opened"}, "createtable", map[string]string{bqTableProject: "other"}); err == nil ||
		!strings.Contains(err.Error(), "chosen in the toolbar") {
		t.Errorf("another Project is answered %v", err)
	}
	if path, _ := createTableTarget("p", []string{"d", "t"}, "insertrows", map[string]string{bqTableDataset: "b"}); !reflect.DeepEqual(path, []string{"d", "t"}) {
		t.Errorf("another action's path became %v", path)
	}
	if err := refuseUnavailable(map[string]string{"encryption": bqEncryptionKMS}, fields); err == nil {
		t.Error("Create table took a Cloud KMS key the engine does not use")
	}
}

// TestBigQueryJobPageNamesUserAndBytes (#1861): a query job's details name
// its user and the bytes it processed and billed, as Google's do.
func TestBigQueryJobPageNamesUserAndBytes(t *testing.T) {
	page := jobPageOf(t, `{"jobReference":{"projectId":"p","jobId":"q1"},"user_email":"ann@example.com",
		"configuration":{"jobType":"QUERY","query":{"query":"SELECT 1"}},
		"statistics":{"creationTime":"1790000000000","startTime":"1790000000000","endTime":"1790000000200",
		"totalBytesProcessed":"2048","query":{"totalBytesProcessed":"2048","totalBytesBilled":"10485760"}},
		"status":{"state":"DONE"}}`)
	props := propertiesOf(page)
	for label, want := range map[string]string{
		"Job/User": "ann@example.com", "Job/Bytes processed": "2.00 KB", "Job/Bytes billed": "10.00 MB",
	} {
		if props[label] != want {
			t.Errorf("%s = %q, want %q", label, props[label], want)
		}
	}
	load := propertiesOf(jobPageOf(t, `{"jobReference":{"projectId":"p","jobId":"l1"},
		"configuration":{"load":{"sourceUris":["gs://b/a.csv"]}},"statistics":{"load":{"outputRows":"1"}},"status":{"state":"DONE"}}`))
	if _, ok := load["Job/Bytes billed"]; ok {
		t.Error("a load job shows bytes billed")
	}
}
