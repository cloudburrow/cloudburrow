package main

// BigQuery Studio's Schedule (#2094): the query editor's Schedule > Create
// new scheduled query and Update scheduled query open Google's New
// scheduled query pane (https://docs.cloud.google.com/bigquery/docs/scheduling-queries,
// read 2026-11-23), which saves a scheduled_query transfer config of this
// instance's BigQuery Data Transfer Service (#1948). The Scheduled queries
// page reaches the pane back through its Edit. The page calls these through
// POST /api/studio/bigquery-scheduled-queries.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	dtpb "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
)

// scheduledQueryForm is the New scheduled query pane's fields.
type scheduledQueryForm struct {
	// Name is the transfer config's resource name: empty creates one.
	Name        string `json:"name,omitempty"`
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"displayName"`
	Query       string `json:"query"`
	// Schedule is the config's schedule; OnDemand turns auto scheduling
	// off (Repeats: On-demand).
	Schedule  string `json:"schedule"`
	OnDemand  bool   `json:"onDemand"`
	StartTime string `json:"startTime,omitempty"` // RFC 3339, UTC
	EndTime   string `json:"endTime,omitempty"`
	// Destination for query results.
	Dataset          string `json:"dataset,omitempty"`
	Table            string `json:"table,omitempty"`
	PartitionField   string `json:"partitionField,omitempty"`
	WriteDisposition string `json:"writeDisposition,omitempty"`
	// Advanced options.
	KMSKey         string `json:"kmsKey,omitempty"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Additional configurations.
	Email bool   `json:"email"`
	Topic string `json:"topic,omitempty"`
}

// StudioCall answers the pane: "get" reads a scheduled query into the
// form, "save" creates one (no name) or updates it.
func (p dataTransferProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if p.only != datatransfer.ScheduledQuery {
		return nil, fmt.Errorf("unknown call %q", call)
	}
	api := p.svc.API()
	if api == nil {
		return nil, errors.New(dataTransferNotStarted)
	}
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	var f scheduledQueryForm
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("a scheduled query: %v", err)
	}
	switch call {
	case "get":
		name, err := p.full(ctx, project, f.Name)
		if err != nil {
			return nil, err
		}
		c, err := api.GetTransferConfig(ctx, &dtpb.GetTransferConfigRequest{Name: name})
		if err != nil {
			return nil, errors.New(consoleErr(err))
		}
		if c.GetDataSourceId() != datatransfer.ScheduledQuery {
			return nil, fmt.Errorf("%s is not a scheduled query", c.GetDisplayName())
		}
		return scheduledQueryOf(c), nil
	case "save":
		return p.saveScheduledQuery(ctx, api, project, f)
	}
	return nil, fmt.Errorf("unknown call %q", call)
}

func scheduledQueryOf(c *dtpb.TransferConfig) scheduledQueryForm {
	ps := c.GetParams().GetFields()
	str := func(k string) string { return ps[k].GetStringValue() }
	ts := func(t *timestamppb.Timestamp) string {
		if t == nil {
			return ""
		}
		return t.AsTime().UTC().Format(time.RFC3339)
	}
	name := c.GetName()
	return scheduledQueryForm{
		Name: name, ID: name[strings.LastIndex(name, "/")+1:], DisplayName: c.GetDisplayName(), Query: str("query"),
		Schedule: c.GetSchedule(), OnDemand: c.GetScheduleOptions().GetDisableAutoScheduling(),
		StartTime: ts(c.GetScheduleOptions().GetStartTime()), EndTime: ts(c.GetScheduleOptions().GetEndTime()),
		Dataset: c.GetDestinationDatasetId(), Table: str("destination_table_name_template"),
		PartitionField: str("partitioning_field"), WriteDisposition: str("write_disposition"),
		KMSKey:         c.GetEncryptionConfiguration().GetKmsKeyName().GetValue(),
		ServiceAccount: c.GetOwnerInfo().GetEmail(),
		Email:          c.GetEmailPreferences().GetEnableFailureEmail(), Topic: c.GetNotificationPubsubTopic(),
	}
}

