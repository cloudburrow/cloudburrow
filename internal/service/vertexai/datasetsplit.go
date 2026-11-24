package vertexai

// A TrainingPipeline's input_data_config (#2210): the managed dataset's
// data, split into training, validation and test sets and exported where
// gcs_destination or bigquery_destination says, and the job's containers
// told where by AIP_DATA_FORMAT, AIP_TRAINING_DATA_URI,
// AIP_VALIDATION_DATA_URI and AIP_TEST_DATA_URI.
//
// Documented behaviour (the InputDataConfig proto, aiplatform v1.127.0,
// read 2026-11-24):
//   - the dataset is in the pipeline's project and location;
//   - with no split, fraction_split's default: roughly 80% training, 10%
//     validation, 10% test; fractions summing to less than 1 leave the
//     remainder "as decided by Vertex AI" (here: training);
//   - filter_split is for datasets with data items (ListDataItems
//     filters, "-" matching nothing, an item going to the first set whose
//     filter matches); predefined_split, timestamp_split and
//     stratified_split are for tabular datasets: predefined by a column
//     whose value is training, validation or test (other rows left out);
//     timestamp the youngest rows to training, the next to validation and
//     the oldest to test (rows whose key is not an RFC 3339 time left
//     out); stratified each value of a column in the fractions (beyond the
//     top 500 values, or under three rows, assigned at random);
//   - gcs_destination: a directory
//     dataset-<dataset-id>-<annotation-type>-<YYYY-MM-DDThh:mm:ss.sssZ>
//     with training-*, validation-* and test-* files, jsonl for
//     non-tabular data and csv for tabular;
//   - bigquery_destination (tabular datasets with a BigQuery source only):
//     a new dataset dataset_<dataset-id>_<annotation-type>_<YYYY_MM_DDThh_mm_ss_sssZ>
//     in the destination's project, its tables training, validation and
//     test; AIP_DATA_FORMAT bigquery.
//
// UNVERIFIED (re-read 2026-11-24; not documented): the shard names (-00000-of-00001), the
// CSV files' header row, and <annotation-type> (datasets.go).

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// dataEnv is where a training pipeline's data went.
type dataEnv struct {
	format               string
	training, validation string
	test                 string
}

func (e dataEnv) vars() []*aiplatformpb.EnvVar {
	return []*aiplatformpb.EnvVar{{Name: "AIP_DATA_FORMAT", Value: e.format}, {Name: "AIP_TRAINING_DATA_URI", Value: e.training},
		{Name: "AIP_VALIDATION_DATA_URI", Value: e.validation}, {Name: "AIP_TEST_DATA_URI", Value: e.test}}
}

// withDataEnv adds the data variables to every worker pool's container.
func withDataEnv(spec *aiplatformpb.CustomJobSpec, e dataEnv) {
	for _, wp := range spec.GetWorkerPoolSpecs() {
		if c := wp.GetContainerSpec(); c != nil {
			c.Env = append(c.Env, e.vars()...)
		}
		if p := wp.GetPythonPackageSpec(); p != nil {
			p.Env = append(p.Env, e.vars()...)
		}
	}
}

