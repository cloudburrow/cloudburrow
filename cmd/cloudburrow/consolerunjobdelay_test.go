package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	runv1 "google.golang.org/api/run/v1"
)

// TestConsoleRunJobSetDelayedExecution (#1450): a job's page offers Set
// delayed execution, its box ticked as the job is now, read through the v1
// API at the region's host; saving it replaces the job with
// spec.template.spec.delayExecution changed and the rest as read.
func TestConsoleRunJobSetDelayedExecution(t *testing.T) {
	var mu sync.Mutex
	job := &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "nightly", ResourceVersion: "7"},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 3, DelayExecution: true,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{Containers: []*runv1.Container{{Image: "busybox:1.36"}}}}}}}}
	var hosts []string
	var replaced *runv1.Job
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hosts = append(hosts, r.Host)
		if r.URL.Path != "/apis/run.googleapis.com/v1/namespaces/demo/jobs/nightly" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut {
			replaced = &runv1.Job{}
			_ = json.NewDecoder(r.Body).Decode(replaced)
		}
		_ = json.NewEncoder(w).Encode(job)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	p := runJobsProvider{runAddr: func() string { return host }, defaultProject: "demo", region: "us-central1"}
	ctx := context.Background()

	actions := p.delayAction(ctx, "", "nightly")
	if len(actions) != 1 || actions[0].ID != actSetDelay || actions[0].Fields[0].Default != "true" {
		t.Fatalf("delay action = %+v; want Set delayed execution, ticked", actions)
	}
	if err := p.ActAt(ctx, "", []string{"nightly"}, actSetDelay, map[string]string{runDelayField: "false"}); err != nil {
		t.Fatalf("Set delayed execution: %v", err)
	}
	if replaced == nil || replaced.Spec.Template.Spec.DelayExecution || replaced.Spec.Template.Spec.TaskCount != 3 ||
		replaced.Metadata.ResourceVersion != "7" {
		t.Errorf("replaced job = %+v; want delayExecution cleared and the rest kept", replaced)
	}
	for _, h := range hosts {
		if h != "us-central1-run.localhost" {
			t.Errorf("request host = %q; want the region's", h)
		}
	}
}
