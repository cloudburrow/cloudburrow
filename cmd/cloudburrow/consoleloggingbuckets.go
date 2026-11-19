package main

// Cloud Logging: log buckets (#1264), beside the metrics, sinks and
// exclusions screens. Every call goes through the ConfigServiceV2 the
// transports serve, so the console cannot accept what the API refuses.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

type loggingBucketsProvider struct{ loggingAdminScreen }

func (loggingBucketsProvider) ID() string    { return "logging-buckets" }
func (loggingBucketsProvider) Title() string { return "Logs Storage" }

const bucketNote = "Reading through a view applies its filter and the bucket's retention: entries older than the retention are not returned (#1410). " +
	"Open a view to read it in the Logs Explorer."

const bucketDeleteRequested = "Delete requested"

func bucketState(b *loggingpb.LogBucket) string {
	if b.GetLifecycleState() == loggingpb.LifecycleState_DELETE_REQUESTED {
		return bucketDeleteRequested
	}
	return "Active"
}

func lockedWord(locked bool) string {
	if locked {
		return "Locked"
	}
	return "Not locked"
}

// bucketStatus is the Logs Storage list's Status: Locked or Unlocked, as
// Google's column reads, and a bucket pending deletion says so.
func bucketStatus(b *loggingpb.LogBucket) string {
	st := "Unlocked"
	if b.GetLocked() {
		st = "Locked"
	}
	if b.GetLifecycleState() == loggingpb.LifecycleState_DELETE_REQUESTED {
		st += ", " + strings.ToLower(bucketDeleteRequested)
	}
	return st
}

// bucketColumns are Google's Logs Storage columns
// (docs.cloud.google.com/logging/docs/buckets, re-read 2026-11-19).
var bucketColumns = []string{"Description", "Retention period", "Region", "Status", "Observability Analytics available", "Linked dataset in BigQuery"}

// bucketRegions are the Select log bucket region choices: global, the
// us and eu multi-regions, and Google Cloud's regions.
func bucketRegions() []string {
	return append([]string{"global", "us", "eu"}, locations.Regions...)
}

func (p loggingBucketsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: bucketColumns, NameColumn: "Name", Noun: "log buckets", Note: bucketNote}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	out.Scope = p.bucketScope(ctx, project) // #1411
	parent, err := bucketParent(ctx, project)
	if err != nil {
		out.Prompt = "Cloud Logging lists buckets per project, folder, organization or billing account. Choose a project in the toolbar, or a parent in the picker."
		return out, nil
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_ListBuckets_FullMethodName, parent); err != nil {
		return console.Listing{}, err
	}
	resp, err := a.ConfigServer().ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: parent + "/locations/-", PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, b := range resp.GetBuckets() {
		loc := strings.Split(b.GetName(), "/")[3]
		analytics, linked := "No", "—"
		if b.GetAnalyticsEnabled() {
			analytics = "Yes"
			var ds []string
			for _, k := range p.links(ctx, b.GetName()).Items {
				ds = append(ds, k.Fields["BigQuery dataset"])
			}
			linked = orDash(strings.Join(ds, ", "))
		}
		out.Items = append(out.Items, console.Resource{Name: b.GetName(), Fields: map[string]string{
			"Region": loc, "Retention period": fmt.Sprintf("%d days", b.GetRetentionDays()), "Status": bucketStatus(b),
			"State": bucketState(b), "Description": orDash(b.GetDescription()),
			"Observability Analytics available": analytics, "Linked dataset in BigQuery": linked,
		}})
	}
	out.Total = len(out.Items)
	out.RowsOpenable = true
	return out, nil
}

