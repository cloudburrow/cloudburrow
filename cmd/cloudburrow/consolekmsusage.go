package main

// A key's Usage tracking tab (#2053), as Google's
// (https://docs.cloud.google.com/kms/docs/view-key-usage, read 2026-11-20):
// the key usage summary (protected resources, projects and unique Google
// Cloud products using the key), then the protected resources with Name,
// Project, Crypto key version, Cloud product, Resource type, Location,
// Creation date and Labels, with the table's Filter box. Both come from
// the Cloud KMS Inventory API this instance serves
// (GetProtectedResourcesSummary and SearchProtectedResources), so the tab
// shows what the products really protect with the key. A key whose project
// is in no organization is searched in its project, and the tab says the
// data is limited to it, as Google's notice does.

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/kms/inventory/apiv1/inventorypb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

var kmsUsageColumns = []string{"Project", "Crypto key version", "Cloud product", "Resource type", "Location", "Creation date", "Labels"}

func (p kmsProvider) usageTracking(ctx context.Context, key string) console.Section {
	sec := console.Section{ID: "usage", Label: "Usage tracking"}
	list := console.Listing{Columns: kmsUsageColumns, NameColumn: "Name", Noun: "protected resources",
		Note: "No resource is protected by this key through CMEK in the scope searched."}
	inv := p.svc.Inventory()
	if inv == nil {
		list.Unavailable = kmsNotStarted
		sec.Listing = list
		return sec
	}
	s, err := inv.GetProtectedResourcesSummary(ctx, &inventorypb.GetProtectedResourcesSummaryRequest{Name: key})
	if err != nil {
		list.Unavailable = "cannot read the key usage summary: " + consoleErr(err)
		sec.Listing = list
		return sec
	}
	list.Summary = []console.PropertyGroup{{Heading: "Key usage summary", Properties: []console.Property{
		{Label: "Protected resources", Value: strconv.FormatInt(s.GetResourceCount(), 10)},
		{Label: "Projects", Value: strconv.Itoa(int(s.GetProjectCount()))},
		{Label: "Google Cloud products", Value: strconv.Itoa(len(s.GetCloudProducts()))},
	}}}
	scope := "organizations/"
	project := strings.SplitN(key, "/", 3)[1]
	limited := false
	for _, w := range s.GetWarnings() {
		if w.GetWarningCode() == inventorypb.Warning_ORG_LESS_PROJECT_PARTIAL_DATA {
			limited = true
		}
	}
	if limited {
		scope = "projects/" + project
		sec.Note = "Data is limited to the selected project: project " + project + " is in no organization, " +
			"so resources in other projects that use this key are not shown."
	} else if org := p.svc.organizationOf(project); org != "" {
		scope += org
	} else {
		scope = "projects/" + project
	}
	token := ""
	for {
		resp, err := inv.SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: scope, CryptoKey: key, PageToken: token})
		if err != nil {
			list.Unavailable = "cannot search the protected resources: " + consoleErr(err)
			break
		}
		for _, r := range resp.GetProtectedResources() {
			labels := make([]string, 0, len(r.GetLabels()))
			for k, v := range r.GetLabels() {
				labels = append(labels, k+": "+v)
			}
			sort.Strings(labels)
			created := "—"
			if r.GetCreateTime() != nil {
				created = r.GetCreateTime().AsTime().UTC().Format(time.RFC3339)
			}
			list.Items = append(list.Items, console.Resource{Name: r.GetName(), Fields: map[string]string{
				"Project": r.GetProjectId(), "Crypto key version": orDash(r.GetCryptoKeyVersion()),
				"Cloud product": r.GetCloudProduct(), "Resource type": r.GetResourceType(), "Location": r.GetLocation(),
				"Creation date": created, "Labels": orDash(strings.Join(labels, ", "))}})
		}
		if token = resp.GetNextPageToken(); token == "" || len(list.Items) >= detailLimit {
			break
		}
	}
	list.Total = len(list.Items)
	sec.Listing = list
	return sec
}
