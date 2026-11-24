package vertexai

// Vertex AI batch prediction (#2210): JobService's BatchPredictionJob
// methods. A job serves its model's container as DeployModel does (the
// Model Registry's ModelRunner, in the instance's cluster), sends it the
// input's instances in batches on its predict route, writes what it
// answers, then removes the container.
//
// Documented behaviour (the BatchPredictionJob proto, aiplatform v1.127.0;
// https://docs.cloud.google.com/vertex-ai/docs/predictions/get-batch-predictions
// and .../predictions/batch-prediction-format; read 2026-11-24):
//   - display_name, input_config (instances_format and a gcs_source or
//     bigquery_source) and output_config (predictions_format and a
//     gcs_destination or bigquery_destination) are required, with a
//     model (a Model Registry model, optionally @version) or an
//     unmanaged_container_model;
//   - instances_format jsonl (one instance per line), csv (a header, then
//     a list per row) or bigquery (a row an object); predictions_format
//     jsonl, csv or bigquery;
//   - Cloud Storage output goes in a directory
//     prediction-MODEL_DISPLAY_NAME-JOB_CREATE_TIME under the prefix, its
//     time in ISO 8601 (YYYY-MM-DDThh:mm:ss.sssZ), as
//     prediction.results-00000-of-00001 lines {"instance": ..., "prediction":
//     ...}, as "Get batch inferences from a custom trained model" names them
//     (prediction.results-{file_number}-of-{number_of_files_generated}), and
//     prediction.errors_stats-00000-of-00001 lines {"instance": ...,
//     "error": ...};
//     BigQuery output in a dataset prediction_MODEL_DISPLAY_NAME_JOB_CREATE_TIME
//     (YYYY_MM_DDThh_mm_ss_sssZ) of the output_uri's project, its tables
//     predictions and errors; output_info names them;
//   - completion_stats counts the successful, failed and incomplete
//     instances; manual_batch_tuning_parameters.batch_size (default 64) is
//     the instances per request; model_parameters are each request's
//     parameters.
//
// UNVERIFIED (re-read 2026-11-24,
// https://docs.cloud.google.com/vertex-ai/docs/predictions/get-batch-predictions
// and the BatchPredictionJob proto): the file number's padding, the errors
// file's name (the page names none; the proto's output_config, written for
// AutoML, names predictions_0001 and errors_0001 files) and the errors
// table's columns beyond instance and error.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// batchJobEntry is a batch prediction job and its run.
type batchJobEntry struct {
	Job    *aiplatformpb.BatchPredictionJob
	cancel context.CancelFunc
	done   chan struct{}
}

var bqURIRE = regexp.MustCompile(`^bq://([a-z0-9.:-]+)(?:\.([A-Za-z0-9_]+)(?:\.([^.:]+))?)?$`)

