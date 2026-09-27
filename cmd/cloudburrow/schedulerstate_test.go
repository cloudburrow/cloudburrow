package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Cloud Scheduler jobs are captured by state save and seedable (#600).

// startedScheduler is a schedulerService as Start leaves it, over a memory
// store, without a listener.
func startedScheduler() *schedulerService {
	db := store.NewMemory()
	st := scheduler.NewStore(db)
	return &schedulerService{db: db, store: st, api: scheduler.NewGRPCServer(st, nil, nil)}
}

func schedulerAdmin(t *testing.T, svc *schedulerService) string {
	t.Helper()
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceScheduler}
	control := lifecycle.NewControlServer(0, nil)
	mountAdmin(control, admin.NewRecorder(10, nil), cfg, adminDeps{scheduler: svc}, "")
	ctx := context.Background()
	if err := control.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Stop(ctx) })
	return "http://" + control.Addr()
}

func post(t *testing.T, url, contentType string, body []byte) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, contentType, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestStateSaveCapturesSchedulerJobs: a job created through the API, paused,
// is saved, deleted, and loaded back record for record, still paused; a job
// created after the save is gone. The manifest lists scheduler as captured.
func TestStateSaveCapturesSchedulerJobs(t *testing.T) {
	svc := startedScheduler()
	base := schedulerAdmin(t, svc)
	ctx := context.Background()
	parent := "projects/snap-proj/locations/us-central1"
	job, err := svc.api.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/nightly", Schedule: "0 3 * * *", TimeZone: "Europe/London",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:9/nightly", Body: []byte("kept")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.api.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: job.Name}); err != nil {
		t.Fatal(err)
	}
	before := dump(t, svc.db)

	code, archive := post(t, base+"/admin/state/export", "", nil)
	if code != http.StatusOK {
		t.Fatalf("export %d %s", code, archive)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	if hdr, err := tr.Next(); err != nil || hdr.Name != "manifest.json" {
		t.Fatalf("first entry %v, %v", hdr, err)
	}
	var m admin.Manifest
	if err := json.NewDecoder(tr).Decode(&m); err != nil {
		t.Fatal(err)
	}
	captured := false
	for _, s := range m.Services {
		if s.Name == "scheduler" {
			captured = s.Captured
		}
	}
	if !captured {
		t.Errorf("the manifest does not list scheduler as captured: %+v", m.Services)
	}

	if _, err := svc.api.DeleteJob(ctx, &schedulerpb.DeleteJobRequest{Name: job.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.api.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/later", Schedule: "* * * * *",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:9/later"}}}}); err != nil {
		t.Fatal(err)
	}

	if code, body := post(t, base+"/admin/state/import", "application/gzip", archive); code != http.StatusOK {
		t.Fatalf("import %d %s", code, body)
	}
	if after := dump(t, svc.db); !reflect.DeepEqual(before, after) {
		t.Errorf("the store differs after the round trip:\nbefore %v\nafter  %v", before, after)
	}
	got, err := svc.api.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.Name})
	if err != nil {
		t.Fatalf("the job is not back: %v", err)
	}
	if got.GetState() != schedulerpb.Job_PAUSED || got.GetTimeZone() != "Europe/London" ||
		string(got.GetHttpTarget().GetBody()) != "kept" || !got.GetScheduleTime().AsTime().Equal(job.GetScheduleTime().AsTime()) {
		t.Errorf("the job came back different: %v", got)
	}
	if _, err := svc.api.GetJob(ctx, &schedulerpb.GetJobRequest{Name: parent + "/jobs/later"}); err == nil {
		t.Error("a job created after the save survived the load")
	}
}

const schedulerSeedDoc = `{"jobs": [
	{"name": "projects/seed-proj/locations/us-central1/jobs/ping", "description": "every five minutes",
	 "schedule": "*/5 * * * *", "timeZone": "America/New_York",
	 "httpTarget": {"uri": "http://127.0.0.1:9/ping", "httpMethod": "PUT", "headers": {"X-Kind": "seed"}, "body": "hello"},
	 "retryConfig": {"retryCount": 3, "minBackoffDuration": "10s", "maxBackoffDuration": "60s", "maxDoublings": 2},
	 "attemptDeadline": "30s"},
	{"name": "projects/seed-proj/locations/us-central1/jobs/publish", "schedule": "0 * * * *",
	 "pubsubTarget": {"topicName": "projects/seed-proj/topics/ticks", "dataBase64": "AAEC/w==", "attributes": {"k": "v"}}}
]}`

// TestSchedulerSeedCreatesJobsOnce: a seed creates jobs as CreateJob does,
// every field read back through the API; a repeat is ALREADY_EXISTS, and
// ifNotExists makes it a no-op.
func TestSchedulerSeedCreatesJobsOnce(t *testing.T) {
	svc := startedScheduler()
	s := &schedulerSeeder{svc: svc}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}
	seedTwice(t, seed, schedulerSeedDoc)

	ctx := context.Background()
	ping, err := svc.api.GetJob(ctx, &schedulerpb.GetJobRequest{Name: "projects/seed-proj/locations/us-central1/jobs/ping"})
	if err != nil {
		t.Fatal(err)
	}
	h := ping.GetHttpTarget()
	rc := ping.GetRetryConfig()
	if ping.GetDescription() != "every five minutes" || ping.GetSchedule() != "*/5 * * * *" || ping.GetTimeZone() != "America/New_York" ||
		h.GetUri() != "http://127.0.0.1:9/ping" || h.GetHttpMethod() != schedulerpb.HttpMethod_PUT ||
		h.GetHeaders()["X-Kind"] != "seed" || string(h.GetBody()) != "hello" ||
		rc.GetRetryCount() != 3 || rc.GetMinBackoffDuration().AsDuration() != 10*time.Second ||
		rc.GetMaxBackoffDuration().AsDuration() != time.Minute || rc.GetMaxDoublings() != 2 ||
		ping.GetAttemptDeadline().AsDuration() != 30*time.Second || ping.GetState() != schedulerpb.Job_ENABLED ||
		!ping.GetScheduleTime().AsTime().After(time.Now()) {
		t.Errorf("seeded HTTP job = %v", ping)
	}
	pub, err := svc.api.GetJob(ctx, &schedulerpb.GetJobRequest{Name: "projects/seed-proj/locations/us-central1/jobs/publish"})
	if err != nil {
		t.Fatal(err)
	}
	if p := pub.GetPubsubTarget(); p.GetTopicName() != "projects/seed-proj/topics/ticks" ||
		string(p.GetData()) != "\x00\x01\x02\xff" || p.GetAttributes()["k"] != "v" || pub.GetTimeZone() != "Etc/UTC" {
		t.Errorf("seeded Pub/Sub job = %v", pub)
	}
}

