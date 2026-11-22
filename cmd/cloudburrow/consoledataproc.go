package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataproc"
)

// The console's Serverless for Apache Spark pages (#2156), as Google's
// Dataproc menu has them under Serverless: Batches (every region's, Create
// batch on a page of its own with the batch type's main file, arguments,
// runtime version, properties, service account and labels; a batch's page
// with its Summary, Output, the driver output from Cloud Storage, and Job
// logs, Cancel and Delete), Interactive sessions (Create from a template
// or a runtime version, its Spark Connect endpoint, Terminate and Delete)
// and Session templates (Create, Edit, Delete).

const dataprocNotStarted = "Serverless for Apache Spark has not started"

func dataprocAPI(svc *dataprocService) (*dataproc.Server, error) {
	if s := svc.API(); s != nil {
		return s, nil
	}
	return nil, errors.New(dataprocNotStarted)
}

var dataprocBatchTypes = map[string]string{"SPARK": "Spark", "PYSPARK": "PySpark", "SPARKR": "SparkR", "SPARKSQL": "Spark SQL", "NOTEBOOK": "PySpark notebook"}

func dataprocBatchType(b *dataprocpb.Batch) string {
	switch b.GetBatchConfig().(type) {
	case *dataprocpb.Batch_PysparkBatch:
		return "PySpark"
	case *dataprocpb.Batch_SparkBatch:
		return "Spark"
	case *dataprocpb.Batch_SparkRBatch:
		return "SparkR"
	case *dataprocpb.Batch_SparkSqlBatch:
		return "Spark SQL"
	case *dataprocpb.Batch_PysparkNotebookBatch:
		return "PySpark notebook"
	}
	return "-"
}

// dataprocState is a state as Google's list shows it: Succeeded, Running.
func dataprocState(s string) string {
	if s == "" || strings.HasSuffix(s, "UNSPECIFIED") {
		return "-"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func dataprocElapsed(start, end time.Time) string {
	if start.IsZero() {
		return "-"
	}
	if end.IsZero() {
		end = time.Now()
	}
	d := end.Sub(start).Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%d hr %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%d min %d sec", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d sec", int(d.Seconds()))
}

func dataprocLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+": "+m[k])
	}
	return kmsOrDash(strings.Join(out, ", "))
}

// dataprocPairs reads "key=value" lines (or commas) into a map.
func dataprocPairs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%q is not key=value", line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func dataprocLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func dataprocURIs(s string) []string {
	var out []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func dataprocPropsText(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, "\n")
}

func dataprocRegionOf(name string) string {
	m := strings.Split(name, "/")
	if len(m) >= 4 {
		return m[3]
	}
	return "-"
}

// dataprocOwned is name's project check, as every console page has.
func dataprocOwned(project, name string) error {
	if project != "" && !strings.HasPrefix(name, "projects/"+project+"/") {
		return fmt.Errorf("%s is not in project %s", name, project)
	}
	return nil
}

func dataprocRuntimeField(section string, def string) console.Field {
	opts := append([]string(nil), dataproc.RuntimeVersions...)
	labels := map[string]string{}
	for _, v := range opts {
		labels[v] = v
	}
	labels[dataproc.DefaultRuntime] = dataproc.DefaultRuntime + " (default)"
	if def == "" {
		def = dataproc.DefaultRuntime
	}
	return console.Field{Name: "runtimeVersion", Label: "Version", Type: "select", Section: section, Options: opts, OptionLabels: labels, Default: def,
		Help: "The Serverless for Apache Spark runtime: its Spark, Java, Scala and Python versions."}
}

// ---- Batches ----

type dataprocBatchesProvider struct{ svc *dataprocService }

func (dataprocBatchesProvider) ID() string    { return "dataproc-batches" }
func (dataprocBatchesProvider) Title() string { return "Batches" }