func checkBatchJob(in *aiplatformpb.BatchPredictionJob) error {
	if in == nil {
		return apierror.InvalidArgument("batch_prediction_job is required")
	}
	if in.GetDisplayName() == "" {
		return apierror.InvalidArgument("batch_prediction_job.display_name is required")
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		return apierror.InvalidArgument("batch_prediction_job.display_name can be up to 128 characters long")
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return err
	}
	switch {
	case in.GetModel() != "" && in.GetUnmanagedContainerModel() != nil:
		return apierror.InvalidArgument("batch_prediction_job: only one of model and unmanaged_container_model can be set")
	case in.GetModel() != "":
		if _, _, _, _, err := parseModelName("batch_prediction_job.model", in.GetModel()); err != nil {
			return err
		}
	case in.GetUnmanagedContainerModel() != nil:
		if in.GetUnmanagedContainerModel().GetContainerSpec().GetImageUri() == "" {
			return apierror.InvalidArgument("batch_prediction_job.unmanaged_container_model.container_spec.image_uri is required")
		}
	default:
		return apierror.InvalidArgument("batch_prediction_job.model is required (or an unmanaged_container_model)")
	}
	ic := in.GetInputConfig()
	switch ic.GetInstancesFormat() {
	case "jsonl", "csv":
		if len(ic.GetGcsSource().GetUris()) == 0 {
			return apierror.InvalidArgument("batch_prediction_job.input_config.gcs_source.uris is required for instances_format %q", ic.GetInstancesFormat())
		}
		for _, u := range ic.GetGcsSource().GetUris() {
			if !strings.HasPrefix(u, "gs://") {
				return apierror.InvalidArgument("batch_prediction_job.input_config.gcs_source.uris %q is not a gs:// URI", u)
			}
		}
	case "bigquery":
		if m := bqURIRE.FindStringSubmatch(ic.GetBigquerySource().GetInputUri()); m == nil || m[3] == "" {
			return apierror.InvalidArgument("batch_prediction_job.input_config.bigquery_source.input_uri %q is not bq://PROJECT.DATASET.TABLE", ic.GetBigquerySource().GetInputUri())
		}
	case "":
		return apierror.InvalidArgument("batch_prediction_job.input_config.instances_format is required")
	default:
		return apierror.InvalidArgument("batch_prediction_job.input_config.instances_format %q is not supported for a custom-trained model: jsonl, csv and bigquery are (tf-record and file-list are not served)", ic.GetInstancesFormat())
	}
	oc := in.GetOutputConfig()
	switch oc.GetPredictionsFormat() {
	case "jsonl", "csv":
		if p := oc.GetGcsDestination().GetOutputUriPrefix(); !strings.HasPrefix(p, "gs://") {
			return apierror.InvalidArgument("batch_prediction_job.output_config.gcs_destination.output_uri_prefix %q is not a gs:// URI", p)
		}
	case "bigquery":
		if bqURIRE.FindStringSubmatch(oc.GetBigqueryDestination().GetOutputUri()) == nil {
			return apierror.InvalidArgument("batch_prediction_job.output_config.bigquery_destination.output_uri %q is not bq://PROJECT[.DATASET]", oc.GetBigqueryDestination().GetOutputUri())
		}
	case "":
		return apierror.InvalidArgument("batch_prediction_job.output_config.predictions_format is required")
	default:
		return apierror.InvalidArgument("batch_prediction_job.output_config.predictions_format %q is not supported: jsonl, csv and bigquery are", oc.GetPredictionsFormat())
	}
	if n := in.GetManualBatchTuningParameters().GetBatchSize(); n < 0 {
		return apierror.InvalidArgument("batch_prediction_job.manual_batch_tuning_parameters.batch_size must not be negative")
	}
	if sa := in.GetServiceAccount(); sa != "" && !strings.Contains(sa, "@") {
		return apierror.InvalidArgument("batch_prediction_job.service_account %q is not a service account email", sa)
	}
	return nil
}

func (j *jobService) CreateBatchPredictionJob(_ context.Context, req *aiplatformpb.CreateBatchPredictionJobRequest) (*aiplatformpb.BatchPredictionJob, error) {
	out, err := j.s.createBatchJob(req.GetParent(), req.GetBatchPredictionJob())
	if err != nil {
		return nil, grpcError(err)
	}
	return out, nil
}

