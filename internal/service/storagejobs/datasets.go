package storagejobs

// Storage Insights dataset configurations (#1976,
// docs.cloud.google.com/storage/docs/insights/datasets), JSON:
//
//	POST   /v1/projects/P/locations/L/datasetConfigs?datasetConfigId=  body: DatasetConfig
//	GET    /v1/projects/P/locations/L/datasetConfigs[/C]
//	PATCH  /v1/projects/P/locations/L/datasetConfigs/C?updateMask=
//	DELETE /v1/projects/P/locations/L/datasetConfigs/C
//	POST   /v1/projects/P/locations/L/datasetConfigs/C:linkDataset
//	POST   /v1/projects/P/locations/L/datasetConfigs/C:unlinkDataset
//	GET|DELETE /v1/projects/P/locations/L/operations[/O], POST .../O:cancel
//
// Every change answers a long-running operation, done when it answers. A
// configuration takes a snapshot of its scope's project, bucket and object
// metadata when it is made and every 24 hours after on the instance clock
// (Google: the first within 24 to 48 hours, then daily), and keeps those
// of the last retention_period_days days. Its scope is its source projects,
// folders or organization (a cloud_storage_object_path file of
// projects/N or folders/N lines becomes the projects or folders it names),
// narrowed by organization_number, by bucket location and by bucket name
// or name regex, include or exclude, and, without
// include_newly_created_buckets, to the buckets that existed when it was
// made. A source project without Storage Intelligence is left out with an
// error_attributes_view row (code 4), as one outside organization_number
// is (code 1).
//
// linkDataset makes the linked dataset in the instance's BigQuery,
// <config id><uid with "_" for "-">, as Google's example names it, with
// Google's views (datasetschema.go), each over a table of its own that the
// front hides and keeps read-only; each snapshot refreshes the tables.
// unlinkDataset deletes it; a linked configuration cannot be deleted.
// The activity views are aggregated from the instance's Cloud Storage
// request log on each snapshot (activity.go, #1979).

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sipb "cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	spb "google.golang.org/genproto/googleapis/rpc/status"
)

// LinkedDatasets makes and fills linked datasets in the instance's
// BigQuery.
type LinkedDatasets interface {
	// CreateDataset makes a dataset; ErrDatasetExists when one has the ID.
	CreateDataset(ctx context.Context, project, dataset, location, description string) error
	// ReplaceTable makes table t anew with rows.
	ReplaceTable(ctx context.Context, project, dataset string, t LinkedTable, rows []map[string]any) error
	CreateView(ctx context.Context, project, dataset string, v LinkedView) error
	DeleteDataset(ctx context.Context, project, dataset string) error
}

// ErrDatasetExists is CreateDataset's error for a dataset ID in use.
var ErrDatasetExists = errors.New("the dataset already exists")

// ProjectInfo is one project of the instance, with its ancestry.
type ProjectInfo struct {
	ID, Name string
	Number   int64
	// Folders are the folder numbers above it, nearest first.
	Folders []string
	// Organization is its organization's number, "" without one.
	Organization string
}

// ProjectLister lists the instance's projects.
type ProjectLister interface {
	ListProjects(ctx context.Context) ([]ProjectInfo, error)
	// Project is one project by ID or number; a project ID the instance
	// has not seen yet is registered, as every service's first call does.
	Project(ctx context.Context, ref string) (ProjectInfo, error)
}

const (
	insightsMetadataType = "type.googleapis.com/google.cloud.storageinsights.v1.OperationMetadata"
	datasetConfigType    = "type.googleapis.com/google.cloud.storageinsights.v1.DatasetConfig"
	linkResponseType     = "type.googleapis.com/google.cloud.storageinsights.v1.LinkDatasetResponse"
	snapshotInterval     = 24 * time.Hour
	// MaxRetentionPeriodDays and MaxActivityRetentionPeriodDays are
	// Google's limits of a dataset's and its activity data's retention.
	MaxRetentionPeriodDays         = 90
	MaxActivityRetentionPeriodDays = 365
	// MaxDatasetScope is Google's limit of projects or folders in a scope.
	MaxDatasetScope = 10000
)

// linkedLocations are the locations Google lets a linked dataset be in.
var linkedLocations = map[string]bool{"us": true, "eu": true, "asia-south1": true, "asia-south2": true, "asia-southeast1": true,
	"europe-west1": true, "us-central1": true, "us-east1": true, "us-east4": true}

// datasetConfigID is Google's rule for a configuration's name: up to 128
// letters, numbers and underscores, starting with a letter.
var datasetConfigID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)

// snapshot is one day's rows, by table key.
type snapshot struct {
	Time time.Time                   `json:"time"`
	Rows map[string][]map[string]any `json:"rows"`
}

type datasetConfig struct {
	Project string // the project ID
	Number  int64
	Config  *sipb.DatasetConfig
	// Activity is activityDataRetentionPeriodDays, which the REST API has
	// and the Go proto does not; nil when unset.
	Activity  *int32
	Snapshots []snapshot
	Next      time.Time
}

type datasetRecord struct {
	Project   string          `json:"project"`
	Number    int64           `json:"number"`
	Config    json.RawMessage `json:"config"`
	Activity  *int32          `json:"activity,omitempty"`
	Snapshots []snapshot      `json:"snapshots"`
	Next      time.Time       `json:"next"`
}

type insightsOp struct {
	Name         string          `json:"name"`
	Target       string          `json:"target"`
	Verb         string          `json:"verb"`
	Created      time.Time       `json:"created"`
	Ended        time.Time       `json:"ended"`
	ResponseType string          `json:"responseType"`
	Response     json.RawMessage `json:"response"`
	Deleted      bool            `json:"deleted,omitempty"`
}

const datasetsFile = "storageinsights-datasets.json"

type datasetsState struct {
	Configs []datasetRecord `json:"configs"`
	Ops     []insightsOp    `json:"ops"`
}

// datasets is the server's dataset configurations and operations.
type datasets struct {
	configs map[string]*datasetConfig
	ops     map[string]*insightsOp
	// snap serializes snapshots and BigQuery refreshes.
	snap sync.Mutex
}

func (s *InsightsServer) restoreDatasets() {
	s.ds = datasets{configs: map[string]*datasetConfig{}, ops: map[string]*insightsOp{}}
	var st datasetsState
	s.rt.restore(datasetsFile, &st)
	for _, r := range st.Configs {
		c := &datasetConfig{Project: r.Project, Number: r.Number, Config: &sipb.DatasetConfig{}, Activity: r.Activity,
			Snapshots: r.Snapshots, Next: r.Next}
		fromProtoJSON(r.Config, c.Config)
		s.ds.configs[c.Config.Name] = c
	}
	for i := range st.Ops {
		op := st.Ops[i]
		s.ds.ops[op.Name] = &op
	}
	s.rt.onTick(s.snapshotDue)
}

