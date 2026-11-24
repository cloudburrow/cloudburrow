package main

// Vertex AI Training's console pages (#2210): Model development >
// Training, with its Training pipelines and Custom jobs tabs, and Models >
// Batch inference, in Google's layout:
//
//   - "go to the Training pipelines page" and "Click Create to open the
//     Train new model pane": Training method (Dataset: No managed dataset;
//     Custom training (advanced)), Model details (Train new model or Train
//     new version), Training container (Prebuilt container: Model
//     framework, Model framework version, Package location, Python module;
//     Custom container: Container image; Model output directory,
//     Arguments), Hyperparameter tuning, Compute and pricing (Region, Worker
//     pool 0), Prediction container (No prediction container, Prebuilt
//     container or Custom container: Container image, Command, Model
//     directory), then Start training
//     (https://docs.cloud.google.com/vertex-ai/docs/training/create-training-pipeline,
//     https://docs.cloud.google.com/vertex-ai/docs/training/create-custom-job,
//     read 2026-11-24). The console cannot create a CustomJob directly, so
//     the Custom jobs tab has no Create, as Google's has none;
//   - "go to the Batch predictions page", "Click Create to open the New
//     batch prediction window": a name, Model name, Select source (File on
//     Cloud Storage (JSON Lines, CSV, TFRecord, TFRecord Gzip) with Source
//     path; Files on Cloud Storage (other); BigQuery path, which needs
//     BigQuery as the output), Destination path, Enable feature attributions
//     for this model, Number of compute nodes, Machine type, Accelerator
//     type and count, then Create
//     (https://docs.cloud.google.com/vertex-ai/docs/predictions/get-batch-predictions,
//     read 2026-11-24).
//
// UNVERIFIED (re-read 2026-11-24, the pages above): the three lists'
// columns and the details pages' fields, which Google's docs do not show;
// the Hyperparameter tuning step's Enable hyperparameter tuning (left
// unchecked in Google's walkthrough) is not offered; batch prediction's
// model monitoring (Preview) is not offered.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (v vertexAPI) jobs(ctx context.Context) (*aiplatform.JobClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewJobClient(ctx, v.opts()...)
}

// customTaskDefinition is the training task definition of a custom
// training pipeline, as the Vertex AI SDK's CustomTrainingJob sends it.
const customTaskDefinition = "gs://google-cloud-aiplatform/schema/trainingjob/definition/custom_task_1.0.0.yaml"

// trainingImages are Google's prebuilt training containers by framework
// and version (https://docs.cloud.google.com/vertex-ai/docs/training/pre-built-containers,
// read 2026-11-24); predictionImages its prebuilt prediction containers
// (https://docs.cloud.google.com/vertex-ai/docs/predictions/pre-built-containers).
var (
	trainingImages = map[string]string{
		"tensorflow-2.17":  "us-docker.pkg.dev/vertex-ai/training/tf-cpu.2-17.py310:latest",
		"tensorflow-2.16":  "us-docker.pkg.dev/vertex-ai/training/tf-cpu.2-16.py310:latest",
		"pytorch-2.4":      "us-docker.pkg.dev/vertex-ai/training/pytorch-xla.2-4.py310:latest",
		"pytorch-2.3":      "us-docker.pkg.dev/vertex-ai/training/pytorch-xla.2-3.py310:latest",
		"scikit-learn-1.6": "us-docker.pkg.dev/vertex-ai/training/sklearn-cpu.1-6:latest",
		"scikit-learn-1.0": "us-docker.pkg.dev/vertex-ai/training/sklearn-cpu.1-0:latest",
		"xgboost-2.1":      "us-docker.pkg.dev/vertex-ai/training/xgboost-cpu.2-1:latest",
		"xgboost-1.6":      "us-docker.pkg.dev/vertex-ai/training/xgboost-cpu.1-6:latest",
	}

	predictionImages = map[string]string{
		"tensorflow-2.15":  "us-docker.pkg.dev/vertex-ai/prediction/tf2-cpu.2-15:latest",
		"tensorflow-2.14":  "us-docker.pkg.dev/vertex-ai/prediction/tf2-cpu.2-14:latest",
		"pytorch-2.4":      "us-docker.pkg.dev/vertex-ai/prediction/pytorch-cpu.2-4:latest",
		"pytorch-2.3":      "us-docker.pkg.dev/vertex-ai/prediction/pytorch-cpu.2-3:latest",
		"scikit-learn-1.6": "us-docker.pkg.dev/vertex-ai/prediction/sklearn-cpu.1-6:latest",
		"scikit-learn-1.5": "us-docker.pkg.dev/vertex-ai/prediction/sklearn-cpu.1-5:latest",
		"xgboost-2.1":      "us-docker.pkg.dev/vertex-ai/prediction/xgboost-cpu.2-1:latest",
		"xgboost-2.0":      "us-docker.pkg.dev/vertex-ai/prediction/xgboost-cpu.2-0:latest",
	}

	trainingFrameworks      = []string{"tensorflow", "pytorch", "scikit-learn", "xgboost"}
	trainingFrameworkLabels = map[string]string{"tensorflow": "TensorFlow", "pytorch": "PyTorch", "scikit-learn": "scikit-learn", "xgboost": "XGBoost"}
	trainingVersions        = map[string][]string{"tensorflow": {"2.17", "2.16"}, "pytorch": {"2.4", "2.3"}, "scikit-learn": {"1.6", "1.0"}, "xgboost": {"2.1", "1.6"}}
	predictionVersions      = map[string][]string{"tensorflow": {"2.15", "2.14"}, "pytorch": {"2.4", "2.3"}, "scikit-learn": {"1.6", "1.5"}, "xgboost": {"2.1", "2.0"}}
)