// CreateForm is Google's Create log bucket: Name, Description, Upgrade to
// use Observability Analytics (with the linked BigQuery dataset), Select
// log bucket region, then Retention period (default 30 days), ending in
// Create bucket's Create (docs.cloud.google.com/logging/docs/buckets,
// re-read 2026-11-19).
func (loggingBucketsProvider) CreateForm() (string, []console.Field) {
	const details, retention = "Bucket details", "Set the retention period"
	return "Create log bucket", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Section: details,
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, Help: "Up to 100 letters, digits, _, - and ., starting with a letter or digit."},
		{Name: "description", Label: "Description", Type: "text", Section: details},
		{Name: "analytics", Label: "Upgrade to use Observability Analytics", Type: "checkbox", Default: "false", Section: details,
			Help: "Query the bucket's entries with SQL in BigQuery. Only a project's bucket can be upgraded."},
		{Name: "link", Label: "Create a new BigQuery dataset that links to this bucket", Type: "checkbox", Default: "false", Section: details,
			ShowWhen: "analytics"},
		{Name: "linkId", Label: "BigQuery dataset name", Type: "text", Required: true, Section: details, ShowWhen: "analytics&link",
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, Help: "The link's name, which names the dataset too, with hyphens as underscores."},
		{Name: "location", Label: "Select log bucket region", Type: "select", Required: true, Default: "global", Options: bucketRegions(), Section: details},
		{Name: "retention", Label: "Retention period", Type: "number", Default: "30", Section: retention, Help: "1 to 3650 days."},
	}
}

// CreateOnPage implements console.PageCreator: Google's Create log bucket
// is a page of two steps, the second Retention period.
func (loggingBucketsProvider) CreateOnPage() bool { return true }

func parseRetention(v string) (int32, error) {
	if strings.TrimSpace(v) == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("retention %q is not a number of days", v)
	}
	return int32(n), nil
}

func (p loggingBucketsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	scope, err := bucketParent(ctx, project)
	if err != nil {
		return "", err
	}
	days, err := parseRetention(values["retention"])
	if err != nil {
		return "", err
	}
	parent := scope + "/locations/" + strings.TrimSpace(values["location"])
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateBucket_FullMethodName, parent); err != nil {
		return "", err
	}
	analytics := values["analytics"] == "true"
	b, err := a.ConfigServer().CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: strings.TrimSpace(values["name"]),
		Bucket: &loggingpb.LogBucket{Description: values["description"], RetentionDays: days, AnalyticsEnabled: analytics}})
	if err != nil {
		return "", err
	}
	if analytics && values["link"] == "true" {
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateLink_FullMethodName, b.GetName()); err != nil {
			return "", err
		}
		if _, err := a.ConfigServer().CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: b.GetName(), LinkId: strings.TrimSpace(values["linkId"]),
			Link: &loggingpb.Link{}}); err != nil {
			return "", fmt.Errorf("bucket %s was created, but its BigQuery dataset was not: %w", b.GetName(), err)
		}
	}
	return b.GetName(), nil
}

func (p loggingBucketsProvider) get(ctx context.Context, project, name string) (*loggingpb.LogBucket, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	if err := bucketOfParent(ctx, project, name); err != nil {
		return nil, err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetBucket_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name})
}

