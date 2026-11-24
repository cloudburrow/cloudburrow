package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataflow"
)

// fakeDataflowEngine runs a job as a two-step pipeline that counts three
// elements, until it is drained or cancelled (streaming) or at once
// (batch).
type fakeDataflowEngine struct{}

func (fakeDataflowEngine) Run(ctx context.Context, run dataflow.Run, r dataflow.Reporter) dataflow.Outcome {
	r.Graph([]dataflow.Transform{
		{ID: "t1", Name: "Read", Kind: "Read", Outputs: []string{"pc1"}, Stage: "Read"},
		{ID: "t2", Name: "Count", Kind: "Composite", Inputs: []string{"pc1"}, Outputs: []string{"pc2"}, Stage: "Count"},
		{ID: "t3", Name: "Count/Sum", Kind: "ParDo", Parent: "t2", Inputs: []string{"pc1"}, Outputs: []string{"pc2"}, Stage: "Count"},
	})
	r.Started(1)
	r.Metrics([]dataflow.Metric{{Name: "ElementCount", Origin: "dataflow/v1b3", Step: "Read", PCollection: "pc1", Kind: "Sum", Value: 3, Updated: time.Now()}})
	r.Message(dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "read 3 elements")
	if !run.Streaming {
		return dataflow.Outcome{State: dataflowpb.JobState_JOB_STATE_DONE}
	}
	select {
	case <-run.Drain:
		return dataflow.Outcome{State: dataflowpb.JobState_JOB_STATE_DONE}
	case <-ctx.Done():
		return dataflow.Outcome{State: dataflowpb.JobState_JOB_STATE_CANCELLED}
	}
}

// fakeDataflowLauncher stands for a Flex Template's program: it submits
// a batch job of the launch's name.
type fakeDataflowLauncher struct{ api **dataflow.Server }

func (l fakeDataflowLauncher) Launch(ctx context.Context, f dataflow.FlexLaunch) error {
	_, err := (*l.api).CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: f.Project, Location: f.Location,
		Job: &dataflowpb.Job{Name: f.JobName, Type: dataflowpb.JobType_JOB_TYPE_BATCH, Environment: &dataflowpb.Environment{
			SdkPipelineOptions: &structpb.Struct{Fields: map[string]*structpb.Value{"options": structpb.NewStructValue(&structpb.Struct{
				Fields: map[string]*structpb.Value{"pipelineUrl": structpb.NewStringValue("gs://b/p.pb")}})}}}}})
	return err
}

type fakeDataflowStorage map[string]string

func (f fakeDataflowStorage) Read(_ context.Context, uri string) ([]byte, error) {
	if b, ok := f[uri]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("not found")
}

