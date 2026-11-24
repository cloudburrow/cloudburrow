package vertexai

// Google Cloud Pipeline Components (#2210): a pipeline task whose executor
// is one of GCPC's launchers (the image gcr.io/ml-pipeline/google-cloud-pipeline-components
// running python3 -u -m google_cloud_pipeline_components.container.v1.X.launcher)
// is run by this server itself, as the launcher's code does it, against the
// instance's own services: the launcher's container only calls Google APIs
// (aiplatform.googleapis.com, bigquery's REST API) and writes gcp_resources
// and its executor output, so nothing of Google's image is needed, and
// nothing is pulled from gcr.io.
//
// Each launcher follows google-cloud-pipeline-components 2.23.0's
// container/v1 code (read 2026-11-24, the wheel's own source):
//   - custom_job (type CustomJob): the payload, empty fields removed as
//     json_util.recursive_remove_empty removes them and max_wait_duration
//     dropped unless the strategy is FLEX_START, is a CustomJob created in
//     the project and location, then waited for; gcp_resources names it
//     ({"resources": [{"resourceType": "CustomJob", "resourceUri":
//     "https://LOCATION-aiplatform.googleapis.com/v1/NAME"}]});
//   - batch_prediction_job (type BatchPredictionJob): the payload with the
//     unmanaged_container_model input artifact's metadata and URI, bq://
//     added to BigQuery URIs, is a BatchPredictionJob; the outputs are the
//     google.VertexBatchPredictionJob artifact batchpredictionjob, the
//     gcs_output_directory artifact and the google.BQTable
//     bigquery_output_table;
//   - bigquery.* (type BigqueryQueryJob, BigqueryCreateModelJob, the
//     BigQuery ML ones): the payload (a BigQuery Job) with the query
//     override, its query built as each launcher builds it, is inserted
//     with jobs.insert on the instance's BigQuery and polled; gcp_resources
//     names it (resourceType BigQueryJob, its selfLink); the outputs are
//     the google.BQTable destination_table, the google.BQMLModel model,
//     or an artifact whose metadata is the query's schema and rows.
// A launcher this instance has no service for (dataflow, dataproc, the
// AutoML, model, endpoint and hyperparameter tuning launchers) fails the
// task naming it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// PipelineBigQuery is a PipelineRunner that can also call the instance's
// BigQuery REST API (as the run's identity): path is from /bigquery/v2/.
type PipelineBigQuery interface {
	BigQueryREST(ctx context.Context, method, path string, body []byte) (int, []byte, error)
}

// gcpcImage is Google Cloud Pipeline Components' launcher image.
const gcpcImage = "gcr.io/ml-pipeline/google-cloud-pipeline-components"

var gcpcModuleRE = regexp.MustCompile(`^google_cloud_pipeline_components\.container\.(v1|preview|_implementation)\.([a-z0-9_.]+)$`)

// gcpcLauncher is the launcher module a GCPC container runs ("custom_job",
// "bigquery.query_job" ...), or "".
func gcpcLauncher(image string, cmd []string) string {
	if !strings.HasPrefix(image, gcpcImage+":") && !strings.HasPrefix(image, gcpcImage+"@") && image != gcpcImage {
		return ""
	}
	for i, c := range cmd {
		if c == "-m" && i+1 < len(cmd) {
			if m := gcpcModuleRE.FindStringSubmatch(cmd[i+1]); m != nil {
				return strings.TrimSuffix(strings.TrimSuffix(m[2], ".launcher"), ".dataflow_launcher")
			}
		}
	}
	return ""
}

// gcpcArgs reads a launcher's --flag value arguments.
func gcpcArgs(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !ok && i+1 < len(args) {
			v = args[i+1]
			i++
		}
		out[k] = v
	}
	return out
}

// removeEmpty is json_util.recursive_remove_empty: a dict's falsy values
// (None, "", 0, false, {}, []) are dropped, a list's empty containers,
// until nothing changes.
func removeEmpty(v any) any {
	for {
		next := removeEmptyOnce(v)
		a, _ := json.Marshal(next)
		b, _ := json.Marshal(v)
		if string(a) == string(b) {
			return next
		}
		v = next
	}
}

