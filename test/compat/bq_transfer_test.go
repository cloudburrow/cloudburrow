//go:build compat

package compat

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
)

// TestBqTransferConfigMkLsShow (#1948): bq mk, ls and show --transfer_config
// against the instance. bq sends its Data Transfer calls to the address
// --api names, BigQuery's, and builds the client from the discovery
// document that address answers ($discovery/rest?version=v1); CloudBurrow's
// BigQuery tunnel answers both for the Data Transfer Service. The config bq
// made is read back through the official Go client.
//
// covers: google.cloud.bigquery.datatransfer.v1.DataTransferService/CheckValidCreds, google.cloud.bigquery.datatransfer.v1.DataTransferService/GetTransferConfig
func TestBqTransferConfigMkLsShow(t *testing.T) {
	h := New(t)
	endpoint := strings.TrimSuffix(h.Endpoint(EnvBigQuery), "/")
	bq, project := bigqueryClient(t, h)
	dts, _ := dataTransferClients(t, h)
	dataset := strings.ReplaceAll(h.Project(), "-", "_") + "_bqdts"
	ds := bq.Dataset(dataset)
	if err := ds.Create(h.Context(), &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	// bq mk asks CheckValidCreds first, and prompts for an authorization
	// code on false.
	creds, err := dts.CheckValidCreds(h.Context(), &datatransferpb.CheckValidCredsRequest{
		Name: "projects/" + project + "/locations/us/dataSources/scheduled_query"})
	if err != nil || !creds.GetHasValidCreds() {
		t.Fatalf("CheckValidCreds(scheduled_query) = %v, %v; want true", creds, err)
	}
	b := newBqSession(t, nil, "--api", endpoint, "--project_id", project, bqToken)

	out := b.must("mk", "--transfer_config", "--data_source=scheduled_query", "--target_dataset="+dataset,
		"--display_name=bq nightly", "--schedule=every 24 hours",
		`--params={"query":"SELECT 1 AS n","destination_table_name_template":"bq_out","write_disposition":"WRITE_TRUNCATE"}`)
	name := regexp.MustCompile(`projects/[0-9]+/locations/[a-z0-9-]+/transferConfigs/[a-f0-9-]+`).FindString(out)
	if name == "" {
		t.Fatalf("bq mk --transfer_config printed no config name:\n%s", out)
	}
	t.Cleanup(func() {
		_ = dts.DeleteTransferConfig(h.Context(), &datatransferpb.DeleteTransferConfigRequest{Name: name})
	})
	got, err := dts.GetTransferConfig(h.Context(), &datatransferpb.GetTransferConfigRequest{Name: name})
	if err != nil {
		t.Fatalf("GetTransferConfig of the config bq made: %v", err)
	}
	if got.GetDisplayName() != "bq nightly" || got.GetSchedule() != "every 24 hours" || got.GetDestinationDatasetId() != dataset {
		t.Errorf("the config bq made reads back as %v", got)
	}

	var listed []map[string]any
	ls := b.must("ls", "--transfer_config", "--transfer_location=us", "--format=json")
	if err := json.Unmarshal([]byte(strings.TrimSpace(ls)), &listed); err != nil {
		t.Fatalf("bq ls --transfer_config printed no JSON: %v\n%s", err, ls)
	}
	found := false
	for _, c := range listed {
		found = found || c["name"] == name
	}
	if !found {
		t.Errorf("bq ls --transfer_config does not list %s:\n%s", name, ls)
	}

	var shown map[string]any
	show := b.must("show", "--format=json", "--transfer_config", name)
	if err := json.Unmarshal([]byte(strings.TrimSpace(show)), &shown); err != nil {
		t.Fatalf("bq show --transfer_config printed no JSON: %v\n%s", err, show)
	}
	if shown["name"] != name || shown["displayName"] != "bq nightly" || shown["dataSourceId"] != "scheduled_query" {
		t.Errorf("bq show --transfer_config = %v", shown)
	}
}