func (p dataprocBatchesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Status", "Region", "Creation time", "Elapsed time", "Type", "Labels"}, NameColumn: "Batch ID", Noun: "batches"}
	if project == "" {
		listing.Prompt = "Serverless for Apache Spark lists a project's batches. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListBatches(ctx, &dataprocpb.ListBatchesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, b := range resp.GetBatches() {
			var end time.Time
			if terminal := b.GetState() == dataprocpb.Batch_SUCCEEDED || b.GetState() == dataprocpb.Batch_FAILED || b.GetState() == dataprocpb.Batch_CANCELLED; terminal {
				end = b.GetStateTime().AsTime()
			}
			listing.Items = append(listing.Items, console.Resource{Name: lastSegment(b.GetName()), Opens: []string{b.GetName()}, Status: b.GetState().String(), Fields: map[string]string{
				"Batch ID": lastSegment(b.GetName()), "Status": dataprocState(b.GetState().String()), "Region": dataprocRegionOf(b.GetName()),
				"Creation time": kmsTime(b.GetCreateTime()), "Elapsed time": dataprocElapsed(b.GetCreateTime().AsTime(), end),
				"Type": dataprocBatchType(b), "Labels": dataprocLabels(b.GetLabels())}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (dataprocBatchesProvider) CreateOnPage() bool  { return true }
func (dataprocBatchesProvider) CreateTitle() string { return "Create batch" }

func (p dataprocBatchesProvider) CreateForm() (string, []console.Field) {
	const info, container, exec, props, labels = "Batch info", "Container", "Execution configuration", "Properties", "Labels"
	return "Create", []console.Field{
		{Name: "batchId", Label: "Batch ID", Type: "text", Section: info, Pattern: `^[a-z0-9\-]{4,63}$`,
			Help: "4 to 63 lower-case letters, numbers and hyphens. Left empty, one is generated."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: info, Options: dataproc.Locations(), Default: "us-central1"},
		{Name: "batchType", Label: "Batch type", Type: "select", Required: true, Section: container, Default: "PYSPARK",
			Options: []string{"SPARK", "PYSPARK", "SPARKR", "SPARKSQL", "NOTEBOOK"}, OptionLabels: dataprocBatchTypes},
		dataprocRuntimeField(container, ""),
		{Name: "mainPythonFile", Label: "Main python file", Type: "text", Section: container, ShowWhen: "batchType=PYSPARK",
			Help: "The gs:// URI of the main .py file, or a file:// path in the runtime image."},
		{Name: "mainR", Label: "Main R file", Type: "text", Section: container, ShowWhen: "batchType=SPARKR", Help: "The gs:// URI of the main .R file."},
		{Name: "notebook", Label: "Notebook file", Type: "text", Section: container, ShowWhen: "batchType=NOTEBOOK",
			Help: "The gs:// URI of a Jupyter notebook (.ipynb); its code cells run in order as the batch's PySpark driver."},
		{Name: "params", Label: "Notebook parameters", Type: "textarea", Section: container, ShowWhen: "batchType=NOTEBOOK",
			Help: "One NAME=value a line, set after the cell tagged parameters."},
		{Name: "queryFile", Label: "Query file", Type: "text", Section: container, ShowWhen: "batchType=SPARKSQL", Help: "The gs:// URI of the Spark SQL script."},
		{Name: "queryVariables", Label: "Query parameters", Type: "textarea", Section: container, ShowWhen: "batchType=SPARKSQL",
			Help: `One NAME=value a line, applied as SET NAME="value"; before the script.`},
		{Name: "mainClass", Label: "Main class or jar", Type: "text", Section: container, ShowWhen: "batchType=SPARK",
			Help: "A main class (with its jar in Jar files), or the gs:// URI of a jar whose manifest names one."},
		{Name: "args", Label: "Arguments", Type: "textarea", Section: container, ShowWhen: "batchType=SPARK|PYSPARK|SPARKR", Help: "One argument a line."},
		{Name: "pyFiles", Label: "Additional python files", Type: "textarea", Section: container, ShowWhen: "batchType=PYSPARK|NOTEBOOK", Help: "gs:// URIs, one a line."},
		{Name: "jarFiles", Label: "Jar files", Type: "textarea", Section: container, ShowWhen: "batchType=SPARK|PYSPARK|SPARKSQL", Help: "gs:// URIs, one a line."},
		{Name: "files", Label: "Files", Type: "textarea", Section: container, ShowWhen: "batchType=SPARK|PYSPARK|SPARKR", Help: "gs:// URIs placed in each executor's working directory."},
		{Name: "archives", Label: "Archive files", Type: "textarea", Section: container, ShowWhen: "batchType=SPARK|PYSPARK|SPARKR", Help: ".jar, .tar, .tar.gz, .tgz and .zip gs:// URIs, extracted in each executor's working directory."},
		{Name: "serviceAccount", Label: "Service account", Type: "text", Section: exec,
			Help: "The service account the batch runs as. Left empty, the Compute Engine default service account."},
		{Name: "kmsKey", Label: "Customer-managed encryption key (CMEK)", Type: "text", Section: exec, Help: "projects/P/locations/L/keyRings/R/cryptoKeys/K, or empty for a Google-managed key."},
		{Name: "properties", Label: "Properties", Type: "textarea", Section: props, Help: "One spark.property=value a line."},
		{Name: "labels", Label: "Labels", Type: "textarea", Section: labels, Help: "One key=value a line; at most 32."},
	}
}

func (p dataprocBatchesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a batch")
	}
	b := &dataprocpb.Batch{RuntimeConfig: &dataprocpb.RuntimeConfig{Version: values["runtimeVersion"]}}
	if b.RuntimeConfig.Properties, err = dataprocPairs(values["properties"]); err != nil {
		return "", fmt.Errorf("properties: %w", err)
	}
	if b.Labels, err = dataprocPairs(values["labels"]); err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	if sa, key := strings.TrimSpace(values["serviceAccount"]), strings.TrimSpace(values["kmsKey"]); sa != "" || key != "" {
		b.EnvironmentConfig = &dataprocpb.EnvironmentConfig{ExecutionConfig: &dataprocpb.ExecutionConfig{ServiceAccount: sa, KmsKey: key}}
	}
	args := dataprocLines(values["args"])
	switch values["batchType"] {
	case "SPARK":
		w := &dataprocpb.SparkBatch{Args: args, JarFileUris: dataprocURIs(values["jarFiles"]), FileUris: dataprocURIs(values["files"]), ArchiveUris: dataprocURIs(values["archives"])}
		if m := strings.TrimSpace(values["mainClass"]); strings.Contains(m, "://") {
			w.Driver = &dataprocpb.SparkBatch_MainJarFileUri{MainJarFileUri: m}
		} else {
			w.Driver = &dataprocpb.SparkBatch_MainClass{MainClass: m}
		}
		b.BatchConfig = &dataprocpb.Batch_SparkBatch{SparkBatch: w}
	case "SPARKR":
		b.BatchConfig = &dataprocpb.Batch_SparkRBatch{SparkRBatch: &dataprocpb.SparkRBatch{MainRFileUri: strings.TrimSpace(values["mainR"]), Args: args,
			FileUris: dataprocURIs(values["files"]), ArchiveUris: dataprocURIs(values["archives"])}}
	case "NOTEBOOK":
		params, err := dataprocPairs(values["params"])
		if err != nil {
			return "", fmt.Errorf("notebook parameters: %w", err)
		}
		b.BatchConfig = &dataprocpb.Batch_PysparkNotebookBatch{PysparkNotebookBatch: &dataprocpb.PySparkNotebookBatch{NotebookFileUri: strings.TrimSpace(values["notebook"]),
			Params: params, PythonFileUris: dataprocURIs(values["pyFiles"])}}
	case "SPARKSQL":
		vars, err := dataprocPairs(values["queryVariables"])
		if err != nil {
			return "", fmt.Errorf("query parameters: %w", err)
		}
		b.BatchConfig = &dataprocpb.Batch_SparkSqlBatch{SparkSqlBatch: &dataprocpb.SparkSqlBatch{QueryFileUri: strings.TrimSpace(values["queryFile"]),
			QueryVariables: vars, JarFileUris: dataprocURIs(values["jarFiles"])}}
	default:
		b.BatchConfig = &dataprocpb.Batch_PysparkBatch{PysparkBatch: &dataprocpb.PySparkBatch{MainPythonFileUri: strings.TrimSpace(values["mainPythonFile"]), Args: args,
			PythonFileUris: dataprocURIs(values["pyFiles"]), JarFileUris: dataprocURIs(values["jarFiles"]), FileUris: dataprocURIs(values["files"]), ArchiveUris: dataprocURIs(values["archives"])}}
	}
	parent := "projects/" + project + "/locations/" + values["region"]
	op, err := s.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: strings.TrimSpace(values["batchId"]), Batch: b})
	if err != nil {
		return "", err
	}
	var md dataprocpb.BatchOperationMetadata
	if err := op.GetMetadata().UnmarshalTo(&md); err != nil {
		return "", err
	}
	return md.GetBatch(), nil
}

func (p dataprocBatchesProvider) batch(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.Batch, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	b, err := s.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: name})
	return s, b, err
}

