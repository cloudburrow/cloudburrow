// Package datatransfer implements the BigQuery Data Transfer Service
// (google.cloud.bigquery.datatransfer.v1) for scheduled queries and dataset
// copies (#1948).
//
// Google publishes no emulator for it, so it is built: transfer configs for
// the scheduled_query and cross_region_copy data sources, their runs and run
// logs, manual runs and backfills, on the schedule syntax BigQuery documents,
// driven by the instance's clock. A run really executes: a scheduled query
// is a BigQuery query job with @run_time and @run_date and a templated
// destination table, a dataset copy copies every table, both through the
// Executor the CLI wires to the local BigQuery emulator.
package datatransfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	dtpb "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Clock is the instance's clock: the BigQuery emulator's, which its
// clock:advance extension moves, so a test advances one clock for both.
type Clock interface{ Now() time.Time }

// QueryJob is one scheduled query run as a BigQuery query job.
type QueryJob struct {
	Project, Location, Query string
	// DestDataset and DestTable are empty for a query with no destination
	// (DDL, DML or a script).
	DestDataset, DestTable, WriteDisposition, PartitioningField string
	// RunTime is @run_time; @run_date is its UTC date.
	RunTime time.Time
}

// CopyJob copies every table of a source dataset into a destination one.
type CopyJob struct {
	Project, DestDataset, SourceProject, SourceDataset string
	Overwrite                                          bool
}

// Executor runs the BigQuery work of a run. The CLI implements it with the
// official BigQuery client against the local emulator.
type Executor interface {
	// DatasetLocation is a dataset's location, or a NOT_FOUND error.
	DatasetLocation(ctx context.Context, project, dataset string) (string, error)
	// RunQuery runs a query job to completion and returns its job ID and
	// the rows it wrote to the destination table.
	RunQuery(ctx context.Context, job QueryJob) (jobID string, rows int64, err error)
	// CopyTables copies the tables and returns the IDs it copied.
	CopyTables(ctx context.Context, job CopyJob) (copied []string, err error)
}

// Publisher publishes a run notification to the local Pub/Sub.
type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error
}

// Email is a failure email a run would send. Nothing leaves the machine:
// it is recorded for the console instead.
type Email struct {
	Time    time.Time `json:"time"`
	Config  string    `json:"config"`
	Run     string    `json:"run"`
	Subject string    `json:"subject"`
}

// record is one transfer config with its runs, kept as one store entry.
type record struct {
	Config *dtpb.TransferConfig
	// Anchor is where an interval schedule counts from.
	Anchor time.Time
	Runs   []*runRecord
}

type runRecord struct {
	Run  *dtpb.TransferRun
	Logs []*dtpb.TransferMessage
}

// stored is record's JSON form, with protos in protojson.
type stored struct {
	Config json.RawMessage `json:"config"`
	Anchor time.Time       `json:"anchor"`
	Runs   []storedRun     `json:"runs,omitempty"`
}

type storedRun struct {
	Run  json.RawMessage   `json:"run"`
	Logs []json.RawMessage `json:"logs,omitempty"`
}

// Server serves google.cloud.bigquery.datatransfer.v1.DataTransferService.
type Server struct {
	dtpb.UnimplementedDataTransferServiceServer

	clock Clock
	exec  Executor
	pub   Publisher
	db    store.Store

	mu       sync.Mutex
	configs  map[string]*record // by config name
	enrolled map[string]map[string]bool
	emails   []Email
	wake     chan struct{}
}