func (s *InsightsServer) saveDatasetsLocked() {
	st := datasetsState{Configs: []datasetRecord{}, Ops: []insightsOp{}}
	for _, c := range s.ds.configs {
		st.Configs = append(st.Configs, datasetRecord{Project: c.Project, Number: c.Number, Config: protoJSON(c.Config),
			Activity: c.Activity, Snapshots: c.Snapshots, Next: c.Next})
	}
	for _, op := range s.ds.ops {
		st.Ops = append(st.Ops, *op)
	}
	s.rt.persist(datasetsFile, st)
}

func (s *InsightsServer) resetDatasetsLocked(project string) {
	for n, c := range s.ds.configs {
		if project == "" || c.Project == project || strings.Split(n, "/")[1] == project {
			delete(s.ds.configs, n)
		}
	}
	for n := range s.ds.ops {
		if project == "" || strings.Split(n, "/")[1] == project {
			delete(s.ds.ops, n)
		}
	}
	s.saveDatasetsLocked()
}

// routeDatasets serves datasetConfigs and operations; p is the path's
// parts from "projects".
func (s *InsightsServer) routeDatasets(r *http.Request, p []string, parent string) (string, any, error) {
	const pre = "storageinsights.projects.locations."
	q := r.URL.Query()
	ctx := r.Context()
	if p[4] == "operations" {
		switch {
		case len(p) == 5 && r.Method == http.MethodGet:
			out, err := s.listOps(parent, q)
			return pre + "operations.list", out, err
		case len(p) == 6:
			id, verb, _ := strings.Cut(p[5], ":")
			name := parent + "/operations/" + id
			switch {
			case verb == "cancel" && r.Method == http.MethodPost:
				return pre + "operations.cancel", map[string]any{}, s.cancelOp(name)
			case verb == "" && r.Method == http.MethodGet:
				out, err := s.getOp(name)
				return pre + "operations.get", out, err
			case verb == "" && r.Method == http.MethodDelete:
				return pre + "operations.delete", map[string]any{}, s.deleteOp(name)
			}
		}
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	switch {
	case len(p) == 5 && r.Method == http.MethodPost:
		c, act, err := decodeDatasetConfig(r)
		if err != nil {
			return pre + "datasetConfigs.create", nil, err
		}
		out, err := s.createDataset(ctx, p[1], parent, q.Get("datasetConfigId"), c, act)
		return pre + "datasetConfigs.create", out, err
	case len(p) == 5 && r.Method == http.MethodGet:
		out, err := s.listDatasets(parent, q)
		return pre + "datasetConfigs.list", out, err
	case len(p) == 6:
		id, verb, _ := strings.Cut(p[5], ":")
		name := parent + "/datasetConfigs/" + id
		switch {
		case verb == "" && r.Method == http.MethodGet:
			out, err := s.getDataset(name)
			return pre + "datasetConfigs.get", out, err
		case verb == "" && r.Method == http.MethodPatch:
			c, act, err := decodeDatasetConfig(r)
			if err != nil {
				return pre + "datasetConfigs.patch", nil, err
			}
			out, err := s.patchDataset(ctx, name, c, act, q.Get("updateMask"))
			return pre + "datasetConfigs.patch", out, err
		case verb == "" && r.Method == http.MethodDelete:
			out, err := s.deleteDataset(name)
			return pre + "datasetConfigs.delete", out, err
		case verb == "linkDataset" && r.Method == http.MethodPost:
			out, err := s.link(ctx, name)
			return pre + "datasetConfigs.linkDataset", out, err
		case verb == "unlinkDataset" && r.Method == http.MethodPost:
			out, err := s.unlink(ctx, name)
			return pre + "datasetConfigs.unlinkDataset", out, err
		}
	}
	return "", nil, notFound("%s not found", r.URL.Path)
}

// decodeDatasetConfig reads a DatasetConfig body and its
// activityDataRetentionPeriodDays.
func decodeDatasetConfig(r *http.Request) (*sipb.DatasetConfig, *int32, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, nil, invalid("reading the request body: %v", err)
	}
	c := &sipb.DatasetConfig{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return c, nil, nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, c); err != nil {
		return nil, nil, invalid("Invalid JSON payload received. %v", err)
	}
	var extra struct {
		Activity *int32 `json:"activityDataRetentionPeriodDays"`
	}
	_ = json.Unmarshal(b, &extra)
	return c, extra.Activity, nil
}

// configJSON is a configuration as the API answers it.
func configJSON(c *datasetConfig) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(protoJSON(c.Config), &out)
	if c.Activity != nil {
		out["activityDataRetentionPeriodDays"] = *c.Activity
	}
	return out
}

func (s *InsightsServer) projectInfo(ctx context.Context, ref string) (ProjectInfo, error) {
	if s.rt.Projects == nil {
		return ProjectInfo{ID: ref, Name: ref}, nil
	}
	p, err := s.rt.Projects.Project(ctx, ref)
	if err != nil {
		return ProjectInfo{}, notFound("Project %s not found.", ref)
	}
	return p, nil
}

// intelligent reports whether a project has Storage Intelligence.
func (s *InsightsServer) intelligent(ctx context.Context, project string) bool {
	e, _ := s.rt.GCS.IntelligenceEdition(ctx, project)
	switch e {
	case "STANDARD", "TRIAL", "ESSENTIALS":
		return true
	}
	return false
}