func (s *Server) createBatchJob(parent string, in *aiplatformpb.BatchPredictionJob) (*aiplatformpb.BatchPredictionJob, error) {
	project, location, err := parseParent("parent", parent)
	if err != nil {
		return nil, err
	}
	if err := checkBatchJob(in); err != nil {
		return nil, err
	}
	r := s.registry()
	if r == nil {
		return nil, apierror.FailedPrecondition("batch prediction serves the model's container in the instance's cluster, as DeployModel does; the Model Registry is not served on this instance (start with --services including run)")
	}
	var model *aiplatformpb.Model
	if m := in.GetModel(); m != "" {
		p, l, _, _, _ := parseModelName("model", m)
		if p != project || l != location {
			return nil, apierror.InvalidArgument("batch_prediction_job.model %q is not in %s", m, parent)
		}
		if model, err = r.get(m); err != nil {
			return nil, err
		}
		if model.GetContainerSpec().GetImageUri() == "" {
			return nil, apierror.FailedPrecondition("Model %s has no container_spec: only a custom-trained model with a serving container can run batch prediction here", m)
		}
	} else if err := r.checkPrebuilt(in.GetUnmanagedContainerModel().GetContainerSpec().GetImageUri()); err != nil {
		return nil, err
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pipelineRunner == nil {
		return nil, apierror.FailedPrecondition("batch prediction reads and writes this instance's Cloud Storage (start with --services including storage)")
	}
	if st.batchJobs == nil {
		st.batchJobs = map[string]*batchJobEntry{}
	}
	name := parent + "/batchPredictionJobs/" + regNumericID()
	for st.batchJobs[name] != nil {
		name = parent + "/batchPredictionJobs/" + regNumericID()
	}
	j := proto.Clone(in).(*aiplatformpb.BatchPredictionJob)
	now := timestamppb.New(st.now())
	j.Name, j.CreateTime, j.UpdateTime, j.StartTime, j.EndTime, j.Error = name, now, now, nil, nil, nil
	j.State, j.OutputInfo, j.CompletionStats, j.PartialFailures = aiplatformpb.JobState_JOB_STATE_QUEUED, nil, nil, nil
	if model != nil {
		j.ModelVersionId = model.GetVersionId()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &batchJobEntry{Job: j, cancel: cancel, done: make(chan struct{})}
	st.batchJobs[name] = e
	st.save()
	go s.runBatchJob(ctx, e, st.pipelineRunner, r, model)
	return proto.Clone(j).(*aiplatformpb.BatchPredictionJob), nil
}

func (st *notebookStore) setBatchJob(e *batchJobEntry, f func(j *aiplatformpb.BatchPredictionJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.batchJobs[e.Job.GetName()] != e {
		return
	}
	f(e.Job)
	e.Job.UpdateTime = timestamppb.New(st.now())
	st.save()
}

// batchStamp is a job's create time as its output directory names it.
func batchStamp(t time.Time, sep string) string {
	s := t.UTC().Format("2006-01-02T15:04:05.000Z")
	if sep != "-" {
		s = strings.NewReplacer("-", sep, ":", sep, ".", sep).Replace(s)
	}
	return s
}

var bqNameSafe = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func (s *Server) runBatchJob(ctx context.Context, e *batchJobEntry, runner PipelineRunner, r *registry, model *aiplatformpb.Model) {
	st := s.notebooks()
	defer close(e.done)
	st.setBatchJob(e, func(j *aiplatformpb.BatchPredictionJob) { j.State = aiplatformpb.JobState_JOB_STATE_PENDING })
	st.mu.Lock()
	job := proto.Clone(e.Job).(*aiplatformpb.BatchPredictionJob)
	st.mu.Unlock()
	stats, info, err := s.batchPredict(ctx, job, runner, r, model, func() {
		st.setBatchJob(e, func(j *aiplatformpb.BatchPredictionJob) {
			j.State, j.StartTime = aiplatformpb.JobState_JOB_STATE_RUNNING, timestamppb.New(st.now())
		})
	})
	st.setBatchJob(e, func(j *aiplatformpb.BatchPredictionJob) {
		j.EndTime, j.CompletionStats, j.OutputInfo = timestamppb.New(st.now()), stats, info
		switch {
		case ctx.Err() != nil:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_CANCELLED, &status.Status{Code: int32(codes.Canceled), Message: "CANCELED"}
		case err != nil:
			ae := apierror.From(err)
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_FAILED, &status.Status{Code: int32(ae.Code), Message: ae.Message}
		case stats.GetSuccessfulCount() == 0 && stats.GetFailedCount() > 0:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_FAILED, &status.Status{Code: int32(codes.InvalidArgument),
				Message: fmt.Sprintf("Batch prediction failed: all %d instances failed; see %s", stats.GetFailedCount(), batchErrorsAt(info))}
		default:
			j.State = aiplatformpb.JobState_JOB_STATE_SUCCEEDED
			if stats.GetFailedCount() > 0 {
				j.PartialFailures = []*status.Status{{Code: int32(codes.InvalidArgument),
					Message: fmt.Sprintf("%d instances failed; see %s", stats.GetFailedCount(), batchErrorsAt(info))}}
			}
		}
	})
}

func batchErrorsAt(info *aiplatformpb.BatchPredictionJob_OutputInfo) string {
	if d := info.GetGcsOutputDirectory(); d != "" {
		return d + "/prediction.errors_stats-00000-of-00001"
	}
	return info.GetBigqueryOutputDataset() + ".errors"
}

// batchInstance is one input instance and its source text.
type batchInstance struct {
	value any
}

// batchPredict runs the job: the instances, the model's container, the
// predictions written out.
func (s *Server) batchPredict(ctx context.Context, job *aiplatformpb.BatchPredictionJob, runner PipelineRunner, r *registry,
	model *aiplatformpb.Model, running func()) (*aiplatformpb.CompletionStats, *aiplatformpb.BatchPredictionJob_OutputInfo, error) {
	project, location, _ := parseParent("parent", path.Dir(path.Dir(job.GetName())))
	instances, err := s.batchInstances(ctx, job, runner, project)
	if err != nil {
		return nil, nil, err
	}
	run, err := batchContainer(job, r, model, project, location)
	if err != nil {
		return nil, nil, err
	}
	url, err := r.runner.Run(ctx, project, run)
	defer func() { _ = r.runner.Remove(context.Background(), project, run.ID) }()
	if err != nil {
		return nil, nil, apierror.FailedPrecondition("the model's container did not start: %v", err)
	}
	running()
	size := int(job.GetManualBatchTuningParameters().GetBatchSize())
	if size <= 0 {
		size = 64
	}
	var params any
	if mp := job.GetModelParameters(); mp != nil {
		params = mp.AsInterface()
	}
	var preds []any
	var errs []map[string]any
	ok := make([]bool, len(instances))
	predictions := make([]any, len(instances))
	for i := 0; i < len(instances); i += size {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		end := min(i+size, len(instances))
		var batch []any
		for _, in := range instances[i:end] {
			batch = append(batch, in.value)
		}
		body := map[string]any{"instances": batch}
		if params != nil {
			body["parameters"] = params
		}
		b, _ := json.Marshal(body)
		a, err := s.forward(ctx, path.Base(job.GetName()), url+run.PredictRoute, b, "application/json")
		var resp struct {
			Predictions []any `json:"predictions"`
		}
		switch {
		case err != nil:
		case a.status/100 != 2:
			err = containerError(path.Base(job.GetName()), a)
		case json.Unmarshal(a.body, &resp) != nil:
			err = fmt.Errorf("the model's container answered no JSON predictions")
		case len(resp.Predictions) != len(batch):
			err = fmt.Errorf("the model's container answered %d predictions for %d instances", len(resp.Predictions), len(batch))
		}
		for k := i; k < end; k++ {
			if err != nil {
				ae := apierror.From(err)
				errs = append(errs, map[string]any{"instance": instances[k].value, "error": map[string]any{"code": int(ae.Code), "message": ae.Message}})
				continue
			}
			ok[k], predictions[k] = true, resp.Predictions[k-i]
		}
	}
	for k := range instances {
		if ok[k] {
			preds = append(preds, map[string]any{"instance": instances[k].value, "prediction": predictions[k]})
		}
	}
	stats := &aiplatformpb.CompletionStats{SuccessfulCount: int64(len(preds)), FailedCount: int64(len(errs))}
	info, err := s.writePredictions(ctx, job, runner, model, project, preds, errs)
	return stats, info, err
}

// batchContainer is the job's model container, with the custom container
// contract's environment.
func batchContainer(job *aiplatformpb.BatchPredictionJob, r *registry, model *aiplatformpb.Model, project, location string) (ContainerRun, error) {
	spec, storage := job.GetUnmanagedContainerModel().GetContainerSpec(), job.GetUnmanagedContainerModel().GetArtifactUri()
	if model != nil {
		spec = model.GetContainerSpec()
		p, l, id, _, _ := parseModelName("model", model.GetName())
		r.mu.Lock()
		storage = r.storageURI(p, l, id, model.GetVersionId())
		r.mu.Unlock()
	}
	if err := r.checkPrebuilt(spec.GetImageUri()); err != nil {
		return ContainerRun{}, err
	}
	id := path.Base(job.GetName())
	port := 8080
	if ps := spec.GetPorts(); len(ps) > 0 {
		port = int(ps[0].GetContainerPort())
	}
	health, predict := spec.GetHealthRoute(), spec.GetPredictRoute()
	if health == "" {
		health = "/v1/endpoints/" + id + "/deployedModels/" + id
	}
	if predict == "" {
		predict = "/v1/endpoints/" + id + "/deployedModels/" + id + ":predict"
	}
	run := ContainerRun{ID: "aip-bp-" + id, Image: spec.GetImageUri(), Command: spec.GetCommand(), Args: spec.GetArgs(), Port: port,
		HealthRoute: health, PredictRoute: predict, MinReplicas: 1, MaxReplicas: 1,
		Labels: map[string]string{"cloudburrow-vertex-batch-prediction-job": id}}
	for _, ev := range spec.GetEnv() {
		run.Env = append(run.Env, EnvPair{ev.GetName(), ev.GetValue()})
	}
	machine := job.GetDedicatedResources().GetMachineSpec().GetMachineType()
	if machine == "" {
		machine = "n1-standard-2"
	}
	run.Env = append(run.Env, EnvPair{"AIP_STORAGE_URI", storage}, EnvPair{"AIP_HTTP_PORT", strconv.Itoa(port)},
		EnvPair{"AIP_HEALTH_ROUTE", health}, EnvPair{"AIP_PREDICT_ROUTE", predict}, EnvPair{"AIP_MODE", "PREDICTION"},
		EnvPair{"AIP_MODE_VERSION", "1.0.0"}, EnvPair{"AIP_MODEL_NAME", id}, EnvPair{"AIP_DEPLOYED_MODEL_ID", id},
		EnvPair{"AIP_PROJECT_NUMBER", project}, EnvPair{"AIP_MACHINE_TYPE", machine}, EnvPair{"AIP_FRAMEWORK", "CUSTOM_CONTAINER"})
	_ = location
	return run, nil
}

// batchInstances reads the job's input.
func (s *Server) batchInstances(ctx context.Context, job *aiplatformpb.BatchPredictionJob, runner PipelineRunner, project string) ([]batchInstance, error) {
	ic := job.GetInputConfig()
	var out []batchInstance
	if ic.GetInstancesFormat() == "bigquery" {
		m := bqURIRE.FindStringSubmatch(ic.GetBigquerySource().GetInputUri())
		rows, err := bigQueryRows(ctx, runner, m[1], m[2], m[3])
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, batchInstance{value: r})
		}
		return out, nil
	}
	for _, u := range ic.GetGcsSource().GetUris() {
		names, bucket, err := expandGCSURI(ctx, runner, u)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			b, err := runner.ReadObject(ctx, bucket, n, 0)
			if err != nil {
				return nil, apierror.InvalidArgument("batch_prediction_job.input_config.gcs_source.uris: gs://%s/%s cannot be read: %v", bucket, n, err)
			}
			if ic.GetInstancesFormat() == "csv" {
				recs, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
				if err != nil {
					return nil, apierror.InvalidArgument("gs://%s/%s is not CSV: %v", bucket, n, err)
				}
				for i, rec := range recs {
					if i == 0 {
						continue // the header
					}
					row := make([]any, len(rec))
					for k, f := range rec {
						if v, err := strconv.ParseFloat(f, 64); err == nil {
							row[k] = v
						} else {
							row[k] = f
						}
					}
					out = append(out, batchInstance{value: row})
				}
				continue
			}
			for i, line := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var v any
				if err := json.Unmarshal([]byte(line), &v); err != nil {
					return nil, apierror.InvalidArgument("gs://%s/%s line %d is not JSON: %v", bucket, n, i+1, err)
				}
				out = append(out, batchInstance{value: v})
			}
		}
	}
	if len(out) == 0 {
		return nil, apierror.InvalidArgument("batch_prediction_job.input_config: the input has no instances")
	}
	return out, nil
}