// jobStateText is a job's state as the lists word it.
func jobStateText(s aiplatformpb.JobState) string {
	switch s {
	case aiplatformpb.JobState_JOB_STATE_QUEUED, aiplatformpb.JobState_JOB_STATE_PENDING:
		return "Pending"
	case aiplatformpb.JobState_JOB_STATE_RUNNING:
		return "Running"
	case aiplatformpb.JobState_JOB_STATE_SUCCEEDED:
		return "Succeeded"
	case aiplatformpb.JobState_JOB_STATE_FAILED:
		return "Failed"
	case aiplatformpb.JobState_JOB_STATE_CANCELLING:
		return "Cancelling"
	case aiplatformpb.JobState_JOB_STATE_CANCELLED:
		return "Cancelled"
	case aiplatformpb.JobState_JOB_STATE_PAUSED:
		return "Paused"
	case aiplatformpb.JobState_JOB_STATE_EXPIRED:
		return "Expired"
	case aiplatformpb.JobState_JOB_STATE_UPDATING:
		return "Updating"
	case aiplatformpb.JobState_JOB_STATE_PARTIALLY_SUCCEEDED:
		return "Partially succeeded"
	}
	return "Unknown"
}

func jobActive(s aiplatformpb.JobState) bool {
	switch s {
	case aiplatformpb.JobState_JOB_STATE_QUEUED, aiplatformpb.JobState_JOB_STATE_PENDING, aiplatformpb.JobState_JOB_STATE_RUNNING, aiplatformpb.JobState_JOB_STATE_PAUSED:
		return true
	}
	return false
}

func pipelineActive(s aiplatformpb.PipelineState) bool {
	switch s {
	case aiplatformpb.PipelineState_PIPELINE_STATE_QUEUED, aiplatformpb.PipelineState_PIPELINE_STATE_PENDING, aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, aiplatformpb.PipelineState_PIPELINE_STATE_PAUSED:
		return true
	}
	return false
}

