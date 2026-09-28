package scheduler

import (
	"context"
	"strings"
	"testing"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// What the contract refuses and the service accepted before #795 is refused,
// on CreateJob and on UpdateJob, with INVALID_ARGUMENT naming the field: a
// time zone of "Local", which is Go's name for the host's zone and no tz
// database name; a schedule with a TZ= or CRON_TZ= prefix, the cron parser's
// own extension (one with no space panicked in it); and a body on an HTTP
// target whose method is not POST, PUT or PATCH. A refused update changes
// nothing, and an invalid cron expression is refused as it was.
func TestSchedulerRefusesWhatTheContractRefuses(t *testing.T) {
	c, _, _, _ := start(t)
	ctx := context.Background()
	name := parent + "/jobs/refusals"
	target := func(m schedulerpb.HttpMethod, body string) *schedulerpb.Job_HttpTarget {
		return &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: "http://127.0.0.1:1/", HttpMethod: m, Body: []byte(body)}}
	}
	for _, tc := range []struct {
		what string
		job  *schedulerpb.Job
		want string
	}{
		{"a Local time zone", &schedulerpb.Job{Name: name, Schedule: "* * * * *", TimeZone: "Local",
			Target: target(schedulerpb.HttpMethod_POST, "")}, `time_zone "Local" is not an IANA time zone`},
		{"a TZ= prefix with no space", &schedulerpb.Job{Name: name, Schedule: "TZ=Asia/Tokyo",
			Target: target(schedulerpb.HttpMethod_POST, "")}, "give the time zone in time_zone"},
		{"a CRON_TZ= prefix", &schedulerpb.Job{Name: name, Schedule: "CRON_TZ=Asia/Tokyo 0 9 * * *",
			Target: target(schedulerpb.HttpMethod_POST, "")}, "give the time zone in time_zone"},
		{"a GET with a body", &schedulerpb.Job{Name: name, Schedule: "* * * * *",
			Target: target(schedulerpb.HttpMethod_GET, "x")}, "http_target.body is allowed only when http_method is POST, PUT or PATCH, not GET"},
		{"an invalid cron expression", &schedulerpb.Job{Name: name, Schedule: "every day",
			Target: target(schedulerpb.HttpMethod_POST, "")}, `schedule "every day" is not a unix-cron expression`},
	} {
		_, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: tc.job})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CreateJob with %s = %v; want INVALID_ARGUMENT %q", tc.what, err, tc.want)
		}
	}

	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "0 9 * * *", TimeZone: "Europe/Paris", Target: target(schedulerpb.HttpMethod_PUT, "b")}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what string
		job  *schedulerpb.Job
		mask []string
		want string
	}{
		{"a Local time zone", &schedulerpb.Job{Name: name, TimeZone: "Local"}, []string{"time_zone"}, "not an IANA time zone"},
		{"a CRON_TZ= schedule", &schedulerpb.Job{Name: name, Schedule: "CRON_TZ=UTC * * * * *"}, []string{"schedule"}, "give the time zone in time_zone"},
		{"a DELETE with a body", &schedulerpb.Job{Name: name, Target: target(schedulerpb.HttpMethod_DELETE, "b")}, []string{"http_target"}, "not DELETE"},
		{"an invalid cron expression", &schedulerpb.Job{Name: name, Schedule: "61 * * * *"}, []string{"schedule"}, "is not a unix-cron expression"},
	} {
		_, err := c.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{Job: tc.job, UpdateMask: &fieldmaskpb.FieldMask{Paths: tc.mask}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("UpdateJob with %s = %v; want INVALID_ARGUMENT %q", tc.what, err, tc.want)
		}
	}
	j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if j.GetSchedule() != "0 9 * * *" || j.GetTimeZone() != "Europe/Paris" || j.GetHttpTarget().GetHttpMethod() != schedulerpb.HttpMethod_PUT {
		t.Errorf("after refused updates the job reads %v", j)
	}
}
