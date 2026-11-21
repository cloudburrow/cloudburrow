//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/apphub/apiv1/apphubpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
)

// TestLoggingFilterAppHub (#2096), on an instance serving Cloud Run, App
// Hub and Cloud Logging, with the official Cloud Run, App Hub and Logging
// clients: a Cloud Run job is registered as a workload of a global
// application; an entry for its monitored resource (cloud_run_job) is then
// listed with the output-only apphub in entries.list's JSON (the
// application, and the workload's id, environment and criticality), and the
// Logging API's own filter on apphub.* matches it over REST and gRPC
// (apphub.application.id, apphub.workload.id), as Google's query language
// takes every LogEntry field (logging-query-language, LogEntry reference,
// re-read 2026-11-21).
func TestLoggingFilterAppHub(t *testing.T) {
	h := New(t)
	clients := appHubClients(t, h) // skips without CLOUDBURROW_TEST_APPHUB
	h.Endpoint(EnvRun)
	logBase := "http://" + h.Endpoint(EnvLogging)
	c := clients["grpc"]
	ctx := h.Context()
	project := h.Project()
	suffix := time.Now().UnixNano() % 1e6
	parent := "projects/" + project + "/locations/us-central1"
	global := "projects/" + project + "/locations/global"

	// The job.
	rctx := runWait(t)
	jc, err := run.NewJobsClient(rctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	jobID := fmt.Sprintf("logs-ah-%d", suffix)
	jop, err := jc.CreateJob(rctx, &runpb.CreateJobRequest{Parent: parent, JobId: jobID, Job: shellJob("echo hi", 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	job := parent + "/jobs/" + jobID
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: job}) })
	if _, err := waitLRO(rctx, jop); err != nil {
		t.Fatalf("waiting for CreateJob: %v", err)
	}

	// The project attached to itself, a global application, the job
	// registered as its workload.
	op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: global,
		ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
	if err != nil {
		t.Fatalf("CreateServiceProjectAttachment: %v", err)
	}
	spa, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the attachment: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := c.DeleteServiceProjectAttachment(h.Context(), &apphubpb.DeleteServiceProjectAttachmentRequest{Name: spa.GetName()}); err == nil {
			_ = dop.Wait(h.Context())
		}
	})
	dw, err := c.LookupDiscoveredWorkload(ctx, &apphubpb.LookupDiscoveredWorkloadRequest{Parent: parent, Uri: "//run.googleapis.com/" + job})
	if err != nil || dw.GetDiscoveredWorkload() == nil {
		t.Fatalf("LookupDiscoveredWorkload: %v, %v", dw, err)
	}
	appID := fmt.Sprintf("logs-app-%d", suffix)
	aop, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: global, ApplicationId: appID,
		Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}}})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	app, err := aop.Wait(ctx)
	if err != nil {
		t.Fatalf("the application: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := c.DeleteApplication(h.Context(), &apphubpb.DeleteApplicationRequest{Name: app.GetName()}); err == nil {
			_ = dop.Wait(h.Context())
		}
	})
	wop, err := c.CreateWorkload(ctx, &apphubpb.CreateWorkloadRequest{Parent: app.GetName(), WorkloadId: "job",
		Workload: &apphubpb.Workload{DiscoveredWorkload: dw.GetDiscoveredWorkload().GetName(), Attributes: &apphubpb.Attributes{
			Criticality: &apphubpb.Criticality{Type: apphubpb.Criticality_HIGH}, Environment: &apphubpb.Environment{Type: apphubpb.Environment_PRODUCTION}}}})
	if err != nil {
		t.Fatalf("CreateWorkload: %v", err)
	}
	wl, err := wop.Wait(ctx)
	if err != nil {
		t.Fatalf("the workload: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := c.DeleteWorkload(h.Context(), &apphubpb.DeleteWorkloadRequest{Name: wl.GetName()}); err == nil {
			_ = dop.Wait(h.Context())
		}
	})

	// The job's entry, and its apphub.
	logName := fmt.Sprintf("projects/%s/logs/apphub-job-%d", project, suffix)
	post := func(path string, body, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(logBase+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %s", path, resp.Status)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	post("/v2/entries:write", map[string]any{"entries": []any{map[string]any{"logName": logName, "textPayload": "from the job",
		"resource": map[string]any{"type": "cloud_run_job", "labels": map[string]string{"project_id": project, "location": "us-central1", "job_name": jobID}}}}}, nil)
	var got struct {
		Entries []struct {
			AppHub *struct {
				Application struct{ Container, Location, ID string } `json:"application"`
				Workload    *struct {
					ID              string `json:"id"`
					EnvironmentType string `json:"environmentType"`
					CriticalityType string `json:"criticalityType"`
				} `json:"workload"`
			} `json:"apphub"`
		} `json:"entries"`
	}
	post("/v2/entries:list", map[string]any{"resourceNames": []string{"projects/" + project}, "filter": fmt.Sprintf("logName=%q", logName)}, &got)
	if len(got.Entries) != 1 || got.Entries[0].AppHub == nil {
		t.Fatalf("entries.list: %+v; want one entry with apphub", got)
	}
	a := got.Entries[0].AppHub
	if a.Application.ID != appID || a.Application.Location != "global" || a.Workload == nil || a.Workload.ID != "job" ||
		a.Workload.EnvironmentType != "PRODUCTION" || a.Workload.CriticalityType != "HIGH" {
		t.Errorf("apphub = %+v; want application %s in global, workload job (PRODUCTION, HIGH)", a, appID)
	}
	appHubLogFilters(t, h, project, logName, appID, "workload", "job")
}
