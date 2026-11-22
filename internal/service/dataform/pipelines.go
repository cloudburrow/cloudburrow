package dataform

// Pipelines, notebooks, unit tests and data preparations (#2105).
//
//   - pipeline_config (CodeCompilationConfig, InstallNpmPackagesRequest;
//     output only on a WorkflowInvocation) "defines the pipeline type and
//     path within the Git repository": a DATAFORM pipeline compiles the
//     folder at path, "where workflow_settings.yaml or dataform.json is
//     located"; SQL and NOTEBOOK are BigQuery Studio's single file assets,
//     the one file at path, compiled here into one operations or notebook
//     action as Dataform core compiles a .sql operation or an actions.yaml
//     notebook (target name the file's base name, schema the default one).
//   - Notebook actions (Dataform core's notebooks, and BigQuery Studio's
//     scheduled notebooks) run on the instance's notebook runtime
//     (internal/nbruntime, CloudBurrow's Colab Enterprise runtime): each
//     code cell in order, stopping at the first that raises. The executed
//     notebook is written to default_notebook_runtime_options'
//     gcs_output_bucket.
//   - default_notebook_runtime_options' gcs_repository_snapshot_destination
//     has a compilation write a snapshot of the compiled files to Cloud
//     Storage, described by gcs_repository_snapshot_metadata.
//   - Unit tests (Dataform core's `type: "test"`) compile to
//     bigquery_unit_test actions; an invocation runs them only with
//     execution_mode UNIT_TESTS_ONLY, and every other action only without
//     it ("DEFAULT ... runs all actions except unit tests"). A test runs its
//     test and expected output queries and compares them as Dataform's CLI
//     does (`dataform test`, @dataform/cli 3.1.6): the same columns, the
//     same number of rows, and equal values row by row.
//   - Data preparations: the SQL ones (Dataform core's
//     `type: "dataPreparation"`, a pipe syntax query) run into their
//     target with their load mode, MERGE included (mergeKeys), the rows
//     failing a validation (${EXPECT}) going to the error table. A YAML one
//     (contents_yaml, a .dp.yaml file) runs its documented parts: each
//     destination node gets its source table's rows; a node with steps,
//     whose format Google does not document, is refused naming them.
//
// UNVERIFIED (re-read 2026-11-22: https://docs.cloud.google.com/dataform/reference/rest/v1/projects.locations.repositories.compilationResults), Google documenting none of them: the notebook
// output object is gs://BUCKET/JOB_ID/FILE.ipynb; the snapshot is a gzipped
// tar of the compiled files at DESTINATION/COMPILATION_ID.tar.gz; a single
// file asset's action target; the job ID's form (Vertex AI's numeric IDs);
// the unit test failure texts are the CLI's.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// NotebookRunner runs a notebook's code cells on a runtime of its own and
// returns the executed notebook (its outputs filled in). A cell that
// raises returns the notebook so far with the error.
type NotebookRunner interface {
	RunNotebook(ctx context.Context, key, project string, notebook []byte) ([]byte, error)
}

// TemplateNotebookRunner is a NotebookRunner that runs a notebook on a
// runtime of a Colab Enterprise runtime template
// (default_notebook_runtime_options.ai_platform_notebook_runtime_template).
type TemplateNotebookRunner interface {
	RunNotebookOnTemplate(ctx context.Context, key, project, template string, notebook []byte) ([]byte, error)
}

// ObjectWriter writes one Cloud Storage object and returns its generation
// and its CRC32C (big-endian, base64), as the JSON API reports them.
type ObjectWriter interface {
	PutObject(ctx context.Context, bucket, object, contentType string, data []byte) (generation int64, crc32c string, err error)
}

// RowReader is a Warehouse that returns a query's rows, each value as its
// string form ("" for NULL); unit tests need it.
type RowReader interface {
	Rows(ctx context.Context, project, location, sql string) (jobID string, columns []string, rows [][]*string, bytes int64, err error)
}

