package dataproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// maxLabels: "No more than 32 labels can be associated with a batch."
const maxLabels = 32

func batchDone(st dataprocpb.Batch_State) bool {
	return st == dataprocpb.Batch_SUCCEEDED || st == dataprocpb.Batch_FAILED || st == dataprocpb.Batch_CANCELLED
}

// checkBatch checks a batch's workload as the API reference describes
// each one's fields.
func checkBatch(b *dataprocpb.Batch) error {
	gs := func(field, uri string, required bool) error {
		if uri == "" {
			if required {
				return apierror.InvalidArgument("%s is required", field)
			}
			return nil
		}
		if !strings.HasPrefix(uri, "gs://") && !strings.HasPrefix(uri, "file:///") {
			return apierror.InvalidArgument("%s %q must be a Cloud Storage URI (gs://bucket/object) or a file of the runtime (file:///path)", field, uri)
		}
		if strings.HasPrefix(uri, "gs://") {
			if bucket, object, ok := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/"); !ok || bucket == "" || object == "" {
				return apierror.InvalidArgument("%s %q must be gs://bucket/object", field, uri)
			}
		}
		return nil
	}
	all := func(field string, uris []string) error {
		for _, u := range uris {
			if err := gs(field, u, true); err != nil {
				return err
			}
		}
		return nil
	}
	switch w := b.GetBatchConfig().(type) {
	case *dataprocpb.Batch_PysparkBatch:
		p := w.PysparkBatch
		if err := gs("pyspark_batch.main_python_file_uri", p.GetMainPythonFileUri(), true); err != nil {
			return err
		}
		if !strings.HasSuffix(p.GetMainPythonFileUri(), ".py") {
			return apierror.InvalidArgument("pyspark_batch.main_python_file_uri %q must be a .py file", p.GetMainPythonFileUri())
		}
		for _, e := range []error{all("pyspark_batch.python_file_uris", p.GetPythonFileUris()), all("pyspark_batch.jar_file_uris", p.GetJarFileUris()),
			all("pyspark_batch.file_uris", p.GetFileUris()), all("pyspark_batch.archive_uris", p.GetArchiveUris())} {
			if e != nil {
				return e
			}
		}
	case *dataprocpb.Batch_SparkBatch:
		p := w.SparkBatch
		if p.GetMainJarFileUri() == "" && p.GetMainClass() == "" {
			return apierror.InvalidArgument("spark_batch needs main_jar_file_uri or main_class")
		}
		if err := gs("spark_batch.main_jar_file_uri", p.GetMainJarFileUri(), false); err != nil {
			return err
		}
		for _, e := range []error{all("spark_batch.jar_file_uris", p.GetJarFileUris()), all("spark_batch.file_uris", p.GetFileUris()),
			all("spark_batch.archive_uris", p.GetArchiveUris())} {
			if e != nil {
				return e
			}
		}
	case *dataprocpb.Batch_SparkRBatch:
		p := w.SparkRBatch
		if err := gs("spark_r_batch.main_r_file_uri", p.GetMainRFileUri(), true); err != nil {
			return err
		}
		// "Must be a .R or .r file."
		if !strings.HasSuffix(p.GetMainRFileUri(), ".R") && !strings.HasSuffix(p.GetMainRFileUri(), ".r") {
			return apierror.InvalidArgument("spark_r_batch.main_r_file_uri %q must be a .R or .r file", p.GetMainRFileUri())
		}
		for _, e := range []error{all("spark_r_batch.file_uris", p.GetFileUris()), all("spark_r_batch.archive_uris", p.GetArchiveUris())} {
			if e != nil {
				return e
			}
		}
	case *dataprocpb.Batch_SparkSqlBatch:
		p := w.SparkSqlBatch
		if err := gs("spark_sql_batch.query_file_uri", p.GetQueryFileUri(), true); err != nil {
			return err
		}
		if err := all("spark_sql_batch.jar_file_uris", p.GetJarFileUris()); err != nil {
			return err
		}
	case *dataprocpb.Batch_PysparkNotebookBatch:
		p := w.PysparkNotebookBatch
		if err := gs("pyspark_notebook_batch.notebook_file_uri", p.GetNotebookFileUri(), true); err != nil {
			return err
		}
		return status.Errorf(codes.Unimplemented, "PySpark notebook batches are not served by CloudBurrow yet: run the notebook's code as a pyspark_batch")
	default:
		return apierror.InvalidArgument("batch needs one of pyspark_batch, spark_batch, spark_r_batch and spark_sql_batch")
	}
	if len(b.GetLabels()) > maxLabels {
		return apierror.InvalidArgument("No more than %d labels can be associated with a batch", maxLabels)
	}
	return checkExecution(b.GetEnvironmentConfig().GetExecutionConfig(), false)
}