func removeEmptyOnce(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, el := range x {
			if truthy(el) {
				out[k] = removeEmptyOnce(el)
			}
		}
		return out
	case []any:
		out := []any{}
		for _, el := range x {
			switch el.(type) {
			case string, float64, bool:
				out = append(out, el)
			default:
				if truthy(removeEmptyOnce(el)) {
					out = append(out, removeEmptyOnce(el))
				}
			}
		}
		return out
	}
	return v
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case float64:
		return x != 0
	case bool:
		return x
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

// gcpcRun is one launcher invocation.
type gcpcRun struct {
	r         *pipelineRun
	module    string
	args      map[string]string
	outDir    string
	execInput map[string]any
	ins       nodeIn
}

// local is the host path of a /cb-outputs path the launcher writes.
func (g *gcpcRun) local(p string) string {
	return filepath.Join(g.outDir, filepath.FromSlash(strings.TrimPrefix(p, "/cb-outputs/")))
}

func (g *gcpcRun) writeResources(kind, uri string) error {
	p := g.args["gcp_resources"]
	if p == "" {
		return fmt.Errorf("the launcher has no --gcp_resources")
	}
	b, _ := json.MarshalIndent(map[string]any{"resources": []any{map[string]any{"resourceType": kind, "resourceUri": uri}}}, "", "  ")
	return os.WriteFile(g.local(p), b, 0o666)
}

// gcpcArtifact is an output artifact a launcher sets.
type gcpcArtifact struct {
	name, uri string
	metadata  map[string]any
}

// writeOutputs is artifact_utils.update_output_artifacts: the executor
// output names each artifact's URI and metadata.
func (g *gcpcRun) writeOutputs(arts ...gcpcArtifact) error {
	out := map[string]any{}
	outs, _ := g.execInput["outputs"].(map[string]any)
	declared, _ := outs["artifacts"].(map[string]any)
	for _, a := range arts {
		d, ok := declared[a.name].(map[string]any)
		if !ok {
			continue
		}
		list, _ := d["artifacts"].([]any)
		if len(list) == 0 {
			continue
		}
		rt, _ := list[0].(map[string]any)
		cp := map[string]any{}
		for k, v := range rt {
			cp[k] = v
		}
		cp["uri"], cp["metadata"] = a.uri, a.metadata
		if a.metadata == nil {
			cp["metadata"] = map[string]any{}
		}
		out[a.name] = map[string]any{"artifacts": []any{cp}}
	}
	b, _ := json.Marshal(map[string]any{"artifacts": out})
	return os.WriteFile(filepath.Join(g.outDir, "executor_output.json"), b, 0o666)
}

// runGCPC runs a GCPC launcher natively.
func (r *pipelineRun) runGCPC(ctx context.Context, module string, args []string, outDir string, execInput map[string]any, ins nodeIn) error {
	g := &gcpcRun{r: r, module: module, args: gcpcArgs(args), outDir: outDir, execInput: execInput, ins: ins}
	project, location := g.args["project"], g.args["location"]
	if project == "" {
		project = r.project
	}
	if location == "" {
		_, location, _ = parseParent("parent", r.parent)
	}
	switch {
	case module == "custom_job":
		return g.customJob(ctx, project, location)
	case module == "batch_prediction_job":
		return g.batchPrediction(ctx, project, location)
	case strings.HasPrefix(module, "bigquery."):
		return g.bigquery(ctx, project, location, strings.TrimPrefix(module, "bigquery."))
	case strings.HasPrefix(module, "dataflow"):
		return fmt.Errorf("Google Cloud Pipeline Components' %s launcher runs a Dataflow job, and Dataflow is not served on this instance", module)
	}
	return fmt.Errorf("Google Cloud Pipeline Components' %s launcher is not run on this instance: CustomJob, BatchPredictionJob and the BigQuery launchers are", module)
}

