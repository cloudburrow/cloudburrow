package vertexai

// Feature attribution monitoring (#2214): v2's FeatureAttributionSpec
// (objective "feature-attribution") and v1's explanation_config
// (FEATURE_ATTRIBUTION_SKEW and FEATURE_ATTRIBUTION_DRIFT). The baseline's
// and the target's rows are explained by Sampled Shapley (explain.go), on
// this instance: v1 through the job's endpoint (its deployed model's
// explanation spec), v2 as a batch explanation, the model version's
// container run for the job as a batch prediction's is.
//
// Documented behaviour ("Monitor feature attributions",
// https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/monitor-explainable-ai;
// ModelDeploymentMonitoringJob REST reference; read 2026-11-24):
//   - the model's "ExplanationSpec.ExplanationParameters field must be
//     populated";
//   - a multi-dimensional feature's attribution is "adding up the
//     attributions across dimensions";
//   - skew thresholds are "against attribution score distance between the
//     training and prediction feature", drift thresholds "between different
//     time windows";
//   - "Each feature must have a non-zero threshold if they need to be
//     monitored. Otherwise no alert will be triggered for that feature";
//   - v2's objective is "SHAP value (SHapley Additive exPlanations)".
//
// UNVERIFIED (Google does not say; re-read 2026-11-24): a feature's
// attribution score is its mean absolute attribution over the rows, and
// the distance is the absolute difference of the two scores; at most
// maxAttributionRows rows of each dataset are explained.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// maxAttributionRows bounds the rows of one dataset a run explains.
const maxAttributionRows = 500

// explainer explains instances.
type explainer func(instances []*structpb.Value) (*aiplatformpb.ExplainResponse, error)

// attributionScores is each feature's mean absolute attribution over rows
// (the first explained output's), explained in batches small enough for
// one Explain.
//
// A numeric feature's value read as text (a CSV file's every value is) is
// sent as the number it spells, as the model is served it.
func attributionScores(rows []map[string]any, features []string, numeric map[string]bool, explain explainer) (map[string]float64, int, error) {
	if len(rows) > maxAttributionRows {
		rows = rows[:maxAttributionRows]
	}
	want := map[string]bool{}
	for _, f := range features {
		want[f] = true
	}
	var insts []*structpb.Value
	for _, r := range rows {
		m := map[string]any{}
		for k, v := range r {
			if !want[k] {
				continue
			}
			if str, ok := v.(string); ok && numeric[k] {
				if f, err := strconv.ParseFloat(strings.TrimSpace(str), 64); err == nil {
					v = f
				}
			}
			m[k] = v
		}
		v, err := structpb.NewValue(m)
		if err != nil {
			return nil, 0, apierror.InvalidArgument("a row cannot be sent to the model: %v", err)
		}
		insts = append(insts, v)
	}
	sum := map[string]float64{}
	n := 0
	for chunk, i := len(insts), 0; i < len(insts); {
		end := min(i+chunk, len(insts))
		resp, err := explain(insts[i:end])
		if err != nil {
			if chunk > 1 && strings.Contains(apierror.From(err).Message, "needs more than") {
				chunk = max(1, chunk/2)
				continue
			}
			return nil, 0, err
		}
		for _, e := range resp.GetExplanations() {
			if len(e.GetAttributions()) == 0 {
				continue
			}
			n++
			for k, v := range e.GetAttributions()[0].GetFeatureAttributions().GetStructValue().GetFields() {
				sum[k] += math.Abs(attributionTotal(v))
			}
		}
		i = end
	}
	out := map[string]float64{}
	for _, f := range features {
		if n > 0 {
			out[f] = sum[f] / float64(n)
		}
	}
	return out, n, nil
}

// attributionTotal is an attribution summed across its dimensions.
func attributionTotal(v *structpb.Value) float64 {
	switch k := v.GetKind().(type) {
	case *structpb.Value_NumberValue:
		return k.NumberValue
	case *structpb.Value_ListValue:
		t := 0.0
		for _, x := range k.ListValue.GetValues() {
			t += attributionTotal(x)
		}
		return t
	}
	return 0
}

// explanationOverride is the spec as an Explain request's override.
func explanationOverride(es *aiplatformpb.ExplanationSpec) *aiplatformpb.ExplanationSpecOverride {
	if es == nil {
		return nil
	}
	return &aiplatformpb.ExplanationSpecOverride{Parameters: es.GetParameters(),
		Metadata: &aiplatformpb.ExplanationMetadataOverride{Inputs: overrideInputs(es.GetMetadata())}}
}

