package run

import (
	"context"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestExpandDynamic(t *testing.T) {
	subs := map[string]string{"V": "gcr.io/proj/app:v1.2.3", "_N": "hello-world", dynamicKey: "1"}
	for in, want := range map[string]string{
		"${V}":           "gcr.io/proj/app:v1.2.3",
		"${_N:6}":        "world",
		"${_N:0:5}":      "hello",
		"${_N: -5}":      "world",
		"${V#*/}":        "proj/app:v1.2.3",
		"${V##*/}":       "app:v1.2.3",
		"${V%:*}":        "gcr.io/proj/app",
		"${V%%.*}":       "gcr",
		"${_N/o/0}":      "hell0-world",
		"${_N//o/0}":     "hell0-w0rld",
		"$$V and $_N":    "$V and hello-world",
		"${UNKNOWN:0:1}": "${UNKNOWN:0:1}",
		"${_N/[eo]?/_}":  "h_lo-world",
		"pre-${_N%-*}-x": "pre-hello-x",
	} {
		if got := substitute(in, subs); got != want {
			t.Errorf("substitute(%q) = %q; want %q", in, got, want)
		}
	}
	// Without dynamicSubstitutions, bash operators are left as written.
	if got := substitute("${_N:0:5}", map[string]string{"_N": "hello-world"}); got != "${_N:0:5}" {
		t.Errorf("static substitute = %q", got)
	}
	s := map[string]string{"PROJECT_ID": "p1", "_IMAGE": "img-${PROJECT_ID}", "_TAG": "${_IMAGE}:${_V:0:3}", "_V": "v1.2.3"}
	resolveDynamic(s)
	if s["_TAG"] != "img-p1:v1." {
		t.Errorf("_TAG = %q", s["_TAG"])
	}
}

// TestCreateBuildDynamicSubstitutionsAndSecretBuildArg (#1449).
func TestCreateBuildDynamicSubstitutionsAndSecretBuildArg(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 2)}
	f := &logFetcher{fakeFetcher: fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}, logs: map[string]string{}}
	cb := s.Builds(f, b).WithBuildSecrets(fakeBuildSecrets{"projects/proj-1/secrets/tok/versions/1": "s3cr3t"}, fakeKMS{}).CloudBuild()
	op, err := cb.CreateBuild(context.Background(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: &cbpb.Build{
		Source:        cbSource(),
		Options:       &cbpb.BuildOptions{DynamicSubstitutions: true, Env: []string{"PLAIN=p"}},
		Substitutions: map[string]string{"_REPO": "us-docker.pkg.dev/${PROJECT_ID}/r", "_VER": "release-1.4"},
		AvailableSecrets: &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{
			{VersionName: "projects/proj-1/secrets/tok/versions/1", Env: "TOKEN"}}},
		Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", SecretEnv: []string{"TOKEN"},
			Args: []string{"build", "-t", "${_REPO}/app:${_VER#release-}", "--build-arg", "TOKEN", "--build-arg", "PLAIN", "--build-arg", "NONE", "."}}},
	}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	spec := <-b.got
	if spec.LocalImage != localImageFor("us-docker.pkg.dev/proj-1/r/app:1.4") {
		t.Errorf("image = %q", spec.LocalImage)
	}
	if strings.Join(spec.BuildArgs, ",") != "TOKEN,PLAIN=p" || spec.Secrets["TOKEN"] != "s3cr3t" || len(spec.Secrets) != 1 {
		t.Errorf("build args = %q, secrets = %v", spec.BuildArgs, len(spec.Secrets))
	}
	if res, err := buildResult(t, s, op.GetName()); err != nil || res.GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("build = %v, %v", res.GetStatus(), err)
	}
}