// dataprocOutput is a batch's driver output, read from its output URI in
// the instance's Cloud Storage.
func (p dataprocBatchesProvider) output(ctx context.Context, b *dataprocpb.Batch) string {
	uri := b.GetRuntimeInfo().GetOutputUri()
	if uri == "" {
		return "The batch has no output yet."
	}
	if p.svc.live == nil || p.svc.live()["storage"] == "" {
		return "Cloud Storage is not served by this instance, so the driver output at " + uri + " cannot be read."
	}
	st := &dataprocStorage{live: p.svc.live}
	c, err := st.client(ctx)
	if err != nil {
		return consoleErr(err)
	}
	defer c.Close()
	bucket, prefix, err := dataprocGS(uri)
	if err != nil {
		return consoleErr(err)
	}
	r, err := c.Bucket(bucket).Object(prefix + ".000000000").NewReader(ctx)
	if err != nil {
		if b.GetState() == dataprocpb.Batch_RUNNING || b.GetState() == dataprocpb.Batch_PENDING {
			return "The driver output is written to " + uri + " when the batch ends."
		}
		return "Cannot read the driver output at " + uri + ": " + consoleErr(err)
	}
	defer r.Close()
	out, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	if len(out) == 0 {
		return "The driver wrote no output."
	}
	return string(out)
}

