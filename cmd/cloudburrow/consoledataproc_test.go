package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/service/dataproc"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

type consoleSpark struct{}

func (consoleSpark) Run(context.Context, sparkproc.Job) (sparkproc.Result, error) {
	return sparkproc.Result{DriverOutput: "done\n"}, nil
}

type consoleConn struct{ done chan struct{} }

func (c consoleConn) Address() string          { return "127.0.0.1:1" }
func (c consoleConn) Done() <-chan struct{}    { return c.done }
func (c consoleConn) Result() sparkproc.Result { return sparkproc.Result{} }
func (c consoleConn) Stop()                    {}

func (consoleSpark) StartConnect(context.Context, sparkproc.Job) (dataproc.Conn, error) {
	return consoleConn{done: make(chan struct{})}, nil
}

// TestConsoleDataproc (#2156): Serverless for Apache Spark's pages'
// providers: Create batch submits a PySpark batch with its arguments,
// properties and labels, listed and opened with its Summary, State
// history, Output and Job logs, then deleted; a session template is
// created and edited, a session made from it is listed with its Spark
// Connect endpoint, terminated and deleted.
func TestConsoleDataproc(t *testing.T) {
	ctx := context.Background()
	const project = "dp-console"
	api, err := dataproc.NewServer(dataproc.Options{Engine: consoleSpark{}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	// A bound port, which a session's Spark Connect endpoint names.
	srv := grpctransport.New("127.0.0.1:0")
	if err := srv.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop(ctx) }()
	svc := &dataprocService{api: api, server: srv}
	batches, sessions, templates := dataprocBatchesProvider{svc: svc}, dataprocSessionsProvider{svc: svc}, dataprocTemplatesProvider{svc: svc}

	name, err := batches.Create(ctx, project, map[string]string{"batchId": "wordcount", "region": "us-central1", "batchType": "PYSPARK",
		"runtimeVersion": "2.2", "mainPythonFile": "file:///tmp/x.py", "args": "a\nb", "properties": "spark.executor.instances=2", "labels": "env=dev"})
	if err != nil || name != "projects/"+project+"/locations/us-central1/batches/wordcount" {
		t.Fatalf("Create batch: %q %v", name, err)
	}
	waitFor := func(cond func() bool) {
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
		}
	}
	waitFor(func() bool {
		l, _ := batches.List(ctx, project)
		return len(l.Items) == 1 && l.Items[0].Fields["Status"] == "Succeeded"
	})
	l, _ := batches.List(ctx, project)
	if f := l.Items[0].Fields; f["Type"] != "PySpark" || f["Labels"] != "env: dev" || f["Region"] != "us-central1" {
		t.Errorf("the batch's row: %v", f)
	}
	d, err := batches.Detail(ctx, project, []string{name})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 4 {
		t.Fatalf("the batch's page: %+v %v", d, err)
	}
	if !strings.Contains(d.Sections[3].Groups[0].Properties[1].Value, `resource.labels.batch_id="wordcount"`) {
		t.Errorf("Job logs' filter: %+v", d.Sections[3])
	}
	acts := map[string]string{}
	for _, a := range batches.DetailActions(ctx, project, []string{name}) {
		acts[a.ID] = a.Unavailable
	}
	if acts["cancel"] == "" || acts["delete"] != "" {
		t.Errorf("a finished batch's actions: %v", acts)
	}
	if err := batches.ActAt(ctx, project, []string{name}, "delete", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := batches.Create(ctx, project, map[string]string{"region": "us-central1", "batchType": "SPARKSQL", "queryFile": "file:///q.sql",
		"queryVariables": "nope"}); err == nil {
		t.Error("a query parameter that is not NAME=value was taken")
	}

	tn, err := templates.Create(ctx, project, map[string]string{"templateId": "connect", "region": "us-central1", "runtimeVersion": "3.0", "idleTtl": "30"})
	if err != nil {
		t.Fatalf("Create session template: %v", err)
	}
	if err := templates.ActAt(ctx, project, []string{tn}, "edit", map[string]string{"runtimeVersion": "3.0", "description": "edited", "idleTtl": "45"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if td, _ := templates.Detail(ctx, project, []string{tn}); td.Sections[0].Groups[0].Properties[3].Value != "edited" {
		t.Errorf("the edited template: %+v", td.Sections[0].Groups[0])
	}
	sn, err := sessions.Create(ctx, project, map[string]string{"sessionId": "sess-1", "region": "us-central1", "template": tn})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	waitFor(func() bool {
		l, _ := sessions.List(ctx, project)
		return len(l.Items) == 1 && l.Items[0].Fields["Status"] == "Active"
	})
	sd, err := sessions.Detail(ctx, project, []string{sn})
	if err != nil || !strings.HasPrefix(sd.Sections[0].Groups[1].Properties[0].Value, "sc://") {
		t.Errorf("the session's Connection: %+v %v", sd, err)
	}
	if err := sessions.ActAt(ctx, project, []string{sn}, "terminate", nil); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	waitFor(func() bool {
		l, _ := sessions.List(ctx, project)
		return len(l.Items) == 1 && l.Items[0].Fields["Status"] == "Terminated"
	})
	if err := sessions.ActAt(ctx, project, []string{sn}, "delete", nil); err != nil {
		t.Fatalf("Delete session: %v", err)
	}
	if err := templates.ActAt(ctx, project, []string{tn}, "delete", nil); err != nil {
		t.Fatalf("Delete template: %v", err)
	}
}