func (g *gcpcRun) payload() (map[string]any, error) {
	var p map[string]any
	if err := json.Unmarshal([]byte(g.args["payload"]), &p); err != nil {
		return nil, fmt.Errorf("the launcher's --payload is not JSON: %v", err)
	}
	return p, nil
}

func vertexURI(location, name string) string {
	return "https://" + location + "-aiplatform.googleapis.com/v1/" + name
}

// jobStateError is the launcher's error for a job that did not succeed.
func jobStateError(name string, state aiplatformpb.JobState, code int32, msg string) error {
	kind := "error"
	switch code {
	case 3, 5, 7, 9, 11:
		kind = "value error in error"
	}
	return fmt.Errorf("Job failed with %s state: JobState.%s. (%s: %s)", kind, state, name, msg)
}

func (g *gcpcRun) customJob(ctx context.Context, project, location string) error {
	p, err := g.payload()
	if err != nil {
		return err
	}
	if js, ok := p["job_spec"].(map[string]any); ok {
		if sc, ok := js["scheduling"].(map[string]any); ok && sc["strategy"] != "FLEX_START" {
			delete(sc, "max_wait_duration")
		}
	}
	b, _ := json.Marshal(removeEmpty(p))
	job := &aiplatformpb.CustomJob{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, job); err != nil {
		return fmt.Errorf("the CustomJob payload: %v", err)
	}
	created, err := g.r.s.createCustomJob("projects/"+project+"/locations/"+location, job, nil)
	if err != nil {
		return apierror.From(err)
	}
	if err := g.writeResources("CustomJob", vertexURI(location, created.GetName())); err != nil {
		return err
	}
	done, err := g.r.s.waitCustomJob(ctx, created.GetName())
	if err != nil {
		return err
	}
	if done.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
		return jobStateError(done.GetName(), done.GetState(), done.GetError().GetCode(), done.GetError().GetMessage())
	}
	return nil
}

var bqDatasetRE = regexp.MustCompile(`^(?:(?:bq|bigquery)://)?([a-z0-9.:-]+)[:.]([A-Za-z0-9_]+)$`)

func (g *gcpcRun) batchPrediction(ctx context.Context, project, location string) error {
	p, err := g.payload()
	if err != nil {
		return err
	}
	if list := g.ins.arts["unmanaged_container_model"]; len(list) > 0 {
		m := camelToSnakeKeys(list[0].Metadata)
		mm, _ := m.(map[string]any)
		if mm == nil {
			mm = map[string]any{}
		}
		mm["artifact_uri"] = list[0].URI
		p["unmanaged_container_model"] = mm
	}
	p = removeEmpty(p).(map[string]any)
	addBQ := func(section, field, key string) {
		s, _ := p[section].(map[string]any)
		f, _ := s[field].(map[string]any)
		if u, ok := f[key].(string); ok && u != "" && !strings.Contains(u, "://") {
			f[key] = "bq://" + u
		}
	}
	addBQ("input_config", "bigquery_source", "input_uri")
	addBQ("output_config", "bigquery_destination", "output_uri")
	b, _ := json.Marshal(p)
	job := &aiplatformpb.BatchPredictionJob{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, job); err != nil {
		return fmt.Errorf("the BatchPredictionJob payload: %v", err)
	}
	created, err := g.r.s.createBatchJob("projects/"+project+"/locations/"+location, job)
	if err != nil {
		return apierror.From(err)
	}
	if err := g.writeResources("BatchPredictionJob", vertexURI(location, created.GetName())); err != nil {
		return err
	}
	done, err := g.r.s.waitBatchJob(ctx, created.GetName())
	if err != nil {
		return err
	}
	if done.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
		return jobStateError(done.GetName(), done.GetState(), done.GetError().GetCode(), done.GetError().GetMessage())
	}
	info := done.GetOutputInfo()
	arts := []gcpcArtifact{{name: "batchpredictionjob", uri: vertexURI(location, done.GetName()), metadata: map[string]any{
		"resourceName": done.GetName(), "bigqueryOutputTable": info.GetBigqueryOutputTable(),
		"bigqueryOutputDataset": info.GetBigqueryOutputDataset(), "gcsOutputDirectory": info.GetGcsOutputDirectory()}}}
	if ds := info.GetBigqueryOutputDataset(); ds != "" {
		m := bqDatasetRE.FindStringSubmatch(ds)
		if m == nil {
			return fmt.Errorf("Invalid BQ dataset address from batch prediction output: %s. Expect: bq://[project_id].[dataset_id].", ds)
		}
		arts = append(arts, bqTable("bigquery_output_table", m[1], m[2], info.GetBigqueryOutputTable()))
	}
	if d := info.GetGcsOutputDirectory(); d != "" {
		arts = append(arts, gcpcArtifact{name: "gcs_output_directory", uri: d, metadata: map[string]any{}})
	}
	return g.writeOutputs(arts...)
}

func camelToSnakeKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, el := range x {
			out[camelToSnake(k)] = camelToSnakeKeys(el)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = camelToSnakeKeys(el)
		}
		return out
	}
	return v
}

func bqTable(name, project, dataset, table string) gcpcArtifact {
	return gcpcArtifact{name: name, uri: fmt.Sprintf("https://www.googleapis.com/bigquery/v2/projects/%s/datasets/%s/tables/%s", project, dataset, table),
		metadata: map[string]any{"projectId": project, "datasetId": dataset, "tableId": table}}
}

func bqModel(name, project, dataset, model string) gcpcArtifact {
	return gcpcArtifact{name: name, uri: fmt.Sprintf("https://www.googleapis.com/bigquery/v2/projects/%s/datasets/%s/models/%s", project, dataset, model),
		metadata: map[string]any{"projectId": project, "datasetId": dataset, "modelId": model}}
}

// backQuoted is bigquery_util.back_quoted_if_needed.
func backQuoted(s string) string {
	if s == "" || strings.HasPrefix(s, "`") {
		return s
	}
	return "`" + s + "`"
}

// pyNum is a float as Python's %s writes it.
func pyNum(f float64) string {
	if f == float64(int64(f)) && f < 1e16 && f > -1e16 {
		return strconv.FormatInt(int64(f), 10) + ".0"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// pyBool is a launcher's bool argument as Python's %s writes it.
func pyBool(s string) string {
	if s != "" && !strings.EqualFold(s, "false") {
		return "True"
	}
	return "False"
}

// optFloat is a launcher's optional float argument.
func optFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// bqMetricsArtifacts are the launchers whose output is an artifact of the
// query's schema and rows, by type: the artifact's name.
var bqMetricsArtifacts = map[string]string{
	"BigqueryEvaluateModelJob": "evaluation_metrics", "BigqueryMLFeatureImportanceJob": "feature_importance",
	"BigqueryMLWeightsJob": "weights", "BigqueryMLAdvancedWeightsJob": "advanced_weights",
	"BigqueryMLTrainingInfoJob": "ml_training_info", "BigqueryMLTrialInfoJob": "trial_info", "BigqueryMLFeatureInfoJob": "feature_info",
	"BigqueryMLCentroidsJob": "centroids", "BigqueryMLArimaCoefficientsJob": "arima_coefficients",
	"BigqueryMLArimaEvaluateJob": "arima_evaluation_metrics",
}

// bqQuery is the query a BigQuery launcher runs ("" for the payload's
// own), and the name of its destination table artifact.
func bqQuery(kind string, a map[string]string) (string, string, error) {
	model, table, stmt := a["model_name"], a["table_name"], a["query_statement"]
	oneOf := func(what string) error {
		if (stmt == "") == (table == "") {
			return fmt.Errorf("One and only one of query_statement and table_name should be populated for BigQuery %s job.", what)
		}
		return nil
	}
	input := func(lead string) string {
		if table != "" {
			return lead + "TABLE " + backQuoted(table)
		}
		if stmt != "" {
			return lead + "(" + stmt + ")"
		}
		return ""
	}
	threshold := func(lead string) string {
		if f, ok := optFloat(a["threshold"]); ok && f > 0 && f < 1 {
			return lead + "STRUCT(" + pyNum(f) + " AS threshold)"
		}
		return ""
	}
	settings := func(fields ...[2]string) string {
		var l []string
		for _, f := range fields {
			if f[1] != "" {
				l = append(l, f[1]+" AS "+f[0])
			}
		}
		return strings.Join(l, ",")
	}
	horizonConf := func() string {
		var h, c string
		if n, err := strconv.Atoi(strings.TrimSpace(a["horizon"])); err == nil && n > 0 {
			h = strconv.Itoa(n)
		}
		if f, ok := optFloat(a["confidence_level"]); ok && f >= 0 && f < 1 {
			c = pyNum(f)
		}
		return ", STRUCT(" + settings([2]string{"horizon", h}, [2]string{"confidence_level", c}) + ")"
	}
	switch kind {
	case "BigqueryQueryJob", "BigqueryCreateModelJob":
		return "", "destination_table", nil
	case "BigqueryPredictModelJob":
		if err := oneOf("predict model"); err != nil {
			return "", "", err
		}
		return fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s`, %s%s)", model, input(""), threshold(", ")), "destination_table", nil
	case "BigqueryEvaluateModelJob":
		if stmt != "" && table != "" {
			return "", "", fmt.Errorf("One and only one of query_statement and table_name should be populated for BigQuery evaluation model job.")
		}
		return fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL %s%s%s)", backQuoted(model), input(", "), threshold(", ")), "", nil
	case "BigqueryDropModelJob":
		return "DROP MODEL " + backQuoted(model), "", nil
	case "BigqueryMLFeatureImportanceJob":
		return fmt.Sprintf("SELECT * FROM ML.FEATURE_IMPORTANCE(MODEL %s)", backQuoted(model)), "", nil
	case "BigqueryMLWeightsJob", "BigqueryMLAdvancedWeightsJob", "BigqueryMLTrainingInfoJob", "BigqueryMLTrialInfoJob",
		"BigqueryMLFeatureInfoJob", "BigqueryMLArimaCoefficientsJob", "BigqueryMLPrincipalComponentsJob", "BigqueryMLPrincipalComponentInfoJob":
		fn := map[string]string{"BigqueryMLWeightsJob": "WEIGHTS", "BigqueryMLAdvancedWeightsJob": "ADVANCED_WEIGHTS",
			"BigqueryMLTrainingInfoJob": "TRAINING_INFO", "BigqueryMLTrialInfoJob": "TRIAL_INFO", "BigqueryMLFeatureInfoJob": "FEATURE_INFO",
			"BigqueryMLArimaCoefficientsJob": "ARIMA_COEFFICIENTS", "BigqueryMLPrincipalComponentsJob": "PRINCIPAL_COMPONENTS",
			"BigqueryMLPrincipalComponentInfoJob": "PRINCIPAL_COMPONENT_INFO"}[kind]
		return fmt.Sprintf("SELECT * FROM ML.%s(MODEL %s)", fn, backQuoted(model)), "destination_table", nil
	case "BigqueryMLCentroidsJob":
		return fmt.Sprintf("SELECT * FROM ML.CENTROIDS(MODEL %s, STRUCT(%s AS standardize))", backQuoted(model), pyBool(a["standardize"])), "", nil
	case "BigqueryMLArimaEvaluateJob":
		extra := ""
		if a["show_all_candidate_models"] != "" && !strings.EqualFold(a["show_all_candidate_models"], "false") {
			extra = ", STRUCT(True AS show_all_candidate_models)"
		}
		return fmt.Sprintf("SELECT * FROM ML.ARIMA_EVALUATE(MODEL %s%s)", backQuoted(model), extra), "", nil
	case "BigqueryMLConfusionMatrixJob":
		if err := oneOf("confusion matrix"); err != nil {
			return "", "", err
		}
		return fmt.Sprintf("SELECT * FROM ML.CONFUSION_MATRIX(MODEL %s%s%s)", backQuoted(model), input(", "), threshold(", ")), "confusion_matrix", nil
	case "BigqueryMLRocCurveJob":
		if err := oneOf("roc curve"); err != nil {
			return "", "", err
		}
		th := ""
		if a["thresholds"] != "" {
			th = ", GENERATE_ARRAY(" + a["thresholds"] + ")"
		}
		return fmt.Sprintf("SELECT * FROM ML.ROC_CURVE(MODEL %s%s%s)", backQuoted(model), input(", "), th), "roc_curve", nil
	case "BigqueryMLForecastJob":
		return fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL %s %s)", backQuoted(model), horizonConf()), "destination_table", nil
	case "BigqueryExplainForecastModelJob":
		return fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL %s %s)", backQuoted(model), horizonConf()), "destination_table", nil
	case "BigqueryExplainPredictModelJob":
		if err := oneOf("explain predict model"); err != nil {
			return "", "", err
		}
		var k, t, n string
		if v, err := strconv.Atoi(strings.TrimSpace(a["top_k_features"])); err == nil && v > 0 {
			k = strconv.Itoa(v)
		}
		if f, ok := optFloat(a["threshold"]); ok && f > 0 && f < 1 {
			t = pyNum(f)
		}
		if v, err := strconv.Atoi(strings.TrimSpace(a["num_integral_steps"])); err == nil && v > 0 {
			n = strconv.Itoa(v)
		}
		return fmt.Sprintf("SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s`, %s, STRUCT(%s))", model, input(""),
			settings([2]string{"top_k_features", k}, [2]string{"threshold", t}, [2]string{"num_integral_steps", n})), "destination_table", nil
	case "BigqueryDetectAnomaliesModelJob":
		var c, p string
		if f, ok := optFloat(a["contamination"]); ok && f >= 0 && f <= 0.5 {
			c = pyNum(f)
		}
		if f, ok := optFloat(a["anomaly_prob_threshold"]); ok && f > 0 && f < 1 {
			p = pyNum(f)
		}
		st := settings([2]string{"contamination", c}, [2]string{"anomaly_prob_threshold", p})
		if stmt != "" || table != "" {
			return fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s`,  STRUCT(%s), %s)", model, st, input("")), "destination_table", nil
		}
		return fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s`,  STRUCT(%s))", model, st), "destination_table", nil
	case "BigqueryMLRecommendJob", "BigqueryMLReconstructionLossJob":
		if stmt != "" && table != "" {
			return "", "", fmt.Errorf("One and only one of query_statement and table_name should be populated for BigQuery ML job.")
		}
		fn := map[string]string{"BigqueryMLRecommendJob": "RECOMMEND", "BigqueryMLReconstructionLossJob": "RECONSTRUCTION_LOSS"}[kind]
		return fmt.Sprintf("SELECT * FROM ML.%s(MODEL %s%s)", fn, backQuoted(model), input(", ")), "destination_table", nil
	case "BigqueryMLGlobalExplainJob":
		return fmt.Sprintf("SELECT * FROM ML.GLOBAL_EXPLAIN(MODEL %s, STRUCT(TRUE AS class_level_explain))", backQuoted(model)), "destination_table", nil
	}
	return "", "", fmt.Errorf("the BigQuery launcher type %s is not run on this instance", kind)
}

