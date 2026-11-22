package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"

	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// TestDataPrepSQLX (#2105): a data preparation's steps become one
// pipe-syntax query in a SQLX dataPreparation action, with its
// destination, error table and load mode in its config.
func TestDataPrepSQLX(t *testing.T) {
	m := studioDataPrep{
		Source: "sales.orders",
		Steps: []studioPrepStep{
			{Type: "filter", Expression: "amount > 0"},
			{Type: "transform", Column: "total", Expression: "amount * qty"},
			{Type: "transform", Column: "name", Expression: "UPPER(name)", Replace: true},
			{Type: "join", Table: "cp.sales.customers", JoinType: "left", Expression: "orders.customer_id = customers.id"},
			{Type: "aggregate", Expression: "SUM(total) AS revenue", Keys: "region, name"},
			{Type: "deduplicate"},
			{Type: "delete", Columns: "name"},
		},
		Destination: "reports.revenue",
		LoadMode:    "unique",
		LoadColumn:  "region",
		ErrorTable:  "reports.revenue_errors",
		Retention:   7,
	}
	got, err := dataPrepSQLX("cp", m, []string{"load"})
	if err != nil {
		t.Fatal(err)
	}
	want := "config {\n" +
		"  type: \"dataPreparation\",\n" +
		"  project: \"cp\",\n" +
		"  dataset: \"reports\",\n" +
		"  name: \"revenue\",\n" +
		"  errorTable: { project: \"cp\", dataset: \"reports\", name: \"revenue_errors\", retentionDays: 7 },\n" +
		"  loadMode: { mode: \"UNIQUE\", columnName: \"region\" },\n" +
		"  dependencies: [\"load\"]\n" +
		"}\n\n" +
		"FROM `cp.sales.orders`\n" +
		"|> WHERE amount > 0\n" +
		"|> EXTEND amount * qty AS `total`\n" +
		"|> SET `name` = UPPER(name)\n" +
		"|> LEFT JOIN `cp.sales.customers` ON orders.customer_id = customers.id\n" +
		"|> AGGREGATE SUM(total) AS revenue GROUP BY `region`, `name`\n" +
		"|> SELECT DISTINCT *\n" +
		"|> DROP `name`\n"
	if got != want {
		t.Errorf("SQLX =\n%s\nwant\n%s", got, want)
	}
	for _, bad := range []studioDataPrep{
		{Source: "orders", Destination: "d.t"},
		{Source: "d.t", Destination: "d.u", LoadMode: "MERGE"},
		{Source: "d.t", Destination: "d.u", LoadMode: "MAXIMUM"},
		{Source: "d.t", Destination: "d.u", Steps: []studioPrepStep{{Type: "filter"}}},
		{Source: "d.t", Destination: "d.u", Steps: []studioPrepStep{{Type: "pivot"}}},
		{Source: "d.t", Destination: "d.u", Steps: []studioPrepStep{{Type: "delete", Columns: "a`b"}}},
	} {
		if _, err := dataPrepSQLX("cp", bad, nil); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

// TestPipelineFiles (#2105): a pipeline's tasks are Dataform actions:
// notebooks in definitions/actions.yaml, queries and data preparations as
// .sqlx files, each task's Run after its dependencies.
func TestPipelineFiles(t *testing.T) {
	m := studioPipeline{
		Settings: studioPipelineSettings{DefaultDataset: "work", Bucket: "gs://out"},
		Tasks: []studioPipelineTask{
			{Name: "load", Type: "sql", SQL: "CREATE OR REPLACE TABLE work.t AS SELECT 1 AS x"},
			{Name: "explore", Type: "notebook", RunAfter: []string{"load"}, Notebook: `{"cells":[]}`},
			{Name: "prep", Type: "data_preparation", RunAfter: []string{"load"},
				DataPreparation: &studioDataPrep{Source: "work.t", Destination: "work.u", LoadMode: "APPEND"}},
		},
	}
	files, err := pipelineFiles("cp", m)
	if err != nil {
		t.Fatal(err)
	}
	if got := files["definitions/load.sqlx"]; got != "config { type: \"operations\" }\n\nCREATE OR REPLACE TABLE work.t AS SELECT 1 AS x\n" {
		t.Errorf("load.sqlx = %q", got)
	}
	if got := files["definitions/actions.yaml"]; got != "actions:\n  - notebook:\n      name: \"explore\"\n      filename: \"explore.ipynb\"\n      dependencyTargets:\n        - name: \"load\"\n" {
		t.Errorf("actions.yaml = %q", got)
	}
	if files["definitions/explore.ipynb"] != `{"cells":[]}` {
		t.Errorf("explore.ipynb = %q", files["definitions/explore.ipynb"])
	}
	if !strings.Contains(files["definitions/prep.sqlx"], `dependencies: ["load"]`) || !strings.Contains(files["definitions/prep.sqlx"], `mode: "APPEND"`) {
		t.Errorf("prep.sqlx = %q", files["definitions/prep.sqlx"])
	}
	ws := files["workflow_settings.yaml"]
	for _, want := range []string{`defaultProject: "cp"`, `defaultDataset: "work"`, `dataformCoreVersion: "3.1.6"`, `outputBucket: "gs://out"`} {
		if !strings.Contains(ws, want) {
			t.Errorf("workflow_settings.yaml lacks %s:\n%s", want, ws)
		}
	}
	for _, bad := range []studioPipeline{
		{Tasks: []studioPipelineTask{{Name: "a.b", Type: "sql", SQL: "SELECT 1"}}},
		{Tasks: []studioPipelineTask{{Name: "a", Type: "sql", SQL: "SELECT 1"}, {Name: "a", Type: "sql", SQL: "SELECT 2"}}},
		{Tasks: []studioPipelineTask{{Name: "a", Type: "sql", SQL: "SELECT 1", RunAfter: []string{"b"}}}},
		{Tasks: []studioPipelineTask{{Name: "a", Type: "sql", SQL: "SELECT 1", RunAfter: []string{"a"}}}},
		{Tasks: []studioPipelineTask{{Name: "a", Type: "sql"}}},
		{Tasks: []studioPipelineTask{{Name: "a", Type: "spark"}}},
		{Tasks: []studioPipelineTask{{Name: "a", Type: "notebook", Notebook: "{"}}},
	} {
		if _, err := pipelineFiles("cp", bad); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

func TestStudioScheduleID(t *testing.T) {
	for in, want := range map[string]string{"Daily run": "Daily-run", "a.b_c": "a-b_c", "  x  ": "x"} {
		if got, err := studioScheduleID(in); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := studioScheduleID("!!"); err == nil {
		t.Error("a name of no letters was accepted")
	}
}

// TestStudioPipelineAsset (#2105): saving a pipeline commits its Dataform
// project to main, drops the files of removed tasks, and a schedule is a
// release and a workflow configuration named after it.
func TestStudioPipelineAsset(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	api, err := dataform.NewServer(dataform.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := dataformProvider{svc: &dataformService{api: api}}
	call := func(name string, body any) (map[string]any, error) {
		b, _ := json.Marshal(body)
		out, err := p.StudioCall(ctx, "cp", name, b)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		raw, _ := json.Marshal(out)
		_ = json.Unmarshal(raw, &m)
		return m, nil
	}
	must := func(name string, body any) map[string]any {
		t.Helper()
		m, err := call(name, body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	model := func(tasks ...studioPipelineTask) string {
		b, _ := json.Marshal(studioPipeline{Tasks: tasks})
		return string(b)
	}
	if _, err := call("save", map[string]any{"kind": "pipeline", "displayName": "Bad", "content": model(studioPipelineTask{Name: "a b", Type: "sql", SQL: "SELECT 1"})}); err == nil {
		t.Error("a pipeline with a bad task name was saved")
	}
	files := must("files", nil)["entries"].([]any)
	if len(files) != 0 {
		t.Errorf("a refused save left %v", files)
	}
	pl := must("save", map[string]any{"kind": "pipeline", "displayName": "Nightly", "content": model(
		studioPipelineTask{Name: "one", Type: "sql", SQL: "SELECT 1"},
		studioPipelineTask{Name: "two", Type: "sql", SQL: "SELECT 2", RunAfter: []string{"one"}})})
	name := pl["name"].(string)
	read := func(path string) (string, error) {
		r, err := api.ReadRepositoryFile(ctx, &dfpb.ReadRepositoryFileRequest{Name: name, Path: path})
		return string(r.GetContents()), err
	}
	if got, err := read("definitions/two.sqlx"); err != nil || !strings.Contains(got, `dependencies: ["one"]`) {
		t.Errorf("two.sqlx = %q, %v", got, err)
	}
	if got, err := read("workflow_settings.yaml"); err != nil || !strings.Contains(got, "dataformCoreVersion") {
		t.Errorf("workflow_settings.yaml = %q, %v", got, err)
	}
	must("save", map[string]any{"name": name, "content": model(studioPipelineTask{Name: "one", Type: "sql", SQL: "SELECT 1"})})
	if _, err := read("definitions/two.sqlx"); err == nil {
		t.Error("a removed task's file was kept")
	}
	if got := must("read", map[string]any{"name": name})["content"].(string); !strings.Contains(got, `"one"`) || strings.Contains(got, `"two"`) {
		t.Errorf("the model = %s", got)
	}
	if kinds := must("files", nil)["entries"].([]any); len(kinds) != 1 || kinds[0].(map[string]any)["kind"] != "pipeline" {
		t.Errorf("files = %v", kinds)
	}

	// A schedule: its release and workflow configurations.
	sc, err := call("createschedule", map[string]any{"name": name, "displayName": "Every day", "cron": "0 6 * * *", "timeZone": "Europe/Paris",
		"serviceAccount": "runner@cp.iam.gserviceaccount.com"})
	if err != nil {
		t.Fatalf("createschedule: %v", err)
	}
	if sc["id"] != "Every-day" {
		t.Errorf("schedule = %v", sc)
	}
	list := must("schedules", map[string]any{"name": name})["schedules"].([]any)
	if len(list) != 1 {
		t.Fatalf("schedules = %v", list)
	}
	got := list[0].(map[string]any)
	if got["cron"] != "0 6 * * *" || got["timeZone"] != "Europe/Paris" || got["serviceAccount"] != "runner@cp.iam.gserviceaccount.com" {
		t.Errorf("schedule = %v", got)
	}
	rc, err := api.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: name + "/releaseConfigs/Every-day"})
	if err != nil || rc.GetGitCommitish() != "main" {
		t.Errorf("release config = %v, %v", rc, err)
	}
	must("toggleschedule", map[string]any{"schedule": got["name"], "disabled": true})
	if l := must("schedules", map[string]any{"name": name})["schedules"].([]any); l[0].(map[string]any)["disabled"] != true {
		t.Errorf("disabled schedule = %v", l)
	}
	if ex := must("executions", map[string]any{"name": name})["executions"].([]any); len(ex) != 0 {
		t.Errorf("executions = %v", ex)
	}
	must("deleteschedule", map[string]any{"schedule": got["name"]})
	if l := must("schedules", map[string]any{"name": name})["schedules"].([]any); len(l) != 0 {
		t.Errorf("schedules after delete = %v", l)
	}
	if _, err := api.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: name + "/releaseConfigs/Every-day"}); err == nil {
		t.Error("the schedule's release config was kept")
	}
	if out := must("dataprepsql", map[string]any{"content": `{"source":"d.t","steps":[{"type":"filter","expression":"x > 1"}]}`}); out["sql"] != "FROM `cp.d.t`\n|> WHERE x > 1" {
		t.Errorf("dataprepsql = %v", out)
	}
	// Another project's repository is refused.
	if _, err := call("schedules", map[string]any{"name": "projects/other/locations/us-central1/repositories/x"}); err == nil {
		t.Error("another project's schedules were listed")
	}
}