func (p dataprocBatchesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, b, err := p.batch(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the batch: " + consoleErr(err)}, nil
	}
	var end time.Time
	if st := b.GetState(); st == dataprocpb.Batch_SUCCEEDED || st == dataprocpb.Batch_FAILED || st == dataprocpb.Batch_CANCELLED {
		end = b.GetStateTime().AsTime()
	}
	rc, ec := b.GetRuntimeConfig(), b.GetEnvironmentConfig().GetExecutionConfig()
	workload := []console.Property{{Label: "Batch type", Value: dataprocBatchType(b)}}
	switch w := b.GetBatchConfig().(type) {
	case *dataprocpb.Batch_PysparkBatch:
		workload = append(workload, console.Property{Label: "Main python file", Value: w.PysparkBatch.GetMainPythonFileUri()},
			console.Property{Label: "Arguments", Value: kmsOrDash(strings.Join(w.PysparkBatch.GetArgs(), " "))},
			console.Property{Label: "Additional python files", Value: kmsOrDash(strings.Join(w.PysparkBatch.GetPythonFileUris(), ", "))})
	case *dataprocpb.Batch_SparkBatch:
		main := w.SparkBatch.GetMainClass()
		if main == "" {
			main = w.SparkBatch.GetMainJarFileUri()
		}
		workload = append(workload, console.Property{Label: "Main class or jar", Value: main},
			console.Property{Label: "Arguments", Value: kmsOrDash(strings.Join(w.SparkBatch.GetArgs(), " "))},
			console.Property{Label: "Jar files", Value: kmsOrDash(strings.Join(w.SparkBatch.GetJarFileUris(), ", "))})
	case *dataprocpb.Batch_SparkRBatch:
		workload = append(workload, console.Property{Label: "Main R file", Value: w.SparkRBatch.GetMainRFileUri()},
			console.Property{Label: "Arguments", Value: kmsOrDash(strings.Join(w.SparkRBatch.GetArgs(), " "))})
	case *dataprocpb.Batch_PysparkNotebookBatch:
		workload = append(workload, console.Property{Label: "Notebook file", Value: w.PysparkNotebookBatch.GetNotebookFileUri()},
			console.Property{Label: "Notebook parameters", Value: dataprocLabels(w.PysparkNotebookBatch.GetParams())})
	case *dataprocpb.Batch_SparkSqlBatch:
		workload = append(workload, console.Property{Label: "Query file", Value: w.SparkSqlBatch.GetQueryFileUri()},
			console.Property{Label: "Query parameters", Value: dataprocLabels(w.SparkSqlBatch.GetQueryVariables())})
	}
	history := console.Listing{Columns: []string{"State", "Message", "Started"}, NameColumn: "#", Noun: "states"}
	for i, h := range b.GetStateHistory() {
		history.Items = append(history.Items, console.Resource{Name: fmt.Sprint(i + 1), Fields: map[string]string{
			"State": dataprocState(h.GetState().String()), "Message": kmsOrDash(h.GetStateMessage()), "Started": kmsTime(h.GetStateStartTime())}})
	}
	history.Items = append(history.Items, console.Resource{Name: fmt.Sprint(len(history.Items) + 1), Fields: map[string]string{
		"State": dataprocState(b.GetState().String()), "Message": kmsOrDash(b.GetStateMessage()), "Started": kmsTime(b.GetStateTime())}})
	history.Total = len(history.Items)
	project, region, id := strings.Split(b.GetName(), "/")[1], dataprocRegionOf(b.GetName()), lastSegment(b.GetName())
	filter := fmt.Sprintf("resource.type=\"cloud_dataproc_batch\"\nresource.labels.project_id=%q\nresource.labels.location=%q\nresource.labels.batch_id=%q", project, region, id)
	usage := b.GetRuntimeInfo().GetApproximateUsage()
	return console.Detail{
		Summary: []console.Property{{Label: "Status", Value: dataprocState(b.GetState().String())}, {Label: "Region", Value: region}, {Label: "Type", Value: dataprocBatchType(b)}},
		Sections: []console.Section{
			{ID: "summary", Label: "Summary", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Batch", Properties: []console.Property{
					{Label: "Batch ID", Value: id}, {Label: "Batch UUID", Value: b.GetUuid(), Copy: "Copy UUID"}, {Label: "Resource name", Value: b.GetName()},
					{Label: "Status", Value: dataprocState(b.GetState().String())}, {Label: "Status message", Value: kmsOrDash(b.GetStateMessage())},
					{Label: "Creator", Value: kmsOrDash(b.GetCreator())}, {Label: "Creation time", Value: kmsTime(b.GetCreateTime())},
					{Label: "Elapsed time", Value: dataprocElapsed(b.GetCreateTime().AsTime(), end)}, {Label: "Labels", Value: dataprocLabels(b.GetLabels())}}},
				{Heading: "Workload", Properties: workload},
				{Heading: "Runtime", Properties: []console.Property{
					{Label: "Version", Value: kmsOrDash(rc.GetVersion())}, {Label: "Container image", Value: kmsOrDash(rc.GetContainerImage())},
					{Label: "Properties", Value: kmsOrDash(strings.ReplaceAll(dataprocPropsText(rc.GetProperties()), "\n", ", "))},
					{Label: "Output URI", Value: kmsOrDash(b.GetRuntimeInfo().GetOutputUri()), Copy: "Copy URI"},
					dataprocSparkUI(p.svc, b.GetUuid(), b.GetState() != dataprocpb.Batch_PENDING, rc.GetProperties()),
					{Label: "Approximate usage", Value: fmt.Sprintf("%.3f DCU-hours, %.3f GB-hours of shuffle storage",
						float64(usage.GetMilliDcuSeconds())/3.6e6, float64(usage.GetShuffleStorageGbSeconds())/3600)}}},
				{Heading: "Execution configuration", Properties: []console.Property{
					{Label: "Service account", Value: kmsOrDash(ec.GetServiceAccount())}, {Label: "Encryption key", Value: kmsOrDash(ec.GetKmsKey())},
					{Label: "Staging bucket", Value: kmsOrDash(ec.GetStagingBucket())}}}}},
			{ID: "states", Label: "State history", Listing: history},
			{ID: "output", Label: "Output", Kind: console.KindText, Text: p.output(ctx, b)},
			{ID: "joblogs", Label: "Job logs", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Cloud Logging", Properties: []console.Property{
					{Label: "Logs", Value: "dataproc.googleapis.com/output (the driver's output) and dataproc.googleapis.com/spark (Spark's own lines, labelled by component)"},
					{Label: "Filter", Value: filter, Copy: "Copy filter"},
					{Label: "Logs Explorer", Value: "Open in Logs Explorer", Link: "/logs?project=" + project}}}}},
		},
	}, nil
}

