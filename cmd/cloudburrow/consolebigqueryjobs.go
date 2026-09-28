package main

// BigQuery jobs in the console (#993): a load from Cloud Storage and an
// export (extract) to Cloud Storage, on a dataset's and a table's page, and
// the Job history screen, which lists every job of the served project and
// opens each one.
//
// The forms offer what the validating front (internal/bigqueryfront) serves
// and CloudBurrow's compat tests exercise, and nothing else
// (docs/compatibility.md, the BigQuery rows of #919, #931, #939, #944, #945,
// #952, #957, #960, #966, #970):
//
//   - a load: CSV, NEWLINE_DELIMITED_JSON or Parquet from gs:// URIs, a
//     wildcard included; WRITE_APPEND or WRITE_TRUNCATE; a schema, autodetect
//     or neither (a table that exists keeps its own); for CSV,
//     skipLeadingRows, fieldDelimiter, quote, allowJaggedRows, nullMarker,
//     nullMarkers, allowQuotedNewlines, encoding (UTF-8 or ISO-8859-1),
//     maxBadRecords, ignoreUnknownValues, preserveAsciiControlCharacters and
//     sourceColumnMatch. timeZone and the date and time formats are 501 from
//     the front, so they are not offered; WRITE_EMPTY and Avro or ORC are not
//     either, as nothing tests them.
//   - an export: one URI (several are 501), CSV or NEWLINE_DELIMITED_JSON,
//     GZIP or none, a one-character delimiter and the header row. Avro and
//     Parquet are 501, and DEFLATE and SNAPPY are only for them.
//
// A combination the front refuses (a JSON autodetect load into a new table,
// a Parquet load whose file has a column the table lacks, a JSON export of a NULL, a
// sourceColumnMatch NAME without one header row, ...) is sent as it is, and
// the refusal is shown in the API's words: the front is the authority, as it
// is for every BigQuery form (#874).
//
// The Job history screen reads jobs.list with the full projection, which the
// front completes (#958, #971, #972), and a job's page reads jobs.get over
// REST, because the generated client decodes a statistics.load.badRecords
// that was left out as 0, and a count the job does not report is not shown
// (#966). Delete job is jobs.delete; Cancel job is jobs.cancel, offered only
// on a job that is not DONE: every job the emulator and the front run is done
// by the time jobs.insert answers, and cancelling a done job leaves it as it
// is, which a button would not say.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	bqv2 "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The choices the load and export forms offer.
var (
	bigqueryLoadFormats   = []string{"CSV", "NEWLINE_DELIMITED_JSON", "PARQUET"}
	bigqueryWriteModes    = []string{"WRITE_APPEND", "WRITE_TRUNCATE"}
	bigqueryEncodings     = []string{"UTF-8", "ISO-8859-1"}
	bigqueryColumnMatches = []string{"POSITION", "NAME"}
	bigqueryExportFormats = []string{"CSV", "NEWLINE_DELIMITED_JSON"}
	bigqueryCompressions  = []string{"NONE", "GZIP"}
)

// bigqueryURIPattern is a Cloud Storage URI: gs://, a bucket, a slash and an
// object name, which may hold one * wildcard.
const bigqueryURIPattern = `^gs://[a-z0-9][a-z0-9._\-]*/.+$`

// bigqueryJobTimeout bounds one load or export, inside the minute the
// console gives an action.
const bigqueryJobTimeout = 50 * time.Second

