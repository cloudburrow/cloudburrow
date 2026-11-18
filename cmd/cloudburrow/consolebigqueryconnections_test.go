package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBigQueryConnectionsSummary (#1755): the datasets list names the
// project's connections, their types and service accounts.
func TestBigQueryConnectionsSummary(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us/connections" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"connections":[{"name":"projects/1/locations/us/connections/ai","cloudResource":{"serviceAccountId":"bqcx-1-ab@gcp-sa-bigquery-condel.iam.gserviceaccount.com"}}]}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	g := p.connectionsSummary(context.Background(), "p")
	if len(g.Properties) != 1 || g.Properties[0].Label != "ai" || !strings.Contains(g.Properties[0].Value, "cloudResource, service account bqcx-1-ab@") {
		t.Errorf("summary %+v", g)
	}
	if g := p.connectionsSummary(context.Background(), "other"); len(g.Properties) != 1 || !strings.HasPrefix(g.Properties[0].Value, "unavailable") {
		t.Errorf("a failed list %+v", g)
	}
}

// TestBigQueryConnectionActions (#1760): the datasets page creates, edits
// and deletes a connection, each one Connection API call, and shows the
// API's own error.
func TestBigQueryConnectionActions(t *testing.T) {
	var got []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":404,"message":"Not found: Connection missing"}}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ids := map[string]bool{}
	for _, a := range connectionActions() {
		ids[a.ID] = true
	}
	if !ids[actCreateConnection] || !ids[actEditConnection] || !ids[actDeleteConnection] {
		t.Fatalf("actions %v", ids)
	}
	ctx := context.Background()
	if err := p.connectionAct(ctx, actCreateConnection, map[string]string{"connection": "ai", "friendlyName": "AI"}); err != nil {
		t.Fatal(err)
	}
	if err := p.connectionAct(ctx, actEditConnection, map[string]string{"connection": "ai", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	if err := p.connectionAct(ctx, actDeleteConnection, map[string]string{"connection": "ai"}); err != nil {
		t.Fatal(err)
	}
	if err := p.connectionAct(ctx, actDeleteConnection, map[string]string{"connection": "missing"}); err == nil || !strings.Contains(err.Error(), "Not found: Connection missing") {
		t.Errorf("a missing connection: %v", err)
	}
	if err := p.connectionAct(ctx, actDeleteConnection, map[string]string{}); err == nil {
		t.Error("no ID was accepted")
	}
	want := []string{
		`POST /v1/projects/p/locations/us/connections?connectionId=ai {"cloudResource":{},"friendlyName":"AI"}`,
		`PATCH /v1/projects/p/locations/us/connections/ai?updateMask=friendlyName%2Cdescription {"description":"d","friendlyName":""}`,
		`DELETE /v1/projects/p/locations/us/connections/ai? `,
		`DELETE /v1/projects/p/locations/us/connections/missing? `,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(got, "\n"))
	}
}

// TestBigQueryConnectionPage (#1770): a connection's page reads
// connections.get, and its Permissions tab is the connection's own IAM
// policy, read, written with the etag read, and tested, with a note that
// says which permission the instance checks and for which caller.
func TestBigQueryConnectionPage(t *testing.T) {
	var got []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		switch r.URL.Path {
		case "/v1/projects/p/locations/us/connections/ai":
			fmt.Fprint(w, `{"name":"projects/1/locations/us/connections/ai","friendlyName":"AI","description":"for models",`+
				`"creationTime":"1700000000000","lastModifiedTime":"1700000001000",`+
				`"cloudResource":{"serviceAccountId":"bqcx-1-ab@gcp-sa-bigquery-condel.iam.gserviceaccount.com"}}`)
		case "/v1/projects/p/locations/us/connections/ai:getIamPolicy":
			fmt.Fprint(w, `{"version":1,"etag":"BwE=","bindings":[{"role":"roles/bigquery.connectionUser","members":["allUsers"]}]}`)
		case "/v1/projects/p/locations/us/connections/ai:setIamPolicy":
			if !strings.Contains(string(b), `"etag":"BwE="`) {
				w.WriteHeader(409)
				fmt.Fprint(w, `{"error":{"code":409,"message":"stale etag"}}`)
				return
			}
			fmt.Fprint(w, `{}`)
		case "/v1/projects/p/locations/us/connections/ai:testIamPermissions":
			fmt.Fprint(w, `{"permissions":["bigquery.connections.use"]}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":404,"message":"Not found: Connection projects/1/locations/us/connections/missing"}}`)
		}
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()
	path := []string{bigqueryConnectionsSegment, "us", "ai"}
	d, err := p.Detail(ctx, "p", path)
	if err != nil || d.Unavailable != "" || len(d.Sections) != 1 {
		t.Fatalf("detail %+v %v", d, err)
	}
	vals := map[string]string{}
	for _, pr := range d.Sections[0].Groups[0].Properties {
		vals[pr.Label] = pr.Value
	}
	for k, want := range map[string]string{"Friendly name": "AI", "Description": "for models", "Created": "2023-11-14T22:13:20Z",
		"Last modified": "2023-11-14T22:13:21Z", "Service account ID": "bqcx-1-ab@gcp-sa-bigquery-condel.iam.gserviceaccount.com"} {
		if vals[k] != want {
			t.Errorf("%s = %q, want %q", k, vals[k], want)
		}
	}
	if !strings.Contains(vals["Connection type"], "Cloud Resource") {
		t.Errorf("type %q", vals["Connection type"])
	}
	if d, _ := p.Detail(ctx, "p", []string{bigqueryConnectionsSegment, "us", "missing"}); !strings.Contains(d.Unavailable, "Not found: Connection") {
		t.Errorf("a missing connection's page %+v", d)
	}
	target := p.PolicyOn(ctx, path)
	if target == nil || !strings.Contains(target.Enforced, "bigquery.connections.use") || !strings.Contains(target.Enforced, "cloudburrow-local@p.iam.gserviceaccount.com") {
		t.Fatalf("policy target %+v", target)
	}
	pol, err := p.GetPolicy(ctx, "p", path)
	if err != nil || pol.Etag != "BwE=" || len(pol.Bindings) != 1 || pol.Bindings[0].Members[0] != "allUsers" {
		t.Fatalf("policy %+v %v", pol, err)
	}
	if err := p.SetPolicy(ctx, "p", path, pol); err != nil {
		t.Errorf("set: %v", err)
	}
	pol.Etag = "old"
	if err := p.SetPolicy(ctx, "p", path, pol); err == nil || !strings.Contains(err.Error(), "stale etag") {
		t.Errorf("a stale etag: %v", err)
	}
	if perms, err := p.TestPermissions(ctx, "p", path, []string{"bigquery.connections.use"}); err != nil || len(perms) != 1 {
		t.Errorf("test %v %v", perms, err)
	}
	// A dataset can be named "connections"; the page's segment has a dash
	// no dataset ID can have.
	if isConnectionPath([]string{"connections", "us", "ai"}) || p.PolicyOn(ctx, []string{"d", "x", "y"}) != nil {
		t.Errorf("a dataset path taken for a connection's")
	}
	if g := p.connectionsSummary(ctx, "p"); len(got) == 0 || g.Properties == nil {
		t.Errorf("summary %+v", g)
	}
}
