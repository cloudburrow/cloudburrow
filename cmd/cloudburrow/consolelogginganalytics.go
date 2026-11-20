package main

// Log Analytics (#1863): Google's page runs SQL over the log views of
// buckets upgraded to use Log Analytics, each named in FROM as
// `PROJECT_ID.LOCATION.BUCKET_ID.LOG_VIEW_ID`
// (docs.cloud.google.com/logging/docs/analyze/query-and-view, re-read
// 2026-11-19). Here the statement runs in the instance's BigQuery, over the
// bucket's linked dataset (#1423, #1435) when it has one, which holds a
// view per log view of the bucket with Log Analytics' schema: each such
// reference is rewritten to `PROJECT_ID.DATASET.LOG_VIEW_ID`, that
// dataset's view of the log view. A bucket with no link is queried as
// Google's page queries it, without one (#2021): over a query dataset the
// Logging server makes for the query and deletes after it
// (logging.Admin.AnalyticsDataset).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

type loggingAnalyticsProvider struct {
	loggingAdminScreen
	// bq is the instance's BigQuery, where the linked datasets are.
	bq bigqueryProvider
}

func (loggingAnalyticsProvider) ID() string    { return "logging-analytics" }
func (loggingAnalyticsProvider) Title() string { return "Log Analytics" }

const analyticsNote = "Each row is a log view of a bucket upgraded to use Log Analytics. Name it in FROM as " +
	"`PROJECT_ID.LOCATION.BUCKET_ID.LOG_VIEW_ID`, as on Google's page; the query runs in this instance's BigQuery, " +
	"over the bucket's linked dataset when it has one, and otherwise over the bucket's entries directly: no link is needed."