// expandGCSURI is the objects a gs:// URI names: one object, or those its
// wildcard (*) matches.
func expandGCSURI(ctx context.Context, runner PipelineRunner, uri string) ([]string, string, error) {
	bucket, object, err := splitNotebookGCS("batch_prediction_job.input_config.gcs_source.uris", uri, true)
	if err != nil {
		return nil, "", err
	}
	if !strings.Contains(object, "*") {
		return []string{object}, bucket, nil
	}
	prefix := object[:strings.Index(object, "*")]
	all, err := runner.ListObjects(ctx, bucket, prefix)
	if err != nil {
		return nil, "", err
	}
	re := regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(object), `\*`, ".*") + "$")
	var out []string
	for _, n := range all {
		if re.MatchString(n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, "", apierror.InvalidArgument("batch_prediction_job.input_config.gcs_source.uris %q matches no object", uri)
	}
	return out, bucket, nil
}

// writePredictions writes the predictions and errors where the job's
// output_config says.
func (s *Server) writePredictions(ctx context.Context, job *aiplatformpb.BatchPredictionJob, runner PipelineRunner, model *aiplatformpb.Model,
	project string, preds []any, errs []map[string]any) (*aiplatformpb.BatchPredictionJob_OutputInfo, error) {
	display := job.GetDisplayName()
	if model != nil {
		display = model.GetDisplayName()
	}
	created := job.GetCreateTime().AsTime()
	oc := job.GetOutputConfig()
	if oc.GetPredictionsFormat() == "bigquery" {
		m := bqURIRE.FindStringSubmatch(oc.GetBigqueryDestination().GetOutputUri())
		bqProject, dataset := m[1], m[2]
		if dataset == "" {
			dataset = "prediction_" + strings.Trim(bqNameSafe.ReplaceAllString(display, "_"), "_") + "_" + batchStamp(created, "_")
		}
		if err := writeBigQueryPredictions(ctx, runner, bqProject, dataset, job.GetName(), preds, errs); err != nil {
			return nil, err
		}
		return &aiplatformpb.BatchPredictionJob_OutputInfo{OutputLocation: &aiplatformpb.BatchPredictionJob_OutputInfo_BigqueryOutputDataset{
			BigqueryOutputDataset: "bq://" + bqProject + "." + dataset}, BigqueryOutputTable: "predictions"}, nil
	}
	dir := strings.TrimSuffix(oc.GetGcsDestination().GetOutputUriPrefix(), "/") + "/prediction-" + display + "-" + batchStamp(created, "-")
	bucket, object, err := splitNotebookGCS("output", dir, true)
	if err != nil {
		return nil, err
	}
	write := func(file string, rows []any) error {
		var b strings.Builder
		if oc.GetPredictionsFormat() == "csv" && strings.HasPrefix(file, "prediction.results") {
			w := csv.NewWriter(&b)
			_ = w.Write([]string{"instance", "prediction"})
			for _, r := range rows {
				m := r.(map[string]any)
				in, _ := json.Marshal(m["instance"])
				p, _ := json.Marshal(m["prediction"])
				_ = w.Write([]string{string(in), string(p)})
			}
			w.Flush()
		} else {
			for _, r := range rows {
				l, _ := json.Marshal(r)
				b.Write(l)
				b.WriteByte('\n')
			}
		}
		ctype := "application/jsonl"
		if oc.GetPredictionsFormat() == "csv" && strings.HasPrefix(file, "prediction.results") {
			ctype = "text/csv"
		}
		return runner.WriteObject(ctx, bucket, object+"/"+file, ctype, []byte(b.String()))
	}
	if err := write("prediction.results-00000-of-00001", preds); err != nil {
		return nil, err
	}
	var er []any
	for _, e := range errs {
		er = append(er, e)
	}
	if err := write("prediction.errors_stats-00000-of-00001", er); err != nil {
		return nil, err
	}
	return &aiplatformpb.BatchPredictionJob_OutputInfo{OutputLocation: &aiplatformpb.BatchPredictionJob_OutputInfo_GcsOutputDirectory{GcsOutputDirectory: dir}}, nil
}