func (p dataprocBatchesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, b, err := p.batch(ctx, project, path[0])
	if err != nil {
		return nil
	}
	cancel := console.Action{ID: "cancel", Label: "Cancel", Destructive: true, Confirm: "The batch's Spark application is stopped and the batch ends Cancelled."}
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The batch and its record are deleted; its output in Cloud Storage stays."}
	switch b.GetState() {
	case dataprocpb.Batch_PENDING, dataprocpb.Batch_RUNNING:
		del.Unavailable = "A batch can be deleted once it has ended: cancel it first."
	default:
		cancel.Unavailable = "The batch has ended."
	}
	return []console.Action{cancel, del}
}

func (p dataprocBatchesProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return errors.New("no batch")
	}
	s, b, err := p.batch(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "cancel":
		return s.CancelBatch(ctx, b.GetName())
	case "delete":
		_, err := s.DeleteBatch(ctx, &dataprocpb.DeleteBatchRequest{Name: b.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Interactive sessions ----

type dataprocSessionsProvider struct{ svc *dataprocService }

func (dataprocSessionsProvider) ID() string    { return "dataproc-sessions" }
func (dataprocSessionsProvider) Title() string { return "Interactive sessions" }

func (p dataprocSessionsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Status", "Region", "Type", "Creator", "Creation time", "Elapsed time", "Template"}, NameColumn: "Session ID", Noun: "sessions"}
	if project == "" {
		listing.Prompt = "Serverless for Apache Spark lists a project's interactive sessions. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListSessions(ctx, &dataprocpb.ListSessionsRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, x := range resp.GetSessions() {
			var end time.Time
			if x.GetState() == dataprocpb.Session_TERMINATED || x.GetState() == dataprocpb.Session_FAILED {
				end = x.GetStateTime().AsTime()
			}
			listing.Items = append(listing.Items, console.Resource{Name: lastSegment(x.GetName()), Opens: []string{x.GetName()}, Status: x.GetState().String(), Fields: map[string]string{
				"Session ID": lastSegment(x.GetName()), "Status": dataprocState(x.GetState().String()), "Region": dataprocRegionOf(x.GetName()),
				"Type": dataprocSessionType(x), "Creator": kmsOrDash(x.GetCreator()), "Creation time": kmsTime(x.GetCreateTime()),
				"Elapsed time": dataprocElapsed(x.GetCreateTime().AsTime(), end), "Template": kmsOrDash(lastSegment(x.GetSessionTemplate()))}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func dataprocSessionType(x interface {
	GetJupyterSession() *dataprocpb.JupyterConfig
}) string {
	if x.GetJupyterSession() != nil {
		return "Jupyter"
	}
	return "Spark Connect"
}

func (dataprocSessionsProvider) CreateOnPage() bool  { return true }
func (dataprocSessionsProvider) CreateTitle() string { return "Create interactive session" }

func (p dataprocSessionsProvider) CreateForm() (string, []console.Field) {
	const info, runtime, exec, props = "Session info", "Runtime configuration", "Execution configuration", "Properties"
	return "Create", []console.Field{
		{Name: "sessionId", Label: "Name", Type: "text", Required: true, Section: info, Pattern: `^[a-z0-9\-]{4,63}$`,
			Help: "4 to 63 lower-case letters, numbers and hyphens."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: info, Options: dataproc.Locations(), Default: "us-central1"},
		{Name: "sessionType", Label: "Session type", Type: "radio", Section: info, Default: "SPARK_CONNECT", Options: []string{"SPARK_CONNECT", "JUPYTER"},
			OptionLabels: map[string]string{"SPARK_CONNECT": "Spark Connect", "JUPYTER": "Jupyter (Python kernel)"}},
		{Name: "displayName", Label: "Kernel display name", Type: "text", Section: info, ShowWhen: "sessionType=JUPYTER"},
		{Name: "template", Label: "Session template", Type: "text", Section: info,
			Help: "projects/P/locations/L/sessionTemplates/T, in the session's region; its settings apply where the session sets none."},
		dataprocRuntimeField(runtime, "3.0"),
		{Name: "serviceAccount", Label: "Service account", Type: "text", Section: exec},
		{Name: "idleTtl", Label: "Max idle time", Type: "text", Section: exec, Help: "Minutes without activity before the session terminates; 10 to 20160 (default 60)."},
		{Name: "ttl", Label: "Max session time", Type: "text", Section: exec, Help: "Minutes the session may run; 10 to 20160, empty for no limit."},
		{Name: "properties", Label: "Properties", Type: "textarea", Section: props, Help: "One spark.property=value a line."},
		{Name: "labels", Label: "Labels", Type: "textarea", Section: props, Help: "One key=value a line; at most 32."},
	}
}

func dataprocMinutes(s string) (*durationpb.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return nil, fmt.Errorf("%q is not a number of minutes", s)
	}
	return durationpb.New(time.Duration(n) * time.Minute), nil
}

func (p dataprocSessionsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a session")
	}
	x := &dataprocpb.Session{SessionTemplate: strings.TrimSpace(values["template"]),
		SessionConfig: &dataprocpb.Session_SparkConnectSession{SparkConnectSession: &dataprocpb.SparkConnectConfig{}}}
	if values["sessionType"] == "JUPYTER" {
		x.SessionConfig = &dataprocpb.Session_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{Kernel: dataprocpb.JupyterConfig_PYTHON,
			DisplayName: strings.TrimSpace(values["displayName"])}}
	}
	if x.SessionTemplate == "" || values["runtimeVersion"] != "" {
		x.RuntimeConfig = &dataprocpb.RuntimeConfig{Version: values["runtimeVersion"]}
	}
	if x.SessionTemplate != "" && x.RuntimeConfig != nil && x.RuntimeConfig.Version == "3.0" {
		// The form's default leaves a template's version be.
		x.RuntimeConfig = nil
	}
	props, err := dataprocPairs(values["properties"])
	if err != nil {
		return "", fmt.Errorf("properties: %w", err)
	}
	if props != nil {
		if x.RuntimeConfig == nil {
			x.RuntimeConfig = &dataprocpb.RuntimeConfig{}
		}
		x.RuntimeConfig.Properties = props
	}
	if x.Labels, err = dataprocPairs(values["labels"]); err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	ec := &dataprocpb.ExecutionConfig{ServiceAccount: strings.TrimSpace(values["serviceAccount"])}
	if ec.IdleTtl, err = dataprocMinutes(values["idleTtl"]); err != nil {
		return "", err
	}
	if ttl, err := dataprocMinutes(values["ttl"]); err != nil {
		return "", err
	} else {
		ec.Ttl = ttl
	}
	if ec.ServiceAccount != "" || ec.IdleTtl != nil || ec.Ttl != nil {
		x.EnvironmentConfig = &dataprocpb.EnvironmentConfig{ExecutionConfig: ec}
	}
	parent := "projects/" + project + "/locations/" + values["region"]
	id := strings.TrimSpace(values["sessionId"])
	if _, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: id, Session: x}); err != nil {
		return "", err
	}
	return parent + "/sessions/" + id, nil
}

func (p dataprocSessionsProvider) session(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.Session, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	x, err := s.GetSession(dataproc.WithAuthority(ctx, p.svc.Addr()), &dataprocpb.GetSessionRequest{Name: name})
	return s, x, err
}

func (p dataprocSessionsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, x, err := p.session(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the session: " + consoleErr(err)}, nil
	}
	var end time.Time
	if x.GetState() == dataprocpb.Session_TERMINATED || x.GetState() == dataprocpb.Session_FAILED {
		end = x.GetStateTime().AsTime()
	}
	rc, ec := x.GetRuntimeConfig(), x.GetEnvironmentConfig().GetExecutionConfig()
	endpoint := x.GetRuntimeInfo().GetEndpoints()[dataproc.ConnectEndpoint]
	conn := []console.Property{{Label: dataproc.ConnectEndpoint, Value: kmsOrDash(endpoint), Copy: "Copy endpoint"}}
	if x.GetJupyterSession() != nil && x.GetState() == dataprocpb.Session_ACTIVE {
		// A Jupyter session's kernel (#2162): a notebook of BigQuery Studio
		// runs on the console's notebook runtime, whose relay reaches this
		// API's gRPC port as host.docker.internal:8081.
		conn = append(conn, console.Property{Label: "Kernel", Value: "Python (" + kmsOrDash(x.GetJupyterSession().GetDisplayName()) + ")"},
			console.Property{Label: "Use in a notebook", Copy: "Copy", Link: "/bigquery-studio",
				Value: fmt.Sprintf(`from pyspark.sql import SparkSession\nspark = SparkSession.builder.remote("sc://host.docker.internal:8081/;session_id=%s").getOrCreate()`, x.GetUuid())})
	}
	if endpoint != "" {
		conn = append(conn, console.Property{Label: "Connect with PySpark", Copy: "Copy",
			Value: fmt.Sprintf(`SparkSession.builder.remote("%s/;session_id=%s").getOrCreate()`, endpoint, x.GetUuid())})
	}
	dur := func(d *durationpb.Duration) string {
		if d == nil {
			return "-"
		}
		return dataprocElapsed(time.Unix(0, 0), time.Unix(0, 0).Add(d.AsDuration()))
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Status", Value: dataprocState(x.GetState().String())}, {Label: "Region", Value: dataprocRegionOf(x.GetName())}, {Label: "Type", Value: dataprocSessionType(x)}},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Session", Properties: []console.Property{
					{Label: "Name", Value: lastSegment(x.GetName())}, {Label: "UUID", Value: x.GetUuid(), Copy: "Copy UUID"}, {Label: "Resource name", Value: x.GetName()},
					{Label: "Status", Value: dataprocState(x.GetState().String())}, {Label: "Status message", Value: kmsOrDash(x.GetStateMessage())},
					{Label: "Creator", Value: kmsOrDash(x.GetCreator())}, {Label: "User", Value: kmsOrDash(x.GetUser())},
					{Label: "Creation time", Value: kmsTime(x.GetCreateTime())}, {Label: "Elapsed time", Value: dataprocElapsed(x.GetCreateTime().AsTime(), end)},
					{Label: "Template", Value: kmsOrDash(x.GetSessionTemplate())}, {Label: "Labels", Value: dataprocLabels(x.GetLabels())}}},
				{Heading: "Connection", Properties: conn},
				{Heading: "Runtime", Properties: []console.Property{
					{Label: "Version", Value: kmsOrDash(rc.GetVersion())},
					{Label: "Properties", Value: kmsOrDash(strings.ReplaceAll(dataprocPropsText(rc.GetProperties()), "\n", ", "))},
					dataprocSparkUI(p.svc, x.GetUuid(), x.GetState() != dataprocpb.Session_CREATING, rc.GetProperties())}},
				{Heading: "Execution configuration", Properties: []console.Property{
					{Label: "Service account", Value: kmsOrDash(ec.GetServiceAccount())},
					{Label: "Max idle time", Value: dur(ec.GetIdleTtl())}, {Label: "Max session time", Value: dur(ec.GetTtl())}}}}},
		},
	}, nil
}