func (p loggingBucketsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	if _, err := bucketParent(ctx, project); err != nil {
		return console.Detail{Prompt: "Choose a project in the toolbar, or a parent on the Log storage screen."}, nil
	}
	if len(path) == 2 {
		return p.viewDetail(ctx, project, path)
	}
	b, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	views := console.Listing{Columns: []string{"Filter", "Description"}, NameColumn: "View", Noun: "views"}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_ListViews_FullMethodName, path[0]); err != nil {
		views.Unavailable = consoleErr(err)
	} else if a, err := p.admin(); err == nil {
		resp, err := a.ConfigServer().ListViews(ctx, &loggingpb.ListViewsRequest{Parent: path[0], PageSize: 1000})
		if err != nil {
			views.Unavailable = consoleErr(err)
		}
		for _, v := range resp.GetViews() {
			id := v.GetName()[strings.LastIndex(v.GetName(), "/")+1:]
			// The view's page (#1748), which links the Logs Explorer
			// through the view (#1410) and has its Permissions tab.
			row := console.Resource{Name: id, Fields: map[string]string{
				"Filter": orDash(v.GetFilter()), "Description": orDash(v.GetDescription())},
				Opens: []string{path[0], id}}
			if viewChangeable(b, id) {
				row.Actions = viewRowActions(v) // #1283
			}
			row.Actions = append(row.Actions, p.viewPermissionsAction(ctx, v.GetName())) // #1519
			views.Items = append(views.Items, row)
		}
		views.Total = len(views.Items)
	}
	d := console.Detail{
		Summary: []console.Property{
			{Label: "Retention period", Value: fmt.Sprintf("%d days", b.GetRetentionDays())},
			{Label: "Status", Value: bucketStatus(b)},
			{Label: "State", Value: bucketState(b)},
		},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Bucket", Properties: []console.Property{
					{Label: "Resource name", Value: path[0]},
					{Label: "Description", Value: orDash(b.GetDescription())},
					{Label: "Retention period", Value: fmt.Sprintf("%d days", b.GetRetentionDays())},
					{Label: "Lock", Value: lockedWord(b.GetLocked())},
					{Label: "State", Value: bucketState(b)},
					{Label: "Observability Analytics", Value: map[bool]string{true: "Enabled", false: "Disabled"}[b.GetAnalyticsEnabled()]},
					{Label: "Created", Value: loggingTime(b.GetCreateTime())},
					{Label: "Updated", Value: loggingTime(b.GetUpdateTime())},
				}}}, Note: bucketNote},
			{ID: "views", Label: "Views", Listing: views},
			{ID: "links", Label: "Linked datasets", Listing: p.links(ctx, path[0])},
			{ID: "copies", Label: "Copy operations", Listing: p.copies(ctx, path[0])},
		},
	}
	if !strings.HasSuffix(path[0], "/buckets/_Required") && b.GetLifecycleState() == loggingpb.LifecycleState_ACTIVE {
		d.Edit = &console.EditForm{Label: "Edit bucket", Note: "A locked bucket's retention cannot change, and a lock cannot be removed.", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: b.GetDescription()},
			{Name: "retention", Label: "Retention", Type: "number", Default: strconv.Itoa(int(b.GetRetentionDays())), Help: "1 to 3650 days."},
			{Name: "locked", Label: "Lock the bucket (cannot be undone)", Type: "checkbox", Default: strconv.FormatBool(b.GetLocked())},
			{Name: "analytics", Label: "Upgrade to use Observability Analytics (needed to link a BigQuery dataset)", Type: "checkbox", Default: strconv.FormatBool(b.GetAnalyticsEnabled())},
		}}
	}
	return d, nil
}

func (p loggingBucketsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a bucket can be edited")
	}
	if _, err := p.get(ctx, project, path[0]); err != nil {
		return err
	}
	days, err := parseRetention(values["retention"])
	if err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateBucket_FullMethodName, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	mask := []string{"description", "retention_days", "locked"}
	if _, ok := values["analytics"]; ok {
		mask = append(mask, "analytics_enabled")
	}
	_, err = a.ConfigServer().UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: path[0],
		Bucket: &loggingpb.LogBucket{Description: values["description"], RetentionDays: days, Locked: values["locked"] == "true",
			AnalyticsEnabled: values["analytics"] == "true"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
	return err
}

func (p loggingBucketsProvider) Delete(ctx context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := bucketOfParent(ctx, project, name); err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_DeleteBucket_FullMethodName, name); err != nil {
		return err
	}
	_, err = a.ConfigServer().DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: name})
	return err
}

// Actions offers Google's row menu: View bucket details, and Restore
// deleted bucket (UndeleteBucket) on a bucket pending deletion. Edit bucket
// is on the bucket's page; Delete is the row menu's own.
func (loggingBucketsProvider) Actions(r console.Resource) []console.Action {
	out := []console.Action{{ID: "view", Label: "View bucket details", Href: "/logs/storage/" + url.PathEscape(r.Name)}}
	if r.Fields["State"] == bucketDeleteRequested {
		out = append(out, console.Action{ID: "undelete", Label: "Restore deleted bucket"})
	}
	return out
}