// TestSchedulerSeedValidation: every job is checked by CreateJob's own
// validation before anything is created, and fields the service does not
// honour are refused by name.
func TestSchedulerSeedValidation(t *testing.T) {
	svc := startedScheduler()
	s := &schedulerSeeder{svc: svc}
	job := func(fields string) string {
		return `{"jobs": [{"name": "projects/seed-proj/locations/us-central1/jobs/j", ` + fields + `}]}`
	}
	target := `"httpTarget": {"uri": "http://127.0.0.1:9/x"}`
	for doc, want := range map[string]string{
		`{"jobs": [{"name": "jobs/j", "schedule": "* * * * *", ` + target + `}]}`:                                      "must be projects/{project}/locations/{location}/jobs/{job}",
		job(`"schedule": "every minute", ` + target):                                                                   "unix-cron",
		job(`"schedule": "* * * * *", "timeZone": "Mars/Olympus", ` + target):                                          "IANA",
		job(`"schedule": "* * * * *"`):                                                                                 "needs a target",
		job(`"schedule": "* * * * *", "httpTarget": {"uri": "ftp://x"}`):                                               "http or https",
		job(`"schedule": "* * * * *", "pubsubTarget": {"topicName": "projects/seed-proj/topics/t"}`):                   "data or at least one attribute",
		job(`"schedule": "* * * * *", ` + target + `, "retryConfig": {"retryCount": 9}`):                               "retry_count",
		job(`"schedule": "* * * * *", ` + target + `, "attemptDeadline": "5s"`):                                        "attempt_deadline",
		job(`"schedule": "* * * * *", ` + target + `, "attemptDeadline": "soon"`):                                      "attemptDeadline",
		job(`"schedule": "* * * * *", "httpTarget": {"uri": "http://x", "httpMethod": "FETCH"}`):                       "httpMethod",
		job(`"schedule": "* * * * *", "httpTarget": {"uri": "http://x", "body": "a", "bodyBase64": "YQ=="}`):           "exclusive",
		job(`"schedule": "* * * * *", "httpTarget": {"uri": "http://x", "oidcToken": {"serviceAccountEmail": "a@b"}}`): "oidcToken",
		job(`"schedule": "* * * * *", "appEngineHttpTarget": {"relativeUri": "/x"}`):                                   "appEngineHttpTarget",
		job(`"schedule": "* * * * *", ` + target + `, "state": "PAUSED"`):                                              "state",
		`{"jobs": [{"name": "projects/seed-proj/locations/us-central1/jobs/j", "schedule": "* * * * *", ` + target + `},
		           {"name": "projects/seed-proj/locations/us-central1/jobs/j", "schedule": "* * * * *", ` + target + `}]}`: "appears twice",
	} {
		if err := s.Validate(json.RawMessage(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want an error containing %q", doc, err, want)
		}
	}
	if keys, _ := svc.db.List(""); len(keys) != 0 {
		t.Errorf("validation created jobs: %v", keys)
	}
}

// TestAnInvalidSchedulerJobSeedsNothing, through /admin/seed: one bad job
// fails the whole document with 400 and creates no job, the valid one
// included.
func TestAnInvalidSchedulerJobSeedsNothing(t *testing.T) {
	svc := startedScheduler()
	base := schedulerAdmin(t, svc)
	doc := `{"components": {"scheduler": {"jobs": [
		{"name": "projects/seed-proj/locations/us-central1/jobs/good", "schedule": "* * * * *", "httpTarget": {"uri": "http://127.0.0.1:9/x"}},
		{"name": "projects/seed-proj/locations/us-central1/jobs/bad", "schedule": "61 * * * *", "httpTarget": {"uri": "http://127.0.0.1:9/x"}}
	]}}}`
	code, body := post(t, base+"/admin/seed", "application/json", []byte(doc))
	if code != http.StatusBadRequest || !strings.Contains(string(body), "jobs[1]") {
		t.Fatalf("seed returned %d %s, want 400 naming jobs[1]", code, body)
	}
	if keys, _ := svc.db.List(""); len(keys) != 0 {
		t.Errorf("a refused seed created jobs: %v", keys)
	}
	good := strings.Replace(doc, `"61 * * * *"`, `"1 * * * *"`, 1)
	if code, body := post(t, base+"/admin/seed", "application/json", []byte(good)); code != http.StatusOK {
		t.Fatalf("the corrected seed returned %d %s", code, body)
	}
	if keys, _ := svc.db.List(""); len(keys) != 2 {
		t.Errorf("the corrected seed created %v, want two jobs", keys)
	}
}