func (st *notebookStore) batchJob(name string) (*batchJobEntry, error) {
	if _, _, _, err := parseChild("name", "batchPredictionJobs", name); err != nil {
		return nil, err
	}
	e, ok := st.batchJobs[name]
	if !ok {
		return nil, apierror.NotFound("BatchPredictionJob %s not found", name)
	}
	return e, nil
}

func (j *jobService) GetBatchPredictionJob(_ context.Context, req *aiplatformpb.GetBatchPredictionJobRequest) (*aiplatformpb.BatchPredictionJob, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.batchJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(e.Job).(*aiplatformpb.BatchPredictionJob), nil
}

func (j *jobService) ListBatchPredictionJobs(_ context.Context, req *aiplatformpb.ListBatchPredictionJobsRequest) (*aiplatformpb.ListBatchPredictionJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := j.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.BatchPredictionJob
	for _, k := range sortedNames(st.batchJobs) {
		if strings.HasPrefix(k, req.GetParent()+"/batchPredictionJobs/") {
			all = append(all, proto.Clone(st.batchJobs[k].Job).(*aiplatformpb.BatchPredictionJob))
		}
	}
	st.mu.Unlock()
	items, next, err := listJobs("batchPredictionJobs", req.GetParent(), req.GetFilter(), "", req.GetPageToken(), req.GetPageSize(), all)
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListBatchPredictionJobsResponse{BatchPredictionJobs: items, NextPageToken: next}, nil
}

