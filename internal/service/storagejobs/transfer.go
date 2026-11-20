package storagejobs

// The Storage Transfer Service API v1 (docs.cloud.google.com/
// storage-transfer/docs/overview), JSON:
//
//	GET    /v1/googleServiceAccounts/P
//	POST   /v1/transferJobs                       body: TransferJob
//	GET    /v1/transferJobs?filter={"projectId":...}
//	GET    /v1/transferJobs/J?projectId=
//	PATCH  /v1/transferJobs/J                     body: {projectId, transferJob, updateTransferJobFieldMask}
//	POST   /v1/transferJobs/J:run                 body: {projectId}
//	DELETE /v1/transferJobs/J?projectId=
//	GET    /v1/transferOperations?name=transferOperations&filter={"projectId":...}
//	GET    /v1/transferOperations/O
//	POST   /v1/transferOperations/O:pause|:resume|:cancel
//	POST|GET|PATCH|DELETE /v1/projects/P/agentPools[/A]
//
// A transfer copies between the instance's buckets (gcsDataSource,
// gcsDataSink, through the JSON API's rewrite, so an object keeps its
// metadata) and local POSIX directories (posixDataSource, posixDataSink),
// which the agent pools serve: a pool is served by a local agent built
// into CloudBurrow, in place of the agents Google's customers run, and its
// bandwidth limit paces the transfers through it. Amazon S3, S3-compatible
// storage, Azure Blob Storage, HDFS and HTTP URL lists are refused: they
// are outside this machine, and CloudBurrow never reaches them. Event-driven
// transfers (eventStream) and cross-bucket replication (replicationSpec)
// are refused too: they are not served yet.

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	stpb "cloud.google.com/go/storagetransfer/apiv1/storagetransferpb"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

const (
	transferOpType = "type.googleapis.com/google.storagetransfer.v1.TransferOperation"
	emptyType      = "type.googleapis.com/google.protobuf.Empty"
	// DefaultAgentPool is the pool every project has.
	DefaultAgentPool = "transfer_service_default"
	// minRepeatInterval is Google's smallest repeat interval.
	minRepeatInterval = time.Hour
	chunk             = 64 << 10
)

var (
	jobNamePattern  = regexp.MustCompile(`^transferJobs/[A-Za-z0-9._~-]*[A-Za-z0-9]$`)
	poolIDPattern   = regexp.MustCompile(`^[a-z]([a-z0-9._~-]{0,38}[a-z0-9])?$`)
	topicPattern    = regexp.MustCompile(`^projects/[^/]+/topics/[^/]+$`)
	maxJobNameRunes = 128
)

type transferJob struct {
	Job     *stpb.TransferJob
	LastRun time.Time // the latest scheduled run time started
}

type transferOp struct {
	Op *stpb.TransferOperation
	// pause and cancel are what was asked of the run.
	pause, cancel bool
	cond          *sync.Cond
}

type transferRecord struct {
	Jobs  []json.RawMessage    `json:"jobs"`
	Last  map[string]time.Time `json:"last"`
	Ops   []json.RawMessage    `json:"ops"`
	Pools []json.RawMessage    `json:"pools"`
}

// TransferServer serves the Storage Transfer Service API v1.
type TransferServer struct {
	server
	mu    sync.Mutex
	jobs  map[string]*transferJob // by name
	ops   map[string]*transferOp  // by name
	pools map[string]*stpb.AgentPool
	seq   int64
}

// NewTransferServer serves the API on addr over rt.
func NewTransferServer(addr string, rt *Runtime) *TransferServer {
	s := &TransferServer{jobs: map[string]*transferJob{}, ops: map[string]*transferOp{}, pools: map[string]*stpb.AgentPool{}}
	s.server = server{name: "storagetransfer", addr: addr, rt: rt, handler: s.route}
	var rec transferRecord
	rt.restore(transferFile, &rec)
	for _, b := range rec.Jobs {
		j := &stpb.TransferJob{}
		fromProtoJSON(b, j)
		s.jobs[j.Name] = &transferJob{Job: j, LastRun: rec.Last[j.Name]}
	}
	for _, b := range rec.Ops {
		o := &stpb.TransferOperation{}
		fromProtoJSON(b, o)
		if !terminal(o.Status) {
			// A run the instance stopped during ends aborted.
			o.Status = stpb.TransferOperation_ABORTED
			o.EndTime = timestamppb.New(rt.Clock.Now())
		}
		s.ops[o.Name] = s.newOp(o)
	}
	for _, b := range rec.Pools {
		p := &stpb.AgentPool{}
		fromProtoJSON(b, p)
		s.pools[p.Name] = p
	}
	rt.onTick(s.runDue)
	return s
}

const transferFile = "storagetransfer.json"

func (s *TransferServer) newOp(o *stpb.TransferOperation) *transferOp {
	return &transferOp{Op: o, cond: sync.NewCond(&s.mu)}
}

func (s *TransferServer) saveLocked() {
	rec := transferRecord{Last: map[string]time.Time{}}
	for n, j := range s.jobs {
		rec.Jobs = append(rec.Jobs, protoJSON(j.Job))
		rec.Last[n] = j.LastRun
	}
	for _, o := range s.ops {
		rec.Ops = append(rec.Ops, protoJSON(o.Op))
	}
	for _, p := range s.pools {
		rec.Pools = append(rec.Pools, protoJSON(p))
	}
	s.rt.persist(transferFile, rec)
}

// ResetProject forgets one project's jobs, runs and pools, or every
// project's.
func (s *TransferServer) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, j := range s.jobs {
		if project == "" || j.Job.ProjectId == project {
			delete(s.jobs, n)
		}
	}
	for n, o := range s.ops {
		if project == "" || o.Op.ProjectId == project {
			o.cancel = true
			o.cond.Broadcast()
			delete(s.ops, n)
		}
	}
	for n := range s.pools {
		if project == "" || strings.HasPrefix(n, "projects/"+project+"/") {
			delete(s.pools, n)
		}
	}
	s.saveLocked()
	return nil
}