func (p dataprocSessionsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, x, err := p.session(ctx, project, path[0])
	if err != nil {
		return nil
	}
	term := console.Action{ID: "terminate", Label: "Terminate", Destructive: true, Confirm: "The session's Spark Connect server stops; its clients lose their connection."}
	if st := x.GetState(); st == dataprocpb.Session_TERMINATED || st == dataprocpb.Session_TERMINATING || st == dataprocpb.Session_FAILED {
		term.Unavailable = "The session has ended."
	}
	return []console.Action{term,
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The session is terminated, if it runs, and deleted."}}
}

func (p dataprocSessionsProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return errors.New("no session")
	}
	s, x, err := p.session(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "terminate":
		_, err := s.TerminateSession(ctx, &dataprocpb.TerminateSessionRequest{Name: x.GetName()})
		return err
	case "delete":
		_, err := s.DeleteSession(ctx, &dataprocpb.DeleteSessionRequest{Name: x.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Session templates ----

type dataprocTemplatesProvider struct{ svc *dataprocService }

func (dataprocTemplatesProvider) ID() string    { return "dataproc-templates" }
func (dataprocTemplatesProvider) Title() string { return "Session templates" }

func (p dataprocTemplatesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Region", "Type", "Runtime version", "Creator", "Last updated"}, NameColumn: "Name", Noun: "session templates"}
	if project == "" {
		listing.Prompt = "Serverless for Apache Spark lists a project's session templates. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListSessionTemplates(ctx, &dataprocpb.ListSessionTemplatesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, t := range resp.GetSessionTemplates() {
			listing.Items = append(listing.Items, console.Resource{Name: lastSegment(t.GetName()), Opens: []string{t.GetName()}, Fields: map[string]string{
				"Name": lastSegment(t.GetName()), "Region": dataprocRegionOf(t.GetName()), "Type": dataprocSessionType(t),
				"Runtime version": kmsOrDash(t.GetRuntimeConfig().GetVersion()), "Creator": kmsOrDash(t.GetCreator()), "Last updated": kmsTime(t.GetUpdateTime())}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (dataprocTemplatesProvider) CreateOnPage() bool  { return true }
func (dataprocTemplatesProvider) CreateTitle() string { return "Create session template" }

func dataprocTemplateFields(t *dataprocpb.SessionTemplate, withID bool) []console.Field {
	const info, runtime, exec, props = "Template info", "Runtime configuration", "Execution configuration", "Properties"
	var fields []console.Field
	if withID {
		fields = append(fields,
			console.Field{Name: "templateId", Label: "Name", Type: "text", Required: true, Section: info, Pattern: `^[a-zA-Z0-9]([a-zA-Z0-9_\-]{0,62}[a-zA-Z0-9])?$`,
				Help: "Letters, numbers, hyphens and underscores, starting and ending with a letter or number."},
			console.Field{Name: "region", Label: "Region", Type: "select", Required: true, Section: info, Options: dataproc.Locations(), Default: "us-central1"})
	}
	ec := t.GetEnvironmentConfig().GetExecutionConfig()
	mins := func(d *durationpb.Duration) string {
		if d == nil {
			return ""
		}
		return fmt.Sprint(int(d.AsDuration().Minutes()))
	}
	return append(fields,
		console.Field{Name: "description", Label: "Description", Type: "textarea", Section: info, Default: t.GetDescription()},
		dataprocRuntimeField(runtime, t.GetRuntimeConfig().GetVersion()),
		console.Field{Name: "serviceAccount", Label: "Service account", Type: "text", Section: exec, Default: ec.GetServiceAccount()},
		console.Field{Name: "idleTtl", Label: "Max idle time", Type: "text", Section: exec, Default: mins(ec.GetIdleTtl()), Help: "Minutes; 10 to 20160 (default 60)."},
		console.Field{Name: "ttl", Label: "Max session time", Type: "text", Section: exec, Default: mins(ec.GetTtl()), Help: "Minutes; 10 to 20160, empty for no limit."},
		console.Field{Name: "properties", Label: "Properties", Type: "textarea", Section: props, Default: dataprocPropsText(t.GetRuntimeConfig().GetProperties()),
			Help: "One spark.property=value a line."},
		console.Field{Name: "labels", Label: "Labels", Type: "textarea", Section: props, Default: dataprocPropsText(t.GetLabels()), Help: "One key=value a line."})
}

func dataprocTemplateOf(name string, values map[string]string) (*dataprocpb.SessionTemplate, error) {
	t := &dataprocpb.SessionTemplate{Name: name, Description: values["description"],
		SessionConfig: &dataprocpb.SessionTemplate_SparkConnectSession{SparkConnectSession: &dataprocpb.SparkConnectConfig{}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: values["runtimeVersion"]}}
	var err error
	if t.RuntimeConfig.Properties, err = dataprocPairs(values["properties"]); err != nil {
		return nil, fmt.Errorf("properties: %w", err)
	}
	if t.Labels, err = dataprocPairs(values["labels"]); err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	ec := &dataprocpb.ExecutionConfig{ServiceAccount: strings.TrimSpace(values["serviceAccount"])}
	if ec.IdleTtl, err = dataprocMinutes(values["idleTtl"]); err != nil {
		return nil, err
	}
	if ttl, err := dataprocMinutes(values["ttl"]); err != nil {
		return nil, err
	} else {
		ec.Ttl = ttl
	}
	if ec.ServiceAccount != "" || ec.IdleTtl != nil || ec.Ttl != nil {
		t.EnvironmentConfig = &dataprocpb.EnvironmentConfig{ExecutionConfig: ec}
	}
	return t, nil
}

func (p dataprocTemplatesProvider) CreateForm() (string, []console.Field) {
	return "Create", dataprocTemplateFields(nil, true)
}

func (p dataprocTemplatesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a session template")
	}
	parent := "projects/" + project + "/locations/" + values["region"]
	t, err := dataprocTemplateOf(parent+"/sessionTemplates/"+strings.TrimSpace(values["templateId"]), values)
	if err != nil {
		return "", err
	}
	got, err := s.CreateSessionTemplate(ctx, &dataprocpb.CreateSessionTemplateRequest{Parent: parent, SessionTemplate: t})
	if err != nil {
		return "", err
	}
	return got.GetName(), nil
}

func (p dataprocTemplatesProvider) template(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.SessionTemplate, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	t, err := s.GetSessionTemplate(ctx, &dataprocpb.GetSessionTemplateRequest{Name: name})
	return s, t, err
}

func (p dataprocTemplatesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, t, err := p.template(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the session template: " + consoleErr(err)}, nil
	}
	ec := t.GetEnvironmentConfig().GetExecutionConfig()
	dur := func(d *durationpb.Duration) string {
		if d == nil {
			return "-"
		}
		return dataprocElapsed(time.Unix(0, 0), time.Unix(0, 0).Add(d.AsDuration()))
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: dataprocRegionOf(t.GetName())}, {Label: "Type", Value: dataprocSessionType(t)}},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Template", Properties: []console.Property{
					{Label: "Name", Value: lastSegment(t.GetName())}, {Label: "UUID", Value: kmsOrDash(t.GetUuid())}, {Label: "Resource name", Value: t.GetName()},
					{Label: "Description", Value: kmsOrDash(t.GetDescription())}, {Label: "Creator", Value: kmsOrDash(t.GetCreator())},
					{Label: "Created", Value: kmsTime(t.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(t.GetUpdateTime())},
					{Label: "Labels", Value: dataprocLabels(t.GetLabels())}}},
				{Heading: "Runtime", Properties: []console.Property{
					{Label: "Version", Value: kmsOrDash(t.GetRuntimeConfig().GetVersion())},
					{Label: "Properties", Value: kmsOrDash(strings.ReplaceAll(dataprocPropsText(t.GetRuntimeConfig().GetProperties()), "\n", ", "))}}},
				{Heading: "Execution configuration", Properties: []console.Property{
					{Label: "Service account", Value: kmsOrDash(ec.GetServiceAccount())},
					{Label: "Max idle time", Value: dur(ec.GetIdleTtl())}, {Label: "Max session time", Value: dur(ec.GetTtl())}}}}},
		},
	}, nil
}

func (p dataprocTemplatesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, t, err := p.template(ctx, project, path[0])
	if err != nil {
		return nil
	}
	return []console.Action{
		{ID: "edit", Label: "Edit", Fields: dataprocTemplateFields(t, false)},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "Sessions made from the template keep running."},
	}
}

