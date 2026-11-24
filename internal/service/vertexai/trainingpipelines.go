package vertexai

// Vertex AI training pipelines (#2210): PipelineService's TrainingPipeline
// methods. A custom training pipeline (training_task_definition
// gs://google-cloud-aiplatform/schema/trainingjob/definition/custom_task_1.0.0.yaml,
// what the Vertex AI SDK's CustomTrainingJob, CustomContainerTrainingJob
// and CustomPythonPackageTrainingJob create) runs its training_task_inputs
// as a CustomJob (customjobs.go) and, when it succeeds, uploads
// model_to_upload to the Model Registry from the job's model directory.
//
// Documented behaviour (the TrainingPipeline proto, aiplatform v1.127.0;
// https://docs.cloud.google.com/vertex-ai/docs/training/create-training-pipeline,
// .../reference/rest/v1/projects.locations.trainingPipelines; read
// 2026-11-24):
//   - display_name, training_task_definition and training_task_inputs are
//     required; training_task_inputs is the CustomJobSpec;
//   - the pipeline is PENDING, RUNNING, then SUCCEEDED, FAILED or
//     CANCELLED; training_task_metadata names its backingCustomJob;
//   - model_to_upload's artifact_uri, when unset, is the job's
//     base_output_directory/model (AIP_MODEL_DIR); model_id and
//     parent_model name the uploaded model (or its new version);
//     model_to_upload is the uploaded Model once the pipeline succeeds;
//   - CancelTrainingPipeline is best effort (error code 1); a running
//     pipeline cannot be deleted.
//
// Not served (UNIMPLEMENTED, naming why): AutoML and hyperparameter tuning
// task definitions, and input_data_config (Vertex AI managed datasets,
// which this instance does not serve).

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// customTaskDefinition is the custom training task's schema.
const customTaskDefinition = "gs://google-cloud-aiplatform/schema/trainingjob/definition/custom_task_1.0.0.yaml"

type trainingEntry struct {
	TP     *aiplatformpb.TrainingPipeline
	cancel context.CancelFunc
}

func trainingRunning(s aiplatformpb.PipelineState) bool {
	switch s {
	case aiplatformpb.PipelineState_PIPELINE_STATE_QUEUED, aiplatformpb.PipelineState_PIPELINE_STATE_PENDING,
		aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLING:
		return true
	}
	return false
}

// trainingSpec reads training_task_inputs as a CustomJobSpec.
func trainingSpec(v *structpb.Value) (*aiplatformpb.CustomJobSpec, error) {
	if v.GetStructValue() == nil {
		return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs is required: the custom task's CustomJobSpec")
	}
	b, err := protojson.Marshal(v)
	if err != nil {
		return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs: %v", err)
	}
	spec := &aiplatformpb.CustomJobSpec{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, spec); err != nil {
		return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs is not a custom task's inputs (a CustomJobSpec): %v", err)
	}
	return spec, nil
}

func (p *pipelineService) CreateTrainingPipeline(_ context.Context, req *aiplatformpb.CreateTrainingPipelineRequest) (*aiplatformpb.TrainingPipeline, error) {
	out, err := p.s.createTrainingPipeline(req.GetParent(), req.GetTrainingPipeline())
	if err != nil {
		return nil, grpcError(err)
	}
	return out, nil
}

