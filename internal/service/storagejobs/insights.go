package storagejobs

// The Storage Insights API v1's inventory reports (docs.cloud.google.com/
// storage/docs/insights/inventory-reports), JSON:
//
//	GET    /v1/projects/P/locations[/L]
//	POST   /v1/projects/P/locations/L/reportConfigs           body: ReportConfig
//	GET    /v1/projects/P/locations/L/reportConfigs[/C]
//	PATCH  /v1/projects/P/locations/L/reportConfigs/C?updateMask=
//	DELETE /v1/projects/P/locations/L/reportConfigs/C?force=
//	GET    /v1/projects/P/locations/L/reportConfigs/C/reportDetails[/D]
//
// A configuration writes one report per occurrence of its frequency
// (DAILY or WEEKLY from frequency_options.start_date at 00:00 UTC, through
// end_date) once the clock reaches it, occurrences before the day the
// configuration was made excepted. A report lists the source bucket's live
// objects, with the configured metadata fields in their order, as one CSV
// or Parquet shard (Google shards only past 1,000,000 objects) named
// <path>/<config>_<YYYY-MM-DDTHH:MM>_0.csv, followed by its manifest,
// <path>/<config>_<YYYY-MM-DDTHH:MM>_manifest.json, as Google names them.
// The destination bucket must be in the source bucket's project and
// location; a bucket takes at most 100 configurations.
//
// Dataset configurations, which link BigQuery datasets of the buckets'
// metadata, are datasets.go's.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sipb "cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/genproto/googleapis/type/datetime"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	spb "google.golang.org/genproto/googleapis/rpc/status"
)

// MaxReportConfigsPerBucket is Google's limit of inventory report
// configurations of one source bucket.
const MaxReportConfigsPerBucket = 100

// inventoryFields are the metadata fields a report can include; project,
// bucket and name are required.
var inventoryFields = []string{"project", "bucket", "name", "location", "size", "timeCreated", "timeDeleted", "updated",
	"storageClass", "etag", "retentionExpirationTime", "crc32c", "md5Hash", "generation", "metageneration", "contentType",
	"contentEncoding", "timeStorageClassUpdated"}

type reportConfig struct {
	Project string
	Config  *sipb.ReportConfig
	// Written are the occurrences written, by target time (RFC 3339).
	Written map[string]bool
	Details []*sipb.ReportDetail
}

type reportRecord struct {
	Project string            `json:"project"`
	Config  json.RawMessage   `json:"config"`
	Written map[string]bool   `json:"written"`
	Details []json.RawMessage `json:"details"`
}

// InsightsServer serves the Storage Insights API v1.
type InsightsServer struct {
	server
	mu      sync.Mutex
	configs map[string]*reportConfig
	// gen serializes report generation.
	gen sync.Mutex
	// ds is the dataset configurations (datasets.go), under mu.
	ds datasets
}

// NewInsightsServer serves the API on addr over rt.
func NewInsightsServer(addr string, rt *Runtime) *InsightsServer {
	s := &InsightsServer{configs: map[string]*reportConfig{}}
	s.server = server{name: "storageinsights", addr: addr, rt: rt, handler: s.route}
	var recs []reportRecord
	rt.restore(insightsFile, &recs)
	for _, r := range recs {
		c := &reportConfig{Project: r.Project, Config: &sipb.ReportConfig{}, Written: r.Written}
		fromProtoJSON(r.Config, c.Config)
		for _, d := range r.Details {
			rd := &sipb.ReportDetail{}
			fromProtoJSON(d, rd)
			c.Details = append(c.Details, rd)
		}
		if c.Written == nil {
			c.Written = map[string]bool{}
		}
		s.configs[c.Config.Name] = c
	}
	rt.onTick(s.writeDue)
	s.restoreDatasets()
	return s
}

const insightsFile = "storageinsights.json"

func (s *InsightsServer) saveLocked() {
	recs := []reportRecord{}
	for _, c := range s.configs {
		r := reportRecord{Project: c.Project, Config: protoJSON(c.Config), Written: c.Written}
		for _, d := range c.Details {
			r.Details = append(r.Details, protoJSON(d))
		}
		recs = append(recs, r)
	}
	s.rt.persist(insightsFile, recs)
}

