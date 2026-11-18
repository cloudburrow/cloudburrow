package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A BigQuery connection's page (#1770), opened from the datasets page's
// External connections (us): its type, friendly name, description,
// service account and creation and modification times, as connections.get
// answers them, and its Permissions tab, the connection's own IAM policy
// (connections.getIamPolicy, setIamPolicy and testIamPermissions), as the
// console's connection details and Share connection show them.
//
// The page's path is bigqueryConnectionsSegment, the location and the
// connection's ID. The segment has a dash, which no dataset ID can have,
// so it never names a dataset.
const bigqueryConnectionsSegment = "-connections"

// connectionLocation is the one location the console's connections are
// in: the instance's (US).
const connectionLocation = "us"

func isConnectionPath(path []string) bool {
	return len(path) == 3 && path[0] == bigqueryConnectionsSegment && path[1] != "" && path[2] != ""
}

// connectionHref is the console route of a connection's page.
func connectionHref(project, id string) string {
	return "/bigquery/" + bigqueryConnectionsSegment + "/" + connectionLocation + "/" + url.PathEscape(id) + "?project=" + url.QueryEscape(project)
}

// connectionPermissionsNote is the connection's Permissions tab's note:
// unlike other resources' policies, this one is checked (ADR-0006, as
// amended by #1765), for one permission, and for one caller.
func connectionPermissionsNote(project string) string {
	return "CloudBurrow checks one permission of this policy (ADR-0006, amended by #1765): bigquery.connections.use, " +
		"which a remote model (CREATE MODEL … REMOTE WITH CONNECTION, ML.GENERATE_TEXT, AI.GENERATE_TEXT) and " +
		"AI.GENERATE … connection_id need on the connection. It is held through an unconditioned binding, here or on " +
		"the project's policy, of a predefined role that includes it (such as roles/bigquery.connectionUser), or " +
		"roles/owner or roles/editor. The instance takes every caller to be serviceAccount:" + bigqueryCaller(project) +
		", so only a binding to that account, allUsers or allAuthenticatedUsers grants it. Every other permission " +
		"of this policy is stored and not enforced, and conditional bindings and custom roles grant nothing here."
}

// connectionJSON is one Connection API call on the connection id (verb
// "" for the connection itself, or an IAM method), decoding the answer
// into out; the error is the API's own message.
func (p bigqueryProvider) connectionJSON(ctx context.Context, project, method, id, verb string, body, out any) error {
	if project == "" {
		project = p.project
	}
	u := "http://" + p.endpoint + "/v1/projects/" + url.PathEscape(project) + "/locations/" + connectionLocation +
		"/connections/" + url.PathEscape(id)
	if verb != "" {
		u += ":" + verb
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s", e.Error.Message)
		}
		return fmt.Errorf("the Connection API answered HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// connectionTypes are the Connection API's connection kinds, by their
// field, with the names the console gives them.
var connectionTypes = []struct{ field, label string }{
	{"cloudResource", "Vertex AI remote models, remote functions and BigLake (Cloud Resource)"},
	{"cloudSql", "Cloud SQL"},
	{"spark", "Apache Spark"},
	{"cloudSpanner", "Cloud Spanner"},
	{"aws", "AWS"},
	{"azure", "Azure"},
	{"salesforceDataCloud", "Salesforce Data Cloud"},
}

// connectionMillis is a connection's creationTime or lastModifiedTime,
// milliseconds since the epoch as a string, as a time.
func connectionMillis(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n int64
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		s = strconv.FormatInt(n, 10)
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// connectionDetail is a connection's page: connections.get.
func (p bigqueryProvider) connectionDetail(ctx context.Context, project, location, id string) (console.Detail, error) {
	trail := []console.Crumb{{Label: "External connections (" + location + ")"}, {Label: id}}
	if location != connectionLocation {
		return console.Detail{Unavailable: "the console's connections are in " + connectionLocation + ", the instance's location", Trail: trail}, nil
	}
	var c map[string]json.RawMessage
	if err := p.connectionJSON(ctx, project, http.MethodGet, id, "", nil, &c); err != nil {
		return console.Detail{Unavailable: "cannot read the connection: " + err.Error(), Trail: trail}, nil
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(c[k], &s)
		return s
	}
	kind, sa := "", ""
	for _, t := range connectionTypes {
		if raw, ok := c[t.field]; ok {
			kind = t.label
			var props struct {
				ServiceAccountID string `json:"serviceAccountId"`
			}
			_ = json.Unmarshal(raw, &props)
			sa = props.ServiceAccountID
		}
	}
	props := []console.Property{
		{Label: "Connection ID", Value: projectOr(project, p.project) + "." + location + "." + id},
		{Label: "Name", Value: str("name")},
		{Label: "Location", Value: location},
		{Label: "Connection type", Value: kind},
		{Label: "Friendly name", Value: str("friendlyName")},
		{Label: "Description", Value: str("description")},
		{Label: "Service account ID", Value: sa},
		{Label: "Created", Value: connectionMillis(c["creationTime"])},
		{Label: "Last modified", Value: connectionMillis(c["lastModifiedTime"])},
	}
	if sa != "" {
		props = append(props, console.Property{Label: "Using it",
			Value: "A remote model over this connection needs its service account to hold roles/aiplatform.user on the model's project, " +
				"and its caller bigquery.connections.use on the connection (the Permissions tab)."})
	}
	return console.Detail{Trail: trail, Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Heading: "Connection info", Properties: props}}}}}, nil
}

