package main

// Storage Transfer Service (#1957): Transfer jobs and Agent pools, as
// Google's Data Transfer pages list them, over storagetransfer.googleapis.com
// v1's REST surface. Create transfer job takes a Cloud Storage bucket or a
// POSIX file system (through an agent pool, whose agent is CloudBurrow's
// own) as source and destination; Amazon S3, Azure Blob Storage and URL
// lists are drawn where Google has them, disabled, because CloudBurrow keeps
// everything local. A job's page has its run history (transfer operations
// with their counters); a run's page has Pause, Resume and Cancel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// transferLocalOnly is why a source outside the instance cannot be chosen.
const transferLocalOnly = "Not available locally: CloudBurrow keeps everything local, so a transfer never reaches Amazon S3, " +
	"Azure Blob Storage or URLs on the internet. Copy between this instance's buckets, or from a POSIX file system."

type storageTransferProvider struct{ endpoint string }

func (storageTransferProvider) ID() string    { return "storage-transfer" }
func (storageTransferProvider) Title() string { return "Transfer jobs" }

type transferTimeOfDay struct {
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
	Seconds int `json:"seconds"`
}

type transferGCS struct {
	BucketName string `json:"bucketName"`
	Path       string `json:"path"`
}

type transferPOSIX struct {
	RootDirectory string `json:"rootDirectory"`
}

// transferJobJSON is a TransferJob as the REST API answers it.
type transferJobJSON struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ProjectID    string `json:"projectId"`
	Status       string `json:"status"`
	TransferSpec *struct {
		GCSDataSource       *transferGCS     `json:"gcsDataSource"`
		POSIXDataSource     *transferPOSIX   `json:"posixDataSource"`
		GCSDataSink         *transferGCS     `json:"gcsDataSink"`
		POSIXDataSink       *transferPOSIX   `json:"posixDataSink"`
		SourceAgentPoolName string           `json:"sourceAgentPoolName"`
		SinkAgentPoolName   string           `json:"sinkAgentPoolName"`
		ObjectConditions    *json.RawMessage `json:"objectConditions"`
		TransferOptions     *struct {
			OverwriteObjectsAlreadyExistingInSink bool   `json:"overwriteObjectsAlreadyExistingInSink"`
			DeleteObjectsUniqueInSink             bool   `json:"deleteObjectsUniqueInSink"`
			DeleteObjectsFromSourceAfterTransfer  bool   `json:"deleteObjectsFromSourceAfterTransfer"`
			OverwriteWhen                         string `json:"overwriteWhen"`
		} `json:"transferOptions"`
	} `json:"transferSpec"`
	NotificationConfig *struct {
		PubsubTopic   string   `json:"pubsubTopic"`
		EventTypes    []string `json:"eventTypes"`
		PayloadFormat string   `json:"payloadFormat"`
	} `json:"notificationConfig"`
	Schedule *struct {
		ScheduleStartDate *insightsDate      `json:"scheduleStartDate"`
		ScheduleEndDate   *insightsDate      `json:"scheduleEndDate"`
		StartTimeOfDay    *transferTimeOfDay `json:"startTimeOfDay"`
		EndTimeOfDay      *transferTimeOfDay `json:"endTimeOfDay"`
		RepeatInterval    string             `json:"repeatInterval"`
	} `json:"schedule"`
	CreationTime         string `json:"creationTime"`
	LastModificationTime string `json:"lastModificationTime"`
	LatestOperationName  string `json:"latestOperationName"`
}

func gcsText(g *transferGCS) string {
	return "gs://" + g.BucketName + "/" + strings.TrimPrefix(g.Path, "/")
}

func (j transferJobJSON) source() string {
	s := j.TransferSpec
	switch {
	case s == nil:
		return ""
	case s.GCSDataSource != nil:
		return gcsText(s.GCSDataSource)
	case s.POSIXDataSource != nil:
		return "file://" + s.POSIXDataSource.RootDirectory
	}
	return ""
}

func (j transferJobJSON) sink() string {
	s := j.TransferSpec
	switch {
	case s == nil:
		return ""
	case s.GCSDataSink != nil:
		return gcsText(s.GCSDataSink)
	case s.POSIXDataSink != nil:
		return "file://" + s.POSIXDataSink.RootDirectory
	}
	return ""
}

func (j transferJobJSON) schedule() string {
	s := j.Schedule
	if s == nil || s.ScheduleStartDate == nil {
		return "On demand"
	}
	at := ""
	if t := s.StartTimeOfDay; t != nil {
		at = fmt.Sprintf(" at %02d:%02d UTC", t.Hours, t.Minutes)
	}
	if s.ScheduleEndDate != nil && s.ScheduleEndDate.String() == s.ScheduleStartDate.String() {
		return "Once, on " + s.ScheduleStartDate.String() + at
	}
	every := "every 24h"
	if s.RepeatInterval != "" {
		if d, err := time.ParseDuration(s.RepeatInterval); err == nil {
			every = "every " + strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
		}
	}
	out := "Starting " + s.ScheduleStartDate.String() + at + ", " + every
	if s.ScheduleEndDate != nil {
		out += ", until " + s.ScheduleEndDate.String()
	}
	return out
}

