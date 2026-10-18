package run

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestFunctionServiceMountsSecrets (#1464): a function's secret
// environment variables are its service's secretKeyRef variables and its
// secret volumes the service's secret volumes, mounted where the function
// asked, from the function's project unless one is named, a project number
// read as the function's project; its binary authorization policy is the
// service's binaryAuthorization. The service renders, so the pod mounts them.
func TestFunctionServiceMountsSecrets(t *testing.T) {
	const project = "proj-1"
	num := resource.ProjectNumber(project)
	fn := &functionspb.Function{Name: "projects/" + project + "/locations/us-central1/functions/f",
		ServiceConfig: &functionspb.ServiceConfig{
			SecretEnvironmentVariables: []*functionspb.SecretEnvVar{
				{Key: "API_KEY", Secret: "api-key", Version: "2"},
				{Key: "OTHER", ProjectId: "other-proj", Secret: "tok"},
			},
			SecretVolumes: []*functionspb.SecretVolume{
				{MountPath: "/etc/secrets", ProjectId: strconv.FormatInt(num, 10), Secret: "cfg",
					Versions: []*functionspb.SecretVolume_SecretVersion{{Version: "1", Path: "cfg.json"}}},
				{MountPath: "/etc/latest", Secret: "plain"},
			},
			BinaryAuthorizationPolicy: "default",
		}}
	svc := functionService(fn, "gcr.io/p/img:1")
	c := svc.GetTemplate().GetContainers()[0]
	env := map[string]*runpb.SecretKeySelector{}
	for _, e := range c.GetEnv() {
		env[e.GetName()] = e.GetValueSource().GetSecretKeyRef()
	}
	if r := env["API_KEY"]; r.GetSecret() != "projects/proj-1/secrets/api-key" || r.GetVersion() != "2" {
		t.Errorf("API_KEY = %v", r)
	}
	if r := env["OTHER"]; r.GetSecret() != "projects/other-proj/secrets/tok" || r.GetVersion() != "latest" {
		t.Errorf("OTHER = %v; want the named project's secret, latest", r)
	}
	vols := svc.GetTemplate().GetVolumes()
	if len(vols) != 2 || vols[0].GetSecret().GetSecret() != "projects/proj-1/secrets/cfg" ||
		len(vols[0].GetSecret().GetItems()) != 1 || vols[0].GetSecret().GetItems()[0].GetPath() != "cfg.json" ||
		vols[1].GetSecret().GetSecret() != "projects/proj-1/secrets/plain" || len(vols[1].GetSecret().GetItems()) != 0 {
		t.Errorf("volumes = %v", vols)
	}
	if m := c.GetVolumeMounts(); len(m) != 2 || m[0].GetMountPath() != "/etc/secrets" || m[0].GetName() != vols[0].GetName() {
		t.Errorf("mounts = %v", m)
	}
	if !svc.GetBinaryAuthorization().GetUseDefault() {
		t.Errorf("binaryAuthorization = %v; want use_default", svc.GetBinaryAuthorization())
	}
	svc.Name = "projects/proj-1/locations/us-central1/services/f"
	out, err := ToKnative(svc, "ns", "t", fakeResolver{})
	if err != nil {
		t.Fatalf("ToKnative: %v", err)
	}
	for _, want := range []string{"secretKeyRef:", "cb-secret-proj-1-projects/proj-1/secrets/api-key", `mountPath: "/etc/secrets"`, `path: "cfg.json"`, "readOnly: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, out)
		}
	}
	fn.ServiceConfig.BinaryAuthorizationPolicy = "projects/proj-1/platforms/cloudRun/policies/p"
	if p := functionService(fn, "i").GetBinaryAuthorization().GetPolicy(); p != fn.ServiceConfig.BinaryAuthorizationPolicy {
		t.Errorf("binaryAuthorization.policy = %q", p)
	}
}