// checkExecution checks the TTLs: "Minimum value is 10 minutes; maximum
// value is 14 days" for ttl, and for idle_ttl, which "Applies to sessions
// only".
func checkExecution(ec *dataprocpb.ExecutionConfig, session bool) error {
	for _, d := range []struct {
		field string
		v     time.Duration
		set   bool
	}{{"ttl", ec.GetTtl().AsDuration(), ec.GetTtl() != nil}, {"idle_ttl", ec.GetIdleTtl().AsDuration(), ec.GetIdleTtl() != nil}} {
		if !d.set {
			continue
		}
		if d.v < 10*time.Minute || d.v > 14*24*time.Hour {
			return apierror.InvalidArgument("environment_config.execution_config.%s must be between 10 minutes and 14 days", d.field)
		}
	}
	if !session && ec.GetIdleTtl() != nil {
		return apierror.InvalidArgument("environment_config.execution_config.idle_ttl applies to sessions only")
	}
	return nil
}

func (s *Server) CreateBatch(ctx context.Context, req *dataprocpb.CreateBatchRequest) (*longrunningpb.Operation, error) {
	project, location, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetBatch() == nil {
		return nil, apierror.InvalidArgument("batch is required")
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	id := req.GetBatchId()
	if id == "" {
		id = strings.ReplaceAll(newUUID(), "-", "")
	}
	if !idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("batch_id %q must be 4-63 characters; valid characters are /[a-z][0-9]-/", id)
	}
	if err := checkBatch(req.GetBatch()); err != nil {
		return nil, err
	}
	version, err := runtimeVersion(req.GetBatch().GetRuntimeConfig().GetVersion())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "the instance is stopping")
	}
	key := req.GetParent() + "|batch|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		// "If two CreateBatchRequests with the same request_id are
		// received, the second request is ignored and the Operation that
		// corresponds to the first Batch created and stored in the backend
		// is returned."
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	name := req.GetParent() + "/batches/" + id
	if _, ok := s.batches[name]; ok {
		return nil, apierror.AlreadyExists("Batch %s already exists", name)
	}
	now := timestamppb.New(s.now())
	b := clone(req.GetBatch())
	b.Name, b.Uuid, b.CreateTime, b.State, b.StateTime, b.StateMessage = name, newUUID(), now, dataprocpb.Batch_PENDING, now, ""
	b.Creator, b.StateHistory, b.RuntimeInfo = s.caller(ctx), nil, &dataprocpb.RuntimeInfo{}
	if b.RuntimeConfig == nil {
		b.RuntimeConfig = &dataprocpb.RuntimeConfig{}
	}
	b.RuntimeConfig.Version = version
	b.Operation = opName(project, location)
	op := s.newOpLocked(b.Operation, &dataprocpb.BatchOperationMetadata{Batch: name, BatchUuid: b.Uuid, CreateTime: now,
		OperationType: dataprocpb.BatchOperationMetadata_BATCH, Description: "Batch", Labels: b.GetLabels()})
	s.batches[name] = b
	if req.GetRequestId() != "" {
		s.requests[key] = op.Name
	}
	s.save()
	rctx, cancel := context.WithCancel(context.Background())
	s.cancels[name] = cancel
	s.wg.Add(1)
	go s.runBatch(rctx, name)
	return clone(op), nil
}

func (s *Server) batchLocked(name string) (*dataprocpb.Batch, error) {
	if !batchRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/batches/{batch}", name)
	}
	b, ok := s.batches[name]
	if !ok {
		return nil, apierror.NotFound("Batch %s not found", name)
	}
	return b, nil
}

func (s *Server) GetBatch(_ context.Context, req *dataprocpb.GetBatchRequest) (*dataprocpb.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.batchLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(b), nil
}

// batchFilter is ListBatches' filter: "Supported fields are batch_id,
// batch_uuid, state, create_time, and labels".
var batchFilter = []aipfilter.Option{
	aipfilter.Fields("batch_id", "batch_uuid", "state", "create_time", "labels"),
	aipfilter.Computed("batch_id", func(m proto.Message) string { return lastSegment(m.(*dataprocpb.Batch).GetName()) }),
	aipfilter.Alias("batch_uuid", "uuid"),
}

