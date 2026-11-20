package main

// The Cloud Storage companion APIs' screens (#1957), each a view over the
// API's own REST surface on its host port, as an application calls it:
//
//   - Cloud Storage > Batch operations: Storage Batch Operations jobs
//     (storagebatchoperations.googleapis.com), with Create job, each job's
//     page (its bucket operations and error summaries), Cancel and Delete;
//   - Cloud Storage > Inventory reports: Storage Insights report
//     configurations (storageinsights.googleapis.com), with Create, Edit,
//     Delete and each configuration's report history
//     (consolestoragejobsinsights.go);
//   - Storage Transfer Service > Transfer jobs and Agent pools
//     (storagetransfer.googleapis.com), with Create transfer job, Start a
//     run, Pause, Resume and Cancel a run, Disable, Enable and Delete
//     (consolestoragejobstransfer.go).
//
// The three APIs share one clock, CloudBurrow's own: schedules (transfer
// runs, inventory reports) follow it, and Advance clock (CloudBurrow
// extension), on the Transfer jobs and Inventory reports lists, moves it
// forward with POST /cloudburrow/v1/clock:advance, by the UI parity rule.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// companionAddr is the loopback host address an in-process API bound on
// port is dialled at, or "" for a port not bound (negative) or not known
// (0, OS-assigned: the caller passes the live address instead).
func companionAddr(cfg config.Config, port int) string {
	if port <= 0 {
		return ""
	}
	host := cfg.BindAddress
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// storageJobsProviders are the companion APIs' screens for the endpoints
// that are served: batch, insights and transfer are host addresses (empty
// for one not served), storage the Cloud Storage tunnel's, for the buckets
// the forms and the inventory reports' locations read.
func storageJobsProviders(storage, batch, insights, transfer string) []console.Provider {
	var out []console.Provider
	if batch != "" {
		out = append(out, storageBatchProvider{endpoint: batch})
	}
	if insights != "" {
		out = append(out, storageInventoryProvider{endpoint: insights, storage: storage})
	}
	if transfer != "" {
		out = append(out, storageTransferProvider{endpoint: transfer}, storageAgentPoolsProvider{endpoint: transfer})
	}
	return out
}

// --- the shared clock ------------------------------------------------------

const actStorageJobsClock = "advance-storage-clock"

const storageJobsClockConfirm = "This is a CloudBurrow test extension, not a Google API: it moves the clock of Storage " +
	"Transfer Service, Storage Insights and Storage Batch Operations forward, and it can never be moved back. The clock " +
	"is the whole instance's: every transfer job's schedule and every inventory report configuration of every project " +
	"sees the time move, and each scheduled run or report it passes starts or is written at once."

// storageJobsClockAction is the list screens' Advance clock.
func storageJobsClockAction() console.Action {
	return console.Action{ID: actStorageJobsClock, Label: "Advance clock (CloudBurrow extension)", Confirm: storageJobsClockConfirm,
		Fields: []console.Field{{Name: "duration", Label: "Advance by", Type: "text", Required: true, Default: "1d",
			Pattern: advancePattern,
			Help: "Days, hours, minutes and seconds, such as 1d (the next daily run or report) or 7d. A CloudBurrow test " +
				"extension: Google's services have no such call (POST /cloudburrow/v1/clock:advance)."}}}
}

// advanceStorageJobsClock moves the shared clock forward by the form's
// duration, and answers the clock's time after it.
func advanceStorageJobsClock(ctx context.Context, endpoint string, values map[string]string) (*console.Listing, error) {
	d, err := parseAdvance(values["duration"])
	if err != nil {
		return nil, err
	}
	var now struct {
		Now string `json:"now"`
	}
	if err := storageCall(ctx, http.MethodPost, "http://"+endpoint+"/cloudburrow/v1/clock:advance",
		map[string]string{"duration": strconv.FormatInt(int64(d/time.Second), 10) + "s"}, &now); err != nil {
		return nil, err
	}
	return &console.Listing{Columns: []string{"Time"}, NameColumn: "Clock", Noun: "clocks", Total: 1,
		Items: []console.Resource{{Name: "Storage Transfer, Storage Insights and Batch Operations",
			Fields: map[string]string{"Time": orDash(now.Now)}}}}, nil
}

// --- shared helpers ---------------------------------------------------------

// listPages reads every page of a list call (at most 20 pages), decoding
// each item under key into out.
func listPages[T any](ctx context.Context, rawURL, key string) ([]T, error) {
	var out []T
	token := ""
	for range 20 {
		u := rawURL
		if token != "" {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "pageToken=" + urlpkg.QueryEscape(token)
		}
		var page map[string]json.RawMessage
		if err := storageCall(ctx, http.MethodGet, u, nil, &page); err != nil {
			return nil, err
		}
		if raw, ok := page[key]; ok {
			var items []T
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		token = ""
		if raw, ok := page["nextPageToken"]; ok {
			_ = json.Unmarshal(raw, &token)
		}
		if token == "" {
			break
		}
	}
	return out, nil
}

// gsBucket reads a bucket from gs://BUCKET/ (Browse's form) or BUCKET.
func gsBucket(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "gs://")
	b, _, _ := strings.Cut(s, "/")
	return b
}

// textLines are a textarea's non-empty lines, trimmed.
func textLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// count formats a JSON int64 (a string in proto JSON) or a number.
func jobCount(v json.Number) string {
	if v == "" {
		return "0"
	}
	return string(v)
}

// --- Storage Batch Operations -----------------------------------------------

type storageBatchProvider struct{ endpoint string }

func (storageBatchProvider) ID() string    { return "storage-batch-operations" }
func (storageBatchProvider) Title() string { return "Batch operations" }

// batchCounters are a job's or bucket operation's counters.
type batchCounters struct {
	Total     json.Number `json:"totalObjectCount"`
	Succeeded json.Number `json:"succeededObjectCount"`
	Failed    json.Number `json:"failedObjectCount"`
	Bytes     json.Number `json:"totalBytesFound"`
}

type batchErrorSummary struct {
	ErrorCode  string      `json:"errorCode"`
	ErrorCount json.Number `json:"errorCount"`
	Entries    []struct {
		ObjectURI string   `json:"objectUri"`
		Details   []string `json:"errorDetails"`
	} `json:"errorLogEntries"`
}

type batchPrefixList struct {
	IncludedObjectPrefixes []string `json:"includedObjectPrefixes,omitempty"`
}

type batchManifest struct {
	ManifestLocation string `json:"manifestLocation,omitempty"`
}

// batchJobJSON is a Job as the REST API answers it; the transformation is
// kept raw, by its field name.
type batchJobJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	BucketList  *struct {
		Buckets []struct {
			Bucket     string           `json:"bucket"`
			PrefixList *batchPrefixList `json:"prefixList"`
			Manifest   *batchManifest   `json:"manifest"`
		} `json:"buckets"`
	} `json:"bucketList"`
	DeleteObject              json.RawMessage     `json:"deleteObject"`
	PutMetadata               json.RawMessage     `json:"putMetadata"`
	RewriteObject             json.RawMessage     `json:"rewriteObject"`
	PutObjectHold             json.RawMessage     `json:"putObjectHold"`
	UpdateObjectCustomContext json.RawMessage     `json:"updateObjectCustomContext"`
	CreateTime                string              `json:"createTime"`
	ScheduleTime              string              `json:"scheduleTime"`
	CompleteTime              string              `json:"completeTime"`
	Counters                  *batchCounters      `json:"counters"`
	ErrorSummaries            []batchErrorSummary `json:"errorSummaries"`
	State                     string              `json:"state"`
	DryRun                    bool                `json:"dryRun"`
}

