package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dtpb "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
)

// dataTransferProvider is BigQuery's Data transfers page, and with only
// set to scheduled_query its Scheduled queries page (#1948), in Google's
// layout: the list of transfer configs, each config's run history and
// configuration, Create transfer, Run transfer now, Disable and Enable,
// Delete. Every change is a call on the service's own methods.
type dataTransferProvider struct {
	svc  *dataTransferSvc
	only string
}

const dataTransferNotStarted = "the BigQuery Data Transfer Service has not started"

func (p dataTransferProvider) ID() string {
	if p.only == datatransfer.ScheduledQuery {
		return "bigquery-scheduled-queries"
	}
	return "bigquery-transfers"
}

func (p dataTransferProvider) Title() string {
	if p.only == datatransfer.ScheduledQuery {
		return "Scheduled queries"
	}
	return "Data transfers"
}

var dataTransferColumns = []string{"Display name", "Source", "Schedule (UTC)", "Region", "Destination dataset", "Next scheduled", "Last run"}

func dtSourceName(id string) string {
	switch id {
	case datatransfer.ScheduledQuery:
		return "Scheduled Query"
	case datatransfer.CrossRegionCopy:
		return "Dataset Copy"
	}
	return id
}

func dtTime(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "—"
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

func dtState(c *dtpb.TransferConfig) string {
	if c.GetDisabled() {
		return "DISABLED"
	}
	if c.GetState() == dtpb.TransferState_TRANSFER_STATE_UNSPECIFIED {
		return "ENABLED"
	}
	return c.GetState().String()
}

func (p dataTransferProvider) configs(ctx context.Context, project string) ([]*dtpb.TransferConfig, error) {
	api := p.svc.API()
	if api == nil {
		return nil, errors.New(dataTransferNotStarted)
	}
	req := &dtpb.ListTransferConfigsRequest{Parent: "projects/" + project, PageSize: 1000}
	if p.only != "" {
		req.DataSourceIds = []string{p.only}
	}
	resp, err := api.ListTransferConfigs(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.GetTransferConfigs(), nil
}

func (p dataTransferProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: dataTransferColumns, NameColumn: "Transfer config ID", Noun: "transfers", AlwaysStatus: true}
	if project == "" {
		listing.Prompt = "Transfers are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	cfgs, err := p.configs(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range cfgs {
		region := strings.Split(c.GetName(), "/")[3]
		// A row is named by the config's ID, as Google's console names a
		// transfer in its address; the full name is on its Configuration tab.
		id := c.GetName()[strings.LastIndex(c.GetName(), "/")+1:]
		listing.Items = append(listing.Items, console.Resource{Name: id, Status: dtState(c), Fields: map[string]string{
			"Display name": orDash(c.GetDisplayName()), "Source": dtSourceName(c.GetDataSourceId()),
			"Schedule (UTC)": orDash(c.GetSchedule()), "Region": region,
			"Destination dataset": orDash(c.GetDestinationDatasetId()),
			"Next scheduled":      dtTime(c.GetNextRunTime()), "Last run": dtLastRun(c),
		}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func dtLastRun(c *dtpb.TransferConfig) string {
	if c.GetState() == dtpb.TransferState_TRANSFER_STATE_UNSPECIFIED {
		return "—"
	}
	return c.GetState().String()
}

// full is the full name of the project's transfer a row or page names,
// by its config ID (a list row) or its full name.
func (p dataTransferProvider) full(ctx context.Context, project, name string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	api := p.svc.API()
	if api == nil {
		return "", errors.New(dataTransferNotStarted)
	}
	if !strings.Contains(name, "/") {
		c, err := api.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: "projects/" + project + "/transferConfigs/" + name})
		if err != nil {
			return "", fmt.Errorf("cannot read the transfer: %s", consoleErr(err))
		}
		name = c.GetName()
	}
	if !strings.HasPrefix(name, "projects/"+datatransfer.ProjectNumber(project)+"/locations/") || !strings.Contains(name, "/transferConfigs/") {
		return "", fmt.Errorf("%s is not a transfer of project %s", name, project)
	}
	return name, nil
}

func (p dataTransferProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	api := p.svc.API()
	if api == nil {
		return console.Detail{Unavailable: dataTransferNotStarted}, nil
	}
	name, err := p.full(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := api.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the transfer: " + consoleErr(err)}, nil
	}
	runs, err := api.ListTransferRuns(ctx, &dtpb.ListTransferRunsRequest{Parent: c.GetName(), PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the runs: " + consoleErr(err)}, nil
	}
	history := console.Listing{Columns: []string{"Run time", "Status", "Start time", "End time", "Message"}, Noun: "runs", AlwaysStatus: true}
	for _, r := range runs.GetTransferRuns() {
		msg := "—"
		if r.GetErrorStatus() != nil {
			msg = r.GetErrorStatus().GetMessage()
		} else if logs, err := api.ListTransferLogs(ctx, &dtpb.ListTransferLogsRequest{Parent: r.GetName()}); err == nil && len(logs.GetTransferMessages()) > 0 {
			msg = logs.GetTransferMessages()[len(logs.GetTransferMessages())-1].GetMessageText()
		}
		history.Items = append(history.Items, console.Resource{Name: r.GetName(), Status: r.GetState().String(), Fields: map[string]string{
			"Run time": dtTime(r.GetRunTime()), "Status": r.GetState().String(), "Start time": dtTime(r.GetStartTime()),
			"End time": dtTime(r.GetEndTime()), "Message": msg}})
	}
	history.Total = len(history.Items)
	var params []console.Property
	for k, v := range c.GetParams().AsMap() {
		params = append(params, console.Property{Label: k, Value: fmt.Sprint(v)})
	}
	sort.Slice(params, func(i, j int) bool { return params[i].Label < params[j].Label })
	notify := "—"
	if c.GetNotificationPubsubTopic() != "" {
		notify = c.GetNotificationPubsubTopic()
	}
	config := console.Section{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Transfer details", Properties: []console.Property{
				{Label: "Resource name", Value: c.GetName()},
				{Label: "Display name", Value: orDash(c.GetDisplayName())},
				{Label: "Source", Value: dtSourceName(c.GetDataSourceId())},
				{Label: "Schedule (UTC)", Value: orDash(c.GetSchedule())},
				{Label: "Start date and run time", Value: dtTime(c.GetScheduleOptions().GetStartTime())},
				{Label: "End date", Value: dtTime(c.GetScheduleOptions().GetEndTime())},
				{Label: "Destination dataset", Value: orDash(c.GetDestinationDatasetId())},
				{Label: "Region", Value: orDash(c.GetDatasetRegion())},
				{Label: "Pub/Sub notifications", Value: notify},
				{Label: "Email notifications", Value: fmt.Sprint(c.GetEmailPreferences().GetEnableFailureEmail())},
				{Label: "Updated", Value: dtTime(c.GetUpdateTime())},
			}},
			{Heading: "Data source details", Properties: params},
		}}
	if emails := dtEmails(api, c.GetName()); len(emails) > 0 {
		config.Note = "Failure emails are recorded here and never sent: " + strings.Join(emails, "; ")
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: dtState(c)},
			{Label: "Source", Value: dtSourceName(c.GetDataSourceId())},
			{Label: "Schedule (UTC)", Value: orDash(c.GetSchedule())},
			{Label: "Next scheduled", Value: dtTime(c.GetNextRunTime())},
		},
		Sections: []console.Section{
			{ID: "runs", Label: "Run history", Listing: history},
			config,
		},
	}, nil
}

