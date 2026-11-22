package dataproc

// Dataproc jobs (#2162): projects.regions.jobs, as the API reference
// documents them (https://docs.cloud.google.com/dataproc/docs/reference/rest/v1/projects.regions.jobs
// and jobs.proto's field comments). A job runs on a RUNNING cluster: its
// driver a container on the cluster's network, spark-submit in client
// mode against the cluster's master (Nodes.Submit). PySpark, Spark, Spark
// SQL and SparkR jobs run; Hadoop MapReduce, Hive, Pig, Presto, Trino and
// Flink jobs need engines a local cluster does not run, and are
// UNIMPLEMENTED with that reason. The driver's output is written to the
// cluster's staging bucket, at driverOutputResourceUri.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// jobIDRE: "The ID must contain only letters (a-z, A-Z), numbers (0-9),
// underscores (_), or hyphens (-). The maximum length is 100 characters."
var jobIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func jobKey(project, region, id string) string {
	return "projects/" + project + "/regions/" + region + "/jobs/" + id
}

func jobDone(j *dataprocpb.Job) bool {
	switch j.GetStatus().GetState() {
	case dataprocpb.JobStatus_DONE, dataprocpb.JobStatus_ERROR, dataprocpb.JobStatus_CANCELLED:
		return true
	}
	return false
}

func jobActive(j *dataprocpb.Job) bool { return !jobDone(j) }

// JobType is the job's type as the console names it.
func JobType(j interface {
	GetPysparkJob() *dataprocpb.PySparkJob
	GetSparkJob() *dataprocpb.SparkJob
	GetSparkSqlJob() *dataprocpb.SparkSqlJob
	GetSparkRJob() *dataprocpb.SparkRJob
	GetHiveJob() *dataprocpb.HiveJob
	GetPigJob() *dataprocpb.PigJob
	GetHadoopJob() *dataprocpb.HadoopJob
	GetPrestoJob() *dataprocpb.PrestoJob
	GetTrinoJob() *dataprocpb.TrinoJob
	GetFlinkJob() *dataprocpb.FlinkJob
}) string {
	switch {
	case j.GetPysparkJob() != nil:
		return "PySpark"
	case j.GetSparkJob() != nil:
		return "Spark"
	case j.GetSparkSqlJob() != nil:
		return "SparkSql"
	case j.GetSparkRJob() != nil:
		return "SparkR"
	case j.GetHiveJob() != nil:
		return "Hive"
	case j.GetPigJob() != nil:
		return "Pig"
	case j.GetHadoopJob() != nil:
		return "Hadoop"
	case j.GetPrestoJob() != nil:
		return "Presto"
	case j.GetTrinoJob() != nil:
		return "Trino"
	case j.GetFlinkJob() != nil:
		return "Flink"
	}
	return ""
}

// unservedJob is why a job type does not run on a local cluster.
func unservedJob(j *dataprocpb.Job) error {
	why := map[string]string{
		"Hadoop": "Hadoop MapReduce jobs need YARN and the Hadoop MapReduce framework, which a local cluster (Spark's standalone scheduler) does not run",
		"Hive":   "Hive jobs need Apache Hive's HiveServer2 and metastore, which a local cluster does not run",
		"Pig":    "Pig jobs need Apache Pig on Hadoop MapReduce or Tez, which a local cluster does not run",
		"Presto": "Presto jobs need the Presto optional component, which is not available on image versions 2.1 and later",
		"Trino":  "Trino jobs need the Trino optional component, which a local cluster does not run",
		"Flink":  "Flink jobs need the Flink optional component, which a local cluster does not run",
	}[JobType(j)]
	if why == "" {
		return nil
	}
	return status.Errorf(codes.Unimplemented, "%s jobs are not served by CloudBurrow: %s. Submit a PySpark, Spark, SparkSql or SparkR job", JobType(j), why)
}