func (j *jobService) DeleteBatchPredictionJob(_ context.Context, req *aiplatformpb.DeleteBatchPredictionJobRequest) (*longrunningpb.Operation, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	e, err := st.batchJob(req.GetName())
	if err == nil && jobRunning(e.Job.GetState()) {
		err = apierror.FailedPrecondition("BatchPredictionJob %s is %s: cancel it before deleting it", req.GetName(), e.Job.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.batchJobs, req.GetName())
	st.save()
	st.mu.Unlock()
	return j.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (j *jobService) CancelBatchPredictionJob(_ context.Context, req *aiplatformpb.CancelBatchPredictionJobRequest) (*emptypb.Empty, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.batchJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if !jobRunning(e.Job.GetState()) {
		return nil, grpcError(apierror.FailedPrecondition("BatchPredictionJob %s is %s and cannot be cancelled", req.GetName(), e.Job.GetState()))
	}
	e.Job.State, e.Job.UpdateTime = aiplatformpb.JobState_JOB_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return &emptypb.Empty{}, nil
}

// waitBatchJob waits for a batch prediction job to end and returns it.
func (s *Server) waitBatchJob(ctx context.Context, name string) (*aiplatformpb.BatchPredictionJob, error) {
	st := s.notebooks()
	st.mu.Lock()
	e, err := st.batchJob(name)
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		st.mu.Lock()
		if jobRunning(e.Job.GetState()) && e.cancel != nil {
			e.cancel()
		}
		st.mu.Unlock()
		<-e.done
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return proto.Clone(e.Job).(*aiplatformpb.BatchPredictionJob), nil
}

// ---- BigQuery input and output, through the instance's BigQuery REST API

// bigQueryRows reads a table's rows as objects (tabledata.list, with the
// table's schema).
func bigQueryRows(ctx context.Context, runner PipelineRunner, project, dataset, table string) ([]map[string]any, error) {
	bq, ok := runner.(PipelineBigQuery)
	if !ok {
		return nil, apierror.FailedPrecondition("BigQuery is not served on this instance (start with --services including bigquery)")
	}
	base := "/bigquery/v2/projects/" + url.PathEscape(project) + "/datasets/" + url.PathEscape(dataset) + "/tables/" + url.PathEscape(table)
	code, b, err := bq.BigQueryREST(ctx, "GET", base, nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, apierror.InvalidArgument("bq://%s.%s.%s cannot be read: %s", project, dataset, table, strings.TrimSpace(string(b)))
	}
	var meta struct {
		Schema bqSchema `json:"schema"`
	}
	_ = json.Unmarshal(b, &meta)
	var out []map[string]any
	token := ""
	for {
		p := base + "/data?maxResults=10000"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		code, b, err := bq.BigQueryREST(ctx, "GET", p, nil)
		if err != nil {
			return nil, err
		}
		if code/100 != 2 {
			return nil, apierror.InvalidArgument("bq://%s.%s.%s cannot be read: %s", project, dataset, table, strings.TrimSpace(string(b)))
		}
		var page struct {
			Rows      []bqRow `json:"rows"`
			PageToken string  `json:"pageToken"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Rows {
			out = append(out, meta.Schema.object(r))
		}
		if page.PageToken == "" {
			return out, nil
		}
		token = page.PageToken
	}
}

type bqField struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Mode   string    `json:"mode,omitempty"`
	Fields []bqField `json:"fields,omitempty"`
}

type bqSchema struct {
	Fields []bqField `json:"fields"`
}

type bqRow struct {
	F []struct {
		V any `json:"v"`
	} `json:"f"`
}

// object is a tabledata row as an object, its values typed by the schema.
func (s bqSchema) object(r bqRow) map[string]any {
	out := map[string]any{}
	for i, f := range s.Fields {
		if i < len(r.F) {
			out[f.Name] = bqValue(f, r.F[i].V)
		}
	}
	return out
}

func bqValue(f bqField, v any) any {
	if v == nil {
		return nil
	}
	if f.Mode == "REPEATED" {
		list, _ := v.([]any)
		out := []any{}
		g := f
		g.Mode = ""
		for _, el := range list {
			if m, ok := el.(map[string]any); ok {
				out = append(out, bqValue(g, m["v"]))
			}
		}
		return out
	}
	switch f.Type {
	case "RECORD", "STRUCT":
		m, _ := v.(map[string]any)
		raw, _ := json.Marshal(m)
		var row bqRow
		_ = json.Unmarshal(raw, &row)
		return bqSchema{Fields: f.Fields}.object(row)
	case "INTEGER", "INT64", "FLOAT", "FLOAT64", "NUMERIC", "BIGNUMERIC":
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				return n
			}
		}
	case "BOOLEAN", "BOOL":
		if s, ok := v.(string); ok {
			return strings.EqualFold(s, "true")
		}
	}
	return v
}

// writeBigQueryPredictions creates the output dataset (when missing) and
// its predictions and errors tables, the instance and prediction (or
// error) of each row as JSON.
func writeBigQueryPredictions(ctx context.Context, runner PipelineRunner, project, dataset, jobName string, preds []any, errs []map[string]any) error {
	bq, ok := runner.(PipelineBigQuery)
	if !ok {
		return apierror.FailedPrecondition("BigQuery is not served on this instance (start with --services including bigquery)")
	}
	call := func(method, p string, body any) error {
		b, _ := json.Marshal(body)
		code, out, err := bq.BigQueryREST(ctx, method, p, b)
		if err != nil {
			return err
		}
		if code/100 != 2 && code != 409 {
			return apierror.FailedPrecondition("writing the predictions to BigQuery: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	pp := "/bigquery/v2/projects/" + url.PathEscape(project)
	if err := call("POST", pp+"/datasets", map[string]any{"datasetReference": map[string]any{"projectId": project, "datasetId": dataset}}); err != nil {
		return err
	}
	tables := map[string][]map[string]any{"predictions": nil, "errors": nil}
	for _, p := range preds {
		m := p.(map[string]any)
		in, _ := json.Marshal(m["instance"])
		out, _ := json.Marshal(m["prediction"])
		tables["predictions"] = append(tables["predictions"], map[string]any{"json": map[string]any{"instance": string(in), "prediction": string(out)}})
	}
	for _, e := range errs {
		in, _ := json.Marshal(e["instance"])
		out, _ := json.Marshal(e["error"])
		tables["errors"] = append(tables["errors"], map[string]any{"json": map[string]any{"instance": string(in), "errors": string(out)}})
	}
	for _, t := range []string{"predictions", "errors"} {
		col := "prediction"
		if t == "errors" {
			col = "errors"
		}
		schema := map[string]any{"fields": []map[string]any{{"name": "instance", "type": "STRING"}, {"name": col, "type": "STRING"}}}
		if err := call("POST", pp+"/datasets/"+url.PathEscape(dataset)+"/tables", map[string]any{
			"tableReference": map[string]any{"projectId": project, "datasetId": dataset, "tableId": t}, "schema": schema,
			"labels": map[string]any{"aiplatform_batch_prediction_job": path.Base(jobName)}}); err != nil {
			return err
		}
		if rows := tables[t]; len(rows) > 0 {
			if err := call("POST", pp+"/datasets/"+url.PathEscape(dataset)+"/tables/"+t+"/insertAll", map[string]any{"rows": rows}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensure protojson stays used for the saved form (customjobs.go).
var _ = protojson.Marshal