func dtEmails(api *datatransfer.Server, config string) []string {
	var out []string
	for _, e := range api.Emails() {
		if e.Config == config {
			out = append(out, e.Time.UTC().Format(time.RFC3339)+" "+e.Subject)
		}
	}
	return out
}

func dtActions(state string) []console.Action {
	run := console.Action{ID: "run", Label: "Run transfer now"}
	if state == "DISABLED" {
		return []console.Action{run, {ID: "enable", Label: "Enable transfer"}}
	}
	return []console.Action{run, {ID: "disable", Label: "Disable transfer"}}
}

// dtDetailActions adds a transfer page's Schedule backfill and Delete run.
func dtDetailActions(state string, runs []*dtpb.TransferRun) []console.Action {
	acts := append(dtActions(state), console.Action{ID: "backfill", Label: "Schedule backfill", Fields: []console.Field{
		{Name: "start", Label: "Start date", Type: "text", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`, Help: "YYYY-MM-DD, UTC; included."},
		{Name: "end", Label: "End date", Type: "text", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`, Help: "YYYY-MM-DD, UTC; excluded."},
	}})
	if len(runs) > 0 {
		var ids []string
		labels := map[string]string{}
		for _, r := range runs {
			ids = append(ids, r.GetName())
			labels[r.GetName()] = dtTime(r.GetRunTime()) + " (" + r.GetState().String() + ")"
		}
		acts = append(acts, console.Action{ID: "delete-run", Label: "Delete run", Destructive: true,
			Fields: []console.Field{{Name: "run", Label: "Run", Type: "select", Required: true, Options: ids, OptionLabels: labels}}})
	}
	return acts
}