func checkJobFiles(j *dataprocpb.Job) error {
	gs := func(field, uri string, required bool) error {
		if uri == "" {
			if required {
				return apierror.InvalidArgument("%s is required", field)
			}
			return nil
		}
		if !strings.HasPrefix(uri, "gs://") && !strings.HasPrefix(uri, "file:///") {
			return apierror.InvalidArgument("%s %q must be a Cloud Storage URI (gs://bucket/object) or a file of the cluster (file:///path)", field, uri)
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
	first := func(errs ...error) error {
		for _, e := range errs {
			if e != nil {
				return e
			}
		}
		return nil
	}
	switch {
	case j.GetPysparkJob() != nil:
		p := j.GetPysparkJob()
		if err := gs("pyspark_job.main_python_file_uri", p.GetMainPythonFileUri(), true); err != nil {
			return err
		}
		if !strings.HasSuffix(p.GetMainPythonFileUri(), ".py") {
			return apierror.InvalidArgument("pyspark_job.main_python_file_uri %q must be a .py file", p.GetMainPythonFileUri())
		}
		return first(all("pyspark_job.python_file_uris", p.GetPythonFileUris()), all("pyspark_job.jar_file_uris", p.GetJarFileUris()),
			all("pyspark_job.file_uris", p.GetFileUris()), all("pyspark_job.archive_uris", p.GetArchiveUris()))
	case j.GetSparkJob() != nil:
		p := j.GetSparkJob()
		if p.GetMainClass() == "" && p.GetMainJarFileUri() == "" {
			return apierror.InvalidArgument("spark_job needs main_jar_file_uri or main_class")
		}
		return first(gs("spark_job.main_jar_file_uri", p.GetMainJarFileUri(), false), all("spark_job.jar_file_uris", p.GetJarFileUris()),
			all("spark_job.file_uris", p.GetFileUris()), all("spark_job.archive_uris", p.GetArchiveUris()))
	case j.GetSparkSqlJob() != nil:
		p := j.GetSparkSqlJob()
		if p.GetQueryFileUri() == "" && len(p.GetQueryList().GetQueries()) == 0 {
			return apierror.InvalidArgument("spark_sql_job needs query_file_uri or query_list")
		}
		return first(gs("spark_sql_job.query_file_uri", p.GetQueryFileUri(), false), all("spark_sql_job.jar_file_uris", p.GetJarFileUris()))
	case j.GetSparkRJob() != nil:
		p := j.GetSparkRJob()
		if err := gs("spark_r_job.main_r_file_uri", p.GetMainRFileUri(), true); err != nil {
			return err
		}
		if !strings.HasSuffix(p.GetMainRFileUri(), ".R") && !strings.HasSuffix(p.GetMainRFileUri(), ".r") {
			return apierror.InvalidArgument("spark_r_job.main_r_file_uri %q must be a .R or .r file", p.GetMainRFileUri())
		}
		return first(all("spark_r_job.file_uris", p.GetFileUris()), all("spark_r_job.archive_uris", p.GetArchiveUris()))
	}
	if err := unservedJob(j); err != nil {
		return err
	}
	return apierror.InvalidArgument("job needs one of pyspark_job, spark_job, spark_sql_job, spark_r_job, hadoop_job, hive_job, pig_job, presto_job, trino_job and flink_job")
}

// checkScheduling: "A job may be reported as thrashing if the driver
// exits with a non-zero code four times within a 10-minute window.
// Maximum value is 10" (max_failures_per_hour), and 240 for
// max_failures_total.
func checkScheduling(sc *dataprocpb.JobScheduling) error {
	if sc.GetMaxFailuresPerHour() < 0 || sc.GetMaxFailuresPerHour() > 10 {
		return apierror.InvalidArgument("scheduling.max_failures_per_hour must be between 0 and 10")
	}
	if sc.GetMaxFailuresTotal() < 0 || sc.GetMaxFailuresTotal() > 240 {
		return apierror.InvalidArgument("scheduling.max_failures_total must be between 0 and 240")
	}
	return nil
}

func (s *Server) SubmitJob(ctx context.Context, req *dataprocpb.SubmitJobRequest) (*dataprocpb.Job, error) {
	j, _, err := s.submitJob(ctx, req, false)
	return j, err
}

func (s *Server) SubmitJobAsOperation(ctx context.Context, req *dataprocpb.SubmitJobRequest) (*longrunningpb.Operation, error) {
	_, op, err := s.submitJob(ctx, req, true)
	return op, err
}

func (s *Server) submitJob(ctx context.Context, req *dataprocpb.SubmitJobRequest, asOp bool) (*dataprocpb.Job, *longrunningpb.Operation, error) {
	if err := checkRegion(req.GetProjectId(), req.GetRegion()); err != nil {
		return nil, nil, err
	}
	if req.GetJob() == nil {
		return nil, nil, apierror.InvalidArgument("job is required")
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	j := clone(req.GetJob())
	if err := checkJobFiles(j); err != nil {
		return nil, nil, err
	}
	if err := checkScheduling(j.GetScheduling()); err != nil {
		return nil, nil, err
	}
	if len(j.GetLabels()) > maxLabels {
		return nil, nil, apierror.InvalidArgument("No more than %d labels can be associated with a job", maxLabels)
	}
	if j.Reference == nil {
		j.Reference = &dataprocpb.JobReference{}
	}
	if p := j.Reference.GetProjectId(); p != "" && p != req.GetProjectId() {
		return nil, nil, apierror.InvalidArgument("job.reference.project_id %q must match the request's project %q", p, req.GetProjectId())
	}
	j.Reference.ProjectId = req.GetProjectId()
	if j.Reference.JobId == "" {
		j.Reference.JobId = strings.ReplaceAll(newUUID(), "-", "")
	}
	if !jobIDRE.MatchString(j.Reference.JobId) {
		return nil, nil, apierror.InvalidArgument("job_id %q must contain only letters, numbers, underscores and hyphens, at most 100", j.Reference.JobId)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, status.Error(codes.Unavailable, "the instance is stopping")
	}
	rkey := req.GetProjectId() + "/" + req.GetRegion() + "|job|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if name, ok := s.requests[rkey]; ok {
			if op, ok := s.ops[name]; ok && asOp {
				return nil, clone(op), nil
			}
			if jb, ok := s.jobs[name]; ok {
				return clone(jb), nil, nil
			}
		}
	}
	name := jobKey(req.GetProjectId(), req.GetRegion(), j.Reference.JobId)
	if _, ok := s.jobs[name]; ok {
		return nil, nil, apierror.AlreadyExists("Already exists: Failed to submit job: Job %s", name)
	}
	if j.Placement == nil {
		return nil, nil, apierror.InvalidArgument("job.placement is required: the cluster_name, or cluster_labels, of the cluster to run on")
	}
	c, err := s.placeLocked(req.GetProjectId(), req.GetRegion(), j.Placement)
	if err != nil {
		return nil, nil, err
	}
	j.Placement.ClusterName, j.Placement.ClusterUuid = c.ClusterName, c.ClusterUuid
	j.JobUuid, j.Done, j.StatusHistory, j.YarnApplications = newUUID(), false, nil, nil
	control := "gs://" + c.GetConfig().GetConfigBucket() + "/google-cloud-dataproc-metainfo/" + c.ClusterUuid + "/jobs/" + j.Reference.JobId + "/"
	j.DriverControlFilesUri, j.DriverOutputResourceUri = control, control+"driveroutput"
	j.Status = &dataprocpb.JobStatus{State: dataprocpb.JobStatus_PENDING, StateStartTime: timestamppb.New(s.now())}
	s.jobs[name] = j
	var op *longrunningpb.Operation
	if asOp {
		op = s.newOpLocked(opName(req.GetProjectId(), req.GetRegion()), &dataprocpb.JobMetadata{JobId: j.Reference.JobId,
			Status: clone(j.Status), OperationType: "Job", StartTime: timestamppb.New(s.now())})
		s.jobOps[name] = op.Name
	}
	if req.GetRequestId() != "" {
		s.requests[rkey] = name
		if op != nil {
			s.requests[rkey] = op.Name
		}
	}
	s.save()
	rctx, cancel := context.WithCancel(context.Background())
	s.cancels[name] = cancel
	s.wg.Add(1)
	go s.runJob(rctx, name)
	if op != nil {
		return clone(j), clone(op), nil
	}
	return clone(j), nil, nil
}

// placeLocked is the RUNNING cluster a placement names: by name, or
// the first (alphabetically) whose labels hold every cluster_labels.
func (s *Server) placeLocked(project, region string, p *dataprocpb.JobPlacement) (*dataprocpb.Cluster, error) {
	var c *dataprocpb.Cluster
	if p.GetClusterName() != "" {
		var err error
		if c, err = s.clusterLocked(project, region, p.GetClusterName(), ""); err != nil {
			return nil, err
		}
	} else if len(p.GetClusterLabels()) > 0 {
		prefix := "projects/" + project + "/regions/" + region + "/clusters/"
		for _, k := range sortedKeys(s.clusters) {
			x := s.clusters[k]
			if !strings.HasPrefix(k, prefix) || x.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
				continue
			}
			ok := true
			for lk, lv := range p.GetClusterLabels() {
				ok = ok && x.GetLabels()[lk] == lv
			}
			if ok {
				c = x
				break
			}
		}
		if c == nil {
			return nil, apierror.NotFound("No RUNNING cluster in %s has the labels %v", region, p.GetClusterLabels())
		}
	} else {
		return nil, apierror.InvalidArgument("job.placement.cluster_name is required")
	}
	if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING && c.GetStatus().GetState() != dataprocpb.ClusterStatus_UPDATING {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: jobs run on a RUNNING cluster", c.ClusterName, c.GetStatus().GetState())
	}
	return c, nil
}

