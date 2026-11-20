package storagejobs

// The Storage Batch Operations API v1 (docs.cloud.google.com/storage/docs/
// batch-operations/overview), JSON:
//
//	GET    /v1/projects/P/locations[/L]
//	POST   /v1/projects/P/locations/global/jobs?jobId=&requestId=   body: Job
//	GET    /v1/projects/P/locations/global/jobs[/J]
//	POST   /v1/projects/P/locations/global/jobs/J:cancel
//	DELETE /v1/projects/P/locations/global/jobs/J
//	GET    /v1/projects/P/locations/global/jobs/J/bucketOperations[/B]
//	GET|DELETE /v1/projects/P/locations/global/operations[/O], POST .../O:cancel
//
// A job runs in the background as soon as it is created, object by
// object, through the Cloud Storage JSON API; its operation is done once
// the job is terminal. What Google documents and is kept: one
// transformation per job; up to 1,000 prefixes and 1,000 buckets; a CSV
// manifest of bucket,name[,generation] rows; dry runs that count and change
// nothing; each bucket's project must have Storage Intelligence; Requester
// Pays buckets are refused; a job still running after 14 days is
// cancelled. Failures of single objects are counted and summarized by
// error code (errorSummaries, with up to 10 log entries each), and leave the
// job SUCCEEDED; a job FAILS only when it cannot run at all (a manifest it
// cannot read).

import (
	"context"
	"encoding/csv"
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

	sbpb "cloud.google.com/go/storagebatchoperations/apiv1/storagebatchoperationspb"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	batchMetadataType = "type.googleapis.com/google.cloud.storagebatchoperations.v1.OperationMetadata"
	batchJobType      = "type.googleapis.com/google.cloud.storagebatchoperations.v1.Job"
	// MaxBatchPrefixes and MaxBatchBuckets are Google's limits.
	MaxBatchPrefixes = 1000
	MaxBatchBuckets  = 1000
	// MaxBatchJobLifetime is how long a job may run before it is cancelled.
	MaxBatchJobLifetime = 14 * 24 * time.Hour
	maxErrorLogEntries  = 10
)

var jobIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,126}[a-z0-9])?$`)

// batchJob is one job and what the server keeps beside it.
type batchJob struct {
	Project string
	Job     *sbpb.Job
	Buckets []*sbpb.BucketOperation
	Op      string
	OpTime  time.Time
	OpEnd   time.Time
	Cancel  bool
	Request string // requestId
}

type batchRecord struct {
	Project string            `json:"project"`
	Job     json.RawMessage   `json:"job"`
	Buckets []json.RawMessage `json:"buckets"`
	Op      string            `json:"op"`
	OpTime  time.Time         `json:"opTime"`
	OpEnd   time.Time         `json:"opEnd,omitzero"`
	Cancel  bool              `json:"cancel,omitempty"`
}

// BatchServer serves the Storage Batch Operations API v1.
type BatchServer struct {
	server
	mu       sync.Mutex
	jobs     map[string]*batchJob // by job name
	ops      map[string]string    // operation name -> job name
	deletedO map[string]bool
}

// NewBatchServer serves the API on addr over rt.
func NewBatchServer(addr string, rt *Runtime) *BatchServer {
	s := &BatchServer{jobs: map[string]*batchJob{}, ops: map[string]string{}, deletedO: map[string]bool{}}
	s.server = server{name: "storagebatchoperations", addr: addr, rt: rt, handler: s.route}
	s.load()
	rt.onTick(s.expire)
	return s
}

const batchFile = "storagebatchoperations.json"

func (s *BatchServer) load() {
	var recs []batchRecord
	s.rt.restore(batchFile, &recs)
	for _, r := range recs {
		j := &batchJob{Project: r.Project, Job: &sbpb.Job{}, Op: r.Op, OpTime: r.OpTime, OpEnd: r.OpEnd, Cancel: r.Cancel}
		fromProtoJSON(r.Job, j.Job)
		for _, b := range r.Buckets {
			bo := &sbpb.BucketOperation{}
			fromProtoJSON(b, bo)
			j.Buckets = append(j.Buckets, bo)
		}
		// A job that was running when the instance stopped does not
		// resume: it ends cancelled, as an interrupted job would.
		if j.Job.State == sbpb.Job_RUNNING || j.Job.State == sbpb.Job_QUEUED {
			j.Job.State = sbpb.Job_CANCELED
			j.Job.CompleteTime = timestamppb.New(s.rt.Clock.Now())
			j.OpEnd = s.rt.Clock.Now()
		}
		s.jobs[j.Job.Name] = j
		s.ops[j.Op] = j.Job.Name
	}
}

// saveLocked persists every job; s.mu is held.
func (s *BatchServer) saveLocked() {
	recs := make([]batchRecord, 0, len(s.jobs))
	for _, j := range s.jobs {
		r := batchRecord{Project: j.Project, Job: protoJSON(j.Job), Op: j.Op, OpTime: j.OpTime, OpEnd: j.OpEnd, Cancel: j.Cancel}
		for _, b := range j.Buckets {
			r.Buckets = append(r.Buckets, protoJSON(b))
		}
		recs = append(recs, r)
	}
	s.rt.persist(batchFile, recs)
}

// ResetProject forgets one project's jobs, or every project's for "".
func (s *BatchServer) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, j := range s.jobs {
		if project == "" || j.Project == project {
			j.Cancel = true
			delete(s.jobs, name)
			delete(s.ops, j.Op)
		}
	}
	s.saveLocked()
	return nil
}

func (s *BatchServer) route(r *http.Request) (string, any, error) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	p := strings.Split(path, "/")
	if len(p) < 2 || p[0] != "projects" {
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	project := p[1]
	q := r.URL.Query()
	const pre = "storagebatchoperations.projects.locations."
	switch {
	case len(p) == 3 && p[2] == "locations" && r.Method == http.MethodGet:
		return pre + "list", locationsList(project, []string{"global"}), nil
	case len(p) == 4 && p[2] == "locations" && r.Method == http.MethodGet:
		if p[3] != "global" {
			return pre + "get", nil, notFound("Location projects/%s/locations/%s not found.", project, p[3])
		}
		return pre + "get", location(project, "global"), nil
	case len(p) < 5 || p[2] != "locations":
		return "", nil, notFound("%s not found", r.URL.Path)
	}
	loc := p[3]
	if loc != "global" {
		return "", nil, invalid("Storage batch operations jobs are in location global only, not %q.", loc)
	}
	parent := "projects/" + project + "/locations/global"
	switch p[4] {
	case "jobs":
		switch {
		case len(p) == 5 && r.Method == http.MethodPost:
			job := &sbpb.Job{}
			if err := decode(r, job); err != nil {
				return pre + "jobs.create", nil, err
			}
			out, err := s.create(r.Context(), project, parent, q.Get("jobId"), q.Get("requestId"), job)
			return pre + "jobs.create", out, err
		case len(p) == 5 && r.Method == http.MethodGet:
			out, err := s.list(parent, q)
			return pre + "jobs.list", out, err
		}
		name := parent + "/jobs/" + strings.Join(p[5:], "/")
		if len(p) == 6 {
			id, verb, _ := strings.Cut(p[5], ":")
			name = parent + "/jobs/" + id
			switch {
			case verb == "cancel" && r.Method == http.MethodPost:
				return pre + "jobs.cancel", map[string]any{}, s.cancel(name)
			case verb == "" && r.Method == http.MethodGet:
				out, err := s.get(name)
				return pre + "jobs.get", out, err
			case verb == "" && r.Method == http.MethodDelete:
				return pre + "jobs.delete", map[string]any{}, s.delete(name)
			}
		}
		if len(p) >= 7 && p[6] == "bucketOperations" && r.Method == http.MethodGet {
			job := parent + "/jobs/" + p[5]
			if len(p) == 7 {
				out, err := s.bucketOps(job, q)
				return pre + "jobs.bucketOperations.list", out, err
			}
			if len(p) == 8 {
				out, err := s.bucketOp(job, p[7])
				return pre + "jobs.bucketOperations.get", out, err
			}
		}
	case "operations":
		if len(p) == 5 && r.Method == http.MethodGet {
			out, err := s.listOps(parent, q)
			return pre + "operations.list", out, err
		}
		if len(p) == 6 {
			id, verb, _ := strings.Cut(p[5], ":")
			name := parent + "/operations/" + id
			switch {
			case verb == "cancel" && r.Method == http.MethodPost:
				return pre + "operations.cancel", map[string]any{}, s.cancelOp(name)
			case verb == "" && r.Method == http.MethodGet:
				out, err := s.operation(name)
				return pre + "operations.get", out, err
			case verb == "" && r.Method == http.MethodDelete:
				return pre + "operations.delete", map[string]any{}, s.deleteOp(name)
			}
		}
	}
	return "", nil, notFound("%s not found", r.URL.Path)
}

func location(project, loc string) map[string]any {
	return map[string]any{"name": "projects/" + project + "/locations/" + loc, "locationId": loc, "displayName": loc}
}

func locationsList(project string, locs []string) map[string]any {
	out := []any{}
	for _, l := range locs {
		out = append(out, location(project, l))
	}
	return map[string]any{"locations": out}
}

// bucketSource is a bucket of a job's source, with its object selection.
type bucketSource struct {
	bucket   string
	prefixes []string
	manifest string
}

func (s *BatchServer) create(ctx context.Context, project, parent, id, requestID string, job *sbpb.Job) (any, error) {
	if id == "" {
		return nil, invalid("job_id is required.")
	}
	if !jobIDPattern.MatchString(id) {
		return nil, invalid("Invalid job_id %q: it must be 1 to 128 lowercase letters, digits and hyphens, starting and ending with a letter or digit.", id)
	}
	srcs, err := jobSources(job)
	if err != nil {
		return nil, err
	}
	if n := transformations(job); n != 1 {
		return nil, invalid("A job must specify exactly one transformation (put_object_hold, delete_object, put_metadata, rewrite_object or update_object_custom_context); it has %d.", n)
	}
	if rw := job.GetRewriteObject(); rw != nil && !kmsKeyPattern.MatchString(rw.GetKmsKey()) {
		return nil, invalid("rewrite_object.kms_key %q must be projects/P/locations/L/keyRings/R/cryptoKeys/K.", rw.GetKmsKey())
	}
	name := parent + "/jobs/" + id
	s.mu.Lock()
	if old, ok := s.jobs[name]; ok {
		s.mu.Unlock()
		if requestID != "" && old.Request == requestID {
			return s.operation(old.Op)
		}
		return nil, exists("Job %s already exists.", name)
	}
	s.mu.Unlock()
	// Each bucket must exist, must not be Requester Pays, and its project
	// must have Storage Intelligence (Google: each bucket "must be enrolled
	// in a Storage Intelligence plan").
	for _, src := range srcs {
		var b struct {
			Bucket
			Billing *struct {
				RequesterPays bool `json:"requesterPays"`
			} `json:"billing"`
		}
		if err := s.rt.GCS.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(src.bucket), nil, nil, &b); err != nil {
			if IsNotFound(err) {
				return nil, notFound("Bucket %s not found.", src.bucket)
			}
			return nil, err
		}
		if b.Billing != nil && b.Billing.RequesterPays {
			return nil, precondition("Bucket %s has Requester Pays enabled, which storage batch operations do not support.", src.bucket)
		}
	}
	edition, err := s.rt.GCS.IntelligenceEdition(ctx, project)
	if err != nil && !IsNotFound(err) {
		var ae *apiError
		if errors.As(err, &ae) {
			return nil, err
		}
	}
	switch edition {
	case "STANDARD", "TRIAL", "ESSENTIALS":
	default:
		return nil, precondition("Storage batch operations need Storage Intelligence: project %s has no Storage Intelligence "+
			"configuration with edition STANDARD or TRIAL (effective edition %q). Enable it with "+
			"gcloud storage intelligence-configs enable --project=%s.", project, edition, project)
	}
	now := s.rt.Clock.Now()
	j := proto.Clone(job).(*sbpb.Job)
	j.Name = name
	j.CreateTime = timestamppb.New(now)
	j.ScheduleTime = timestamppb.New(now)
	j.State = sbpb.Job_RUNNING
	j.Counters = &sbpb.Counters{}
	j.CompleteTime = nil
	j.ErrorSummaries = nil
	j.IsMultiBucketJob = len(srcs) > 1
	bj := &batchJob{Project: project, Job: j, Op: parent + "/operations/" + newID(12), OpTime: now, Request: requestID}
	for _, src := range srcs {
		bo := &sbpb.BucketOperation{Name: name + "/bucketOperations/" + src.bucket, BucketName: src.bucket,
			CreateTime: timestamppb.New(now), State: sbpb.BucketOperation_QUEUED, Counters: &sbpb.Counters{}}
		if src.manifest != "" {
			bo.ObjectConfiguration = &sbpb.BucketOperation_Manifest{Manifest: &sbpb.Manifest{ManifestLocation: src.manifest}}
		} else {
			bo.ObjectConfiguration = &sbpb.BucketOperation_PrefixList{PrefixList: &sbpb.PrefixList{IncludedObjectPrefixes: src.prefixes}}
		}
		setBucketTransformation(bo, j)
		bj.Buckets = append(bj.Buckets, bo)
	}
	s.mu.Lock()
	s.jobs[name] = bj
	s.ops[bj.Op] = name
	s.saveLocked()
	s.mu.Unlock()
	go s.run(bj, srcs)
	return s.operation(bj.Op)
}

var kmsKeyPattern = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)

func jobSources(job *sbpb.Job) ([]bucketSource, error) {
	bl := job.GetBucketList()
	if bl == nil || len(bl.GetBuckets()) == 0 {
		return nil, invalid("A job's source must be a bucket_list with at least one bucket.")
	}
	if len(bl.GetBuckets()) > MaxBatchBuckets {
		return nil, invalid("A job can name at most %d buckets; it names %d.", MaxBatchBuckets, len(bl.GetBuckets()))
	}
	var out []bucketSource
	prefixes := 0
	seen := map[string]bool{}
	for _, b := range bl.GetBuckets() {
		if b.GetBucket() == "" {
			return nil, invalid("bucket_list.buckets[].bucket is required.")
		}
		if seen[b.GetBucket()] {
			return nil, invalid("Bucket %s is named twice in bucket_list.", b.GetBucket())
		}
		seen[b.GetBucket()] = true
		src := bucketSource{bucket: b.GetBucket()}
		switch {
		case b.GetManifest() != nil:
			loc := b.GetManifest().GetManifestLocation()
			if !strings.HasPrefix(loc, "gs://") || !strings.Contains(strings.TrimPrefix(loc, "gs://"), "/") {
				return nil, invalid("manifest.manifest_location %q must be gs://BUCKET/OBJECT.", loc)
			}
			src.manifest = loc
		case b.GetPrefixList() != nil:
			src.prefixes = b.GetPrefixList().GetIncludedObjectPrefixes()
			prefixes += len(src.prefixes)
		default:
			return nil, invalid("Bucket %s needs a prefix_list or a manifest.", b.GetBucket())
		}
		out = append(out, src)
	}
	if prefixes > MaxBatchPrefixes {
		return nil, invalid("A job can name at most %d object prefixes; it names %d.", MaxBatchPrefixes, prefixes)
	}
	return out, nil
}

func transformations(j *sbpb.Job) int {
	n := 0
	for _, set := range []bool{j.GetPutObjectHold() != nil, j.GetDeleteObject() != nil, j.GetPutMetadata() != nil,
		j.GetRewriteObject() != nil, j.GetUpdateObjectCustomContext() != nil} {
		if set {
			n++
		}
	}
	return n
}

func setBucketTransformation(bo *sbpb.BucketOperation, j *sbpb.Job) {
	switch t := j.Transformation.(type) {
	case *sbpb.Job_PutObjectHold:
		bo.Transformation = &sbpb.BucketOperation_PutObjectHold{PutObjectHold: t.PutObjectHold}
	case *sbpb.Job_DeleteObject:
		bo.Transformation = &sbpb.BucketOperation_DeleteObject{DeleteObject: t.DeleteObject}
	case *sbpb.Job_PutMetadata:
		bo.Transformation = &sbpb.BucketOperation_PutMetadata{PutMetadata: t.PutMetadata}
	case *sbpb.Job_RewriteObject:
		bo.Transformation = &sbpb.BucketOperation_RewriteObject{RewriteObject: t.RewriteObject}
	case *sbpb.Job_UpdateObjectCustomContext:
		bo.Transformation = &sbpb.BucketOperation_UpdateObjectCustomContext{UpdateObjectCustomContext: t.UpdateObjectCustomContext}
	}
}

// target is one object a job acts on.
type target struct {
	bucket, name string
	gen          int64
	size         int64
}

// run runs a job to its end.
func (s *BatchServer) run(bj *batchJob, srcs []bucketSource) {
	ctx := context.Background()
	failedJob := ""
	for i, src := range srcs {
		if s.cancelled(bj) {
			break
		}
		bo := bj.Buckets[i]
		s.mu.Lock()
		bo.State = sbpb.BucketOperation_RUNNING
		bo.StartTime = timestamppb.New(s.rt.Clock.Now())
		s.mu.Unlock()
		targets, err := s.targets(ctx, src)
		if err != nil {
			failedJob = err.Error()
			s.mu.Lock()
			bo.State = sbpb.BucketOperation_FAILED
			bo.CompleteTime = timestamppb.New(s.rt.Clock.Now())
			addError(&bo.ErrorSummaries, statusOf(err), src.manifest, err.Error())
			addError(&bj.Job.ErrorSummaries, statusOf(err), src.manifest, err.Error())
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		bo.Counters.TotalObjectCount = int64(len(targets))
		bj.Job.Counters.TotalObjectCount += int64(len(targets))
		if src.manifest == "" {
			var total int64
			for _, t := range targets {
				total += t.size
			}
			bo.Counters.TotalBytesFound = proto.Int64(total)
			bj.Job.Counters.TotalBytesFound = proto.Int64(bj.Job.Counters.GetTotalBytesFound() + total)
		}
		s.mu.Unlock()
		for _, t := range targets {
			if s.cancelled(bj) {
				break
			}
			var err error
			if !bj.Job.DryRun {
				err = s.apply(ctx, bj.Job, t, bo, bj.Job.Counters)
			}
			s.mu.Lock()
			if err != nil {
				uri := "gs://" + t.bucket + "/" + t.name
				bo.Counters.FailedObjectCount++
				bj.Job.Counters.FailedObjectCount++
				addError(&bo.ErrorSummaries, statusOf(err), uri, err.Error())
				addError(&bj.Job.ErrorSummaries, statusOf(err), uri, err.Error())
			} else {
				bo.Counters.SucceededObjectCount++
				bj.Job.Counters.SucceededObjectCount++
			}
			s.mu.Unlock()
		}
		s.mu.Lock()
		bo.CompleteTime = timestamppb.New(s.rt.Clock.Now())
		if bj.Cancel {
			bo.State = sbpb.BucketOperation_CANCELED
		} else {
			bo.State = sbpb.BucketOperation_SUCCEEDED
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.rt.Clock.Now()
	switch {
	case bj.Cancel:
		bj.Job.State = sbpb.Job_CANCELED
		for _, bo := range bj.Buckets {
			if bo.State == sbpb.BucketOperation_QUEUED || bo.State == sbpb.BucketOperation_RUNNING {
				bo.State = sbpb.BucketOperation_CANCELED
				bo.CompleteTime = timestamppb.New(now)
			}
		}
	case failedJob != "":
		bj.Job.State = sbpb.Job_FAILED
	default:
		bj.Job.State = sbpb.Job_SUCCEEDED
	}
	bj.Job.CompleteTime = timestamppb.New(now)
	bj.OpEnd = now
	if _, ok := s.jobs[bj.Job.Name]; ok {
		s.saveLocked()
	}
}

func (s *BatchServer) cancelled(bj *batchJob) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bj.Cancel
}

func addError(sums *[]*sbpb.ErrorSummary, c int32, uri, detail string) {
	for _, e := range *sums {
		if int32(e.ErrorCode) == c {
			e.ErrorCount++
			if len(e.ErrorLogEntries) < maxErrorLogEntries {
				e.ErrorLogEntries = append(e.ErrorLogEntries, &sbpb.ErrorLogEntry{ObjectUri: uri, ErrorDetails: []string{detail}})
			}
			return
		}
	}
	*sums = append(*sums, &sbpb.ErrorSummary{ErrorCode: code.Code(c), ErrorCount: 1,
		ErrorLogEntries: []*sbpb.ErrorLogEntry{{ObjectUri: uri, ErrorDetails: []string{detail}}}})
}

// targets lists the objects a bucket's source selects.
func (s *BatchServer) targets(ctx context.Context, src bucketSource) ([]target, error) {
	if src.manifest != "" {
		b, o, _ := strings.Cut(strings.TrimPrefix(src.manifest, "gs://"), "/")
		rc, err := s.rt.GCS.Read(ctx, b, o, 0)
		if err != nil {
			return nil, fmt.Errorf("reading manifest %s: %w", src.manifest, err)
		}
		defer rc.Close()
		return manifestTargets(rc, src.bucket)
	}
	prefixes := src.prefixes
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	seen := map[string]bool{}
	var out []target
	for _, p := range prefixes {
		objs, err := s.rt.GCS.List(ctx, src.bucket, p, false)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if seen[o.Name] {
				continue
			}
			seen[o.Name] = true
			out = append(out, target{bucket: src.bucket, name: o.Name, gen: o.Gen(), size: o.SizeBytes()})
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].name < out[k].name })
	return out, nil
}

// manifestTargets reads a manifest's rows: bucket,name[,generation], after
// an optional header row; rows of other buckets are not this bucket's.
func manifestTargets(r io.Reader, bucket string) ([]target, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	var out []target
	first := true
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the manifest is not CSV: %w", err)
		}
		if first {
			first = false
			if len(rec) >= 2 && strings.TrimSpace(rec[0]) == "bucket" && strings.TrimSpace(rec[1]) == "name" {
				continue
			}
		}
		if len(rec) < 2 || rec[0] == "" || rec[1] == "" {
			return nil, fmt.Errorf("manifest row %q needs a bucket and a name", strings.Join(rec, ","))
		}
		if rec[0] != bucket {
			continue
		}
		t := target{bucket: rec[0], name: rec[1]}
		if len(rec) > 2 && strings.TrimSpace(rec[2]) != "" {
			g, err := strconv.ParseInt(strings.TrimSpace(rec[2]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("manifest row %q: generation %q is not a number", strings.Join(rec, ","), rec[2])
			}
			t.gen = g
		}
		out = append(out, t)
	}
}

// apply performs the job's transformation on one object.
func (s *BatchServer) apply(ctx context.Context, j *sbpb.Job, t target, bo *sbpb.BucketOperation, jc *sbpb.Counters) error {
	g := s.rt.GCS
	switch tr := j.Transformation.(type) {
	case *sbpb.Job_DeleteObject:
		gen := t.gen
		if !tr.DeleteObject.GetPermanentObjectDeletionEnabled() && gen > 0 {
			// Not permanent: only the live version, which a versioned
			// bucket keeps as noncurrent.
			o, err := g.Get(ctx, t.bucket, t.name, 0)
			if err != nil {
				return err
			}
			if o.Gen() != gen {
				return precondition("generation %d of gs://%s/%s is not live, and permanent_object_deletion_enabled is false", gen, t.bucket, t.name)
			}
			gen = 0
		}
		if tr.DeleteObject.GetPermanentObjectDeletionEnabled() && gen == 0 {
			o, err := g.Get(ctx, t.bucket, t.name, 0)
			if err != nil {
				return err
			}
			gen = o.Gen()
		}
		return g.Delete(ctx, t.bucket, t.name, gen)
	case *sbpb.Job_PutMetadata:
		_, err := g.Patch(ctx, t.bucket, t.name, t.gen, metadataPatch(tr.PutMetadata))
		return err
	case *sbpb.Job_PutObjectHold:
		body := map[string]any{}
		if v, ok := holdValue(tr.PutObjectHold.GetTemporaryHold()); ok {
			body["temporaryHold"] = v
		}
		if v, ok := holdValue(tr.PutObjectHold.GetEventBasedHold()); ok {
			body["eventBasedHold"] = v
		}
		_, err := g.Patch(ctx, t.bucket, t.name, t.gen, body)
		return err
	case *sbpb.Job_RewriteObject:
		q := url.Values{"destinationKmsKeyName": {tr.RewriteObject.GetKmsKey()}}
		_, err := g.Rewrite(ctx, t.bucket, t.name, t.gen, t.bucket, t.name, q, nil)
		return err
	case *sbpb.Job_UpdateObjectCustomContext:
		return s.updateContexts(ctx, tr.UpdateObjectCustomContext, t, bo, jc)
	}
	return invalid("the job has no transformation")
}

func holdValue(h sbpb.PutObjectHold_HoldStatus) (bool, bool) {
	switch h {
	case sbpb.PutObjectHold_SET:
		return true, true
	case sbpb.PutObjectHold_UNSET:
		return false, true
	}
	return false, false
}

func metadataPatch(m *sbpb.PutMetadata) map[string]any {
	body := map[string]any{}
	set := func(k string, v *string) {
		if v != nil {
			if *v == "" {
				body[k] = nil
			} else {
				body[k] = *v
			}
		}
	}
	set("contentDisposition", m.ContentDisposition)
	set("contentEncoding", m.ContentEncoding)
	set("contentLanguage", m.ContentLanguage)
	set("contentType", m.ContentType)
	set("cacheControl", m.CacheControl)
	set("customTime", m.CustomTime)
	if len(m.GetCustomMetadata()) > 0 {
		md := map[string]any{}
		for k, v := range m.GetCustomMetadata() {
			if v == "" {
				md[k] = nil
			} else {
				md[k] = v
			}
		}
		body["metadata"] = md
	}
	if r := m.GetObjectRetention(); r != nil {
		ret := map[string]any{}
		if r.RetainUntilTime != nil {
			ret["retainUntilTime"] = r.GetRetainUntilTime()
		}
		if r.RetentionMode != nil {
			ret["mode"] = strings.ToTitle(r.GetRetentionMode().String()[:1]) + strings.ToLower(r.GetRetentionMode().String()[1:])
		}
		body["retention"] = ret
	}
	return body
}

// updateContexts edits an object's custom contexts.
func (s *BatchServer) updateContexts(ctx context.Context, u *sbpb.UpdateObjectCustomContext, t target, bo *sbpb.BucketOperation, jc *sbpb.Counters) error {
	var cur struct {
		Contexts struct {
			Custom map[string]json.RawMessage `json:"custom"`
		} `json:"contexts"`
	}
	if err := s.rt.GCS.do(ctx, http.MethodGet, objPath(t.bucket, t.name), genQuery(t.gen), nil, &cur); err != nil {
		return err
	}
	custom := map[string]any{}
	var created, updated, deleted int64
	if u.GetClearAll() {
		for k := range cur.Contexts.Custom {
			custom[k] = nil
			deleted++
		}
	} else if up := u.GetCustomContextUpdates(); up != nil {
		for _, k := range up.GetKeysToClear() {
			if _, ok := cur.Contexts.Custom[k]; ok {
				custom[k] = nil
				deleted++
			}
		}
		for k, v := range up.GetUpdates() {
			custom[k] = map[string]any{"value": v.GetValue()}
			if _, ok := cur.Contexts.Custom[k]; ok {
				updated++
			} else {
				created++
			}
		}
	}
	if len(custom) > 0 {
		if _, err := s.rt.GCS.Patch(ctx, t.bucket, t.name, t.gen, map[string]any{"contexts": map[string]any{"custom": custom}}); err != nil {
			return err
		}
	}
	s.mu.Lock()
	for _, c := range []*sbpb.Counters{bo.Counters, jc} {
		c.ObjectCustomContextsCreated = proto.Int64(c.GetObjectCustomContextsCreated() + created)
		c.ObjectCustomContextsUpdated = proto.Int64(c.GetObjectCustomContextsUpdated() + updated)
		c.ObjectCustomContextsDeleted = proto.Int64(c.GetObjectCustomContextsDeleted() + deleted)
	}
	s.mu.Unlock()
	return nil
}

// expire cancels jobs past Google's 14-day lifetime.
func (s *BatchServer) expire(_ context.Context, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.Job.State == sbpb.Job_RUNNING && now.Sub(j.Job.CreateTime.AsTime()) > MaxBatchJobLifetime {
			j.Cancel = true
		}
	}
}

func (s *BatchServer) snapshot(name string) (*sbpb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	if !ok {
		return nil, notFound("Job %s not found.", name)
	}
	return proto.Clone(j.Job).(*sbpb.Job), nil
}

func (s *BatchServer) get(name string) (any, error) { return s.snapshot(name) }

// Job is a job, for the console.
func (s *BatchServer) Job(name string) (*sbpb.Job, error) { return s.snapshot(name) }

func (s *BatchServer) list(parent string, q url.Values) (any, error) {
	filter := strings.TrimSpace(q.Get("filter"))
	var want string
	if filter != "" {
		m := regexp.MustCompile(`^state\s*[=:]\s*"?([A-Z_]+)"?$`).FindStringSubmatch(filter)
		if m == nil {
			return nil, invalid("Invalid filter %q: only state=STATE is supported.", filter)
		}
		want = m[1]
	}
	s.mu.Lock()
	var jobs []*sbpb.Job
	for _, j := range s.jobs {
		if strings.HasPrefix(j.Job.Name, parent+"/") && (want == "" || j.Job.State.String() == want) {
			jobs = append(jobs, proto.Clone(j.Job).(*sbpb.Job))
		}
	}
	s.mu.Unlock()
	desc := strings.Contains(q.Get("orderBy"), "desc")
	sort.Slice(jobs, func(i, k int) bool {
		a, b := jobs[i].CreateTime.AsTime(), jobs[k].CreateTime.AsTime()
		if a.Equal(b) {
			return jobs[i].Name < jobs[k].Name
		}
		if desc {
			return a.After(b)
		}
		return a.Before(b)
	})
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(jobs, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	return &sbpb.ListJobsResponse{Jobs: page, NextPageToken: next}, nil
}

func (s *BatchServer) cancel(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	if !ok {
		return notFound("Job %s not found.", name)
	}
	if j.Job.State != sbpb.Job_RUNNING && j.Job.State != sbpb.Job_QUEUED {
		return precondition("Job %s is %s: only a running job can be cancelled.", name, j.Job.State)
	}
	j.Cancel = true
	return nil
}

func (s *BatchServer) delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	if !ok {
		return notFound("Job %s not found.", name)
	}
	if j.Job.State == sbpb.Job_RUNNING || j.Job.State == sbpb.Job_QUEUED {
		return precondition("Job %s is still running: cancel it before deleting it.", name)
	}
	delete(s.jobs, name)
	delete(s.ops, j.Op)
	s.saveLocked()
	return nil
}

func (s *BatchServer) bucketOps(job string, q url.Values) (any, error) {
	s.mu.Lock()
	j, ok := s.jobs[job]
	var out []*sbpb.BucketOperation
	if ok {
		for _, b := range j.Buckets {
			out = append(out, proto.Clone(b).(*sbpb.BucketOperation))
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound("Job %s not found.", job)
	}
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(out, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	return &sbpb.ListBucketOperationsResponse{BucketOperations: page, NextPageToken: next}, nil
}

func (s *BatchServer) bucketOp(job, id string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[job]
	if ok {
		for _, b := range j.Buckets {
			if b.BucketName == id {
				return proto.Clone(b).(*sbpb.BucketOperation), nil
			}
		}
	}
	return nil, notFound("Bucket operation %s/bucketOperations/%s not found.", job, id)
}

func (s *BatchServer) operation(name string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jn, ok := s.ops[name]
	if !ok || s.deletedO[name] {
		return nil, notFound("Operation %s not found.", name)
	}
	j := s.jobs[jn]
	return s.opJSON(j), nil
}

// opJSON is a job's operation; s.mu is held.
func (s *BatchServer) opJSON(j *batchJob) map[string]any {
	md := &sbpb.OperationMetadata{Operation: j.Op, CreateTime: timestamppb.New(j.OpTime), Job: proto.Clone(j.Job).(*sbpb.Job),
		RequestedCancellation: j.Cancel, ApiVersion: "v1"}
	done := j.Job.State != sbpb.Job_RUNNING && j.Job.State != sbpb.Job_QUEUED
	if done {
		md.EndTime = timestamppb.New(j.OpEnd)
	}
	op := map[string]any{"name": j.Op, "metadata": marshalAny(batchMetadataType, md), "done": done}
	if done {
		switch j.Job.State {
		case sbpb.Job_CANCELED:
			op["error"] = map[string]any{"code": 1, "message": "The job was cancelled."}
		case sbpb.Job_FAILED:
			msg := "The job failed."
			if len(j.Job.ErrorSummaries) > 0 && len(j.Job.ErrorSummaries[0].ErrorLogEntries) > 0 {
				msg = strings.Join(j.Job.ErrorSummaries[0].ErrorLogEntries[0].ErrorDetails, "; ")
			}
			op["error"] = map[string]any{"code": 9, "message": msg}
		default:
			op["response"] = marshalAny(batchJobType, j.Job)
		}
	}
	return op
}

func (s *BatchServer) listOps(parent string, q url.Values) (any, error) {
	s.mu.Lock()
	var names []string
	for op, jn := range s.ops {
		if strings.HasPrefix(op, parent+"/") && !s.deletedO[op] && s.jobs[jn] != nil {
			names = append(names, op)
		}
	}
	sort.Strings(names)
	ops := []any{}
	for _, n := range names {
		ops = append(ops, s.opJSON(s.jobs[s.ops[n]]))
	}
	s.mu.Unlock()
	size, _ := strconv.Atoi(q.Get("pageSize"))
	page, next, err := pageOf(ops, int32(size), q.Get("pageToken"), 100, 1000)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"operations": page}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

func (s *BatchServer) cancelOp(name string) error {
	s.mu.Lock()
	jn, ok := s.ops[name]
	s.mu.Unlock()
	if !ok {
		return notFound("Operation %s not found.", name)
	}
	return s.cancel(jn)
}

func (s *BatchServer) deleteOp(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[name]; !ok || s.deletedO[name] {
		return notFound("Operation %s not found.", name)
	}
	s.deletedO[name] = true
	return nil
}