// checkInputDataConfig validates input_data_config against its dataset.
func (s *Server) checkInputDataConfig(parent string, c *aiplatformpb.InputDataConfig) error {
	if c.GetDatasetId() == "" {
		return apierror.InvalidArgument("training_pipeline.input_data_config.dataset_id is required")
	}
	name := parent + "/datasets/" + c.GetDatasetId()
	st := s.notebooks()
	st.mu.Lock()
	ds, err := st.dsets().dataset(name)
	kind := ""
	if err == nil {
		kind = datasetKind(ds.GetMetadataSchemaUri())
	}
	bqSource := kind == "tabular" && tabularBigQuery(ds) != ""
	st.mu.Unlock()
	if err != nil {
		return apierror.NotFound("training_pipeline.input_data_config.dataset_id: Dataset %s not found", name)
	}
	if c.GetSavedQueryId() != "" && c.GetAnnotationSchemaUri() != "" {
		return apierror.InvalidArgument("training_pipeline.input_data_config: only one of saved_query_id and annotation_schema_uri may be set")
	}
	tabularOnly := func(what string) error {
		if kind != "tabular" {
			return apierror.InvalidArgument("training_pipeline.input_data_config.%s is supported only for tabular Datasets", what)
		}
		return nil
	}
	switch sp := c.GetSplit().(type) {
	case *aiplatformpb.InputDataConfig_FractionSplit:
		if err := checkFractions(sp.FractionSplit.GetTrainingFraction(), sp.FractionSplit.GetValidationFraction(), sp.FractionSplit.GetTestFraction(), "training_pipeline.input_data_config.fraction_split"); err != nil {
			return err
		}
	case *aiplatformpb.InputDataConfig_FilterSplit:
		if kind == "tabular" {
			return apierror.InvalidArgument("training_pipeline.input_data_config.filter_split is supported only for Datasets containing DataItems (unstructured Datasets)")
		}
	case *aiplatformpb.InputDataConfig_PredefinedSplit:
		if err := tabularOnly("predefined_split"); err != nil {
			return err
		}
		if sp.PredefinedSplit.GetKey() == "" {
			return apierror.InvalidArgument("training_pipeline.input_data_config.predefined_split.key is required")
		}
	case *aiplatformpb.InputDataConfig_TimestampSplit:
		if err := tabularOnly("timestamp_split"); err != nil {
			return err
		}
		t := sp.TimestampSplit
		if t.GetKey() == "" {
			return apierror.InvalidArgument("training_pipeline.input_data_config.timestamp_split.key is required")
		}
		if err := checkFractions(t.GetTrainingFraction(), t.GetValidationFraction(), t.GetTestFraction(), "training_pipeline.input_data_config.timestamp_split"); err != nil {
			return err
		}
	case *aiplatformpb.InputDataConfig_StratifiedSplit:
		if err := tabularOnly("stratified_split"); err != nil {
			return err
		}
		t := sp.StratifiedSplit
		if t.GetKey() == "" {
			return apierror.InvalidArgument("training_pipeline.input_data_config.stratified_split.key is required")
		}
		if err := checkFractions(t.GetTrainingFraction(), t.GetValidationFraction(), t.GetTestFraction(), "training_pipeline.input_data_config.stratified_split"); err != nil {
			return err
		}
	}
	switch d := c.GetDestination().(type) {
	case nil:
		return apierror.InvalidArgument("training_pipeline.input_data_config needs gcs_destination or bigquery_destination: where the training data is written")
	case *aiplatformpb.InputDataConfig_GcsDestination:
		if _, _, err := splitNotebookGCS("training_pipeline.input_data_config.gcs_destination.output_uri_prefix", d.GcsDestination.GetOutputUriPrefix(), true); err != nil {
			return err
		}
	case *aiplatformpb.InputDataConfig_BigqueryDestination:
		if !bqSource {
			return apierror.InvalidArgument("training_pipeline.input_data_config.bigquery_destination is only applicable to custom training with a tabular Dataset with a BigQuery source")
		}
		if !bqProjectRE.MatchString(d.BigqueryDestination.GetOutputUri()) {
			return apierror.InvalidArgument("training_pipeline.input_data_config.bigquery_destination.output_uri %q is not bq://PROJECT or bq://PROJECT.DATASET", d.BigqueryDestination.GetOutputUri())
		}
	}
	return nil
}

// tabularBigQuery is a tabular dataset's bigquerySource.uri, or "".
func tabularBigQuery(ds *aiplatformpb.Dataset) string {
	return ds.GetMetadata().GetStructValue().GetFields()["inputConfig"].GetStructValue().GetFields()["bigquerySource"].GetStructValue().GetFields()["uri"].GetStringValue()
}