func (s *Server) setJobStateLocked(name string, j *dataprocpb.Job, st dataprocpb.JobStatus_State, details string) {
	if j.Status != nil {
		j.StatusHistory = append(j.StatusHistory, j.Status)
	}
	j.Status = &dataprocpb.JobStatus{State: st, Details: details, StateStartTime: timestamppb.New(s.now())}
	j.Done = jobDone(j)
	if opn := s.jobOps[name]; opn != "" {
		if op := s.ops[opn]; op != nil && !op.Done {
			var md dataprocpb.JobMetadata
			if op.GetMetadata().UnmarshalTo(&md) == nil {
				md.Status = clone(j.Status)
				op.Metadata, _ = anyOf(&md)
			}
		}
	}
}

var jobRE = regexp.MustCompile(`^projects/([^/]+)/regions/([^/]+)/jobs/([^/]+)$`)

// runJob runs the job name on its cluster, restarting it as its
// scheduling allows.
func (s *Server) runJob(ctx context.Context, name string) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if c := s.cancels[name]; c != nil {
			c()
		}
		delete(s.cancels, name)
		s.mu.Unlock()
	}()
	m := jobRE.FindStringSubmatch(name)
	project, region := m[1], m[2]
	var failures []time.Time
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		j := s.jobs[name]
		if j == nil || jobDone(j) {
			s.mu.Unlock()
			return
		}
		cname := clusterKey(project, region, j.GetPlacement().GetClusterName())
		c := s.clusters[cname]
		nodes := s.nodes[cname]
		if c == nil || nodes == nil || c.ClusterUuid != j.GetPlacement().GetClusterUuid() {
			s.setJobStateLocked(name, j, dataprocpb.JobStatus_ERROR, "The job's cluster is not running")
			s.endJobOpLocked(name, j)
			s.save()
			s.mu.Unlock()
			return
		}
		snap := clone(j)
		cluster := clone(c)
		s.setJobStateLocked(name, j, dataprocpb.JobStatus_SETUP_DONE, "")
		s.save()
		s.mu.Unlock()

		job, err := s.clusterJob(ctx, snap, cluster, project, region)
		var res sparkproc.Result
		if err == nil {
			s.mu.Lock()
			if j := s.jobs[name]; j != nil && !jobDone(j) && j.GetStatus().GetState() == dataprocpb.JobStatus_SETUP_DONE {
				s.setJobStateLocked(name, j, dataprocpb.JobStatus_RUNNING, "")
				j.YarnApplications = []*dataprocpb.YarnApplication{{Name: snap.GetReference().GetJobId(), State: dataprocpb.YarnApplication_RUNNING,
					TrackingUrl: gatewayBase(cluster.ClusterUuid) + "sparkhistory/"}}
				s.save()
			}
			s.mu.Unlock()
			res, err = nodes.Submit(ctx, job)
		}
		s.writeDriverOutput(snap, project, region, cluster, res.DriverOutput, attempt)
		s.uploadEventLog(cluster, nodes, res.AppID)
		s.mu.Lock()
		j = s.jobs[name]
		if j == nil {
			s.mu.Unlock()
			return
		}
		if len(j.YarnApplications) > 0 {
			app := j.YarnApplications[0]
			app.Progress = 1
			app.State = dataprocpb.YarnApplication_FINISHED
			if res.AppID != "" {
				app.TrackingUrl = gatewayBase(cluster.ClusterUuid) + "sparkhistory/history/" + res.AppID + "/jobs/"
			}
			if ctx.Err() != nil {
				app.State = dataprocpb.YarnApplication_KILLED
			} else if err != nil || res.Failed {
				app.State = dataprocpb.YarnApplication_FAILED
			}
		}
		switch {
		case ctx.Err() != nil:
			s.setJobStateLocked(name, j, dataprocpb.JobStatus_CANCELLED, "")
		case err != nil || res.Failed:
			msg := res.Message
			if err != nil {
				msg = err.Error()
				var u *sparkproc.Unavailable
				if errors.As(err, &u) {
					msg = "Apache Spark is not available on this instance: " + u.Msg
				}
			}
			now := s.now()
			failures = append(failures, now)
			hour := 0
			for _, f := range failures {
				if now.Sub(f) < time.Hour {
					hour++
				}
			}
			sc := j.GetScheduling()
			if (sc.GetMaxFailuresPerHour() > 0 && hour <= int(sc.GetMaxFailuresPerHour())) ||
				(sc.GetMaxFailuresTotal() > 0 && len(failures) <= int(sc.GetMaxFailuresTotal())) {
				// A restartable job's driver failed: "The job is being
				// retried" (ATTEMPT_FAILURE), then it runs again.
				s.setJobStateLocked(name, j, dataprocpb.JobStatus_ATTEMPT_FAILURE, msg)
				s.setJobStateLocked(name, j, dataprocpb.JobStatus_PENDING, "")
				s.save()
				s.mu.Unlock()
				continue
			}
			s.setJobStateLocked(name, j, dataprocpb.JobStatus_ERROR, msg)
		default:
			s.setJobStateLocked(name, j, dataprocpb.JobStatus_DONE, "")
		}
		s.endJobOpLocked(name, j)
		if c := s.clusters[cname]; c != nil && !s.clusterBusyLocked(c.ClusterUuid) {
			s.markIdleLocked(c)
		}
		s.save()
		s.mu.Unlock()
		return
	}
}