func (dataTransferProvider) Actions(r console.Resource) []console.Action { return dtActions(r.Status) }

func (p dataTransferProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	api := p.svc.API()
	if api == nil || len(path) != 1 {
		return nil
	}
	name, err := p.full(ctx, project, path[0])
	if err != nil {
		return nil
	}
	c, err := api.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: name})
	if err != nil {
		return nil
	}
	runs, _ := api.ListTransferRuns(ctx, &dtpb.ListTransferRunsRequest{Parent: c.GetName(), PageSize: 1000})
	acts := dtDetailActions(dtState(c), runs.GetTransferRuns())
	if p.only == datatransfer.ScheduledQuery {
		acts = append([]console.Action{scheduledQueryEdit(c.GetName())}, acts...)
	}
	return acts
}

func (p dataTransferProvider) Act(ctx context.Context, project, name, action string) error {
	api := p.svc.API()
	if api == nil {
		return errors.New(dataTransferNotStarted)
	}
	name, err := p.full(ctx, project, name)
	if err != nil {
		return err
	}
	switch action {
	case "run":
		_, err = api.StartManualTransferRuns(ctx, &dtpb.StartManualTransferRunsRequest{Parent: name,
			Time: &dtpb.StartManualTransferRunsRequest_RequestedRunTime{RequestedRunTime: timestamppb.New(api.Now())}})
	case "enable", "disable":
		_, err = api.UpdateTransferConfig(ctx, &dtpb.UpdateTransferConfigRequest{
			TransferConfig: &dtpb.TransferConfig{Name: name, Disabled: action == "disable"},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

func (p dataTransferProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("transfer actions apply to a transfer")
	}
	api := p.svc.API()
	switch action {
	case "backfill":
		if api == nil {
			return errors.New(dataTransferNotStarted)
		}
		name, err := p.full(ctx, project, path[0])
		if err != nil {
			return err
		}
		start, err1 := time.Parse("2006-01-02", strings.TrimSpace(v["start"]))
		end, err2 := time.Parse("2006-01-02", strings.TrimSpace(v["end"]))
		if err1 != nil || err2 != nil {
			return errors.New("the start and end dates must be YYYY-MM-DD")
		}
		_, err = api.StartManualTransferRuns(ctx, &dtpb.StartManualTransferRunsRequest{Parent: name,
			Time: &dtpb.StartManualTransferRunsRequest_RequestedTimeRange{RequestedTimeRange: &dtpb.StartManualTransferRunsRequest_TimeRange{
				StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}}})
		return err
	case "delete-run":
		if api == nil {
			return errors.New(dataTransferNotStarted)
		}
		name, err := p.full(ctx, project, path[0])
		if err != nil {
			return err
		}
		if !strings.HasPrefix(v["run"], name+"/runs/") {
			return fmt.Errorf("%s is not a run of this transfer", v["run"])
		}
		_, err = api.DeleteTransferRun(ctx, &dtpb.DeleteTransferRunRequest{Name: v["run"]})
		return err
	}
	return p.Act(ctx, project, path[0], action)
}

func (p dataTransferProvider) Delete(ctx context.Context, project, name string) error {
	api := p.svc.API()
	if api == nil {
		return errors.New(dataTransferNotStarted)
	}
	name, err := p.full(ctx, project, name)
	if err != nil {
		return err
	}
	_, err = api.DeleteTransferConfig(ctx, &dtpb.DeleteTransferConfigRequest{Name: name})
	return err
}

func (p dataTransferProvider) CreateForm() (string, []console.Field) {
	src := []console.Field{{Name: "source", Label: "Source type", Type: "select", Required: true,
		Options:      []string{datatransfer.ScheduledQuery, datatransfer.CrossRegionCopy},
		OptionLabels: map[string]string{datatransfer.ScheduledQuery: "Scheduled Query", datatransfer.CrossRegionCopy: "Dataset Copy"},
		Default:      datatransfer.ScheduledQuery}}
	title := "Create transfer"
	if p.only == datatransfer.ScheduledQuery {
		title = "Create scheduled query"
		src = nil
	}
	q := "source=" + datatransfer.ScheduledQuery
	cp := "source=" + datatransfer.CrossRegionCopy
	if p.only != "" {
		q, cp = "", "never=1"
	}
	fields := append(src,
		console.Field{Name: "display_name", Label: "Transfer config name", Type: "text", Required: true},
		console.Field{Name: "schedule", Label: "Repeat frequency (custom schedule)", Type: "text", Default: "every 24 hours",
			Help: `UTC, in BigQuery's schedule syntax: "every 24 hours", "every mon 09:03", "1st sunday of month 00:00".`},
		console.Field{Name: "dataset", Label: "Destination dataset", Type: "text", Required: true},
		console.Field{Name: "query", Label: "Query", Type: "textarea", ShowWhen: q,
			Help: "GoogleSQL; @run_time (TIMESTAMP) and @run_date (DATE) are the run's."},
		console.Field{Name: "table", Label: "Destination table", Type: "text", ShowWhen: q,
			Help: `{run_date} and {run_time|"%Y%m%d"} are templated.`},
		console.Field{Name: "write_disposition", Label: "Destination table write preference", Type: "radio", ShowWhen: q,
			Options: []string{"WRITE_APPEND", "WRITE_TRUNCATE"}, OptionLabels: map[string]string{"WRITE_APPEND": "Append to table", "WRITE_TRUNCATE": "Overwrite table"},
			Default: "WRITE_APPEND"},
		console.Field{Name: "source_project", Label: "Source project", Type: "text", ShowWhen: cp},
		console.Field{Name: "source_dataset", Label: "Source dataset", Type: "text", ShowWhen: cp},
		console.Field{Name: "overwrite", Label: "Overwrite destination table", Type: "checkbox", ShowWhen: cp},
		console.Field{Name: "topic", Label: "Pub/Sub topic", Type: "text", Help: "projects/PROJECT/topics/TOPIC; published to this instance's Pub/Sub."},
		console.Field{Name: "email", Label: "Send email notifications", Type: "checkbox", Help: "Failure emails are recorded on the transfer's page, never sent."},
	)
	return title, fields
}

func (p dataTransferProvider) CreateOnPage() bool { return true }

func (p dataTransferProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	api := p.svc.API()
	if api == nil {
		return "", errors.New(dataTransferNotStarted)
	}
	if project == "" {
		return "", errors.New("choose a project first")
	}
	source := v["source"]
	if p.only != "" || source == "" {
		source = datatransfer.ScheduledQuery
	}
	params := map[string]any{}
	if source == datatransfer.ScheduledQuery {
		params["query"] = v["query"]
		if v["table"] != "" {
			params["destination_table_name_template"] = v["table"]
		}
		if v["write_disposition"] != "" {
			params["write_disposition"] = v["write_disposition"]
		}
	} else {
		params["source_project_id"] = v["source_project"]
		params["source_dataset_id"] = v["source_dataset"]
		params["overwrite_destination_table"] = fmt.Sprint(v["overwrite"] == "true" || v["overwrite"] == "on")
	}
	ps, err := structpb.NewStruct(params)
	if err != nil {
		return "", err
	}
	c := &dtpb.TransferConfig{DisplayName: v["display_name"], DataSourceId: source, Schedule: v["schedule"], Params: ps,
		NotificationPubsubTopic: v["topic"],
		Destination:             &dtpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: v["dataset"]}}
	if v["email"] == "true" || v["email"] == "on" {
		c.EmailPreferences = &dtpb.EmailPreferences{EnableFailureEmail: true}
	}
	got, err := api.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/" + project, TransferConfig: c})
	if err != nil {
		return "", err
	}
	return got.GetName(), nil
}
