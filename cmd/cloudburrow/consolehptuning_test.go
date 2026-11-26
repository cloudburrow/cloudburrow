package main

import (
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestTrainNewModelHyperparameterTuning (#2210): the Hyperparameter tuning
// step makes the pipeline a hyperparameter tuning task whose trial job is
// the custom task's CustomJobSpec.
func TestTrainNewModelHyperparameterTuning(t *testing.T) {
	tp, err := trainingPipelineOf("p", "us-central1", map[string]string{"displayName": "tuned", "container": "custom",
		"image": "us-central1-docker.pkg.dev/p/r/train:1", "outputDir": "gs://b/out", "hpEnable": "true",
		"hpParams": "lr DOUBLE 0.001 0.1 log\nlayers INTEGER 1 4\nopt CATEGORICAL adam,sgd\nbatch DISCRETE 16,32,64 linear",
		"hpMetric": "accuracy", "hpGoal": "MAXIMIZE", "hpMaxTrials": "8", "hpParallel": "2", "hpAlgorithm": "RANDOM_SEARCH"})
	if err != nil {
		t.Fatal(err)
	}
	if tp.GetTrainingTaskDefinition() != hpTaskDefinition {
		t.Fatalf("definition %s", tp.GetTrainingTaskDefinition())
	}
	b, _ := protojson.Marshal(tp.GetTrainingTaskInputs())
	hp := &aiplatformpb.HyperparameterTuningJob{}
	if err := protojson.Unmarshal(b, hp); err != nil {
		t.Fatalf("the inputs are not a HyperparameterTuningJobSpec: %v (%s)", err, b)
	}
	ps := hp.GetStudySpec().GetParameters()
	if hp.GetMaxTrialCount() != 8 || hp.GetParallelTrialCount() != 2 || hp.GetStudySpec().GetAlgorithm() != aiplatformpb.StudySpec_RANDOM_SEARCH ||
		len(ps) != 4 || ps[0].GetScaleType() != aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE || ps[1].GetIntegerValueSpec().GetMaxValue() != 4 ||
		len(ps[2].GetCategoricalValueSpec().GetValues()) != 2 || len(ps[3].GetDiscreteValueSpec().GetValues()) != 3 ||
		hp.GetStudySpec().GetMetrics()[0].GetMetricId() != "accuracy" ||
		hp.GetTrialJobSpec().GetWorkerPoolSpecs()[0].GetContainerSpec().GetImageUri() != "us-central1-docker.pkg.dev/p/r/train:1" ||
		hp.GetTrialJobSpec().GetBaseOutputDirectory().GetOutputUriPrefix() != "gs://b/out" {
		t.Errorf("the tuning task = %v", hp)
	}
	for _, bad := range []string{"", "lr DOUBLE x 1", "lr FLOAT 0 1", "opt CATEGORICAL a,b log", "n INTEGER 0.5 2", "lr DOUBLE 0 1 cubic"} {
		if _, err := trainingPipelineOf("p", "us-central1", map[string]string{"displayName": "x", "container": "custom", "image": "i",
			"hpEnable": "true", "hpParams": bad, "hpMetric": "m", "hpMaxTrials": "1", "hpParallel": "1"}); err == nil {
			t.Errorf("hyperparameters %q were accepted", bad)
		}
	}
}
