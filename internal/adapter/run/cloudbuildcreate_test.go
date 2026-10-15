package run

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// logFetcher is a fakeFetcher that also records the logs written.
type logFetcher struct {
	fakeFetcher
	mu   sync.Mutex
	logs map[string]string
}

func (f *logFetcher) Put(_ context.Context, _, bucket, object string, data []byte, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[bucket+"/"+object] = string(data)
	return nil
}

type slowBuilder struct{}

func (slowBuilder) Build(ctx context.Context, spec BuildSpec) error {
	_, _ = io.ReadAll(spec.Archive)
	<-ctx.Done()
	return ctx.Err()
}

func cbSource() *cbpb.Source {
	return &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: "b", Object: "src.tgz"}}}
}

func TestCreateBuildDockerStepsAndLog(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 3)}
	f := &logFetcher{fakeFetcher: fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}, logs: map[string]string{}}
	cb := s.Builds(f, b).CloudBuild()
	img := "gcr.io/$PROJECT_ID/unit-cb:${_TAG}"
	op, err := cb.CreateBuild(context.Background(), &cbpb.CreateBuildRequest{
		Parent: "projects/proj-1/locations/us-central1",
		Build: &cbpb.Build{Source: cbSource(), Substitutions: map[string]string{"_TAG": "v1"}, LogsBucket: "gs://logs/sub",
			Steps: []*cbpb.BuildStep{
				{Id: "build", Name: "gcr.io/cloud-builders/docker", Dir: "app", Args: []string{"build", "--network", "cloudbuild", "-t", img, "-f", "docker/Dockerfile", "."}},
				{Name: "gcr.io/cloud-builders/docker", WaitFor: []string{"build"}, Args: []string{"push", img}},
				// gcloud builds submit --tag's step shape.
				{Name: "gcr.io/cloud-builders/gcb-internal", Args: []string{"docker", "build", "--network", "cloudbuild", "--no-cache", "-t", "gcr.io/p/other", "."}},
			},
			Images: []string{img}},
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	spec := <-b.got
	if spec.Dockerfile != "app/docker/Dockerfile|archive" || spec.Context != "app" || spec.LocalImage != "dev.local/gcr.io/proj-1/unit-cb:v1" {
		t.Errorf("spec = %+v", spec)
	}
	waitOp(t, s, op.GetName())
	got, _ := s.Operation(op.GetName())
	if got.GetError() != nil {
		t.Fatalf("operation failed: %v", got.GetError())
	}
	var res cbpb.Build
	if err := got.GetResponse().UnmarshalTo(&res); err != nil {
		t.Fatal(err)
	}
	if res.GetStatus() != cbpb.Build_SUCCESS || res.GetSteps()[0].GetStatus() != cbpb.Build_SUCCESS ||
		res.GetSteps()[1].GetTiming().GetEndTime() == nil || len(res.GetResults().GetImages()) != 1 ||
		res.GetResults().GetImages()[0].GetName() != "gcr.io/proj-1/unit-cb:v1" || res.GetTiming()["BUILD"] == nil {
		t.Errorf("build = %v", &res)
	}
	log := f.logs["logs/sub/log-"+res.GetId()+".txt"]
	for _, want := range []string{"starting build", "Starting Step #0 - \"build\"", "Step #1: The push refers to", "PUSH", "DONE"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if c := containerImage("gcr.io/proj-1/unit-cb@" + res.GetResults().GetImages()[0].GetDigest()); c != spec.LocalImage {
		t.Errorf("the image by digest resolves to %q", c)
	}
}

func TestCreateBuildFailureAndCancel(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	f := fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}
	cb := s.Builds(f, &fakeBuilder{got: make(chan BuildSpec, 1)}).CloudBuild()
	// A push of an image no step built fails the build at that step.
	op, err := cb.CreateBuild(context.Background(), &cbpb.CreateBuildRequest{ProjectId: "proj-1",
		Build: &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"push", "gcr.io/p/x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(op.GetName(), "operations/build/proj-1/") {
		t.Errorf("global operation name = %q", op.GetName())
	}
	waitOp(t, s, op.GetName())
	got, _ := s.Operation(op.GetName())
	if codes.Code(got.GetError().GetCode()) != codes.Unknown {
		t.Errorf("failed build's operation = %v", got)
	}
	cb2 := s.Builds(f, slowBuilder{}).CloudBuild()
	op, err = cb2.CreateBuild(context.Background(), &cbpb.CreateBuildRequest{Parent: "projects/proj-1/locations/us-central1",
		Build: &cbpb.Build{Source: cbSource(), Timeout: durationpb.New(time.Minute),
			Steps: []*cbpb.BuildStep{{Name: "docker", Args: []string{"build", "-t", "gcr.io/p/slow", "."}}}}})
	if err != nil {
		t.Fatal(err)
	}
	name := "projects/proj-1/locations/us-central1/builds/" + op.GetName()[strings.LastIndex(op.GetName(), "/")+1:]
	time.Sleep(50 * time.Millisecond)
	b, err := cb2.CancelBuild(context.Background(), &cbpb.CancelBuildRequest{Name: name})
	if err != nil || b.GetStatus() != cbpb.Build_CANCELLED {
		t.Fatalf("CancelBuild = %v, %v", b, err)
	}
	waitOp(t, s, op.GetName())
	if got, _ := s.Operation(op.GetName()); codes.Code(got.GetError().GetCode()) != codes.Canceled {
		t.Errorf("cancelled build's operation = %v", got)
	}
}

func TestCreateBuildRefusals(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	cb := s.Builds(fakeFetcher{}, &fakeBuilder{got: make(chan BuildSpec, 1)}).CloudBuild()
	for name, tc := range map[string]struct {
		b    *cbpb.Build
		code codes.Code
	}{
		"no steps":       {&cbpb.Build{Source: cbSource()}, codes.InvalidArgument},
		"arbitrary step": {&cbpb.Build{Source: cbSource(), Steps: []*cbpb.BuildStep{{Name: "ubuntu", Args: []string{"echo", "hi"}}}}, codes.Unimplemented},
		"docker run":     {&cbpb.Build{Source: cbSource(), Steps: []*cbpb.BuildStep{{Name: "docker", Args: []string{"run", "x"}}}}, codes.Unimplemented},
		"secrets":        {&cbpb.Build{Source: cbSource(), AvailableSecrets: &cbpb.Secrets{}, Steps: []*cbpb.BuildStep{{Name: "docker", Args: []string{"push", "x"}}}}, codes.Unimplemented},
		"repo source":    {&cbpb.Build{Source: &cbpb.Source{Source: &cbpb.Source_RepoSource{RepoSource: &cbpb.RepoSource{}}}, Steps: []*cbpb.BuildStep{{Name: "docker", Args: []string{"push", "x"}}}}, codes.Unimplemented},
		"bad waitFor":    {&cbpb.Build{Source: cbSource(), Steps: []*cbpb.BuildStep{{Name: "docker", WaitFor: []string{"later"}, Args: []string{"push", "x"}}}}, codes.InvalidArgument},
		"escaping dir":   {&cbpb.Build{Source: cbSource(), Steps: []*cbpb.BuildStep{{Name: "docker", Dir: "../x", Args: []string{"build", "-t", "x", "."}}}}, codes.Unimplemented},
		"bad subst key":  {&cbpb.Build{Source: cbSource(), Substitutions: map[string]string{"FOO": "x"}, Steps: []*cbpb.BuildStep{{Name: "docker", Args: []string{"push", "x"}}}}, codes.InvalidArgument},
	} {
		_, err := cb.CreateBuild(context.Background(), &cbpb.CreateBuildRequest{ProjectId: "p", Build: tc.b})
		if status.Code(err) != tc.code {
			t.Errorf("%s: %v; want %v", name, err, tc.code)
		}
	}
	if got := substitute("$$HOME ${PROJECT_ID}/$_X $UNSET", map[string]string{"PROJECT_ID": "p", "_X": "y"}); got != "$HOME p/y $UNSET" {
		t.Errorf("substitute = %q", got)
	}
}
