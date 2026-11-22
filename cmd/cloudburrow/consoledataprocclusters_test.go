package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/service/dataproc"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

type consoleClusterSpark struct{ consoleSpark }

type consoleNodes struct {
	mu      sync.Mutex
	workers int
}

func (consoleClusterSpark) StartCluster(_ context.Context, spec sparkproc.ClusterSpec) (dataproc.Nodes, error) {
	return &consoleNodes{workers: spec.Workers}, nil
}
func (n *consoleNodes) Scale(_ context.Context, w int) error {
	n.mu.Lock()
	n.workers = w
	n.mu.Unlock()
	return nil
}
func (n *consoleNodes) Workers() int                { n.mu.Lock(); defer n.mu.Unlock(); return n.workers }
func (n *consoleNodes) Stop(context.Context) error  { return nil }
func (n *consoleNodes) Start(context.Context) error { return nil }
func (n *consoleNodes) Delete()                     {}
func (n *consoleNodes) Web(context.Context) map[string]string {
	return map[string]string{"history": "127.0.0.1:1"}
}
func (n *consoleNodes) HistoryDir() string                               { return "" }
func (n *consoleNodes) Diagnose(context.Context, string) ([]byte, error) { return []byte("x"), nil }
func (n *consoleNodes) Submit(context.Context, sparkproc.Job) (sparkproc.Result, error) {
	return sparkproc.Result{DriverOutput: "pi\n", AppID: "app-1"}, nil
}