// bigqueryLoadFields are Load from Cloud Storage's inputs. On a dataset's
// page the form names the destination table; on a table's page it is that
// table.
func bigqueryLoadFields(withTable bool) []console.Field {
	var fields []console.Field
	if withTable {
		fields = append(fields, console.Field{Name: "tableId", Label: "Table ID", Type: "text", Required: true,
			Pattern: bigqueryTableIDPattern,
			Help:    "The table to load into: made by the load if it does not exist."})
	}
	fields = append(fields,
		console.Field{Name: "uris", Label: "Source URIs", Type: "textarea", Required: true,
			Help: "One gs:// URI per line, in this instance's Cloud Storage. An object name may hold one * " +
				"wildcard, which reads every object it matches."},
		console.Field{Name: "format", Label: "File format", Type: "select", Options: bigqueryLoadFormats},
		console.Field{Name: "writeDisposition", Label: "Write preference", Type: "select", Options: bigqueryWriteModes,
			Help: "WRITE_APPEND adds the rows to the table; WRITE_TRUNCATE replaces what it holds."},
		console.Field{Name: "autodetect", Label: "Auto-detect the schema", Type: "checkbox", Section: "Schema",
			Help: "BigQuery reads the columns from the data. Leave it off, with no fields below, to load into a " +
				"table that exists with its own schema."},
		console.Field{Name: "schema", Label: "Schema", Type: "schema", Options: bigqueryColumnTypes,
			Pattern: bigqueryFieldNamePattern, Section: "Schema",
			Help: "Optional: the columns of the loaded data, in order. " + bigqueryFieldNameHelp},
		console.Field{Name: "maxBadRecords", Label: "Number of errors allowed", Type: "number", Section: "Advanced options",
			Help: "How many bad records the load leaves out before it fails. Each one left out is listed on the " +
				"job's page. Empty is 0."},
		console.Field{Name: "ignoreUnknownValues", Label: "Ignore unknown values", Type: "checkbox", Section: "Advanced options",
			Help: "A value for a column the table does not have is dropped rather than making its record bad."},
		console.Field{Name: "skipLeadingRows", Label: "Header rows to skip", Type: "number", Section: "CSV options",
			Help: "CSV only: rows at the top of each file that are not data. Empty is 0."},
		console.Field{Name: "fieldDelimiter", Label: "Field delimiter", Type: "text", Default: ",", Section: "CSV options",
			Help: "CSV only: one ASCII character, or \\t for a tab."},
		console.Field{Name: "quote", Label: "Quote character", Type: "text", Default: `"`, Section: "CSV options",
			Help: "CSV only: one ASCII character. Empty: values are not quoted."},
		console.Field{Name: "nullMarker", Label: "Null marker", Type: "text", Section: "CSV options",
			Help: "CSV only: the text that stands for NULL, such as \\N."},
		console.Field{Name: "nullMarkers", Label: "Null markers", Type: "textarea", Section: "CSV options",
			Help: "CSV only: one per line, each loaded as NULL. Not with a null marker."},
		console.Field{Name: "encoding", Label: "Encoding", Type: "select", Options: bigqueryEncodings, Section: "CSV options"},
		console.Field{Name: "sourceColumnMatch", Label: "Source column match", Type: "select", Options: bigqueryColumnMatches,
			Section: "CSV options",
			Help: "CSV only: POSITION puts each value in the column at its place; NAME in the column its header " +
				"names, with one header row to skip."},
		console.Field{Name: "allowJaggedRows", Label: "Allow jagged rows", Type: "checkbox", Section: "CSV options",
			Help: "CSV only: a row missing trailing columns loads them as NULL."},
		console.Field{Name: "allowQuotedNewlines", Label: "Allow quoted newlines", Type: "checkbox", Section: "CSV options",
			Help: "CSV only: a quoted value may span lines."},
		console.Field{Name: "preserveAsciiControlCharacters", Label: "Preserve ASCII control characters",
			Type: "checkbox", Section: "CSV options",
			Help: "CSV only: control characters other than tab and newline are loaded as they are."},
	)
	return fields
}

// bigqueryExportFields are Export to Cloud Storage's inputs.
func bigqueryExportFields() []console.Field {
	return []console.Field{
		{Name: "uri", Label: "Destination URI", Type: "text", Required: true, Pattern: bigqueryURIPattern,
			Help: "One gs:// URI in this instance's Cloud Storage, in a bucket that exists. A * wildcard is " +
				"written as 000000000000."},
		{Name: "format", Label: "Export format", Type: "select", Options: bigqueryExportFormats},
		{Name: "compression", Label: "Compression", Type: "select", Options: bigqueryCompressions},
		{Name: "fieldDelimiter", Label: "Field delimiter", Type: "text", Default: ",",
			Help: "CSV only: one printable ASCII character, or \\t for a tab."},
		{Name: "header", Label: "Print header", Type: "checkbox", Default: "true",
			Help: "CSV only: the column names as the first row."},
	}
}

