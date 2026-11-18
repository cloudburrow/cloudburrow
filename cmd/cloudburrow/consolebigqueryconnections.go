package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// connectionsSummary is the project's BigQuery connections in the
// instance's location (US), as the Connection API the BigQuery front
// serves lists them (#1755): each connection's ID, type and the service
// account BigQuery gave it, as the console's Explorer shows them under
// External connections.
func (p bigqueryProvider) connectionsSummary(ctx context.Context, project string) console.PropertyGroup {
	g := console.PropertyGroup{Heading: "External connections (us)"}
	u := "http://" + p.endpoint + "/v1/projects/" + url.PathEscape(project) + "/locations/us/connections?pageSize=1000"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		g.Properties = []console.Property{{Label: "Connections", Value: "unavailable: " + err.Error()}}
		return g
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.Properties = []console.Property{{Label: "Connections", Value: "unavailable: " + err.Error()}}
		return g
	}
	defer resp.Body.Close()
	var list struct {
		Connections []map[string]json.RawMessage `json:"connections"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&list) != nil {
		g.Properties = []console.Property{{Label: "Connections", Value: fmt.Sprintf("unavailable: the Connection API answered HTTP %d", resp.StatusCode)}}
		return g
	}
	if len(list.Connections) == 0 {
		g.Properties = []console.Property{{Label: "Connections", Value: "none"}}
		return g
	}
	for _, c := range list.Connections {
		var name string
		_ = json.Unmarshal(c["name"], &name)
		id := name[strings.LastIndex(name, "/")+1:]
		kind, sa := "", ""
		for _, t := range []string{"cloudResource", "cloudSql", "spark", "cloudSpanner", "aws", "azure", "salesforceDataCloud"} {
			if raw, ok := c[t]; ok {
				kind = t
				var props struct {
					ServiceAccountID string `json:"serviceAccountId"`
				}
				_ = json.Unmarshal(raw, &props)
				sa = props.ServiceAccountID
			}
		}
		v := kind
		if sa != "" {
			v += ", service account " + sa
		}
		g.Properties = append(g.Properties, console.Property{Label: id, Value: v})
	}
	return g
}
