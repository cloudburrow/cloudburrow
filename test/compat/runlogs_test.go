//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// runLogEntries polls the official logadmin client until filter matches at
// least want entries, and returns them.
func runLogEntries(ctx context.Context, t *testing.T, ac *logadmin.Client, filter string, want int) []*logging.Entry {
	t.Helper()
	var got []*logging.Entry
	for ctx.Err() == nil {
		got = got[:0]
		it := ac.Entries(ctx, logadmin.Filter(filter))
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("Entries(%s): %v", filter, err)
			}
			got = append(got, e)
		}
		if len(got) >= want {
			return got
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Entries(%s) = %d entries; want %d", filter, len(got), want)
	return nil
}

// checkRunLogPair finds the plain stdout line and the JSON stderr line naming
// marker among entries, and checks what Cloud Run writes for each. The
// client decodes the log ID, which is run.googleapis.com%2Fstdout on the
// wire.
func checkRunLogPair(t *testing.T, entries []*logging.Entry, project, marker string, labels map[string]string) {
	t.Helper()
	var plain, structured *logging.Entry
	for _, e := range entries {
		switch p := e.Payload.(type) {
		case string:
			if strings.Contains(p, marker) {
				plain = e
			}
		case *structpb.Struct:
			if p.GetFields()["marker"].GetStringValue() == marker {
				structured = e
			}
		}
	}
	if plain == nil || structured == nil {
		t.Fatalf("entries for %s: plain %v, JSON %v; want both", marker, plain, structured)
	}
	if plain.LogName != "projects/"+project+"/logs/run.googleapis.com/stdout" || plain.Payload != "plain line "+marker ||
		plain.Severity != logging.Default {
		t.Errorf("the plain line = %+v; want a stdout textPayload with severity DEFAULT", plain)
	}
	p := structured.Payload.(*structpb.Struct).GetFields()
	if structured.LogName != "projects/"+project+"/logs/run.googleapis.com/stderr" || structured.Severity != logging.Warning ||
		p["message"].GetStringValue() != "json line "+marker || p["severity"] != nil {
		t.Errorf("the JSON line = %+v; want a stderr jsonPayload with severity WARNING and its message", structured)
	}
	for _, e := range []*logging.Entry{plain, structured} {
		for k, v := range labels {
			if got := e.Resource.GetLabels()[k]; got != v {
				t.Errorf("%s resource label %s = %q; want %q (%v)", e.LogName, k, got, v, e.Resource)
			}
		}
		if e.Labels["instanceId"] == "" {
			t.Errorf("%s has no instanceId label: %v", e.LogName, e.Labels)
		}
	}
}

// TestCloudRunLogsInCloudLogging (#1298): a revision's stdout and stderr, and
// a job task's, are in Cloud Logging as Cloud Run writes them, read back with
// the official logadmin client: logs run.googleapis.com/stdout and stderr,
// resources cloud_run_revision (service_name, revision_name, location,
// configuration_name) and cloud_run_job (job_name, location), the
// instanceId and execution labels, a textPayload for a plain line and a
// jsonPayload with its severity for a JSON one.
func TestCloudRunLogsInCloudLogging(t *testing.T) {
	h := New(t)
	rc := runClient(t, h)
	jc, _ := runJobClients(t, h)
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)

	ac, err := logadmin.NewClient(ctx, "projects/"+h.Project(), option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })

	// A service.
	id := "compat-runlogs"
	name := runParent(h) + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: envProbeImage}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}
	marker := "svc-" + time.Now().Format("150405.000000")
	if code, body := httpGet(t, ingress(t), hostOf(t, svc.GetUri()), "/log?marker="+marker); code != http.StatusOK {
		t.Fatalf("/log = %d %s", code, body)
	}
	rev := svc.GetLatestReadyRevision()[strings.LastIndex(svc.GetLatestReadyRevision(), "/")+1:]
	entries := runLogEntries(ctx, t, ac, `resource.type = "cloud_run_revision"`, 2)
	checkRunLogPair(t, entries, h.Project(), marker, map[string]string{"service_name": id, "revision_name": rev,
		"configuration_name": id, "location": "us-central1", "project_id": h.Project()})

	// A job.
	jobID := "compat-runlogs-job"
	jobName := runParent(h) + "/jobs/" + jobID
	jmarker := "job-" + time.Now().Format("150405.000000")
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: jobID,
		Job: shellJob(`echo "plain line `+jmarker+`"; echo '{"severity":"WARNING","message":"json line `+jmarker+`","marker":"`+jmarker+`"}' >&2`, 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: jobName}) })
	if _, err := jop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	execution, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the execution: %v", err)
	}
	execID := execution.GetName()[strings.LastIndex(execution.GetName(), "/")+1:]
	entries = runLogEntries(ctx, t, ac, `resource.type = "cloud_run_job"`, 2)
	checkRunLogPair(t, entries, h.Project(), jmarker, map[string]string{"job_name": jobID, "location": "us-central1",
		"project_id": h.Project()})
	for _, e := range entries {
		if !strings.Contains(fmt.Sprint(e.Payload), jmarker) {
			continue
		}
		if e.Labels["run.googleapis.com/execution_name"] != execID || e.Labels["run.googleapis.com/task_index"] != "0" {
			t.Errorf("a task entry's labels = %v; want execution_name %s and task_index 0", e.Labels, execID)
		}
	}
}