func (g *gcpcRun) bigquery(ctx context.Context, project, location, launcher string) error {
	bq, ok := g.r.runner.(PipelineBigQuery)
	if !ok {
		return fmt.Errorf("the %s launcher runs a BigQuery job, and BigQuery is not served on this instance (start with --services including bigquery)", launcher)
	}
	kind := g.args["type"]
	var sql, tableArt string
	var err error
	if launcher == "export_model" {
		// bigquery_export_model_job: the model must be there, then EXPORT
		// MODEL as GCPC writes it (#2210).
		if sql, err = g.exportModelQuery(ctx, bq); err != nil {
			return err
		}
		kind = "BigqueryExportModelJob"
	} else if sql, tableArt, err = bqQuery(kind, g.args); err != nil {
		return err
	}
	var job map[string]any
	if p := g.args["payload"]; p != "" {
		if err := json.Unmarshal([]byte(p), &job); err != nil {
			return fmt.Errorf("the BigQuery job payload is not JSON: %v", err)
		}
	}
	if job == nil {
		job = map[string]any{}
	}
	var override map[string]any
	if o := g.args["job_configuration_query_override"]; o != "" {
		if err := json.Unmarshal([]byte(o), &override); err != nil {
			return fmt.Errorf("the job_configuration_query_override is not JSON: %v", err)
		}
	}
	if override == nil {
		override = map[string]any{}
	}
	if sql != "" {
		override["query"] = sql
	}
	override, _ = removeEmpty(override).(map[string]any)
	conf, _ := job["configuration"].(map[string]any)
	if conf == nil {
		conf = map[string]any{}
		job["configuration"] = conf
	}
	q, _ := conf["query"].(map[string]any)
	if q == nil {
		q = map[string]any{}
		conf["query"] = q
	}
	for k, v := range override {
		q[k] = v
	}
	job, _ = removeEmpty(job).(map[string]any)
	q, _ = job["configuration"].(map[string]any)["query"].(map[string]any)
	if q["useLegacySql"] == true {
		return fmt.Errorf("Legacy SQL is not supported. Use standard SQL instead.")
	}
	q["useLegacySql"] = false
	if location != "" {
		ref, _ := job["jobReference"].(map[string]any)
		if ref == nil {
			ref = map[string]any{}
			job["jobReference"] = ref
		}
		ref["location"] = location
	}
	body, _ := json.Marshal(job)
	code, out, err := bq.BigQueryREST(ctx, "POST", "/bigquery/v2/projects/"+url.PathEscape(project)+"/jobs", body)
	if err != nil {
		return err
	}
	var created map[string]any
	_ = json.Unmarshal(out, &created)
	self, _ := created["selfLink"].(string)
	ref, _ := created["jobReference"].(map[string]any)
	jobID, _ := ref["jobId"].(string)
	if code/100 != 2 || jobID == "" {
		return fmt.Errorf("BigQquery Job failed. Cannot retrieve the job name. Request:%s; Response: %s.", body, strings.TrimSpace(string(out)))
	}
	jobLoc, _ := ref["location"].(string)
	// The selfLink as Google writes it, which is what gcp_resources names.
	self = "https://www.googleapis.com/bigquery/v2/projects/" + project + "/jobs/" + jobID + "?location=" + jobLoc
	if err := g.writeResources("BigQueryJob", self); err != nil {
		return err
	}
	getPath := "/bigquery/v2/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(jobID) + "?location=" + url.QueryEscape(jobLoc)
	var got struct {
		ID     string `json:"id"`
		Status struct {
			State       string         `json:"state"`
			ErrorResult map[string]any `json:"errorResult"`
		} `json:"status"`
		Configuration struct {
			Query struct {
				DestinationTable *struct {
					ProjectID string `json:"projectId"`
					DatasetID string `json:"datasetId"`
					TableID   string `json:"tableId"`
				} `json:"destinationTable"`
			} `json:"query"`
		} `json:"configuration"`
		Statistics struct {
			Query struct {
				StatementType  string `json:"statementType"`
				DDLTargetTable *struct {
					ProjectID string `json:"projectId"`
					DatasetID string `json:"datasetId"`
					TableID   string `json:"tableId"`
				} `json:"ddlTargetTable"`
			} `json:"query"`
		} `json:"statistics"`
	}
	for {
		code, out, err := bq.BigQueryREST(ctx, "GET", getPath, nil)
		if err != nil {
			return err
		}
		if code/100 == 2 {
			_ = json.Unmarshal(out, &got)
			if got.Status.ErrorResult != nil {
				st, _ := json.Marshal(map[string]any{"state": got.Status.State, "errorResult": got.Status.ErrorResult})
				return fmt.Errorf("The BigQuery job %s failed. Error: %s", self, st)
			}
			if strings.EqualFold(got.Status.State, "DONE") {
				break
			}
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			_, _, _ = bq.BigQueryREST(context.Background(), "POST", "/bigquery/v2/projects/"+url.PathEscape(project)+"/jobs/"+url.PathEscape(jobID)+"/cancel?location="+url.QueryEscape(jobLoc), nil)
			return ctx.Err()
		}
	}
	switch kind {
	case "BigqueryCreateModelJob":
		t := got.Statistics.Query.DDLTargetTable
		if got.Statistics.Query.StatementType != "CREATE_MODEL" || t == nil {
			return fmt.Errorf("Unexpected create model result: statement type %q", got.Statistics.Query.StatementType)
		}
		return g.writeOutputs(bqModel("model", t.ProjectID, t.DatasetID, t.TableID))
	case "BigqueryDropModelJob":
		return nil
	case "BigqueryExportModelJob":
		p := g.args["exported_model_path"]
		if p == "" {
			return fmt.Errorf("the launcher has no --exported_model_path")
		}
		return os.WriteFile(g.local(p), []byte(g.args["model_destination_path"]), 0o666)
	}
	if name, ok := bqMetricsArtifacts[kind]; ok {
		code, out, err := bq.BigQueryREST(ctx, "GET", "/bigquery/v2/projects/"+url.PathEscape(project)+"/queries/"+url.PathEscape(jobID)+"?location="+url.QueryEscape(jobLoc), nil)
		if err != nil {
			return err
		}
		if code/100 != 2 {
			return fmt.Errorf("getting the query results of %s: %s", self, strings.TrimSpace(string(out)))
		}
		var res map[string]any
		_ = json.Unmarshal(out, &res)
		return g.writeOutputs(gcpcArtifact{name: name, uri: "", metadata: map[string]any{"schema": res["schema"], "rows": res["rows"]}})
	}
	if t := got.Configuration.Query.DestinationTable; t != nil && tableArt != "" {
		return g.writeOutputs(bqTable(tableArt, t.ProjectID, t.DatasetID, t.TableID))
	}
	return nil
}