// tabularGCS is a tabular dataset's gcsSource.uri list.
func tabularGCS(ds *aiplatformpb.Dataset) []string {
	g := ds.GetMetadata().GetStructValue().GetFields()["inputConfig"].GetStructValue().GetFields()["gcsSource"].GetStructValue().GetFields()["uri"]
	if g == nil {
		return nil
	}
	if l := g.GetListValue(); l != nil {
		var out []string
		for _, v := range l.GetValues() {
			out = append(out, v.GetStringValue())
		}
		return out
	}
	return []string{g.GetStringValue()}
}

// exportTrainingData exports a pipeline's input data, answering where.
func (s *Server) exportTrainingData(ctx context.Context, runner PipelineRunner, parent string, c *aiplatformpb.InputDataConfig, at time.Time) (dataEnv, error) {
	dataset := parent + "/datasets/" + c.GetDatasetId()
	v, err := s.datasetView(dataset)
	if err != nil {
		return dataEnv{}, err
	}
	if datasetKind(v.dataset.GetMetadataSchemaUri()) == "tabular" {
		return s.exportTabular(ctx, runner, v.dataset, c, at)
	}
	sel := dsSelect{annotationsFilter: c.GetAnnotationsFilter(), savedQuery: c.GetSavedQueryId(), schema: c.GetAnnotationSchemaUri()}
	switch sp := c.GetSplit().(type) {
	case *aiplatformpb.InputDataConfig_FractionSplit:
		sel.fraction = sp.FractionSplit
	case *aiplatformpb.InputDataConfig_FilterSplit:
		sel.filter = sp.FilterSplit
	}
	files, _, format, err := s.exportUnstructured(ctx, runner, dataset, sel, strings.TrimSuffix(c.GetGcsDestination().GetOutputUriPrefix(), "/"), at)
	if err != nil {
		return dataEnv{}, err
	}
	return dataEnv{format: format, training: files[0], validation: files[1], test: files[2]}, nil
}

// tabRows are a tabular source's columns and rows (each value as text, ok
// false for NULL), and its BigQuery schema when it is a table.
type tabRows struct {
	cols   []string
	rows   [][]string
	nulls  [][]bool
	schema *bqSchema
	raw    []map[string]any
}

func (s *Server) readTabular(ctx context.Context, runner PipelineRunner, ds *aiplatformpb.Dataset) (tabRows, error) {
	if uri := tabularBigQuery(ds); uri != "" {
		m := dsBQTableRE.FindStringSubmatch(uri)
		if m == nil {
			return tabRows{}, apierror.InvalidArgument("the dataset's bigquerySource.uri %q is not bq://PROJECT.DATASET.TABLE", uri)
		}
		schema, rows, err := bigQueryTable(ctx, runner, m[1], m[2], m[3])
		if err != nil {
			return tabRows{}, err
		}
		t := tabRows{schema: &schema, raw: rows}
		for _, f := range schema.Fields {
			t.cols = append(t.cols, f.Name)
		}
		for _, r := range rows {
			vals := make([]string, len(t.cols))
			nulls := make([]bool, len(t.cols))
			for i, c := range t.cols {
				if r[c] == nil {
					nulls[i] = true
					continue
				}
				switch x := r[c].(type) {
				case string:
					vals[i] = x
				default:
					b, _ := json.Marshal(x)
					vals[i] = string(b)
				}
			}
			t.rows, t.nulls = append(t.rows, vals), append(t.nulls, nulls)
		}
		return t, nil
	}
	uris := tabularGCS(ds)
	if len(uris) == 0 {
		return tabRows{}, apierror.FailedPrecondition("the tabular dataset %s has no metadata.inputConfig (gcsSource or bigquerySource) to read", ds.GetName())
	}
	files, err := expandURIs(ctx, runner, "the dataset's gcsSource.uri", uris)
	if err != nil {
		return tabRows{}, err
	}
	var t tabRows
	for _, bo := range files {
		b, err := runner.ReadObject(ctx, bo[0], bo[1], 0)
		if err != nil {
			return tabRows{}, apierror.InvalidArgument("gs://%s/%s cannot be read: %v", bo[0], bo[1], err)
		}
		recs, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
		if err != nil {
			return tabRows{}, apierror.InvalidArgument("gs://%s/%s is not CSV: %v", bo[0], bo[1], err)
		}
		if len(recs) == 0 {
			continue
		}
		if t.cols == nil {
			t.cols = recs[0]
		} else if strings.Join(recs[0], ",") != strings.Join(t.cols, ",") {
			return tabRows{}, apierror.InvalidArgument("gs://%s/%s has the header %q, and the dataset's first file %q: every CSV file needs the same columns", bo[0], bo[1], strings.Join(recs[0], ","), strings.Join(t.cols, ","))
		}
		for _, r := range recs[1:] {
			t.rows, t.nulls = append(t.rows, r), append(t.nulls, make([]bool, len(r)))
		}
	}
	if t.cols == nil {
		return tabRows{}, apierror.FailedPrecondition("the tabular dataset %s's CSV files are empty", ds.GetName())
	}
	return t, nil
}