// validateDataset checks a configuration as Google does.
func (s *InsightsServer) validateDataset(ctx context.Context, c *sipb.DatasetConfig, act *int32) error {
	if len([]rune(c.GetDescription())) > 256 {
		return invalid("description must be at most 256 characters.")
	}
	if d := c.GetRetentionPeriodDays(); d < 1 || d > MaxRetentionPeriodDays {
		return invalid("retention_period_days must be between 1 and %d, not %d.", MaxRetentionPeriodDays, d)
	}
	if act != nil && (*act < 0 || *act > MaxActivityRetentionPeriodDays) {
		return invalid("activity_data_retention_period_days must be between 0 and %d, not %d.", MaxActivityRetentionPeriodDays, *act)
	}
	switch o := c.GetSourceOptions().(type) {
	case nil:
		return invalid("One of source_projects, source_folders, organization_scope or cloud_storage_object_path is required.")
	case *sipb.DatasetConfig_SourceProjects_:
		n := o.SourceProjects.GetProjectNumbers()
		if len(n) == 0 {
			return invalid("source_projects.project_numbers must name at least one project.")
		}
		if len(n) > MaxDatasetScope {
			return invalid("A dataset scope can name at most %d projects, not %d.", MaxDatasetScope, len(n))
		}
		for _, x := range n {
			if x <= 0 {
				return invalid("Project number %d is not valid.", x)
			}
		}
	case *sipb.DatasetConfig_SourceFolders_:
		n := o.SourceFolders.GetFolderNumbers()
		if len(n) == 0 {
			return invalid("source_folders.folder_numbers must name at least one folder.")
		}
		if len(n) > MaxDatasetScope {
			return invalid("A dataset scope can name at most %d folders, not %d.", MaxDatasetScope, len(n))
		}
	case *sipb.DatasetConfig_OrganizationScope:
		if !o.OrganizationScope {
			return invalid("organization_scope must be true when it is set.")
		}
		if c.GetOrganizationNumber() == 0 {
			return invalid("organization_number is required with organization_scope.")
		}
	case *sipb.DatasetConfig_CloudStorageObjectPath:
		// Turned into projects or folders before this.
	}
	if c.GetOrganizationNumber() < 0 {
		return invalid("organization_number %d is not valid.", c.GetOrganizationNumber())
	}
	var locs []string
	switch l := c.GetCloudStorageLocations().(type) {
	case *sipb.DatasetConfig_IncludeCloudStorageLocations:
		locs = l.IncludeCloudStorageLocations.GetLocations()
	case *sipb.DatasetConfig_ExcludeCloudStorageLocations:
		locs = l.ExcludeCloudStorageLocations.GetLocations()
	}
	if c.GetCloudStorageLocations() != nil && len(locs) == 0 {
		return invalid("A location filter must name at least one location.")
	}
	for _, l := range locs {
		if !locationPattern.MatchString(strings.ToLower(l)) {
			return invalid("%q is not a Cloud Storage location.", l)
		}
	}
	bs := bucketFilters(c)
	if c.GetCloudStorageBuckets() != nil && len(bs) == 0 {
		return invalid("A bucket filter must name at least one bucket or bucket_prefix_regex.")
	}
	for _, b := range bs {
		switch {
		case b.GetBucketName() != "":
		case b.GetBucketPrefixRegex() != "":
			if _, err := regexp.Compile(b.GetBucketPrefixRegex()); err != nil {
				return invalid("bucket_prefix_regex %q is not a valid RE2 regular expression: %v", b.GetBucketPrefixRegex(), err)
			}
		default:
			return invalid("Each cloud_storage_buckets entry needs bucket_name or bucket_prefix_regex.")
		}
	}
	_ = ctx
	return nil
}

func bucketFilters(c *sipb.DatasetConfig) []*sipb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket {
	switch b := c.GetCloudStorageBuckets().(type) {
	case *sipb.DatasetConfig_IncludeCloudStorageBuckets:
		return b.IncludeCloudStorageBuckets.GetCloudStorageBuckets()
	case *sipb.DatasetConfig_ExcludeCloudStorageBuckets:
		return b.ExcludeCloudStorageBuckets.GetCloudStorageBuckets()
	}
	return nil
}

// readScopeFile turns cloud_storage_object_path into the projects or
// folders its file names.
func (s *InsightsServer) readScopeFile(ctx context.Context, c *sipb.DatasetConfig) error {
	path := c.GetCloudStorageObjectPath()
	if path == "" {
		if _, ok := c.GetSourceOptions().(*sipb.DatasetConfig_CloudStorageObjectPath); ok {
			return invalid("cloud_storage_object_path must not be empty.")
		}
		return nil
	}
	rest, ok := strings.CutPrefix(path, "gs://")
	bucket, object, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return invalid("cloud_storage_object_path %q must be gs://BUCKET/OBJECT.", path)
	}
	rc, err := s.rt.GCS.Read(ctx, bucket, object, 0)
	if err != nil {
		if IsNotFound(err) {
			return invalid("cloud_storage_object_path %s does not exist.", path)
		}
		return err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return err
	}
	var projects, folders []int64
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, num, _ := strings.Cut(line, "/")
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n <= 0 || (kind != "projects" && kind != "folders") {
			return invalid("Line %q of %s must be projects/PROJECT_NUMBER or folders/FOLDER_NUMBER.", line, path)
		}
		if kind == "projects" {
			projects = append(projects, n)
		} else {
			folders = append(folders, n)
		}
	}
	switch {
	case len(projects) > 0 && len(folders) > 0:
		return invalid("%s names both projects and folders; a file can name only one kind.", path)
	case len(projects) > 0:
		c.SourceOptions = &sipb.DatasetConfig_SourceProjects_{SourceProjects: &sipb.DatasetConfig_SourceProjects{ProjectNumbers: projects}}
	case len(folders) > 0:
		c.SourceOptions = &sipb.DatasetConfig_SourceFolders_{SourceFolders: &sipb.DatasetConfig_SourceFolders{FolderNumbers: folders}}
	default:
		return invalid("%s names no project or folder.", path)
	}
	return nil
}

// identityName is the service agent of a configuration: the project's
// for IDENTITY_TYPE_PER_PROJECT, as Google documents it, and one of the
// configuration's own otherwise (its form UNVERIFIED, re-read 2026-11-19,
// https://docs.cloud.google.com/storage/docs/insights/datasets).
func identityName(c *sipb.DatasetConfig, number int64) string {
	if c.GetIdentity().GetType() == sipb.Identity_IDENTITY_TYPE_PER_PROJECT {
		return fmt.Sprintf("service-%d@gcp-sa-storageinsights.iam.gserviceaccount.com", number)
	}
	return fmt.Sprintf("service-%d-%s@gcp-sa-storageinsights.iam.gserviceaccount.com", number,
		strings.ReplaceAll(c.GetUid(), "-", "")[:12])
}

// linkedDatasetID is the linked dataset's ID: the configuration's ID and
// its uid, as Google's example (my_dataset276daa7e_2991_...) shows.
func linkedDatasetID(c *sipb.DatasetConfig) string {
	return c.Name[strings.LastIndex(c.Name, "/")+1:] + strings.ReplaceAll(c.GetUid(), "-", "_")
}