func (s *Server) ListBatches(_ context.Context, req *dataprocpb.ListBatchesRequest) (*dataprocpb.ListBatchesResponse, error) {
	if _, _, err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	// "Currently the only supported sort orders are unspecified (empty)
	// and create_time desc"; unspecified lists them newest first too.
	switch strings.Join(strings.Fields(req.GetOrderBy()), " ") {
	case "", "create_time desc", "createTime desc":
	default:
		return nil, apierror.InvalidArgument("order_by %q: the only supported sort orders are unspecified (empty) and create_time desc", req.GetOrderBy())
	}
	s.mu.Lock()
	var all []*dataprocpb.Batch
	for _, k := range sortedKeys(s.batches) {
		if strings.HasPrefix(k, req.GetParent()+"/batches/") {
			all = append(all, clone(s.batches[k]))
		}
	}
	s.mu.Unlock()
	sortNewest(all, func(b *dataprocpb.Batch) *timestamppb.Timestamp { return b.GetCreateTime() })
	size := int(req.GetPageSize())
	if size == 0 {
		size = 20 // "If unspecified, at most 20 batches will be returned."
	}
	out, next, err := aipfilter.ListPage("dataproc-batches:"+req.GetParent(), req.GetFilter(), "", all, req.GetPageToken(), size, batchFilter...)
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListBatchesResponse{Batches: out, NextPageToken: next}, nil
}

func sortNewest[T any](items []T, at func(T) *timestamppb.Timestamp) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && at(items[j]).AsTime().After(at(items[j-1]).AsTime()); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// DeleteBatch: "If the batch is not in a CANCELLED, SUCCEEDED or FAILED
// State, the delete operation fails and the response returns
// FAILED_PRECONDITION."
func (s *Server) DeleteBatch(_ context.Context, req *dataprocpb.DeleteBatchRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.batchLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if !batchDone(b.State) {
		return nil, apierror.FailedPrecondition("Batch %s is in state %s: only a CANCELLED, SUCCEEDED or FAILED batch can be deleted", b.Name, b.State)
	}
	delete(s.batches, b.Name)
	s.save()
	return &emptypb.Empty{}, nil
}

// setBatchStateLocked moves b to st, its previous state into its history.
func (s *Server) setBatchStateLocked(b *dataprocpb.Batch, st dataprocpb.Batch_State, msg string) {
	b.StateHistory = append(b.StateHistory, &dataprocpb.Batch_StateHistory{State: b.State, StateMessage: b.StateMessage, StateStartTime: b.StateTime})
	b.State, b.StateMessage, b.StateTime = st, msg, timestamppb.New(s.now())
}

// StagingBucket is a project's staging bucket in a region, when a
// workload names none: "a regional staging bucket shared across
// workloads", its name prefixed dataproc-staging- (the staging bucket
// page, read 2026-11-22). The rest of its name, the region, the project's
// number and a suffix, is UNVERIFIED: the page does not give it (docs
// re-read 2026-11-22: https://docs.cloud.google.com/managed-spark/docs/concepts/staging-bucket-serverless).
func (s *Server) StagingBucket(project, location string) string {
	number := project
	if s.opts.ProjectNumber != nil {
		if n := s.opts.ProjectNumber(project); n != "" {
			number = n
		}
	}
	sum := sha256.Sum256([]byte(project + "/" + location))
	return "dataproc-staging-" + location + "-" + number + "-" + hex.EncodeToString(sum[:4])
}

// outputURI is where a batch's driver output is written: its stdout and
// stderr, under the staging bucket's google-cloud-dataproc-metainfo/
// folder as Dataproc writes a job's (UNVERIFIED for batches: the
// reference says only that it points to "the stdout and stderr of the
// workload"; re-read 2026-11-22:
// https://docs.cloud.google.com/managed-spark/docs/reference/rest/v1/RuntimeInfo).
func outputURI(bucket, uuid string) string {
	return "gs://" + bucket + "/google-cloud-dataproc-metainfo/" + uuid + "/jobs/srvls-batch-" + uuid + "/driveroutput"
}