func (s *TransferServer) route(r *http.Request) (string, any, error) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	p := strings.Split(path, "/")
	q := r.URL.Query()
	const pre = "storagetransfer."
	ctx := r.Context()
	switch p[0] {
	case "googleServiceAccounts":
		if len(p) == 2 && r.Method == http.MethodGet {
			n := resource.ProjectNumber(p[1])
			return pre + "googleServiceAccounts.get", &stpb.GoogleServiceAccount{
				AccountEmail: fmt.Sprintf("project-%d@storage-transfer-service.iam.gserviceaccount.com", n),
				SubjectId:    strconv.FormatInt(100000000000000000+n, 10)}, nil
		}
	case "transferJobs":
		switch {
		case len(p) == 1 && r.Method == http.MethodPost:
			j := &stpb.TransferJob{}
			if err := decode(r, j); err != nil {
				return pre + "transferJobs.create", nil, err
			}
			out, err := s.create(ctx, j)
			return pre + "transferJobs.create", out, err
		case len(p) == 1 && r.Method == http.MethodGet:
			out, err := s.listJobs(q)
			return pre + "transferJobs.list", out, err
		case len(p) >= 2:
			id, verb, _ := strings.Cut(strings.Join(p[1:], "/"), ":")
			name := "transferJobs/" + id
			switch {
			case verb == "run" && r.Method == http.MethodPost:
				var body struct {
					ProjectID string `json:"projectId"`
				}
				_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
				out, err := s.runJob(name, body.ProjectID)
				return pre + "transferJobs.run", out, err
			case verb == "" && r.Method == http.MethodGet:
				out, err := s.getJob(name, q.Get("projectId"))
				return pre + "transferJobs.get", out, err
			case verb == "" && r.Method == http.MethodPatch:
				req := &stpb.UpdateTransferJobRequest{}
				if err := decode(r, req); err != nil {
					return pre + "transferJobs.patch", nil, err
				}
				out, err := s.patchJob(ctx, name, req)
				return pre + "transferJobs.patch", out, err
			case verb == "" && r.Method == http.MethodDelete:
				return pre + "transferJobs.delete", map[string]any{}, s.deleteJob(name, q.Get("projectId"))
			}
		}
	case "transferOperations":
		switch {
		case len(p) == 1 && r.Method == http.MethodGet:
			out, err := s.listOps(q)
			return pre + "transferOperations.list", out, err
		case len(p) >= 2:
			id, verb, _ := strings.Cut(strings.Join(p[1:], "/"), ":")
			name := "transferOperations/" + id
			switch {
			case verb == "" && r.Method == http.MethodGet:
				out, err := s.operation(name)
				return pre + "transferOperations.get", out, err
			case r.Method == http.MethodPost && (verb == "pause" || verb == "resume" || verb == "cancel"):
				return pre + "transferOperations." + verb, map[string]any{}, s.control(name, verb)
			}
		}
	case "projects":
		if len(p) >= 3 && p[2] == "agentPools" {
			project := p[1]
			switch {
			case len(p) == 3 && r.Method == http.MethodPost:
				ap := &stpb.AgentPool{}
				if err := decode(r, ap); err != nil {
					return pre + "projects.agentPools.create", nil, err
				}
				out, err := s.createPool(project, q.Get("agentPoolId"), ap)
				return pre + "projects.agentPools.create", out, err
			case len(p) == 3 && r.Method == http.MethodGet:
				out, err := s.listPools(project, q)
				return pre + "projects.agentPools.list", out, err
			case len(p) == 4:
				name := "projects/" + project + "/agentPools/" + p[3]
				switch r.Method {
				case http.MethodGet:
					out, err := s.pool(name)
					return pre + "projects.agentPools.get", out, err
				case http.MethodPatch:
					ap := &stpb.AgentPool{}
					if err := decode(r, ap); err != nil {
						return pre + "projects.agentPools.patch", nil, err
					}
					out, err := s.patchPool(name, ap, q.Get("updateMask"))
					return pre + "projects.agentPools.patch", out, err
				case http.MethodDelete:
					return pre + "projects.agentPools.delete", map[string]any{}, s.deletePool(name)
				}
			}
		}
	}
	return "", nil, notFound("%s not found", r.URL.Path)
}

// validateSpec checks a transfer spec, refusing what is outside this
// machine.
func (s *TransferServer) validateSpec(ctx context.Context, project string, spec *stpb.TransferSpec) error {
	if spec == nil {
		return invalid("transfer_spec is required.")
	}
	switch spec.DataSource.(type) {
	case *stpb.TransferSpec_AwsS3DataSource:
		return localOnly("A transfer from Amazon S3 (awsS3DataSource)")
	case *stpb.TransferSpec_AwsS3CompatibleDataSource:
		return localOnly("A transfer from S3-compatible storage (awsS3CompatibleDataSource)")
	case *stpb.TransferSpec_AzureBlobStorageDataSource:
		return localOnly("A transfer from Azure Blob Storage (azureBlobStorageDataSource)")
	case *stpb.TransferSpec_HttpDataSource:
		return localOnly("A transfer from a URL list (httpDataSource)")
	case *stpb.TransferSpec_HdfsDataSource:
		return localOnly("A transfer from HDFS (hdfsDataSource)")
	case nil:
		return invalid("transfer_spec needs a data source.")
	}
	if spec.DataSink == nil {
		return invalid("transfer_spec needs a data sink.")
	}
	if spec.GetGcsIntermediateDataLocation() != nil {
		return invalid("gcs_intermediate_data_location is only for transfers between file systems, which go directly here.")
	}
	if g := spec.GetGcsDataSource(); g != nil {
		if err := s.checkGCS(ctx, "gcs_data_source", g); err != nil {
			return err
		}
	}
	if g := spec.GetGcsDataSink(); g != nil {
		if err := s.checkGCS(ctx, "gcs_data_sink", g); err != nil {
			return err
		}
	}
	if spec.GetPosixDataSource() != nil && spec.GetPosixDataSink() != nil {
		// Google needs an intermediate bucket for file system to file system.
		return invalid("A transfer between two POSIX file systems needs gcs_intermediate_data_location, which CloudBurrow does not serve: transfer through a bucket in two jobs.")
	}
	if px := spec.GetPosixDataSource(); px != nil {
		if err := checkRoot("posix_data_source.root_directory", px.GetRootDirectory(), true); err != nil {
			return err
		}
		if err := s.checkPool(project, &spec.SourceAgentPoolName); err != nil {
			return err
		}
	}
	if px := spec.GetPosixDataSink(); px != nil {
		if err := checkRoot("posix_data_sink.root_directory", px.GetRootDirectory(), false); err != nil {
			return err
		}
		if err := s.checkPool(project, &spec.SinkAgentPoolName); err != nil {
			return err
		}
	}
	if o := spec.GetTransferOptions(); o != nil && o.GetDeleteObjectsUniqueInSink() && o.GetDeleteObjectsFromSourceAfterTransfer() {
		return invalid("delete_objects_unique_in_sink and delete_objects_from_source_after_transfer are mutually exclusive.")
	}
	if c := spec.GetObjectConditions(); c != nil {
		for _, ps := range [][]string{c.GetIncludePrefixes(), c.GetExcludePrefixes()} {
			if len(ps) > 1000 {
				return invalid("object_conditions take at most 1000 include and 1000 exclude prefixes.")
			}
			for _, p := range ps {
				if p == "" || strings.HasPrefix(p, "/") {
					return invalid("Prefix %q must not be empty or start with /.", p)
				}
			}
		}
	}
	if m := spec.GetTransferManifest(); m != nil && !strings.HasPrefix(m.GetLocation(), "gs://") {
		return invalid("transfer_manifest.location %q must be gs://BUCKET/OBJECT.", m.GetLocation())
	}
	return nil
}