// TestFunctionFieldsAreChecked (#1464): repo_source and a VPC connector are
// refused with the reason; a worker pool must be one of Cloud Build's in the
// function's region; a KMS key must be a CryptoKey's name; secrets need
// their key, secret and an absolute mount path.
func TestFunctionFieldsAreChecked(t *testing.T) {
	pool := "projects/proj-1/locations/us-central1/workerPools/unit-fn-pool"
	triggerStore.mu.Lock()
	triggerStore.pools[pool] = &cbpb.WorkerPool{Name: pool}
	triggerStore.mu.Unlock()
	t.Cleanup(func() {
		triggerStore.mu.Lock()
		delete(triggerStore.pools, pool)
		triggerStore.mu.Unlock()
	})
	base := func() *functionspb.Function {
		return &functionspb.Function{BuildConfig: &functionspb.BuildConfig{}, ServiceConfig: &functionspb.ServiceConfig{}}
	}
	for _, c := range []struct {
		name string
		mut  func(*functionspb.Function)
		code codes.Code
		want string
	}{
		{"repo source", func(f *functionspb.Function) {
			f.BuildConfig.Source = &functionspb.Source{Source: &functionspb.Source_RepoSource{RepoSource: &functionspb.RepoSource{}}}
		}, codes.Unimplemented, "Cloud Source Repositories"},
		{"vpc connector", func(f *functionspb.Function) { f.ServiceConfig.VpcConnector = "projects/p/locations/l/connectors/c" }, codes.Unimplemented, "Serverless VPC Access"},
		{"pool", func(f *functionspb.Function) { f.BuildConfig.WorkerPool = pool }, codes.OK, ""},
		{"missing pool", func(f *functionspb.Function) { f.BuildConfig.WorkerPool = pool + "-x" }, codes.NotFound, "not found"},
		{"bad pool", func(f *functionspb.Function) { f.BuildConfig.WorkerPool = "pool" }, codes.InvalidArgument, "build_config.worker_pool"},
		{"kms", func(f *functionspb.Function) { f.KmsKeyName = "projects/p/locations/us/keyRings/r/cryptoKeys/k" }, codes.OK, ""},
		{"bad kms", func(f *functionspb.Function) { f.KmsKeyName = "k" }, codes.InvalidArgument, "kms_key_name"},
		{"secret env", func(f *functionspb.Function) {
			f.ServiceConfig.SecretEnvironmentVariables = []*functionspb.SecretEnvVar{{Key: "K", Secret: "s", Version: "1"}}
		}, codes.OK, ""},
		{"secret env no key", func(f *functionspb.Function) {
			f.ServiceConfig.SecretEnvironmentVariables = []*functionspb.SecretEnvVar{{Secret: "s"}}
		}, codes.InvalidArgument, "key and secret"},
		{"relative mount", func(f *functionspb.Function) {
			f.ServiceConfig.SecretVolumes = []*functionspb.SecretVolume{{MountPath: "etc", Secret: "s"}}
		}, codes.InvalidArgument, "mount_path"},
		{"escaping path", func(f *functionspb.Function) {
			f.ServiceConfig.SecretVolumes = []*functionspb.SecretVolume{{MountPath: "/etc/s", Secret: "s",
				Versions: []*functionspb.SecretVolume_SecretVersion{{Version: "1", Path: "../x"}}}}
		}, codes.InvalidArgument, "relative path"},
		{"binary authorization", func(f *functionspb.Function) { f.ServiceConfig.BinaryAuthorizationPolicy = "default" }, codes.OK, ""},
	} {
		fn := base()
		c.mut(fn)
		err := checkFunction("us-central1", fn)
		if err == nil && c.code != codes.OK || err != nil && (apierror.From(err).Code != c.code || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v; want %v naming %q", c.name, err, c.code, c.want)
		}
	}
	if err := checkFunction("europe-west1", &functionspb.Function{BuildConfig: &functionspb.BuildConfig{WorkerPool: pool}}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("a pool in another region = %v; want INVALID_ARGUMENT", err)
	}
}