var (
	gcsBucketRE   = regexp.MustCompile(`^gs://[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)
	gcsPrefixRE   = regexp.MustCompile(`^gs://([a-z0-9][a-z0-9._-]{1,220}[a-z0-9])(/.*)?$`)
	runtimeTmplRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/notebookRuntimeTemplates/[^/]+$`)
)

func checkPipelineConfig(field string, p *dfpb.PipelineConfig) error {
	if p == nil {
		return nil
	}
	switch p.GetPipelineType() {
	case dfpb.PipelineConfig_DATAFORM, dfpb.PipelineConfig_SQL, dfpb.PipelineConfig_NOTEBOOK:
	default:
		return apierror.InvalidArgument("%s.pipeline_type is required: DATAFORM, SQL or NOTEBOOK", field)
	}
	pth := p.GetPath()
	if pth == "" && p.GetPipelineType() != dfpb.PipelineConfig_DATAFORM {
		return apierror.InvalidArgument("%s.path is required", field)
	}
	if strings.HasPrefix(pth, "/") || strings.Contains(pth, "\\") {
		return apierror.InvalidArgument("%s.path %q must be a relative path within the repository", field, pth)
	}
	for _, seg := range strings.Split(pth, "/") {
		if seg == ".." {
			return apierror.InvalidArgument("%s.path %q must be a relative path within the repository", field, pth)
		}
	}
	return nil
}

func checkNotebookRuntimeOptions(o *dfpb.NotebookRuntimeOptions) error {
	if o == nil {
		return nil
	}
	if b := o.GetGcsOutputBucket(); b != "" && !gcsBucketRE.MatchString(b) {
		return apierror.InvalidArgument("default_notebook_runtime_options.gcs_output_bucket %q must be gs://BUCKET", b)
	}
	if t := o.GetAiPlatformNotebookRuntimeTemplate(); t != "" && !runtimeTmplRE.MatchString(t) {
		return apierror.InvalidArgument("default_notebook_runtime_options.ai_platform_notebook_runtime_template %q must be projects/*/locations/*/notebookRuntimeTemplates/*", t)
	}
	if d := o.GetGcsRepositorySnapshotDestination(); d != nil {
		u := d.GetRepositorySnapshotUri()
		if u == "" && o.GetGcsOutputBucket() == "" {
			return apierror.InvalidArgument("default_notebook_runtime_options.gcs_repository_snapshot_destination.repository_snapshot_uri is required without a gcs_output_bucket")
		}
		if u != "" && !gcsPrefixRE.MatchString(u) {
			return apierror.InvalidArgument("default_notebook_runtime_options.gcs_repository_snapshot_destination.repository_snapshot_uri %q must be gs://BUCKET/PATH/", u)
		}
	}
	return nil
}

// pipelineRoot is the directory a DATAFORM pipeline compiles: dir/path,
// which must hold workflow_settings.yaml or dataform.json.
func pipelineRoot(dir string, p *dfpb.PipelineConfig) (string, error) {
	root := filepath.Join(dir, filepath.FromSlash(p.GetPath()))
	for _, f := range []string{"workflow_settings.yaml", "dataform.json"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			return root, nil
		}
	}
	return "", apierror.InvalidArgument("pipeline_config.path %q holds neither workflow_settings.yaml nor dataform.json", p.GetPath())
}

// singleFileActions compiles a SQL or NOTEBOOK single file asset: the
// file at the pipeline's path, as one action.
func singleFileActions(dir, project string, p *dfpb.PipelineConfig, cfg *dfpb.CodeCompilationConfig) ([]*dfpb.CompilationResultAction, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p.GetPath())))
	if err != nil {
		return nil, apierror.InvalidArgument("pipeline_config.path %q: no such file in the repository", p.GetPath())
	}
	db := cfg.GetDefaultDatabase()
	if db == "" {
		db = project
	}
	schema := cfg.GetDefaultSchema()
	if schema == "" {
		schema = "dataform"
	}
	base := path.Base(p.GetPath())
	name := strings.TrimSuffix(base, path.Ext(base))
	t := &dfpb.Target{Database: db + cfg.GetDatabaseSuffix(), Schema: schema + cfg.GetSchemaSuffix(), Name: cfg.GetTablePrefix() + name}
	a := &dfpb.CompilationResultAction{Target: t, CanonicalTarget: clone(t), FilePath: p.GetPath()}
	if p.GetPipelineType() == dfpb.PipelineConfig_NOTEBOOK {
		contents, err := stripOutputs(b)
		if err != nil {
			return nil, apierror.InvalidArgument("pipeline_config.path %q: %v", p.GetPath(), err)
		}
		a.CompiledObject = &dfpb.CompilationResultAction_Notebook_{Notebook: &dfpb.CompilationResultAction_Notebook{Contents: string(contents)}}
	} else {
		a.CompiledObject = &dfpb.CompilationResultAction_Operations_{Operations: &dfpb.CompilationResultAction_Operations{Queries: []string{string(b)}}}
	}
	return []*dfpb.CompilationResultAction{a}, nil
}

// stripOutputs clears a notebook's outputs and execution counts, as
// Dataform core does to a notebook it compiles.
func stripOutputs(b []byte) ([]byte, error) {
	var nb map[string]any
	if err := json.Unmarshal(b, &nb); err != nil {
		return nil, errors.New("not an nbformat notebook")
	}
	cells, _ := nb["cells"].([]any)
	for _, c := range cells {
		if cell, _ := c.(map[string]any); cell != nil && cell["cell_type"] == "code" {
			cell["outputs"] = []any{}
			cell["execution_count"] = nil
		}
	}
	return json.Marshal(nb)
}

// uploadSnapshot writes the compiled files of dir to the snapshot
// destination, for the compilation id.
func (s *Server) uploadSnapshot(ctx context.Context, dir, id string, o *dfpb.NotebookRuntimeOptions) (*dfpb.GcsRepositorySnapshotMetadata, error) {
	d := o.GetGcsRepositorySnapshotDestination()
	if d == nil {
		return nil, nil
	}
	if s.opts.Storage == nil {
		return nil, apierror.FailedPrecondition("gcs_repository_snapshot_destination: Cloud Storage is not served on this instance")
	}
	uri := d.GetRepositorySnapshotUri()
	if uri == "" {
		uri = o.GetGcsOutputBucket() + "/"
	}
	m := gcsPrefixRE.FindStringSubmatch(uri)
	bucket, prefix := m[1], strings.TrimPrefix(m[2], "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	data, err := tarGz(dir)
	if err != nil {
		return nil, apierror.Internal(err, "pack the repository snapshot")
	}
	object := prefix + id + ".tar.gz"
	gen, crc, err := s.opts.Storage.PutObject(ctx, bucket, object, "application/gzip", data)
	if err != nil {
		return nil, apierror.FailedPrecondition("write the repository snapshot to gs://%s/%s: %v", bucket, object, err)
	}
	return &dfpb.GcsRepositorySnapshotMetadata{RepositorySnapshotUri: "gs://" + bucket + "/" + object, Crc32CChecksum: crc, Generation: gen}, nil
}

// tarGz packs a directory's files, leaving out .git and node_modules.
func tarGz(dir string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if e.IsDir() {
			if e.Name() == ".git" || e.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !e.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b))}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// jobID is a notebook execution's ID, numeric as Vertex AI's are.
func jobID() string {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n.String()
}

// runNotebook runs a notebook action and writes its output.
func (s *Server) runNotebook(ctx context.Context, inv string, a *dfpb.CompilationResultAction, opts *dfpb.NotebookRuntimeOptions) (*dfpb.WorkflowInvocationAction_NotebookAction, error) {
	nb := a.GetNotebook()
	act := &dfpb.WorkflowInvocationAction_NotebookAction{Contents: nb.GetContents(), FilePath: a.GetFilePath()}
	template := opts.GetAiPlatformNotebookRuntimeTemplate()
	tr, onTemplate := s.opts.Notebooks.(TemplateNotebookRunner)
	if template != "" && !onTemplate {
		return act, fmt.Errorf("the notebook runtime template %s cannot be used: this instance serves no Colab Enterprise runtime templates (Vertex AI is not served)", template)
	}
	if s.opts.Notebooks == nil {
		return act, errors.New("notebooks need the notebook runtime, which this instance cannot run (Docker and BigQuery are needed)")
	}
	contents := []byte(nb.GetContents())
	if len(bytes.TrimSpace(contents)) == 0 {
		return act, errors.New("the notebook has no contents")
	}
	act.JobId = jobID()
	project := a.GetTarget().GetDatabase()
	key := "dataform/" + path.Base(inv) + "/" + act.JobId
	var out []byte
	var runErr error
	if template != "" {
		// ai_platform_notebook_runtime_template: the notebook runs on a
		// runtime made from that Colab Enterprise template.
		out, runErr = tr.RunNotebookOnTemplate(ctx, key, project, template, contents)
	} else {
		out, runErr = s.opts.Notebooks.RunNotebook(ctx, key, project, contents)
	}
	if bucket := opts.GetGcsOutputBucket(); bucket != "" && out != nil {
		if s.opts.Storage == nil {
			return act, errors.New("gcs_output_bucket: Cloud Storage is not served on this instance")
		}
		file := path.Base(a.GetFilePath())
		if file == "" || file == "." {
			file = a.GetTarget().GetName() + ".ipynb"
		}
		object := act.JobId + "/" + file
		if _, _, err := s.opts.Storage.PutObject(ctx, strings.TrimPrefix(bucket, "gs://"), object, "application/x-ipynb+json", out); err != nil {
			if runErr == nil {
				return act, fmt.Errorf("write the executed notebook to %s/%s: %v", bucket, object, err)
			}
		}
	}
	return act, runErr
}

// runUnitTest runs a unit test's two queries and compares their rows.
func (s *Server) runUnitTest(ctx context.Context, a *dfpb.CompilationResultAction, location string) (*dfpb.WorkflowInvocationAction_BigQueryUnitTestAction, error) {
	u := a.GetBigqueryUnitTest()
	act := &dfpb.WorkflowInvocationAction_BigQueryUnitTestAction{ActualResultsSqlScript: u.GetTestQuery(), ExpectedResultsSqlScript: u.GetExpectedOutputQuery()}
	rr, ok := s.opts.Warehouse.(RowReader)
	if !ok || s.opts.Warehouse == nil {
		return act, errors.New("BigQuery is not served on this instance")
	}
	project := a.GetTarget().GetDatabase()
	id, cols, rows, n, err := rr.Rows(ctx, project, location, u.GetTestQuery())
	act.ActualResultsJobId, act.TotalProcessedBytes = id, n
	if err != nil {
		return act, fmt.Errorf("the test query failed: %v", err)
	}
	eid, ecols, erows, en, err := rr.Rows(ctx, project, location, u.GetExpectedOutputQuery())
	act.ExpectedResultsJobId = eid
	act.TotalProcessedBytes += en
	act.TotalBilledBytes = act.TotalProcessedBytes
	if err != nil {
		return act, fmt.Errorf("the expected output query failed: %v", err)
	}
	if msgs := compareRows(cols, rows, ecols, erows); len(msgs) > 0 {
		return act, errors.New(strings.Join(msgs, "\n"))
	}
	return act, nil
}

// compareRows is @dataform/cli's unit test comparison (api/commands/test.ts):
// the row counts, then the column sets, then each value row by row.
func compareRows(cols []string, rows [][]*string, ecols []string, erows [][]*string) []string {
	if len(rows) != len(erows) {
		return []string{fmt.Sprintf("Expected %d rows, but saw %d rows.", len(erows), len(rows))}
	}
	if len(rows) == 0 {
		return nil
	}
	a, e := append([]string(nil), cols...), append([]string(nil), ecols...)
	sort.Strings(a)
	sort.Strings(e)
	if strings.Join(a, ",") != strings.Join(e, ",") {
		return []string{fmt.Sprintf("Expected columns %q, but saw %q.", strings.Join(e, ","), strings.Join(a, ","))}
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}
	var msgs []string
	for r := range erows {
		for j, c := range ecols {
			want, got := erows[r][j], rows[r][idx[c]]
			if (want == nil) != (got == nil) || (want != nil && *want != *got) {
				msgs = append(msgs, fmt.Sprintf("For row %d and column %q: expected %s, but saw %s.", r, c, show(want), show(got)))
			}
		}
	}
	return msgs
}

func show(v *string) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%q", *v)
}

// runDataPreparation runs a data preparation into its target with its
// load mode: a SQL one's pipe-syntax query (its validations sending the
// rows that fail them to the error table), or a YAML one's nodes. merge
// are a MERGE load mode's unique keys (mergeKeys).
func (s *Server) runDataPreparation(ctx context.Context, a *dfpb.CompilationResultAction, location string, merge []string) (*dfpb.WorkflowInvocationAction_DataPreparationAction, error) {
	dp := a.GetDataPreparation()
	act := &dfpb.WorkflowInvocationAction_DataPreparationAction{}
	if y := dp.GetContentsYaml(); y != "" {
		act.Definition = &dfpb.WorkflowInvocationAction_DataPreparationAction_ContentsYaml{ContentsYaml: y}
		return act, s.runYAMLDataPreparation(ctx, act, a, location)
	}
	sqlDef := dp.GetContentsSql()
	def := &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionSqlDefinition{Query: sqlDef.GetQuery()}
	if et := sqlDef.GetErrorTable(); et != nil {
		def.ErrorTable = &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionErrorTable{Target: clone(et.GetTarget()), RetentionDays: et.GetRetentionDays()}
	}
	switch m := sqlDef.GetLoad().GetMode().(type) {
	case *dfpb.CompilationResultAction_LoadConfig_Append:
		def.LoadConfig = &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig{Mode: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig_Append{Append: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionSimpleLoadMode{}}}
	case *dfpb.CompilationResultAction_LoadConfig_Maximum:
		def.LoadConfig = &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig{Mode: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig_Maximum{Maximum: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionIncrementalLoadMode{Column: m.Maximum.GetColumn()}}}
	case *dfpb.CompilationResultAction_LoadConfig_Unique:
		def.LoadConfig = &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig{Mode: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig_Unique{Unique: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionIncrementalLoadMode{Column: m.Unique.GetColumn()}}}
	default:
		// MERGE has no API load config (mergeKeys).
		if len(merge) == 0 {
			def.LoadConfig = &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig{Mode: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionLoadConfig_Replace{Replace: &dfpb.WorkflowInvocationAction_DataPreparationAction_ActionSimpleLoadMode{}}}
		}
	}
	act.Definition = &dfpb.WorkflowInvocationAction_DataPreparationAction_ContentsSql{ContentsSql: def}
	w := s.opts.Warehouse
	if w == nil {
		return act, errors.New("BigQuery is not served on this instance")
	}
	t := a.GetTarget()
	project := t.GetDatabase()
	var et *dfpb.Target
	if e := sqlDef.GetErrorTable(); e != nil && e.GetTarget() != nil {
		et = e.GetTarget()
	}
	q, checks := validations(sqlDef.GetQuery())
	var stmts []string
	run := func(st string) error {
		stmts = append(stmts, st)
		act.GeneratedSql = strings.Join(stmts, ";\n")
		id, err := w.Exec(ctx, project, location, st)
		if id != "" {
			act.JobId = id
		}
		return err
	}
	if err := w.EnsureDataset(ctx, project, t.GetSchema(), location); err != nil {
		return act, fmt.Errorf("create dataset %s.%s: %w", project, t.GetSchema(), err)
	}
	if et != nil {
		if err := w.EnsureDataset(ctx, et.GetDatabase(), et.GetSchema(), location); err != nil {
			return act, fmt.Errorf("create dataset %s.%s: %w", et.GetDatabase(), et.GetSchema(), err)
		}
		if err := run(errorTableDDL(et, sqlDef.GetErrorTable().GetRetentionDays())); err != nil {
			return act, err
		}
	}
	// Validations: "Sends rows that don't meet the validation rule criteria
	// to an error table. If data fails the validation rule and no error
	// table is configured, the data preparation fails during execution"
	// (data-prep-introduction). Each is checked on the rows the steps
	// before it pass.
	for _, c := range checks {
		failing := c.before + "\n|> WHERE NOT (" + c.cond + ") OR (" + c.cond + ") IS NULL"
		if et == nil {
			id, n, err := w.Count(ctx, project, location, "SELECT COUNT(*) FROM (\n"+failing+"\n)")
			if id != "" {
				act.JobId = id
			}
			if err != nil {
				return act, err
			}
			if n > 0 {
				rows := "rows"
				if n == 1 {
					rows = "row"
				}
				return act, fmt.Errorf("%d %s failed the validation rule %q, and the data preparation has no error table", n, rows, c.cond)
			}
			continue
		}
		msg, _ := json.Marshal("Validation failed: " + c.cond)
		if err := run("insert into " + bq(et) + " (error_time, error, row)\nselect current_timestamp(), " + string(msg) + ", to_json(failed)\nfrom (\n" + failing + "\n) as failed"); err != nil {
			return act, err
		}
	}
	exists, _, columns, err := w.Table(ctx, project, t.GetSchema(), t.GetName())
	if err != nil {
		return act, err
	}
	switch m := sqlDef.GetLoad().GetMode().(type) {
	case *dfpb.CompilationResultAction_LoadConfig_Append:
		if exists {
			err = run(insertInto(t, columns, q))
		} else {
			err = run("create table " + bq(t) + " as " + q)
		}
	case *dfpb.CompilationResultAction_LoadConfig_Maximum:
		c := "`" + m.Maximum.GetColumn() + "`"
		if exists {
			err = run(insertInto(t, columns, "select * from ("+q+"\n) where (select max("+c+") from "+bq(t)+") is null or "+c+" > (select max("+c+") from "+bq(t)+")"))
		} else {
			err = run("create table " + bq(t) + " as " + q)
		}
	case *dfpb.CompilationResultAction_LoadConfig_Unique:
		c := "`" + m.Unique.GetColumn() + "`"
		if exists {
			err = run(insertInto(t, columns, "select * from ("+q+"\n) where "+c+" not in (select "+c+" from "+bq(t)+" where "+c+" is not null)"))
		} else {
			err = run("create table " + bq(t) + " as " + q)
		}
	default:
		switch {
		case len(merge) > 0 && exists:
			// Upsert: "When an existing row in the destination table
			// matches the specified merge keys for an input record, the
			// values in this row are updated ... Otherwise, a new row is
			// inserted" (data-prep-introduction).
			err = run(mergeInto(t, columns, q, backticked(merge), ""))
		case len(merge) > 0:
			err = run("create table " + bq(t) + " as " + q)
		default:
			err = run("create or replace table " + bq(t) + " as " + q)
		}
	}
	return act, err
}

// errorTableDDL creates a data preparation's error table: it "holds the
// rows a step could not prepare".
//
// UNVERIFIED (read 2026-11-22: https://docs.cloud.google.com/bigquery/docs/data-prep-introduction):
// Google documents no error table schema; this one holds the time, the
// failure and the row as JSON, partitioned by day so retention_days is
// its partition expiration ("Error table partition expiration in days").
func errorTableDDL(e *dfpb.Target, days int32) string {
	opts := ""
	if days > 0 {
		opts = fmt.Sprintf(" OPTIONS(partition_expiration_days=%d)", days)
	}
	return "create table if not exists " + bq(e) + " (error_time TIMESTAMP, error STRING, row JSON) partition by date(error_time)" + opts
}

// validationMarker is what Dataform core compiles a SQLX data
// preparation's ${EXPECT} to (DataPreparationContext.EXPECT, @dataform/core
// 3.1.6): a validation, a WHERE whose failing rows go to the error table.
const validationMarker = "/* @@VALIDATION */ WHERE"

type validation struct {
	before string // the query up to the validation
	cond   string
}

// validations splits a compiled data preparation query at its
// validations: the query with each made a plain WHERE (the rows that pass
// it), and each validation's condition with the query before it.
func validations(q string) (string, []validation) {
	var out []validation
	for {
		i := strings.Index(q, validationMarker)
		if i < 0 {
			return q, out
		}
		pipe := strings.LastIndex(q[:i], "|>")
		if pipe < 0 || strings.TrimSpace(q[pipe+2:i]) != "" {
			// Not a pipe operator: leave it a WHERE.
			q = q[:i] + "WHERE" + q[i+len(validationMarker):]
			continue
		}
		rest := q[i+len(validationMarker):]
		end := strings.Index(rest, "|>")
		if end < 0 {
			end = len(rest)
		}
		cond := strings.TrimSpace(rest[:end])
		out = append(out, validation{before: strings.TrimRight(q[:pipe], " \n\t"), cond: cond})
		q = q[:pipe] + "|> WHERE " + cond + "\n" + rest[end:]
		q = strings.TrimRight(q, "\n")
	}
}

// yamlNodeKeys and yamlConfigKeys are what Google's data preparation YAML
// is publicly known to hold (Dataform core 3.1.6, core/actions/data_preparation.ts,
// read 2026-11-22: https://github.com/dataform-co/dataform): a node's id,
// its source table or source node, its destination table and the
// generated schemas, and the configuration's error table. Google
// documents no step format (https://docs.cloud.google.com/bigquery/docs/manage-data-preparations,
// read 2026-11-22: such files are migrated to SQLX), so a node holding
// anything else is refused, naming it.
var (
	yamlNodeKeys   = map[string]bool{"id": true, "source": true, "destination": true, "generated": true}
	yamlConfigKeys = map[string]bool{"errorTable": true}
)

type yamlTableRef struct {
	Project string `yaml:"project"`
	Dataset string `yaml:"dataset"`
	Table   string `yaml:"table"`
}

func (r *yamlTableRef) target(def *dfpb.Target) *dfpb.Target {
	t := &dfpb.Target{Database: r.Project, Schema: r.Dataset, Name: r.Table}
	if t.Database == "" {
		t.Database = def.GetDatabase()
	}
	if t.Schema == "" {
		t.Schema = def.GetSchema()
	}
	return t
}

// YAMLDataPreparation is a YAML data preparation's documented parts.
type YAMLDataPreparation struct {
	ErrorTable *yamlTableRef
	Nodes      []YAMLDataPrepNode
}

// YAMLDataPrepNode is one node: its source table or node, and destination.
type YAMLDataPrepNode struct {
	ID          string
	Source      *yamlTableRef
	SourceNode  string
	Destination *yamlTableRef
}

// ParseYAMLDataPreparation reads a data preparation's YAML, refusing what
// is not documented (yamlNodeKeys).
func ParseYAMLDataPreparation(text string) (*YAMLDataPreparation, error) {
	var raw struct {
		Configuration map[string]yaml.Node   `yaml:"configuration"`
		Nodes         []map[string]yaml.Node `yaml:"nodes"`
	}
	dec := yaml.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("the data preparation is not YAML: %v", err)
	}
	var top map[string]yaml.Node
	_ = yaml.Unmarshal([]byte(text), &top)
	for k := range top {
		if k != "configuration" && k != "nodes" {
			return nil, fmt.Errorf("the data preparation's %q is not a documented part of a data preparation's YAML (configuration and nodes)", k)
		}
	}
	out := &YAMLDataPreparation{}
	for k, v := range raw.Configuration {
		if !yamlConfigKeys[k] {
			return nil, fmt.Errorf("the data preparation's configuration.%s is not documented by Google; only configuration.errorTable runs here", k)
		}
		var r yamlTableRef
		if err := v.Decode(&r); err != nil || r.Table == "" {
			return nil, errors.New("configuration.errorTable must be a table: {project, dataset, table}")
		}
		out.ErrorTable = &r
	}
	ids := map[string]bool{}
	for i, n := range raw.Nodes {
		var node YAMLDataPrepNode
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !yamlNodeKeys[k] {
				return nil, fmt.Errorf("node %d's %q is not documented by Google (a node runs here with id, source, destination and generated only); open the data preparation in BigQuery Studio and migrate it to SQLX", i+1, k)
			}
		}
		if v, ok := n["id"]; ok {
			_ = v.Decode(&node.ID)
		}
		if node.ID == "" {
			node.ID = fmt.Sprintf("node%d", i+1)
		}
		if ids[node.ID] {
			return nil, fmt.Errorf("node id %q is used twice", node.ID)
		}
		ids[node.ID] = true
		if v, ok := n["source"]; ok {
			var src struct {
				Table  *yamlTableRef `yaml:"table"`
				NodeID string        `yaml:"nodeId"`
			}
			var srcKeys map[string]yaml.Node
			_ = v.Decode(&srcKeys)
			for k := range srcKeys {
				if k != "table" && k != "nodeId" {
					return nil, fmt.Errorf("node %q's source.%s is not documented by Google (a source is a table or a nodeId)", node.ID, k)
				}
			}
			if err := v.Decode(&src); err != nil {
				return nil, fmt.Errorf("node %q's source: %v", node.ID, err)
			}
			node.Source, node.SourceNode = src.Table, src.NodeID
		}
		if v, ok := n["destination"]; ok {
			var dst struct {
				Table *yamlTableRef `yaml:"table"`
			}
			if err := v.Decode(&dst); err != nil {
				return nil, fmt.Errorf("node %q's destination: %v", node.ID, err)
			}
			node.Destination = dst.Table
		}
		if (node.Source == nil) == (node.SourceNode == "") {
			return nil, fmt.Errorf("node %q needs a source: a table or a nodeId", node.ID)
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out, nil
}

// Query is node id's rows, as a query: its source table's, or its source
// node's.
func (y *YAMLDataPreparation) Query(id string, def *dfpb.Target) (string, error) {
	byID := map[string]YAMLDataPrepNode{}
	for _, n := range y.Nodes {
		byID[n.ID] = n
	}
	seen := map[string]bool{}
	for {
		n, ok := byID[id]
		if !ok {
			return "", fmt.Errorf("no node %q", id)
		}
		if seen[id] {
			return "", fmt.Errorf("node %q's sources form a cycle", id)
		}
		seen[id] = true
		if n.Source != nil {
			return "SELECT * FROM " + bq(n.Source.target(def)), nil
		}
		id = n.SourceNode
	}
}

// runYAMLDataPreparation writes each destination node's rows to its table
// (a full refresh: a load mode in the YAML is not documented).
func (s *Server) runYAMLDataPreparation(ctx context.Context, act *dfpb.WorkflowInvocationAction_DataPreparationAction, a *dfpb.CompilationResultAction, location string) error {
	y, err := ParseYAMLDataPreparation(act.GetContentsYaml())
	if err != nil {
		return err
	}
	w := s.opts.Warehouse
	if w == nil {
		return errors.New("BigQuery is not served on this instance")
	}
	def := a.GetTarget()
	project := def.GetDatabase()
	var stmts []string
	run := func(st string) error {
		stmts = append(stmts, st)
		act.GeneratedSql = strings.Join(stmts, ";\n")
		id, err := w.Exec(ctx, project, location, st)
		if id != "" {
			act.JobId = id
		}
		return err
	}
	if y.ErrorTable != nil {
		et := y.ErrorTable.target(def)
		if err := w.EnsureDataset(ctx, et.GetDatabase(), et.GetSchema(), location); err != nil {
			return fmt.Errorf("create dataset %s.%s: %w", et.GetDatabase(), et.GetSchema(), err)
		}
		if err := run(errorTableDDL(et, 0)); err != nil {
			return err
		}
	}
	wrote := false
	for _, n := range y.Nodes {
		if n.Destination == nil {
			continue
		}
		q, err := y.Query(n.ID, def)
		if err != nil {
			return err
		}
		t := n.Destination.target(def)
		if err := w.EnsureDataset(ctx, t.GetDatabase(), t.GetSchema(), location); err != nil {
			return fmt.Errorf("create dataset %s.%s: %w", t.GetDatabase(), t.GetSchema(), err)
		}
		if err := run("create or replace table " + bq(t) + " as " + q); err != nil {
			return err
		}
		wrote = true
	}
	if !wrote {
		return errors.New("the data preparation has no destination node")
	}
	return nil
}