// elapsed is the time from start (else create) to end, or to now.
func elapsed(create, start, end *timestamppb.Timestamp) string {
	from := start
	if from == nil {
		from = create
	}
	if from == nil {
		return "—"
	}
	to := time.Now()
	if end != nil {
		to = end.AsTime()
	}
	d := to.Sub(from.AsTime()).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

func optVertexTime(t *timestamppb.Timestamp) string {
	if t == nil {
		return "—"
	}
	return vertexTime(t)
}

func jobErrorText(code int32, msg string) string {
	if code == 0 && msg == "" {
		return ""
	}
	return msg
}

// trainingType names a training pipeline's task definition.
func trainingType(def string) string {
	switch {
	case strings.Contains(def, "/custom_task_"):
		return "Custom training"
	case strings.Contains(def, "hyperparameter_tuning_task"):
		return "Hyperparameter tuning"
	case strings.Contains(def, "/automl_"):
		return "AutoML"
	case def == "":
		return "—"
	}
	return vertexLast(def)
}

func cancelAction(what string) console.Action {
	return console.Action{ID: "cancel", Label: "Cancel", Confirm: "The " + what + " is stopped; it is kept, Cancelled."}
}

// workerPoolProps describe a CustomJobSpec's worker pools.
func workerPoolProps(spec *aiplatformpb.CustomJobSpec) []console.PropertyGroup {
	var out []console.PropertyGroup
	for i, wp := range spec.GetWorkerPoolSpecs() {
		if wp == nil || (wp.GetContainerSpec() == nil && wp.GetPythonPackageSpec() == nil) {
			continue
		}
		props := []console.Property{
			{Label: "Machine type", Value: dashIfEmpty(wp.GetMachineSpec().GetMachineType())},
			{Label: "Replica count", Value: strconv.FormatInt(wp.GetReplicaCount(), 10)},
		}
		if a := wp.GetMachineSpec().GetAcceleratorType(); a != aiplatformpb.AcceleratorType_ACCELERATOR_TYPE_UNSPECIFIED {
			props = append(props, console.Property{Label: "Accelerator", Value: fmt.Sprintf("%d x %s", wp.GetMachineSpec().GetAcceleratorCount(), a)})
		}
		if c := wp.GetContainerSpec(); c != nil {
			props = append(props, console.Property{Label: "Container image", Value: c.GetImageUri(), Copy: c.GetImageUri()},
				console.Property{Label: "Command", Value: dashIfEmpty(strings.Join(c.GetCommand(), " "))},
				console.Property{Label: "Arguments", Value: dashIfEmpty(strings.Join(c.GetArgs(), " "))})
		}
		if p := wp.GetPythonPackageSpec(); p != nil {
			props = append(props, console.Property{Label: "Executor image", Value: p.GetExecutorImageUri(), Copy: p.GetExecutorImageUri()},
				console.Property{Label: "Package location", Value: dashIfEmpty(strings.Join(p.GetPackageUris(), ", "))},
				console.Property{Label: "Python module", Value: dashIfEmpty(p.GetPythonModule())},
				console.Property{Label: "Arguments", Value: dashIfEmpty(strings.Join(p.GetArgs(), " "))})
		}
		if d := wp.GetDiskSpec(); d != nil && d.GetBootDiskSizeGb() > 0 {
			props = append(props, console.Property{Label: "Boot disk", Value: fmt.Sprintf("%d GB %s", d.GetBootDiskSizeGb(), d.GetBootDiskType())})
		}
		out = append(out, console.PropertyGroup{Heading: fmt.Sprintf("Worker pool %d", i), Properties: props})
	}
	return out
}

// ---- Training pipelines

type trainingPipelinesProvider struct{ api vertexAPI }

func (trainingPipelinesProvider) ID() string           { return "vertex-training-pipelines" }
func (trainingPipelinesProvider) Title() string        { return "Training" }
func (trainingPipelinesProvider) ScopeKey() string     { return colabRegionKey }
func (trainingPipelinesProvider) CreateTitle() string  { return "Train new model" }
func (trainingPipelinesProvider) CreateSubmit() string { return "Start training" }

func (p trainingPipelinesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Name", "Training type", "Model", "Created", "Elapsed time", "Labels"},
		NameColumn: "ID", Noun: "training pipelines", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "A custom training pipeline runs its CustomJob's containers on this machine's Docker, with no network, then uploads the trained model to the Model Registry."}
	if project == "" {
		l.Prompt = "Training pipelines are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListTrainingPipelines(ctx, &aiplatformpb.ListTrainingPipelinesRequest{Parent: colabParent(ctx, project)}); ; {
		tp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		if pipelineActive(tp.GetState()) {
			actions = append(actions, cancelAction("training pipeline"))
		}
		model := tp.GetModelToUpload().GetDisplayName()
		if m := tp.GetModelToUpload().GetName(); m != "" {
			model = vertexLast(m)
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(tp.GetName()), Status: pipelineStateText(tp.GetState()), Actions: actions, Fields: map[string]string{
			"Name": dashIfEmpty(tp.GetDisplayName()), "Training type": trainingType(tp.GetTrainingTaskDefinition()), "Model": dashIfEmpty(model),
			"Created": optVertexTime(tp.GetCreateTime()), "Elapsed time": elapsed(tp.GetCreateTime(), tp.GetStartTime(), tp.GetEndTime()),
			"Labels": pipelineLabels(tp.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p trainingPipelinesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	tp, err := c.GetTrainingPipeline(ctx, &aiplatformpb.GetTrainingPipelineRequest{Name: colabParent(ctx, project) + "/trainingPipelines/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{
		{Label: "Name", Value: dashIfEmpty(tp.GetDisplayName())},
		{Label: "ID", Value: vertexLast(tp.GetName()), Copy: tp.GetName()},
		{Label: "Status", Value: pipelineStateText(tp.GetState())},
		{Label: "Region", Value: vertexLocation(tp.GetName())},
		{Label: "Training type", Value: trainingType(tp.GetTrainingTaskDefinition())},
		{Label: "Dataset", Value: map[bool]string{true: "No managed dataset", false: tp.GetInputDataConfig().GetDatasetId()}[tp.GetInputDataConfig().GetDatasetId() == ""]},
		{Label: "Created", Value: optVertexTime(tp.GetCreateTime())},
		{Label: "Start time", Value: optVertexTime(tp.GetStartTime())},
		{Label: "End time", Value: optVertexTime(tp.GetEndTime())},
		{Label: "Elapsed time", Value: elapsed(tp.GetCreateTime(), tp.GetStartTime(), tp.GetEndTime())},
		{Label: "Labels", Value: pipelineLabels(tp.GetLabels())},
	}
	if e := jobErrorText(tp.GetError().GetCode(), tp.GetError().GetMessage()); e != "" {
		props = append(props, console.Property{Label: "Error", Value: e})
	}
	groups := []console.PropertyGroup{{Heading: "Training pipeline", Properties: props}}
	// The training task's CustomJobSpec, read back from its inputs.
	spec := &aiplatformpb.CustomJobSpec{}
	if b, err := protojson.Marshal(tp.GetTrainingTaskInputs()); err == nil && tp.GetTrainingTaskInputs() != nil {
		_ = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, spec)
	}
	job := tp.GetTrainingTaskMetadata().GetStructValue().GetFields()["backingCustomJob"].GetStringValue()
	tc := []console.Property{{Label: "Model output directory", Value: dashIfEmpty(spec.GetBaseOutputDirectory().GetOutputUriPrefix())}}
	if job != "" {
		tc = append(tc, console.Property{Label: "Custom job", Value: vertexLast(job), Link: "/agent-platform/training/custom-jobs/" + vertexLast(job) + "?project=" + project + "&region=" + vertexLocation(job)})
	}
	groups = append(groups, console.PropertyGroup{Heading: "Training container", Properties: tc})
	groups = append(groups, workerPoolProps(spec)...)
	m := tp.GetModelToUpload()
	if m != nil {
		mp := []console.Property{{Label: "Model name", Value: dashIfEmpty(m.GetDisplayName())}}
		if n := m.GetName(); n != "" {
			mp = append(mp, console.Property{Label: "Model", Value: vertexLast(n), Copy: n})
		}
		if v := m.GetVersionId(); v != "" {
			mp = append(mp, console.Property{Label: "Version", Value: v})
		}
		if cs := m.GetContainerSpec(); cs != nil {
			mp = append(mp, console.Property{Label: "Container image", Value: cs.GetImageUri(), Copy: cs.GetImageUri()})
		} else {
			mp = append(mp, console.Property{Label: "Prediction container", Value: "No prediction container"})
		}
		groups = append(groups, console.PropertyGroup{Heading: "Prediction container", Properties: mp})
	}
	d := console.Detail{Title: dashIfEmpty(tp.GetDisplayName()),
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups}}}
	if pipelineActive(tp.GetState()) {
		d.Actions = []console.Action{cancelAction("training pipeline")}
	}
	return d, nil
}

func (trainingPipelinesProvider) CreateForm() (string, []console.Field) {
	var trainOpts, predOpts []string
	var trainWhen, predWhen []console.ConditionalOptions
	labels := map[string]string{}
	for _, f := range trainingFrameworks {
		for _, v := range trainingVersions[f] {
			trainOpts = append(trainOpts, f+"-"+v)
			labels[f+"-"+v] = v
		}
		for _, v := range predictionVersions[f] {
			predOpts = append(predOpts, f+"-"+v)
			labels[f+"-"+v] = v
		}
		var tw, pw []string
		for _, v := range trainingVersions[f] {
			tw = append(tw, f+"-"+v)
		}
		for _, v := range predictionVersions[f] {
			pw = append(pw, f+"-"+v)
		}
		trainWhen = append(trainWhen, console.ConditionalOptions{When: "framework=" + f, Options: tw})
		predWhen = append(predWhen, console.ConditionalOptions{When: "predFramework=" + f, Options: pw})
	}
	return "Start training", []console.Field{
		{Name: "dataset", Label: "Dataset", Type: "select", Section: "Training method", Options: []string{"none"}, Default: "none",
			OptionLabels: map[string]string{"none": "No managed dataset"}},
		{Name: "method", Label: "Training method", Type: "radio", Section: "Training method", Options: []string{"custom"}, Default: "custom",
			OptionLabels: map[string]string{"custom": "Custom training (advanced)"}},
		{Name: "modelMode", Label: "Model", Type: "radio", Section: "Model details", Options: []string{"new", "version"}, Default: "new",
			OptionLabels: map[string]string{"new": "Train new model", "version": "Train new version"}},
		{Name: "displayName", Label: "Name", Type: "text", Required: true, Section: "Model details", Pattern: `^.{1,128}$`,
			Help: "The training pipeline's name, and the model's."},
		{Name: "parentModel", Label: "Model", Type: "text", Section: "Model details", ShowWhen: "modelMode=version", Pattern: `^(projects/[^\/]+/locations/[^\/]+/models/[^\/@]+|[a-z0-9_\-]+)?$`,
			Help: "The model to add a version to: its ID, or projects/PROJECT/locations/REGION/models/ID."},
		{Name: "container", Label: "Training container", Type: "radio", Section: "Training container", Options: []string{"prebuilt", "custom"}, Default: "custom",
			OptionLabels: map[string]string{"prebuilt": "Prebuilt container", "custom": "Custom container"}},
		{Name: "framework", Label: "Model framework", Type: "select", Section: "Training container", ShowWhen: "container=prebuilt", Options: trainingFrameworks,
			Default: "scikit-learn", OptionLabels: trainingFrameworkLabels},
		{Name: "frameworkVersion", Label: "Model framework version", Type: "select", Section: "Training container", ShowWhen: "container=prebuilt",
			Options: trainOpts, OptionsWhen: trainWhen, OptionLabels: labels},
		{Name: "packageUri", Label: "Package location", Type: "text", Section: "Training container", ShowWhen: "container=prebuilt", Pattern: `^(gs://\S+(,\s*gs://\S+)*)?$`,
			Help: "The Cloud Storage URI of your Python training package (gs://BUCKET/trainer-0.1.tar.gz); several separated by commas."},
		{Name: "pythonModule", Label: "Python module", Type: "text", Section: "Training container", ShowWhen: "container=prebuilt", Help: "The module to run, such as trainer.task."},
		{Name: "image", Label: "Container image", Type: "text", Section: "Training container", ShowWhen: "container=custom",
			Help: "The training image: in this instance's Artifact Registry (REGION-docker.pkg.dev/PROJECT/REPOSITORY/IMAGE) or a public registry."},
		{Name: "outputDir", Label: "Model output directory", Type: "text", Section: "Training container", Pattern: `^(gs://[a-z0-9][\-a-z0-9._]+(/.*)?)?$`,
			Help: "gs://BUCKET/folder: the training writes the model to its model subdirectory (AIP_MODEL_DIR)."},
		{Name: "args", Label: "Arguments", Type: "textarea", Section: "Training container", Help: "One argument per line, passed to the training code."},
		{Name: "machineType", Label: "Machine type", Type: "select", Section: "Compute and pricing", Options: colabMachineTypes, Default: "n1-standard-4",
			Help: "Worker pool 0's machine; jobs run on this machine's CPU."},
		{Name: "replicaCount", Label: "Worker count", Type: "text", Section: "Compute and pricing", Default: "1", Pattern: `^[1-9][0-9]*$`},
		{Name: "diskSize", Label: "Disk size (GB)", Type: "text", Section: "Compute and pricing", Default: "100", Pattern: `^[1-9][0-9]*$`},
		{Name: "prediction", Label: "Prediction container", Type: "radio", Section: "Prediction container", Options: []string{"none", "prebuilt", "custom"}, Default: "none",
			OptionLabels: map[string]string{"none": "No prediction container", "prebuilt": "Prebuilt container", "custom": "Custom container"}},
		{Name: "predFramework", Label: "Model framework", Type: "select", Section: "Prediction container", ShowWhen: "prediction=prebuilt", Options: trainingFrameworks,
			Default: "scikit-learn", OptionLabels: trainingFrameworkLabels},
		{Name: "predFrameworkVersion", Label: "Model framework version", Type: "select", Section: "Prediction container", ShowWhen: "prediction=prebuilt",
			Options: predOpts, OptionsWhen: predWhen, OptionLabels: labels},
		{Name: "predImage", Label: "Container image", Type: "text", Section: "Prediction container", ShowWhen: "prediction=custom"},
		{Name: "predCommand", Label: "Command", Type: "text", Section: "Prediction container", ShowWhen: "prediction=custom", Help: "Optional: the serving command, separated by spaces."},
		{Name: "predRoute", Label: "Predict route", Type: "text", Section: "Prediction container", ShowWhen: "prediction=custom", Pattern: `^(/\S*)?$`},
		{Name: "healthRoute", Label: "Health route", Type: "text", Section: "Prediction container", ShowWhen: "prediction=custom", Pattern: `^(/\S*)?$`},
		{Name: "predPort", Label: "Port", Type: "text", Section: "Prediction container", ShowWhen: "prediction=custom", Pattern: `^([1-9][0-9]{0,4})?$`},
	}
}

// trainingPipelineOf builds the TrainingPipeline the Train new model pane
// describes.
func trainingPipelineOf(project, region string, values map[string]string) (*aiplatformpb.TrainingPipeline, error) {
	name := strings.TrimSpace(values["displayName"])
	if name == "" {
		return nil, errors.New("the training pipeline needs a name")
	}
	var args []string
	for _, a := range strings.Split(values["args"], "\n") {
		if a = strings.TrimSpace(a); a != "" {
			args = append(args, a)
		}
	}
	replicas, _ := strconv.Atoi(strings.TrimSpace(values["replicaCount"]))
	if replicas < 1 {
		replicas = 1
	}
	disk, _ := strconv.Atoi(strings.TrimSpace(values["diskSize"]))
	if disk < 1 {
		disk = 100
	}
	machine := values["machineType"]
	if machine == "" {
		machine = "n1-standard-4"
	}
	wp := map[string]any{"machineSpec": map[string]any{"machineType": machine}, "replicaCount": strconv.Itoa(replicas),
		"diskSpec": map[string]any{"bootDiskType": "pd-ssd", "bootDiskSizeGb": disk}}
	switch values["container"] {
	case "prebuilt":
		img, ok := trainingImages[values["frameworkVersion"]]
		if !ok {
			return nil, errors.New("choose the Model framework version")
		}
		var pkgs []string
		for _, u := range strings.Split(values["packageUri"], ",") {
			if u = strings.TrimSpace(u); u != "" {
				pkgs = append(pkgs, u)
			}
		}
		if len(pkgs) == 0 || strings.TrimSpace(values["pythonModule"]) == "" {
			return nil, errors.New("a prebuilt container needs the Package location and the Python module")
		}
		ps := map[string]any{"executorImageUri": img, "packageUris": pkgs, "pythonModule": strings.TrimSpace(values["pythonModule"])}
		if len(args) > 0 {
			ps["args"] = args
		}
		wp["pythonPackageSpec"] = ps
	default:
		img := strings.TrimSpace(values["image"])
		if img == "" {
			return nil, errors.New("a custom container needs its Container image")
		}
		cs := map[string]any{"imageUri": img}
		if len(args) > 0 {
			cs["args"] = args
		}
		wp["containerSpec"] = cs
	}
	spec := map[string]any{"workerPoolSpecs": []any{wp}}
	if out := strings.TrimSpace(values["outputDir"]); out != "" {
		spec["baseOutputDirectory"] = map[string]any{"outputUriPrefix": out}
	}
	b, _ := json.Marshal(spec)
	inputs := &structpb.Value{}
	if err := protojson.Unmarshal(b, inputs); err != nil {
		return nil, err
	}
	tp := &aiplatformpb.TrainingPipeline{DisplayName: name, TrainingTaskDefinition: customTaskDefinition, TrainingTaskInputs: inputs}
	switch values["prediction"] {
	case "prebuilt":
		img, ok := predictionImages[values["predFrameworkVersion"]]
		if !ok {
			return nil, errors.New("choose the prediction container's Model framework version")
		}
		tp.ModelToUpload = &aiplatformpb.Model{DisplayName: name, ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: img}}
	case "custom":
		img := strings.TrimSpace(values["predImage"])
		if img == "" {
			return nil, errors.New("a custom prediction container needs its Container image")
		}
		cs := &aiplatformpb.ModelContainerSpec{ImageUri: img, Command: strings.Fields(values["predCommand"]),
			PredictRoute: strings.TrimSpace(values["predRoute"]), HealthRoute: strings.TrimSpace(values["healthRoute"])}
		if port, err := strconv.Atoi(strings.TrimSpace(values["predPort"])); err == nil && port > 0 {
			cs.Ports = []*aiplatformpb.Port{{ContainerPort: int32(port)}}
		}
		tp.ModelToUpload = &aiplatformpb.Model{DisplayName: name, ContainerSpec: cs}
	}
	if values["modelMode"] == "version" {
		pm := strings.TrimSpace(values["parentModel"])
		if pm == "" {
			return nil, errors.New("Train new version needs the model to add a version to")
		}
		if !strings.HasPrefix(pm, "projects/") {
			pm = "projects/" + project + "/locations/" + region + "/models/" + pm
		}
		if tp.ModelToUpload == nil {
			return nil, errors.New("a new version needs a prediction container")
		}
		tp.ParentModel = pm
	}
	return tp, nil
}

func (p trainingPipelinesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before training a model")
	}
	tp, err := trainingPipelineOf(project, colabRegion(ctx), values)
	if err != nil {
		return "", err
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	out, err := c.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: colabParent(ctx, project), TrainingPipeline: tp})
	if err != nil {
		return "", err
	}
	return vertexLast(out.GetName()), nil
}