// ResetProject forgets one project's configurations, or every project's.
func (s *InsightsServer) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, c := range s.configs {
		if project == "" || c.Project == project {
			delete(s.configs, n)
		}
	}
	s.saveLocked()
	s.resetDatasetsLocked(project)
	return nil
}

func (s *InsightsServer) route(r *http.Request) (string, any, error) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	p := strings.Split(path, "/")
	if len(p) < 3 || p[0] != "projects" || p[2] != "locations" {
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	project := p[1]
	q := r.URL.Query()
	const pre = "storageinsights.projects.locations."
	if len(p) == 3 && r.Method == http.MethodGet {
		return pre + "list", locationsList(project, insightsLocations), nil
	}
	if len(p) == 4 && r.Method == http.MethodGet {
		if unsupportedInsightsLocations[p[3]] || !locationPattern.MatchString(p[3]) {
			return pre + "get", nil, notFound("Location projects/%s/locations/%s not found.", project, p[3])
		}
		return pre + "get", location(project, p[3]), nil
	}
	if len(p) < 5 {
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	parent := strings.Join(p[:4], "/")
	switch p[4] {
	case "reportConfigs":
		switch {
		case len(p) == 5 && r.Method == http.MethodPost:
			c := &sipb.ReportConfig{}
			if err := decode(r, c); err != nil {
				return pre + "reportConfigs.create", nil, err
			}
			out, err := s.create(r.Context(), project, parent, c)
			return pre + "reportConfigs.create", out, err
		case len(p) == 5 && r.Method == http.MethodGet:
			out, err := s.list(parent, q)
			return pre + "reportConfigs.list", out, err
		case len(p) == 6:
			name := parent + "/reportConfigs/" + p[5]
			switch r.Method {
			case http.MethodGet:
				out, err := s.get(name)
				return pre + "reportConfigs.get", out, err
			case http.MethodPatch:
				c := &sipb.ReportConfig{}
				if err := decode(r, c); err != nil {
					return pre + "reportConfigs.patch", nil, err
				}
				out, err := s.patch(r.Context(), name, c, q.Get("updateMask"))
				return pre + "reportConfigs.patch", out, err
			case http.MethodDelete:
				return pre + "reportConfigs.delete", map[string]any{}, s.delete(name)
			}
		case len(p) >= 7 && p[6] == "reportDetails" && r.Method == http.MethodGet:
			name := parent + "/reportConfigs/" + p[5]
			if len(p) == 7 {
				out, err := s.details(name, q)
				return pre + "reportConfigs.reportDetails.list", out, err
			}
			out, err := s.detail(name, strings.Join(p[7:], "/"))
			return pre + "reportConfigs.reportDetails.get", out, err
		}
	case "datasetConfigs", "operations":
		return s.routeDatasets(r, p, parent)
	}
	return "", nil, notFound("%s not found", r.URL.Path)
}

// locationPattern is a Cloud Storage location's form: a multi-region, a
// dual-region or a region, lower case.
var locationPattern = regexp.MustCompile(`^[a-z]+(-[a-z]+[0-9]+)?[0-9]*$`)

// insightsLocations are the locations Google lists; every location of a
// bucket is accepted.
var insightsLocations = []string{"us", "eu", "asia", "us-central1", "us-east1", "us-west1", "europe-west1", "asia-east1"}

// unsupportedInsightsLocations are the regions Google documents inventory
// reports as not supported in.
var unsupportedInsightsLocations = map[string]bool{"africa-south1": true, "asia-southeast3": true, "europe-north2": true,
	"europe-west10": true, "europe-west12": true, "me-central1": true, "me-central2": true, "northamerica-south1": true,
	"eur5": true, "eur7": true, "eur8": true}

func (s *InsightsServer) validate(ctx context.Context, project, loc string, c *sipb.ReportConfig) error {
	if unsupportedInsightsLocations[loc] {
		return invalid("Inventory reports are not supported in location %s.", loc)
	}
	opts := c.GetObjectMetadataReportOptions()
	if opts == nil {
		return invalid("object_metadata_report_options is required.")
	}
	if len(opts.GetMetadataFields()) == 0 {
		return invalid("object_metadata_report_options.metadata_fields is required.")
	}
	known := map[string]bool{}
	for _, f := range inventoryFields {
		known[f] = true
	}
	have := map[string]bool{}
	for _, f := range opts.GetMetadataFields() {
		if !known[f] {
			return invalid("Unknown metadata field %q: the fields are %s.", f, strings.Join(inventoryFields, ", "))
		}
		if have[f] {
			return invalid("Metadata field %q is named twice.", f)
		}
		have[f] = true
	}
	for _, f := range []string{"project", "bucket", "name"} {
		if !have[f] {
			return invalid("metadata_fields must include project, bucket and name; %q is missing.", f)
		}
	}
	src := opts.GetStorageFilters().GetBucket()
	dst := opts.GetStorageDestinationOptions().GetBucket()
	if src == "" {
		return invalid("object_metadata_report_options.storage_filters.bucket is required.")
	}
	if dst == "" {
		return invalid("object_metadata_report_options.storage_destination_options.bucket is required.")
	}
	if strings.HasPrefix(opts.GetStorageDestinationOptions().GetDestinationPath(), "/") {
		return invalid("destination_path must not start with /.")
	}
	sb, err := s.rt.GCS.Bucket(ctx, src)
	if err != nil {
		if IsNotFound(err) {
			return invalid("Source bucket %s does not exist.", src)
		}
		return err
	}
	db, err := s.rt.GCS.Bucket(ctx, dst)
	if err != nil {
		if IsNotFound(err) {
			return invalid("Destination bucket %s does not exist.", dst)
		}
		return err
	}
	if sb.ProjectNumber != db.ProjectNumber {
		return invalid("Destination bucket %s must be in the same project as source bucket %s.", dst, src)
	}
	if !strings.EqualFold(sb.Location, db.Location) {
		return invalid("Destination bucket %s (%s) must be in the same location as source bucket %s (%s).", dst,
			strings.ToLower(db.Location), src, strings.ToLower(sb.Location))
	}
	if csvOpts := c.GetCsvOptions(); csvOpts != nil {
		if len([]rune(csvOpts.GetDelimiter())) > 1 {
			return invalid("csv_options.delimiter must be one character.")
		}
	}
	f := c.GetFrequencyOptions()
	if f == nil {
		return invalid("frequency_options is required.")
	}
	if f.GetFrequency() != sipb.FrequencyOptions_DAILY && f.GetFrequency() != sipb.FrequencyOptions_WEEKLY {
		return invalid("frequency_options.frequency must be DAILY or WEEKLY.")
	}
	if !validDate(f.GetStartDate()) {
		return invalid("frequency_options.start_date is required and must be a full date.")
	}
	if !validDate(f.GetEndDate()) {
		return invalid("frequency_options.end_date is required and must be a full date.")
	}
	if dateTime(f.GetEndDate()).Before(dateTime(f.GetStartDate())) {
		return invalid("frequency_options.end_date must not be before start_date.")
	}
	return nil
}

func validDate(d *date.Date) bool {
	return d != nil && d.Year > 0 && d.Month >= 1 && d.Month <= 12 && d.Day >= 1 && d.Day <= 31
}

func dateTime(d *date.Date) time.Time {
	return time.Date(int(d.Year), time.Month(d.Month), int(d.Day), 0, 0, 0, 0, time.UTC)
}

func (s *InsightsServer) create(ctx context.Context, project, parent string, c *sipb.ReportConfig) (any, error) {
	loc := strings.TrimPrefix(parent, "projects/"+project+"/locations/")
	if c.ReportFormat == nil {
		c.ReportFormat = &sipb.ReportConfig_CsvOptions{CsvOptions: &sipb.CSVOptions{}}
	}
	if err := s.validate(ctx, project, loc, c); err != nil {
		return nil, err
	}
	src := c.GetObjectMetadataReportOptions().GetStorageFilters().GetBucket()
	s.mu.Lock()
	n := 0
	for _, o := range s.configs {
		if o.Config.GetObjectMetadataReportOptions().GetStorageFilters().GetBucket() == src {
			n++
		}
	}
	if n >= MaxReportConfigsPerBucket {
		s.mu.Unlock()
		return nil, errorf(http.StatusTooManyRequests, "RESOURCE_EXHAUSTED",
			"Bucket %s already has %d inventory report configurations, the most a bucket can have.", src, MaxReportConfigsPerBucket)
	}
	now := s.rt.Clock.Now()
	cc := proto.Clone(c).(*sipb.ReportConfig)
	cc.Name = parent + "/reportConfigs/" + newUUID()
	cc.CreateTime = timestamppb.New(now)
	cc.UpdateTime = timestamppb.New(now)
	s.configs[cc.Name] = &reportConfig{Project: project, Config: cc, Written: map[string]bool{}}
	s.saveLocked()
	s.mu.Unlock()
	// Today's occurrence, when it is due, is written right away.
	go s.writeDue(context.Background(), s.rt.Clock.Now())
	return proto.Clone(cc), nil
}

func (s *InsightsServer) get(name string) (*sipb.ReportConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.configs[name]
	if !ok {
		return nil, notFound("ReportConfig %s not found.", name)
	}
	return proto.Clone(c.Config).(*sipb.ReportConfig), nil
}

func (s *InsightsServer) list(parent string, q url.Values) (any, error) {
	s.mu.Lock()
	var out []*sipb.ReportConfig
	for _, c := range s.configs {
		if strings.HasPrefix(c.Config.Name, parent+"/") || (strings.HasSuffix(parent, "/locations/-") &&
			strings.HasPrefix(c.Config.Name, strings.TrimSuffix(parent, "-"))) {
			out = append(out, proto.Clone(c.Config).(*sipb.ReportConfig))
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, k int) bool {
		a, b := out[i].CreateTime.AsTime(), out[k].CreateTime.AsTime()
		if a.Equal(b) {
			return out[i].Name < out[k].Name
		}
		return a.Before(b)
	})
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(out, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	return &sipb.ListReportConfigsResponse{ReportConfigs: page, NextPageToken: next}, nil
}

func (s *InsightsServer) patch(ctx context.Context, name string, in *sipb.ReportConfig, mask string) (any, error) {
	cur, err := s.get(name)
	if err != nil {
		return nil, err
	}
	if mask == "" {
		return nil, invalid("update_mask is required.")
	}
	next := proto.Clone(cur).(*sipb.ReportConfig)
	paths := strings.Split(mask, ",")
	for _, p := range paths {
		switch strings.TrimSpace(p) {
		case "name", "createTime", "create_time", "updateTime", "update_time":
			return nil, invalid("Field %s cannot be updated.", p)
		}
	}
	if err := applyMask(next, in, paths); err != nil {
		return nil, err
	}
	loc := strings.Split(name, "/")[3]
	if err := s.validate(ctx, strings.Split(name, "/")[1], loc, next); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.rt.Clock.Now())
	s.mu.Lock()
	c, ok := s.configs[name]
	if !ok {
		s.mu.Unlock()
		return nil, notFound("ReportConfig %s not found.", name)
	}
	c.Config = next
	s.saveLocked()
	s.mu.Unlock()
	return proto.Clone(next), nil
}

func (s *InsightsServer) delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.configs[name]; !ok {
		return notFound("ReportConfig %s not found.", name)
	}
	delete(s.configs, name)
	s.saveLocked()
	return nil
}