func (s *TransferServer) checkGCS(ctx context.Context, what string, g *stpb.GcsData) error {
	if g.GetBucketName() == "" {
		return invalid("%s.bucket_name is required.", what)
	}
	if p := g.GetPath(); p != "" && !strings.HasSuffix(p, "/") {
		return invalid("%s.path %q must be empty or end with /.", what, p)
	}
	if _, err := s.rt.GCS.Bucket(ctx, g.GetBucketName()); err != nil {
		if IsNotFound(err) {
			return precondition("Bucket %s (%s) does not exist on this instance.", g.GetBucketName(), what)
		}
		return err
	}
	return nil
}

func checkRoot(what, dir string, mustExist bool) error {
	if !filepath.IsAbs(dir) {
		return invalid("%s %q must be an absolute path.", what, dir)
	}
	if mustExist {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			return precondition("%s %q is not a directory the local agent can read.", what, dir)
		}
	}
	return nil
}

// checkPool resolves a spec's agent pool (the default when empty).
func (s *TransferServer) checkPool(project string, name *string) error {
	if *name == "" {
		*name = "projects/" + project + "/agentPools/" + DefaultAgentPool
	}
	if _, err := s.pool(*name); err != nil {
		return invalid("Agent pool %s does not exist.", *name)
	}
	return nil
}

func validateSchedule(sc *stpb.Schedule) error {
	if sc == nil {
		return nil
	}
	if !validDate(sc.GetScheduleStartDate()) {
		return invalid("schedule.schedule_start_date is required.")
	}
	if sc.GetScheduleEndDate() != nil {
		if !validDate(sc.GetScheduleEndDate()) {
			return invalid("schedule.schedule_end_date is not a valid date.")
		}
		if dateTime(sc.GetScheduleEndDate()).Before(dateTime(sc.GetScheduleStartDate())) {
			return invalid("schedule.schedule_end_date must not be before schedule_start_date.")
		}
	}
	if ri := sc.GetRepeatInterval(); ri != nil && ri.AsDuration() < minRepeatInterval {
		return invalid("schedule.repeat_interval must be at least 1 hour.")
	}
	return nil
}

func validateNotification(n *stpb.NotificationConfig) error {
	if n == nil {
		return nil
	}
	if !topicPattern.MatchString(n.GetPubsubTopic()) {
		return invalid("notification_config.pubsub_topic %q must be projects/PROJECT/topics/TOPIC.", n.GetPubsubTopic())
	}
	if n.GetPayloadFormat() == stpb.NotificationConfig_PAYLOAD_FORMAT_UNSPECIFIED {
		return invalid("notification_config.payload_format is required.")
	}
	return nil
}

func (s *TransferServer) create(ctx context.Context, j *stpb.TransferJob) (any, error) {
	if j.GetProjectId() == "" {
		return nil, invalid("project_id is required.")
	}
	if j.GetEventStream() != nil {
		return nil, unimplemented("Event-driven transfers (eventStream) are not served by CloudBurrow yet.")
	}
	if j.GetReplicationSpec() != nil {
		return nil, unimplemented("Cross-bucket replication (replicationSpec) is not served by CloudBurrow yet: use a transfer_spec between the buckets.")
	}
	if j.Name != "" && (!jobNamePattern.MatchString(j.Name) || len(j.Name) > maxJobNameRunes || strings.HasPrefix(j.Name, "transferJobs/OPI")) {
		return nil, invalid("Invalid transfer job name %q: it must be transferJobs/ followed by letters, digits, -, ., _ and ~, end with a letter or digit, and be at most 128 characters.", j.Name)
	}
	if err := s.validateSpec(ctx, j.GetProjectId(), j.GetTransferSpec()); err != nil {
		return nil, err
	}
	if err := validateSchedule(j.GetSchedule()); err != nil {
		return nil, err
	}
	if err := validateNotification(j.GetNotificationConfig()); err != nil {
		return nil, err
	}
	switch j.Status {
	case stpb.TransferJob_STATUS_UNSPECIFIED:
		j.Status = stpb.TransferJob_ENABLED
	case stpb.TransferJob_DELETED:
		return nil, invalid("A transfer job cannot be created DELETED.")
	}
	now := s.rt.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.Name == "" {
		for {
			j.Name = "transferJobs/" + strconv.FormatInt(now.UnixNano()/1000+s.seq, 10)
			s.seq++
			if s.jobs[j.Name] == nil {
				break
			}
		}
	} else if s.jobs[j.Name] != nil {
		return nil, exists("Transfer job %s already exists.", j.Name)
	}
	j.CreationTime = timestamppb.New(now)
	j.LastModificationTime = timestamppb.New(now)
	j.DeletionTime = nil
	j.LatestOperationName = ""
	s.jobs[j.Name] = &transferJob{Job: j}
	s.saveLocked()
	go s.runDue(context.Background(), s.rt.Clock.Now())
	return proto.Clone(j), nil
}

func (s *TransferServer) jobLocked(name, project string) (*transferJob, error) {
	j, ok := s.jobs[name]
	if !ok || (project != "" && j.Job.ProjectId != project) {
		return nil, notFound("Transfer job %s not found.", name)
	}
	return j, nil
}