func (s *InsightsServer) newOp(parent, target, verb, typ string, resp proto.Message) map[string]any {
	now := s.rt.Clock.Now()
	b, _ := protojson.Marshal(resp)
	op := &insightsOp{Name: parent + "/operations/operation-" + strconv.FormatInt(now.UnixMilli(), 10) + "-" + newID(8),
		Target: target, Verb: verb, Created: now, Ended: now, ResponseType: typ, Response: b}
	if c, ok := resp.(*sipb.DatasetConfig); ok {
		if dc := s.ds.configs[c.Name]; dc != nil {
			raw, _ := json.Marshal(configJSON(dc))
			op.Response = raw
		}
	}
	s.ds.ops[op.Name] = op
	s.saveDatasetsLocked()
	return opJSON(op)
}

func opJSON(op *insightsOp) map[string]any {
	md := &sipb.OperationMetadata{CreateTime: timestamppb.New(op.Created), EndTime: timestamppb.New(op.Ended), Target: op.Target,
		Verb: op.Verb, ApiVersion: "v1"}
	resp := map[string]any{}
	_ = json.Unmarshal(op.Response, &resp)
	resp["@type"] = op.ResponseType
	return map[string]any{"name": op.Name, "metadata": marshalAny(insightsMetadataType, md), "done": true, "response": resp}
}

