//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"testing"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Cloud Scheduler across stop and up (#596), the pattern of #418: in
// persistent mode its jobs are a durable store under the state directory, in
// ephemeral mode they are memory. TestSchedulerRestartSetup runs alone just
// before `stop`, and TestSchedulerAcrossRestart after each `up`.
const (
	envSchedulerProbe  = "CLOUDBURROW_TEST_SCHEDULER_PROBE"
	envSchedulerExpect = "CLOUDBURROW_TEST_SCHEDULER_EXPECT"
)

// schedulerProbeJob is one job as the setup left it.
type schedulerProbeJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	TimeZone string `json:"timeZone"`
	URI      string `json:"uri"`
	State    string `json:"state"`
}

// TestSchedulerRestartSetup leaves an enabled and a paused HTTP job, each
// with its own schedule and time zone, for TestSchedulerAcrossRestart. Neither
// target answers and neither schedule is due during a CI run.
func TestSchedulerRestartSetup(t *testing.T) {
	path := os.Getenv(envSchedulerProbe)
	if path == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envSchedulerProbe)
	}
	h := New(t)
	ctx := h.Context()
	c := schedulerClient(t, h)
	parent := "projects/" + h.Project() + "/locations/us-central1"
	jobs := []schedulerProbeJob{
		{Name: parent + "/jobs/restart-probe", Schedule: "0 3 1 1 *", TimeZone: "Europe/London", URI: "http://127.0.0.1:1/enabled", State: "ENABLED"},
		{Name: parent + "/jobs/restart-probe-paused", Schedule: "30 4 * * 1", TimeZone: "America/New_York", URI: "http://127.0.0.1:1/paused", State: "PAUSED"},
	}
	for _, j := range jobs {
		if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
			Name: j.Name, Schedule: j.Schedule, TimeZone: j.TimeZone, Description: "restart probe",
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: j.URI, HttpMethod: schedulerpb.HttpMethod_POST}}}}); err != nil {
			t.Fatalf("CreateJob %s: %v", j.Name, err)
		}
		if j.State == "PAUSED" {
			if p, err := c.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: j.Name}); err != nil || p.GetState() != schedulerpb.Job_PAUSED {
				t.Fatalf("PauseJob = %v, %v", p.GetState(), err)
			}
		}
	}
	b, _ := json.Marshal(jobs)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSchedulerAcrossRestart: after stop and up in persistent mode both jobs
// read back with their schedule, time zone, target and state; in ephemeral
// mode they are gone.
func TestSchedulerAcrossRestart(t *testing.T) {
	expect := os.Getenv(envSchedulerExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envSchedulerExpect)
	}
	b, err := os.ReadFile(os.Getenv(envSchedulerProbe))
	if err != nil {
		t.Fatalf("the probe TestSchedulerRestartSetup left: %v", err)
	}
	var jobs []schedulerProbeJob
	if err := json.Unmarshal(b, &jobs); err != nil || len(jobs) == 0 {
		t.Fatalf("the probe file: %d jobs, %v", len(jobs), err)
	}
	h := New(t)
	ctx := h.Context()
	c := schedulerClient(t, h)
	for _, want := range jobs {
		got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: want.Name})
		switch expect {
		case "present":
			if err != nil {
				t.Errorf("persistent mode after stop/up: GetJob %s = %v; want the job created before stop", want.Name, err)
				continue
			}
			if got.GetSchedule() != want.Schedule || got.GetTimeZone() != want.TimeZone ||
				got.GetHttpTarget().GetUri() != want.URI || got.GetState().String() != want.State {
				t.Errorf("GetJob %s = schedule %q, time zone %q, uri %q, state %v; want %q, %q, %q, %s", want.Name,
					got.GetSchedule(), got.GetTimeZone(), got.GetHttpTarget().GetUri(), got.GetState(),
					want.Schedule, want.TimeZone, want.URI, want.State)
			}
		case "absent":
			if status.Code(err) != codes.NotFound {
				t.Errorf("ephemeral mode after stop/up: GetJob %s = %v; want NotFound", want.Name, err)
			}
		default:
			t.Fatalf("%s must be present or absent, not %q", envSchedulerExpect, expect)
		}
	}
}