func (p dataprocTemplatesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no session template")
	}
	s, t, err := p.template(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "edit":
		nt, err := dataprocTemplateOf(t.GetName(), values)
		if err != nil {
			return err
		}
		_, err = s.UpdateSessionTemplate(ctx, &dataprocpb.UpdateSessionTemplateRequest{SessionTemplate: nt})
		return err
	case "delete":
		_, err := s.DeleteSessionTemplate(ctx, &dataprocpb.DeleteSessionTemplateRequest{Name: t.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// dataprocSparkUI is a batch's or session's View Spark UI: its
// application in the instance's Spark History Server (#2162). Google
// disables it for a workload submitted with
// spark.dataproc.appContext.enabled=false
// (https://docs.cloud.google.com/dataproc-serverless/docs/guides/monitor-troubleshoot-batches,
// read 2026-11-22).
func dataprocSparkUI(svc *dataprocService, uuid string, ran bool, props map[string]string) console.Property {
	if strings.EqualFold(props["spark.dataproc.appContext.enabled"], "false") {
		return console.Property{Label: "Spark UI", Value: "Disabled: the workload was submitted with spark.dataproc.appContext.enabled set to false"}
	}
	if !ran || svc.Addr() == "" {
		return console.Property{Label: "Spark UI", Value: "Available once the workload starts"}
	}
	return console.Property{Label: "Spark UI", Value: "View Spark UI", Link: "http://" + svc.Addr() + dataproc.SparkUIPath(uuid)}
}