// TestV1FunctionFields (#1464): a v1 function's worker pool, KMS key and
// secrets are its 2nd-gen function's; sourceRepository and a VPC connector
// are refused with the reason.
func TestV1FunctionFields(t *testing.T) {
	name := "projects/proj-1/locations/us-central1/functions/f"
	in := &fv1.CloudFunction{Name: name, Runtime: "go123", SourceCode: &fv1.CloudFunction_SourceArchiveUrl{SourceArchiveUrl: "gs://b/o.zip"},
		Trigger:                    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}},
		KmsKeyName:                 "projects/p/locations/us/keyRings/r/cryptoKeys/k",
		BuildWorkerPool:            "projects/proj-1/locations/us-central1/workerPools/w",
		SecretEnvironmentVariables: []*fv1.SecretEnvVar{{Key: "K", Secret: "s", Version: "1"}},
		SecretVolumes: []*fv1.SecretVolume{{MountPath: "/etc/s", Secret: "v",
			Versions: []*fv1.SecretVolume_SecretVersion{{Version: "2", Path: "f"}}}}}
	fn, err := toV2(name, in)
	if err != nil {
		t.Fatalf("toV2: %v", err)
	}
	sc := fn.GetServiceConfig()
	if fn.GetKmsKeyName() != in.GetKmsKeyName() || fn.GetBuildConfig().GetWorkerPool() != in.GetBuildWorkerPool() ||
		sc.GetSecretEnvironmentVariables()[0].GetKey() != "K" || sc.GetSecretVolumes()[0].GetVersions()[0].GetPath() != "f" {
		t.Errorf("toV2 = %v", fn)
	}
	for _, c := range []struct {
		mut  func(*fv1.CloudFunction)
		want string
	}{
		{func(f *fv1.CloudFunction) {
			f.SourceCode = &fv1.CloudFunction_SourceRepository{SourceRepository: &fv1.SourceRepository{Url: "u"}}
		}, "Cloud Source Repositories"},
		{func(f *fv1.CloudFunction) { f.VpcConnector = "c" }, "Serverless VPC Access"},
		{func(f *fv1.CloudFunction) { f.Network = "n" }, "Serverless VPC Access"},
	} {
		f := &fv1.CloudFunction{}
		c.mut(f)
		if err := unsupportedV1(f); apierror.From(err).Code != codes.Unimplemented || !strings.Contains(err.Error(), c.want) {
			t.Errorf("unsupportedV1 = %v; want UNIMPLEMENTED naming %q", err, c.want)
		}
	}
	if _, err := v2Trigger("p", &fv1.EventTrigger{EventType: "providers/cloud.firestore/eventTypes/document.write", Resource: "r"}); apierror.From(err).Code != codes.Unimplemented ||
		!strings.Contains(err.Error(), "Firestore emulator") {
		t.Errorf("a Firestore trigger = %v; want UNIMPLEMENTED with the reason", err)
	}
}