// endJobOpLocked ends a job's SubmitJobAsOperation operation with it.
func (s *Server) endJobOpLocked(name string, j *dataprocpb.Job) {
	opn := s.jobOps[name]
	if opn == "" {
		return
	}
	switch j.GetStatus().GetState() {
	case dataprocpb.JobStatus_DONE:
		s.finishOpLocked(opn, clone(j), nil)
	case dataprocpb.JobStatus_CANCELLED:
		s.finishOpLocked(opn, nil, status.Error(codes.Canceled, "Job "+j.GetReference().GetJobId()+" was cancelled"))
	default:
		s.finishOpLocked(opn, nil, status.Error(codes.Unknown, "Job "+j.GetReference().GetJobId()+" failed: "+j.GetStatus().GetDetails()))
	}
}

// writeDriverOutput writes the driver's output to driverOutputResourceUri,
// as Dataproc writes it: driveroutput.000000000 (attempt 0), .000000001
// after a restart.
func (s *Server) writeDriverOutput(j *dataprocpb.Job, project, region string, c *dataprocpb.Cluster, out string, attempt int) {
	if s.opts.Storage == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bucket := c.GetConfig().GetConfigBucket()
	if bucket == s.StagingBucket(project, region) {
		if err := s.opts.Storage.EnsureBucket(ctx, project, bucket, region); err != nil {
			s.logf("dataproc: staging bucket %s: %v", bucket, err)
		}
	}
	if err := s.opts.Storage.Write(ctx, fmt.Sprintf("%s.%09d", j.GetDriverOutputResourceUri(), attempt), []byte(out)); err != nil {
		s.logf("dataproc: driver output of %s: %v", j.GetReference().GetJobId(), err)
	}
}

