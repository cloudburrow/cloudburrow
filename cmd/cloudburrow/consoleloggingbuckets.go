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
)

type loggingBucketsProvider struct{ loggingAdminScreen }

func (loggingBucketsProvider) ID() string    { return "logging-buckets" }
func (loggingBucketsProvider) Title() string { return "Log storage" }

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

func (p loggingBucketsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Location", "Retention", "Lock", "State", "Description"}, NameColumn: "Bucket", Noun: "buckets", Note: bucketNote}
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
		out.Items = append(out.Items, console.Resource{Name: b.GetName(), Fields: map[string]string{
			"Location": loc, "Retention": fmt.Sprintf("%d days", b.GetRetentionDays()), "Lock": lockedWord(b.GetLocked()),
			"State": bucketState(b), "Description": orDash(b.GetDescription()),
		}})
	}
	out.Total = len(out.Items)
	out.RowsOpenable = true
	return out, nil
}

func (loggingBucketsProvider) CreateForm() (string, []console.Field) {
	return "Create log bucket", []console.Field{
		{Name: "name", Label: "Bucket name", Type: "text", Required: true,
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, Help: "Up to 100 letters, digits, _, - and ., starting with a letter or digit."},
		{Name: "location", Label: "Region", Type: "text", Required: true, Default: "global", Help: "global, or a region such as us-central1."},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "retention", Label: "Retention period (days)", Type: "number", Default: "30", Help: "1 to 3650 days."},
	}
}

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
	b, err := a.ConfigServer().CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: strings.TrimSpace(values["name"]),
		Bucket: &loggingpb.LogBucket{Description: values["description"], RetentionDays: days}})
	if err != nil {
		return "", err
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
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if _, err := bucketParent(ctx, project); err != nil {
		return console.Detail{Prompt: "Choose a project in the toolbar, or a parent on the Log storage screen."}, nil
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
			row := console.Resource{Name: id, Fields: map[string]string{
				"Filter": orDash(v.GetFilter()), "Description": orDash(v.GetDescription())},
				Link: viewExplorerHref(project, v.GetName())} // #1410
			if viewChangeable(b, id) {
				row.Actions = viewRowActions(v) // #1283
			}
			views.Items = append(views.Items, row)
		}
		views.Total = len(views.Items)
	}
	d := console.Detail{
		Summary: []console.Property{
			{Label: "Retention", Value: fmt.Sprintf("%d days", b.GetRetentionDays())},
			{Label: "Lock", Value: lockedWord(b.GetLocked())},
			{Label: "State", Value: bucketState(b)},
		},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Bucket", Properties: []console.Property{
					{Label: "Resource name", Value: path[0]},
					{Label: "Description", Value: orDash(b.GetDescription())},
					{Label: "Retention", Value: fmt.Sprintf("%d days", b.GetRetentionDays())},
					{Label: "Lock", Value: lockedWord(b.GetLocked())},
					{Label: "State", Value: bucketState(b)},
					{Label: "Log Analytics", Value: map[bool]string{true: "Enabled", false: "Disabled"}[b.GetAnalyticsEnabled()]},
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
			{Name: "retention", Label: "Retention period (days)", Type: "number", Default: strconv.Itoa(int(b.GetRetentionDays())), Help: "1 to 3650 days."},
			{Name: "locked", Label: "Lock the bucket (cannot be undone)", Type: "checkbox", Default: strconv.FormatBool(b.GetLocked())},
			{Name: "analytics", Label: "Log Analytics (needed to link a BigQuery dataset)", Type: "checkbox", Default: strconv.FormatBool(b.GetAnalyticsEnabled())},
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

// Actions offers Undelete on a bucket pending deletion.
func (loggingBucketsProvider) Actions(r console.Resource) []console.Action {
	if r.Fields["State"] == bucketDeleteRequested {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return nil
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
	_ console.PathActor = loggingBucketsProvider{}
	_ console.Driller   = loggingBucketsProvider{}
	_ console.Creator   = loggingBucketsProvider{}
	_ console.Editor    = loggingBucketsProvider{}
	_ console.Deleter   = loggingBucketsProvider{}
	_ console.Actor     = loggingBucketsProvider{}
)
