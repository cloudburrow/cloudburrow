package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fakeBuildSecrets map[string]string

func (f fakeBuildSecrets) AccessSecretVersion(_ context.Context, name string) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, errors.New("NotFound: " + name)
	}
	return []byte(v), nil
}

type fakeKMS struct{}

func (fakeKMS) Decrypt(_ context.Context, key string, ct []byte) ([]byte, error) {
	return []byte("dec(" + string(ct) + ")"), nil
}

// echoBuilder is a stepBuilder whose steps print their secrets, so the
// test sees the log mask them.
type echoBuilder struct{ stepBuilder }

func (b *echoBuilder) OpenWorkspace(ctx context.Context, id string, archive io.Reader) (BuildWorkspace, error) {
	if _, err := b.stepBuilder.OpenWorkspace(ctx, id, archive); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *echoBuilder) RunStep(ctx context.Context, spec StepSpec) error {
	for _, k := range []string{"TOKEN", "OTHER", "INLINE"} {
		if v, ok := spec.Secrets[k]; ok {
			fmt.Fprintf(spec.Log, "%s=%s\n", k, v)
		}
	}
	return b.stepBuilder.RunStep(ctx, spec)
}

// TestCreateBuildSecretsScriptAndAllowFailure (#1407): availableSecrets
// are read at build time and given to the steps whose secretEnv names
// them, masked in the log; a script step runs its file by its #! line or
// /bin/sh; allowFailure and allowExitCodes leave the build SUCCESS with the
// step FAILURE and its exit code.
func TestCreateBuildSecretsScriptAndAllowFailure(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &echoBuilder{stepBuilder{fakeBuilder: fakeBuilder{got: make(chan BuildSpec, 2)}}}
	logs := &logFetcher{fakeFetcher: fakeFetcher{objects: map[string]string{}}, logs: map[string]string{}}
	cb := s.Builds(logs, b).AllowHostSteps(true).
		WithBuildSecrets(fakeBuildSecrets{"projects/proj-1/secrets/tok/versions/latest": "s3cr3t-value"}, fakeKMS{}).CloudBuild()
	f := false
	op, err := cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: &cbpb.Build{
		LogsBucket: "gs://logs",
		Options: &cbpb.BuildOptions{AutomapSubstitutions: true, Env: []string{"G=global", "O=opt"},
			SecretEnv: []string{"INLINE"}},
		Substitutions: map[string]string{"_X": "x"},
		AvailableSecrets: &cbpb.Secrets{
			SecretManager: []*cbpb.SecretManagerSecret{{VersionName: "projects/$PROJECT_ID/secrets/tok/versions/latest", Env: "TOKEN"}},
			Inline: []*cbpb.InlineSecret{{KmsKeyName: "projects/proj-1/locations/global/keyRings/r/cryptoKeys/k",
				EnvMap: map[string][]byte{"INLINE": []byte("ct")}}}},
		Steps: []*cbpb.BuildStep{
			{Name: "alpine", Entrypoint: "sh", Args: []string{"-c", "x"}, SecretEnv: []string{"TOKEN"}, Env: []string{"O=step"}},
			{Name: "alpine", Script: "echo plain $_X"},
			{Name: "python", Script: "#!/usr/bin/env python\nprint(1)", AutomapSubstitutions: &f},
			{Name: "alpine", Args: []string{"fail", "x"}, AllowFailure: true},
			{Name: "alpine", Args: []string{"fail", "y"}, AllowExitCodes: []int32{1, 3}},
		}}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	res, err := buildResult(t, s, op.GetName())
	if err != nil || res.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("build = %v, %v", res, err)
	}
	for _, i := range []int{3, 4} {
		if st := res.GetSteps()[i]; st.GetStatus() != cbpb.Build_FAILURE || st.GetExitCode() != 3 {
			t.Errorf("allowed step %d = %v", i, st)
		}
	}
	specs := map[int]StepSpec{}
	b.mu.Lock()
	for _, sp := range b.specs {
		specs[sp.Index] = sp
	}
	b.mu.Unlock()
	s0 := specs[0]
	env := strings.Join(s0.Env, " ")
	if s0.Secrets["TOKEN"] != "s3cr3t-value" || s0.Secrets["INLINE"] != "dec(ct)" || strings.Contains(env, "s3cr3t") ||
		!strings.HasSuffix(env, "G=global O=opt O=step") || !strings.Contains(env, "_X=x") {
		t.Errorf("step 0 = %+v", s0)
	}
	if sp := specs[1]; sp.Script != "echo plain $_X" || sp.Entrypoint != "/bin/sh" || len(sp.Args) != 1 || sp.Args[0] != ScriptPath {
		t.Errorf("sh script step = %+v", sp)
	}
	if sp := specs[2]; sp.Entrypoint != ScriptPath || len(sp.Args) != 0 || strings.Contains(strings.Join(sp.Env, " "), "_X=") {
		t.Errorf("#! script step, automap off = %+v", sp)
	}
	log, _ := BuildLog(res.GetName())
	if strings.Contains(log, "s3cr3t-value") || strings.Contains(log, "dec(ct)") || !strings.Contains(log, "TOKEN="+secretMask) {
		t.Errorf("the log does not mask the secrets:\n%s", log)
	}
	logs.mu.Lock()
	for name, data := range logs.logs {
		if strings.Contains(data, "s3cr3t-value") {
			t.Errorf("the log object %s holds the secret", name)
		}
	}
	logs.mu.Unlock()

	// A failure allowExitCodes does not list fails the build.
	op, err = cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: "alpine", Args: []string{"fail", "x"}, AllowFailure: true, AllowExitCodes: []int32{2}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := buildResult(t, s, op.GetName()); res.GetStatus() != cbpb.Build_FAILURE {
		t.Errorf("exit 3 with allowExitCodes [2] = %v", res.GetStatus())
	}

	// A secret that cannot be read fails the build before its steps.
	bad := &cbpb.Build{AvailableSecrets: &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{
		{VersionName: "projects/proj-1/secrets/missing/versions/1", Env: "TOKEN"}}},
		Steps: []*cbpb.BuildStep{{Name: "alpine", Args: []string{"echo"}, SecretEnv: []string{"TOKEN"}}}}
	op, err = cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: bad})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := buildResult(t, s, op.GetName()); res.GetStatus() != cbpb.Build_FAILURE ||
		!strings.Contains(res.GetStatusDetail(), "failed to access secret version") || res.GetSteps()[0].GetStatus() != cbpb.Build_QUEUED {
		t.Errorf("a missing secret = %v", res)
	}

	for name, in := range map[string]*cbpb.Build{
		"undefined secretEnv": {Steps: []*cbpb.BuildStep{{Name: "alpine", SecretEnv: []string{"NOPE"}}}},
		"script with args":    {Steps: []*cbpb.BuildStep{{Name: "alpine", Script: "echo", Args: []string{"x"}}}},
		"script entrypoint":   {Steps: []*cbpb.BuildStep{{Name: "alpine", Script: "echo", Entrypoint: "sh"}}},
		"exit code 256":       {Steps: []*cbpb.BuildStep{{Name: "alpine", AllowExitCodes: []int32{256}}}},
		"bad versionName": {AvailableSecrets: &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{{VersionName: "tok", Env: "T"}}},
			Steps: []*cbpb.BuildStep{{Name: "alpine"}}},
		"duplicate env": {AvailableSecrets: &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{
			{VersionName: "projects/p/secrets/a/versions/1", Env: "T"}, {VersionName: "projects/p/secrets/b/versions/1", Env: "T"}}},
			Steps: []*cbpb.BuildStep{{Name: "alpine"}}},
		"options.secretEnv undefined": {Options: &cbpb.BuildOptions{SecretEnv: []string{"X"}}, Steps: []*cbpb.BuildStep{{Name: "alpine"}}},
	} {
		_, err := cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: proto.Clone(in).(*cbpb.Build)})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v; want INVALID_ARGUMENT", name, err)
		}
	}
	// A docker build step the local builder runs cannot take secretEnv.
	_, err = cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-1", Build: &cbpb.Build{
		Source: cbSource(), AvailableSecrets: &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{
			{VersionName: "projects/p/secrets/a/versions/1", Env: "T"}}},
		Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", "x", "."}, SecretEnv: []string{"T"}}}}})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("secretEnv on a docker build step = %v; want UNIMPLEMENTED", err)
	}
}

func TestSecretLines(t *testing.T) {
	got := secretLines("line1\nline2\n")
	if strings.Join(got, "|") != "line1\nline2|line1|line2" {
		t.Errorf("secretLines = %q", got)
	}
	if secretLines("\n") != nil {
		t.Errorf("an empty secret masks something")
	}
}
