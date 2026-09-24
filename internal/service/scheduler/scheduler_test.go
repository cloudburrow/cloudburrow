package scheduler

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const parent = "projects/demo-project/locations/us-central1"

type published struct {
	mu   sync.Mutex
	msgs []string
}

func (p *published) publish(_ context.Context, topic string, data []byte, _ map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, topic+":"+string(data))
	return nil
}

// start serves the API in-process over the official client, with a fake
// clock driving the runner.
func start(t *testing.T) (*scheduler.CloudSchedulerClient, *Runner, *sched.FakeClock, *published) {
	t.Helper()
	clock := sched.NewFakeClock(time.Date(2026, 9, 24, 10, 0, 30, 0, time.UTC))
	st := NewStore(store.NewMemory())
	pub := &published{}
	runner := NewRunner(st, nil, clock, pub.publish, time.Second)
	g := grpc.NewServer()
	NewGRPCServer(st, runner, clock).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); g.Stop() })
	c, err := scheduler.NewCloudSchedulerClient(context.Background(), option.WithEndpoint(ln.Addr().String()),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, runner, clock, pub
}

type hits struct {
	mu  sync.Mutex
	got []http.Header
	ch  chan struct{}
}

func target(t *testing.T) (*httptest.Server, *hits) {
	h := &hits{ch: make(chan struct{}, 10)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.got = append(h.got, r.Header.Clone())
		h.mu.Unlock()
		h.ch <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

func httpJob(id, uri string) *schedulerpb.Job {
	return &schedulerpb.Job{Name: parent + "/jobs/" + id, Schedule: "* * * * *", TimeZone: "Europe/Paris",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: uri, HttpMethod: schedulerpb.HttpMethod_POST, Body: []byte("hi")}}}
}

func wait(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

func TestRunJobDeliversAtOnce(t *testing.T) {
	c, _, _, _ := start(t)
	ctx := context.Background()
	srv, h := target(t)
	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: httpJob("every-minute", srv.URL)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// Every minute, in Paris: the next run is the next whole minute.
	if got := j.GetScheduleTime().AsTime(); !got.Equal(time.Date(2026, 9, 24, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("schedule time = %v", got)
	}
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
	wait(t, h.ch, "RunJob delivery")
	h.mu.Lock()
	hdr := h.got[0]
	h.mu.Unlock()
	if hdr.Get("X-CloudScheduler") != "true" || hdr.Get("X-CloudScheduler-JobName") != "every-minute" || hdr.Get("User-Agent") != "Google-Cloud-Scheduler" {
		t.Errorf("delivery headers = %v", hdr)
	}
}

func TestScheduleFiresPauseStopsResumeRestarts(t *testing.T) {
	c, _, clock, _ := start(t)
	ctx := context.Background()
	srv, h := target(t)
	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: httpJob("ticker", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(40 * time.Second) // past 10:01
	wait(t, h.ch, "the scheduled run")

	if p, err := c.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: j.GetName()}); err != nil || p.GetState() != schedulerpb.Job_PAUSED {
		t.Fatalf("PauseJob = %v, %v", p, err)
	}
	clock.Advance(2 * time.Minute)
	time.Sleep(200 * time.Millisecond)
	select {
	case <-h.ch:
		t.Fatal("a paused job fired")
	default:
	}

	if r, err := c.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: j.GetName()}); err != nil || r.GetState() != schedulerpb.Job_ENABLED {
		t.Fatalf("ResumeJob = %v, %v", r, err)
	}
	clock.Advance(time.Minute)
	wait(t, h.ch, "the run after resume")
}

func TestPubSubTargetPublishes(t *testing.T) {
	c, _, _, pub := start(t)
	ctx := context.Background()
	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/pub", Schedule: "0 9 * * 1",
		Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{TopicName: "projects/demo-project/topics/t", Data: []byte("tick")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pub.mu.Lock()
		n := len(pub.msgs)
		pub.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pub.msgs) != 1 || pub.msgs[0] != "projects/demo-project/topics/t:tick" {
		t.Errorf("published = %v", pub.msgs)
	}
}

func TestRetriesFollowRetryConfig(t *testing.T) {
	c, _, clock, _ := start(t)
	ctx := context.Background()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	job := httpJob("flaky", srv.URL)
	job.Schedule = "0 0 1 1 *" // not due during the test: only RunJob fires it
	job.RetryConfig = &schedulerpb.RetryConfig{RetryCount: 2, MinBackoffDuration: durationpb.New(10 * time.Second)}
	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	waitFor := func(n int) {
		t.Helper()
		for d := time.Now().Add(5 * time.Second); count() < n && time.Now().Before(d); {
			time.Sleep(10 * time.Millisecond)
		}
		if count() != n {
			t.Fatalf("calls = %d, want %d", count(), n)
		}
	}
	waitFor(1)
	clock.Advance(9 * time.Second)
	time.Sleep(100 * time.Millisecond)
	if count() != 1 {
		t.Fatalf("retried before min backoff: %d calls", count())
	}
	clock.Advance(time.Second)
	waitFor(2)
	clock.Advance(20 * time.Second) // doubled
	waitFor(3)
	clock.Advance(time.Hour)
	time.Sleep(100 * time.Millisecond)
	if count() != 3 {
		t.Errorf("retry_count 2 made %d attempts, want 3", count())
	}
	got, _ := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: j.GetName()})
	if got.GetStatus().GetCode() == 0 || got.GetLastAttemptTime() == nil {
		t.Errorf("status after failures = %v", got.GetStatus())
	}
}

func TestRefusalsAndCRUD(t *testing.T) {
	c, _, _, _ := start(t)
	ctx := context.Background()
	oidc := httpJob("oidc", "http://127.0.0.1:1/")
	oidc.GetHttpTarget().AuthorizationHeader = &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{ServiceAccountEmail: "a@b"}}
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: oidc}); status.Code(err) != codes.Unimplemented {
		t.Errorf("OIDC job = %v, want Unimplemented", err)
	}
	ae := &schedulerpb.Job{Name: parent + "/jobs/ae", Schedule: "* * * * *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{RelativeUri: "/"}}}
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: ae}); status.Code(err) != codes.Unimplemented {
		t.Errorf("App Engine job = %v, want Unimplemented", err)
	}
	bad := httpJob("bad-cron", "http://127.0.0.1:1/")
	bad.Schedule = "every tuesday"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad cron = %v", err)
	}
	badTZ := httpJob("bad-tz", "http://127.0.0.1:1/")
	badTZ.TimeZone = "Mars/Olympus"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: badTZ}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad zone = %v", err)
	}

	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: httpJob("crud", "http://127.0.0.1:1/")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: httpJob("crud", "http://127.0.0.1:1/")}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v", err)
	}
	upd := httpJob("crud", "http://127.0.0.1:1/")
	upd.Schedule = "30 8 * * *"
	upd.Description = "morning"
	u, err := c.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{Job: upd})
	if err != nil || u.GetSchedule() != "30 8 * * *" || u.GetDescription() != "morning" {
		t.Errorf("UpdateJob = %v, %v", u, err)
	}
	it := c.ListJobs(ctx, &schedulerpb.ListJobsRequest{Parent: parent})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("ListJobs = %d jobs, want 1", n)
	}
	if err := c.DeleteJob(ctx, &schedulerpb.DeleteJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: j.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after delete = %v", err)
	}
}