func (p dataTransferProvider) saveScheduledQuery(ctx context.Context, api *datatransfer.Server, project string, f scheduledQueryForm) (any, error) {
	if strings.TrimSpace(f.DisplayName) == "" {
		return nil, errors.New("Name for the scheduled query is required.")
	}
	if strings.TrimSpace(f.Query) == "" {
		return nil, errors.New("The query is empty: write one in the editor first.")
	}
	if f.Table != "" && f.Dataset == "" {
		return nil, errors.New("Dataset name is required for a destination table.")
	}
	params := map[string]any{"query": f.Query}
	if f.Table != "" {
		params["destination_table_name_template"] = f.Table
		wd := f.WriteDisposition
		if wd == "" {
			wd = "WRITE_APPEND"
		}
		params["write_disposition"] = wd
		if f.PartitionField != "" {
			params["partitioning_field"] = f.PartitionField
		}
	}
	ps, err := structpb.NewStruct(params)
	if err != nil {
		return nil, err
	}
	opts := &dtpb.ScheduleOptions{DisableAutoScheduling: f.OnDemand}
	for _, t := range []struct {
		in  string
		out **timestamppb.Timestamp
		lbl string
	}{{f.StartTime, &opts.StartTime, "Start time"}, {f.EndTime, &opts.EndTime, "Schedule end time"}} {
		if t.in == "" {
			continue
		}
		v, err := time.Parse(time.RFC3339, t.in)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.lbl, err)
		}
		*t.out = timestamppb.New(v)
	}
	if opts.StartTime != nil && opts.EndTime != nil && !opts.EndTime.AsTime().After(opts.StartTime.AsTime()) {
		return nil, errors.New("The schedule end time must be after the start time.")
	}
	c := &dtpb.TransferConfig{DisplayName: f.DisplayName, DataSourceId: datatransfer.ScheduledQuery, Params: ps,
		ScheduleOptions: opts, NotificationPubsubTopic: f.Topic,
		EmailPreferences: &dtpb.EmailPreferences{EnableFailureEmail: f.Email}}
	if !f.OnDemand {
		c.Schedule = f.Schedule
	}
	if f.Dataset != "" {
		c.Destination = &dtpb.TransferConfig_DestinationDatasetId{DestinationDatasetId: f.Dataset}
	}
	if f.KMSKey != "" {
		c.EncryptionConfiguration = &dtpb.EncryptionConfiguration{KmsKeyName: &wrapperspb.StringValue{Value: f.KMSKey}}
	}
	var got *dtpb.TransferConfig
	if f.Name == "" {
		got, err = api.CreateTransferConfig(ctx, &dtpb.CreateTransferConfigRequest{Parent: "projects/" + project,
			TransferConfig: c, ServiceAccountName: f.ServiceAccount})
	} else {
		name, ferr := p.full(ctx, project, f.Name)
		if ferr != nil {
			return nil, ferr
		}
		c.Name = name
		paths := []string{"display_name", "params", "schedule", "schedule_options", "notification_pubsub_topic", "email_preferences", "encryption_configuration"}
		if f.Dataset != "" {
			paths = append(paths, "destination_dataset_id")
		}
		got, err = api.UpdateTransferConfig(ctx, &dtpb.UpdateTransferConfigRequest{TransferConfig: c,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, ServiceAccountName: f.ServiceAccount})
	}
	if err != nil {
		return nil, errors.New(consoleErr(err))
	}
	return scheduledQueryOf(got), nil
}

// scheduledQueryEdit is a scheduled query page's Edit: the query opened in
// BigQuery Studio's editor, where Schedule > Update scheduled query saves it
// back, as Google's "Edit" on the Scheduled query details page does.
func scheduledQueryEdit(name string) console.Action {
	return console.Action{ID: "edit", Label: "Edit", Href: "/bigquery-studio?scheduledQuery=" + url.QueryEscape(name)}
}