// transformation is the job's operation as Google's console names it, and
// its settings.
func (j batchJobJSON) transformation() (string, string) {
	for _, t := range []struct {
		raw   json.RawMessage
		label string
	}{
		{j.DeleteObject, "Delete objects"}, {j.PutMetadata, "Update object metadata"},
		{j.RewriteObject, "Update encryption key"}, {j.PutObjectHold, "Update object holds"},
		{j.UpdateObjectCustomContext, "Update object contexts"},
	} {
		if len(t.raw) > 0 && string(t.raw) != "null" {
			return t.label, string(t.raw)
		}
	}
	return "", ""
}

func (j batchJobJSON) buckets() string {
	if j.BucketList == nil {
		return ""
	}
	var out []string
	for _, b := range j.BucketList.Buckets {
		out = append(out, b.Bucket)
	}
	return strings.Join(out, ", ")
}

func (j batchJobJSON) source() string {
	if j.BucketList == nil {
		return ""
	}
	var out []string
	for _, b := range j.BucketList.Buckets {
		switch {
		case b.Manifest != nil:
			out = append(out, "Manifest "+b.Manifest.ManifestLocation)
		case b.PrefixList != nil && len(b.PrefixList.IncludedObjectPrefixes) > 0:
			out = append(out, "Prefixes "+strings.Join(b.PrefixList.IncludedObjectPrefixes, ", "))
		default:
			out = append(out, "Every object in gs://"+b.Bucket)
		}
	}
	return strings.Join(out, "; ")
}