func (t tabRows) col(key string) (int, error) {
	for i, c := range t.cols {
		if c == key {
			return i, nil
		}
	}
	return -1, apierror.InvalidArgument("the split key %q is not one of the dataset's columns (%s)", key, strings.Join(t.cols, ", "))
}

func rowKey(i int, r []string) string { return strconv.Itoa(i) + "\x00" + strings.Join(r, "\x00") }

// parseTime reads an RFC 3339 time, or a BigQuery TIMESTAMP's seconds.
func parseTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC(), true
	}
	return time.Time{}, false
}

func fractionsOr(tr, va, te float64) (float64, float64, float64) {
	if tr == 0 && va == 0 && te == 0 {
		return 0.8, 0.1, 0.1
	}
	return tr, va, te
}

// tabularRoles gives each row a role ("" left out) by c's split.
func tabularRoles(t tabRows, c *aiplatformpb.InputDataConfig) ([]string, error) {
	roles := make([]string, len(t.rows))
	switch sp := c.GetSplit().(type) {
	case *aiplatformpb.InputDataConfig_PredefinedSplit:
		k, err := t.col(sp.PredefinedSplit.GetKey())
		if err != nil {
			return nil, err
		}
		for i, r := range t.rows {
			if k < len(r) && !t.nulls[i][k] {
				switch v := strings.ToLower(strings.TrimSpace(r[k])); v {
				case "training", "validation", "test":
					roles[i] = v
				}
			}
		}
	case *aiplatformpb.InputDataConfig_TimestampSplit:
		ts := sp.TimestampSplit
		k, err := t.col(ts.GetKey())
		if err != nil {
			return nil, err
		}
		tr, va, te := fractionsOr(ts.GetTrainingFraction(), ts.GetValidationFraction(), ts.GetTestFraction())
		type tr2 struct {
			i int
			t time.Time
		}
		var keyed []tr2
		for i, r := range t.rows {
			if k < len(r) && !t.nulls[i][k] {
				if at, ok := parseTime(strings.TrimSpace(r[k])); ok {
					keyed = append(keyed, tr2{i, at})
				}
			}
		}
		// The youngest first.
		sort.SliceStable(keyed, func(a, b int) bool { return keyed[a].t.After(keyed[b].t) })
		n := float64(len(keyed))
		for j, e := range keyed {
			switch p := float64(j) / n; {
			case p < tr:
				roles[e.i] = "training"
			case p < tr+va:
				roles[e.i] = "validation"
			case p < tr+va+te:
				roles[e.i] = "test"
			default:
				roles[e.i] = "training"
			}
		}
	case *aiplatformpb.InputDataConfig_StratifiedSplit:
		ss := sp.StratifiedSplit
		k, err := t.col(ss.GetKey())
		if err != nil {
			return nil, err
		}
		tr, va, te := fractionsOr(ss.GetTrainingFraction(), ss.GetValidationFraction(), ss.GetTestFraction())
		groups := map[string][]int{}
		for i, r := range t.rows {
			v := ""
			if k < len(r) {
				v = r[k]
			}
			groups[v] = append(groups[v], i)
		}
		vals := make([]string, 0, len(groups))
		for v := range groups {
			vals = append(vals, v)
		}
		sort.SliceStable(vals, func(a, b int) bool {
			if len(groups[vals[a]]) != len(groups[vals[b]]) {
				return len(groups[vals[a]]) > len(groups[vals[b]])
			}
			return vals[a] < vals[b]
		})
		for rank, v := range vals {
			idx := groups[v]
			if rank >= 500 || len(idx) < 3 {
				for _, i := range idx {
					roles[i] = fractionRole(rowKey(i, t.rows[i]), tr, va, te)
				}
				continue
			}
			sort.SliceStable(idx, func(a, b int) bool {
				ha := sha256.Sum256([]byte(rowKey(idx[a], t.rows[idx[a]])))
				hb := sha256.Sum256([]byte(rowKey(idx[b], t.rows[idx[b]])))
				return string(ha[:]) < string(hb[:])
			})
			n := float64(len(idx))
			for j, i := range idx {
				switch p := float64(j) / n; {
				case p < va:
					roles[i] = "validation"
				case p < va+te:
					roles[i] = "test"
				default:
					roles[i] = "training"
				}
			}
		}
	default:
		tr, va, te := 0.8, 0.1, 0.1
		if f := c.GetFractionSplit(); f != nil {
			tr, va, te = fractionsOr(f.GetTrainingFraction(), f.GetValidationFraction(), f.GetTestFraction())
		}
		for i, r := range t.rows {
			roles[i] = fractionRole(rowKey(i, r), tr, va, te)
		}
	}
	return roles, nil
}