// runBatch runs the batch name to its end, once a worker is free.
func (s *Server) runBatch(ctx context.Context, name string) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if c := s.cancels[name]; c != nil {
			c()
		}
		delete(s.cancels, name)
		s.mu.Unlock()
	}()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.endBatch(name, sparkproc.Result{}, nil, true, time.Time{})
		return
	}
	project, location := batchProjectLocation(name)
	s.mu.Lock()
	b := s.batches[name]
	if b == nil || b.State != dataprocpb.Batch_PENDING {
		s.mu.Unlock()
		s.endBatch(name, sparkproc.Result{}, nil, true, time.Time{})
		return
	}
	bucket := b.GetEnvironmentConfig().GetExecutionConfig().GetStagingBucket()
	ownBucket := bucket == ""
	if ownBucket {
		bucket = s.StagingBucket(project, location)
	}
	b.RuntimeInfo.OutputUri = outputURI(bucket, b.Uuid)
	s.setBatchStateLocked(b, dataprocpb.Batch_RUNNING, "")
	snap := clone(b)
	s.save()
	s.mu.Unlock()
	started := s.now()

	var res sparkproc.Result
	job, err := s.batchJob(ctx, snap, project, location)
	if err == nil {
		if s.opts.Engine == nil {
			err = &sparkproc.Unavailable{Msg: "this instance runs no Apache Spark"}
		} else {
			res, err = s.opts.Engine.Run(ctx, job)
		}
	}
	if s.opts.Storage != nil && (res.DriverOutput != "" || err == nil) {
		wctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if ownBucket {
			if e := s.opts.Storage.EnsureBucket(wctx, project, bucket, location); e != nil {
				s.logf("dataproc: staging bucket %s: %v", bucket, e)
			}
		}
		if e := s.opts.Storage.Write(wctx, snap.RuntimeInfo.OutputUri+".000000000", []byte(res.DriverOutput)); e != nil {
			s.logf("dataproc: driver output of %s: %v", name, e)
		}
		cancel()
	}
	s.endBatch(name, res, err, ctx.Err() != nil, started)
	s.copyToPHS(snap.GetEnvironmentConfig().GetPeripheralsConfig().GetSparkHistoryServerConfig().GetDataprocCluster(), snap.GetUuid())
}