// NewServer returns the service over db. exec and pub may be nil: a run
// then fails, and a notification is logged as undelivered.
func NewServer(db store.Store, clock Clock, exec Executor, pub Publisher) (*Server, error) {
	s := &Server{clock: clock, exec: exec, pub: pub, db: db, configs: map[string]*record{},
		enrolled: map[string]map[string]bool{}, wake: make(chan struct{}, 1)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Register adds the service to a gRPC server, or to the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) { dtpb.RegisterDataTransferServiceServer(g, s) }

const configPrefix = "transferConfigs/"

func (s *Server) load() error {
	keys, err := s.db.List(configPrefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		b, err := s.db.Get(k)
		if err != nil {
			return err
		}
		var st stored
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("transfer config %s: %w", k, err)
		}
		rec := &record{Config: &dtpb.TransferConfig{}, Anchor: st.Anchor}
		if err := protojson.Unmarshal(st.Config, rec.Config); err != nil {
			return fmt.Errorf("transfer config %s: %w", k, err)
		}
		for _, sr := range st.Runs {
			rr := &runRecord{Run: &dtpb.TransferRun{}}
			if err := protojson.Unmarshal(sr.Run, rr.Run); err != nil {
				return err
			}
			for _, l := range sr.Logs {
				m := &dtpb.TransferMessage{}
				if err := protojson.Unmarshal(l, m); err != nil {
					return err
				}
				rr.Logs = append(rr.Logs, m)
			}
			rec.Runs = append(rec.Runs, rr)
		}
		s.configs[rec.Config.GetName()] = rec
	}
	if b, err := s.db.Get("enrolled"); err == nil {
		_ = json.Unmarshal(b, &s.enrolled)
	}
	return nil
}

// save writes one record; the caller holds mu.
func (s *Server) save(rec *record) error {
	cfg, err := protojson.Marshal(rec.Config)
	if err != nil {
		return err
	}
	st := stored{Config: cfg, Anchor: rec.Anchor}
	for _, rr := range rec.Runs {
		r, err := protojson.Marshal(rr.Run)
		if err != nil {
			return err
		}
		sr := storedRun{Run: r}
		for _, l := range rr.Logs {
			b, err := protojson.Marshal(l)
			if err != nil {
				return err
			}
			sr.Logs = append(sr.Logs, b)
		}
		st.Runs = append(st.Runs, sr)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.db.Put(storeKey(rec.Config.GetName()), b)
}

func storeKey(name string) string { return configPrefix + strings.ReplaceAll(name, "/", "~") }

// Reset forgets every config of project, or of every project for "".
func (s *Server) Reset(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.configs {
		if project == "" || strings.HasPrefix(name, "projects/"+project+"/") {
			if err := s.db.Delete(storeKey(name)); err != nil {
				return err
			}
			delete(s.configs, name)
		}
	}
	if project == "" {
		s.enrolled = map[string]map[string]bool{}
		s.emails = nil
	}
	return nil
}

// Emails is every failure email recorded, newest last.
func (s *Server) Emails() []Email {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Email(nil), s.emails...)
}

// Now is the instance's time.
func (s *Server) Now() time.Time { return s.clock.Now().UTC() }

// --- names ---

var (
	projectPart = `(?:[a-z][a-z0-9-]{4,28}[a-z0-9]|[0-9]+)`
	parentRE    = regexp.MustCompile(`^projects/(` + projectPart + `)(?:/locations/([a-z0-9-]+))?$`)
	configRE    = regexp.MustCompile(`^projects/(` + projectPart + `)(?:/locations/([a-z0-9-]+))?/transferConfigs/([A-Za-z0-9_-]+)$`)
	runRE       = regexp.MustCompile(`^(projects/` + projectPart + `(?:/locations/[a-z0-9-]+)?/transferConfigs/[A-Za-z0-9_-]+)/runs/([A-Za-z0-9_-]+)$`)
	dataSrcRE   = regexp.MustCompile(`^(projects/` + projectPart + `(?:/locations/[a-z0-9-]+)?)/dataSources/([a-z_]+)$`)
	topicRE     = regexp.MustCompile(`^projects/` + projectPart + `/topics/[A-Za-z][A-Za-z0-9._~%+-]{2,254}$`)
	datasetRE   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[0:8] + "-0000-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// findConfig resolves a config name, with or without its location, as
// Google answers both forms.
func (s *Server) findConfig(name string) (*record, error) {
	m := configRE.FindStringSubmatch(name)
	if m == nil {
		return nil, apierror.InvalidArgument("Invalid resource name %q: expected projects/{project}/locations/{location}/transferConfigs/{config}", name)
	}
	if rec, ok := s.configs[name]; ok {
		return rec, nil
	}
	if m[2] == "" {
		for n, rec := range s.configs {
			if strings.HasPrefix(n, "projects/"+m[1]+"/") && strings.HasSuffix(n, "/transferConfigs/"+m[3]) {
				return rec, nil
			}
		}
	}
	return nil, apierror.NotFound("Requested entity was not found.")
}

func (s *Server) findRun(name string) (*record, *runRecord, error) {
	m := runRE.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, apierror.InvalidArgument("Invalid resource name %q: expected projects/{project}/locations/{location}/transferConfigs/{config}/runs/{run}", name)
	}
	rec, err := s.findConfig(m[1])
	if err != nil {
		return nil, nil, err
	}
	for _, rr := range rec.Runs {
		if strings.HasSuffix(rr.Run.GetName(), "/runs/"+m[2]) {
			return rec, rr, nil
		}
	}
	return nil, nil, apierror.NotFound("Requested entity was not found.")
}

