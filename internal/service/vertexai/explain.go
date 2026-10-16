package vertexai

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sort"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// Explain on a custom endpoint (#1454).
//
// Vertex Explainable AI's Sampled Shapley method needs nothing from a model
// but its predictions: for each of path_count random orderings of the input
// features, it moves the features one at a time from their baseline to the
// instance's value and credits each with the change in the output. So it is
// computed here by calling the container's predict route with the perturbed
// instances, as Vertex does for a custom container, and answered as
// ExplainResponse.
//
// On Vertex the explanation spec lives on the DeployedModel. A custom
// endpoint here is a Cloud Run service with no DeployedModel to carry one, so
// the spec is explanation_spec_override's: parameters.sampled_shapley_attribution
// and metadata.inputs, a feature name per input with its baselines. Without
// it Explain is FAILED_PRECONDITION, as Vertex answers for a model deployed
// without a spec. The other methods are refused: integrated gradients and
// XRAI need the model's gradients, which a predict route does not expose, and
// example-based explanations need an Example index, which nothing here builds.

// maxExplainPredictions bounds the perturbed instances one Explain sends.
const maxExplainPredictions = 10000

// explainRand is the source of the feature orderings. A fixed seed makes one
// request's attributions reproducible; Vertex's are random, and no client can
// rely on which orderings were drawn.
func explainRand() *rand.Rand { return rand.New(rand.NewPCG(0x5eed, 0x1454)) }

type explainSpec struct {
	pathCount     int
	topK          int
	outputIndices []int
	features      []string
	baselines     map[string][]*structpb.Value
}

func explainSpecOf(endpoint string, req *aiplatformpb.ExplainRequest) (*explainSpec, error) {
	o := req.GetExplanationSpecOverride()
	p := o.GetParameters()
	if p == nil {
		return nil, apierror.FailedPrecondition("endpoint %s has no explanation spec: a Cloud Run predictor has no DeployedModel to carry one, "+
			"so Explain takes it from explanation_spec_override (parameters.sampled_shapley_attribution and metadata.inputs); see docs/compatibility.md", endpoint)
	}
	if o.GetExamplesOverride() != nil || p.GetExamples() != nil {
		return nil, apierror.Unimplemented("example-based explanations are not served: they need an Example index, which nothing here builds")
	}
	if p.GetIntegratedGradientsAttribution() != nil || p.GetXraiAttribution() != nil {
		return nil, apierror.Unimplemented("integrated gradients and XRAI attributions are not served: they need the model's gradients, " +
			"which a custom container's predict route does not expose; sampled_shapley_attribution needs only predictions")
	}
	ss := p.GetSampledShapleyAttribution()
	if ss == nil {
		return nil, apierror.InvalidArgument("explanation_spec_override.parameters names no attribution method: set sampled_shapley_attribution")
	}
	spec := &explainSpec{pathCount: int(ss.GetPathCount()), topK: int(p.GetTopK()), baselines: map[string][]*structpb.Value{}}
	if spec.pathCount < 1 || spec.pathCount > 50 {
		return nil, apierror.InvalidArgument("sampled_shapley_attribution.path_count is %d; the valid range is [1, 50]", spec.pathCount)
	}
	if spec.topK < 0 {
		return nil, apierror.InvalidArgument("parameters.top_k is %d; it must not be negative", spec.topK)
	}
	for i, v := range p.GetOutputIndices().GetValues() {
		n, ok := v.GetKind().(*structpb.Value_NumberValue)
		if !ok || n.NumberValue < 0 || n.NumberValue != float64(int(n.NumberValue)) {
			return nil, apierror.InvalidArgument("parameters.output_indices[%d] is not a non-negative integer", i)
		}
		spec.outputIndices = append(spec.outputIndices, int(n.NumberValue))
	}
	if spec.topK > 0 && len(spec.outputIndices) > 0 {
		return nil, apierror.InvalidArgument("parameters.top_k and parameters.output_indices are exclusive")
	}
	inputs := o.GetMetadata().GetInputs()
	if len(inputs) == 0 {
		return nil, apierror.InvalidArgument("explanation_spec_override.metadata.inputs is empty: name each feature to attribute, " +
			"a key of a struct instance (or the one input of a non-struct instance)")
	}
	for name, in := range inputs {
		spec.features = append(spec.features, name)
		spec.baselines[name] = in.GetInputBaselines()
	}
	sort.Strings(spec.features)
	return spec, nil
}

// baselineCount is how many baselines are averaged over: every feature has
// none, one, or the same number n > 1.
func (s *explainSpec) baselineCount() (int, error) {
	n := 1
	for _, f := range s.features {
		if k := len(s.baselines[f]); k > 1 {
			if n > 1 && k != n {
				return 0, apierror.InvalidArgument("metadata.inputs[%q] has %d input_baselines and another input has %d: "+
					"each input has none, one, or the same number", f, k, n)
			}
			n = k
		}
	}
	return n, nil
}