func (s *Server) createTrainingPipeline(parent string, in *aiplatformpb.TrainingPipeline) (*aiplatformpb.TrainingPipeline, error) {
	project, location, err := parseParent("parent", parent)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("training_pipeline is required")
	}
	if in.GetDisplayName() == "" {
		return nil, apierror.InvalidArgument("training_pipeline.display_name is required")
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		return nil, apierror.InvalidArgument("training_pipeline.display_name can be up to 128 characters long")
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	def := in.GetTrainingTaskDefinition()
	switch {
	case def == "":
		return nil, apierror.InvalidArgument("training_pipeline.training_task_definition is required")
	case def != customTaskDefinition:
		return nil, apierror.Unimplemented("training_pipeline.training_task_definition %s is not served on this instance: only custom training (%s) is; "+
			"AutoML and hyperparameter tuning pipelines are not run locally", def, customTaskDefinition)
	}
	if in.GetInputDataConfig() != nil {
		return nil, apierror.Unimplemented("training_pipeline.input_data_config names a Vertex AI managed dataset, which this instance does not serve: " +
			"pass the training data to the job's container (its args or environment) as Cloud Storage or BigQuery URIs")
	}
	spec, err := trainingSpec(in.GetTrainingTaskInputs())
	if err != nil {
		return nil, err
	}
	if err := checkCustomJobSpec("training_pipeline.training_task_inputs", spec); err != nil {
		return nil, err
	}
	if m := in.GetModelToUpload(); m != nil {
		if m.GetDisplayName() == "" {
			return nil, apierror.InvalidArgument("training_pipeline.model_to_upload.display_name is required")
		}
		if m.GetContainerSpec().GetImageUri() == "" {
			return nil, apierror.InvalidArgument("training_pipeline.model_to_upload.container_spec.image_uri is required")
		}
		if m.GetArtifactUri() == "" && spec.GetBaseOutputDirectory().GetOutputUriPrefix() == "" {
			return nil, apierror.InvalidArgument("training_pipeline.model_to_upload has no artifact_uri and training_task_inputs no base_output_directory to take the model from")
		}
		if s.registry() == nil {
			return nil, apierror.FailedPrecondition("training_pipeline.model_to_upload is uploaded to the Model Registry, which is not served on this instance (start with --services including run)")
		}
	}
	if pm := in.GetParentModel(); pm != "" {
		if _, _, _, _, err := parseModelName("training_pipeline.parent_model", pm); err != nil {
			return nil, err
		}
	}
	st := s.notebooks()
	st.mu.Lock()
	if st.pipelineRunner == nil {
		st.mu.Unlock()
		return nil, apierror.FailedPrecondition("Vertex AI custom training needs Docker and this instance's Cloud Storage (start with --services including storage)")
	}
	if st.trainingPipelines == nil {
		st.trainingPipelines = map[string]*trainingEntry{}
	}
	name := parent + "/trainingPipelines/" + regNumericID()
	for st.trainingPipelines[name] != nil {
		name = parent + "/trainingPipelines/" + regNumericID()
	}
	tp := proto.Clone(in).(*aiplatformpb.TrainingPipeline)
	now := timestamppb.New(st.now())
	tp.Name, tp.CreateTime, tp.UpdateTime, tp.StartTime, tp.EndTime, tp.Error = name, now, now, nil, nil, nil
	tp.State, tp.TrainingTaskMetadata = aiplatformpb.PipelineState_PIPELINE_STATE_PENDING, nil
	ctx, cancel := context.WithCancel(context.Background())
	e := &trainingEntry{TP: tp, cancel: cancel}
	st.trainingPipelines[name] = e
	st.save()
	out := proto.Clone(tp).(*aiplatformpb.TrainingPipeline)
	st.mu.Unlock()
	go s.runTrainingPipeline(ctx, e, parent, project, location, spec)
	return out, nil
}

func (st *notebookStore) setTraining(e *trainingEntry, f func(tp *aiplatformpb.TrainingPipeline)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.trainingPipelines[e.TP.GetName()] != e {
		return
	}
	f(e.TP)
	e.TP.UpdateTime = timestamppb.New(st.now())
	st.save()
}