// connectionPolicyTarget is a connection's Permissions tab.
func (p bigqueryProvider) connectionPolicyTarget() *console.PolicyTarget {
	return &console.PolicyTarget{Link: compatibilityDoc + "bigquery-is-a-community-emulator",
		RoleHelp: "A role name, such as roles/bigquery.connectionUser. The Connection API stores any role that is not empty; " +
			"only a predefined role that includes bigquery.connections.use grants it here.",
		Enforced: connectionPermissionsNote(p.project)}
}

// connectionPolicy is connections.getIamPolicy.
func (p bigqueryProvider) connectionPolicy(ctx context.Context, project, id string) (console.Policy, error) {
	var pol struct {
		Etag     string `json:"etag"`
		Bindings []struct {
			Role      string   `json:"role"`
			Members   []string `json:"members"`
			Condition *struct {
				Title, Expression string
			} `json:"condition"`
		} `json:"bindings"`
	}
	if err := p.connectionJSON(ctx, project, http.MethodPost, id, "getIamPolicy",
		map[string]any{"options": map[string]any{"requestedPolicyVersion": 3}}, &pol); err != nil {
		return console.Policy{}, err
	}
	out := console.Policy{Etag: pol.Etag, Bindings: []console.Binding{}}
	for _, b := range pol.Bindings {
		cb := console.Binding{Role: b.Role, Members: append([]string(nil), b.Members...)}
		if b.Condition != nil {
			// Shown, never written back: a conditional binding grants
			// nothing here.
			cb.Condition = b.Condition.Expression
			if b.Condition.Title != "" {
				cb.Condition = b.Condition.Title + ": " + b.Condition.Expression
			}
		}
		out.Bindings = append(out.Bindings, cb)
	}
	return out, nil
}

// setConnectionPolicy is connections.setIamPolicy with the etag the page
// read, which the API refuses when the policy has changed since.
func (p bigqueryProvider) setConnectionPolicy(ctx context.Context, project, id string, pol console.Policy) error {
	bindings := []map[string]any{}
	for _, b := range pol.Bindings {
		if b.Condition != "" || b.Conditional != nil {
			return fmt.Errorf("the binding for %s has a condition, which the console cannot write back", b.Role)
		}
		bindings = append(bindings, map[string]any{"role": b.Role, "members": b.Members})
	}
	return p.connectionJSON(ctx, project, http.MethodPost, id, "setIamPolicy",
		map[string]any{"policy": map[string]any{"version": 1, "etag": pol.Etag, "bindings": bindings}}, nil)
}

// testConnectionPermissions is connections.testIamPermissions.
func (p bigqueryProvider) testConnectionPermissions(ctx context.Context, project, id string, perms []string) ([]string, error) {
	var res struct {
		Permissions []string `json:"permissions"`
	}
	if err := p.connectionJSON(ctx, project, http.MethodPost, id, "testIamPermissions", map[string]any{"permissions": perms}, &res); err != nil {
		return nil, err
	}
	return res.Permissions, nil
}

// projectOr is project, or def when it is empty.
func projectOr(project, def string) string {
	if project == "" {
		return def
	}
	return project
}
