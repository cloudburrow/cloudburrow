package main

import (
	"context"
	"strings"
	"testing"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
)

// TestConsoleCleanRoomTemplates (#2101): the Data clean rooms page lists
// only clean rooms; Create clean room makes a dcr_exchange_config
// exchange; its Templates tab's Create Template, Edit template, Submit for
// review and Delete template act on the service, each offered in the state
// the query templates page allows it. Add data and Approve run BigQuery
// queries, which the browser test makes.
func TestConsoleCleanRoomTemplates(t *testing.T) {
	hp, api, _ := hubConsole(t)
	p := cleanRoomsProvider(hp)
	ctx := context.Background()
	row, err := p.Create(ctx, "hub-console", map[string]string{"id": "room", "displayName": "Room", "location": "us", "logEmail": "true"})
	if err != nil || row != "us/room" {
		t.Fatalf("Create clean room = %q, %v", row, err)
	}
	room := "projects/hub-console/locations/us/dataExchanges/room"
	if e, err := api.GetDataExchange(ctx, &ahpb.GetDataExchangeRequest{Name: room}); err != nil ||
		e.GetSharingEnvironmentConfig().GetDcrExchangeConfig() == nil || !e.GetLogLinkedDatasetQueryUserEmail() {
		t.Fatalf("the clean room: %v, %v", e, err)
	}
	list, err := p.List(ctx, "hub-console")
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "us/room" {
		t.Fatalf("Data clean rooms lists %v, %v; want the clean room alone", list.Items, err)
	}
	if _, err := p.Detail(ctx, "hub-console", []string{"us/ex"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "hub-console", []string{"us/ex"}); !strings.Contains(d.Unavailable, "not a clean room") {
		t.Errorf("a data exchange on the clean rooms page: %q", d.Unavailable)
	}
	path := []string{"us/room"}
	has := func(label string) bool {
		for _, a := range p.DetailActions(ctx, "hub-console", path) {
			if a.Label == label {
				return true
			}
		}
		return false
	}
	if has("Submit for review") || !has("Add data") || !has("Create Template") || !has("Subscribe") {
		t.Errorf("a clean room with no template offers the wrong actions")
	}
	if err := p.ActAt(ctx, "hub-console", path, "create-template", map[string]string{"id": "q1", "displayName": "Q one",
		"definition": "q1(y INT64) AS (SELECT y AS n)"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "hub-console", path, "create-template", map[string]string{"id": "q2", "displayName": "Q two",
		"definition": "q2() AS (SELECT 1 AS n)"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "hub-console", path, "edit-template", map[string]string{"template": "q1", "description": "edited",
		"primaryContact": "dcr@example.com"}); err != nil {
		t.Fatal(err)
	}
	q1 := room + "/queryTemplates/q1"
	if got, err := api.GetQueryTemplate(ctx, &ahpb.GetQueryTemplateRequest{Name: q1}); err != nil || got.GetDescription() != "edited" ||
		got.GetRoutine().GetDefinitionBody() != "q1(y INT64) AS (SELECT y AS n)" {
		t.Errorf("after Edit template: %v, %v", got, err)
	}
	if err := p.ActAt(ctx, "hub-console", path, "submit-template", map[string]string{"template": "q1"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "hub-console", path, "delete-template", map[string]string{"template": "q2"}); err != nil {
		t.Fatal(err)
	}
	if !has("Approve") {
		t.Errorf("a submitted template is not offered for approval")
	}
	if err := p.ActAt(ctx, "hub-console", path, "edit-template", map[string]string{"template": "q1"}); err == nil {
		t.Errorf("Edit template of a submitted template was allowed")
	}
	d, err := p.Detail(ctx, "hub-console", path)
	if err != nil {
		t.Fatal(err)
	}
	var templates string
	for _, s := range d.Sections {
		if s.ID == "templates" {
			for _, it := range s.Listing.Items {
				templates += it.Name + ":" + it.Fields["Approval Status"] + " "
			}
		}
	}
	if templates != "q1:Requires review " {
		t.Errorf("Templates tab = %q, want q1 Requires review", templates)
	}
	if err := p.Delete(ctx, "hub-console", "us/ex"); err == nil {
		t.Errorf("the clean rooms page deleted a data exchange")
	}
	if err := p.Delete(ctx, "hub-console", "us/room"); err != nil {
		t.Fatal(err)
	}
}