func (p trainingPipelinesProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteTrainingPipeline(ctx, &aiplatformpb.DeleteTrainingPipelineRequest{Name: colabParent(ctx, project) + "/trainingPipelines/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (trainingPipelinesProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p trainingPipelinesProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.CancelTrainingPipeline(ctx, &aiplatformpb.CancelTrainingPipelineRequest{Name: colabParent(ctx, project) + "/trainingPipelines/" + name})
}

// ---- Custom jobs

type customJobsProvider struct{ api vertexAPI }

func (customJobsProvider) ID() string       { return "vertex-custom-jobs" }
func (customJobsProvider) Title() string    { return "Training" }
func (customJobsProvider) ScopeKey() string { return colabRegionKey }

func (p customJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Name", "Created", "Elapsed time", "Labels"},
		NameColumn: "ID", Noun: "custom jobs", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each worker of a custom job is a container of its image on this machine's Docker, with no network. Create one from the API, a pipeline's CustomTrainingJobOp, or a training pipeline."}
	if project == "" {
		l.Prompt = "Custom jobs are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListCustomJobs(ctx, &aiplatformpb.ListCustomJobsRequest{Parent: colabParent(ctx, project)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		if jobActive(j.GetState()) {
			actions = append(actions, cancelAction("custom job"))
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: jobStateText(j.GetState()), Actions: actions, Fields: map[string]string{
			"Name": dashIfEmpty(j.GetDisplayName()), "Created": optVertexTime(j.GetCreateTime()),
			"Elapsed time": elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime()), "Labels": pipelineLabels(j.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p customJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	j, err := c.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: colabParent(ctx, project) + "/customJobs/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	sa := j.GetJobSpec().GetServiceAccount()
	if sa == "" {
		sa = "Vertex AI Custom Code Service Agent"
	}
	props := []console.Property{
		{Label: "Name", Value: dashIfEmpty(j.GetDisplayName())},
		{Label: "ID", Value: vertexLast(j.GetName()), Copy: j.GetName()},
		{Label: "Status", Value: jobStateText(j.GetState())},
		{Label: "Region", Value: vertexLocation(j.GetName())},
		{Label: "Created", Value: optVertexTime(j.GetCreateTime())},
		{Label: "Start time", Value: optVertexTime(j.GetStartTime())},
		{Label: "End time", Value: optVertexTime(j.GetEndTime())},
		{Label: "Elapsed time", Value: elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime())},
		{Label: "Base output directory", Value: dashIfEmpty(j.GetJobSpec().GetBaseOutputDirectory().GetOutputUriPrefix())},
		{Label: "Service account", Value: sa},
		{Label: "Labels", Value: pipelineLabels(j.GetLabels())},
	}
	if e := jobErrorText(j.GetError().GetCode(), j.GetError().GetMessage()); e != "" {
		props = append(props, console.Property{Label: "Error", Value: e})
	}
	groups := append([]console.PropertyGroup{{Heading: "Custom job", Properties: props}}, workerPoolProps(j.GetJobSpec())...)
	d := console.Detail{Title: dashIfEmpty(j.GetDisplayName()),
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups}}}
	if jobActive(j.GetState()) {
		d.Actions = []console.Action{cancelAction("custom job")}
	}
	return d, nil
}

func (p customJobsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteCustomJob(ctx, &aiplatformpb.DeleteCustomJobRequest{Name: colabParent(ctx, project) + "/customJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (customJobsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p customJobsProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.CancelCustomJob(ctx, &aiplatformpb.CancelCustomJobRequest{Name: colabParent(ctx, project) + "/customJobs/" + name})
}

// ---- Batch predictions

type batchPredictionsProvider struct{ api vertexAPI }

func (batchPredictionsProvider) ID() string           { return "vertex-batch-predictions" }
func (batchPredictionsProvider) Title() string        { return "Batch inference" }
func (batchPredictionsProvider) ScopeKey() string     { return colabRegionKey }
func (batchPredictionsProvider) CreateTitle() string  { return "New batch prediction" }
func (batchPredictionsProvider) CreateSubmit() string { return "Create" }
func (batchPredictionsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p batchPredictionsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Name", "Model", "Created", "Elapsed time", "Labels"},
		NameColumn: "ID", Noun: "batch predictions", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "A batch prediction runs the model's serving container in this instance's cluster, reads its input from Cloud Storage or BigQuery and writes its predictions there."}
	if project == "" {
		l.Prompt = "Batch predictions are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListBatchPredictionJobs(ctx, &aiplatformpb.ListBatchPredictionJobsRequest{Parent: colabParent(ctx, project)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		if jobActive(j.GetState()) {
			actions = append(actions, cancelAction("batch prediction"))
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: jobStateText(j.GetState()), Actions: actions, Fields: map[string]string{
			"Name": dashIfEmpty(j.GetDisplayName()), "Model": dashIfEmpty(vertexLast(j.GetModel())), "Created": optVertexTime(j.GetCreateTime()),
			"Elapsed time": elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime()), "Labels": pipelineLabels(j.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func batchSource(j *aiplatformpb.BatchPredictionJob) string {
	in := j.GetInputConfig()
	if u := in.GetBigquerySource().GetInputUri(); u != "" {
		return u
	}
	return dashIfEmpty(strings.Join(in.GetGcsSource().GetUris(), ", "))
}

func batchDestination(j *aiplatformpb.BatchPredictionJob) string {
	out := j.GetOutputConfig()
	if u := out.GetBigqueryDestination().GetOutputUri(); u != "" {
		return u
	}
	return dashIfEmpty(out.GetGcsDestination().GetOutputUriPrefix())
}

func (p batchPredictionsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	j, err := c.GetBatchPredictionJob(ctx, &aiplatformpb.GetBatchPredictionJobRequest{Name: colabParent(ctx, project) + "/batchPredictionJobs/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	output := j.GetOutputInfo().GetGcsOutputDirectory()
	if t := j.GetOutputInfo().GetBigqueryOutputTable(); t != "" {
		output = j.GetOutputInfo().GetBigqueryOutputDataset() + "." + t
	}
	nodes := j.GetDedicatedResources().GetStartingReplicaCount()
	machine := j.GetDedicatedResources().GetMachineSpec().GetMachineType()
	props := []console.Property{
		{Label: "Name", Value: dashIfEmpty(j.GetDisplayName())},
		{Label: "ID", Value: vertexLast(j.GetName()), Copy: j.GetName()},
		{Label: "Status", Value: jobStateText(j.GetState())},
		{Label: "Region", Value: vertexLocation(j.GetName())},
		{Label: "Model", Value: dashIfEmpty(vertexLast(j.GetModel())), Copy: j.GetModel()},
		{Label: "Model version ID", Value: dashIfEmpty(j.GetModelVersionId())},
		{Label: "Source format", Value: dashIfEmpty(j.GetInputConfig().GetInstancesFormat())},
		{Label: "Source", Value: batchSource(j)},
		{Label: "Destination format", Value: dashIfEmpty(j.GetOutputConfig().GetPredictionsFormat())},
		{Label: "Destination path", Value: batchDestination(j)},
		{Label: "Output location", Value: dashIfEmpty(output)},
		{Label: "Machine type", Value: dashIfEmpty(machine)},
		{Label: "Number of compute nodes", Value: map[bool]string{true: "—", false: strconv.Itoa(int(nodes))}[nodes == 0]},
		{Label: "Feature attributions", Value: map[bool]string{true: "Enabled", false: "Disabled"}[j.GetGenerateExplanation()]},
		{Label: "Created", Value: optVertexTime(j.GetCreateTime())},
		{Label: "Start time", Value: optVertexTime(j.GetStartTime())},
		{Label: "End time", Value: optVertexTime(j.GetEndTime())},
		{Label: "Elapsed time", Value: elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime())},
		{Label: "Labels", Value: pipelineLabels(j.GetLabels())},
	}
	if s := j.GetCompletionStats(); s != nil {
		props = append(props, console.Property{Label: "Successful predictions", Value: strconv.FormatInt(s.GetSuccessfulCount(), 10)},
			console.Property{Label: "Failed predictions", Value: strconv.FormatInt(s.GetFailedCount(), 10)})
	}
	if e := jobErrorText(j.GetError().GetCode(), j.GetError().GetMessage()); e != "" {
		props = append(props, console.Property{Label: "Error", Value: e})
	}
	for _, pe := range j.GetPartialFailures() {
		props = append(props, console.Property{Label: "Partial failure", Value: pe.GetMessage()})
	}
	d := console.Detail{Title: dashIfEmpty(j.GetDisplayName()),
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Batch prediction", Properties: props}}}}}
	if jobActive(j.GetState()) {
		d.Actions = []console.Action{cancelAction("batch prediction")}
	}
	return d, nil
}

func (p batchPredictionsProvider) CreateForm() (string, []console.Field) {
	return "Create", p.fields(nil)
}

// CreateFormFor offers the region's models as Model name.
func (p batchPredictionsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	var models []string
	labels := map[string]string{}
	if project != "" {
		if c, err := p.api.models(ctx); err == nil {
			for it := c.ListModels(ctx, &aiplatformpb.ListModelsRequest{Parent: colabParent(ctx, project)}); ; {
				m, err := it.Next()
				if err != nil {
					break
				}
				models = append(models, m.GetName())
				labels[m.GetName()] = m.GetDisplayName()
			}
			_ = c.Close()
		}
	}
	f := p.fields(models)
	for i := range f {
		if f[i].Name == "model" {
			f[i].OptionLabels = labels
		}
	}
	return f
}

func (batchPredictionsProvider) fields(models []string) []console.Field {
	model := console.Field{Name: "model", Label: "Model name", Type: "select", Required: true, Section: "Define your batch prediction", Options: models,
		Help: "A model of the Model Registry in this region, with a prediction container."}
	if models == nil {
		model.Type, model.Options = "text", nil
		model.Help = "The model's resource name: projects/PROJECT/locations/REGION/models/ID."
	}
	return []console.Field{
		{Name: "displayName", Label: "Batch prediction name", Type: "text", Required: true, Section: "Define your batch prediction", Pattern: `^.{1,128}$`},
		model,
		{Name: "source", Label: "Select source", Type: "radio", Section: "Define your batch prediction", Options: []string{"gcs", "gcs-list", "bigquery"}, Default: "gcs",
			OptionLabels: map[string]string{"gcs": "File on Cloud Storage (JSON Lines, CSV, TFRecord, TFRecord Gzip)", "gcs-list": "Files on Cloud Storage (other)", "bigquery": "BigQuery path"}},
		{Name: "sourcePath", Label: "Source path", Type: "text", Section: "Define your batch prediction", ShowWhen: "source=gcs", Pattern: `^(gs://[a-z0-9][\-a-z0-9._]+/\S+)?$`,
			Help: "gs://BUCKET/instances.jsonl: its extension (.jsonl, .csv, .tfrecord, .gz) is its format."},
		{Name: "fileList", Label: "Files", Type: "textarea", Section: "Define your batch prediction", ShowWhen: "source=gcs-list",
			Help: "One gs:// file per line, each sent to the model as one instance."},
		{Name: "bqSource", Label: "BigQuery path", Type: "text", Section: "Define your batch prediction", ShowWhen: "source=bigquery", Pattern: `^(bq://[^.\s]+\.[^.\s]+\.[^.\s]+)?$`,
			Help: "bq://PROJECT.DATASET.TABLE. BigQuery input writes its predictions to BigQuery."},
		{Name: "destination", Label: "Destination path", Type: "text", Section: "Define your batch prediction", ShowWhen: "source=gcs|gcs-list", Pattern: `^(gs://[a-z0-9][\-a-z0-9._]+(/\S*)?)?$`,
			Help: "gs://BUCKET/folder: the predictions are written under it."},
		{Name: "bqDestination", Label: "BigQuery destination", Type: "text", Section: "Define your batch prediction", ShowWhen: "source=bigquery", Pattern: `^(bq://[^.\s]+(\.[^.\s]+)?)?$`,
			Help: "bq://PROJECT or bq://PROJECT.DATASET: a dataset (made when only the project is named) and table of predictions."},
		{Name: "explain", Label: "Enable feature attributions for this model", Type: "checkbox", Section: "Define your batch prediction"},
		{Name: "nodes", Label: "Number of compute nodes", Type: "text", Section: "Compute", Default: "1", Pattern: `^[1-9][0-9]*$`},
		{Name: "machineType", Label: "Machine type", Type: "select", Section: "Compute", Options: colabMachineTypes, Default: "n1-standard-4"},
		{Name: "accelerator", Label: "Accelerator type", Type: "select", Section: "Compute", Options: colabAccelerators, Default: "NONE"},
		{Name: "acceleratorCount", Label: "Accelerator count", Type: "text", Section: "Compute", ShowWhen: "accelerator=NVIDIA_TESLA_T4|NVIDIA_L4|NVIDIA_TESLA_V100|NVIDIA_TESLA_A100|NVIDIA_A100_80GB",
			Default: "1", Pattern: `^[1-9][0-9]*$`},
	}
}

// instancesFormat is a Cloud Storage source file's format, by its
// extension.
func instancesFormat(uri string) (string, error) {
	u := strings.ToLower(uri)
	switch {
	case strings.HasSuffix(u, ".jsonl"), strings.HasSuffix(u, ".json"):
		return "jsonl", nil
	case strings.HasSuffix(u, ".csv"):
		return "csv", nil
	case strings.HasSuffix(u, ".tfrecord"):
		return "tf-record", nil
	case strings.HasSuffix(u, ".gz"):
		return "tf-record-gzip", nil
	}
	return "", fmt.Errorf("the Source path %s is not a JSON Lines, CSV, TFRecord or TFRecord Gzip file (.jsonl, .csv, .tfrecord, .gz)", uri)
}

// batchPredictionOf builds the job the New batch prediction page describes.
func batchPredictionOf(values map[string]string) (*aiplatformpb.BatchPredictionJob, error) {
	name := strings.TrimSpace(values["displayName"])
	model := strings.TrimSpace(values["model"])
	if name == "" || model == "" {
		return nil, errors.New("the batch prediction needs a name and a model")
	}
	j := &aiplatformpb.BatchPredictionJob{DisplayName: name, Model: model, InputConfig: &aiplatformpb.BatchPredictionJob_InputConfig{},
		OutputConfig: &aiplatformpb.BatchPredictionJob_OutputConfig{}, GenerateExplanation: values["explain"] == "true"}
	switch values["source"] {
	case "bigquery":
		src := strings.TrimSpace(values["bqSource"])
		if src == "" {
			return nil, errors.New("BigQuery path is required")
		}
		j.InputConfig.InstancesFormat = "bigquery"
		j.InputConfig.Source = &aiplatformpb.BatchPredictionJob_InputConfig_BigquerySource{BigquerySource: &aiplatformpb.BigQuerySource{InputUri: src}}
		dst := strings.TrimSpace(values["bqDestination"])
		if dst == "" {
			dst = "bq://" + strings.SplitN(strings.TrimPrefix(src, "bq://"), ".", 2)[0]
		}
		j.OutputConfig.PredictionsFormat = "bigquery"
		j.OutputConfig.Destination = &aiplatformpb.BatchPredictionJob_OutputConfig_BigqueryDestination{BigqueryDestination: &aiplatformpb.BigQueryDestination{OutputUri: dst}}
	default:
		var uris []string
		if values["source"] == "gcs-list" {
			for _, l := range strings.Split(values["fileList"], "\n") {
				if l = strings.TrimSpace(l); l != "" {
					uris = append(uris, l)
				}
			}
			j.InputConfig.InstancesFormat = "file-list"
			if len(uris) == 0 {
				return nil, errors.New("list the files on Cloud Storage")
			}
		} else {
			src := strings.TrimSpace(values["sourcePath"])
			if src == "" {
				return nil, errors.New("Source path is required")
			}
			f, err := instancesFormat(src)
			if err != nil {
				return nil, err
			}
			uris = []string{src}
			j.InputConfig.InstancesFormat = f
		}
		j.InputConfig.Source = &aiplatformpb.BatchPredictionJob_InputConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: uris}}
		dst := strings.TrimSpace(values["destination"])
		if dst == "" {
			return nil, errors.New("Destination path is required")
		}
		j.OutputConfig.PredictionsFormat = "jsonl"
		j.OutputConfig.Destination = &aiplatformpb.BatchPredictionJob_OutputConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: dst}}
	}
	nodes, _ := strconv.Atoi(strings.TrimSpace(values["nodes"]))
	if nodes < 1 {
		nodes = 1
	}
	machine := values["machineType"]
	if machine == "" {
		machine = "n1-standard-4"
	}
	ms := &aiplatformpb.MachineSpec{MachineType: machine}
	if a := values["accelerator"]; a != "" && a != "NONE" {
		v, ok := aiplatformpb.AcceleratorType_value[a]
		if !ok {
			return nil, fmt.Errorf("unknown accelerator type %q", a)
		}
		ms.AcceleratorType = aiplatformpb.AcceleratorType(v)
		n, _ := strconv.Atoi(values["acceleratorCount"])
		if n < 1 {
			n = 1
		}
		ms.AcceleratorCount = int32(n)
	}
	j.DedicatedResources = &aiplatformpb.BatchDedicatedResources{MachineSpec: ms, StartingReplicaCount: int32(nodes), MaxReplicaCount: int32(nodes)}
	return j, nil
}

func (p batchPredictionsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a batch prediction")
	}
	j, err := batchPredictionOf(values)
	if err != nil {
		return "", err
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	out, err := c.CreateBatchPredictionJob(ctx, &aiplatformpb.CreateBatchPredictionJobRequest{Parent: colabParent(ctx, project), BatchPredictionJob: j})
	if err != nil {
		return "", err
	}
	return vertexLast(out.GetName()), nil
}

func (p batchPredictionsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteBatchPredictionJob(ctx, &aiplatformpb.DeleteBatchPredictionJobRequest{Name: colabParent(ctx, project) + "/batchPredictionJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (batchPredictionsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p batchPredictionsProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.CancelBatchPredictionJob(ctx, &aiplatformpb.CancelBatchPredictionJobRequest{Name: colabParent(ctx, project) + "/batchPredictionJobs/" + name})
}