// List is the page's Log views panel: each log view of the project's
// buckets with Log Analytics on, by the name a query gives it.
func (p loggingAnalyticsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Bucket", "Region", "Linked dataset in BigQuery"}, NameColumn: "Log view",
		Noun: "log views", Note: analyticsNote}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	resp, err := a.ConfigServer().ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	buckets := loggingBucketsProvider{p.loggingAdminScreen}
	for _, b := range resp.GetBuckets() {
		if !b.GetAnalyticsEnabled() || b.GetLifecycleState() == loggingpb.LifecycleState_DELETE_REQUESTED {
			continue
		}
		parts := strings.Split(b.GetName(), "/")
		var ds []string
		for _, k := range buckets.links(ctx, b.GetName()).Items {
			ds = append(ds, k.Fields["BigQuery dataset"])
		}
		vr, err := a.ConfigServer().ListViews(ctx, &loggingpb.ListViewsRequest{Parent: b.GetName(), PageSize: 1000})
		if err != nil {
			continue
		}
		for _, v := range vr.GetViews() {
			view := v.GetName()[strings.LastIndex(v.GetName(), "/")+1:]
			out.Items = append(out.Items, console.Resource{Name: strings.Join([]string{parts[1], parts[3], parts[5], view}, "."),
				Fields: map[string]string{"Bucket": parts[5], "Region": parts[3], "Linked dataset in BigQuery": orDash(strings.Join(ds, ", "))}})
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}

// QueryHint is the editor's contract.
func (loggingAnalyticsProvider) QueryHint() string {
	return "GoogleSQL, one SELECT (or WITH … SELECT), over log views named `PROJECT_ID.LOCATION.BUCKET_ID.LOG_VIEW_ID`. " +
		"It runs in this instance's BigQuery, with or without a linked dataset, over Log Analytics' schema " +
		"(timestamp, severity, log_name, resource, text_payload, json_payload, labels, trace, …)."
}

// logViewRef is a backquoted four-part log view name.
var logViewRef = regexp.MustCompile("`([a-z][a-z0-9-]{4,28}[a-z0-9])\\.([a-z0-9-]+)\\.([A-Za-z0-9_.-]+?)\\.([A-Za-z0-9_-]+)`")

// Query rewrites each log view the statement names to its linked dataset's
// view and runs it, read-only, in BigQuery.
func (p loggingAnalyticsProvider) Query(ctx context.Context, project string, _ []string, statement string) (console.Listing, error) {
	if project == "" {
		return console.Listing{Prompt: "Choose a project in the toolbar."}, nil
	}
	if err := readOnlyBigQuery(statement); err != nil {
		return console.Listing{}, err
	}
	bq := p.bq.in(project)
	rewritten, drops, err := p.rewrite(ctx, statement, bq.project)
	for _, d := range drops {
		defer d()
	}
	if err != nil {
		return console.Listing{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := bq.client(ctx)
	if err != nil {
		return console.Listing{}, err
	}
	defer c.Close()
	q := c.Query(rewritten)
	q.DefaultProjectID = bq.project
	it, err := q.Read(ctx)
	if err != nil {
		return console.Listing{}, err
	}
	out, err := readRows(it, nil)
	if err != nil {
		return console.Listing{}, err
	}
	// The column types, for the results' chart (#2021).
	out.ColumnTypes = map[string]string{}
	for _, f := range it.Schema {
		if f.Repeated {
			out.ColumnTypes[f.Name] = "REPEATED"
			continue
		}
		out.ColumnTypes[f.Name] = string(f.Type)
	}
	return out, nil
}

// rewrite replaces each `PROJECT.LOCATION.BUCKET.VIEW` with the bucket's
// linked dataset's view of it, or, for a bucket with no link, its query
// dataset's, made in fallback when BigQuery does not serve the bucket's
// project; drops delete the query datasets once the query has run. It
// refuses a statement that names no log view, a bucket that does not exist
// or has Log Analytics off, and a view the bucket does not have.
func (p loggingAnalyticsProvider) rewrite(ctx context.Context, statement, fallback string) (_ string, drops []func(), _ error) {
	a, err := p.admin()
	if err != nil {
		return "", nil, err
	}
	made := map[string]string{} // bucket name: project.dataset
	buckets := loggingBucketsProvider{p.loggingAdminScreen}
	var failure error
	found := false
	out := logViewRef.ReplaceAllStringFunc(statement, func(ref string) string {
		m := logViewRef.FindStringSubmatch(ref)
		found = true
		if failure != nil {
			return ref
		}
		name := fmt.Sprintf("projects/%s/locations/%s/buckets/%s", m[1], m[2], m[3])
		b, err := a.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name})
		if err != nil {
			failure = fmt.Errorf("%s: %v", strings.Trim(ref, "`"), err)
			return ref
		}
		if !b.GetAnalyticsEnabled() {
			failure = fmt.Errorf("log bucket %s is not upgraded to use Log Analytics: upgrade it in Logs Storage first", m[3])
			return ref
		}
		if _, err := a.ConfigServer().GetView(ctx, &loggingpb.GetViewRequest{Name: name + "/views/" + m[4]}); err != nil {
			failure = fmt.Errorf("log bucket %s has no log view %s", m[3], m[4])
			return ref
		}
		links := buckets.links(ctx, name)
		if links.Unavailable != "" {
			failure = fmt.Errorf("the links of log bucket %s could not be read: %s", m[3], links.Unavailable)
			return ref
		}
		for _, k := range links.Items {
			parts := strings.Split(k.Fields["BigQuery dataset"], "/")
			if len(parts) == 5 {
				return fmt.Sprintf("`%s.%s.%s`", parts[2], parts[4], m[4])
			}
		}
		at, ok := made[name]
		if !ok {
			project, dataset, drop, err := a.AnalyticsDataset(ctx, name, fallback)
			if err != nil {
				failure = fmt.Errorf("%s: %v", strings.Trim(ref, "`"), err)
				return ref
			}
			drops = append(drops, drop)
			at = project + "." + dataset
			made[name] = at
		}
		return fmt.Sprintf("`%s.%s`", at, m[4])
	})
	if failure != nil {
		return "", drops, failure
	}
	if !found {
		return "", drops, fmt.Errorf("name a log view in FROM as `PROJECT_ID.LOCATION.BUCKET_ID.LOG_VIEW_ID`, such as `%s`",
			"my-project.global._Default._AllLogs")
	}
	return out, drops, nil
}

var (
	_ console.Provider = loggingAnalyticsProvider{}
	_ console.Executor = loggingAnalyticsProvider{}
)
