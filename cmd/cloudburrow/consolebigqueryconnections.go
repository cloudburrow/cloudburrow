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

// The datasets page's connection actions (#1760): Create connection (a
// Cloud resource connection in the US, the kind a remote model names),
// Edit connection (its friendly name and description, by update mask)
// and Delete connection, each one call of the Connection API the
// BigQuery front serves, as the console's Add data → Connections to
// external data sources, connection details' Edit and Delete do.
const (
	actCreateConnection = "createconnection"
	actEditConnection   = "editconnection"
	actDeleteConnection = "deleteconnection"
)

// connectionIDField names a connection; the Connection API's documented
// characters.
func connectionIDField(help string) console.Field {
	return console.Field{Name: "connection", Label: "Connection ID", Type: "text", Required: true, Pattern: `[A-Za-z0-9_\-]{1,1024}`, Help: help}
}

// connectionActions are the datasets page's connection actions.
func connectionActions() []console.Action {
	return []console.Action{
		{ID: actCreateConnection, Label: "Create connection", Fields: []console.Field{
			connectionIDField("Letters, numbers, underscores and dashes. The connection is a Vertex AI remote models, remote functions and BigLake (Cloud Resource) connection in the US, which CREATE MODEL … REMOTE WITH CONNECTION `project.us.ID` names."),
			{Name: "friendlyName", Label: "Friendly name", Type: "text"},
			{Name: "description", Label: "Description", Type: "text"},
		}},
		{ID: actEditConnection, Label: "Edit connection", Fields: []console.Field{
			connectionIDField("The ID of a connection in the US, as External connections lists it."),
			{Name: "friendlyName", Label: "Friendly name", Type: "text", Help: "Empty clears it."},
			{Name: "description", Label: "Description", Type: "text", Help: "Empty clears it."},
		}},
		{ID: actDeleteConnection, Label: "Delete connection", Destructive: true,
			Confirm: "The connection is deleted; a remote model that names it can no longer run.", Fields: []console.Field{
				connectionIDField("The ID of a connection in the US, as External connections lists it."),
			}},
	}
}

// connectionCall is one Connection API call: nil, or the API's error.
func (p bigqueryProvider) connectionCall(ctx context.Context, method, path string, query url.Values, body any) error {
	u := "http://" + p.endpoint + "/v1/projects/" + url.PathEscape(p.project) + "/locations/us/connections" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error.Message != "" {
		return fmt.Errorf("%s", e.Error.Message)
	}
	return fmt.Errorf("the Connection API answered HTTP %d", resp.StatusCode)
}

// connectionAct performs a connection action.
func (p bigqueryProvider) connectionAct(ctx context.Context, action string, values map[string]string) error {
	id := strings.TrimSpace(values["connection"])
	if id == "" {
		return fmt.Errorf("name the connection")
	}
	switch action {
	case actCreateConnection:
		body := map[string]any{"cloudResource": map[string]any{}}
		if v := strings.TrimSpace(values["friendlyName"]); v != "" {
			body["friendlyName"] = v
		}
		if v := strings.TrimSpace(values["description"]); v != "" {
			body["description"] = v
		}
		return p.connectionCall(ctx, http.MethodPost, "", url.Values{"connectionId": {id}}, body)
	case actEditConnection:
		body := map[string]any{"friendlyName": strings.TrimSpace(values["friendlyName"]), "description": strings.TrimSpace(values["description"])}
		return p.connectionCall(ctx, http.MethodPatch, "/"+url.PathEscape(id), url.Values{"updateMask": {"friendlyName,description"}}, body)
	case actDeleteConnection:
		return p.connectionCall(ctx, http.MethodDelete, "/"+url.PathEscape(id), nil, nil)
	}
	return fmt.Errorf("unknown action %q", action)
}