// sortedKeys are a score map's features in order.
func sortedKeys(m map[string]float64) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- v1: through the job's endpoint

// mdmAttributionStats are a v1 run's attribution skew and drift stats of
// one deployed model.
func (s *Server) mdmAttributionStats(ctx context.Context, job *aiplatformpb.ModelDeploymentMonitoringJob,
	c *aiplatformpb.ModelDeploymentMonitoringObjectiveConfig, fields []monitorField, train, prev, cur []map[string]any, start, end time.Time) ([]*mdmStat, error) {
	oc := c.GetObjectiveConfig()
	if !oc.GetExplanationConfig().GetEnableFeatureAttributes() || len(cur) == 0 {
		return nil, nil
	}
	parts := strings.Split(job.GetName(), "/")
	if len(parts) < 4 {
		return nil, apierror.InvalidArgument("job name %q is malformed", job.GetName())
	}
	t := target{project: parts[1], location: parts[3], endpoint: lastSegment(job.GetEndpoint())}
	explain := func(insts []*structpb.Value) (*aiplatformpb.ExplainResponse, error) {
		return s.explain(ctx, t, &aiplatformpb.ExplainRequest{Endpoint: job.GetEndpoint(), Instances: insts, DeployedModelId: c.GetDeployedModelId()})
	}
	var names []string
	numeric := map[string]bool{}
	for _, f := range fields {
		names = append(names, f.Name)
		numeric[f.Name] = f.Numeric
	}
	curScores, _, err := attributionScores(cur, names, numeric, explain)
	if err != nil {
		return nil, err
	}
	var out []*mdmStat
	add := func(obj aiplatformpb.ModelDeploymentMonitoringObjectiveType, base map[string]float64, th map[string]*aiplatformpb.ThresholdConfig) {
		for _, f := range names {
			d := &featureDistance{Field: monitorField{Name: f}, Distance: math.Abs(curScores[f] - base[f])}
			out = append(out, mdmStatOf(job.GetName(), c.GetDeployedModelId(), obj, d, th[f].GetValue(), start, end))
		}
	}
	if sk := oc.GetTrainingPredictionSkewDetectionConfig(); sk != nil && len(train) > 0 {
		base, _, err := attributionScores(train, names, numeric, explain)
		if err != nil {
			return out, err
		}
		add(aiplatformpb.ModelDeploymentMonitoringObjectiveType_FEATURE_ATTRIBUTION_SKEW, base, sk.GetAttributionScoreSkewThresholds())
	}
	if dr := oc.GetPredictionDriftDetectionConfig(); dr != nil && len(prev) > 0 {
		base, _, err := attributionScores(prev, names, numeric, explain)
		if err != nil {
			return out, err
		}
		add(aiplatformpb.ModelDeploymentMonitoringObjectiveType_FEATURE_ATTRIBUTION_DRIFT, base, dr.GetAttributionScoreDriftThresholds())
	}
	return out, nil
}

// ---- v2: a batch explanation on the model version's container

// batchExplainer runs the monitor's model version's container for job and
// explains through it; stop removes the container.
func (s *Server) batchExplainer(ctx context.Context, m *betapb.ModelMonitor, job string) (explainer, func(), error) {
	r, err := s.needRegistry()
	if err != nil {
		return nil, nil, err
	}
	vm := m.GetModelMonitoringTarget().GetVertexModel()
	ref := vm.GetModel()
	if vm.GetModelVersionId() != "" {
		ref += "@" + vm.GetModelVersionId()
	}
	model, err := r.get(ref)
	if err != nil {
		return nil, nil, err
	}
	es := model.GetExplanationSpec()
	if b := m.GetExplanationSpec(); b != nil {
		es = &aiplatformpb.ExplanationSpec{}
		raw, _ := protojson.Marshal(b)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, es); err != nil {
			return nil, nil, apierror.InvalidArgument("model_monitor.explanation_spec: %v", err)
		}
	}
	if es.GetParameters() == nil {
		return nil, nil, apierror.FailedPrecondition("feature attribution monitoring needs an explanation spec: the ModelMonitor's explanation_spec, or the model's (%s)", ref)
	}
	if r.runner == nil {
		return nil, nil, apierror.FailedPrecondition("feature attribution monitoring runs the model's container, which needs Docker on this instance")
	}
	parts := strings.Split(job, "/")
	project, location := parts[1], parts[3]
	id := "mm" + lastSegment(job)
	run, err := batchContainer(&aiplatformpb.BatchPredictionJob{Name: "projects/" + project + "/locations/" + location + "/batchPredictionJobs/" + id}, r, model, project, location)
	if err != nil {
		return nil, nil, err
	}
	url, err := r.runner.Run(ctx, project, run)
	stop := func() { _ = r.runner.Remove(context.Background(), project, run.ID) }
	if err != nil {
		stop()
		return nil, nil, apierror.FailedPrecondition("the model's container did not start: %v", err)
	}
	predict := func(body []byte) (json.RawMessage, error) {
		a, err := s.forward(ctx, id, url+run.PredictRoute, body, "application/json")
		if err != nil {
			return nil, err
		}
		if a.status/100 != 2 {
			return nil, containerError(id, a)
		}
		return a.body, nil
	}
	override := explanationOverride(es)
	return func(insts []*structpb.Value) (*aiplatformpb.ExplainResponse, error) {
		return s.explainWith(ref, &aiplatformpb.ExplainRequest{Instances: insts, ExplanationSpecOverride: override}, predict)
	}, stop, nil
}

