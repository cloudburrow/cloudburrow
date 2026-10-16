package run

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// stepBuilder is a fakeBuilder that also runs arbitrary steps: each step
// appends its args to the workspace, and "fail N" exits N.
type stepBuilder struct {
	fakeBuilder
	mu      sync.Mutex
	opened  string
	specs   []StepSpec
	running map[int]bool
	overlap bool
	closed  bool
	ws      strings.Builder
}

func (b *stepBuilder) OpenWorkspace(_ context.Context, id string, archive io.Reader) (BuildWorkspace, error) {
	data := ""
	if archive != nil {
		d, _ := io.ReadAll(archive)
		data = string(d)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened, b.running = id, map[int]bool{}
	b.ws.WriteString(data)
	return b, nil
}

func (b *stepBuilder) RunStep(ctx context.Context, spec StepSpec) error {
	b.mu.Lock()
	b.specs = append(b.specs, spec)
	if len(b.running) > 0 {
		b.overlap = true
	}
	b.running[spec.Index] = true
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.running, spec.Index); b.mu.Unlock() }()
	fmt.Fprintf(spec.Log, "running %s\n", spec.Image)
	if len(spec.Args) == 2 && spec.Args[0] == "sleep" {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(spec.Args) == 2 && spec.Args[0] == "fail" {
		return &StepExitError{Code: 3}
	}
	b.mu.Lock()
	b.ws.WriteString("+" + strings.Join(spec.Args, " "))
	b.mu.Unlock()
	return nil
}

func (b *stepBuilder) Archive(context.Context) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return io.NopCloser(strings.NewReader(b.ws.String())), nil
}

func (b *stepBuilder) Close() { b.mu.Lock(); b.closed = true; b.mu.Unlock() }

func buildResult(t *testing.T, s *Server, op string) (*cbpb.Build, error) {
	t.Helper()
	waitOp(t, s, op)
	got, _ := s.Operation(op)
	md := &cbpb.BuildOperationMetadata{}
	_ = got.GetMetadata().UnmarshalTo(md)
	if got.GetError() != nil {
		return md.GetBuild(), status.Error(codes.Code(got.GetError().GetCode()), got.GetError().GetMessage())
	}
	var res cbpb.Build
	if err := got.GetResponse().UnmarshalTo(&res); err != nil {
		t.Fatal(err)
	}
	return &res, nil
}