func TestConsoleDataflow(t *testing.T) {
	ctx := context.Background()
	const project = "df-console"
	tmpl := `{"type": "JOB_TYPE_STREAMING", "environment": {"sdkPipelineOptions": {"options": {"pipelineUrl": "gs://b/p.pb", "streaming": true}},
		"userAgent": {"name": "Apache Beam Python 3.12 SDK", "version": "2.78.0"}}}`
	flex := `{"image": "us-central1-docker.pkg.dev/p/r/flex:1", "metadata": {"name": "flex", "parameters": []}}`
	var api *dataflow.Server
	launcher := fakeDataflowLauncher{api: &api}
	api, err := dataflow.NewServer(dataflow.Options{Engine: fakeDataflowEngine{}, Launcher: launcher,
		Storage: fakeDataflowStorage{"gs://b/templates/words": tmpl, "gs://b/templates/flex.json": flex}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := dataflowProvider{svc: &dataflowService{api: api}}

	form := func(over map[string]string) map[string]string {
		v := map[string]string{}
		_, fields := p.CreateForm()
		for _, f := range fields {
			v[f.Name] = f.Default
		}
		for k, x := range over {
			v[k] = x
		}
		return v
	}
	if _, err := p.Create(ctx, project, form(map[string]string{"jobName": "words", "gcsPath": "gs://b/missing", "tempLocation": "gs://b/tmp"})); err == nil {
		t.Error("a job was launched from a template that does not exist")
	}
	name, err := p.Create(ctx, project, form(map[string]string{"jobName": "words", "gcsPath": "gs://b/templates/words",
		"tempLocation": "gs://b/tmp", "maxWorkers": "2"}))
	if err != nil || !strings.HasPrefix(name, "us-central1/") {
		t.Fatalf("Create job from template: %q, %v", name, err)
	}
	var j *dataflowpb.Job
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, j, err = p.job(ctx, project, name)
		if err == nil && j.GetCurrentState() == dataflowpb.JobState_JOB_STATE_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job is %v, %v; want running", j.GetCurrentState(), err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	row := l.Items[0]
	if row.Name != "words" || row.Fields["Type"] != "Streaming" || row.Fields["Status"] != "Running" || row.Fields["Region"] != "us-central1" ||
		row.Fields["SDK version"] != "Apache Beam Python 3.12 SDK 2.78.0" || len(row.Opens) != 1 || row.Opens[0] != name {
		t.Errorf("the Jobs row is %+v", row)
	}

	d, err := p.Detail(ctx, project, []string{name})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	sections := map[string]console.Section{}
	var ids []string
	for _, s := range d.Sections {
		sections[s.ID] = s
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, ","); got != "graph,execution,metrics,autoscaling,info,joblogs,workerlogs" {
		t.Errorf("the job page's tabs are %s", got)
	}
	g := sections["graph"].Pipeline
	if g == nil || len(g.Steps) != 2 || g.Steps[0].Name != "Read" || g.Steps[1].Name != "Count" || len(g.Steps[1].Inputs) != 1 ||
		g.Steps[1].Inputs[0] != "Read" || g.Steps[0].Properties[0].Value != "3" {
		t.Errorf("the job graph is %+v", g)
	}
	if n := len(sections["execution"].Listing.Items); n != 2 {
		t.Errorf("Execution details lists %d stages, want 2", n)
	}
	if n := len(sections["metrics"].Listing.Items); n != 1 {
		t.Errorf("Job metrics lists %d metrics, want 1", n)
	}
	if !strings.Contains(listingText(sections["joblogs"].Listing), "read 3 elements") {
		t.Errorf("Job logs: %+v", sections["joblogs"].Listing)
	}
	if sections["workerlogs"].Listing.Unavailable == "" {
		t.Error("Worker logs without Cloud Logging do not say so")
	}

	acts := p.DetailActions(ctx, project, []string{name})
	if len(acts) != 1 || acts[0].ID != "stop" || strings.Join(acts[0].Fields[0].Options, ",") != "cancel,drain" {
		t.Fatalf("a running streaming job's actions are %+v", acts)
	}
	if err := p.ActAt(ctx, project, []string{name}, "stop", map[string]string{"mode": "drain"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, j, _ = p.job(ctx, project, name)
		if j.GetCurrentState() == dataflowpb.JobState_JOB_STATE_DRAINED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Stop (Drain) the job is %s", j.GetCurrentState())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if acts := p.DetailActions(ctx, project, []string{name}); len(acts) != 0 {
		t.Errorf("a drained job offers %+v", acts)
	}
	// A Custom Template's path that is a Flex Template's spec launches it.
	fname, err := p.Create(ctx, project, form(map[string]string{"jobName": "flexjob", "gcsPath": "gs://b/templates/flex.json", "tempLocation": "gs://b/tmp"}))
	if err != nil {
		t.Fatalf("Create job from a Flex Template: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, j, _ = p.job(ctx, project, fname)
		if j.GetCurrentState() == dataflowpb.JobState_JOB_STATE_DONE && j.GetName() == "flexjob" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Flex Template's job is %s", j.GetCurrentState())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if l, _ := p.List(ctx, ""); l.Prompt == "" {
		t.Error("the Jobs list without a project has no prompt")
	}
}

func listingText(l console.Listing) string {
	var b strings.Builder
	for _, r := range l.Items {
		b.WriteString(r.Name)
		for _, v := range r.Fields {
			b.WriteString(" " + v)
		}
		b.WriteString("\n")
	}
	return b.String()
}