func (s *InsightsServer) createDataset(ctx context.Context, ref, parent, id string, in *sipb.DatasetConfig, act *int32) (any, error) {
	loc := strings.Split(parent, "/")[3]
	if !linkedLocations[strings.ToLower(loc)] {
		return nil, invalid("Location %s is not supported for Storage Insights datasets: the linked dataset must be in one of "+
			"EU, US, asia-south1, asia-south2, asia-southeast1, europe-west1, us-central1, us-east1 or us-east4.", loc)
	}
	if id == "" {
		return nil, invalid("dataset_config_id is required.")
	}
	if !datasetConfigID.MatchString(id) {
		return nil, invalid("dataset_config_id %q must be up to 128 letters, numbers and underscores, starting with a letter.", id)
	}
	proj, err := s.projectInfo(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !s.intelligent(ctx, proj.ID) {
		return nil, precondition("Storage Insights datasets need Storage Intelligence: project %s has no Storage Intelligence "+
			"configuration with edition STANDARD or TRIAL. Enable it with gcloud storage intelligence-configs enable --project=%s.",
			proj.ID, proj.ID)
	}
	c := proto.Clone(in).(*sipb.DatasetConfig)
	if err := s.readScopeFile(ctx, c); err != nil {
		return nil, err
	}
	if err := s.validateDataset(ctx, c, act); err != nil {
		return nil, err
	}
	name := parent + "/datasetConfigs/" + id
	now := s.rt.Clock.Now()
	c.Name = name
	c.Uid = newUUID()
	c.CreateTime = timestamppb.New(now)
	c.UpdateTime = timestamppb.New(now)
	if c.Identity == nil {
		c.Identity = &sipb.Identity{}
	}
	if c.Identity.Type == sipb.Identity_IDENTITY_TYPE_UNSPECIFIED {
		c.Identity.Type = sipb.Identity_IDENTITY_TYPE_PER_CONFIG
	}
	c.Identity.Name = identityName(c, proj.Number)
	c.Link = nil
	c.Status = &spb.Status{}
	c.DatasetConfigState = sipb.DatasetConfig_CONFIG_STATE_ACTIVE
	s.mu.Lock()
	if _, ok := s.ds.configs[name]; ok {
		s.mu.Unlock()
		return nil, exists("DatasetConfig %s already exists.", name)
	}
	s.ds.configs[name] = &datasetConfig{Project: proj.ID, Number: proj.Number, Config: c, Activity: act, Next: now}
	s.saveDatasetsLocked()
	s.mu.Unlock()
	// The first snapshot is taken at once.
	s.snapshotDue(ctx, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newOp(parent, name, "create", datasetConfigType, c), nil
}

func (s *InsightsServer) getDataset(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.ds.configs[name]
	if !ok {
		return nil, notFound("DatasetConfig %s not found.", name)
	}
	return configJSON(c), nil
}

func (s *InsightsServer) listDatasets(parent string, q url.Values) (any, error) {
	s.mu.Lock()
	var cs []*datasetConfig
	for _, c := range s.ds.configs {
		if strings.HasPrefix(c.Config.Name, parent+"/") || (strings.HasSuffix(parent, "/locations/-") &&
			strings.HasPrefix(c.Config.Name, strings.TrimSuffix(parent, "-"))) {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, k int) bool {
		a, b := cs[i].Config.CreateTime.AsTime(), cs[k].Config.CreateTime.AsTime()
		if a.Equal(b) {
			return cs[i].Config.Name < cs[k].Config.Name
		}
		return a.Before(b)
	})
	items := make([]any, 0, len(cs))
	for _, c := range cs {
		items = append(items, configJSON(c))
	}
	s.mu.Unlock()
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(items, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"datasetConfigs": page}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

// mutableDatasetFields are the fields an update can change.
var mutableDatasetFields = map[string]bool{"description": true, "labels": true, "retention_period_days": true,
	"activity_data_retention_period_days": true, "source_projects": true, "source_folders": true, "organization_scope": true,
	"cloud_storage_object_path": true, "organization_number": true, "include_cloud_storage_locations": true,
	"exclude_cloud_storage_locations": true, "include_cloud_storage_buckets": true, "exclude_cloud_storage_buckets": true,
	"include_newly_created_buckets": true, "skip_verification_and_ingest": true}

func snakeCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *InsightsServer) patchDataset(ctx context.Context, name string, in *sipb.DatasetConfig, act *int32, mask string) (any, error) {
	s.mu.Lock()
	cur, ok := s.ds.configs[name]
	var next *sipb.DatasetConfig
	var nextAct *int32
	if ok {
		next = proto.Clone(cur.Config).(*sipb.DatasetConfig)
		nextAct = cur.Activity
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound("DatasetConfig %s not found.", name)
	}
	if strings.TrimSpace(mask) == "" {
		return nil, invalid("update_mask is required.")
	}
	var paths []string
	for _, p := range strings.Split(mask, ",") {
		p = strings.TrimSpace(p)
		sp := snakeCase(p)
		if strings.HasPrefix(sp, "labels.") {
			sp = "labels"
		}
		if !mutableDatasetFields[sp] {
			return nil, invalid("Field %s cannot be updated.", p)
		}
		if sp == "activity_data_retention_period_days" {
			nextAct = act
			continue
		}
		paths = append(paths, sp)
	}
	if err := applyMask(next, in, paths); err != nil {
		return nil, err
	}
	if err := s.readScopeFile(ctx, next); err != nil {
		return nil, err
	}
	if err := s.validateDataset(ctx, next, nextAct); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.rt.Clock.Now())
	s.mu.Lock()
	c, ok := s.ds.configs[name]
	if !ok {
		s.mu.Unlock()
		return nil, notFound("DatasetConfig %s not found.", name)
	}
	c.Config, c.Activity = next, nextAct
	c.prune(s.rt.Clock.Now())
	linked := c.Config.GetLink().GetLinked()
	s.saveDatasetsLocked()
	s.mu.Unlock()
	if linked {
		// A shorter retention drops the older snapshots from the dataset.
		s.ds.snap.Lock()
		err := s.refresh(ctx, name)
		s.ds.snap.Unlock()
		if err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newOp(strings.Join(strings.Split(name, "/")[:4], "/"), name, "update", datasetConfigType, next), nil
}

func (s *InsightsServer) deleteDataset(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.ds.configs[name]
	if !ok {
		return nil, notFound("DatasetConfig %s not found.", name)
	}
	if c.Config.GetLink().GetLinked() {
		return nil, precondition("DatasetConfig %s has a linked dataset, %s: unlink it before you delete the configuration.",
			name, c.Config.GetLink().GetDataset())
	}
	delete(s.ds.configs, name)
	return s.newOp(strings.Join(strings.Split(name, "/")[:4], "/"), name, "delete", emptyType, &emptypb.Empty{}), nil
}

func (s *InsightsServer) link(ctx context.Context, name string) (any, error) {
	if s.rt.Linked == nil {
		return nil, precondition("BigQuery is not served on this instance, so a Storage Insights dataset cannot be linked: " +
			"start the instance with BigQuery.")
	}
	s.ds.snap.Lock()
	defer s.ds.snap.Unlock()
	s.mu.Lock()
	c, ok := s.ds.configs[name]
	var cfg *sipb.DatasetConfig
	project := ""
	if ok {
		cfg = proto.Clone(c.Config).(*sipb.DatasetConfig)
		project = c.Project
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound("DatasetConfig %s not found.", name)
	}
	if cfg.GetLink().GetLinked() {
		return nil, precondition("DatasetConfig %s is already linked to dataset %s.", name, cfg.GetLink().GetDataset())
	}
	ds := linkedDatasetID(cfg)
	loc := strings.Split(name, "/")[3]
	if loc == "us" || loc == "eu" {
		loc = strings.ToUpper(loc)
	}
	err := s.rt.Linked.CreateDataset(ctx, project, ds, loc, "Storage Insights linked dataset of "+name+
		". Its views are read-only and refreshed by the dataset configuration.")
	if errors.Is(err, ErrDatasetExists) {
		return nil, exists("BigQuery dataset %s.%s already exists.", project, ds)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c, ok := s.ds.configs[name]; ok {
		c.Config.Link = &sipb.DatasetConfig_Link{Dataset: project + "." + ds, Linked: true}
	}
	s.mu.Unlock()
	if err := s.refresh(ctx, name); err != nil {
		_ = s.rt.Linked.DeleteDataset(ctx, project, ds)
		s.mu.Lock()
		if c, ok := s.ds.configs[name]; ok {
			c.Config.Link = nil
		}
		s.mu.Unlock()
		return nil, err
	}
	for _, t := range insightsTables {
		src := fmt.Sprintf("`%s.%s.%s`", project, ds, t.tableID())
		views := []LinkedView{{ID: t.view, Description: t.desc, Query: "SELECT * FROM " + src}}
		if t.latest != "" {
			views = append(views, LinkedView{ID: t.latest, Description: t.desc + " The latest snapshot only.",
				Query: fmt.Sprintf("SELECT * FROM %s WHERE snapshotTime = (SELECT MAX(snapshotTime) FROM %s)", src, src)})
		}
		for _, v := range views {
			if err := s.rt.Linked.CreateView(ctx, project, ds, v); err != nil {
				_ = s.rt.Linked.DeleteDataset(ctx, project, ds)
				s.mu.Lock()
				if c, ok := s.ds.configs[name]; ok {
					c.Config.Link = nil
				}
				s.mu.Unlock()
				return nil, err
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveDatasetsLocked()
	return s.newOp(strings.Join(strings.Split(name, "/")[:4], "/"), name, "linkDataset", linkResponseType, &sipb.LinkDatasetResponse{}), nil
}

func (s *InsightsServer) unlink(ctx context.Context, name string) (any, error) {
	s.ds.snap.Lock()
	defer s.ds.snap.Unlock()
	s.mu.Lock()
	c, ok := s.ds.configs[name]
	var l *sipb.DatasetConfig_Link
	project := ""
	if ok {
		l = c.Config.GetLink()
		project = c.Project
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound("DatasetConfig %s not found.", name)
	}
	if !l.GetLinked() {
		return nil, precondition("DatasetConfig %s has no linked dataset.", name)
	}
	if s.rt.Linked != nil {
		_, ds, _ := strings.Cut(l.GetDataset(), ".")
		if err := s.rt.Linked.DeleteDataset(ctx, project, ds); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.ds.configs[name]; ok {
		c.Config.Link = nil
	}
	s.saveDatasetsLocked()
	return s.newOp(strings.Join(strings.Split(name, "/")[:4], "/"), name, "unlinkDataset", emptyType, &emptypb.Empty{}), nil
}

func (s *InsightsServer) getOp(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ds.ops[name]
	if !ok || op.Deleted {
		return nil, notFound("Operation %s not found.", name)
	}
	return opJSON(op), nil
}

func (s *InsightsServer) listOps(parent string, q url.Values) (any, error) {
	s.mu.Lock()
	var ops []*insightsOp
	for _, op := range s.ds.ops {
		if !op.Deleted && strings.HasPrefix(op.Name, parent+"/") {
			ops = append(ops, op)
		}
	}
	sort.Slice(ops, func(i, k int) bool { return ops[i].Name < ops[k].Name })
	items := make([]any, 0, len(ops))
	for _, op := range ops {
		items = append(items, opJSON(op))
	}
	s.mu.Unlock()
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(items, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"operations": page}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

// cancelOp succeeds on an operation that is done, as every one is.
func (s *InsightsServer) cancelOp(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ds.ops[name]; !ok || op.Deleted {
		return notFound("Operation %s not found.", name)
	}
	return nil
}

func (s *InsightsServer) deleteOp(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ds.ops[name]
	if !ok || op.Deleted {
		return notFound("Operation %s not found.", name)
	}
	op.Deleted = true
	s.saveDatasetsLocked()
	return nil
}

// prune drops the snapshots older than the retention period.
// A snapshot older than the retention period and within the activity
// data's keeps its activity rows alone.
func (c *datasetConfig) prune(now time.Time) {
	days := c.Config.GetRetentionPeriodDays()
	act := activityDays(days, c.Activity)
	cut := now.Add(-time.Duration(days) * snapshotInterval)
	actCut := now.Add(-time.Duration(act) * snapshotInterval)
	keep := c.Snapshots[:0]
	for _, sn := range c.Snapshots {
		switch {
		case sn.Time.After(cut):
			keep = append(keep, sn)
		case sn.Time.After(actCut):
			rows := map[string][]map[string]any{}
			for k, v := range sn.Rows {
				if activityKeys[k] {
					rows[k] = v
				}
			}
			for _, e := range sn.Rows["events"] {
				if e["eventCode"] == "3" {
					rows["events"] = append(rows["events"], e)
				}
			}
			keep = append(keep, snapshot{Time: sn.Time, Rows: rows})
		}
	}
	c.Snapshots = keep
}

// keeps reports whether a configuration's dataset shows a snapshot's rows
// of table key (an events row of eventCode 3 is activity data) at now.
func (c *datasetConfig) keeps(key string, row map[string]any, sn snapshot, now time.Time) bool {
	days := c.Config.GetRetentionPeriodDays()
	if activityKeys[key] || (key == "events" && row["eventCode"] == "3") {
		days = activityDays(days, c.Activity)
	}
	return sn.Time.After(now.Add(-time.Duration(days) * snapshotInterval))
}

// snapshotDue takes every configuration's snapshots due by now and
// refreshes the linked datasets.
func (s *InsightsServer) snapshotDue(ctx context.Context, now time.Time) {
	s.ds.snap.Lock()
	defer s.ds.snap.Unlock()
	// The requests the activity views count, up to now.
	s.rt.flushActivity(ctx)
	type job struct {
		name string
		at   []time.Time
	}
	var jobs []job
	s.mu.Lock()
	for name, c := range s.ds.configs {
		var at []time.Time
		days := int(c.Config.GetRetentionPeriodDays())
		if behind := int(now.Sub(c.Next) / snapshotInterval); days > 0 && behind >= days {
			// Only the snapshots the retention period keeps.
			c.Next = c.Next.Add(time.Duration(behind-days+1) * snapshotInterval)
		}
		for !c.Next.After(now) {
			at = append(at, c.Next)
			c.Next = c.Next.Add(snapshotInterval)
		}
		if len(at) > 0 {
			jobs = append(jobs, job{name, at})
		}
	}
	s.mu.Unlock()
	for _, j := range jobs {
		s.mu.Lock()
		c, ok := s.ds.configs[j.name]
		var cfg *sipb.DatasetConfig
		var number int64
		var act int32
		var from time.Time
		if ok {
			cfg, number = proto.Clone(c.Config).(*sipb.DatasetConfig), c.Number
			act = activityDays(c.Config.GetRetentionPeriodDays(), c.Activity)
			if n := len(c.Snapshots); n > 0 {
				from = c.Snapshots[n-1].Time
			}
		}
		s.mu.Unlock()
		if !ok {
			continue
		}
		var snaps []snapshot
		for _, at := range j.at {
			if from.IsZero() || !from.Before(at) {
				from = at.Add(-snapshotInterval)
			}
			snaps = append(snaps, s.take(ctx, cfg, number, from, at, act))
			from = at
		}
		s.mu.Lock()
		linked := false
		if c, ok := s.ds.configs[j.name]; ok {
			c.Snapshots = append(c.Snapshots, snaps...)
			c.prune(now)
			linked = c.Config.GetLink().GetLinked()
			s.saveDatasetsLocked()
		}
		s.mu.Unlock()
		if linked {
			_ = s.refresh(ctx, j.name)
		}
	}
}

// refresh writes a linked configuration's retained snapshots into its
// dataset's tables; s.ds.snap is held.
func (s *InsightsServer) refresh(ctx context.Context, name string) error {
	s.mu.Lock()
	c, ok := s.ds.configs[name]
	if !ok || !c.Config.GetLink().GetLinked() || s.rt.Linked == nil {
		s.mu.Unlock()
		return nil
	}
	project := c.Project
	_, ds, _ := strings.Cut(c.Config.GetLink().GetDataset(), ".")
	snaps := append([]snapshot{}, c.Snapshots...)
	cc := &datasetConfig{Config: proto.Clone(c.Config).(*sipb.DatasetConfig), Activity: c.Activity}
	s.mu.Unlock()
	now := s.rt.Clock.Now()
	for _, t := range insightsTables {
		var rows []map[string]any
		if t.key == "object_contexts_catalog" {
			if len(snaps) > 0 {
				rows = snaps[len(snaps)-1].Rows[t.key]
			}
		} else {
			for _, sn := range snaps {
				for _, row := range sn.Rows[t.key] {
					if cc.keeps(t.key, row, sn, now) {
						rows = append(rows, row)
					}
				}
			}
		}
		if err := s.rt.Linked.ReplaceTable(ctx, project, ds, LinkedTable{ID: t.tableID(), Description: t.desc, Schema: t.schema},
			rows); err != nil {
			return fmt.Errorf("refreshing %s.%s.%s: %w", project, ds, t.tableID(), err)
		}
	}
	return nil
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// take takes one snapshot of a configuration's scope, stamped at.
// Its activity rows count the requests in (from, at], unless the
// configuration keeps no activity data (act 0).
func (s *InsightsServer) take(ctx context.Context, c *sipb.DatasetConfig, number int64, from, at time.Time, act int32) snapshot {
	ctx = context.WithValue(ctx, snapshotRead{}, true)
	sn := snapshot{Time: at, Rows: map[string][]map[string]any{}}
	loc := strings.Split(c.Name, "/")[3]
	add := func(table string, row map[string]any) { sn.Rows[table] = append(sn.Rows[table], row) }
	fail := func(code int, project int64, org string) {
		row := map[string]any{"errorCode": code, "errorSource": "CONFIGURATION_PREPROCESSING", "errorTime": ts(at),
			"sourceGcsLocation": loc, "projectErrorRecord": map[string]any{"projectNumber": project}}
		if org != "" {
			row["projectErrorRecord"].(map[string]any)["organizationName"] = "organizations/" + org
		}
		add("error_attributes", row)
	}
	var all []ProjectInfo
	if s.rt.Projects != nil {
		all, _ = s.rt.Projects.ListProjects(ctx)
	} else {
		p := strings.Split(c.Name, "/")[1]
		all = []ProjectInfo{{ID: p, Name: p, Number: number}}
	}
	inScope := map[int64]bool{}
	inFolders := map[string]bool{}
	switch o := c.GetSourceOptions().(type) {
	case *sipb.DatasetConfig_SourceProjects_:
		for _, n := range o.SourceProjects.GetProjectNumbers() {
			inScope[n] = true
		}
	case *sipb.DatasetConfig_SourceFolders_:
		for _, n := range o.SourceFolders.GetFolderNumbers() {
			inFolders[strconv.FormatInt(n, 10)] = true
		}
	}
	org := ""
	if c.GetOrganizationNumber() != 0 {
		org = strconv.FormatInt(c.GetOrganizationNumber(), 10)
	}
	var projects []ProjectInfo
	for _, p := range all {
		in := inScope[p.Number]
		for _, f := range p.Folders {
			in = in || inFolders[f]
		}
		if _, ok := c.GetSourceOptions().(*sipb.DatasetConfig_OrganizationScope); ok {
			in = p.Organization != "" && p.Organization == org
		}
		if !in {
			continue
		}
		if org != "" && p.Organization != org {
			fail(1, p.Number, org)
			continue
		}
		if !s.intelligent(ctx, p.ID) {
			fail(4, p.Number, p.Organization)
			continue
		}
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, k int) bool { return projects[i].Number < projects[k].Number })
	inProjects := map[int64]bool{}
	scoped := map[string]map[string]any{}
	objects := map[string]map[string]any{}
	for _, p := range projects {
		inProjects[p.Number] = true
		add("project_attributes", map[string]any{"snapshotTime": ts(at), "name": p.Name, "id": p.ID, "number": p.Number})
		buckets, err := s.listBuckets(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, b := range buckets {
			if !bucketInScope(c, b) {
				continue
			}
			objs, err := s.listObjects(ctx, str(b["name"]))
			if err != nil {
				add("error_attributes", map[string]any{"errorCode": 2, "errorSource": "CONFIGURATION_PREPROCESSING",
					"errorTime": ts(at), "sourceGcsLocation": loc, "bucketErrorRecord": map[string]any{"bucketName": str(b["name"]),
						"serviceAccount": c.GetIdentity().GetName()}})
				continue
			}
			scoped[str(b["name"])] = b
			var total int64
			for _, o := range objs {
				objects[str(b["name"])+"/"+str(o["name"])] = o
				row := objectRow(o, b, p.Number, at)
				total += toInt(o["size"])
				add("object_attributes", row)
				if ctxs := row["contexts"].([]map[string]any); len(ctxs) > 0 {
					var cc []map[string]any
					for _, x := range ctxs {
						cc = append(cc, map[string]any{"key": x["key"], "value": x["value"], "contextCreationTime": x["createTime"],
							"contextUpdateTime": x["updateTime"]})
					}
					add("object_contexts_catalog", map[string]any{"project": p.Number, "bucket": row["bucket"], "name": row["name"],
						"generation": row["generation"], "location": row["location"], "createdTime": str(o["timeCreated"]),
						"catalogUpdatedTime": ts(at), "contexts": cc})
				}
			}
			add("bucket_attributes", bucketRow(b, p.Number, at, int64(len(objs)), total))
		}
	}
	for _, v := range []string{"bucket_attributes_view", "object_attributes_view", "project_attributes_view"} {
		add("events", map[string]any{"manifest": map[string]any{"snapshotTime": ts(at), "viewName": v, "location": loc},
			"eventTime": ts(s.rt.Clock.Now()), "eventCode": "1"})
	}
	add("events", map[string]any{"globalManifest": map[string]any{"snapshotTime": ts(at)}, "eventTime": ts(s.rt.Clock.Now()),
		"eventCode": "2"})
	if act != 0 {
		s.activityRows(&sn, from, at, loc, inProjects, scoped, objects)
	}
	return sn
}

func bucketInScope(c *sipb.DatasetConfig, b map[string]any) bool {
	loc := strings.ToLower(str(b["location"]))
	match := func(ls []string) bool {
		for _, l := range ls {
			if strings.ToLower(l) == loc {
				return true
			}
		}
		return false
	}
	switch l := c.GetCloudStorageLocations().(type) {
	case *sipb.DatasetConfig_IncludeCloudStorageLocations:
		if !match(l.IncludeCloudStorageLocations.GetLocations()) {
			return false
		}
	case *sipb.DatasetConfig_ExcludeCloudStorageLocations:
		if match(l.ExcludeCloudStorageLocations.GetLocations()) {
			return false
		}
	}
	name := str(b["name"])
	named := false
	for _, f := range bucketFilters(c) {
		if f.GetBucketName() != "" && f.GetBucketName() == name {
			named = true
		}
		if re := f.GetBucketPrefixRegex(); re != "" {
			if r, err := regexp.Compile("^(?:" + re + ")"); err == nil && r.MatchString(name) {
				named = true
			}
		}
	}
	switch c.GetCloudStorageBuckets().(type) {
	case *sipb.DatasetConfig_IncludeCloudStorageBuckets:
		if !named {
			return false
		}
	case *sipb.DatasetConfig_ExcludeCloudStorageBuckets:
		if named {
			return false
		}
	}
	if !c.GetIncludeNewlyCreatedBuckets() {
		if t, err := time.Parse(time.RFC3339Nano, str(b["timeCreated"])); err == nil && t.After(c.GetCreateTime().AsTime()) {
			return false
		}
	}
	return true
}

func (s *InsightsServer) listBuckets(ctx context.Context, project string) ([]map[string]any, error) {
	var out []map[string]any
	q := url.Values{"project": {project}, "maxResults": {"1000"}}
	for {
		var page struct {
			Items         []map[string]any `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := s.rt.GCS.do(ctx, http.MethodGet, "/storage/v1/b", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

func (s *InsightsServer) listObjects(ctx context.Context, bucket string) ([]map[string]any, error) {
	var out []map[string]any
	q := url.Values{"maxResults": {"1000"}}
	for {
		var page struct {
			Items         []map[string]any `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := s.rt.GCS.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(bucket)+"/o", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	}
	return 0
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// nullable is v, or nil for "".
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// tsOf is a JSON API time as a TIMESTAMP value, nil when unset.
func tsOf(v any) any {
	t, err := time.Parse(time.RFC3339Nano, str(v))
	if err != nil {
		return nil
	}
	return ts(t)
}

// dtOf is a JSON API time as a DATETIME (UTC) value.
func dtOf(v any) any {
	t, err := time.Parse(time.RFC3339Nano, str(v))
	if err != nil {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000")
}

func pairs(m map[string]any) []map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []map[string]any{}
	for _, k := range keys {
		out = append(out, map[string]any{"key": k, "value": fmt.Sprint(m[k])})
	}
	return out
}

func bucketRow(b map[string]any, project int64, at time.Time, count, total int64) map[string]any {
	iam := obj(b["iamConfiguration"])
	pap := nullable(str(iam["publicAccessPrevention"]))
	ubla := boolOf(obj(iam["uniformBucketLevelAccess"])["enabled"])
	row := map[string]any{
		"snapshotTime": ts(at), "name": str(b["name"]), "location": str(b["location"]), "project": project,
		"storageClass":   nullable(str(b["storageClass"])),
		"public":         map[string]any{"bucketPolicyOnly": ubla, "publicAccessPrevention": pap},
		"versioning":     boolOf(obj(b["versioning"])["enabled"]),
		"lifecycle":      len(obj(b["lifecycle"])) > 0 && obj(b["lifecycle"])["rule"] != nil,
		"metageneration": toInt(b["metageneration"]), "timeCreated": tsOf(b["timeCreated"]),
		"labels":                         pairs(obj(b["labels"])),
		"defaultEventBasedHold":          boolOf(b["defaultEventBasedHold"]),
		"objectRetentionEnabled":         strings.EqualFold(str(obj(b["objectRetention"])["mode"]), "Enabled"),
		"enforcedEncryptionAllowedTypes": []string{},
		"iamConfiguration": map[string]any{"uniformBucketLevelAccess": map[string]any{"enabled": ubla},
			"publicAccessPrevention": pap},
		"resourceTags": []map[string]any{},
		"objectCount":  count, "totalSize": total,
	}
	if a := obj(b["autoclass"]); a != nil {
		row["autoclass"] = map[string]any{"enabled": boolOf(a["enabled"]), "toggleTime": tsOf(a["toggleTime"])}
	}
	if r := obj(b["retentionPolicy"]); r != nil {
		row["retentionPeriod"] = toInt(r["retentionPeriod"])
		if boolOf(r["isLocked"]) {
			row["retentionMode"] = "Locked"
		} else {
			row["retentionMode"] = "Unlocked"
		}
	}
	if sd := obj(b["softDeletePolicy"]); sd != nil {
		row["softDeletePolicy"] = map[string]any{"effectiveTime": dtOf(sd["effectiveTime"]),
			"retentionDurationSeconds": toInt(sd["retentionDurationSeconds"])}
	}
	enc := map[string]any{"defaultEncryptionType": "GOOGLE_MANAGED"}
	if k := str(obj(b["encryption"])["defaultKmsKeyName"]); k != "" {
		enc = map[string]any{"defaultEncryptionType": "CUSTOMER_MANAGED", "defaultKmsKeyName": k}
	}
	row["encryption"] = enc
	return row
}

// crc32cInt is a JSON API crc32c (base64, big-endian) as an integer.
func crc32cInt(v string) any {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(b) != 4 {
		return nil
	}
	return int64(binary.BigEndian.Uint32(b))
}

func objectRow(o, b map[string]any, project int64, at time.Time) map[string]any {
	row := map[string]any{
		"snapshotTime": ts(at), "bucket": str(o["bucket"]), "location": str(b["location"]),
		"contentDisposition": nullable(str(o["contentDisposition"])), "contentEncoding": nullable(str(o["contentEncoding"])),
		"contentLanguage": nullable(str(o["contentLanguage"])), "contentType": nullable(str(o["contentType"])),
		"customTime": tsOf(o["customTime"]), "etag": nullable(str(o["etag"])), "eventBasedHold": boolOf(o["eventBasedHold"]),
		"generation": toInt(o["generation"]), "mediaLink": nullable(str(o["mediaLink"])), "metadata": pairs(obj(o["metadata"])),
		"metageneration": toInt(o["metageneration"]), "name": str(o["name"]), "selfLink": nullable(str(o["selfLink"])),
		"size": toInt(o["size"]), "storageClass": nullable(str(o["storageClass"])), "temporaryHold": boolOf(o["temporaryHold"]),
		"timeCreated": tsOf(o["timeCreated"]), "timeDeleted": tsOf(o["timeDeleted"]), "updated": tsOf(o["updated"]),
		"timeStorageClassUpdated": tsOf(o["timeStorageClassUpdated"]),
		"retentionExpirationTime": tsOf(o["retentionExpirationTime"]),
		"softDeleteTime":          dtOf(o["softDeleteTime"]), "hardDeleteTime": dtOf(o["hardDeleteTime"]), "project": project,
		"encryptionType": "GOOGLE_MANAGED",
	}
	if n := toInt(o["componentCount"]); n > 0 {
		row["componentCount"] = n
	}
	// Google gives no checksums of an object encrypted with a Cloud KMS key.
	switch {
	case str(o["kmsKeyName"]) != "":
		row["encryptionType"] = "CUSTOMER_MANAGED"
	case o["customerEncryption"] != nil:
		row["encryptionType"] = "CUSTOMER_SUPPLIED"
		row["crc32c"], row["md5Hash"] = crc32cInt(str(o["crc32c"])), nullable(str(o["md5Hash"]))
	default:
		row["crc32c"], row["md5Hash"] = crc32cInt(str(o["crc32c"])), nullable(str(o["md5Hash"]))
	}
	if r := obj(o["retention"]); r != nil {
		row["retainUntilTime"] = tsOf(r["retainUntilTime"])
		row["retainUntilTimeLockStatus"] = strings.EqualFold(str(r["mode"]), "Locked")
	}
	ctxs := []map[string]any{}
	custom := obj(obj(o["contexts"])["custom"])
	keys := make([]string, 0, len(custom))
	for k := range custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := obj(custom[k])
		ctxs = append(ctxs, map[string]any{"key": k, "value": str(v["value"]), "type": "CUSTOM",
			"createTime": tsOf(v["createTime"]), "updateTime": tsOf(v["updateTime"])})
	}
	row["contexts"] = ctxs
	return row
}