// featureOf reads feature f of inst; withFeature returns inst with f set to
// v. A non-struct instance is its one feature.
func featureOf(inst *structpb.Value, f string, single bool) (*structpb.Value, bool) {
	if single && inst.GetStructValue() == nil {
		return inst, true
	}
	v, ok := inst.GetStructValue().GetFields()[f]
	return v, ok
}

func withFeature(inst *structpb.Value, f string, v *structpb.Value, single bool) *structpb.Value {
	if single && inst.GetStructValue() == nil {
		return v
	}
	out := proto.Clone(inst).(*structpb.Value)
	out.GetStructValue().Fields[f] = v
	return out
}

// baselineFor is feature f's b-th baseline; a number with none is 0.
func (s *explainSpec) baselineFor(f string, b int, actual *structpb.Value, i int) (*structpb.Value, error) {
	switch bs := s.baselines[f]; len(bs) {
	case 0:
		if _, ok := actual.GetKind().(*structpb.Value_NumberValue); ok {
			return structpb.NewNumberValue(0), nil
		}
		return nil, apierror.InvalidArgument("instances[%d] feature %q is not a number, so it needs metadata.inputs[%q].input_baselines: "+
			"only a number defaults to a baseline of 0", i, f, f)
	case 1:
		return bs[0], nil
	default:
		return bs[b], nil
	}
}

// explainPlan is the perturbed instances for one request instance: first the
// instance itself, then for each baseline its all-baseline instance and, per
// path, the instances after each feature in the path's order moved to its
// actual value.
type explainPlan struct {
	start     int
	baselines int
	orders    [][][]string // [baseline][path] feature order
}

// outputs reads a prediction as the vector of numbers it explains.
func outputs(p json.RawMessage, i int) ([]float64, bool, error) {
	var x float64
	if json.Unmarshal(p, &x) == nil {
		return []float64{x}, true, nil
	}
	var xs []float64
	if json.Unmarshal(p, &xs) == nil && len(xs) > 0 {
		return xs, false, nil
	}
	return nil, false, apierror.FailedPrecondition("the prediction for perturbed instance %d is %s, not a number or a list of numbers, "+
		"which is what Sampled Shapley attributes", i, p)
}