// --- data sources ---

func checkParent(parent string) ([]string, error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return nil, apierror.InvalidArgument("Invalid parent %q: expected projects/{project} or projects/{project}/locations/{location}", parent)
	}
	return m, nil
}

// GetDataSource answers scheduled_query and cross_region_copy.
func (s *Server) GetDataSource(_ context.Context, req *dtpb.GetDataSourceRequest) (*dtpb.DataSource, error) {
	m := dataSrcRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("Invalid resource name %q: expected projects/{project}/locations/{location}/dataSources/{data_source}", req.GetName()))
	}
	if _, ok := dataSources[m[2]]; !ok {
		return nil, apierror.Wrap(apierror.NotFound("Data source with id %s not found.", m[2]))
	}
	return dataSource(m[1], m[2]), nil
}

// ListDataSources lists the two data sources.
func (s *Server) ListDataSources(_ context.Context, req *dtpb.ListDataSourcesRequest) (*dtpb.ListDataSourcesResponse, error) {
	if _, err := checkParent(req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := paging.Page("dts-sources:"+req.GetParent(), dataSourceIDs, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &dtpb.ListDataSourcesResponse{NextPageToken: next}
	for _, id := range page {
		resp.DataSources = append(resp.DataSources, dataSource(req.GetParent(), id))
	}
	return resp, nil
}

// EnrollDataSources records the data sources a project enrolled in, which
// the console's transfer list shows.
func (s *Server) EnrollDataSources(_ context.Context, req *dtpb.EnrollDataSourcesRequest) (*emptypb.Empty, error) {
	return s.enroll(req.GetName(), req.GetDataSourceIds(), true)
}

// UnenrollDataSources reverses EnrollDataSources.
func (s *Server) UnenrollDataSources(_ context.Context, req *dtpb.UnenrollDataSourcesRequest) (*emptypb.Empty, error) {
	return s.enroll(req.GetName(), req.GetDataSourceIds(), false)
}

func (s *Server) enroll(name string, ids []string, on bool) (*emptypb.Empty, error) {
	m, err := checkParent(name)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	for _, id := range ids {
		if _, ok := dataSources[id]; !ok {
			return nil, apierror.Wrap(apierror.InvalidArgument("Data source with id %s not found.", id))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.enrolled[m[1]]
	if set == nil {
		set = map[string]bool{}
		s.enrolled[m[1]] = set
	}
	for _, id := range ids {
		if on {
			set[id] = true
		} else {
			delete(set, id)
		}
	}
	b, _ := json.Marshal(s.enrolled)
	return &emptypb.Empty{}, apierror.Wrap(s.db.Put("enrolled", b))
}

// Enrolled is the data sources project enrolled in, sorted.
func (s *Server) Enrolled(project string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.enrolled[project] {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// --- transfer configs ---

func param(p *structpb.Struct, key string) string {
	v, ok := p.GetFields()[key]
	if !ok {
		return ""
	}
	switch k := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return k.StringValue
	case *structpb.Value_BoolValue:
		if k.BoolValue {
			return "true"
		}
		return "false"
	case *structpb.Value_NumberValue:
		return fmt.Sprint(k.NumberValue)
	}
	return ""
}

// validate checks a config as Google does for its data source, returning
// the parsed schedule (nil when auto scheduling is off or none applies).
func (s *Server) validate(ctx context.Context, project string, c *dtpb.TransferConfig) (*Schedule, error) {
	src, ok := dataSources[c.GetDataSourceId()]
	if !ok {
		if c.GetDataSourceId() == "" {
			return nil, apierror.InvalidArgument("Data source id is required.")
		}
		return nil, apierror.InvalidArgument("Data source with id %s not found.", c.GetDataSourceId())
	}
	if c.GetScheduleOptionsV2() != nil {
		return nil, apierror.Unimplemented("schedule_options_v2 is not supported; use schedule and schedule_options")
	}
	params := c.GetParams()
	known := map[string]bool{}
	for _, p := range src.GetParameters() {
		known[p.GetParamId()] = true
		if p.GetRequired() && param(params, p.GetParamId()) == "" {
			return nil, apierror.InvalidArgument("Missing required parameter: %s", p.GetParamId())
		}
	}
	for k := range params.GetFields() {
		if !known[k] {
			return nil, apierror.InvalidArgument("Invalid parameter: %s is not a parameter of data source %s", k, c.GetDataSourceId())
		}
	}
	dest := c.GetDestinationDatasetId()
	if dest != "" && (!datasetRE.MatchString(dest) || len(dest) > 1024) {
		return nil, apierror.InvalidArgument("Invalid dataset ID %q. Dataset IDs must be alphanumeric (plus underscores) and must be at most 1024 characters long.", dest)
	}
	switch c.GetDataSourceId() {
	case ScheduledQuery:
		wd := param(params, "write_disposition")
		if wd != "" && wd != "WRITE_TRUNCATE" && wd != "WRITE_APPEND" {
			return nil, apierror.InvalidArgument("Invalid value for write_disposition: %s. Expected WRITE_TRUNCATE or WRITE_APPEND.", wd)
		}
		tmpl := param(params, "destination_table_name_template")
		if dest != "" && tmpl == "" {
			return nil, apierror.InvalidArgument("Missing required parameter: destination_table_name_template")
		}
		if dest == "" && tmpl != "" {
			return nil, apierror.InvalidArgument("destination_table_name_template is set but the transfer config has no destination dataset")
		}
		if tmpl != "" {
			if _, err := RenderTableTemplate(tmpl, time.Unix(0, 0)); err != nil {
				return nil, apierror.InvalidArgument("%v", err)
			}
		}
	case CrossRegionCopy:
		if dest == "" {
			return nil, apierror.InvalidArgument("Destination dataset id is required.")
		}
		if v := param(params, "overwrite_destination_table"); v != "" && v != "true" && v != "false" {
			return nil, apierror.InvalidArgument("Invalid value for overwrite_destination_table: %s", v)
		}
		if param(params, "source_project_id") == project && param(params, "source_dataset_id") == dest {
			return nil, apierror.InvalidArgument("The source and destination datasets must be different.")
		}
	}
	if t := c.GetNotificationPubsubTopic(); t != "" && !topicRE.MatchString(t) {
		return nil, apierror.InvalidArgument("Invalid Pub/Sub topic %q: expected projects/{project}/topics/{topic}", t)
	}
	if o := c.GetScheduleOptions(); o != nil && o.GetStartTime() != nil && o.GetEndTime() != nil &&
		!o.GetEndTime().AsTime().After(o.GetStartTime().AsTime()) {
		return nil, apierror.InvalidArgument("The end time of the schedule must be after its start time.")
	}
	text := c.GetSchedule()
	if text == "" {
		text = src.GetDefaultSchedule()
	}
	sch, err := ParseSchedule(text)
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if min := src.GetMinimumScheduleInterval().AsDuration(); sch.MinimumInterval() < min {
		return nil, apierror.InvalidArgument("Invalid schedule: %q. The minimum interval between runs of data source %s is %s.",
			text, c.GetDataSourceId(), minuteText(min))
	}
	return sch, nil
}

func minuteText(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// nextRun sets next_run_time from now; the caller holds mu.
func (s *Server) nextRun(rec *record, now time.Time) {
	c := rec.Config
	c.NextRunTime = nil
	if c.GetDisabled() || c.GetScheduleOptions().GetDisableAutoScheduling() {
		return
	}
	text := c.GetSchedule()
	if text == "" {
		text = defaultSchedule
	}
	sch, err := ParseSchedule(text)
	if err != nil {
		return
	}
	after := now
	if st := c.GetScheduleOptions().GetStartTime(); st != nil && st.AsTime().After(after) {
		after = st.AsTime().Add(-time.Nanosecond)
	}
	next := sch.Next(after, rec.Anchor)
	if et := c.GetScheduleOptions().GetEndTime(); et != nil && next.After(et.AsTime()) {
		return
	}
	if !next.IsZero() {
		c.NextRunTime = timestamppb.New(next)
	}
}

// CreateTransferConfig validates and stores a config, and schedules it.
func (s *Server) CreateTransferConfig(ctx context.Context, req *dtpb.CreateTransferConfigRequest) (*dtpb.TransferConfig, error) {
	m, err := checkParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetTransferConfig() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("Transfer config is required."))
	}
	c := proto.Clone(req.GetTransferConfig()).(*dtpb.TransferConfig)
	project := m[1]
	if _, err := s.validate(ctx, project, c); err != nil {
		return nil, apierror.Wrap(err)
	}
	location := m[2]
	if dest := c.GetDestinationDatasetId(); dest != "" {
		loc, err := s.datasetLocation(ctx, project, dest)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if location != "" && !strings.EqualFold(location, loc) {
			return nil, apierror.Wrap(apierror.InvalidArgument("Cannot create a transfer in %s when the destination dataset is located in %s", location, loc))
		}
		location = strings.ToLower(loc)
		c.DatasetRegion = location
	}
	if c.GetDataSourceId() == CrossRegionCopy {
		if _, err := s.datasetLocation(ctx, param(c.GetParams(), "source_project_id"), param(c.GetParams(), "source_dataset_id")); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	if location == "" {
		location = "us"
	}
	now := s.Now()
	c.Name = "projects/" + project + "/locations/" + location + "/transferConfigs/" + newID()
	c.UpdateTime = timestamppb.New(now)
	c.State = dtpb.TransferState_TRANSFER_STATE_UNSPECIFIED
	c.UserId = 0
	c.Error = nil
	if sa := req.GetServiceAccountName(); sa != "" {
		c.OwnerInfo = &dtpb.UserInfo{Email: proto.String(sa)}
	} else {
		c.OwnerInfo = nil
	}
	rec := &record{Config: c, Anchor: now}
	if st := c.GetScheduleOptions().GetStartTime(); st != nil {
		rec.Anchor = st.AsTime()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun(rec, now)
	s.configs[c.Name] = rec
	if err := s.save(rec); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.notify()
	return proto.Clone(c).(*dtpb.TransferConfig), nil
}

func (s *Server) datasetLocation(ctx context.Context, project, dataset string) (string, error) {
	if s.exec == nil {
		return "", apierror.FailedPrecondition("BigQuery is not running on this instance")
	}
	loc, err := s.exec.DatasetLocation(ctx, project, dataset)
	if err != nil {
		if e := apierror.From(err); e != nil && e.Code == codes.NotFound {
			return "", apierror.NotFound("Not found: Dataset %s:%s", project, dataset)
		}
		return "", err
	}
	return loc, nil
}

// GetTransferConfig answers a config.
func (s *Server) GetTransferConfig(_ context.Context, req *dtpb.GetTransferConfigRequest) (*dtpb.TransferConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.findConfig(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return proto.Clone(rec.Config).(*dtpb.TransferConfig), nil
}

// ListTransferConfigs lists a project's configs, in one location or all
// when the parent names none, optionally of some data sources.
func (s *Server) ListTransferConfigs(_ context.Context, req *dtpb.ListTransferConfigsRequest) (*dtpb.ListTransferConfigsResponse, error) {
	m, err := checkParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	ids := map[string]bool{}
	for _, id := range req.GetDataSourceIds() {
		ids[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	prefix := "projects/" + m[1] + "/locations/"
	if m[2] != "" {
		prefix += m[2] + "/"
	}
	for name, rec := range s.configs {
		if strings.HasPrefix(name, prefix) && (len(ids) == 0 || ids[rec.Config.GetDataSourceId()]) {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	scope := "dts-configs:" + req.GetParent() + ":" + strings.Join(req.GetDataSourceIds(), ",")
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &dtpb.ListTransferConfigsResponse{NextPageToken: next}
	for _, k := range page {
		resp.TransferConfigs = append(resp.TransferConfigs, proto.Clone(s.configs[k].Config).(*dtpb.TransferConfig))
	}
	return resp, nil
}

// snake turns a JSON field name in an update mask into the proto's.
func snake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			b.WriteRune(r + 'a' - 'A')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// UpdateTransferConfig applies the fields update_mask names.
func (s *Server) UpdateTransferConfig(ctx context.Context, req *dtpb.UpdateTransferConfigRequest) (*dtpb.TransferConfig, error) {
	in := req.GetTransferConfig()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("Transfer config is required."))
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("Update mask is required."))
	}
	s.mu.Lock()
	rec, err := s.findConfig(in.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, apierror.Wrap(err)
	}
	c := proto.Clone(rec.Config).(*dtpb.TransferConfig)
	s.mu.Unlock()
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch snake(p) {
		case "display_name":
			c.DisplayName = in.GetDisplayName()
		case "params":
			c.Params = in.GetParams()
		case "schedule":
			c.Schedule = in.GetSchedule()
		case "schedule_options":
			c.ScheduleOptions = in.GetScheduleOptions()
		case "schedule_options.disable_auto_scheduling", "schedule_options.start_time", "schedule_options.end_time":
			if c.ScheduleOptions == nil {
				c.ScheduleOptions = &dtpb.ScheduleOptions{}
			}
			o := in.GetScheduleOptions()
			switch snake(p) {
			case "schedule_options.disable_auto_scheduling":
				c.ScheduleOptions.DisableAutoScheduling = o.GetDisableAutoScheduling()
			case "schedule_options.start_time":
				c.ScheduleOptions.StartTime = o.GetStartTime()
			default:
				c.ScheduleOptions.EndTime = o.GetEndTime()
			}
		case "disabled":
			c.Disabled = in.GetDisabled()
		case "destination_dataset_id":
			c.Destination = in.GetDestination()
		case "notification_pubsub_topic":
			c.NotificationPubsubTopic = in.GetNotificationPubsubTopic()
		case "email_preferences":
			c.EmailPreferences = in.GetEmailPreferences()
		case "data_refresh_window_days":
			c.DataRefreshWindowDays = in.GetDataRefreshWindowDays()
		case "encryption_configuration":
			c.EncryptionConfiguration = in.GetEncryptionConfiguration()
		case "data_source_id":
			if in.GetDataSourceId() != c.GetDataSourceId() {
				return nil, apierror.Wrap(apierror.InvalidArgument("The data source of a transfer config cannot be changed."))
			}
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("Invalid update mask path: %s", p))
		}
	}
	project := configRE.FindStringSubmatch(c.GetName())[1]
	if _, err := s.validate(ctx, project, c); err != nil {
		return nil, apierror.Wrap(err)
	}
	if dest := c.GetDestinationDatasetId(); dest != "" && dest != rec.Config.GetDestinationDatasetId() {
		loc, err := s.datasetLocation(ctx, project, dest)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if !strings.EqualFold(loc, c.GetDatasetRegion()) {
			return nil, apierror.Wrap(apierror.InvalidArgument("Cannot move the transfer to a dataset in %s from %s", loc, c.GetDatasetRegion()))
		}
	}
	if sa := req.GetServiceAccountName(); sa != "" {
		c.OwnerInfo = &dtpb.UserInfo{Email: proto.String(sa)}
	}
	now := s.Now()
	c.UpdateTime = timestamppb.New(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.configs[c.GetName()]
	if !ok {
		return nil, apierror.Wrap(apierror.NotFound("Requested entity was not found."))
	}
	cur.Config = c
	if c.GetSchedule() != rec.Config.GetSchedule() || !proto.Equal(c.GetScheduleOptions(), rec.Config.GetScheduleOptions()) {
		cur.Anchor = now
		if st := c.GetScheduleOptions().GetStartTime(); st != nil {
			cur.Anchor = st.AsTime()
		}
	}
	s.nextRun(cur, now)
	if err := s.save(cur); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.notify()
	return proto.Clone(c).(*dtpb.TransferConfig), nil
}

// DeleteTransferConfig deletes a config and its runs.
func (s *Server) DeleteTransferConfig(_ context.Context, req *dtpb.DeleteTransferConfigRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.findConfig(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.db.Delete(storeKey(rec.Config.GetName())); err != nil {
		return nil, apierror.Wrap(err)
	}
	delete(s.configs, rec.Config.GetName())
	return &emptypb.Empty{}, nil
}

// --- runs ---

// addRun queues a PENDING run of rec for runTime; the caller holds mu.
func (s *Server) addRun(rec *record, runTime, scheduleTime time.Time) *dtpb.TransferRun {
	c := rec.Config
	now := s.Now()
	run := &dtpb.TransferRun{
		Name:         c.GetName() + "/runs/" + newID(),
		ScheduleTime: timestamppb.New(scheduleTime), RunTime: timestamppb.New(runTime),
		UpdateTime: timestamppb.New(now), Params: c.GetParams(), DataSourceId: c.GetDataSourceId(),
		State: dtpb.TransferState_PENDING, Schedule: c.GetSchedule(),
		NotificationPubsubTopic: c.GetNotificationPubsubTopic(), EmailPreferences: c.GetEmailPreferences(),
	}
	if d := c.GetDestinationDatasetId(); d != "" {
		run.Destination = &dtpb.TransferRun_DestinationDatasetId{DestinationDatasetId: d}
	}
	rec.Runs = append(rec.Runs, &runRecord{Run: run})
	c.State = dtpb.TransferState_PENDING
	return proto.Clone(run).(*dtpb.TransferRun)
}

// StartManualTransferRuns queues a run now for requested_run_time, or one
// per scheduled time in requested_time_range (a backfill: start included,
// end excluded).
func (s *Server) StartManualTransferRuns(_ context.Context, req *dtpb.StartManualTransferRunsRequest) (*dtpb.StartManualTransferRunsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.findConfig(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	now := s.Now()
	resp := &dtpb.StartManualTransferRunsResponse{}
	switch t := req.GetTime().(type) {
	case *dtpb.StartManualTransferRunsRequest_RequestedRunTime:
		rt := t.RequestedRunTime.AsTime()
		if rt.After(now) {
			return nil, apierror.Wrap(apierror.InvalidArgument("Requested run time must be in the past."))
		}
		resp.Runs = append(resp.Runs, s.addRun(rec, rt, now))
	case *dtpb.StartManualTransferRunsRequest_RequestedTimeRange:
		runs, err := s.backfill(rec, t.RequestedTimeRange.GetStartTime(), t.RequestedTimeRange.GetEndTime(), now, false)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		resp.Runs = runs
	default:
		resp.Runs = append(resp.Runs, s.addRun(rec, now, now))
	}
	if err := s.save(rec); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.notify()
	return resp, nil
}

// ScheduleTransferRuns is the deprecated backfill: a run per scheduled time
// in [start_time, end_time].
func (s *Server) ScheduleTransferRuns(_ context.Context, req *dtpb.ScheduleTransferRunsRequest) (*dtpb.ScheduleTransferRunsResponse, error) {
	if req.GetStartTime() == nil || req.GetEndTime() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("Both start_time and end_time are required."))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.findConfig(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	runs, err := s.backfill(rec, req.GetStartTime(), req.GetEndTime(), s.Now(), true)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.save(rec); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.notify()
	return &dtpb.ScheduleTransferRunsResponse{Runs: runs}, nil
}

func (s *Server) backfill(rec *record, start, end *timestamppb.Timestamp, now time.Time, inclusive bool) ([]*dtpb.TransferRun, error) {
	if start == nil || end == nil {
		return nil, apierror.InvalidArgument("Both start_time and end_time of the requested time range are required.")
	}
	st, et := start.AsTime(), end.AsTime()
	if et.Before(st) {
		return nil, apierror.InvalidArgument("The start time must be before the end time.")
	}
	if et.After(now) {
		return nil, apierror.InvalidArgument("The end time cannot be in the future.")
	}
	text := rec.Config.GetSchedule()
	if text == "" {
		text = defaultSchedule
	}
	sch, err := ParseSchedule(text)
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	var runs []*dtpb.TransferRun
	for t := sch.Next(st.Add(-time.Nanosecond), rec.Anchor); !t.IsZero() && (t.Before(et) || (inclusive && t.Equal(et))); t = sch.Next(t, rec.Anchor) {
		if len(runs) >= 1000 {
			return nil, apierror.InvalidArgument("The requested time range covers more than 1000 runs.")
		}
		runs = append(runs, s.addRun(rec, t, now))
	}
	return runs, nil
}

// GetTransferRun answers a run.
func (s *Server) GetTransferRun(_ context.Context, req *dtpb.GetTransferRunRequest) (*dtpb.TransferRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, rr, err := s.findRun(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return proto.Clone(rr.Run).(*dtpb.TransferRun), nil
}

// DeleteTransferRun deletes a run and its logs.
func (s *Server) DeleteTransferRun(_ context.Context, req *dtpb.DeleteTransferRunRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, rr, err := s.findRun(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	for i, r := range rec.Runs {
		if r == rr {
			rec.Runs = append(rec.Runs[:i], rec.Runs[i+1:]...)
			break
		}
	}
	return &emptypb.Empty{}, apierror.Wrap(s.save(rec))
}

// ListTransferRuns lists a config's runs, newest run time first.
func (s *Server) ListTransferRuns(_ context.Context, req *dtpb.ListTransferRunsRequest) (*dtpb.ListTransferRunsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.findConfig(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	states := map[dtpb.TransferState]bool{}
	for _, st := range req.GetStates() {
		states[st] = true
	}
	runs := append([]*runRecord(nil), rec.Runs...)
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i].Run, runs[j].Run
		if !a.GetRunTime().AsTime().Equal(b.GetRunTime().AsTime()) {
			return a.GetRunTime().AsTime().After(b.GetRunTime().AsTime())
		}
		return a.GetScheduleTime().AsTime().After(b.GetScheduleTime().AsTime())
	})
	latest := req.GetRunAttempt() == dtpb.ListTransferRunsRequest_LATEST
	seen := map[int64]bool{}
	byKey := map[string]*runRecord{}
	var keys []string
	for i, rr := range runs {
		if len(states) > 0 && !states[rr.Run.GetState()] {
			continue
		}
		rt := rr.Run.GetRunTime().AsTime().UnixNano()
		if latest && seen[rt] {
			continue
		}
		seen[rt] = true
		k := fmt.Sprintf("%08d", i)
		keys = append(keys, k)
		byKey[k] = rr
	}
	scope := fmt.Sprintf("dts-runs:%s:%v:%v", req.GetParent(), req.GetStates(), latest)
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &dtpb.ListTransferRunsResponse{NextPageToken: next}
	for _, k := range page {
		resp.TransferRuns = append(resp.TransferRuns, proto.Clone(byKey[k].Run).(*dtpb.TransferRun))
	}
	return resp, nil
}

// ListTransferLogs lists a run's messages, oldest first.
func (s *Server) ListTransferLogs(_ context.Context, req *dtpb.ListTransferLogsRequest) (*dtpb.ListTransferLogsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, rr, err := s.findRun(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	types := map[dtpb.TransferMessage_MessageSeverity]bool{}
	for _, t := range req.GetMessageTypes() {
		types[t] = true
	}
	var keys []string
	byKey := map[string]*dtpb.TransferMessage{}
	for i, m := range rr.Logs {
		if len(types) > 0 && !types[m.GetSeverity()] {
			continue
		}
		k := fmt.Sprintf("%08d", i)
		keys = append(keys, k)
		byKey[k] = m
	}
	scope := fmt.Sprintf("dts-logs:%s:%v", req.GetParent(), req.GetMessageTypes())
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &dtpb.ListTransferLogsResponse{NextPageToken: next}
	for _, k := range page {
		resp.TransferMessages = append(resp.TransferMessages, proto.Clone(byKey[k]).(*dtpb.TransferMessage))
	}
	return resp, nil
}