// bigqueryCSVOnly are the load fields that apply to CSV and to no other
// format, with the value each holds when left alone.
var bigqueryCSVOnly = map[string]string{
	"skipLeadingRows": "", "fieldDelimiter": ",", "quote": `"`, "nullMarker": "", "nullMarkers": "",
	"encoding": "UTF-8", "sourceColumnMatch": "POSITION", "allowJaggedRows": "false",
	"allowQuotedNewlines": "false", "preserveAsciiControlCharacters": "false",
}

// checked reads a checkbox's value.
func checked(values map[string]string, name string) bool { return values[name] == "true" }

// optionalCount reads a number field, empty being 0.
func optionalCount(values map[string]string, name, label string) (int64, error) {
	s := strings.TrimSpace(values[name])
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s is a whole number of 0 or more, not %q", label, s)
	}
	return n, nil
}

// loadSource builds the load's source from the form: the URIs, the format
// and, for CSV, its options. A CSV option changed on a load of another
// format is refused, since only CSV reads them.
func loadSource(values map[string]string) (*bigquery.GCSReference, error) {
	var uris []string
	for _, line := range strings.Split(values["uris"], "\n") {
		if u := strings.TrimSpace(line); u != "" {
			uris = append(uris, u)
		}
	}
	if len(uris) == 0 {
		return nil, errors.New("at least one gs:// URI is required")
	}
	format := values["format"]
	if format == "" {
		format = "CSV"
	}
	if !slices.Contains(bigqueryLoadFormats, format) {
		return nil, fmt.Errorf("the file format is one of %s, not %q", strings.Join(bigqueryLoadFormats, ", "), format)
	}
	src := bigquery.NewGCSReference(uris...)
	src.SourceFormat = bigquery.DataFormat(format)
	src.AutoDetect = checked(values, "autodetect")
	if strings.TrimSpace(values["schema"]) != "" {
		schema, err := parseSchemaField(values["schema"])
		if err != nil {
			return nil, err
		}
		src.Schema = schema
	}
	var err error
	if src.MaxBadRecords, err = optionalCount(values, "maxBadRecords", "Number of errors allowed"); err != nil {
		return nil, err
	}
	src.IgnoreUnknownValues = checked(values, "ignoreUnknownValues")

	if format != "CSV" {
		names := make([]string, 0, len(bigqueryCSVOnly))
		for name, unset := range bigqueryCSVOnly {
			if v, ok := values[name]; ok && strings.TrimSpace(v) != strings.TrimSpace(unset) {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			return nil, fmt.Errorf("%s applies to CSV only, and this load is %s", strings.Join(names, ", "), format)
		}
		return src, nil
	}
	if src.SkipLeadingRows, err = optionalCount(values, "skipLeadingRows", "Header rows to skip"); err != nil {
		return nil, err
	}
	// An option left at BigQuery's default is not sent, so the job holds
	// what the form changed, as a client that sets nothing sends it.
	if d, ok := values["fieldDelimiter"]; ok && d != "," {
		src.FieldDelimiter = d
	}
	if q, ok := values["quote"]; ok && q != `"` {
		// An empty quote is "no quoting", which the client sends only when
		// told to (ForceZeroQuote).
		src.Quote, src.ForceZeroQuote = q, q == ""
	}
	src.NullMarker = values["nullMarker"]
	for _, line := range strings.Split(values["nullMarkers"], "\n") {
		if m := strings.TrimRight(line, "\r"); m != "" {
			src.NullMarkers = append(src.NullMarkers, m)
		}
	}
	if e := values["encoding"]; e != "" {
		if !slices.Contains(bigqueryEncodings, e) {
			return nil, fmt.Errorf("the encoding is UTF-8 or ISO-8859-1, not %q", e)
		}
		if e != "UTF-8" {
			src.Encoding = bigquery.Encoding(e)
		}
	}
	if m := values["sourceColumnMatch"]; m != "" {
		if !slices.Contains(bigqueryColumnMatches, m) {
			return nil, fmt.Errorf("the source column match is POSITION or NAME, not %q", m)
		}
		if m != "POSITION" {
			src.SourceColumnMatch = bigquery.SourceColumnMatch(m)
		}
	}
	src.AllowJaggedRows = checked(values, "allowJaggedRows")
	src.AllowQuotedNewlines = checked(values, "allowQuotedNewlines")
	src.PreserveASCIIControlCharacters = checked(values, "preserveAsciiControlCharacters")
	return src, nil
}

// loadJob runs Load from Cloud Storage into datasetID.tableID and waits for
// it. A job that fails is the error, in the API's words; one that succeeds
// is the result row, which links to its page in Job history.
func (p bigqueryProvider) loadJob(ctx context.Context, project, datasetID, tableID string, values map[string]string) (*console.Listing, error) {
	src, err := loadSource(values)
	if err != nil {
		return nil, err
	}
	write := values["writeDisposition"]
	if write == "" {
		write = "WRITE_APPEND"
	}
	if !slices.Contains(bigqueryWriteModes, write) {
		return nil, fmt.Errorf("the write preference is WRITE_APPEND or WRITE_TRUNCATE, not %q", write)
	}
	ctx, cancel := context.WithTimeout(ctx, bigqueryJobTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	loader := c.Dataset(datasetID).Table(tableID).LoaderFrom(src)
	loader.WriteDisposition = bigquery.TableWriteDisposition(write)
	job, err := loader.Run(ctx)
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	return p.finishedJob(ctx, project, job)
}

// exportJob runs Export to Cloud Storage of datasetID.tableID and waits for
// it.
func (p bigqueryProvider) exportJob(ctx context.Context, project, datasetID, tableID string, values map[string]string) (*console.Listing, error) {
	uri := strings.TrimSpace(values["uri"])
	if !strings.HasPrefix(uri, "gs://") {
		return nil, errors.New("the destination is a gs:// URI")
	}
	format := values["format"]
	if format == "" {
		format = "CSV"
	}
	if !slices.Contains(bigqueryExportFormats, format) {
		return nil, fmt.Errorf("the export format is CSV or NEWLINE_DELIMITED_JSON, not %q", format)
	}
	compression := values["compression"]
	if compression == "" {
		compression = "NONE"
	}
	if !slices.Contains(bigqueryCompressions, compression) {
		return nil, fmt.Errorf("the compression is NONE or GZIP, not %q", compression)
	}
	dst := bigquery.NewGCSReference(uri)
	dst.DestinationFormat = bigquery.DataFormat(format)
	dst.Compression = bigquery.Compression(compression)
	header := values["header"] != "false"
	if format == "CSV" {
		if d, ok := values["fieldDelimiter"]; ok {
			dst.FieldDelimiter = d
		}
	} else if d := values["fieldDelimiter"]; (d != "" && d != ",") || values["header"] == "false" {
		return nil, fmt.Errorf("the field delimiter and the header row apply to CSV only, and this export is %s", format)
	}
	ctx, cancel := context.WithTimeout(ctx, bigqueryJobTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	extractor := c.Dataset(datasetID).Table(tableID).ExtractorTo(dst)
	extractor.DisableHeader = format == "CSV" && !header
	job, err := extractor.Run(ctx)
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	return p.finishedJob(ctx, project, job)
}

// finishedJob waits for a job and says how it ended: the API's error if it
// failed, else one row naming it, with what it did.
func (p bigqueryProvider) finishedJob(ctx context.Context, project string, job *bigquery.Job) (*console.Listing, error) {
	status, err := job.Wait(ctx)
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	if err := status.Err(); err != nil {
		return nil, jobRefusal(err)
	}
	fields := map[string]string{"State": "Succeeded"}
	out := &console.Listing{NameColumn: "Job", Noun: "jobs"}
	switch d := statusDetails(status).(type) {
	case *bigquery.LoadStatistics:
		out.Columns = []string{"State", "Output rows", "Input files"}
		fields["Output rows"] = strconv.FormatInt(d.OutputRows, 10)
		fields["Input files"] = strconv.FormatInt(d.InputFiles, 10)
	case *bigquery.ExtractStatistics:
		out.Columns = []string{"State", "Files written"}
		n := int64(0)
		for _, c := range d.DestinationURIFileCounts {
			n += c
		}
		fields["Files written"] = strconv.FormatInt(n, 10)
	default:
		out.Columns = []string{"State"}
	}
	if len(status.Errors) > 0 {
		noun := "records"
		if len(status.Errors) == 1 {
			noun = "record"
		}
		out.Note = fmt.Sprintf("The job left out %d bad %s; its page lists them.", len(status.Errors), noun)
	}
	out.Items = []console.Resource{{Name: job.ID(), Fields: fields,
		Link: "/bigquery-jobs/" + url.PathEscape(job.ID()) + "?project=" + url.QueryEscape(project)}}
	out.Total = 1
	return out, nil
}

func statusDetails(s *bigquery.JobStatus) bigquery.Statistics {
	if s == nil || s.Statistics == nil {
		return nil
	}
	return s.Statistics.Details
}

// jobRefusal is a failed job's errorResult in the API's words: its message,
// and where it applies when the API says.
func jobRefusal(err error) error {
	var e *bigquery.Error
	if errors.As(err, &e) && e.Message != "" {
		if e.Location != "" && !strings.Contains(e.Message, e.Location) {
			return fmt.Errorf("%s (%s)", e.Message, e.Location)
		}
		return errors.New(e.Message)
	}
	return bigqueryRefusal(err)
}

// ActAtResult implements console.ResultActor: a load and an export answer
// with the job they ran, and Insert rows with Skip invalid rows with the rows
// it skipped (#994); every other action with nothing.
func (p bigqueryProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if err := p.writable(project); err != nil {
		return nil, err
	}
	switch {
	case action == "load" && len(path) == 1:
		return p.loadJob(ctx, project, path[0], strings.TrimSpace(values["tableId"]), values)
	case action == "load" && len(path) == 2:
		return p.loadJob(ctx, project, path[0], path[1], values)
	case action == "export" && len(path) == 2:
		return p.exportJob(ctx, project, path[0], path[1], values)
	case action == "insertrows" && len(path) == 2:
		return p.insertRows(ctx, path[0], path[1], values)
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

// --- Job history ----------------------------------------------------------

// bigqueryJobsProvider is the Job history screen: every job of the served
// project, newest first, as jobs.list gives them, and each job's page.
type bigqueryJobsProvider struct {
	bq bigqueryProvider
}

func (bigqueryJobsProvider) ID() string    { return "bigquery-jobs" }
func (bigqueryJobsProvider) Title() string { return "Job history" }

// service is the generated client, whose jobs.list gives each job's
// configuration and errorResult.
func (p bigqueryJobsProvider) service(ctx context.Context) (*bqv2.Service, error) {
	svc, err := bqv2.NewService(ctx, option.WithEndpoint("http://"+p.bq.endpoint), option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	return svc, nil
}

func (p bigqueryJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{
		Columns: []string{"Type", "Created", "Error"}, NameColumn: "Job ID", Noun: "jobs",
		RowsOpenable: true, AlwaysStatus: true,
	}
	if prompt := p.bq.scope(project); prompt != "" {
		base.Prompt = prompt
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := p.service(ctx)
	if err != nil {
		base.Unavailable = err.Error()
		return base, nil
	}
	token := ""
	for {
		call := svc.Jobs.List(p.bq.project).Projection("full").MaxResults(int64(detailLimit)).Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		page, err := call.Do()
		if err != nil {
			base.Unavailable = "listing jobs: " + bigqueryRefusal(err).Error()
			return base, nil
		}
		for _, j := range page.Jobs {
			if j.JobReference == nil {
				continue
			}
			base.Items = append(base.Items, console.Resource{
				Name:   j.JobReference.JobId,
				Status: jobStateWord(j.State, j.ErrorResult),
				Fields: map[string]string{
					"Type":    jobType(j.Configuration),
					"Created": jobTime(j.Statistics),
					"Error":   orDash(errorText(j.ErrorResult)),
				},
			})
			if len(base.Items) >= detailLimit {
				base.Note = truncatedNote(len(base.Items), "jobs")
				base.Total = len(base.Items)
				return base, nil
			}
		}
		if page.NextPageToken == "" || page.NextPageToken == token {
			break
		}
		token = page.NextPageToken
	}
	base.Total = len(base.Items)
	if base.Total == 0 {
		base.Note = "No jobs yet: a load, an export or a query job run here or by a client is listed here."
	}
	return base, nil
}

// jobStateWord is a job's state as the list shows it: a DONE job succeeded
// or failed, which the console's colours tell apart.
func jobStateWord(state string, errorResult *bqv2.ErrorProto) string {
	switch {
	case errorResult != nil:
		return "Failed"
	case state == "DONE":
		return "Succeeded"
	case state == "RUNNING":
		return "Running"
	case state == "PENDING":
		return "Pending"
	}
	return orDash(state)
}

// jobType is the configuration's jobType, or the kind of configuration it
// holds when it names none.
func jobType(c *bqv2.JobConfiguration) string {
	switch {
	case c == nil:
		return "—"
	case c.JobType != "":
		return c.JobType
	case c.Load != nil:
		return "LOAD"
	case c.Extract != nil:
		return "EXTRACT"
	case c.Query != nil:
		return "QUERY"
	case c.Copy != nil:
		return "COPY"
	}
	return "—"
}

// millis renders a time in milliseconds since the epoch, as BigQuery gives a
// job's times (#971), or "" for none.
func millis(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func jobTime(s *bqv2.JobStatistics) string {
	if s == nil {
		return "—"
	}
	return orDash(millis(s.CreationTime))
}

func errorText(e *bqv2.ErrorProto) string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Detail opens one job.
func (p bigqueryJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if prompt := p.bq.scope(project); prompt != "" {
		return console.Detail{Prompt: prompt}, nil
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	job, raw, err := p.getJob(ctx, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the job: " + err.Error()}, nil
	}
	return bigqueryJobPage(p.bq.project, job, raw), nil
}

// rawJob is what a job's page needs from jobs.get beyond the generated
// client's decoding: the configuration as the API gave it, and which load
// statistics it reported, since a count left out decodes as 0.
type rawJob struct {
	Configuration json.RawMessage `json:"configuration"`
	Statistics    struct {
		Load map[string]json.RawMessage `json:"load"`
	} `json:"statistics"`
}

// getJob reads jobs.get over REST.
func (p bigqueryJobsProvider) getJob(ctx context.Context, id string) (*bqv2.Job, rawJob, error) {
	u := "http://" + p.bq.endpoint + "/bigquery/v2/projects/" + url.PathEscape(p.bq.project) + "/jobs/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, rawJob{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, rawJob{}, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, rawJob{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return nil, rawJob{}, errors.New(e.Error.Message)
		}
		return nil, rawJob{}, fmt.Errorf("jobs.get answered %d", resp.StatusCode)
	}
	var job bqv2.Job
	var raw rawJob
	if err := json.Unmarshal(b, &job); err != nil {
		return nil, rawJob{}, fmt.Errorf("reading the job: %w", err)
	}
	_ = json.Unmarshal(b, &raw)
	return &job, raw, nil
}

// bigqueryJobPage is a job's page: its state, what it was asked to do, what it
// did, and its errors.
func bigqueryJobPage(project string, job *bqv2.Job, raw rawJob) console.Detail {
	var errorResult *bqv2.ErrorProto
	var errs []*bqv2.ErrorProto
	state := ""
	if job.Status != nil {
		errorResult, errs, state = job.Status.ErrorResult, job.Status.Errors, job.Status.State
	}
	word := jobStateWord(state, errorResult)
	typ := jobType(job.Configuration)
	id := ""
	location := ""
	if job.JobReference != nil {
		id, location = job.JobReference.JobId, job.JobReference.Location
	}

	jobProps := []console.Property{
		{Label: "Job ID", Value: project + ":" + id},
		{Label: "Type", Value: typ},
		{Label: "State", Value: orDash(state)},
	}
	if location != "" {
		jobProps = append(jobProps, console.Property{Label: "Location", Value: location})
	}
	if s := job.Statistics; s != nil {
		for _, t := range []struct {
			label string
			ms    int64
		}{{"Created", s.CreationTime}, {"Started", s.StartTime}, {"Ended", s.EndTime}} {
			if v := millis(t.ms); v != "" {
				jobProps = append(jobProps, console.Property{Label: t.label, Value: v})
			}
		}
		if s.StartTime > 0 && s.EndTime >= s.StartTime {
			jobProps = append(jobProps, console.Property{Label: "Duration",
				Value: (time.Duration(s.EndTime-s.StartTime) * time.Millisecond).String()})
		}
	}
	if errorResult != nil {
		jobProps = append(jobProps, console.Property{Label: "Error", Value: describeError(errorResult)})
	}
	groups := []console.PropertyGroup{{Heading: "Job", Properties: jobProps}}
	groups = append(groups, configurationGroups(job.Configuration)...)
	if g, ok := statisticsGroup(job.Statistics, raw); ok {
		groups = append(groups, g)
	}

	errList := console.Listing{Columns: []string{"Reason", "Location", "Message"}, NameColumn: "#", Noun: "errors"}
	for i, e := range errs {
		errList.Items = append(errList.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{
			"Reason": orDash(e.Reason), "Location": orDash(e.Location), "Message": orDash(e.Message),
		}})
	}
	errList.Total = len(errList.Items)
	switch {
	case errList.Total == 0:
		errList.Note = "The job reported no errors."
	case errorResult == nil:
		errList.Note = "The job succeeded and left out these bad records."
	}

	sections := []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups},
		{ID: "errors", Label: "Errors", Listing: errList},
	}
	if len(raw.Configuration) > 0 {
		var pretty strings.Builder
		var v any
		if json.Unmarshal(raw.Configuration, &v) == nil {
			enc := json.NewEncoder(&pretty)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			_ = enc.Encode(v)
			sections = append(sections, console.Section{ID: "configuration", Label: "Configuration",
				Kind: console.KindText, Text: strings.TrimRight(pretty.String(), "\n")})
		}
	}
	summary := []console.Property{{Label: "Type", Value: typ}, {Label: "State", Value: word}}
	if job.Statistics != nil {
		if v := millis(job.Statistics.CreationTime); v != "" {
			summary = append(summary, console.Property{Label: "Created", Value: v})
		}
	}
	return console.Detail{Summary: summary, Sections: sections}
}

func describeError(e *bqv2.ErrorProto) string {
	s := e.Message
	if e.Reason != "" {
		s = e.Reason + ": " + s
	}
	if e.Location != "" {
		s += " (" + e.Location + ")"
	}
	return s
}

// configurationGroups says what the job was asked to do, per kind.
func configurationGroups(c *bqv2.JobConfiguration) []console.PropertyGroup {
	if c == nil {
		return nil
	}
	add := func(props []console.Property, label, value string) []console.Property {
		if strings.TrimSpace(value) == "" {
			return props
		}
		return append(props, console.Property{Label: label, Value: value})
	}
	table := func(t *bqv2.TableReference) string {
		if t == nil {
			return ""
		}
		return t.ProjectId + "." + t.DatasetId + "." + t.TableId
	}
	var out []console.PropertyGroup
	if q := c.Query; q != nil {
		var props []console.Property
		props = add(props, "Query", q.Query)
		props = add(props, "Destination table", table(q.DestinationTable))
		props = add(props, "Write preference", q.WriteDisposition)
		out = append(out, console.PropertyGroup{Heading: "Query", Properties: props})
	}
	if l := c.Load; l != nil {
		var props []console.Property
		props = add(props, "Source URIs", strings.Join(l.SourceUris, "\n"))
		props = add(props, "File format", l.SourceFormat)
		props = add(props, "Destination table", table(l.DestinationTable))
		props = add(props, "Write preference", l.WriteDisposition)
		if l.Autodetect {
			props = add(props, "Auto-detect the schema", "Yes")
		}
		if l.Schema != nil && len(l.Schema.Fields) > 0 {
			names := make([]string, len(l.Schema.Fields))
			for i, f := range l.Schema.Fields {
				names[i] = f.Name + " " + f.Type
			}
			props = add(props, "Schema", strings.Join(names, ", "))
		}
		if l.SkipLeadingRows > 0 {
			props = add(props, "Header rows to skip", strconv.FormatInt(l.SkipLeadingRows, 10))
		}
		if l.MaxBadRecords > 0 {
			props = add(props, "Number of errors allowed", strconv.FormatInt(l.MaxBadRecords, 10))
		}
		props = add(props, "Field delimiter", l.FieldDelimiter)
		props = add(props, "Encoding", l.Encoding)
		out = append(out, console.PropertyGroup{Heading: "Load", Properties: props})
	}
	if e := c.Extract; e != nil {
		var props []console.Property
		props = add(props, "Source table", table(e.SourceTable))
		props = add(props, "Destination URIs", strings.Join(e.DestinationUris, "\n"))
		props = add(props, "Export format", e.DestinationFormat)
		props = add(props, "Compression", e.Compression)
		props = add(props, "Field delimiter", e.FieldDelimiter)
		out = append(out, console.PropertyGroup{Heading: "Export", Properties: props})
	}
	if cp := c.Copy; cp != nil {
		var props []console.Property
		props = add(props, "Source table", table(cp.SourceTable))
		props = add(props, "Destination table", table(cp.DestinationTable))
		out = append(out, console.PropertyGroup{Heading: "Copy", Properties: props})
	}
	var kept []console.PropertyGroup
	for _, g := range out {
		if len(g.Properties) > 0 {
			kept = append(kept, g)
		}
	}
	return kept
}

// statisticsGroup is what the job reported doing: a load's counts, each
// only if the job reported it, and an export's files.
func statisticsGroup(s *bqv2.JobStatistics, raw rawJob) (console.PropertyGroup, bool) {
	if s == nil {
		return console.PropertyGroup{}, false
	}
	var props []console.Property
	if l := s.Load; l != nil {
		for _, c := range []struct {
			key, label string
			n          int64
		}{
			{"outputRows", "Output rows", l.OutputRows},
			{"badRecords", "Bad records", l.BadRecords},
			{"inputFiles", "Input files", l.InputFiles},
			{"inputFileBytes", "Input file bytes", l.InputFileBytes},
		} {
			if _, reported := raw.Statistics.Load[c.key]; reported {
				props = append(props, console.Property{Label: c.label, Value: strconv.FormatInt(c.n, 10)})
			}
		}
	}
	if e := s.Extract; e != nil && len(e.DestinationUriFileCounts) > 0 {
		counts := make([]string, len(e.DestinationUriFileCounts))
		for i, n := range e.DestinationUriFileCounts {
			counts[i] = strconv.FormatInt(n, 10)
		}
		props = append(props, console.Property{Label: "Files written per URI", Value: strings.Join(counts, ", ")})
	}
	if q := s.Query; q != nil && q.StatementType != "" {
		props = append(props, console.Property{Label: "Statement type", Value: q.StatementType})
	}
	if len(props) == 0 {
		return console.PropertyGroup{}, false
	}
	return console.PropertyGroup{Heading: "Statistics", Properties: props}, true
}

// Delete implements console.Deleter for a job row: jobs.delete, which
// deletes the job's record and nothing it wrote.
func (p bigqueryJobsProvider) Delete(ctx context.Context, project, name string) error {
	if err := p.bq.writable(project); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := p.service(ctx)
	if err != nil {
		return err
	}
	return bigqueryRefusal(svc.Jobs.Delete(p.bq.project, name).Context(ctx).Do())
}

// DetailActions offers Delete job on a job's page, and Cancel job on one
// that is not done.
func (p bigqueryJobsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.bq.writable(project) != nil || len(path) != 1 {
		return nil
	}
	actions := []console.Action{}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if job, _, err := p.getJob(ctx, path[0]); err == nil && job.Status != nil && job.Status.State != "DONE" {
		actions = append(actions, console.Action{ID: "cancel", Label: "Cancel job",
			Confirm: "The job is asked to stop; what it has already written stays."})
	}
	return append(actions, console.Action{ID: "delete", Label: "Delete job", Destructive: true, Leaves: true,
		Confirm: "The job's record is deleted: Job history, jobs.get and jobs.list no longer find it. " +
			"Nothing the job wrote is deleted."})
}

// ActAt implements console.PathActor.
func (p bigqueryJobsProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if err := p.bq.writable(project); err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	switch action {
	case "delete":
		return p.Delete(ctx, project, path[0])
	case "cancel":
		ctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		svc, err := p.service(ctx)
		if err != nil {
			return err
		}
		_, err = svc.Jobs.Cancel(p.bq.project, path[0]).Context(ctx).Do()
		return bigqueryRefusal(err)
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.ResultActor = bigqueryProvider{}
	_ console.Driller     = bigqueryJobsProvider{}
	_ console.Deleter     = bigqueryJobsProvider{}
	_ console.PathActor   = bigqueryJobsProvider{}
)