func (p loggingBucketsProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "undelete" {
		return fmt.Errorf("unknown action %q", action)
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := bucketOfParent(ctx, project, name); err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UndeleteBucket_FullMethodName, name); err != nil {
		return err
	}
	_, err = a.ConfigServer().UndeleteBucket(ctx, &loggingpb.UndeleteBucketRequest{Name: name})
	return err
}

// viewDetail is a view's page (#1748): its configuration, a link to the
// Logs Explorer reading through it (#1410), and the Permissions tab
// (PolicyOn), with Edit, Delete and the row's Permissions form among its
// actions (DetailActions).
func (p loggingBucketsProvider) viewDetail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if err := bucketOfParent(ctx, project, path[0]); err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	v, err := p.view(ctx, path[0]+"/views/"+path[1])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	explore := console.Property{Label: "Logs Explorer", Value: "Read entries through this view", Link: viewExplorerHref(project, v.GetName())}
	return console.Detail{
		Summary: []console.Property{{Label: "Filter", Value: orDash(v.GetFilter())}, explore},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "View", Properties: []console.Property{
				{Label: "Resource name", Value: v.GetName()},
				{Label: "Filter", Value: orDash(v.GetFilter())},
				{Label: "Description", Value: orDash(v.GetDescription())},
				{Label: "Created", Value: loggingTime(v.GetCreateTime())},
				{Label: "Updated", Value: loggingTime(v.GetUpdateTime())},
				explore,
			}}}}},
	}, nil
}

// viewExplorerHref is the Logs Explorer reading through a view (#1410).
func viewExplorerHref(project, view string) string {
	q := url.Values{"scope": {"view"}, "view": {view}}
	if project != "" {
		q.Set("project", project)
	}
	return "/logs?" + q.Encode()
}

