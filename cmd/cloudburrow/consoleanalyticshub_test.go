package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/analyticshub"
)

// hubConsole is the Analytics Hub screen over an in-memory server with one
// exchange and one listing, no BigQuery behind it (a subscription is then
// refused, so subscriptions are not made here: the browser test makes them).
func hubConsole(t *testing.T) (analyticsHubProvider, *analyticshub.Server, *httptest.Server) {
	t.Helper()
	api, err := analyticshub.NewServer("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.OrganizationOf(func(string) string { return "organizations/42" })
	ctx := context.Background()
	if _, err := api.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/hub-console/locations/us", DataExchangeId: "ex",
		DataExchange: &ahpb.DataExchange{DisplayName: "Exchange"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: "projects/hub-console/locations/us/dataExchanges/ex", ListingId: "orders",
		Listing: &ahpb.Listing{DisplayName: "Orders", Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{
			Dataset: "projects/hub-console/datasets/shared"}}}}); err != nil {
		t.Fatal(err)
	}
	p := analyticsHubProvider{svc: &analyticsHubSvc{cfg: config.Config{}, api: api, orgOf: func(string) string { return "organizations/42" }}}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	return p, api, srv
}

func hubDetail(t *testing.T, srv *httptest.Server, path ...string) console.Detail {
	t.Helper()
	q := url.Values{"project": {"hub-console"}, "name": path}
	resp, err := http.Get(srv.URL + "/api/detail/bigquery-sharing?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d console.Detail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func hubEdit(t *testing.T, srv *httptest.Server, path []string, values map[string]string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"Path": path, "Values": values})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/resources/bigquery-sharing?project=hub-console", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestConsoleAnalyticsHubEditAndPermissions (#1989): Edit exchange and
// Edit listing send UpdateDataExchange and UpdateListing with only what
// changed, read back through the service; a listing has a page of its own;
// My subscriptions lists the organization's exchanges; and the exchange's
// and the listing's Permissions tabs keep the contract every tab keeps.
func TestConsoleAnalyticsHubEditAndPermissions(t *testing.T) {
	p, api, srv := hubConsole(t)
	ctx := context.Background()
	const ex = "projects/hub-console/locations/us/dataExchanges/ex"

	d := hubDetail(t, srv, "us/ex")
	if d.Edit == nil || d.Edit.Label != "Edit exchange" {
		t.Fatalf("the exchange's page has no Edit exchange: %+v", d.Edit)
	}
	if code, body := hubEdit(t, srv, []string{"us/ex"}, map[string]string{"displayName": "Renamed", "documentation": "# Docs",
		"public": "true", "primaryContact": "", "description": "", "logEmail": "false"}); code != http.StatusOK {
		t.Fatalf("Edit exchange = %d: %s", code, body)
	}
	e, err := api.GetDataExchange(ctx, &ahpb.GetDataExchangeRequest{Name: ex})
	if err != nil || e.GetDisplayName() != "Renamed" || e.GetDocumentation() != "# Docs" || e.GetDiscoveryType() != ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC {
		t.Fatalf("after Edit exchange: %v, %v", e, err)
	}
	if code, body := hubEdit(t, srv, []string{"us/ex"}, map[string]string{"displayName": " bad name"}); code == http.StatusOK || !strings.Contains(body, "display_name") {
		t.Errorf("Edit exchange with a name the service refuses = %d: %s", code, body)
	}

	ld := hubDetail(t, srv, "us/ex", "orders")
	if ld.Unavailable != "" || ld.Edit == nil || ld.Edit.Label != "Edit listing" {
		t.Fatalf("the listing's page: %q, %+v", ld.Unavailable, ld.Edit)
	}
	for _, f := range ld.Edit.Fields {
		if f.Name == "dataset" {
			t.Errorf("Edit listing offers the shared dataset, which cannot be changed")
		}
	}
	if code, body := hubEdit(t, srv, []string{"us/ex", "orders"}, map[string]string{"displayName": "Orders v2", "restrictQuery": "true",
		"categories": "CATEGORY_COMMERCE", "providerName": "Acme", "providerContact": "data@acme.example", "tables": "t1\nt2"}); code != http.StatusOK {
		t.Fatalf("Edit listing = %d: %s", code, body)
	}
	l, err := api.GetListing(ctx, &ahpb.GetListingRequest{Name: ex + "/listings/orders"})
	switch {
	case err != nil:
		t.Fatal(err)
	case l.GetDisplayName() != "Orders v2" || !l.GetRestrictedExportConfig().GetEnabled() || !l.GetRestrictedExportConfig().GetRestrictQueryResult():
		t.Errorf("after Edit listing: %v", l)
	case len(l.GetCategories()) != 1 || l.GetDataProvider().GetName() != "Acme" || len(l.GetBigqueryDataset().GetSelectedResources()) != 2:
		t.Errorf("after Edit listing: categories %v, provider %v, tables %v", l.GetCategories(), l.GetDataProvider(), l.GetBigqueryDataset().GetSelectedResources())
	case l.GetBigqueryDataset().GetDataset() != "projects/hub-console/datasets/shared":
		t.Errorf("Edit listing changed the shared dataset: %s", l.GetBigqueryDataset().GetDataset())
	}
	if code, body := hubEdit(t, srv, []string{"us/ex", "orders"}, map[string]string{"categories": "NOT_ONE"}); code == http.StatusOK || !strings.Contains(body, "not a listing category") {
		t.Errorf("Edit listing with an unknown category = %d: %s", code, body)
	}

	mine := hubDetail(t, srv, hubSubscriptionsRow)
	if mine.Unavailable != "" || len(mine.Sections) != 2 || mine.Sections[0].ID != "subscriptions" {
		t.Fatalf("My subscriptions: %q, %+v", mine.Unavailable, mine.Sections)
	}
	if org := mine.Sections[1].Listing; len(org.Items) != 1 || org.Items[0].Name != ex {
		t.Errorf("Organization exchanges = %+v, want %s", org.Items, ex)
	}

	for _, c := range []struct {
		path []string
		name string
	}{{[]string{"us/ex"}, ex}, {[]string{"us/ex", "orders"}, ex + "/listings/orders"}} {
		var lastStale error
		name := c.name
		exercisePermissions(t, p, permissionsCase{service: "bigquery-sharing", project: "hub-console", path: c.path,
			readBack: func(t *testing.T) *iampb.Policy {
				pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
				if err != nil {
					t.Fatal(err)
				}
				return pol
			},
			write: func(t *testing.T, add *iampb.Binding) {
				pol, _ := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
				stale := pol.GetEtag()
				pol.Bindings = append(pol.Bindings, add)
				if _, err := api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol}); err != nil {
					t.Fatal(err)
				}
				pol.Etag = stale
				_, lastStale = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol})
			},
			refusal: func() string { return grpcRefusal(lastStale) },
		})
	}
	if hasPermissionsTab(t, srv, "bigquery-sharing", "hub-console", hubSubscriptionsRow) {
		t.Error("My subscriptions has a Permissions tab")
	}
}