func (s *Server) runTrainingPipeline(ctx context.Context, e *trainingEntry, parent, project, location string, spec *aiplatformpb.CustomJobSpec) {
	st := s.notebooks()
	st.mu.Lock()
	tp := proto.Clone(e.TP).(*aiplatformpb.TrainingPipeline)
	st.mu.Unlock()
	fail := func(code codes.Code, msg string) {
		st.setTraining(e, func(t *aiplatformpb.TrainingPipeline) {
			t.State, t.Error, t.EndTime = aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, &status.Status{Code: int32(code), Message: msg}, timestamppb.New(st.now())
			if code == codes.Canceled {
				t.State = aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED
			}
		})
	}
	job, err := s.createCustomJob(parent, &aiplatformpb.CustomJob{DisplayName: tp.GetDisplayName() + "-custom-job", JobSpec: spec,
		Labels: tp.GetLabels(), EncryptionSpec: tp.GetEncryptionSpec()}, nil)
	if err != nil {
		ae := apierror.From(err)
		fail(ae.Code, ae.Message)
		return
	}
	meta, _ := structpb.NewValue(map[string]any{"backingCustomJob": job.GetName()})
	st.setTraining(e, func(t *aiplatformpb.TrainingPipeline) {
		t.State, t.StartTime, t.TrainingTaskMetadata = aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, timestamppb.New(st.now()), meta
	})
	done, err := s.waitCustomJob(ctx, job.GetName())
	switch {
	case ctx.Err() != nil:
		fail(codes.Canceled, "CANCELED")
		return
	case err != nil:
		ae := apierror.From(err)
		fail(ae.Code, ae.Message)
		return
	case done.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED:
		fail(codes.Code(done.GetError().GetCode()), fmt.Sprintf("Job failed. The backing CustomJob %s %s: %s", job.GetName(),
			strings.ToLower(strings.TrimPrefix(done.GetState().String(), "JOB_STATE_")), done.GetError().GetMessage()))
		return
	}
	var uploaded *aiplatformpb.Model
	if m := tp.GetModelToUpload(); m != nil {
		r := s.registry()
		model := proto.Clone(m).(*aiplatformpb.Model)
		if model.ArtifactUri == "" {
			model.ArtifactUri = strings.TrimSuffix(spec.GetBaseOutputDirectory().GetOutputUriPrefix(), "/") + "/model"
		}
		resp, err := r.upload(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, Model: model, ModelId: tp.GetModelId(), ParentModel: tp.GetParentModel()})
		if err != nil {
			ae := apierror.From(err)
			fail(ae.Code, "Model upload failed: "+ae.Message)
			return
		}
		if uploaded, err = r.get(resp.GetModel() + "@" + resp.GetModelVersionId()); err != nil {
			uploaded = &aiplatformpb.Model{Name: resp.GetModel(), VersionId: resp.GetModelVersionId()}
		}
	}
	st.setTraining(e, func(t *aiplatformpb.TrainingPipeline) {
		t.State, t.EndTime = aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED, timestamppb.New(st.now())
		if uploaded != nil {
			t.ModelToUpload = uploaded
			_, _, t.ModelId, _, _ = parseModelName("model", uploaded.GetName())
		}
	})
	_, _ = project, location
}

func (st *notebookStore) training(name string) (*trainingEntry, error) {
	if _, _, _, err := parseChild("name", "trainingPipelines", name); err != nil {
		return nil, err
	}
	e, ok := st.trainingPipelines[name]
	if !ok {
		return nil, apierror.NotFound("TrainingPipeline %s not found", name)
	}
	return e, nil
}

func (p *pipelineService) GetTrainingPipeline(_ context.Context, req *aiplatformpb.GetTrainingPipelineRequest) (*aiplatformpb.TrainingPipeline, error) {
	st := p.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.training(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(e.TP).(*aiplatformpb.TrainingPipeline), nil
}

func (p *pipelineService) ListTrainingPipelines(_ context.Context, req *aiplatformpb.ListTrainingPipelinesRequest) (*aiplatformpb.ListTrainingPipelinesResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := p.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.TrainingPipeline
	for _, k := range sortedNames(st.trainingPipelines) {
		if strings.HasPrefix(k, req.GetParent()+"/trainingPipelines/") {
			all = append(all, proto.Clone(st.trainingPipelines[k].TP).(*aiplatformpb.TrainingPipeline))
		}
	}
	st.mu.Unlock()
	items, next, err := listJobs("trainingPipelines", req.GetParent(), req.GetFilter(), "", req.GetPageToken(), req.GetPageSize(), all)
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListTrainingPipelinesResponse{TrainingPipelines: items, NextPageToken: next}, nil
}

func (p *pipelineService) DeleteTrainingPipeline(_ context.Context, req *aiplatformpb.DeleteTrainingPipelineRequest) (*longrunningpb.Operation, error) {
	st := p.s.notebooks()
	st.mu.Lock()
	e, err := st.training(req.GetName())
	if err == nil && trainingRunning(e.TP.GetState()) {
		err = apierror.FailedPrecondition("TrainingPipeline %s is %s: cancel it before deleting it", req.GetName(), e.TP.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.trainingPipelines, req.GetName())
	st.save()
	st.mu.Unlock()
	return p.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (p *pipelineService) CancelTrainingPipeline(_ context.Context, req *aiplatformpb.CancelTrainingPipelineRequest) (*emptypb.Empty, error) {
	st := p.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.training(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if !trainingRunning(e.TP.GetState()) {
		return nil, grpcError(apierror.FailedPrecondition("TrainingPipeline %s is %s and cannot be cancelled", req.GetName(), e.TP.GetState()))
	}
	e.TP.State, e.TP.UpdateTime = aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return &emptypb.Empty{}, nil
}