// TestCreateBuildArbitrarySteps (#1382): a step of any image runs in the
// workspace with its env, entrypoint, dir and volumes; a docker build after
// it builds what it wrote; waitFor "-" runs steps at once.
func TestCreateBuildArbitrarySteps(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &stepBuilder{fakeBuilder: fakeBuilder{got: make(chan BuildSpec, 2)}}
	f := fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}
	cb := s.Builds(f, b).AllowHostSteps(true).CloudBuild()
	op, err := cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{Parent: "projects/proj-1/locations/us-central1",
		Build: &cbpb.Build{Source: cbSource(), Substitutions: map[string]string{"_V": "1"},
			Options: &cbpb.BuildOptions{AutomapSubstitutions: true},
			Steps: []*cbpb.BuildStep{
				{Id: "a", Name: "alpine", Entrypoint: "sh", Args: []string{"sleep", "x"}, WaitFor: []string{"-"},
					Env: []string{"V=$_V"}, Dir: "sub", Volumes: []*cbpb.Volume{{Name: "cache", Path: "/cache"}}},
				{Id: "b", Name: "alpine", Args: []string{"sleep", "y"}, WaitFor: []string{"-"}},
				{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", "gcr.io/p/img", "."}},
			}, Images: []string{"gcr.io/p/img"}}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	spec := <-b.got
	res, err := buildResult(t, s, op.GetName())
	if err != nil || res.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("build = %v, %v", res, err)
	}
	if !strings.HasPrefix(spec.Dockerfile, "Dockerfile|archive+") || !strings.Contains(spec.Dockerfile, "sleep x") {
		t.Errorf("the docker build saw %q; want the workspace earlier steps wrote", spec.Dockerfile)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if closed {
			break
		}
	}
	b.mu.Lock()
	if !b.overlap || !b.closed || b.opened != res.GetId() {
		t.Errorf("overlap %v closed %v opened %q", b.overlap, b.closed, b.opened)
	}
	var a StepSpec
	for _, sp := range b.specs {
		if sp.Index == 0 {
			a = sp
		}
	}
	if a.Entrypoint != "sh" || a.Dir != "sub" || len(a.Volumes) != 1 || a.Volumes[0] != (StepVolume{"cache", "/cache"}) ||
		a.Env[0] != "V=1" || !strings.Contains(strings.Join(a.Env, " "), "BUILD_ID="+res.GetId()) {
		t.Errorf("step 0 spec = %+v", a)
	}

	b.mu.Unlock()
	// A step that exits non-zero fails the build with its exit code; the
	// step waiting for it never starts.
	op, err = cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: "alpine", Args: []string{"fail", "now"}}, {Name: "alpine", Args: []string{"echo", "never"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err = buildResult(t, s, op.GetName())
	if status.Code(err) != codes.Unknown || res.GetStatus() != cbpb.Build_FAILURE || res.GetSteps()[0].GetExitCode() != 3 ||
		res.GetSteps()[1].GetStatus() != cbpb.Build_QUEUED ||
		res.GetStatusDetail() != `build step 0 "alpine" failed: step exited with non-zero status: 3` {
		t.Errorf("failed build = %v, %v", res, err)
	}

	for name, st := range map[string]*cbpb.BuildStep{
		"secretEnv":     {Name: "alpine", SecretEnv: []string{"X"}},
		"reserved path": {Name: "alpine", Volumes: []*cbpb.Volume{{Name: "v", Path: "/workspace"}}},
		"relative path": {Name: "alpine", Volumes: []*cbpb.Volume{{Name: "v", Path: "rel"}}},
		"bad env":       {Name: "alpine", Env: []string{"NOEQUALS"}},
		"docker volume": {Name: "docker", Args: []string{"push", "x"}, Volumes: []*cbpb.Volume{{Name: "v", Path: "/v"}}},
	} {
		_, err := cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "p", Build: &cbpb.Build{Steps: []*cbpb.BuildStep{st}}})
		if c := status.Code(err); c != codes.InvalidArgument && c != codes.Unimplemented {
			t.Errorf("%s: %v; want a refusal", name, err)
		}
	}
}