// transferCounters are a TransferOperation's counters (int64s are strings
// in proto JSON).
type transferCounters struct {
	Found          json.Number `json:"objectsFoundFromSource"`
	BytesFound     json.Number `json:"bytesFoundFromSource"`
	Copied         json.Number `json:"objectsCopiedToSink"`
	BytesCopied    json.Number `json:"bytesCopiedToSink"`
	Skipped        json.Number `json:"objectsFromSourceSkippedBySync"`
	DeletedSource  json.Number `json:"objectsDeletedFromSource"`
	DeletedSink    json.Number `json:"objectsDeletedFromSink"`
	FailedFromSrc  json.Number `json:"objectsFromSourceFailed"`
	FoundOnlySink  json.Number `json:"objectsFoundOnlyFromSink"`
	BytesFailedSrc json.Number `json:"bytesFromSourceFailed"`
}

// transferOperationJSON is a google.longrunning Operation whose metadata
// is a TransferOperation.
type transferOperationJSON struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Metadata struct {
		Name            string            `json:"name"`
		Status          string            `json:"status"`
		StartTime       string            `json:"startTime"`
		EndTime         string            `json:"endTime"`
		TransferJobName string            `json:"transferJobName"`
		Counters        *transferCounters `json:"counters"`
		ErrorBreakdowns []struct {
			ErrorCode  string      `json:"errorCode"`
			ErrorCount json.Number `json:"errorCount"`
			Entries    []struct {
				URL     string   `json:"url"`
				Details []string `json:"errorDetails"`
			} `json:"errorLogEntries"`
		} `json:"errorBreakdowns"`
	} `json:"metadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o transferOperationJSON) counters() transferCounters {
	if o.Metadata.Counters == nil {
		return transferCounters{}
	}
	return *o.Metadata.Counters
}

func (p storageTransferProvider) base() string { return "http://" + p.endpoint + "/v1/" }

// filter is a list call's required JSON filter.
func transferFilter(f map[string]any) string {
	b, _ := json.Marshal(f)
	return urlpkg.QueryEscape(string(b))
}

func (p storageTransferProvider) job(ctx context.Context, project, id string) (transferJobJSON, error) {
	var j transferJobJSON
	err := storageCall(ctx, http.MethodGet, p.base()+"transferJobs/"+urlpkg.PathEscape(id)+"?projectId="+urlpkg.QueryEscape(project), nil, &j)
	return j, err
}

func (p storageTransferProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{
		Columns:      []string{"Description", "Source", "Destination", "Schedule", "Last run", "Last run status"},
		NameColumn:   "Job name",
		Noun:         "transfer jobs",
		AlwaysStatus: true,
		Note: "Storage Transfer Service jobs between this instance's buckets and local POSIX file systems. Schedules " +
			"follow the instance's clock, which Advance clock moves.",
	}
	if project == "" {
		listing.Prompt = "Transfer jobs are shown for a project. Choose one in the toolbar."
		return listing, nil
	}
	jobs, err := listPages[transferJobJSON](ctx, p.base()+"transferJobs?pageSize=100&filter="+
		transferFilter(map[string]any{"projectId": project}), "transferJobs")
	if err != nil {
		return console.Listing{}, fmt.Errorf("list transfer jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Status == "DELETED" {
			continue
		}
		last, lastStatus := "", ""
		if j.LatestOperationName != "" {
			var op transferOperationJSON
			if storageCall(ctx, http.MethodGet, p.base()+j.LatestOperationName, nil, &op) == nil {
				last, lastStatus = shortTime(op.Metadata.StartTime), op.Metadata.Status
			}
		}
		listing.Items = append(listing.Items, console.Resource{Name: strings.TrimPrefix(j.Name, "transferJobs/"), Status: j.Status,
			Fields: map[string]string{"Description": j.Description, "Source": j.source(), "Destination": j.sink(),
				"Schedule": j.schedule(), "Last run": last, "Last run status": lastStatus}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

var timeOfDayRE = regexp.MustCompile(`^\s*([01]?[0-9]|2[0-3]):([0-5][0-9])\s*$`)

func (storageTransferProvider) CreateForm() (string, []console.Field) {
	unavailable := map[string]string{"s3": transferLocalOnly, "azure": transferLocalOnly, "url": transferLocalOnly}
	return "Create transfer job", []console.Field{
		{Name: "sourceType", Label: "Source type", Type: "radio", Required: true, Section: "Get started", Default: "gcs",
			Options: []string{"gcs", "posix", "s3", "azure", "url"},
			OptionLabels: map[string]string{"gcs": "Google Cloud Storage", "posix": "POSIX file system", "s3": "Amazon S3",
				"azure": "Azure Blob Storage or Data Lake Storage Gen2", "url": "URL list"},
			UnavailableOptions: unavailable},
		{Name: "sinkType", Label: "Destination type", Type: "radio", Required: true, Section: "Get started", Default: "gcs",
			Options: []string{"gcs", "posix"}, OptionLabels: map[string]string{"gcs": "Google Cloud Storage", "posix": "POSIX file system"}},
		{Name: "sourceBucket", Label: "Bucket or folder", Type: "text", Section: "Choose a source", Browse: "storage", ShowWhen: "sourceType=gcs",
			Help: "gs://BUCKET/ or gs://BUCKET/FOLDER/: the objects under it are transferred."},
		{Name: "sourceRoot", Label: "Source directory path", Type: "text", Section: "Choose a source", ShowWhen: "sourceType=posix",
			Pattern: `^\s*(/\S*)?\s*$`, Help: "An absolute path on this machine, read by CloudBurrow's own transfer agent."},
		{Name: "sourcePool", Label: "Agent pool", Type: "text", Section: "Choose a source", ShowWhen: "sourceType=posix",
			Default: "transfer_service_default", Help: "The agent pool whose bandwidth limit the transfer keeps to (Agent pools)."},
		{Name: "sinkBucket", Label: "Bucket or folder", Type: "text", Section: "Choose a destination", Browse: "storage", ShowWhen: "sinkType=gcs"},
		{Name: "sinkRoot", Label: "Destination directory path", Type: "text", Section: "Choose a destination", ShowWhen: "sinkType=posix",
			Pattern: `^\s*(/\S*)?\s*$`, Help: "An absolute path on this machine."},
		{Name: "sinkPool", Label: "Agent pool", Type: "text", Section: "Choose a destination", ShowWhen: "sinkType=posix",
			Default: "transfer_service_default"},
		{Name: "description", Label: "Description", Type: "text", Section: "Choose when to run job"},
		{Name: "when", Label: "Run", Type: "radio", Section: "Choose when to run job", Default: "now",
			Options: []string{"now", "once", "repeat", "none"},
			OptionLabels: map[string]string{"now": "Run once, starting now", "once": "Run once, starting on",
				"repeat": "Run with custom frequency", "none": "Run on demand"}},
		{Name: "startDate", Label: "Starting on", Type: "date", Section: "Choose when to run job", ShowWhen: "when=once",
			Help: "UTC, on the instance's clock."},
		{Name: "repeatStart", Label: "Starting on", Type: "date", Section: "Choose when to run job", ShowWhen: "when=repeat"},
		{Name: "startTime", Label: "Start time (UTC)", Type: "text", Section: "Choose when to run job", Default: "00:00",
			Pattern: `^\s*(([01]?[0-9]|2[0-3]):[0-5][0-9])?\s*$`, Help: "HH:MM", ShowWhen: "when=once"},
		{Name: "repeatTime", Label: "Start time (UTC)", Type: "text", Section: "Choose when to run job", Default: "00:00",
			Pattern: `^\s*(([01]?[0-9]|2[0-3]):[0-5][0-9])?\s*$`, Help: "HH:MM", ShowWhen: "when=repeat"},
		{Name: "every", Label: "Every", Type: "text", Section: "Choose when to run job", Default: "1d", Pattern: advancePattern,
			ShowWhen: "when=repeat", Help: "Days, hours and minutes, such as 1d or 6h; at least an hour."},
		{Name: "endDate", Label: "Ending on", Type: "date", Section: "Choose when to run job", ShowWhen: "when=repeat",
			Help: "Empty: it repeats until the job is disabled."},
		{Name: "includePrefixes", Label: "Include objects based on prefix", Type: "textarea", Section: "Choose settings",
			Help: "One prefix per line, relative to the source folder."},
		{Name: "excludePrefixes", Label: "Exclude objects based on prefix", Type: "textarea", Section: "Choose settings"},
		{Name: "minAge", Label: "Only objects last modified at least this long ago", Type: "text", Section: "Choose settings",
			Pattern: advancePattern, Help: "Such as 1d or 12h; empty for none."},
		{Name: "maxAge", Label: "Only objects last modified at most this long ago", Type: "text", Section: "Choose settings",
			Pattern: advancePattern},
		{Name: "modifiedSince", Label: "Only objects last modified on or after", Type: "datetime-local", Section: "Choose settings",
			Help: "UTC."},
		{Name: "modifiedBefore", Label: "Only objects last modified before", Type: "datetime-local", Section: "Choose settings"},
		{Name: "overwrite", Label: "When to overwrite", Type: "radio", Section: "Choose settings", Default: "DIFFERENT",
			Options:      []string{"DIFFERENT", "ALWAYS", "NEVER"},
			OptionLabels: map[string]string{"DIFFERENT": "If different", "ALWAYS": "Always", "NEVER": "Never"}},
		{Name: "deleteMode", Label: "When to delete", Type: "radio", Section: "Choose settings", Default: "never",
			Options: []string{"never", "source", "sink"},
			OptionLabels: map[string]string{"never": "Never", "source": "Delete files from source after they're transferred",
				"sink": "Delete files from destination if they're not also at source"}},
		{Name: "topic", Label: "Pub/Sub topic for notifications", Type: "text", Section: "Choose settings",
			Pattern: `^\s*(projects/[^\/]+/topics/\S+)?\s*$`, Help: "projects/P/topics/T, published to on this instance's Pub/Sub."},
		{Name: "notifySuccess", Label: "Notify on success", Type: "checkbox", Section: "Choose settings", Default: "true", ShowWhen: "topic"},
		{Name: "notifyFailed", Label: "Notify on failure", Type: "checkbox", Section: "Choose settings", Default: "true", ShowWhen: "topic"},
		{Name: "notifyAborted", Label: "Notify on cancellation", Type: "checkbox", Section: "Choose settings", Default: "true", ShowWhen: "topic"},
	}
}

// CreateOnPage implements console.PageCreator.
func (storageTransferProvider) CreateOnPage() bool { return true }

// CreateTitle implements console.CreateTitler.
func (storageTransferProvider) CreateTitle() string { return "Create a transfer job" }

// clockNow is the instance's clock, which schedules follow.
func (p storageTransferProvider) clockNow(ctx context.Context) time.Time {
	var c struct {
		Now string `json:"now"`
	}
	if storageCall(ctx, http.MethodGet, "http://"+p.endpoint+"/cloudburrow/v1/clock", nil, &c) == nil {
		if t, err := time.Parse(time.RFC3339Nano, c.Now); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

func gcsLocation(raw string) (*transferGCS, error) {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "gs://")
	bucket, path, _ := strings.Cut(s, "/")
	if bucket == "" {
		return nil, errors.New("choose a bucket")
	}
	return &transferGCS{BucketName: bucket, Path: path}, nil
}

func poolName(project, raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		v = "transfer_service_default"
	}
	if strings.HasPrefix(v, "projects/") {
		return v
	}
	return "projects/" + project + "/agentPools/" + v
}

func timeOfDay(raw string) (map[string]int, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]int{"hours": 0, "minutes": 0}, nil
	}
	m := timeOfDayRE.FindStringSubmatch(raw)
	if m == nil {
		return nil, fmt.Errorf("%q is not a time such as 09:30", raw)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return map[string]int{"hours": h, "minutes": mi}, nil
}

// transferJobBody is the TransferJob the form describes.
func (p storageTransferProvider) transferJobBody(ctx context.Context, project string, values map[string]string) (map[string]any, error) {
	spec := map[string]any{}
	switch values["sourceType"] {
	case "gcs", "":
		g, err := gcsLocation(values["sourceBucket"])
		if err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}
		spec["gcsDataSource"] = g
	case "posix":
		root := strings.TrimSpace(values["sourceRoot"])
		if !strings.HasPrefix(root, "/") {
			return nil, errors.New("the source directory is an absolute path")
		}
		spec["posixDataSource"] = map[string]any{"rootDirectory": root}
		spec["sourceAgentPoolName"] = poolName(project, values["sourcePool"])
	default:
		return nil, errors.New(transferLocalOnly)
	}
	switch values["sinkType"] {
	case "gcs", "":
		g, err := gcsLocation(values["sinkBucket"])
		if err != nil {
			return nil, fmt.Errorf("destination: %w", err)
		}
		spec["gcsDataSink"] = g
	case "posix":
		root := strings.TrimSpace(values["sinkRoot"])
		if !strings.HasPrefix(root, "/") {
			return nil, errors.New("the destination directory is an absolute path")
		}
		spec["posixDataSink"] = map[string]any{"rootDirectory": root}
		spec["sinkAgentPoolName"] = poolName(project, values["sinkPool"])
	default:
		return nil, fmt.Errorf("%q is not a destination type", values["sinkType"])
	}
	cond := map[string]any{}
	if v := textLines(values["includePrefixes"]); len(v) > 0 {
		cond["includePrefixes"] = v
	}
	if v := textLines(values["excludePrefixes"]); len(v) > 0 {
		cond["excludePrefixes"] = v
	}
	for field, key := range map[string]string{"minAge": "minTimeElapsedSinceLastModification", "maxAge": "maxTimeElapsedSinceLastModification"} {
		if strings.TrimSpace(values[field]) == "" {
			continue
		}
		d, err := parseAdvance(values[field])
		if err != nil {
			return nil, err
		}
		cond[key] = strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	for field, key := range map[string]string{"modifiedSince": "lastModifiedSince", "modifiedBefore": "lastModifiedBefore"} {
		v := strings.TrimSpace(values[field])
		if v == "" {
			continue
		}
		t, err := time.Parse("2006-01-02T15:04", v)
		if err != nil {
			return nil, fmt.Errorf("%q is not a date and time", v)
		}
		cond[key] = t.UTC().Format(time.RFC3339)
	}
	if len(cond) > 0 {
		spec["objectConditions"] = cond
	}
	opts := map[string]any{}
	switch values["overwrite"] {
	case "ALWAYS":
		opts["overwriteWhen"] = "ALWAYS"
		opts["overwriteObjectsAlreadyExistingInSink"] = true
	case "NEVER":
		opts["overwriteWhen"] = "NEVER"
	default:
		opts["overwriteWhen"] = "DIFFERENT"
	}
	switch values["deleteMode"] {
	case "source":
		opts["deleteObjectsFromSourceAfterTransfer"] = true
	case "sink":
		opts["deleteObjectsUniqueInSink"] = true
	}
	spec["transferOptions"] = opts
	job := map[string]any{"projectId": project, "status": "ENABLED", "transferSpec": spec}
	if d := strings.TrimSpace(values["description"]); d != "" {
		job["description"] = d
	}
	date := func(t time.Time) map[string]int {
		return map[string]int{"year": t.Year(), "month": int(t.Month()), "day": t.Day()}
	}
	switch values["when"] {
	case "now", "":
		today := date(p.clockNow(ctx))
		job["schedule"] = map[string]any{"scheduleStartDate": today, "scheduleEndDate": today}
	case "once":
		d, err := parseInsightsDate(values["startDate"])
		if err != nil || d == nil {
			return nil, errors.New("give the date to run on")
		}
		at, err := timeOfDay(values["startTime"])
		if err != nil {
			return nil, err
		}
		job["schedule"] = map[string]any{"scheduleStartDate": d, "scheduleEndDate": d, "startTimeOfDay": at}
	case "repeat":
		d, err := parseInsightsDate(values["repeatStart"])
		if err != nil || d == nil {
			return nil, errors.New("give the date to start on")
		}
		at, err := timeOfDay(values["repeatTime"])
		if err != nil {
			return nil, err
		}
		every, err := parseAdvance(values["every"])
		if err != nil {
			return nil, err
		}
		if every < time.Hour {
			return nil, errors.New("a transfer repeats at most every hour")
		}
		s := map[string]any{"scheduleStartDate": d, "startTimeOfDay": at,
			"repeatInterval": strconv.FormatInt(int64(every/time.Second), 10) + "s"}
		if e, err := parseInsightsDate(values["endDate"]); err != nil {
			return nil, err
		} else if e != nil {
			s["scheduleEndDate"] = e
		}
		job["schedule"] = s
	case "none":
	default:
		return nil, fmt.Errorf("%q is not a schedule", values["when"])
	}
	if t := strings.TrimSpace(values["topic"]); t != "" {
		var events []string
		for field, ev := range map[string]string{"notifySuccess": "TRANSFER_OPERATION_SUCCESS",
			"notifyFailed": "TRANSFER_OPERATION_FAILED", "notifyAborted": "TRANSFER_OPERATION_ABORTED"} {
			if values[field] == "true" {
				events = append(events, ev)
			}
		}
		if len(events) == 0 {
			return nil, errors.New("choose at least one event to notify on")
		}
		job["notificationConfig"] = map[string]any{"pubsubTopic": t, "eventTypes": events, "payloadFormat": "JSON"}
	}
	return job, nil
}

func (p storageTransferProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project")
	}
	job, err := p.transferJobBody(ctx, project, values)
	if err != nil {
		return "", err
	}
	var made transferJobJSON
	if err := storageCall(ctx, http.MethodPost, p.base()+"transferJobs", job, &made); err != nil {
		return "", err
	}
	return strings.TrimPrefix(made.Name, "transferJobs/"), nil
}

func (p storageTransferProvider) operations(ctx context.Context, project, jobName string) ([]transferOperationJSON, error) {
	return listPages[transferOperationJSON](ctx, p.base()+"transferOperations?pageSize=100&filter="+
		transferFilter(map[string]any{"projectId": project, "jobNames": []string{jobName}}), "operations")
}

func (p storageTransferProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	if len(path) == 2 {
		return p.operationDetail(ctx, path)
	}
	j, err := p.job(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the transfer job: " + err.Error()}, nil
	}
	props := []console.Property{
		{Label: "Job name", Value: j.Name, Copy: j.Name},
		{Label: "Description", Value: j.Description},
		{Label: "Status", Value: j.Status},
		{Label: "Source", Value: j.source()},
		{Label: "Destination", Value: j.sink()},
		{Label: "Schedule", Value: j.schedule()},
		{Label: "Created", Value: shortTime(j.CreationTime)},
		{Label: "Last modified", Value: shortTime(j.LastModificationTime)},
	}
	var settings []console.Property
	if s := j.TransferSpec; s != nil {
		if s.SourceAgentPoolName != "" {
			settings = append(settings, console.Property{Label: "Source agent pool", Value: s.SourceAgentPoolName})
		}
		if s.SinkAgentPoolName != "" {
			settings = append(settings, console.Property{Label: "Destination agent pool", Value: s.SinkAgentPoolName})
		}
		if s.ObjectConditions != nil {
			settings = append(settings, console.Property{Label: "Object conditions", Value: string(*s.ObjectConditions)})
		}
		if o := s.TransferOptions; o != nil {
			when := o.OverwriteWhen
			if when == "" && o.OverwriteObjectsAlreadyExistingInSink {
				when = "ALWAYS"
			}
			del := "Never"
			if o.DeleteObjectsFromSourceAfterTransfer {
				del = "Delete files from source after they're transferred"
			} else if o.DeleteObjectsUniqueInSink {
				del = "Delete files from destination if they're not also at source"
			}
			settings = append(settings, console.Property{Label: "When to overwrite", Value: orDash(when)},
				console.Property{Label: "When to delete", Value: del})
		}
	}
	if n := j.NotificationConfig; n != nil {
		settings = append(settings, console.Property{Label: "Pub/Sub notifications", Value: n.PubsubTopic + " (" + strings.Join(n.EventTypes, ", ") + ")"})
	}
	runs := console.Listing{Columns: []string{"Started", "Ended", "Objects found", "Copied", "Skipped", "Deleted from source",
		"Deleted from destination", "Failed", "Bytes copied"}, NameColumn: "Operation", Noun: "runs", AlwaysStatus: true}
	ops, err := p.operations(ctx, project, j.Name)
	if err != nil {
		runs.Note = "cannot list the runs: " + err.Error()
	}
	for i := len(ops) - 1; i >= 0; i-- {
		o := ops[i]
		c := o.counters()
		runs.Items = append(runs.Items, console.Resource{Name: strings.TrimPrefix(o.Name, "transferOperations/"), Status: o.Metadata.Status,
			Opens: []string{path[0], strings.TrimPrefix(o.Name, "transferOperations/")},
			Fields: map[string]string{"Started": shortTime(o.Metadata.StartTime), "Ended": shortTime(o.Metadata.EndTime),
				"Objects found": jobCount(c.Found), "Copied": jobCount(c.Copied), "Skipped": jobCount(c.Skipped),
				"Deleted from source": jobCount(c.DeletedSource), "Deleted from destination": jobCount(c.DeletedSink),
				"Failed": jobCount(c.FailedFromSrc), "Bytes copied": jobCount(c.BytesCopied)}})
	}
	runs.Total = len(runs.Items)
	groups := []console.PropertyGroup{{Heading: "Job", Properties: props}}
	if len(settings) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Settings", Properties: settings})
	}
	return console.Detail{Summary: []console.Property{props[2], props[3], props[4]}, Sections: []console.Section{
		{ID: "runs", Label: "Run history", Listing: runs},
		{ID: "config", Label: "Configuration", Kind: console.KindProperties, Groups: groups},
	}}, nil
}

func (p storageTransferProvider) operation(ctx context.Context, id string) (transferOperationJSON, error) {
	var o transferOperationJSON
	err := storageCall(ctx, http.MethodGet, p.base()+"transferOperations/"+urlpkg.PathEscape(id), nil, &o)
	return o, err
}

func (p storageTransferProvider) operationDetail(ctx context.Context, path []string) (console.Detail, error) {
	o, err := p.operation(ctx, path[1])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the run: " + err.Error()}, nil
	}
	c := o.counters()
	props := []console.Property{
		{Label: "Operation", Value: o.Name, Copy: o.Name},
		{Label: "Status", Value: o.Metadata.Status},
		{Label: "Transfer job", Value: o.Metadata.TransferJobName},
		{Label: "Started", Value: shortTime(o.Metadata.StartTime)},
		{Label: "Ended", Value: shortTime(o.Metadata.EndTime)},
	}
	if o.Error != nil {
		props = append(props, console.Property{Label: "Error", Value: o.Error.Message})
	}
	counters := []console.Property{
		{Label: "Objects found at source", Value: jobCount(c.Found)},
		{Label: "Bytes found at source", Value: jobCount(c.BytesFound)},
		{Label: "Objects copied", Value: jobCount(c.Copied)},
		{Label: "Bytes copied", Value: jobCount(c.BytesCopied)},
		{Label: "Objects skipped (already at destination)", Value: jobCount(c.Skipped)},
		{Label: "Objects deleted from source", Value: jobCount(c.DeletedSource)},
		{Label: "Objects deleted from destination", Value: jobCount(c.DeletedSink)},
		{Label: "Objects failed", Value: jobCount(c.FailedFromSrc)},
	}
	errs := console.Listing{Columns: []string{"Count", "Object", "Details"}, NameColumn: "Error code", Noun: "errors"}
	for _, b := range o.Metadata.ErrorBreakdowns {
		if len(b.Entries) == 0 {
			errs.Items = append(errs.Items, console.Resource{Name: b.ErrorCode, Fields: map[string]string{"Count": jobCount(b.ErrorCount)}})
		}
		for _, e := range b.Entries {
			errs.Items = append(errs.Items, console.Resource{Name: b.ErrorCode, Fields: map[string]string{
				"Count": jobCount(b.ErrorCount), "Object": e.URL, "Details": strings.Join(e.Details, "; ")}})
		}
	}
	errs.Total = len(errs.Items)
	return console.Detail{Title: strings.TrimPrefix(o.Name, "transferOperations/"), Summary: props[1:3],
		Trail: []console.Crumb{{Label: "Transfer jobs"}, {Label: path[0], Path: path[:1]}},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Run", Properties: props}, {Heading: "Progress", Properties: counters}}},
			{ID: "errors", Label: "Errors", Listing: errs},
		}}, nil
}

const (
	actTransferRun     = "start-run"
	actTransferDisable = "disable"
	actTransferEnable  = "enable"
	actTransferPause   = "pause"
	actTransferResume  = "resume"
	actTransferCancel  = "cancel"
)

// Actions implements console.Actor: Start a run, and Disable or Enable.
func (storageTransferProvider) Actions(r console.Resource) []console.Action {
	switch r.Status {
	case "ENABLED":
		return []console.Action{{ID: actTransferRun, Label: "Start a run"}, {ID: actTransferDisable, Label: "Disable job"}}
	case "DISABLED":
		return []console.Action{{ID: actTransferEnable, Label: "Enable job"}}
	}
	return nil
}

func (p storageTransferProvider) Act(ctx context.Context, project, name, action string) error {
	jobURL := p.base() + "transferJobs/" + urlpkg.PathEscape(name)
	switch action {
	case actTransferRun:
		return storageCall(ctx, http.MethodPost, jobURL+":run", map[string]any{"projectId": project}, nil)
	case actTransferDisable, actTransferEnable:
		status := map[string]string{actTransferDisable: "DISABLED", actTransferEnable: "ENABLED"}[action]
		return p.setStatus(ctx, project, name, status)
	}
	return fmt.Errorf("%s is not a transfer job action", action)
}

func (p storageTransferProvider) setStatus(ctx context.Context, project, name, status string) error {
	return storageCall(ctx, http.MethodPatch, p.base()+"transferJobs/"+urlpkg.PathEscape(name), map[string]any{
		"projectId": project, "transferJob": map[string]any{"status": status}, "updateTransferJobFieldMask": "status"}, nil)
}

// DetailActions implements console.PathActor: the job's actions on its
// page, and Pause, Resume and Cancel on a run's.
func (p storageTransferProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		j, err := p.job(ctx, project, path[0])
		if err != nil {
			return nil
		}
		return p.Actions(console.Resource{Status: j.Status})
	case 2:
		o, err := p.operation(ctx, path[1])
		if err != nil {
			return nil
		}
		cancel := console.Action{ID: actTransferCancel, Label: "Cancel run", Destructive: true,
			Confirm: "Cancel this run? Objects already transferred stay at the destination."}
		switch o.Metadata.Status {
		case "IN_PROGRESS", "QUEUED":
			return []console.Action{{ID: actTransferPause, Label: "Pause run"}, cancel}
		case "PAUSED":
			return []console.Action{{ID: actTransferResume, Label: "Resume run"}, cancel}
		}
	}
	return nil
}

// ActAt implements console.PathActor.
func (p storageTransferProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	switch len(path) {
	case 0:
		_, err := p.ActAtResult(ctx, project, path, action, values)
		return err
	case 1:
		return p.Act(ctx, project, path[0], action)
	case 2:
		switch action {
		case actTransferPause, actTransferResume, actTransferCancel:
			return storageCall(ctx, http.MethodPost, p.base()+"transferOperations/"+urlpkg.PathEscape(path[1])+":"+action, map[string]any{}, nil)
		}
	}
	return fmt.Errorf("unknown action %q", action)
}

// ActAtResult implements console.ResultActor: Advance clock answers the
// clock's new time; every other action, nothing.
func (p storageTransferProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 {
		if action != actStorageJobsClock {
			return nil, fmt.Errorf("unknown action %q", action)
		}
		return advanceStorageJobsClock(ctx, p.endpoint, values)
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

// ListActions implements console.ListActor: Advance clock.
func (storageTransferProvider) ListActions(context.Context, string) []console.Action {
	return []console.Action{storageJobsClockAction()}
}

// Delete implements console.Deleter: transferJobs.delete.
func (p storageTransferProvider) Delete(ctx context.Context, project, name string) error {
	return storageCall(ctx, http.MethodDelete, p.base()+"transferJobs/"+urlpkg.PathEscape(name)+"?projectId="+urlpkg.QueryEscape(project), nil, nil)
}

var (
	_ console.PageCreator  = storageTransferProvider{}
	_ console.CreateTitler = storageTransferProvider{}
	_ console.Driller      = storageTransferProvider{}
	_ console.Actor        = storageTransferProvider{}
	_ console.ListActor    = storageTransferProvider{}
	_ console.ResultActor  = storageTransferProvider{}
	_ console.Deleter      = storageTransferProvider{}
)

// --- Agent pools --------------------------------------------------------------

type storageAgentPoolsProvider struct{ endpoint string }

func (storageAgentPoolsProvider) ID() string    { return "storage-transfer-agent-pools" }
func (storageAgentPoolsProvider) Title() string { return "Agent pools" }

type agentPoolJSON struct {
	Name           string `json:"name"`
	DisplayName    string `json:"displayName"`
	State          string `json:"state"`
	BandwidthLimit *struct {
		LimitMbps json.Number `json:"limitMbps"`
	} `json:"bandwidthLimit"`
}

func (a agentPoolJSON) limit() string {
	if a.BandwidthLimit == nil || a.BandwidthLimit.LimitMbps == "" || a.BandwidthLimit.LimitMbps == "0" {
		return "No limit"
	}
	return string(a.BandwidthLimit.LimitMbps) + " MB/s"
}

func (p storageAgentPoolsProvider) poolsURL(project string) string {
	return "http://" + p.endpoint + "/v1/projects/" + urlpkg.PathEscape(project) + "/agentPools"
}

func (p storageAgentPoolsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Display name", "Bandwidth limit"}, NameColumn: "Pool name", Noun: "agent pools",
		AlwaysStatus: true,
		Note: "Agent pools of POSIX file system transfers. CloudBurrow's own transfer agent serves every pool on this machine; " +
			"a pool's bandwidth limit paces the transfers through it."}
	if project == "" {
		listing.Prompt = "Agent pools are shown for a project. Choose one in the toolbar."
		return listing, nil
	}
	pools, err := listPages[agentPoolJSON](ctx, p.poolsURL(project)+"?pageSize=100", "agentPools")
	if err != nil {
		return console.Listing{}, fmt.Errorf("list agent pools: %w", err)
	}
	for _, a := range pools {
		listing.Items = append(listing.Items, console.Resource{Name: lastSegment(a.Name), Status: a.State,
			Fields: map[string]string{"Display name": a.DisplayName, "Bandwidth limit": a.limit()}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

const agentPoolIDPattern = `^[a-z]([a-z0-9\-._~]{3,61}[a-z0-9])?$`

func (storageAgentPoolsProvider) CreateForm() (string, []console.Field) {
	return "Create pool", []console.Field{
		{Name: "agentPoolId", Label: "Name", Type: "text", Required: true, Pattern: agentPoolIDPattern,
			Help: "Lowercase letters, digits, hyphens, periods, underscores and tildes, 5 to 63 characters, starting with a letter."},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "limitMbps", Label: "Bandwidth limit (MB/s)", Type: "number",
			Help: "Empty for no limit. Transfers through the pool keep to it."},
	}
}

func agentPoolBody(values map[string]string) (map[string]any, error) {
	body := map[string]any{"displayName": strings.TrimSpace(values["displayName"])}
	if v := strings.TrimSpace(values["limitMbps"]); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a bandwidth limit: a whole number of MB/s above 0", v)
		}
		body["bandwidthLimit"] = map[string]any{"limitMbps": strconv.FormatInt(n, 10)}
	}
	return body, nil
}

func (p storageAgentPoolsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project")
	}
	id := strings.TrimSpace(values["agentPoolId"])
	body, err := agentPoolBody(values)
	if err != nil {
		return "", err
	}
	if err := storageCall(ctx, http.MethodPost, p.poolsURL(project)+"?agentPoolId="+urlpkg.QueryEscape(id), body, nil); err != nil {
		return "", err
	}
	return id, nil
}

func (p storageAgentPoolsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	var a agentPoolJSON
	if err := storageCall(ctx, http.MethodGet, p.poolsURL(project)+"/"+urlpkg.PathEscape(path[0]), nil, &a); err != nil {
		return console.Detail{Unavailable: "cannot read the agent pool: " + err.Error()}, nil
	}
	limit := ""
	if a.BandwidthLimit != nil {
		limit = string(a.BandwidthLimit.LimitMbps)
	}
	props := []console.Property{
		{Label: "Pool name", Value: a.Name, Copy: a.Name},
		{Label: "Display name", Value: a.DisplayName},
		{Label: "Status", Value: a.State},
		{Label: "Bandwidth limit", Value: a.limit()},
		{Label: "Agents", Value: "CloudBurrow's built-in transfer agent, on this machine"},
	}
	return console.Detail{Summary: props[2:4], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Agent pool", Properties: props}}},
	}, Edit: &console.EditForm{Label: "Edit", Title: "Edit agent pool", Submit: "Save", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: a.DisplayName},
		{Name: "limitMbps", Label: "Bandwidth limit (MB/s)", Type: "number", Default: limit, Help: "Empty for no limit."},
	}}}, nil
}

// Edit implements console.Editor: agentPools.patch of the display name and
// bandwidth limit.
func (p storageAgentPoolsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only an agent pool can be edited")
	}
	body, err := agentPoolBody(values)
	if err != nil {
		return err
	}
	return storageCall(ctx, http.MethodPatch, p.poolsURL(project)+"/"+urlpkg.PathEscape(path[0])+"?updateMask="+
		urlpkg.QueryEscape("displayName,bandwidthLimit"), body, nil)
}

// Delete implements console.Deleter: agentPools.delete.
func (p storageAgentPoolsProvider) Delete(ctx context.Context, project, name string) error {
	return storageCall(ctx, http.MethodDelete, p.poolsURL(project)+"/"+urlpkg.PathEscape(name), nil, nil)
}

var (
	_ console.Creator = storageAgentPoolsProvider{}
	_ console.Driller = storageAgentPoolsProvider{}
	_ console.Editor  = storageAgentPoolsProvider{}
	_ console.Deleter = storageAgentPoolsProvider{}
)