// clusterJob is the Spark runner's job for j on cluster c, its files
// read.
func (s *Server) clusterJob(ctx context.Context, j *dataprocpb.Job, c *dataprocpb.Cluster, project, region string) (sparkproc.Job, error) {
	job := sparkproc.Job{SparkJobID: j.GetReference().GetJobId(), Project: project, Location: region,
		Procedure: jobKey(project, region, j.GetReference().GetJobId()), ServiceAccount: c.GetConfig().GetGceClusterConfig().GetServiceAccount()}
	// The driver's output goes to Cloud Logging when the cluster enables
	// it (dataproc:dataproc.logging.stackdriver.job.driver.enable, false
	// by default), as the cloud_dataproc_job resource.
	if c.GetConfig().GetSoftwareConfig().GetProperties()["dataproc:dataproc.logging.stackdriver.job.driver.enable"] == "true" {
		job.Log = &sparkproc.LogTarget{Type: "cloud_dataproc_job",
			Labels: map[string]string{"project_id": project, "region": region, "job_id": j.GetReference().GetJobId(), "job_uuid": j.GetJobUuid()},
			Output: "dataproc.job.driver", Spark: "dataproc.job.driver"}
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
	switch {
	case j.GetPysparkJob() != nil:
		p := j.GetPysparkJob()
		job.Kind, job.Language, job.Args, job.Properties = sparkproc.KindPySpark, "PYTHON", p.GetArgs(), p.GetProperties()
		job.Main = one(p.GetMainPythonFileUri())
		job.PyFiles, job.Jars, job.Files, job.Archives = many(p.GetPythonFileUris()), many(p.GetJarFileUris()), many(p.GetFileUris()), many(p.GetArchiveUris())
	case j.GetSparkJob() != nil:
		p := j.GetSparkJob()
		job.Kind, job.Language, job.Args, job.MainClass, job.Properties = sparkproc.KindSpark, "JAVA", p.GetArgs(), p.GetMainClass(), p.GetProperties()
		job.Main = one(p.GetMainJarFileUri())
		job.Jars, job.Files, job.Archives = many(p.GetJarFileUris()), many(p.GetFileUris()), many(p.GetArchiveUris())
	case j.GetSparkRJob() != nil:
		p := j.GetSparkRJob()
		job.Kind, job.Language, job.Args, job.Properties = sparkproc.KindSparkR, "R", p.GetArgs(), p.GetProperties()
		job.Main = one(p.GetMainRFileUri())
		job.Files, job.Archives = many(p.GetFileUris()), many(p.GetArchiveUris())
	case j.GetSparkSqlJob() != nil:
		p := j.GetSparkSqlJob()
		job.Kind, job.Language, job.QueryVariables, job.Properties = sparkproc.KindSparkSQL, "SQL", p.GetScriptVariables(), p.GetProperties()
		if p.GetQueryFileUri() != "" {
			job.Main = one(p.GetQueryFileUri())
		} else {
			q := strings.Join(p.GetQueryList().GetQueries(), ";\n")
			if !strings.HasSuffix(strings.TrimSpace(q), ";") {
				q += ";"
			}
			job.Main = &sparkproc.File{URI: "query.sql", Data: []byte(q + "\n")}
		}
		job.Jars = many(p.GetJarFileUris())
	}
	return job, err
}

func (s *Server) jobLocked(project, region, id string) (*dataprocpb.Job, error) {
	if err := checkRegion(project, region); err != nil {
		return nil, err
	}
	j, ok := s.jobs[jobKey(project, region, id)]
	if !ok {
		return nil, apierror.NotFound("Not found: Job projects/%s/regions/%s/jobs/%s", project, region, id)
	}
	return j, nil
}

func (s *Server) GetJob(ctx context.Context, req *dataprocpb.GetJobRequest) (*dataprocpb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(req.GetProjectId(), req.GetRegion(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	return renderJob(ctx, j), nil
}

// jobFilter matches ListJobs' filter: status.state (ACTIVE or
// NON_ACTIVE), insertTime (an RFC 3339 time in quotes) and labels.[KEY].
func jobFilter(filter string) (func(*dataprocpb.Job) bool, error) {
	terms, err := andTerms(filter)
	if err != nil {
		return nil, err
	}
	if err := checkTerms(terms, "status.state", "insertTime"); err != nil {
		return nil, err
	}
	for _, t := range terms {
		if t.field == "status.state" && t.value != "ACTIVE" && t.value != "NON_ACTIVE" && t.value != "*" {
			return nil, apierror.InvalidArgument("Invalid filter: status.state can be either ACTIVE or NON_ACTIVE, not %q", t.value)
		}
		if t.field == "insertTime" {
			if _, err := time.Parse(time.RFC3339, t.value); err != nil {
				return nil, apierror.InvalidArgument("Invalid filter: insertTime %q must be an RFC 3339 timestamp", t.value)
			}
		}
	}
	return func(j *dataprocpb.Job) bool {
		for _, t := range terms {
			switch {
			case t.field == "status.state":
				if (t.value == "ACTIVE" && !jobActive(j)) || (t.value == "NON_ACTIVE" && jobActive(j)) {
					return false
				}
			case t.field == "insertTime":
				at, _ := time.Parse(time.RFC3339, t.value)
				ins := insertTime(j)
				ok := map[string]bool{"<": ins.Before(at), "<=": !ins.After(at), ">": ins.After(at), ">=": !ins.Before(at), "=": ins.Equal(at)}[t.op]
				if !ok {
					return false
				}
			case strings.HasPrefix(t.field, "labels."):
				v, ok := j.GetLabels()[strings.TrimPrefix(t.field, "labels.")]
				if !ok || (t.value != "*" && v != t.value) {
					return false
				}
			}
		}
		return true
	}, nil
}

// insertTime is when the job was submitted: its first status's start.
func insertTime(j *dataprocpb.Job) time.Time {
	if len(j.GetStatusHistory()) > 0 {
		return j.GetStatusHistory()[0].GetStateStartTime().AsTime()
	}
	return j.GetStatus().GetStateStartTime().AsTime()
}

func (s *Server) ListJobs(ctx context.Context, req *dataprocpb.ListJobsRequest) (*dataprocpb.ListJobsResponse, error) {
	if err := checkRegion(req.GetProjectId(), req.GetRegion()); err != nil {
		return nil, err
	}
	match, err := jobFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	prefix := "projects/" + req.GetProjectId() + "/regions/" + req.GetRegion() + "/jobs/"
	s.mu.Lock()
	var all []*dataprocpb.Job
	for _, k := range sortedKeys(s.jobs) {
		j := s.jobs[k]
		if !strings.HasPrefix(k, prefix) || !match(j) {
			continue
		}
		if req.GetClusterName() != "" && j.GetPlacement().GetClusterName() != req.GetClusterName() {
			continue
		}
		// "If filter is provided, jobStateMatcher will be ignored."
		if req.GetFilter() == "" {
			switch req.GetJobStateMatcher() {
			case dataprocpb.ListJobsRequest_ACTIVE:
				if !jobActive(j) {
					continue
				}
			case dataprocpb.ListJobsRequest_NON_ACTIVE:
				if jobActive(j) {
					continue
				}
			}
		}
		all = append(all, renderJob(ctx, j))
	}
	s.mu.Unlock()
	sortNewest(all, func(j *dataprocpb.Job) *timestamppb.Timestamp { return timestamppb.New(insertTime(j)) })
	out, next, err := aipfilter.Page("dataproc-jobs:"+prefix+req.GetFilter()+req.GetClusterName()+req.GetJobStateMatcher().String(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListJobsResponse{Jobs: out, NextPageToken: next}, nil
}

// UpdateJob: "Currently, labels is the only field that can be updated."
func (s *Server) UpdateJob(ctx context.Context, req *dataprocpb.UpdateJobRequest) (*dataprocpb.Job, error) {
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	for _, p := range paths {
		if p != "labels" {
			return nil, apierror.InvalidArgument("Invalid update mask path %q: labels is the only field that can be updated", p)
		}
	}
	if len(req.GetJob().GetLabels()) > maxLabels {
		return nil, apierror.InvalidArgument("No more than %d labels can be associated with a job", maxLabels)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(req.GetProjectId(), req.GetRegion(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	j.Labels = req.GetJob().GetLabels()
	s.save()
	return renderJob(ctx, j), nil
}

// CancelJob starts a job's cancellation: CANCEL_PENDING, then
// CANCEL_STARTED as its driver is stopped, then CANCELLED.
func (s *Server) CancelJob(ctx context.Context, req *dataprocpb.CancelJobRequest) (*dataprocpb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(req.GetProjectId(), req.GetRegion(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	if jobDone(j) {
		return nil, apierror.FailedPrecondition("Job %s is in state %s and cannot be cancelled", req.GetJobId(), j.GetStatus().GetState())
	}
	name := jobKey(req.GetProjectId(), req.GetRegion(), req.GetJobId())
	s.setJobStateLocked(name, j, dataprocpb.JobStatus_CANCEL_PENDING, "")
	if c := s.cancels[name]; c != nil {
		s.setJobStateLocked(name, j, dataprocpb.JobStatus_CANCEL_STARTED, "")
		c()
	} else {
		s.setJobStateLocked(name, j, dataprocpb.JobStatus_CANCELLED, "")
		s.endJobOpLocked(name, j)
	}
	s.save()
	return renderJob(ctx, j), nil
}

// DeleteJob: "Deletes the job from the project. If the job is active,
// the delete fails, and the response returns FAILED_PRECONDITION."
func (s *Server) DeleteJob(_ context.Context, req *dataprocpb.DeleteJobRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(req.GetProjectId(), req.GetRegion(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	if !jobDone(j) {
		return nil, apierror.FailedPrecondition("Job %s is in state %s: an active job cannot be deleted", req.GetJobId(), j.GetStatus().GetState())
	}
	name := jobKey(req.GetProjectId(), req.GetRegion(), req.GetJobId())
	delete(s.jobs, name)
	delete(s.jobOps, name)
	s.save()
	return &emptypb.Empty{}, nil
}

// renderJob is a job as the caller sees it: its Spark applications'
// tracking URLs absolute, on the host the API was reached at.
func renderJob(ctx context.Context, j *dataprocpb.Job) *dataprocpb.Job {
	out := clone(j)
	if host := authority(ctx); host != "" {
		for _, a := range out.GetYarnApplications() {
			if strings.HasPrefix(a.TrackingUrl, "/") {
				a.TrackingUrl = "http://" + host + a.TrackingUrl
			}
		}
	}
	return out
}