// links is a bucket's links (#1292), from ListLinks.
func (p loggingBucketsProvider) links(ctx context.Context, bucket string) console.Listing {
	l := console.Listing{Columns: []string{"BigQuery dataset", "BigQuery views", "Description", "Created"}, NameColumn: "Link", Noun: "links",
		Note: linkNote}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_ListLinks_FullMethodName, bucket); err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	a, err := p.admin()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	resp, err := a.ConfigServer().ListLinks(ctx, &loggingpb.ListLinksRequest{Parent: bucket, PageSize: 1000})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	// The dataset has a view per log view of the bucket (#1435).
	var views []string
	if len(resp.GetLinks()) > 0 {
		if vr, err := a.ConfigServer().ListViews(ctx, &loggingpb.ListViewsRequest{Parent: bucket, PageSize: 1000}); err == nil {
			for _, v := range vr.GetViews() {
				views = append(views, v.GetName()[strings.LastIndex(v.GetName(), "/")+1:])
			}
			sort.Strings(views)
		}
	}
	for _, k := range resp.GetLinks() {
		id := k.GetName()[strings.LastIndex(k.GetName(), "/")+1:]
		l.Items = append(l.Items, console.Resource{Name: id, Link: logAnalyticsLink(k.GetBigqueryDataset().GetDatasetId()),
			Fields: map[string]string{
				"BigQuery dataset": k.GetBigqueryDataset().GetDatasetId(), "BigQuery views": orDash(strings.Join(views, ", ")),
				"Description": orDash(k.GetDescription()), "Created": loggingTime(k.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l
}

const linkNote = "A link makes a read-only BigQuery dataset with a view per log view of the bucket (_AllLogs and each view's filter), " +
	"kept current as entries arrive, views change, retention ages entries out and DeleteLog removes them (#1423, #1435). Open a link to query it in Log Analytics: BigQuery's query editor on the dataset."

// logAnalyticsLink is the BigQuery console page of a link's dataset
// (bigquery.googleapis.com/projects/P/datasets/D), its query editor opened
// on a query of _AllLogs (#1423), or "".
func logAnalyticsLink(datasetID string) string {
	parts := strings.Split(datasetID, "/")
	if len(parts) != 5 {
		return ""
	}
	project, dataset := parts[2], parts[4]
	q := url.Values{}
	q.Set("project", project)
	q.Set("query", fmt.Sprintf("SELECT timestamp, severity, log_name, resource.type AS resource_type, text_payload, json_payload\n"+
		"FROM `%s.%s._AllLogs`\nORDER BY timestamp DESC\nLIMIT 100", project, dataset))
	return "/bigquery/" + url.PathEscape(dataset) + "?" + q.Encode()
}

// linkActions offers Create link on an active bucket with Log Analytics
// on, and Delete link on one with links (#1292). DetailActions
// (consoleloggingviews.go) adds them to a bucket's page.
func (p loggingBucketsProvider) linkActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	b, err := p.get(ctx, project, path[0])
	if err != nil || b.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE || !b.GetAnalyticsEnabled() {
		return nil
	}
	out := []console.Action{{ID: "create-link", Label: "Create link", Fields: []console.Field{
		{Name: "linkId", Label: "Link name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`,
			Help: "Names the BigQuery dataset too, with hyphens as underscores."},
		{Name: "description", Label: "Description", Type: "text"},
	}}}
	var ids []string
	for _, k := range p.links(ctx, path[0]).Items {
		ids = append(ids, k.Name)
	}
	if len(ids) > 0 {
		out = append(out, console.Action{ID: "delete-link", Label: "Delete link", Destructive: true,
			Fields: []console.Field{{Name: "linkId", Label: "Link", Type: "select", Required: true, Options: ids, Default: ids[0]}}})
	}
	return out
}

// actOnLink creates or deletes a link through CreateLink and DeleteLink;
// ActAt (consoleloggingviews.go) sends it the link actions.
func (p loggingBucketsProvider) actOnLink(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if _, err := p.get(ctx, project, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	id := strings.TrimSpace(values["linkId"])
	switch action {
	case "create-link":
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateLink_FullMethodName, path[0]); err != nil {
			return err
		}
		_, err = a.ConfigServer().CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: path[0], LinkId: id,
			Link: &loggingpb.Link{Description: values["description"]}})
	case "delete-link":
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_DeleteLink_FullMethodName, path[0]+"/links/"+id); err != nil {
			return err
		}
		_, err = a.ConfigServer().DeleteLink(ctx, &loggingpb.DeleteLinkRequest{Name: path[0] + "/links/" + id})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

var (
	_ console.PathActor   = loggingBucketsProvider{}
	_ console.Driller     = loggingBucketsProvider{}
	_ console.Creator     = loggingBucketsProvider{}
	_ console.PageCreator = loggingBucketsProvider{}
	_ console.Editor      = loggingBucketsProvider{}
	_ console.Deleter     = loggingBucketsProvider{}
	_ console.Actor       = loggingBucketsProvider{}
)

// Google's Logs Storage names the bucket in each row's menu: Edit bucket,
// Delete bucket (#1863).
func (loggingBucketsProvider) DeleteLabel() string  { return "Delete bucket" }
func (loggingBucketsProvider) RowEditLabel() string { return "Edit bucket" }

var (
	_ console.DeleteLabeler = loggingBucketsProvider{}
	_ console.RowEditor     = loggingBucketsProvider{}
)

// Google's Create log bucket opens from a button of that name, moves
// between its steps with Next, and ends in Create bucket
// (docs.cloud.google.com/logging/docs/buckets, re-read 2026-11-19; #1863).
func (loggingBucketsProvider) CreateLayout() string { return console.CreateLayoutNext }
func (loggingBucketsProvider) CreateSubmit() string { return "Create bucket" }

var (
	_ console.CreateLayouter  = loggingBucketsProvider{}
	_ console.CreateSubmitter = loggingBucketsProvider{}
)