func (s *TransferServer) getJob(name, project string) (any, error) {
	if project == "" {
		return nil, invalid("project_id is required.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(name, project)
	if err != nil {
		return nil, err
	}
	return proto.Clone(j.Job), nil
}

type listFilter struct {
	ProjectID        string   `json:"projectId"`
	ProjectIDSnake   string   `json:"project_id"`
	JobNames         []string `json:"jobNames"`
	JobStatuses      []string `json:"jobStatuses"`
	OperationNames   []string `json:"operationNames"`
	TransferStatuses []string `json:"transferStatuses"`
}

func parseFilter(s string) (listFilter, error) {
	var f listFilter
	if strings.TrimSpace(s) == "" {
		return f, invalid("filter is required: a JSON object such as {\"projectId\":\"my-project\"}.")
	}
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return f, invalid("Invalid filter %q: %v", s, err)
	}
	if f.ProjectID == "" {
		f.ProjectID = f.ProjectIDSnake
	}
	if f.ProjectID == "" {
		return f, invalid("The filter's projectId is required.")
	}
	return f, nil
}

func has(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *TransferServer) listJobs(q url.Values) (any, error) {
	f, err := parseFilter(q.Get("filter"))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var out []*stpb.TransferJob
	for _, j := range s.jobs {
		st := j.Job.Status.String()
		if j.Job.ProjectId != f.ProjectID || !has(f.JobNames, j.Job.Name) {
			continue
		}
		if len(f.JobStatuses) == 0 && j.Job.Status == stpb.TransferJob_DELETED {
			continue
		}
		if !has(f.JobStatuses, st) {
			continue
		}
		out = append(out, proto.Clone(j.Job).(*stpb.TransferJob))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(out, int32(size), q.Get("pageToken"), 256, 256)
	if err != nil {
		return nil, err
	}
	return &stpb.ListTransferJobsResponse{TransferJobs: page, NextPageToken: next}, nil
}

// updatableJobFields are the fields Google lets a patch change.
var updatableJobFields = map[string]bool{"description": true, "transfer_spec": true, "transferSpec": true,
	"notification_config": true, "notificationConfig": true, "logging_config": true, "loggingConfig": true,
	"status": true, "schedule": true, "service_account": true, "serviceAccount": true}

func (s *TransferServer) patchJob(ctx context.Context, name string, req *stpb.UpdateTransferJobRequest) (any, error) {
	if req.GetProjectId() == "" {
		return nil, invalid("project_id is required.")
	}
	if req.GetTransferJob() == nil {
		return nil, invalid("transfer_job is required.")
	}
	s.mu.Lock()
	j, err := s.jobLocked(name, req.GetProjectId())
	var next *stpb.TransferJob
	if err == nil {
		if j.Job.Status == stpb.TransferJob_DELETED {
			err = notFound("Transfer job %s not found.", name)
		} else {
			next = proto.Clone(j.Job).(*stpb.TransferJob)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateTransferJobFieldMask().GetPaths()
	if len(paths) == 0 {
		// No mask: every updatable field the body sets.
		in := req.GetTransferJob().ProtoReflect()
		for k := range updatableJobFields {
			if f := field(in, k); f != nil && in.Has(f) && strings.Contains(k, "_") == (string(f.Name()) == k) {
				paths = append(paths, string(f.Name()))
			}
		}
	}
	for _, p := range paths {
		if !updatableJobFields[strings.Split(p, ".")[0]] {
			return nil, invalid("Field %s of a transfer job cannot be updated.", p)
		}
	}
	if err := applyMask(next, req.GetTransferJob(), paths); err != nil {
		return nil, err
	}
	if err := s.validateSpec(ctx, next.ProjectId, next.GetTransferSpec()); err != nil {
		return nil, err
	}
	if err := validateSchedule(next.GetSchedule()); err != nil {
		return nil, err
	}
	if err := validateNotification(next.GetNotificationConfig()); err != nil {
		return nil, err
	}
	now := s.rt.Clock.Now()
	next.LastModificationTime = timestamppb.New(now)
	if next.Status == stpb.TransferJob_DELETED {
		next.DeletionTime = timestamppb.New(now)
	}
	s.mu.Lock()
	if cur, ok := s.jobs[name]; ok {
		cur.Job = next
		s.saveLocked()
	}
	s.mu.Unlock()
	return proto.Clone(next), nil
}

func (s *TransferServer) deleteJob(name, project string) error {
	if project == "" {
		return invalid("project_id is required.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(name, project)
	if err != nil {
		return err
	}
	if j.Job.Status == stpb.TransferJob_DELETED {
		return notFound("Transfer job %s not found.", name)
	}
	now := s.rt.Clock.Now()
	j.Job.Status = stpb.TransferJob_DELETED
	j.Job.DeletionTime = timestamppb.New(now)
	j.Job.LastModificationTime = timestamppb.New(now)
	s.saveLocked()
	return nil
}

func (s *TransferServer) runJob(name, project string) (any, error) {
	if project == "" {
		return nil, invalid("project_id is required.")
	}
	s.mu.Lock()
	j, err := s.jobLocked(name, project)
	if err == nil && j.Job.Status != stpb.TransferJob_ENABLED {
		err = precondition("Transfer job %s is %s: only an enabled job can run.", name, j.Job.Status)
	}
	if err == nil && s.runningLocked(name) {
		err = precondition("Transfer job %s already has an operation in progress.", name)
	}
	var op *transferOp
	if err == nil {
		op = s.startLocked(j)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.operation(op.Op.Name)
}

func (s *TransferServer) runningLocked(job string) bool {
	for _, o := range s.ops {
		if o.Op.TransferJobName == job && !terminal(o.Op.Status) {
			return true
		}
	}
	return false
}

func terminal(st stpb.TransferOperation_Status) bool {
	return st == stpb.TransferOperation_SUCCESS || st == stpb.TransferOperation_FAILED || st == stpb.TransferOperation_ABORTED
}

// startLocked starts a run of j; s.mu is held.
func (s *TransferServer) startLocked(j *transferJob) *transferOp {
	now := s.rt.Clock.Now()
	s.seq++
	id := "transferJobs-" + strings.TrimPrefix(j.Job.Name, "transferJobs/") + "-" + strconv.FormatInt(now.UnixNano()+s.seq, 10)
	o := s.newOp(&stpb.TransferOperation{
		Name: "transferOperations/" + id, ProjectId: j.Job.ProjectId, TransferSpec: proto.Clone(j.Job.TransferSpec).(*stpb.TransferSpec),
		NotificationConfig: j.Job.NotificationConfig, LoggingConfig: j.Job.LoggingConfig, StartTime: timestamppb.New(now),
		Status: stpb.TransferOperation_IN_PROGRESS, Counters: &stpb.TransferCounters{}, TransferJobName: j.Job.Name,
	})
	s.ops[o.Op.Name] = o
	j.Job.LatestOperationName = o.Op.Name
	s.saveLocked()
	go s.execute(o)
	return o
}

// runDue starts every scheduled run now due.
func (s *TransferServer) runDue(_ context.Context, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.Job.Status != stpb.TransferJob_ENABLED || j.Job.Schedule == nil {
			continue
		}
		at, ok := dueRun(j.Job, j.LastRun, now)
		if !ok {
			continue
		}
		j.LastRun = at
		if s.runningLocked(j.Job.Name) {
			s.saveLocked()
			continue // Google skips a run while one is in progress.
		}
		s.startLocked(j)
	}
}

// dueRun is the latest scheduled run time of job after last and not after
// now.
func dueRun(job *stpb.TransferJob, last, now time.Time) (time.Time, bool) {
	sc := job.Schedule
	start := dateTime(sc.GetScheduleStartDate())
	created := job.GetCreationTime().AsTime()
	var first time.Time
	if tod := sc.GetStartTimeOfDay(); tod != nil {
		first = start.Add(time.Duration(tod.Hours)*time.Hour + time.Duration(tod.Minutes)*time.Minute + time.Duration(tod.Seconds)*time.Second)
	} else if !start.After(created.Truncate(24 * time.Hour)) {
		// No start time, starting today or earlier: the first run is now.
		first = created
	} else {
		first = start
	}
	end := time.Time{}
	if e := sc.GetScheduleEndDate(); validDate(e) {
		end = dateTime(e).Add(24*time.Hour - time.Nanosecond)
		if tod := sc.GetEndTimeOfDay(); tod != nil {
			end = dateTime(e).Add(time.Duration(tod.Hours)*time.Hour + time.Duration(tod.Minutes)*time.Minute + time.Duration(tod.Seconds)*time.Second)
		}
	}
	once := sc.GetScheduleEndDate() != nil && dateTime(sc.GetScheduleEndDate()).Equal(start)
	step := 24 * time.Hour
	if ri := sc.GetRepeatInterval(); ri != nil {
		step = ri.AsDuration()
	}
	var due time.Time
	found := false
	for t, n := first, 0; !t.After(now) && n < 1_000_000; n++ {
		if !end.IsZero() && t.After(end) {
			break
		}
		if t.After(last) {
			due, found = t, true
		}
		if once {
			break
		}
		if sc.GetStartTimeOfDay() == nil && sc.GetRepeatInterval() == nil {
			t = t.Truncate(24 * time.Hour).Add(24 * time.Hour) // each day at midnight UTC
		} else {
			t = t.Add(step)
		}
	}
	return due, found
}

// item is one object or file a run considers.
type item struct {
	rel     string // name relative to the source root
	size    int64
	updated time.Time
	md5     string
	gen     int64
	path    string // a POSIX file's path
}

// execute runs a transfer operation to its end.
func (s *TransferServer) execute(o *transferOp) {
	ctx := context.Background()
	err := s.transfer(ctx, o)
	s.mu.Lock()
	now := s.rt.Clock.Now()
	switch {
	case o.cancel:
		o.Op.Status = stpb.TransferOperation_ABORTED
	case err != nil:
		o.Op.Status = stpb.TransferOperation_FAILED
		addTransferError(o.Op, code.Code(statusOf(err)), "", err.Error())
	case o.Op.Counters.ObjectsFromSourceFailed > 0 || o.Op.Counters.ObjectsFailedToDeleteFromSink > 0:
		o.Op.Status = stpb.TransferOperation_FAILED
	default:
		o.Op.Status = stpb.TransferOperation_SUCCESS
	}
	o.Op.EndTime = timestamppb.New(now)
	op := proto.Clone(o.Op).(*stpb.TransferOperation)
	s.saveLocked()
	s.mu.Unlock()
	s.notify(op)
}

func addTransferError(op *stpb.TransferOperation, c code.Code, url, detail string) {
	for _, e := range op.ErrorBreakdowns {
		if e.ErrorCode == c {
			e.ErrorCount++
			if len(e.ErrorLogEntries) < maxErrorLogEntries {
				e.ErrorLogEntries = append(e.ErrorLogEntries, &stpb.ErrorLogEntry{Url: url, ErrorDetails: []string{detail}})
			}
			return
		}
	}
	op.ErrorBreakdowns = append(op.ErrorBreakdowns, &stpb.ErrorSummary{ErrorCode: c, ErrorCount: 1,
		ErrorLogEntries: []*stpb.ErrorLogEntry{{Url: url, ErrorDetails: []string{detail}}}})
}

// wait blocks while the run is paused, and reports whether it was
// cancelled.
func (s *TransferServer) wait(o *transferOp) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for o.pause && !o.cancel {
		o.cond.Wait()
	}
	return o.cancel
}

// transfer does a run's work.
func (s *TransferServer) transfer(ctx context.Context, o *transferOp) error {
	s.mu.Lock()
	spec := proto.Clone(o.Op.TransferSpec).(*stpb.TransferSpec)
	counters := o.Op.Counters
	s.mu.Unlock()
	add := func(f func(c *stpb.TransferCounters)) {
		s.mu.Lock()
		f(counters)
		s.mu.Unlock()
	}
	opts := spec.GetTransferOptions()
	srcItems, err := s.listSource(ctx, spec)
	if err != nil {
		return err
	}
	if m := spec.GetTransferManifest(); m != nil {
		srcItems, err = s.manifestFilter(ctx, m.GetLocation(), srcItems)
		if err != nil {
			return err
		}
	}
	srcItems = s.filter(spec.GetObjectConditions(), srcItems)
	var bytesFound int64
	for _, it := range srcItems {
		bytesFound += it.size
	}
	add(func(c *stpb.TransferCounters) {
		c.ObjectsFoundFromSource = int64(len(srcItems))
		c.BytesFoundFromSource = bytesFound
	})
	sinkItems, err := s.listSink(ctx, spec)
	if err != nil {
		return err
	}
	sinkIdx := map[string]item{}
	for _, it := range sinkItems {
		sinkIdx[it.rel] = it
	}
	limit := s.bandwidth(spec)
	for _, it := range srcItems {
		if s.wait(o) {
			return nil
		}
		sink, inSink := sinkIdx[it.rel]
		if inSink && skip(opts, it, sink) {
			add(func(c *stpb.TransferCounters) {
				c.ObjectsFromSourceSkippedBySync++
				c.BytesFromSourceSkippedBySync += it.size
			})
			continue
		}
		if err := s.copyItem(ctx, o, spec, it, limit); err != nil {
			if s.wait(o) {
				return nil
			}
			add(func(c *stpb.TransferCounters) {
				c.ObjectsFromSourceFailed++
				c.BytesFromSourceFailed += it.size
			})
			s.mu.Lock()
			addTransferError(o.Op, code.Code(statusOf(err)), sourceURL(spec, it), err.Error())
			s.mu.Unlock()
			continue
		}
		add(func(c *stpb.TransferCounters) {
			c.ObjectsCopiedToSink++
			c.BytesCopiedToSink += it.size
		})
		if opts.GetDeleteObjectsFromSourceAfterTransfer() {
			if err := s.deleteSource(ctx, spec, it); err != nil {
				s.mu.Lock()
				addTransferError(o.Op, code.Code(statusOf(err)), sourceURL(spec, it), "deleting from the source: "+err.Error())
				s.mu.Unlock()
			} else {
				add(func(c *stpb.TransferCounters) {
					c.ObjectsDeletedFromSource++
					c.BytesDeletedFromSource += it.size
				})
			}
		}
	}
	if opts.GetDeleteObjectsUniqueInSink() {
		inSource := map[string]bool{}
		for _, it := range srcItems {
			inSource[it.rel] = true
		}
		for _, it := range s.filter(spec.GetObjectConditions(), sinkItems) {
			if inSource[it.rel] {
				continue
			}
			if s.wait(o) {
				return nil
			}
			add(func(c *stpb.TransferCounters) {
				c.ObjectsFoundOnlyFromSink++
				c.BytesFoundOnlyFromSink += it.size
			})
			if err := s.deleteSink(ctx, spec, it); err != nil {
				add(func(c *stpb.TransferCounters) {
					c.ObjectsFailedToDeleteFromSink++
					c.BytesFailedToDeleteFromSink += it.size
				})
				continue
			}
			add(func(c *stpb.TransferCounters) {
				c.ObjectsDeletedFromSink++
				c.BytesDeletedFromSink += it.size
			})
		}
	}
	return nil
}

// skip reports whether a source item already in the sink is left alone.
func skip(o *stpb.TransferOptions, src, sink item) bool {
	if o.GetOverwriteObjectsAlreadyExistingInSink() {
		return false
	}
	switch o.GetOverwriteWhen() {
	case stpb.TransferOptions_ALWAYS:
		return false
	case stpb.TransferOptions_NEVER:
		return true
	}
	// DIFFERENT, the default: the same size and checksum is the same data.
	return src.size == sink.size && src.md5 != "" && src.md5 == sink.md5
}

func sourceURL(spec *stpb.TransferSpec, it item) string {
	if g := spec.GetGcsDataSource(); g != nil {
		return "gs://" + g.GetBucketName() + "/" + g.GetPath() + it.rel
	}
	return "file://" + it.path
}

func (s *TransferServer) listSource(ctx context.Context, spec *stpb.TransferSpec) ([]item, error) {
	if g := spec.GetGcsDataSource(); g != nil {
		return s.listGCS(ctx, g)
	}
	if px := spec.GetPosixDataSource(); px != nil {
		return listPOSIX(px.GetRootDirectory())
	}
	return nil, invalid("no data source")
}

func (s *TransferServer) listSink(ctx context.Context, spec *stpb.TransferSpec) ([]item, error) {
	if g := spec.GetGcsDataSink(); g != nil {
		return s.listGCS(ctx, g)
	}
	if px := spec.GetPosixDataSink(); px != nil {
		if _, err := os.Stat(px.GetRootDirectory()); os.IsNotExist(err) {
			return nil, nil
		}
		return listPOSIX(px.GetRootDirectory())
	}
	return nil, invalid("no data sink")
}

func (s *TransferServer) listGCS(ctx context.Context, g *stpb.GcsData) ([]item, error) {
	objs, err := s.rt.GCS.List(ctx, g.GetBucketName(), g.GetPath(), false)
	if err != nil {
		return nil, err
	}
	out := make([]item, 0, len(objs))
	for _, o := range objs {
		if strings.HasSuffix(o.Name, "/") && o.SizeBytes() == 0 {
			continue // a folder placeholder
		}
		out = append(out, item{rel: strings.TrimPrefix(o.Name, g.GetPath()), size: o.SizeBytes(), updated: o.UpdatedTime(),
			md5: o.MD5Hash, gen: o.Gen()})
	}
	return out, nil
}

func listPOSIX(root string) ([]item, error) {
	var out []item
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil // directories, and symlinks, which are not followed
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		sum, err := fileMD5(p)
		if err != nil {
			return err
		}
		out = append(out, item{rel: filepath.ToSlash(rel), size: fi.Size(), updated: fi.ModTime().UTC(), md5: sum, path: p})
		return nil
	})
	if err != nil {
		return nil, precondition("the local agent cannot list %s: %v", root, err)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].rel < out[k].rel })
	return out, nil
}

func fileMD5(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// manifestFilter keeps the items a transfer manifest lists (its first
// column, relative to the source root).
func (s *TransferServer) manifestFilter(ctx context.Context, loc string, items []item) ([]item, error) {
	b, o, _ := strings.Cut(strings.TrimPrefix(loc, "gs://"), "/")
	rc, err := s.rt.GCS.Read(ctx, b, o, 0)
	if err != nil {
		return nil, fmt.Errorf("reading transfer manifest %s: %w", loc, err)
	}
	defer rc.Close()
	cr := csv.NewReader(rc)
	cr.FieldsPerRecord = -1
	want := map[string]bool{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalid("transfer manifest %s is not CSV: %v", loc, err)
		}
		if len(rec) > 0 && rec[0] != "" {
			want[strings.TrimPrefix(rec[0], "/")] = true
		}
	}
	var out []item
	for _, it := range items {
		if want[it.rel] {
			out = append(out, it)
		}
	}
	return out, nil
}

// filter applies object conditions, against the clock.
func (s *TransferServer) filter(c *stpb.ObjectConditions, items []item) []item {
	if c == nil {
		return items
	}
	now := s.rt.Clock.Now()
	var out []item
	for _, it := range items {
		if len(c.GetIncludePrefixes()) > 0 {
			ok := false
			for _, p := range c.GetIncludePrefixes() {
				ok = ok || strings.HasPrefix(it.rel, p)
			}
			if !ok {
				continue
			}
		}
		excluded := false
		for _, p := range c.GetExcludePrefixes() {
			excluded = excluded || strings.HasPrefix(it.rel, p)
		}
		if excluded {
			continue
		}
		age := now.Sub(it.updated)
		if d := c.GetMinTimeElapsedSinceLastModification(); d != nil && age < d.AsDuration() {
			continue
		}
		if d := c.GetMaxTimeElapsedSinceLastModification(); d != nil && age > d.AsDuration() {
			continue
		}
		if t := c.GetLastModifiedSince(); t != nil && it.updated.Before(t.AsTime()) {
			continue
		}
		if t := c.GetLastModifiedBefore(); t != nil && !it.updated.Before(t.AsTime()) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// bandwidth is the bytes per second a run's agent pool allows (0: no
// limit).
func (s *TransferServer) bandwidth(spec *stpb.TransferSpec) int64 {
	var limit int64
	for _, name := range []string{spec.GetSourceAgentPoolName(), spec.GetSinkAgentPoolName()} {
		if name == "" {
			continue
		}
		if p, err := s.pool(name); err == nil {
			if mbps := p.(*stpb.AgentPool).GetBandwidthLimit().GetLimitMbps(); mbps > 0 && (limit == 0 || mbps*125000 < limit) {
				limit = mbps * 125000
			}
		}
	}
	if spec.GetPosixDataSource() == nil && spec.GetPosixDataSink() == nil {
		return 0
	}
	return limit
}

// pacedReader paces reads to limit bytes per second, and blocks while the
// run is paused.
type pacedReader struct {
	r     io.Reader
	limit int64
	s     *TransferServer
	o     *transferOp
	start time.Time
	n     int64
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if p.s.wait(p.o) {
		return 0, errAborted
	}
	if len(b) > chunk {
		b = b[:chunk]
	}
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.limit > 0 {
		want := time.Duration(float64(p.n) / float64(p.limit) * float64(time.Second))
		if d := want - time.Since(p.start); d > 0 {
			time.Sleep(d)
		}
	}
	return n, err
}

var errAborted = fmt.Errorf("the operation was aborted")

func (s *TransferServer) copyItem(ctx context.Context, o *transferOp, spec *stpb.TransferSpec, it item, limit int64) error {
	g := s.rt.GCS
	src, dst := spec.GetGcsDataSource(), spec.GetGcsDataSink()
	switch {
	case src != nil && dst != nil:
		_, err := g.Rewrite(ctx, src.GetBucketName(), src.GetPath()+it.rel, it.gen, dst.GetBucketName(), dst.GetPath()+it.rel, nil, nil)
		return err
	case spec.GetPosixDataSource() != nil && dst != nil:
		f, err := os.Open(it.path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = g.Write(ctx, dst.GetBucketName(), map[string]any{"name": dst.GetPath() + it.rel, "contentType": "application/octet-stream"},
			&pacedReader{r: f, limit: limit, s: s, o: o, start: time.Now()})
		return err
	case src != nil && spec.GetPosixDataSink() != nil:
		rc, err := g.Read(ctx, src.GetBucketName(), src.GetPath()+it.rel, it.gen)
		if err != nil {
			return err
		}
		defer rc.Close()
		p := filepath.Join(spec.GetPosixDataSink().GetRootDirectory(), filepath.FromSlash(it.rel))
		root := filepath.Clean(spec.GetPosixDataSink().GetRootDirectory())
		if !strings.HasPrefix(filepath.Clean(p), root+string(filepath.Separator)) {
			return invalid("object %s would be written outside %s", it.rel, root)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := p + ".cloudburrow-transfer"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, &pacedReader{r: rc, limit: limit, s: s, o: o, start: time.Now()})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, p)
	}
	return invalid("unsupported source and sink")
}

func (s *TransferServer) deleteSource(ctx context.Context, spec *stpb.TransferSpec, it item) error {
	if g := spec.GetGcsDataSource(); g != nil {
		return s.rt.GCS.Delete(ctx, g.GetBucketName(), g.GetPath()+it.rel, 0)
	}
	return os.Remove(it.path)
}

func (s *TransferServer) deleteSink(ctx context.Context, spec *stpb.TransferSpec, it item) error {
	if g := spec.GetGcsDataSink(); g != nil {
		return s.rt.GCS.Delete(ctx, g.GetBucketName(), g.GetPath()+it.rel, 0)
	}
	return os.Remove(it.path)
}

// notify publishes a finished run's notification.
func (s *TransferServer) notify(op *stpb.TransferOperation) {
	n := op.GetNotificationConfig()
	if n == nil || s.rt.Publisher == nil {
		return
	}
	var ev stpb.NotificationConfig_EventType
	switch op.Status {
	case stpb.TransferOperation_SUCCESS:
		ev = stpb.NotificationConfig_TRANSFER_OPERATION_SUCCESS
	case stpb.TransferOperation_FAILED:
		ev = stpb.NotificationConfig_TRANSFER_OPERATION_FAILED
	case stpb.TransferOperation_ABORTED:
		ev = stpb.NotificationConfig_TRANSFER_OPERATION_ABORTED
	default:
		return
	}
	if len(n.GetEventTypes()) > 0 {
		ok := false
		for _, e := range n.GetEventTypes() {
			ok = ok || e == ev
		}
		if !ok {
			return
		}
	}
	attrs := map[string]string{"eventType": ev.String(), "payloadFormat": n.GetPayloadFormat().String(),
		"transferJobName": op.TransferJobName, "transferOperationName": op.Name, "projectId": op.ProjectId,
		"eventTime": s.rt.Clock.Now().Format(time.RFC3339Nano)}
	var data []byte
	if n.GetPayloadFormat() == stpb.NotificationConfig_JSON {
		data, _ = protojson.Marshal(op)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.rt.Publisher.Publish(ctx, n.GetPubsubTopic(), data, attrs); err != nil {
		s.mu.Lock()
		if o, ok := s.ops[op.Name]; ok {
			addTransferError(o.Op, code.Code_FAILED_PRECONDITION, "", "publishing the notification to "+n.GetPubsubTopic()+": "+err.Error())
			s.saveLocked()
		}
		s.mu.Unlock()
	}
}

func (s *TransferServer) operation(name string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.ops[name]
	if !ok {
		return nil, notFound("Transfer operation %s not found.", name)
	}
	return opJSONLocked(o), nil
}

func opJSONLocked(o *transferOp) map[string]any {
	done := terminal(o.Op.Status)
	out := map[string]any{"name": o.Op.Name, "metadata": marshalAny(transferOpType, o.Op), "done": done}
	if done {
		switch o.Op.Status {
		case stpb.TransferOperation_SUCCESS:
			out["response"] = map[string]any{"@type": emptyType}
		case stpb.TransferOperation_ABORTED:
			out["error"] = map[string]any{"code": 1, "message": "The transfer operation was aborted."}
		default:
			msg := "The transfer operation failed."
			if len(o.Op.ErrorBreakdowns) > 0 && len(o.Op.ErrorBreakdowns[0].ErrorLogEntries) > 0 {
				msg += " " + strings.Join(o.Op.ErrorBreakdowns[0].ErrorLogEntries[0].ErrorDetails, "; ")
			}
			out["error"] = map[string]any{"code": 2, "message": msg}
		}
	}
	return out
}

func (s *TransferServer) listOps(q url.Values) (any, error) {
	f, err := parseFilter(q.Get("filter"))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var names []string
	for n, o := range s.ops {
		if o.Op.ProjectId == f.ProjectID && has(f.JobNames, o.Op.TransferJobName) && has(f.OperationNames, n) &&
			has(f.TransferStatuses, o.Op.Status.String()) {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, k int) bool {
		a, b := s.ops[names[i]].Op.StartTime.AsTime(), s.ops[names[k]].Op.StartTime.AsTime()
		if a.Equal(b) {
			return names[i] > names[k]
		}
		return a.After(b)
	})
	ops := []any{}
	for _, n := range names {
		ops = append(ops, opJSONLocked(s.ops[n]))
	}
	s.mu.Unlock()
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(ops, int32(size), q.Get("pageToken"), 256, 256)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"operations": page}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

func (s *TransferServer) control(name, verb string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.ops[name]
	if !ok {
		return notFound("Transfer operation %s not found.", name)
	}
	st := o.Op.Status
	switch verb {
	case "pause":
		if st != stpb.TransferOperation_IN_PROGRESS {
			return precondition("Transfer operation %s is %s: only an operation in progress can be paused.", name, st)
		}
		o.pause = true
		o.Op.Status = stpb.TransferOperation_PAUSED
	case "resume":
		if st != stpb.TransferOperation_PAUSED {
			return precondition("Transfer operation %s is %s: only a paused operation can be resumed.", name, st)
		}
		o.pause = false
		o.Op.Status = stpb.TransferOperation_IN_PROGRESS
	case "cancel":
		if terminal(st) {
			return precondition("Transfer operation %s is %s and cannot be cancelled.", name, st)
		}
		o.cancel = true
	}
	o.cond.Broadcast()
	s.saveLocked()
	return nil
}

// Agent pools.

func defaultPool(project string) *stpb.AgentPool {
	return &stpb.AgentPool{Name: "projects/" + project + "/agentPools/" + DefaultAgentPool, State: stpb.AgentPool_CREATED}
}

func (s *TransferServer) createPool(project, id string, ap *stpb.AgentPool) (any, error) {
	if !poolIDPattern.MatchString(id) || strings.HasPrefix(id, "goog") {
		return nil, invalid("Invalid agent_pool_id %q: it must be 1 to 40 lowercase letters, digits, -, ., _ and ~, starting with a letter, and not start with goog.", id)
	}
	name := "projects/" + project + "/agentPools/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pools[name]; ok || id == DefaultAgentPool {
		return nil, exists("Agent pool %s already exists.", name)
	}
	p := proto.Clone(ap).(*stpb.AgentPool)
	p.Name = name
	p.State = stpb.AgentPool_CREATED
	s.pools[name] = p
	s.saveLocked()
	return proto.Clone(p), nil
}

func (s *TransferServer) pool(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.poolLocked(name)
}

func (s *TransferServer) poolLocked(name string) (proto.Message, error) {
	if p, ok := s.pools[name]; ok {
		return proto.Clone(p), nil
	}
	parts := strings.Split(name, "/")
	if len(parts) == 4 && parts[0] == "projects" && parts[2] == "agentPools" && parts[3] == DefaultAgentPool {
		return defaultPool(parts[1]), nil
	}
	return nil, notFound("Agent pool %s not found.", name)
}

func (s *TransferServer) listPools(project string, q url.Values) (any, error) {
	s.mu.Lock()
	out := []*stpb.AgentPool{defaultPool(project)}
	if p, ok := s.pools[out[0].Name]; ok {
		out[0] = p
	}
	for n, p := range s.pools {
		if strings.HasPrefix(n, "projects/"+project+"/") && n != out[0].Name {
			out = append(out, proto.Clone(p).(*stpb.AgentPool))
		}
	}
	s.mu.Unlock()
	if f := q.Get("filter"); f != "" {
		var lf struct {
			AgentPoolNames []string `json:"agentPoolNames"`
		}
		if err := json.Unmarshal([]byte(f), &lf); err != nil {
			return nil, invalid("Invalid filter %q: %v", f, err)
		}
		var keep []*stpb.AgentPool
		for _, p := range out {
			if has(lf.AgentPoolNames, p.Name) {
				keep = append(keep, p)
			}
		}
		out = keep
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(out, int32(size), q.Get("pageToken"), 256, 256)
	if err != nil {
		return nil, err
	}
	return &stpb.ListAgentPoolsResponse{AgentPools: page, NextPageToken: next}, nil
}

func (s *TransferServer) patchPool(name string, in *stpb.AgentPool, mask string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.poolLocked(name)
	if err != nil {
		return nil, err
	}
	next := cur.(*stpb.AgentPool)
	paths := strings.Split(mask, ",")
	if mask == "" {
		paths = []string{"display_name", "bandwidth_limit"}
	}
	for _, p := range paths {
		switch strings.TrimSpace(p) {
		case "display_name", "displayName", "bandwidth_limit", "bandwidthLimit":
		default:
			return nil, invalid("Field %s of an agent pool cannot be updated.", p)
		}
	}
	if err := applyMask(next, in, paths); err != nil {
		return nil, err
	}
	s.pools[name] = next
	s.saveLocked()
	return proto.Clone(next), nil
}

func (s *TransferServer) deletePool(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.poolLocked(name); err != nil {
		return err
	}
	if strings.HasSuffix(name, "/"+DefaultAgentPool) {
		return precondition("The default agent pool %s cannot be deleted.", name)
	}
	for _, j := range s.jobs {
		sp := j.Job.GetTransferSpec()
		if j.Job.Status != stpb.TransferJob_DELETED && (sp.GetSourceAgentPoolName() == name || sp.GetSinkAgentPoolName() == name) {
			return precondition("Agent pool %s is used by transfer job %s.", name, j.Job.Name)
		}
	}
	delete(s.pools, name)
	s.saveLocked()
	return nil
}
