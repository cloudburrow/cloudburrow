package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/service/apphub"
)

// App Hub's discovery of Compute Engine's supported types (#2153), as
// https://docs.cloud.google.com/app-hub/docs/supported-resources writes
// them (read 2026-11-22), each by project ID: forwarding rules (global and
// regional), backend services and service attachments as services, and
// instance groups (zonal and regional, managed or not) as workloads. The
// page writes only the global backend service URI; a regional backend
// service is discovered at its regional path (UNVERIFIED, re-read 2026-11-22:
// https://docs.cloud.google.com/app-hub/docs/supported-resources gives
// no regional form).
var hubComputeTypes = []struct {
	coll     string
	workload bool
}{
	{"forwardingRules", false},
	{"backendServices", false},
	{"serviceAttachments", false},
	{"instanceGroups", true},
}

// appHubComputeResources are project's discovered Compute Engine
// resources, through the Compute API's aggregated lists and the global
// collections.
func appHubComputeResources(ctx context.Context, l hubLister, base, project string) []apphub.Resource {
	if base == "" {
		return nil
	}
	p := "/compute/v1/projects/" + url.PathEscape(project) + "/"
	var out []apphub.Resource
	seen := map[string]bool{}
	add := func(self string, workload bool) {
		i := strings.Index(self, "/projects/")
		if i < 0 {
			return
		}
		rel := withProject(self[i+1:], project)
		if seen[rel] {
			return
		}
		seen[rel] = true
		loc, zone := "global", ""
		if r := nameSegment(rel, "regions"); r != "" {
			loc = r
		}
		if z := nameSegment(rel, "zones"); z != "" {
			loc, zone = hubRegionOfZone(z), z
		}
		out = append(out, apphub.Resource{URI: "//compute.googleapis.com/" + rel, Project: project, Location: loc, Zone: zone, Workload: workload})
	}
	for _, t := range hubComputeTypes {
		for _, it := range l.aggregated(ctx, base, p+"aggregated/"+t.coll, t.coll) {
			add(it.str("selfLink"), t.workload)
		}
		if t.coll == "forwardingRules" || t.coll == "backendServices" {
			for _, it := range l.list(ctx, base, p+"global/"+t.coll, "items") {
				add(it.str("selfLink"), t.workload)
			}
		}
	}
	return out
}

// aggregated GETs a Compute aggregated list, following nextPageToken, and
// returns every scope's field items.
func (l hubLister) aggregated(ctx context.Context, base, path, field string) []hubItem {
	var out []hubItem
	token := ""
	for range 100 {
		u := base + path
		if token != "" {
			u += "?pageToken=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return out
		}
		resp, err := l.client.Do(req)
		if err != nil {
			return out
		}
		var page struct {
			Items         map[string]map[string]json.RawMessage `json:"items"`
			NextPageToken string                                `json:"nextPageToken"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			return out
		}
		for _, scope := range page.Items {
			var items []hubItem
			if raw, ok := scope[field]; ok {
				_ = json.Unmarshal(raw, &items)
			}
			out = append(out, items...)
		}
		if page.NextPageToken == "" || page.NextPageToken == token {
			return out
		}
		token = page.NextPageToken
	}
	return out
}