// exportModelQuery is bigquery_export_model_job's query: model_name must
// be projectId.datasetId.modelId and name a model, and the statement is
// EXPORT MODEL `name` OPTIONS(URI="<model_destination_path>",
// add_serving_default_signature=True), as GCPC 2.23.0's remote runner
// writes it.
func (g *gcpcRun) exportModelQuery(ctx context.Context, bq PipelineBigQuery) (string, error) {
	name := g.args["model_name"]
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf(`The model name must be in the format "projectId.datasetId.modelId"`)
	}
	code, out, err := bq.BigQueryREST(ctx, "GET", "/bigquery/v2/projects/"+url.PathEscape(parts[0])+"/datasets/"+url.PathEscape(parts[1])+"/models/"+url.PathEscape(parts[2]), nil)
	if err != nil {
		return "", err
	}
	var model map[string]any
	_ = json.Unmarshal(out, &model)
	if _, ok := model["modelType"]; code/100 != 2 || !ok {
		return "", fmt.Errorf(`Cannot get model resource. The model name must be in the format "projectId.datasetId.modelId" `)
	}
	return fmt.Sprintf(`EXPORT MODEL %s OPTIONS(URI="%s",add_serving_default_signature=True)`, backQuoted(name), g.args["model_destination_path"]), nil
}

// resolveRunPlaceholders replaces the pipeline-level placeholders GCPC
// components take as defaults ({{$.pipeline_google_cloud_project_id}},
// {{$.pipeline_google_cloud_location}}, {{$.pipeline_persistent_resource_id}})
// in a task's parameter values, as Vertex AI Pipelines resolves them.
func (r *pipelineRun) resolveRunPlaceholders(s string) string {
	if !strings.Contains(s, "{{$.pipeline_") {
		return s
	}
	_, location, _ := parseParent("parent", r.parent)
	return strings.NewReplacer("{{$.pipeline_google_cloud_project_id}}", r.project,
		"{{$.pipeline_google_cloud_location}}", location,
		"{{$.pipeline_persistent_resource_id}}", "").Replace(s)
}

var _ = path.Base