func (s *Server) exportTabular(ctx context.Context, runner PipelineRunner, ds *aiplatformpb.Dataset, c *aiplatformpb.InputDataConfig, at time.Time) (dataEnv, error) {
	t, err := s.readTabular(ctx, runner, ds)
	if err != nil {
		return dataEnv{}, err
	}
	roles, err := tabularRoles(t, c)
	if err != nil {
		return dataEnv{}, err
	}
	id := ds.GetName()[strings.LastIndex(ds.GetName(), "/")+1:]
	if bd := c.GetBigqueryDestination(); bd != nil {
		project := strings.TrimPrefix(bd.GetOutputUri(), "bq://")
		if i := strings.Index(project, "."); i >= 0 {
			project = project[:i]
		}
		dataset := "dataset_" + id + "_tables_" + strings.NewReplacer("-", "_", ":", "_", ".", "_").Replace(at.UTC().Format("2006-01-02T15:04:05.000Z"))
		byRole := map[string][]map[string]any{}
		for i, r := range t.raw {
			if roles[i] != "" {
				byRole[roles[i]] = append(byRole[roles[i]], r)
			}
		}
		if err := writeBigQueryTables(ctx, runner, project, dataset, *t.schema, byRole); err != nil {
			return dataEnv{}, err
		}
		base := "bq://" + project + "." + dataset + "."
		return dataEnv{format: "bigquery", training: base + "training", validation: base + "validation", test: base + "test"}, nil
	}
	dir := strings.TrimSuffix(c.GetGcsDestination().GetOutputUriPrefix(), "/") + "/dataset-" + id + "-tables-" + at.UTC().Format("2006-01-02T15:04:05.000Z")
	var env dataEnv
	env.format = "csv"
	for _, role := range []string{"training", "validation", "test"} {
		var b strings.Builder
		w := csv.NewWriter(&b)
		_ = w.Write(t.cols)
		for i, r := range t.rows {
			if roles[i] == role {
				_ = w.Write(r)
			}
		}
		w.Flush()
		uri := dir + "/" + role + "-00000-of-00001.csv"
		bucket, object, err := splitNotebookGCS("gcs_destination", uri, false)
		if err != nil {
			return dataEnv{}, err
		}
		if err := runner.WriteObject(ctx, bucket, object, "text/csv", []byte(b.String())); err != nil {
			return dataEnv{}, apierror.FailedPrecondition("writing %s: %v", uri, err)
		}
		w2 := dir + "/" + role + "-*.csv"
		switch role {
		case "training":
			env.training = w2
		case "validation":
			env.validation = w2
		case "test":
			env.test = w2
		}
	}
	return env, nil
}