// TestConsoleDataprocClusters (#2162): the Clusters, Jobs, Workflows and
// Autoscaling policies pages' providers: an autoscaling policy created and
// edited; Create cluster (Standard, 2 workers, component gateway, the
// policy) listed and opened with its Jobs, VM instances, Configuration and
// Web interfaces (the Spark History Server on the API's port); Edit resizes
// it; Submit a job runs on it and opens with its Output and Spark UI; a
// workflow template created and Run, its run listed; Stop, Start and
// Delete.
func TestConsoleDataprocClusters(t *testing.T) {
	ctx := context.Background()
	const project = "dpc-console"
	api, err := dataproc.NewServer(dataproc.Options{Engine: consoleClusterSpark{}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	srv := grpctransport.New("127.0.0.1:0")
	if err := srv.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop(ctx) }()
	svc := &dataprocService{api: api, server: srv}
	clusters, jobs, workflows, policies := dataprocClustersProvider{svc: svc}, dataprocJobsProvider{svc: svc}, dataprocWorkflowsProvider{svc: svc}, dataprocPoliciesProvider{svc: svc}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	pol, err := policies.Create(ctx, project, map[string]string{"policyId": "scale", "region": "us-central1", "minWorkers": "2", "maxWorkers": "5",
		"scaleUp": "0.5", "scaleDown": "1.0", "decommission": "0", "cooldown": "120"})
	if err != nil || pol != "projects/"+project+"/regions/us-central1/autoscalingPolicies/scale" {
		t.Fatalf("Create policy: %q %v", pol, err)
	}
	if err := policies.ActAt(ctx, project, []string{pol}, "edit", map[string]string{"minWorkers": "2", "maxWorkers": "8", "scaleUp": "0.5", "scaleDown": "1.0", "decommission": "0", "cooldown": "120"}); err != nil {
		t.Fatalf("Edit policy: %v", err)
	}
	if l, _ := policies.List(ctx, project); len(l.Items) != 1 || l.Items[0].Fields["Primary workers"] != "2 to 8" {
		t.Errorf("policies: %+v", l.Items)
	}

	name, err := clusters.Create(ctx, project, map[string]string{"clusterName": "c1", "region": "us-central1", "clusterType": "STANDARD", "imageVersion": "2.2-debian12",
		"componentGateway": "true", "workers": "2", "masterMachineType": "n4-standard-4", "workerMachineType": "n4-standard-4", "autoscalingPolicy": "scale", "labels": "env=dev"})
	if err != nil || name != "projects/"+project+"/regions/us-central1/clusters/c1" {
		t.Fatalf("Create cluster: %q %v", name, err)
	}
	waitFor("the cluster", func() bool {
		l, _ := clusters.List(ctx, project)
		return len(l.Items) == 1 && l.Items[0].Fields["Status"] == "Running"
	})
	l, _ := clusters.List(ctx, project)
	if f := l.Items[0].Fields; f["Total worker nodes"] != "2" || f["Labels"] != "env: dev" || !strings.HasPrefix(f["Cloud Storage staging bucket"], "dataproc-staging-us-central1-") {
		t.Errorf("the cluster's row: %v", f)
	}
	d, err := clusters.Detail(ctx, project, []string{name})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 4 {
		t.Fatalf("cluster page: %+v %v", d, err)
	}
	if web := d.Sections[3].Listing; len(web.Items) == 0 || web.Items[0].Name != "Spark History Server" || !strings.HasPrefix(web.Items[0].Link, "http://"+srv.Addr()+"/component-gateway/") {
		t.Errorf("Web interfaces: %+v", web)
	}
	if vms := d.Sections[1].Listing; vms.Total != 3 {
		t.Errorf("VM instances: %+v", vms.Items)
	}
	if err := clusters.ActAt(ctx, project, []string{name}, "edit", map[string]string{"workers": "3", "autoscalingPolicy": "projects/" + project + "/regions/us-central1/autoscalingPolicies/scale", "labels": "env=dev"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	waitFor("the resize", func() bool {
		l, _ := clusters.List(ctx, project)
		return len(l.Items) == 1 && l.Items[0].Fields["Total worker nodes"] == "3" && l.Items[0].Fields["Status"] == "Running"
	})

	job, err := jobs.Create(ctx, project, map[string]string{"jobId": "pi-1", "region": "us-central1", "cluster": "c1", "jobType": "PYSPARK", "mainPythonFile": "file:///tmp/pi.py", "args": "10"})
	if err != nil || job != "projects/"+project+"/regions/us-central1/jobs/pi-1" {
		t.Fatalf("Submit job: %q %v", job, err)
	}
	waitFor("the job", func() bool {
		l, _ := jobs.List(ctx, project)
		return len(l.Items) == 1 && l.Items[0].Fields["Status"] == "Done"
	})
	jd, err := jobs.Detail(ctx, project, []string{job})
	if err != nil || jd.Unavailable != "" || jd.Sections[0].ID != "output" {
		t.Fatalf("job page: %+v %v", jd, err)
	}
	found := false
	for _, p := range jd.Sections[1].Groups[0].Properties {
		found = found || (p.Label == "Spark UI" && strings.HasSuffix(p.Link, "/sparkhistory/history/app-1/jobs/"))
	}
	if !found {
		t.Errorf("no Spark UI link: %+v", jd.Sections[1].Groups[0].Properties)
	}
	if _, err := jobs.Create(ctx, project, map[string]string{"region": "us-central1", "cluster": "c1", "jobType": "HIVE"}); err == nil || !strings.Contains(err.Error(), "Hive") {
		t.Errorf("Hive job: %v", err)
	}
	if err := jobs.ActAt(ctx, project, []string{job}, "delete", nil); err != nil {
		t.Errorf("Delete job: %v", err)
	}

	tmpl, err := workflows.Create(ctx, project, map[string]string{"templateId": "wf-1", "region": "us-central1", "placement": "SELECTOR", "selector": "env=dev",
		"jobs": "one pyspark file:///tmp/a.py 1\ntwo pyspark file:///tmp/b.py after=one"})
	if err != nil || tmpl != "projects/"+project+"/regions/us-central1/workflowTemplates/wf-1" {
		t.Fatalf("Create template: %q %v", tmpl, err)
	}
	if err := workflows.ActAt(ctx, project, []string{tmpl}, "run", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitFor("the workflow", func() bool {
		d, _ := workflows.Detail(ctx, project, []string{tmpl})
		runs := d.Sections[3].Listing
		return len(runs.Items) == 1 && runs.Items[0].Fields["Jobs"] == "2 of 2 completed" && runs.Items[0].Fields["Status"] == "Done"
	})
	if err := workflows.ActAt(ctx, project, []string{tmpl}, "delete", nil); err != nil {
		t.Errorf("Delete template: %v", err)
	}

	for _, act := range []string{"stop", "start"} {
		if err := clusters.ActAt(ctx, project, []string{name}, act, nil); err != nil {
			t.Fatalf("%s: %v", act, err)
		}
		want := map[string]string{"stop": "Stopped", "start": "Running"}[act]
		waitFor(act, func() bool {
			l, _ := clusters.List(ctx, project)
			return len(l.Items) == 1 && l.Items[0].Fields["Status"] == want
		})
	}
	if err := clusters.ActAt(ctx, project, []string{name}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	waitFor("the deletion", func() bool {
		l, _ := clusters.List(ctx, project)
		return len(l.Items) == 0
	})
	if err := policies.ActAt(ctx, project, []string{pol}, "delete", nil); err != nil {
		t.Errorf("Delete policy: %v", err)
	}
	_ = dataprocpb.ClusterStatus_RUNNING
}
