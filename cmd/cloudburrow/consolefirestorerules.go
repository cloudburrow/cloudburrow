package main

// A Firestore database's Rules, Usage and Query insights tabs (#2048).
//
// Rules is Google's Security page of a Standard database
// (firebase.google.com/docs/firestore/security/get-started, read
// 2026-11-20): the published rules, Edit rules then Publish, which creates
// a ruleset through the Firebase Rules API and releases it as
// cloud.firestore (cloud.firestore/<database> for a named database), and the
// Rules Playground, which simulates one request (get, list, create, update
// or delete, at a location, authenticated or not) against the rules in the
// editor through the API's projects.test. The released rules are enforced
// by the Firestore emulator on Firebase client SDK requests; server client
// libraries bypass them, as on Google.
//
// Usage charts Cloud Monitoring's document read, write and delete counts
// (docs.cloud.google.com/firestore/native/docs/monitor-usage), and Query
// insights tables each query shape's executions, errors, latency, results
// and documents scanned (…/query-insights), both from the counts the
// Firestore front keeps of the calls through it (internal/firestoreadmin,
// usage.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	firebaserules "google.golang.org/api/firebaserules/v1"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// defaultFirestoreRules are the rules Google's console starts a database's
// editor with in production mode: every client request denied.
const defaultFirestoreRules = `rules_version = '2';

service cloud.firestore {
  match /databases/{database}/documents {
    match /{document=**} {
      allow read, write: if false;
    }
  }
}
`

// rulesRelease is the release a database's rules are published as.
func rulesRelease(project, db string) string {
	if db == "(default)" {
		return "projects/" + project + "/releases/cloud.firestore"
	}
	return "projects/" + project + "/releases/cloud.firestore/" + db
}

// firebaseRulesAddr is the Firebase Rules API's address, or "" when it is
// not served.
func firebaseRulesAddr(cfg config.Config) func() string {
	return func() string {
		if !firebaseRulesEnabled(cfg) || cfg.Endpoints.FirebaseRules == 0 {
			return ""
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Endpoints.FirebaseRules))
	}
}

// rulesAPI is the Firebase Rules API, or an error when it is not served.
func (p firestoreDatabasesProvider) rulesAPI(ctx context.Context) (*firebaserules.Service, error) {
	addr := ""
	if p.rules != nil {
		addr = p.rules()
	}
	if addr == "" {
		return nil, errors.New("the Firebase Rules API is not running")
	}
	return firebaserules.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
}

// publishedRules is the source of a database's released ruleset, and the
// release, or "" when none is released.
func (p firestoreDatabasesProvider) publishedRules(ctx context.Context, api *firebaserules.Service, project, db string) (string, *firebaserules.Release, error) {
	rel, err := api.Projects.Releases.Get(rulesRelease(project, db)).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			return "", nil, nil
		}
		return "", nil, err
	}
	rs, err := api.Projects.Rulesets.Get(rel.RulesetName).Context(ctx).Do()
	if err != nil {
		return "", rel, err
	}
	var parts []string
	for _, f := range rs.Source.Files {
		parts = append(parts, f.Content)
	}
	return strings.Join(parts, "\n"), rel, nil
}

// rulesSection is the database's Rules tab.
func (p firestoreDatabasesProvider) rulesSection(ctx context.Context, project, db string) console.Section {
	sec := console.Section{ID: "rules", Label: "Rules", Kind: console.KindText}
	api, err := p.rulesAPI(ctx)
	if err != nil {
		sec.Unavailable = err.Error()
		return sec
	}
	src, rel, err := p.publishedRules(ctx, api, project, db)
	if err != nil {
		sec.Unavailable = "reading the rules: " + err.Error()
		return sec
	}
	sec.Actions = p.rulesActions(src)
	if rel == nil {
		sec.Text = defaultFirestoreRules
		sec.Note = "No rules are published for this database, so every client request is denied, as on Google. " +
			"Edit rules and Publish to release a ruleset as " + strings.TrimPrefix(rulesRelease(project, db), "projects/"+project+"/releases/") +
			". Server client libraries bypass rules, as on Google."
		return sec
	}
	sec.Text = src
	sec.Note = "Published " + rulesTime(rel.UpdateTime) + " as ruleset " + rel.RulesetName[strings.LastIndexByte(rel.RulesetName, '/')+1:] +
		". Firebase client SDK requests are checked against these rules; server client libraries bypass them, as on Google."
	return sec
}

func rulesTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