func (j batchJobJSON) counters() batchCounters {
	if j.Counters == nil {
		return batchCounters{}
	}
	return *j.Counters
}

func (p storageBatchProvider) parent(project string) string {
	return "http://" + p.endpoint + "/v1/projects/" + urlpkg.PathEscape(project) + "/locations/global"
}

func (p storageBatchProvider) jobURL(project, id string) string {
	return p.parent(project) + "/jobs/" + urlpkg.PathEscape(id)
}

func (p storageBatchProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{
		Columns:      []string{"Operation", "Buckets", "Objects", "Failed", "Dry run", "Created", "Completed"},
		NameColumn:   "Job ID",
		Noun:         "jobs",
		AlwaysStatus: true,
		Note: "Storage batch operations jobs over this instance's buckets. A job needs the project's Storage " +
			"Intelligence (Storage Intelligence > Configuration) enabled, as on Google Cloud.",
	}
	if project == "" {
		listing.Prompt = "Batch operations are shown for a project. Choose one in the toolbar."
		return listing, nil
	}
	jobs, err := listPages[batchJobJSON](ctx, p.parent(project)+"/jobs?pageSize=100", "jobs")
	if err != nil {
		return console.Listing{}, fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		op, _ := j.transformation()
		c := j.counters()
		dry := ""
		if j.DryRun {
			dry = "Yes"
		}
		listing.Items = append(listing.Items, console.Resource{Name: lastSegment(j.Name), Status: j.State, Fields: map[string]string{
			"Operation": op, "Buckets": j.buckets(),
			"Objects": jobCount(c.Succeeded) + " of " + jobCount(c.Total), "Failed": jobCount(c.Failed), "Dry run": dry,
			"Created": shortTime(j.CreateTime), "Completed": shortTime(j.CompleteTime),
		}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

const batchJobIDPattern = `^[a-z0-9]([a-z0-9-]{0,126}[a-z0-9])?$`

func (storageBatchProvider) CreateForm() (string, []console.Field) {
	unchanged := map[string]string{"": "No change", "SET": "Set", "UNSET": "Remove"}
	return "Create job", []console.Field{
		{Name: "jobId", Label: "Job ID", Type: "text", Required: true, Section: "Name your job", Pattern: batchJobIDPattern,
			Help: "Lowercase letters, digits and hyphens, up to 128 characters, unique in the project."},
		{Name: "description", Label: "Description", Type: "text", Section: "Name your job"},
		{Name: "bucket", Label: "Bucket", Type: "text", Required: true, Section: "Choose a source", Browse: "storage",
			Help: "The bucket whose objects the job changes."},
		{Name: "source", Label: "Objects", Type: "radio", Section: "Choose a source", Default: "prefixes",
			Options:      []string{"prefixes", "manifest"},
			OptionLabels: map[string]string{"prefixes": "Object prefixes", "manifest": "Manifest file (CSV)"}},
		{Name: "prefixes", Label: "Object prefixes", Type: "textarea", Section: "Choose a source", ShowWhen: "source=prefixes",
			Help: "One prefix per line, up to 1,000. Leave it empty for every object in the bucket."},
		{Name: "manifest", Label: "Manifest location", Type: "text", Section: "Choose a source", ShowWhen: "source=manifest",
			Pattern: `^\s*(gs://[^\/\s]+/\S+)?\s*$`,
			Help:    "gs://BUCKET/OBJECT.csv, a CSV whose header is bucket,name,generation (generation optional)."},
		{Name: "operation", Label: "Transformation", Type: "radio", Section: "Choose a transformation", Default: "delete",
			Options: []string{"delete", "metadata", "hold", "rewrite"},
			OptionLabels: map[string]string{"delete": "Delete objects", "metadata": "Update object metadata",
				"hold": "Update object holds", "rewrite": "Update encryption key"}},
		{Name: "permanent", Label: "Delete permanently, noncurrent versions included", Type: "checkbox",
			Section: "Choose a transformation", ShowWhen: "operation=delete",
			Help: "Unchecked, a versioned bucket keeps each deleted object as a noncurrent version."},
		{Name: "contentType", Label: "Content-Type", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=metadata"},
		{Name: "cacheControl", Label: "Cache-Control", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=metadata"},
		{Name: "contentDisposition", Label: "Content-Disposition", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=metadata"},
		{Name: "contentEncoding", Label: "Content-Encoding", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=metadata"},
		{Name: "contentLanguage", Label: "Content-Language", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=metadata"},
		{Name: "customTime", Label: "Custom-Time", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=metadata",
			Pattern: `^\s*([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z)?\s*$`, Help: "An RFC 3339 time, such as 2026-01-01T00:00:00Z."},
		{Name: "customMetadata", Label: "Custom metadata", Type: "map", Section: "Choose a transformation", ShowWhen: "operation=metadata"},
		{Name: "temporaryHold", Label: "Temporary hold", Type: "select", Section: "Choose a transformation", ShowWhen: "operation=hold",
			Options: []string{"", "SET", "UNSET"}, OptionLabels: unchanged},
		{Name: "eventBasedHold", Label: "Event-based hold", Type: "select", Section: "Choose a transformation", ShowWhen: "operation=hold",
			Options: []string{"", "SET", "UNSET"}, OptionLabels: unchanged},
		{Name: "kmsKey", Label: "Cloud KMS key", Type: "text", Section: "Choose a transformation", ShowWhen: "operation=rewrite",
			Pattern: `^\s*(projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/\s]+)?\s*$`,
			Help:    "projects/P/locations/L/keyRings/R/cryptoKeys/K: each object is rewritten encrypted with it."},
		{Name: "dryRun", Label: "Dry run", Type: "checkbox", Section: "Review",
			Help: "Counts the objects the job would change, and changes none."},
	}
}

// CreateOnPage implements console.PageCreator: Google's Create a batch
// operations job is a page of steps.
func (storageBatchProvider) CreateOnPage() bool { return true }

// CreateTitle implements console.CreateTitler.
func (storageBatchProvider) CreateTitle() string { return "Create a batch operations job" }

// batchJobBody is the Job the form describes.
func batchJobBody(values map[string]string) (map[string]any, error) {
	bucket := gsBucket(values["bucket"])
	if bucket == "" {
		return nil, errors.New("choose a bucket")
	}
	b := map[string]any{"bucket": bucket}
	if values["source"] == "manifest" {
		m := strings.TrimSpace(values["manifest"])
		if m == "" {
			return nil, errors.New("give the manifest's location, gs://BUCKET/OBJECT.csv")
		}
		b["manifest"] = map[string]any{"manifestLocation": m}
	} else {
		prefixes := textLines(values["prefixes"])
		if len(prefixes) > 1000 {
			return nil, fmt.Errorf("%d prefixes: a job takes at most 1,000", len(prefixes))
		}
		if len(prefixes) == 0 {
			prefixes = []string{""}
		}
		b["prefixList"] = map[string]any{"includedObjectPrefixes": prefixes}
	}
	job := map[string]any{"bucketList": map[string]any{"buckets": []any{b}}}
	if d := strings.TrimSpace(values["description"]); d != "" {
		job["description"] = d
	}
	if values["dryRun"] == "true" {
		job["dryRun"] = true
	}
	switch values["operation"] {
	case "delete", "":
		job["deleteObject"] = map[string]any{"permanentObjectDeletionEnabled": values["permanent"] == "true"}
	case "metadata":
		md := map[string]any{}
		for _, k := range []string{"contentType", "cacheControl", "contentDisposition", "contentEncoding", "contentLanguage", "customTime"} {
			if v := strings.TrimSpace(values[k]); v != "" {
				md[k] = v
			}
		}
		if raw := strings.TrimSpace(values["customMetadata"]); raw != "" {
			custom := map[string]string{}
			for _, l := range textLines(raw) {
				k, v, ok := strings.Cut(l, "=")
				if !ok {
					return nil, fmt.Errorf("custom metadata %q is not KEY=VALUE", l)
				}
				custom[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			md["customMetadata"] = custom
		}
		if len(md) == 0 {
			return nil, errors.New("set at least one metadata field")
		}
		job["putMetadata"] = md
	case "hold":
		h := map[string]any{}
		for _, k := range []string{"temporaryHold", "eventBasedHold"} {
			if v := values[k]; v == "SET" || v == "UNSET" {
				h[k] = v
			}
		}
		if len(h) == 0 {
			return nil, errors.New("set or remove at least one hold")
		}
		job["putObjectHold"] = h
	case "rewrite":
		k := strings.TrimSpace(values["kmsKey"])
		if k == "" {
			return nil, errors.New("give the Cloud KMS key to rewrite the objects with")
		}
		job["rewriteObject"] = map[string]any{"kmsKey": k}
	default:
		return nil, fmt.Errorf("%q is not a transformation", values["operation"])
	}
	return job, nil
}

func (p storageBatchProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project")
	}
	id := strings.TrimSpace(values["jobId"])
	if !regexp.MustCompile(batchJobIDPattern).MatchString(id) {
		return "", fmt.Errorf("%q is not a job ID: lowercase letters, digits and hyphens", id)
	}
	job, err := batchJobBody(values)
	if err != nil {
		return "", err
	}
	if err := storageCall(ctx, http.MethodPost, p.parent(project)+"/jobs?jobId="+urlpkg.QueryEscape(id), job, nil); err != nil {
		return "", err
	}
	return id, nil
}

func (p storageBatchProvider) job(ctx context.Context, project, id string) (batchJobJSON, error) {
	var j batchJobJSON
	err := storageCall(ctx, http.MethodGet, p.jobURL(project, id), nil, &j)
	return j, err
}

func (p storageBatchProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	j, err := p.job(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the job: " + err.Error()}, nil
	}
	op, settings := j.transformation()
	c := j.counters()
	dry := "No"
	if j.DryRun {
		dry = "Yes"
	}
	props := []console.Property{
		{Label: "Job ID", Value: lastSegment(j.Name)},
		{Label: "Status", Value: j.State},
		{Label: "Description", Value: j.Description},
		{Label: "Transformation", Value: op},
		{Label: "Settings", Value: settings},
		{Label: "Source", Value: j.source()},
		{Label: "Dry run", Value: dry},
		{Label: "Created", Value: shortTime(j.CreateTime)},
		{Label: "Started", Value: shortTime(j.ScheduleTime)},
		{Label: "Completed", Value: shortTime(j.CompleteTime)},
		{Label: "Resource name", Value: j.Name, Copy: j.Name},
	}
	progress := []console.Property{
		{Label: "Objects found", Value: jobCount(c.Total)},
		{Label: "Succeeded", Value: jobCount(c.Succeeded)},
		{Label: "Failed", Value: jobCount(c.Failed)},
		{Label: "Bytes found", Value: string(c.Bytes)},
	}
	ops := console.Listing{Columns: []string{"Objects", "Failed", "Started", "Completed"}, NameColumn: "Bucket",
		Noun: "bucket operations", AlwaysStatus: true}
	type bucketOp struct {
		Name         string         `json:"name"`
		BucketName   string         `json:"bucketName"`
		State        string         `json:"state"`
		StartTime    string         `json:"startTime"`
		CompleteTime string         `json:"completeTime"`
		Counters     *batchCounters `json:"counters"`
	}
	bops, err := listPages[bucketOp](ctx, p.jobURL(project, path[0])+"/bucketOperations?pageSize=100", "bucketOperations")
	if err != nil {
		ops.Note = "cannot list the bucket operations: " + err.Error()
	}
	for _, b := range bops {
		bc := batchCounters{}
		if b.Counters != nil {
			bc = *b.Counters
		}
		ops.Items = append(ops.Items, console.Resource{Name: orDash(b.BucketName), Status: b.State, Fields: map[string]string{
			"Objects": jobCount(bc.Succeeded) + " of " + jobCount(bc.Total), "Failed": jobCount(bc.Failed),
			"Started": shortTime(b.StartTime), "Completed": shortTime(b.CompleteTime)}})
	}
	ops.Total = len(ops.Items)
	errs := console.Listing{Columns: []string{"Count", "Object", "Details"}, NameColumn: "Error code", Noun: "errors"}
	for _, s := range j.ErrorSummaries {
		if len(s.Entries) == 0 {
			errs.Items = append(errs.Items, console.Resource{Name: s.ErrorCode, Fields: map[string]string{"Count": jobCount(s.ErrorCount)}})
		}
		for _, e := range s.Entries {
			errs.Items = append(errs.Items, console.Resource{Name: s.ErrorCode, Fields: map[string]string{
				"Count": jobCount(s.ErrorCount), "Object": e.ObjectURI, "Details": strings.Join(e.Details, "; ")}})
		}
	}
	errs.Total = len(errs.Items)
	return console.Detail{Summary: []console.Property{props[1], props[3]}, Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Job", Properties: props}, {Heading: "Progress", Properties: progress}}},
		{ID: "buckets", Label: "Bucket operations", Listing: ops},
		{ID: "errors", Label: "Errors", Listing: errs},
	}}, nil
}

const actBatchCancel = "cancel"

// Actions implements console.Actor: Cancel on a running job.
func (storageBatchProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "RUNNING" {
		return []console.Action{{ID: actBatchCancel, Label: "Cancel", Destructive: true,
			Confirm: "Cancel the job? Objects already changed stay changed."}}
	}
	return nil
}

func (p storageBatchProvider) Act(ctx context.Context, project, name, action string) error {
	if action != actBatchCancel {
		return fmt.Errorf("%s is not a batch operations action", action)
	}
	return storageCall(ctx, http.MethodPost, p.jobURL(project, name)+":cancel", map[string]any{}, nil)
}

// DetailActions implements console.PathActor.
func (p storageBatchProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	j, err := p.job(ctx, project, path[0])
	if err != nil {
		return nil
	}
	return p.Actions(console.Resource{Status: j.State})
}

// ActAt implements console.PathActor.
func (p storageBatchProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.Act(ctx, project, path[0], action)
}

// Delete implements console.Deleter: jobs.delete, which Google refuses
// while the job runs.
func (p storageBatchProvider) Delete(ctx context.Context, project, name string) error {
	return storageCall(ctx, http.MethodDelete, p.jobURL(project, name), nil, nil)
}

var (
	_ console.PageCreator  = storageBatchProvider{}
	_ console.CreateTitler = storageBatchProvider{}
	_ console.Driller      = storageBatchProvider{}
	_ console.Actor        = storageBatchProvider{}
	_ console.PathActor    = storageBatchProvider{}
	_ console.Deleter      = storageBatchProvider{}
)