func batchProjectLocation(name string) (string, string) {
	m := batchRE.FindStringSubmatch(name)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// endBatch ends the batch name: CANCELLED when cancelled, FAILED on err or
// a failed application, else SUCCEEDED; its operation ends with it.
func (s *Server) endBatch(name string, res sparkproc.Result, err error, cancelled bool, started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.batches[name]
	if b == nil || batchDone(b.State) {
		return
	}
	switch {
	case cancelled:
		s.setBatchStateLocked(b, dataprocpb.Batch_CANCELLED, "")
		s.finishOpLocked(b.Operation, nil, status.Error(codes.Canceled, "Batch "+name+" was cancelled"))
	case err != nil || res.Failed:
		msg := res.Message
		if err != nil {
			msg = err.Error()
			var u *sparkproc.Unavailable
			if errors.As(err, &u) {
				msg = "Apache Spark is not available on this instance: " + u.Msg
			}
		}
		s.setBatchStateLocked(b, dataprocpb.Batch_FAILED, msg)
		s.finishOpLocked(b.Operation, nil, status.Error(codes.Unknown, "Batch "+name+" failed: "+msg))
	default:
		s.setBatchStateLocked(b, dataprocpb.Batch_SUCCEEDED, "")
	}
	if !started.IsZero() {
		b.RuntimeInfo.ApproximateUsage = usage(b.GetRuntimeConfig().GetProperties(), s.now().Sub(started), timestamppb.New(s.now()))
	}
	if b.State == dataprocpb.Batch_SUCCEEDED {
		s.finishOpLocked(b.Operation, clone(b), nil)
	}
	s.save()
}

// usage is a workload's approximate usage, "calculated when the workload
// completes": its DCUs (one per vCPU of its driver and executors, by
// spark.driver.cores, spark.executor.cores and spark.executor.instances,
// 4, 4 and 2 when unset, Serverless for Apache Spark's defaults) times its
// seconds. How Google weighs memory into a DCU is not modelled.
func usage(props map[string]string, d time.Duration, at *timestamppb.Timestamp) *dataprocpb.UsageMetrics {
	n := func(k string, def int64) int64 {
		if v, err := strconv.ParseInt(props[k], 10, 64); err == nil && v > 0 {
			return v
		}
		return def
	}
	dcu := n("spark.driver.cores", 4) + n("spark.executor.cores", 4)*n("spark.executor.instances", 2)
	secs := int64(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return &dataprocpb.UsageMetrics{MilliDcuSeconds: dcu * 1000 * secs, UpdateTime: at}
}

// readFile is a workload's file: a gs:// object read from the instance's
// Cloud Storage, or a file:/// of the runtime, passed as its path.
func (s *Server) readFile(ctx context.Context, uri string) (sparkproc.File, error) {
	if strings.HasPrefix(uri, "file:///") {
		return sparkproc.File{URI: uri}, nil
	}
	if s.opts.Storage == nil {
		return sparkproc.File{}, fmt.Errorf("%s cannot be read: this instance serves no Cloud Storage", uri)
	}
	b, err := s.opts.Storage.Read(ctx, uri)
	if err != nil {
		return sparkproc.File{}, fmt.Errorf("%s cannot be read: %w", uri, err)
	}
	return sparkproc.File{URI: uri, Data: b}, nil
}

func (s *Server) readFiles(ctx context.Context, uris []string) ([]sparkproc.File, error) {
	var out []sparkproc.File
	for _, u := range uris {
		f, err := s.readFile(ctx, u)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// batchJob is the Spark runner's job for b, its files read.
func (s *Server) batchJob(ctx context.Context, b *dataprocpb.Batch, project, location string) (sparkproc.Job, error) {
	job := baseJob(b.GetUuid(), project, location, b.GetName(), b.GetRuntimeConfig(), b.GetEnvironmentConfig())
	job.Log = &sparkproc.LogTarget{Type: "cloud_dataproc_batch",
		Labels: map[string]string{"project_id": project, "location": location, "batch_id": lastSegment(b.GetName())},
		Output: "dataproc.googleapis.com%2Foutput", Spark: "dataproc.googleapis.com%2Fspark"}
	if s.opts.ProjectNumber != nil && job.KMSKey != "" {
		job.KeyAgent = "service-" + s.opts.ProjectNumber(project) + "@dataproc-accounts.iam.gserviceaccount.com"
	}
	var err error
	one := func(uri string) *sparkproc.File {
		if err != nil || uri == "" {
			return nil
		}
		f, e := s.readFile(ctx, uri)
		if e != nil {
			err = e
			return nil
		}
		return &f
	}
	many := func(uris []string) []sparkproc.File {
		if err != nil {
			return nil
		}
		fs, e := s.readFiles(ctx, uris)
		if e != nil {
			err = e
		}
		return fs
	}
	switch w := b.GetBatchConfig().(type) {
	case *dataprocpb.Batch_PysparkBatch:
		p := w.PysparkBatch
		job.Kind, job.Language, job.Args = sparkproc.KindPySpark, "PYTHON", p.GetArgs()
		job.Main = one(p.GetMainPythonFileUri())
		job.PyFiles, job.Jars, job.Files, job.Archives = many(p.GetPythonFileUris()), many(p.GetJarFileUris()), many(p.GetFileUris()), many(p.GetArchiveUris())
	case *dataprocpb.Batch_SparkBatch:
		p := w.SparkBatch
		job.Kind, job.Language, job.Args, job.MainClass = sparkproc.KindSpark, "JAVA", p.GetArgs(), p.GetMainClass()
		job.Main = one(p.GetMainJarFileUri())
		job.Jars, job.Files, job.Archives = many(p.GetJarFileUris()), many(p.GetFileUris()), many(p.GetArchiveUris())
	case *dataprocpb.Batch_SparkRBatch:
		p := w.SparkRBatch
		job.Kind, job.Language, job.Args = sparkproc.KindSparkR, "R", p.GetArgs()
		job.Main = one(p.GetMainRFileUri())
		job.Files, job.Archives = many(p.GetFileUris()), many(p.GetArchiveUris())
	case *dataprocpb.Batch_SparkSqlBatch:
		p := w.SparkSqlBatch
		job.Kind, job.Language, job.QueryVariables = sparkproc.KindSparkSQL, "SQL", p.GetQueryVariables()
		job.Main = one(p.GetQueryFileUri())
		job.Jars = many(p.GetJarFileUris())
	}
	return job, err
}

// baseJob is a workload's job before its files: its runtime and
// environment.
func baseJob(id, project, location, name string, rc *dataprocpb.RuntimeConfig, env *dataprocpb.EnvironmentConfig) sparkproc.Job {
	ec := env.GetExecutionConfig()
	job := sparkproc.Job{SparkJobID: id, Project: project, Location: location, Procedure: name,
		Properties: rc.GetProperties(), RuntimeVersion: rc.GetVersion(), ContainerImage: rc.GetContainerImage(),
		ServiceAccount: ec.GetServiceAccount(), KMSKey: ec.GetKmsKey(), AppID: appID(id)}
	if sa := ec.GetServiceAccount(); sa != "" {
		// The workload runs as its service account: the BigQuery
		// connector's token is one minted for it.
		job.RunAs = sa
	}
	return job
}