func (s *InsightsServer) details(name string, q url.Values) (any, error) {
	s.mu.Lock()
	c, ok := s.configs[name]
	var out []*sipb.ReportDetail
	if ok {
		for _, d := range c.Details {
			out = append(out, proto.Clone(d).(*sipb.ReportDetail))
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound("ReportConfig %s not found.", name)
	}
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(out, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	return &sipb.ListReportDetailsResponse{ReportDetails: page, NextPageToken: next}, nil
}

func (s *InsightsServer) detail(config, id string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.configs[config]; ok {
		for _, d := range c.Details {
			if d.Name == config+"/reportDetails/"+id {
				return proto.Clone(d), nil
			}
		}
	}
	return nil, notFound("ReportDetail %s/reportDetails/%s not found.", config, id)
}

// occurrences are a configuration's report times due by now and not yet
// written.
func occurrences(c *reportConfig, now time.Time) []time.Time {
	f := c.Config.GetFrequencyOptions()
	if f == nil || !validDate(f.GetStartDate()) {
		return nil
	}
	step := 24 * time.Hour
	if f.GetFrequency() == sipb.FrequencyOptions_WEEKLY {
		step = 7 * 24 * time.Hour
	}
	created := c.Config.GetCreateTime().AsTime().UTC().Truncate(24 * time.Hour)
	var out []time.Time
	for t := dateTime(f.GetStartDate()); !t.After(now); t = t.Add(step) {
		if validDate(f.GetEndDate()) && t.After(dateTime(f.GetEndDate())) {
			break
		}
		if t.Before(created) || c.Written[t.Format(time.RFC3339)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// writeDue writes every report now due.
func (s *InsightsServer) writeDue(ctx context.Context, now time.Time) {
	s.gen.Lock()
	defer s.gen.Unlock()
	type job struct {
		name string
		cfg  *sipb.ReportConfig
		at   time.Time
	}
	var jobs []job
	s.mu.Lock()
	for name, c := range s.configs {
		for _, t := range occurrences(c, now) {
			jobs = append(jobs, job{name, proto.Clone(c.Config).(*sipb.ReportConfig), t})
		}
	}
	s.mu.Unlock()
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].at.Before(jobs[k].at) })
	for _, j := range jobs {
		d := s.write(ctx, j.cfg, j.at)
		s.mu.Lock()
		if c, ok := s.configs[j.name]; ok {
			c.Written[j.at.Format(time.RFC3339)] = true
			c.Details = append(c.Details, d)
			s.saveLocked()
		}
		s.mu.Unlock()
	}
}

// write writes one report and its manifest, and returns its detail.
func (s *InsightsServer) write(ctx context.Context, c *sipb.ReportConfig, at time.Time) *sipb.ReportDetail {
	opts := c.GetObjectMetadataReportOptions()
	src := opts.GetStorageFilters().GetBucket()
	dst := opts.GetStorageDestinationOptions().GetBucket()
	path := strings.TrimSuffix(opts.GetStorageDestinationOptions().GetDestinationPath(), "/")
	id := c.Name[strings.LastIndex(c.Name, "/")+1:]
	stamp := at.Format("2006-01-02T15:04")
	base := id + "_" + stamp
	if path != "" {
		base = path + "/" + base
	}
	snapshot := s.rt.Clock.Now()
	d := &sipb.ReportDetail{
		Name:             c.Name + "/reportDetails/" + newUUID(),
		SnapshotTime:     timestamppb.New(snapshot),
		ReportPathPrefix: "gs://" + dst + "/" + base,
		TargetDatetime: &datetime.DateTime{Year: int32(at.Year()), Month: int32(at.Month()), Day: int32(at.Day()),
			Hours: int32(at.Hour()), Minutes: int32(at.Minute()), TimeOffset: &datetime.DateTime_UtcOffset{UtcOffset: durationpb.New(0)}},
		Labels:        c.GetLabels(),
		ReportMetrics: &sipb.ReportDetail_Metrics{},
		Status:        &spb.Status{},
	}
	fail := func(err error) *sipb.ReportDetail {
		d.Status = &spb.Status{Code: statusOf(err), Message: err.Error()}
		return d
	}
	b, err := s.rt.GCS.Bucket(ctx, src)
	if err != nil {
		return fail(err)
	}
	objs, err := s.rt.GCS.List(ctx, src, "", false)
	if err != nil {
		return fail(err)
	}
	project := strings.Split(c.Name, "/")[1]
	rows := make([][]string, 0, len(objs))
	for _, o := range objs {
		rows = append(rows, inventoryRow(opts.GetMetadataFields(), project, b, o))
	}
	var data []byte
	ext, ctype := ".csv", "text/csv"
	if c.GetParquetOptions() != nil {
		ext, ctype = ".parquet", "application/vnd.apache.parquet"
		data, err = parquetReport(opts.GetMetadataFields(), rows)
	} else {
		data, err = csvReport(c.GetCsvOptions(), opts.GetMetadataFields(), rows)
	}
	if err != nil {
		return fail(err)
	}
	shard := base + "_0" + ext
	if _, err := s.rt.GCS.Write(ctx, dst, map[string]any{"name": shard, "contentType": ctype}, bytes.NewReader(data)); err != nil {
		return fail(err)
	}
	cfgJSON := json.RawMessage(protoJSON(c))
	manifest, _ := json.MarshalIndent(map[string]any{
		"report_config":            cfgJSON,
		"records_processed":        len(rows),
		"snapshot_time":            snapshot.Format(time.RFC3339),
		"target_datetime":          at.Format(time.RFC3339),
		"shard_count":              1,
		"report_shards_file_names": []string{shard[strings.LastIndex(shard, "/")+1:]},
	}, "", "  ")
	if _, err := s.rt.GCS.Write(ctx, dst, map[string]any{"name": base + "_manifest.json", "contentType": "application/json"},
		bytes.NewReader(manifest)); err != nil {
		return fail(err)
	}
	d.ShardsCount = 1
	d.ReportMetrics.ProcessedRecordsCount = int64(len(rows))
	return d
}

func inventoryRow(fields []string, project string, b Bucket, o Object) []string {
	row := make([]string, len(fields))
	for i, f := range fields {
		switch f {
		case "project":
			row[i] = project
		case "bucket":
			row[i] = o.Bucket
		case "name":
			row[i] = o.Name
		case "location":
			row[i] = strings.ToLower(b.Location)
		case "size":
			row[i] = o.Size
		case "timeCreated":
			row[i] = o.TimeCreated
		case "timeDeleted":
			row[i] = o.TimeDeleted
		case "updated":
			row[i] = o.Updated
		case "storageClass":
			row[i] = o.StorageClass
		case "etag":
			row[i] = o.Etag
		case "retentionExpirationTime":
			row[i] = o.RetentionExpirationTime
		case "crc32c":
			row[i] = o.Crc32c
		case "md5Hash":
			row[i] = o.MD5Hash
		case "generation":
			row[i] = o.Generation
		case "metageneration":
			row[i] = o.Metageneration
		case "contentType":
			row[i] = o.ContentType
		case "contentEncoding":
			row[i] = o.ContentEncoding
		case "timeStorageClassUpdated":
			row[i] = o.TimeStorageClassUpdated
		}
	}
	return row
}

func csvReport(o *sipb.CSVOptions, fields []string, rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if d := []rune(o.GetDelimiter()); len(d) == 1 {
		w.Comma = d[0]
	}
	if o.GetHeaderRequired() {
		if err := w.Write(fields); err != nil {
			return nil, err
		}
	}
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if sep := o.GetRecordSeparator(); sep != "" && sep != "\n" {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte(sep))
	}
	return out, nil
}

// intFields are the numeric columns of a Parquet report.
var intFields = map[string]bool{"size": true, "generation": true, "metageneration": true}

func parquetReport(fields []string, rows [][]string) ([]byte, error) {
	var cols []arrow.Field
	for _, f := range fields {
		t := arrow.DataType(arrow.BinaryTypes.String)
		if intFields[f] {
			t = arrow.PrimitiveTypes.Int64
		}
		cols = append(cols, arrow.Field{Name: f, Type: t, Nullable: true})
	}
	schema := arrow.NewSchema(cols, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	for _, r := range rows {
		for i, f := range fields {
			v := r[i]
			if intFields[f] {
				ib := b.Field(i).(*array.Int64Builder)
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					ib.AppendNull()
				} else {
					ib.Append(n)
				}
				continue
			}
			sb := b.Field(i).(*array.StringBuilder)
			if v == "" {
				sb.AppendNull()
			} else {
				sb.Append(v)
			}
		}
	}
	rec := b.NewRecordBatch()
	defer rec.Release()
	var buf bytes.Buffer
	w, err := pqarrow.NewFileWriter(schema, &buf, parquet.NewWriterProperties(), pqarrow.DefaultWriterProps())
	if err != nil {
		return nil, err
	}
	if err := w.Write(rec); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("writing the Parquet report: %w", err)
	}
	return buf.Bytes(), nil
}