// explain serves Explain on a custom endpoint, for the gRPC and JSON routes.
func (s *Server) explain(ctx context.Context, t target, req *aiplatformpb.ExplainRequest) (*aiplatformpb.ExplainResponse, error) {
	if t.endpoint == "" {
		return nil, apierror.Unimplemented("Explain is served on custom endpoints (projects/{p}/locations/{l}/endpoints/{e}); " +
			"a publisher model carries no explanation spec")
	}
	if len(req.GetInstances()) == 0 {
		return nil, apierror.InvalidArgument("instances is empty: at least one instance is required")
	}
	spec, err := explainSpecOf(t.endpoint, req)
	if err != nil {
		return nil, err
	}
	nb, err := spec.baselineCount()
	if err != nil {
		return nil, err
	}
	single := len(spec.features) == 1
	rng := explainRand()
	var all []*structpb.Value
	plans := make([]explainPlan, len(req.GetInstances()))
	for i, inst := range req.GetInstances() {
		if inst.GetStructValue() == nil && !single {
			return nil, apierror.InvalidArgument("instances[%d] is not a struct, so metadata.inputs must name exactly one input; it names %d", i, len(spec.features))
		}
		actual := map[string]*structpb.Value{}
		for _, f := range spec.features {
			v, ok := featureOf(inst, f, single)
			if !ok {
				return nil, apierror.InvalidArgument("instances[%d] has no feature %q named in metadata.inputs", i, f)
			}
			actual[f] = v
		}
		plan := explainPlan{start: len(all), baselines: nb}
		all = append(all, inst)
		for b := range nb {
			cur := inst
			for _, f := range spec.features {
				bv, err := spec.baselineFor(f, b, actual[f], i)
				if err != nil {
					return nil, err
				}
				cur = withFeature(cur, f, bv, single)
			}
			base := cur
			all = append(all, base)
			var orders [][]string
			for range spec.pathCount {
				order := append([]string(nil), spec.features...)
				rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
				orders = append(orders, order)
				cur := base
				for _, f := range order {
					cur = withFeature(cur, f, actual[f], single)
					all = append(all, cur)
				}
			}
			plan.orders = append(plan.orders, orders)
		}
		plans[i] = plan
		if len(all) > maxExplainPredictions {
			return nil, apierror.InvalidArgument("the explanation needs more than %d predictions (instances × baselines × path_count × features); "+
				"lower path_count or send fewer instances", maxExplainPredictions)
		}
	}

	body, err := predictBody(all, req.GetParameters())
	if err != nil {
		return nil, err
	}
	out, err := s.endpointPredict(ctx, t.project, t.location, t.endpoint, body)
	if err != nil {
		return nil, err
	}
	var resp prediction.Response
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, apierror.Internal(err, "decoding endpoint %s's answer: %v", t.endpoint, err)
	}
	if id := req.GetDeployedModelId(); id != "" && id != resp.DeployedModelID {
		return nil, apierror.NotFound("endpoint %s has no deployed model %q: its container answers as %q", t.endpoint, id, resp.DeployedModelID)
	}
	vec := make([][]float64, len(resp.Predictions))
	scalar := make([]bool, len(resp.Predictions))
	for k, p := range resp.Predictions {
		if vec[k], scalar[k], err = outputs(p, k); err != nil {
			return nil, err
		}
	}

	result := &aiplatformpb.ExplainResponse{DeployedModelId: resp.DeployedModelID}
	for i, plan := range plans {
		inst := vec[plan.start]
		pv := &structpb.Value{}
		if err := protojson.Unmarshal(resp.Predictions[plan.start], pv); err != nil {
			return nil, apierror.Internal(err, "decoding prediction %d: %v", i, err)
		}
		result.Predictions = append(result.Predictions, pv)
		idx, err := spec.explained(inst, scalar[plan.start], i)
		if err != nil {
			return nil, err
		}
		exp := &aiplatformpb.Explanation{}
		for _, k := range idx {
			attr := map[string]float64{}
			var base float64
			pos := plan.start + 1
			for b := range plan.baselines {
				baseOut := vec[pos]
				if len(baseOut) != len(inst) {
					return nil, apierror.FailedPrecondition("endpoint %s answered %d outputs for one instance and %d for another", t.endpoint, len(inst), len(baseOut))
				}
				base += baseOut[k]
				basePos := pos
				pos++
				for _, order := range plan.orders[b] {
					prev := vec[basePos][k]
					for _, f := range order {
						if len(vec[pos]) != len(inst) {
							return nil, apierror.FailedPrecondition("endpoint %s answered %d outputs for one instance and %d for another", t.endpoint, len(inst), len(vec[pos]))
						}
						attr[f] += vec[pos][k] - prev
						prev = vec[pos][k]
						pos++
					}
				}
			}
			n := float64(plan.baselines * spec.pathCount)
			fields := map[string]*structpb.Value{}
			for f, a := range attr {
				fields[f] = structpb.NewNumberValue(a / n)
			}
			a := &aiplatformpb.Attribution{
				BaselineOutputValue: base / float64(plan.baselines),
				InstanceOutputValue: inst[k],
				FeatureAttributions: structpb.NewStructValue(&structpb.Struct{Fields: fields}),
			}
			if !scalar[plan.start] {
				a.OutputIndex = []int32{int32(k)}
			}
			exp.Attributions = append(exp.Attributions, a)
		}
		result.Explanations = append(result.Explanations, exp)
	}
	return result, nil
}

// explained is the output indices to attribute for an instance whose
// prediction is out: output_indices, or the top_k largest (default 1).
func (s *explainSpec) explained(out []float64, scalar bool, i int) ([]int, error) {
	if scalar {
		return []int{0}, nil
	}
	if len(s.outputIndices) > 0 {
		for _, k := range s.outputIndices {
			if k >= len(out) {
				return nil, apierror.InvalidArgument("parameters.output_indices names output %d, and instances[%d]'s prediction has %d", k, i, len(out))
			}
		}
		return s.outputIndices, nil
	}
	k := s.topK
	if k == 0 {
		k = 1
	}
	idx := make([]int, len(out))
	for j := range idx {
		idx[j] = j
	}
	sort.SliceStable(idx, func(a, b int) bool { return out[idx[a]] > out[idx[b]] })
	if k > len(idx) {
		k = len(idx)
	}
	return idx[:k], nil
}

// Explain serves Sampled Shapley attributions on a custom endpoint.
func (p *predictionService) Explain(ctx context.Context, req *aiplatformpb.ExplainRequest) (*aiplatformpb.ExplainResponse, error) {
	t, err := parseTarget("endpoint", req.GetEndpoint())
	if err != nil {
		return nil, grpcError(err)
	}
	resp, err := p.s.explain(ctx, t, req)
	return resp, grpcError(err)
}

// explainREST serves POST /v1/projects/{p}/locations/{l}/endpoints/{e}:explain.
func (s *Server) explainREST(w http.ResponseWriter, r *http.Request, t target, body []byte) {
	req := &aiplatformpb.ExplainRequest{}
	if err := protojson.Unmarshal(body, req); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("the request is not an ExplainRequest: %v", err))
		return
	}
	resp, err := s.explain(r.Context(), t, req)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		apierror.WriteJSON(w, apierror.Internal(err, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