// TestRetryBuild (#1382): a new build, with a new ID, of the same request;
// a SubmitBuild build is refused.
func TestRetryBuild(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &stepBuilder{fakeBuilder: fakeBuilder{got: make(chan BuildSpec, 2)}}
	cb := s.Builds(fakeFetcher{}, b).AllowHostSteps(true).CloudBuild()
	op, err := cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{Parent: "projects/proj-r/locations/europe-west1",
		Build: &cbpb.Build{Tags: []string{"t1"}, Steps: []*cbpb.BuildStep{{Name: "alpine", Args: []string{"echo", "$BUILD_ID"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := buildResult(t, s, op.GetName())
	rop, err := cb.RetryBuild(fromLoopback(), &cbpb.RetryBuildRequest{Name: first.GetName()})
	if err != nil {
		t.Fatalf("RetryBuild: %v", err)
	}
	if !strings.HasPrefix(rop.GetName(), "projects/proj-r/locations/europe-west1/operations/") {
		t.Errorf("retry operation = %s", rop.GetName())
	}
	second, err := buildResult(t, s, rop.GetName())
	if err != nil || second.GetId() == first.GetId() || second.GetStatus() != cbpb.Build_SUCCESS ||
		second.GetTags()[0] != "t1" || second.GetSteps()[0].GetArgs()[1] != second.GetId() {
		t.Errorf("retried build = %v, %v", second, err)
	}
	gop, err := cb.RetryBuild(fromLoopback(), &cbpb.RetryBuildRequest{ProjectId: "proj-r", Id: first.GetId()})
	if err != nil || !strings.HasPrefix(gop.GetName(), "operations/build/proj-r/") {
		t.Errorf("global RetryBuild = %v, %v", gop, err)
	}
	waitOp(t, s, gop.GetName())
	sub := newCloudBuild("projects/proj-r/locations/europe-west1", "gcr.io/p/x", BuildDocker, "b", "o")
	if _, err := cb.RetryBuild(fromLoopback(), &cbpb.RetryBuildRequest{Name: sub.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("RetryBuild of a SubmitBuild build = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := cb.RetryBuild(fromLoopback(), &cbpb.RetryBuildRequest{ProjectId: "proj-r", Id: "missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("RetryBuild of a missing build = %v", err)
	}
}

// TestListBuildsFilterAndPaging (#1382).
func TestListBuildsFilterAndPaging(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cloudBuilds.mu.Lock()
	for i, st := range []cbpb.Build_Status{cbpb.Build_SUCCESS, cbpb.Build_FAILURE, cbpb.Build_WORKING, cbpb.Build_SUCCESS} {
		loc := "us-central1"
		if i == 3 {
			loc = "global"
		}
		id := fmt.Sprintf("lf-%d", i)
		b := &cbpb.Build{Name: "projects/proj-lf/locations/" + loc + "/builds/" + id, Id: id, ProjectId: "proj-lf",
			Status: st, CreateTime: timestamppb.New(base.Add(time.Duration(i) * time.Hour)),
			Images: []string{"gcr.io/proj-lf/img" + fmt.Sprint(i%2)}, Tags: []string{"tag" + fmt.Sprint(i)}, BuildTriggerId: "trig" + fmt.Sprint(i%2)}
		if i == 0 {
			b.Tags = append(b.Tags, "prod")
		}
		cloudBuilds.builds[b.Name] = b
	}
	cloudBuilds.mu.Unlock()
	cb := &CloudBuildServer{}
	ids := func(filter, parent string) string {
		t.Helper()
		resp, err := cb.ListBuilds(context.Background(), &cbpb.ListBuildsRequest{ProjectId: "proj-lf", Parent: parent, Filter: filter})
		if err != nil {
			t.Fatalf("ListBuilds(%q): %v", filter, err)
		}
		var out []string
		for _, b := range resp.GetBuilds() {
			out = append(out, b.GetId())
		}
		return strings.Join(out, ",")
	}
	for filter, want := range map[string]string{
		``:                                    "lf-3,lf-2,lf-1,lf-0",
		`status="SUCCESS"`:                    "lf-3,lf-0",
		`status="QUEUED" OR status="WORKING"`: "lf-2",
		`status!="SUCCESS"`:                   "lf-2,lf-1",
		`tags="prod"`:                         "lf-0",
		`tags=("tag1" OR "tag2")`:             "lf-2,lf-1",
		`images="gcr.io/proj-lf/img1"`:        "lf-3,lf-1",
		`trigger_id="trig0" AND status="SUCCESS"`: "lf-0",
		`build_id="lf-1"`:                         "lf-1",
		`create_time>"2026-01-02T04:04:05Z"`:      "lf-3,lf-2",
		`create_time>="2026-01-02T04:04:05Z" AND create_time<"2026-01-02T06:00:00+00:00"`: "lf-2,lf-1",
		`(status="SUCCESS" OR status="FAILURE") AND images="gcr.io/proj-lf/img0"`:         "lf-0",
		`NOT status="SUCCESS"`:             "lf-2,lf-1",
		`-tags="prod" status="SUCCESS"`:    "lf-3",
		`source.storage_source.bucket!=""`: "",
	} {
		if got := ids(filter, ""); got != want {
			t.Errorf("filter %q = %q, want %q", filter, got, want)
		}
	}
	if got := ids(`status="SUCCESS"`, "projects/proj-lf/locations/us-central1"); got != "lf-0" {
		t.Errorf("regional filter = %q", got)
	}
	for _, bad := range []string{`status=`, `nosuch="x"`, `create_time>"yesterday"`, `tags>"x"`, `(status="SUCCESS"`, `status="x`} {
		if _, err := cb.ListBuilds(context.Background(), &cbpb.ListBuildsRequest{ProjectId: "proj-lf", Filter: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("filter %q = %v; want INVALID_ARGUMENT", bad, err)
		}
	}
	// Pages of 3 then 1, bound to the filter.
	var all []string
	token := ""
	for page := 0; ; page++ {
		resp, err := cb.ListBuilds(context.Background(), &cbpb.ListBuildsRequest{ProjectId: "proj-lf", PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range resp.GetBuilds() {
			all = append(all, b.GetId())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
		if _, err := cb.ListBuilds(context.Background(), &cbpb.ListBuildsRequest{ProjectId: "proj-lf", Filter: `status="SUCCESS"`, PageToken: token}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("a token reused with another filter = %v", err)
		}
	}
	if strings.Join(all, ",") != "lf-3,lf-2,lf-1,lf-0" {
		t.Errorf("paged = %v", all)
	}
}