// rulesActions are the Rules tab's Edit rules and Rules Playground.
func (p firestoreDatabasesProvider) rulesActions(current string) []console.Action {
	if current == "" {
		current = defaultFirestoreRules
	}
	methods := []string{"get", "list", "create", "update", "delete"}
	return []console.Action{
		{ID: "editrules", Label: "Edit rules", Title: "Edit rules", Submit: "Publish", Fields: []console.Field{
			{Name: "source", Label: "Rules", Type: "textarea", Required: true, Default: current,
				Help: "Firestore Security Rules; Publish creates a ruleset and releases it for this database."}}},
		{ID: "playground", Label: "Rules Playground", Title: "Rules Playground", Submit: "Run", Fields: []console.Field{
			{Name: "method", Label: "Simulation type", Type: "select", Required: true, Options: methods, Default: "get",
				OptionLabels: map[string]string{"get": "get", "list": "list", "create": "create", "update": "update", "delete": "delete"}},
			{Name: "location", Label: "Location", Type: "text", Required: true, Default: "/",
				Help: "A document path under the database's documents, such as /users/alice."},
			{Name: "authenticated", Label: "Authenticated", Type: "checkbox"},
			{Name: "provider", Label: "Provider", Type: "select", Options: []string{"password", "google.com", "anonymous", "phone", "custom"},
				Default: "password", Help: "The Firebase Authentication provider of the simulated user."},
			{Name: "uid", Label: "Firebase UID", Type: "text", Help: "The simulated user's UID, when Authenticated."},
			{Name: "data", Label: "Document data", Type: "textarea",
				Help: "For create and update: the document's fields as JSON, such as {\"name\": \"Alice\"}."},
			{Name: "source", Label: "Rules", Type: "textarea", Required: true, Default: current,
				Help: "The rules to simulate against, as in the editor."},
		}},
	}
}

func rulesSource(src string) *firebaserules.Source {
	return &firebaserules.Source{Files: []*firebaserules.File{{Name: "firestore.rules", Content: src}}}
}

// publishRules creates a ruleset of src and releases it for the database.
func (p firestoreDatabasesProvider) publishRules(ctx context.Context, project, db, src string) error {
	api, err := p.rulesAPI(ctx)
	if err != nil {
		return err
	}
	rs, err := api.Projects.Rulesets.Create("projects/"+project, &firebaserules.Ruleset{Source: rulesSource(src)}).Context(ctx).Do()
	if err != nil {
		return err
	}
	name := rulesRelease(project, db)
	if _, err := api.Projects.Releases.Get(name).Context(ctx).Do(); err == nil {
		_, err = api.Projects.Releases.Patch(name, &firebaserules.UpdateReleaseRequest{
			Release: &firebaserules.Release{Name: name, RulesetName: rs.Name}}).Context(ctx).Do()
		return err
	} else if !isNotFound(err) {
		return err
	}
	_, err = api.Projects.Releases.Create("projects/"+project, &firebaserules.Release{Name: name, RulesetName: rs.Name}).Context(ctx).Do()
	return err
}