// TestBuildApproval (#1449): a trigger needing approval starts its builds
// PENDING; ApproveBuild runs one, rejects another, and CancelBuild ends a
// third.
func TestBuildApproval(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 4)}
	f := &logFetcher{fakeFetcher: fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}, logs: map[string]string{}}
	cb := s.Builds(f, b).CloudBuild()
	ctx := context.WithValue(context.Background(), approvalCtxKey{}, true)
	tr, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: "projects/proj-a/locations/global",
		Trigger: &cbpb.BuildTrigger{Name: "gated", BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Source: cbSource(),
			Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", "x/y", "."}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !TriggerNeedsApproval(tr.GetResourceName()) {
		t.Fatal("approval not recorded")
	}
	run := func() *cbpb.Build {
		op, err := cb.RunBuildTrigger(context.Background(), &cbpb.RunBuildTriggerRequest{Name: tr.GetResourceName()})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.Operation(op.GetName())
		md := &cbpb.BuildOperationMetadata{}
		_ = got.GetMetadata().UnmarshalTo(md)
		return md.GetBuild()
	}
	pending := run()
	if pending.GetStatus() != cbpb.Build_PENDING || pending.GetApproval().GetState() != cbpb.BuildApproval_PENDING {
		t.Fatalf("pending build = %v %v", pending.GetStatus(), pending.GetApproval())
	}
	select {
	case <-b.got:
		t.Fatal("a pending build ran")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := cb.ApproveBuild(context.Background(), &cbpb.ApproveBuildRequest{Name: pending.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no decision = %v", err)
	}
	op, err := cb.ApproveBuild(context.Background(), &cbpb.ApproveBuildRequest{Name: pending.GetName(),
		ApprovalResult: &cbpb.ApprovalResult{Decision: cbpb.ApprovalResult_APPROVED, Comment: "ok"}})
	if err != nil || !op.GetDone() {
		t.Fatalf("ApproveBuild: %v", err)
	}
	<-b.got
	waitBuild := func(name string, want cbpb.Build_Status) *cbpb.Build {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if g, _ := cb.GetBuild(context.Background(), &cbpb.GetBuildRequest{Name: name}); g.GetStatus() == want {
				return g
			}
		}
		g, _ := cb.GetBuild(context.Background(), &cbpb.GetBuildRequest{Name: name})
		t.Fatalf("build %s = %v; want %v", name, g.GetStatus(), want)
		return nil
	}
	if g := waitBuild(pending.GetName(), cbpb.Build_SUCCESS); g.GetApproval().GetState() != cbpb.BuildApproval_APPROVED ||
		g.GetApproval().GetResult().GetComment() != "ok" {
		t.Errorf("approved build approval = %v", g.GetApproval())
	}
	if _, err := cb.ApproveBuild(context.Background(), &cbpb.ApproveBuildRequest{Name: pending.GetName(),
		ApprovalResult: &cbpb.ApprovalResult{Decision: cbpb.ApprovalResult_APPROVED}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("approving twice = %v", err)
	}
	rejected := run()
	if _, err := cb.ApproveBuild(context.Background(), &cbpb.ApproveBuildRequest{Name: rejected.GetName(),
		ApprovalResult: &cbpb.ApprovalResult{Decision: cbpb.ApprovalResult_REJECTED}}); err != nil {
		t.Fatal(err)
	}
	if g := waitBuild(rejected.GetName(), cbpb.Build_CANCELLED); g.GetApproval().GetState() != cbpb.BuildApproval_REJECTED {
		t.Errorf("rejected approval = %v", g.GetApproval())
	}
	cancelled := run()
	if g, err := cb.CancelBuild(context.Background(), &cbpb.CancelBuildRequest{Name: cancelled.GetName()}); err != nil ||
		g.GetStatus() != cbpb.Build_CANCELLED || g.GetApproval().GetState() != cbpb.BuildApproval_CANCELLED {
		t.Errorf("cancel pending = %v, %v", g, err)
	}
	// JSON answers carry approvalConfig.
	out := string(addApprovalConfig([]byte(`{"triggers":[{"resourceName":"` + tr.GetResourceName() + `"}]}`)))
	if !strings.Contains(out, `"approvalConfig":{"approvalRequired":true}`) {
		t.Errorf("list JSON = %s", out)
	}
}