// monitorAttribution is a v2 run's feature attribution stats and alerts.
func (s *Server) monitorAttribution(ctx context.Context, m *betapb.ModelMonitor, monitorName, job, schedule string,
	fa *betapb.ModelMonitoringObjectiveSpec_FeatureAttributionSpec, base, target []map[string]any, now time.Time) ([]*mmStat, []*mmAlert, error) {
	features := fa.GetFeatures()
	if len(features) == 0 {
		for _, f := range m.GetModelMonitoringSchema().GetFeatureFields() {
			features = append(features, f.GetName())
		}
	}
	if len(features) == 0 {
		return nil, nil, apierror.InvalidArgument("feature_attribution_spec.features is empty and the ModelMonitor's schema has no feature fields")
	}
	numeric := map[string]bool{}
	for _, f := range m.GetModelMonitoringSchema().GetFeatureFields() {
		numeric[f.GetName()], _ = fieldNumeric(f.GetDataType())
	}
	explain, stop, err := s.batchExplainer(ctx, m, job)
	if err != nil {
		return nil, nil, err
	}
	defer stop()
	bs, _, err := attributionScores(base, features, numeric, explain)
	if err != nil {
		return nil, nil, err
	}
	ts, _, err := attributionScores(target, features, numeric, explain)
	if err != nil {
		return nil, nil, err
	}
	var stats []*mmStat
	var alerts []*mmAlert
	for _, f := range features {
		dev := math.Abs(ts[f] - bs[f])
		p := &betapb.ModelMonitoringStatsDataPoint{
			CurrentStats:       &betapb.ModelMonitoringStatsDataPoint_TypedValue{Value: &betapb.ModelMonitoringStatsDataPoint_TypedValue_DoubleValue{DoubleValue: ts[f]}},
			BaselineStats:      &betapb.ModelMonitoringStatsDataPoint_TypedValue{Value: &betapb.ModelMonitoringStatsDataPoint_TypedValue_DoubleValue{DoubleValue: bs[f]}},
			ModelMonitoringJob: job, Schedule: schedule, CreateTime: timestamppb.New(now), Algorithm: attributionAlgorithm,
		}
		cond := fa.GetDefaultAlertCondition()
		if c := fa.GetFeatureAlertConditions()[f]; c != nil {
			cond = c
		}
		if cond != nil {
			p.ThresholdValue = cond.GetThreshold()
			p.HasAnomaly = dev > p.ThresholdValue
		}
		stats = append(stats, &mmStat{Monitor: monitorName, Objective: objAttribution, StatsName: f, Point: p})
		if p.HasAnomaly {
			body, _ := structpb.NewValue(map[string]any{"featureName": f, "baselineAttribution": bs[f], "targetAttribution": ts[f],
				"distance": dev, "threshold": p.ThresholdValue, "algorithm": attributionAlgorithm})
			alerts = append(alerts, &mmAlert{Monitor: monitorName, Alert: &betapb.ModelMonitoringAlert{StatsName: f, ObjectiveType: objAttribution,
				AlertTime: timestamppb.New(now), Anomaly: &betapb.ModelMonitoringAnomaly{ModelMonitoringJob: job, Algorithm: attributionAlgorithm,
					Anomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly_{TabularAnomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly{
						Summary: fmt.Sprintf("the attribution score of %s moved from %g to %g, %g, above the threshold %g", f, bs[f], ts[f], dev, p.ThresholdValue),
						Anomaly: body, TriggerTime: timestamppb.New(now), Condition: cond}}}}})
		}
	}
	return stats, alerts, nil
}

// attributionAlgorithm names how attribution stats are computed here.
const attributionAlgorithm = "sampled_shapley_attribution"