// TestV1FailedCreateAnswer (#1620): a v1 create whose build fails answers
// as Google does, the shape google_cloudfunctions_function relies on. The
// operation is done with the build's error and no response, and the
// function stays, OFFLINE with what the caller sent, so the provider's
// read finds it (it is not NOT_FOUND) and its delete removes it.
func TestV1FailedCreateAnswer(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 1), fail: errors.New("the source is not a zip archive")}
	v := s.Functions(s.Builds(fakeFetcher{objects: map[string]string{"b/bad.zip": "not a zip"}}, b), func() string { return "" }).V1()
	const loc = "projects/proj-1620/locations/us-central1"
	name := loc + "/functions/v1-failed-create"
	ctx := context.Background()
	op, err := v.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: loc, Function: &fv1.CloudFunction{
		Name: name, Runtime: "go123", EntryPoint: "Hello",
		SourceCode:           &fv1.CloudFunction_SourceArchiveUrl{SourceArchiveUrl: "gs://b/bad.zip"},
		Trigger:              &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}},
		EnvironmentVariables: map[string]string{"MODE": "bad"}}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	t.Cleanup(func() {
		functions.mu.Lock()
		delete(functions.fns, name)
		functions.mu.Unlock()
	})
	<-b.got
	waitOp(t, s, op.GetName())
	done, err := s.Operation(op.GetName())
	if err != nil {
		t.Fatalf("Operation: %v", err)
	}
	if !done.GetDone() || done.GetError() == nil || done.GetResponse() != nil ||
		!strings.HasPrefix(done.GetError().GetMessage(), "Build failed: ") || !strings.Contains(done.GetError().GetMessage(), "not a zip") {
		t.Errorf("the create's operation = %v; want it done with the build's error and no response", done)
	}
	fn, err := v.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetFunction after the failed create = %v; want the function, OFFLINE", err)
	}
	if fn.GetStatus() != fv1.CloudFunctionStatus_OFFLINE || fn.GetHttpsTrigger() == nil || fn.GetSourceArchiveUrl() != "gs://b/bad.zip" ||
		fn.GetRuntime() != "go123" || fn.GetEnvironmentVariables()["MODE"] != "bad" {
		t.Errorf("GetFunction after the failed create = %v; want it OFFLINE with what was sent", fn)
	}
	del, err := v.DeleteFunction(ctx, &fv1.DeleteFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteFunction of the failed function: %v", err)
	}
	waitOp(t, s, del.GetName())
	if _, err := v.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name}); apierror.From(err).Code != codes.NotFound {
		t.Errorf("GetFunction after delete = %v; want NOT_FOUND", err)
	}
}

// TestOtherEventTypesSayWhy (#1464): an event type no served service emits
// is refused with the reason.
func TestOtherEventTypesSayWhy(t *testing.T) {
	on := (&FunctionsServer{}).WithEvents(Events{})
	for typ, want := range map[string]string{
		"google.cloud.audit.log.v1.written":              "Cloud Audit Logs",
		"google.cloud.firestore.document.v1.written":     "emulators",
		"google.firebase.auth.user.v1.created":           "Firebase",
		"google.cloud.cloudbuild.build.v1.statusChanged": "not served",
	} {
		err := on.checkEventTrigger(&functionspb.EventTrigger{EventType: typ})
		if apierror.From(err).Code != codes.Unimplemented || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), typ) {
			t.Errorf("%s = %v; want UNIMPLEMENTED naming it and %q", typ, err, want)
		}
	}
}

// TestSubmitBuildRuntime (#1464): buildpack_build.runtime is the builder's
// GOOGLE_RUNTIME, and a GOOGLE_RUNTIME the caller set wins.
func TestSubmitBuildRuntime(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 2)}
	bs := s.Builds(fakeFetcher{objects: map[string]string{"b/src.tgz": "x"}}, b)
	for _, c := range []struct {
		env  map[string]string
		want string
	}{{nil, "go123"}, {map[string]string{"GOOGLE_RUNTIME": "go124"}, "go124"}} {
		req := submit("gcr.io/p/unit-runtime:v1")
		req.BuildType = &runpb.SubmitBuildRequest_BuildpackBuild{BuildpackBuild: &runpb.SubmitBuildRequest_BuildpacksBuild{
			Runtime: "go123", EnvironmentVariables: c.env}}
		resp, err := bs.SubmitBuild(context.Background(), req)
		if err != nil {
			t.Fatalf("SubmitBuild: %v", err)
		}
		if spec := <-b.got; spec.Env["GOOGLE_RUNTIME"] != c.want {
			t.Errorf("GOOGLE_RUNTIME = %q; want %q", spec.Env["GOOGLE_RUNTIME"], c.want)
		}
		waitOp(t, s, resp.GetBuildOperation().GetName())
	}
}