// simulateRules runs the Rules Playground's one request.
func (p firestoreDatabasesProvider) simulateRules(ctx context.Context, project, db string, v map[string]string) (*console.Listing, error) {
	api, err := p.rulesAPI(ctx)
	if err != nil {
		return nil, err
	}
	method := v["method"]
	switch method {
	case "get", "list", "create", "update", "delete":
	default:
		return nil, errors.New("choose a simulation type")
	}
	loc := "/" + strings.Trim(strings.TrimSpace(v["location"]), "/")
	req := map[string]any{"method": method, "path": "/databases/" + db + "/documents" + strings.TrimSuffix(loc, "/"),
		"time": time.Now().UTC().Format(time.RFC3339Nano)}
	if v["authenticated"] == "true" || v["authenticated"] == "on" {
		uid := strings.TrimSpace(v["uid"])
		if uid == "" {
			return nil, errors.New("an authenticated simulation needs a Firebase UID")
		}
		provider := v["provider"]
		if provider == "" {
			provider = "password"
		}
		req["auth"] = map[string]any{"uid": uid, "token": map[string]any{"sub": uid, "firebase": map[string]any{"sign_in_provider": provider}}}
	}
	if d := strings.TrimSpace(v["data"]); d != "" && (method == "create" || method == "update") {
		var data map[string]any
		if err := json.Unmarshal([]byte(d), &data); err != nil {
			return nil, fmt.Errorf("document data is not a JSON object: %v", err)
		}
		req["resource"] = map[string]any{"data": data}
	}
	res, err := api.Projects.Test("projects/"+project, &firebaserules.TestRulesetRequest{Source: rulesSource(v["source"]),
		TestSuite: &firebaserules.TestSuite{TestCases: []*firebaserules.TestCase{{Request: req, Expectation: "ALLOW"}}}}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &console.Listing{Columns: []string{"Message"}, NameColumn: "Line", Noun: "issues"}
	for _, is := range res.Issues {
		line := ""
		if is.SourcePosition != nil {
			line = strconv.FormatInt(is.SourcePosition.Line, 10)
		}
		out.Items = append(out.Items, console.Resource{Name: line, Fields: map[string]string{"Message": is.Severity + ": " + is.Description}})
	}
	if len(res.TestResults) == 0 {
		out.Text = "The rules did not compile, so the request was not simulated."
		return out, nil
	}
	tr := res.TestResults[0]
	verdict := "allowed"
	if tr.State != "SUCCESS" {
		verdict = "denied"
	}
	out.Text = "Simulated " + method + " " + verdict + "."
	for _, m := range tr.DebugMessages {
		if m = strings.TrimSpace(m); m != "" {
			out.Text += "\n" + m
		}
	}
	return out, nil
}

// usageSection is the database's Usage tab: its document reads, writes and
// deletes in each of the last 60 minutes.
func (p firestoreDatabasesProvider) usageSection(ctx context.Context, project, db string) console.Section {
	sec := console.Section{ID: "usage", Label: "Usage", Kind: console.KindChart}
	now := time.Now().UTC().Truncate(time.Minute)
	since := now.Add(-59 * time.Minute)
	minutes, err := firestoreUsage(ctx, p.endpoint, since)
	if err != nil {
		sec.Unavailable = "reading the usage: " + err.Error()
		return sec
	}
	name := p.dbName(project, db)
	byMinute := map[time.Time]*firestoreadmin.DatabaseUsage{}
	for _, m := range minutes {
		if d := m.Databases[name]; d != nil {
			byMinute[m.Start] = d
		}
	}
	series := []console.ChartSeries{{Label: "Reads", Unit: "documents"}, {Label: "Writes", Unit: "documents"}, {Label: "Deletes", Unit: "documents"}}
	var reads, writes, deletes int64
	for t := since; !t.After(now); t = t.Add(time.Minute) {
		var r, w, d float64
		if u := byMinute[t]; u != nil {
			for _, n := range u.Reads {
				r += float64(n)
			}
			for _, n := range u.Writes {
				w += float64(n)
			}
			d = float64(u.Deletes)
		}
		reads, writes, deletes = reads+int64(r), writes+int64(w), deletes+int64(d)
		at := t.Format(time.RFC3339)
		series[0].Points = append(series[0].Points, console.ChartPoint{At: at, Value: &r})
		series[1].Points = append(series[1].Points, console.ChartPoint{At: at, Value: &w})
		series[2].Points = append(series[2].Points, console.ChartPoint{At: at, Value: &d})
	}
	sec.Series = series
	sec.Note = fmt.Sprintf("Last 60 minutes: %d reads, %d writes, %d deletes. Successful document operations by minute, "+
		"as Cloud Monitoring's Firestore metrics document/read_ops_count, write_ops_count and delete_ops_count report them. "+
		"Snapshot listeners and Write streams count as Google bills them.", reads, writes, deletes)
	return sec
}

// insightsSection is the database's Query insights tab: each query shape's
// statistics over the last 24 hours.
func (p firestoreDatabasesProvider) insightsSection(ctx context.Context, project, db string) console.Section {
	cols := []string{"Method", "Execution count", "Error count", "Avg. execution duration (ms)", "Avg. results returned", "Avg. documents scanned"}
	sec := console.Section{ID: "insights", Label: "Query insights",
		Listing: console.Listing{Columns: cols, NameColumn: "Normalized query", Noun: "queries"}}
	minutes, err := firestoreUsage(ctx, p.endpoint, time.Now().Add(-24*time.Hour))
	if err != nil {
		sec.Listing.Unavailable = "reading the query statistics: " + err.Error()
		return sec
	}
	name := p.dbName(project, db)
	total := map[string]*firestoreadmin.QueryStat{}
	for _, m := range minutes {
		d := m.Databases[name]
		if d == nil {
			continue
		}
		for k, q := range d.Queries {
			t := total[k]
			if t == nil {
				t = &firestoreadmin.QueryStat{Shape: q.Shape, Method: q.Method}
				total[k] = t
			}
			t.Executions += q.Executions
			t.Errors += q.Errors
			t.DurationNs += q.DurationNs
			t.Results += q.Results
			t.DocumentsScanned += q.DocumentsScanned
		}
	}
	keys := make([]string, 0, len(total))
	for k := range total {
		keys = append(keys, k)
	}
	// Ranked by load, total execution time, as Google's table is.
	sort.Slice(keys, func(i, j int) bool {
		if total[keys[i]].DurationNs != total[keys[j]].DurationNs {
			return total[keys[i]].DurationNs > total[keys[j]].DurationNs
		}
		return keys[i] < keys[j]
	})
	avg := func(n, d int64) string { return strconv.FormatFloat(float64(n)/float64(d), 'f', 1, 64) }
	for _, k := range keys {
		q := total[k]
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: q.Shape, Fields: map[string]string{
			"Method": q.Method, "Execution count": strconv.FormatInt(q.Executions, 10), "Error count": strconv.FormatInt(q.Errors, 10),
			"Avg. execution duration (ms)": strconv.FormatFloat(float64(q.DurationNs)/float64(q.Executions)/1e6, 'f', 2, 64),
			"Avg. results returned":        avg(q.Results, q.Executions), "Avg. documents scanned": avg(q.DocumentsScanned, q.Executions)}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	sec.Note = "Queries run in the last 24 hours (runQuery, runAggregationQuery, listDocuments, listCollectionIds and " +
		"partitionQuery), their values replaced by ?, ranked by total execution time. A Standard edition query is served " +
		"from an index, so each document it scans is a result it returns."
	return sec
}