// bigQueryTable is a table's schema and rows.
func bigQueryTable(ctx context.Context, runner PipelineRunner, project, dataset, table string) (bqSchema, []map[string]any, error) {
	bq, ok := runner.(PipelineBigQuery)
	if !ok {
		return bqSchema{}, nil, apierror.FailedPrecondition("BigQuery is not served on this instance (start with --services including bigquery)")
	}
	code, b, err := bq.BigQueryREST(ctx, "GET", "/bigquery/v2/projects/"+url.PathEscape(project)+"/datasets/"+url.PathEscape(dataset)+"/tables/"+url.PathEscape(table), nil)
	if err != nil {
		return bqSchema{}, nil, err
	}
	if code/100 != 2 {
		return bqSchema{}, nil, apierror.InvalidArgument("bq://%s.%s.%s cannot be read: %s", project, dataset, table, strings.TrimSpace(string(b)))
	}
	var meta struct {
		Schema bqSchema `json:"schema"`
	}
	_ = json.Unmarshal(b, &meta)
	rows, err := bigQueryRows(ctx, runner, project, dataset, table)
	return meta.Schema, rows, err
}

// insertValue is a tabledata value as insertAll takes it back: a
// TIMESTAMP's seconds as a number.
func insertValue(f bqField, v any) any {
	if s, ok := v.(string); ok && (f.Type == "TIMESTAMP") {
		if x, err := strconv.ParseFloat(s, 64); err == nil {
			return x
		}
	}
	return v
}

// writeBigQueryTables creates dataset and its training, validation and
// test tables with schema, holding byRole's rows.
func writeBigQueryTables(ctx context.Context, runner PipelineRunner, project, dataset string, schema bqSchema, byRole map[string][]map[string]any) error {
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
		if code/100 != 2 {
			return apierror.FailedPrecondition("writing the training data to BigQuery: %s", strings.TrimSpace(string(out)))
		}
		var r struct {
			InsertErrors []any `json:"insertErrors"`
		}
		if json.Unmarshal(out, &r) == nil && len(r.InsertErrors) > 0 {
			e, _ := json.Marshal(r.InsertErrors)
			return apierror.FailedPrecondition("writing the training data to BigQuery: %s", e)
		}
		return nil
	}
	pp := "/bigquery/v2/projects/" + url.PathEscape(project)
	if err := call("POST", pp+"/datasets", map[string]any{"datasetReference": map[string]any{"projectId": project, "datasetId": dataset}}); err != nil {
		return err
	}
	for _, t := range []string{"training", "validation", "test"} {
		if err := call("POST", pp+"/datasets/"+url.PathEscape(dataset)+"/tables", map[string]any{
			"tableReference": map[string]any{"projectId": project, "datasetId": dataset, "tableId": t}, "schema": schema}); err != nil {
			return err
		}
		rows := byRole[t]
		for start := 0; start < len(rows); start += 500 {
			end := min(start+500, len(rows))
			var batch []map[string]any
			for _, r := range rows[start:end] {
				j := map[string]any{}
				for _, f := range schema.Fields {
					j[f.Name] = insertValue(f, r[f.Name])
				}
				batch = append(batch, map[string]any{"json": j})
			}
			if err := call("POST", pp+"/datasets/"+url.PathEscape(dataset)+"/tables/"+t+"/insertAll", map[string]any{"rows": batch}); err != nil {
				return err
			}
		}
	}
	return nil
}

var _ = fmt.Sprint

var (
	// dsBQTableRE is bq://PROJECT.DATASET.TABLE; bqProjectRE bq://PROJECT
	// or bq://PROJECT.DATASET.
	dsBQTableRE = regexp.MustCompile(`^bq://([a-z][a-z0-9:.-]*?)\.([A-Za-z0-9_]+)\.([A-Za-z0-9_$-]+)$`)
	bqProjectRE = regexp.MustCompile(`^bq://[a-z][a-z0-9:.-]*?(\.[A-Za-z0-9_]+)?$`)
)
