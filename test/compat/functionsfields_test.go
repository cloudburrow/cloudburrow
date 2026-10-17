//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fieldsPool makes a Cloud Build private pool (#1422) in the run parent's
// region, for a function's build to name.
func fieldsPool(ctx context.Context, t *testing.T, h *Harness) string {
	t.Helper()
	cb := cloudBuildClient(ctx, t, h)
	id := "fn-pool-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	op, err := cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: runParent(h), WorkerPoolId: id,
		WorkerPool: &cbpb.WorkerPool{}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	pool, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateWorkerPool.Wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cb.DeleteWorkerPool(context.Background(), &cbpb.DeleteWorkerPoolRequest{Name: pool.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	return pool.GetName()
}

const fieldsKMSKey = "projects/kms-proj/locations/us-central1/keyRings/ring/cryptoKeys/fn-key"

// covers: google.cloud.functions.v2.FunctionService/GenerateUploadUrl, google.cloud.functions.v2.FunctionService/CreateFunction, google.cloud.functions.v2.FunctionService/GetFunction, google.cloud.functions.v2.FunctionService/UpdateFunction, google.cloud.functions.v2.FunctionService/DeleteFunction, google.cloud.run.v2.Builds/SubmitBuild
//
// TestFunctionsFields (#1464): the official Cloud Functions v2 client, on an
// instance with --services run,storage,pubsub, through the fields that were
// refused UNIMPLEMENTED, with a source that is not a zip, so its build fails
// and no build is needed. GenerateUploadUrl takes a kms_key_name. A function
// with secret environment variables and volumes, a Cloud Build worker pool,
// a KMS key and a binary authorization policy is created; its build fails,
// and GetFunction reads every field back as set; an update of the policy
// alone is kept. A worker pool that does not exist is NOT_FOUND and a key
// that is not a CryptoKey's name INVALID_ARGUMENT. repo_source, a VPC
// connector and an audit log or Firestore trigger are UNIMPLEMENTED with the
// reason. SubmitBuild takes buildpack_build.runtime.
func TestFunctionsFields(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	fc := functionsClient(t, h)
	pool := fieldsPool(ctx, t, h)

	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent, KmsKeyName: fieldsKMSKey})
	if err != nil {
		t.Fatalf("GenerateUploadUrl with kms_key_name: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("this is not a zip archive"))
	if _, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent, KmsKeyName: "fn-key"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GenerateUploadUrl with a key that is not a CryptoKey's name = %v; want INVALID_ARGUMENT", err)
	}

	src := &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}
	build := func() *functionspb.BuildConfig {
		return &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Secrets", Source: src, WorkerPool: pool}
	}
	id := "sdk-fn-fields"
	name := parent + "/functions/" + id
	in := &functionspb.Function{
		BuildConfig: build(),
		ServiceConfig: &functionspb.ServiceConfig{
			SecretEnvironmentVariables: []*functionspb.SecretEnvVar{{Key: "API_KEY", ProjectId: h.Project(), Secret: "fn-api-key", Version: "1"}},
			SecretVolumes: []*functionspb.SecretVolume{{MountPath: "/etc/secrets", ProjectId: h.Project(), Secret: "fn-config",
				Versions: []*functionspb.SecretVolume_SecretVersion{{Version: "latest", Path: "config.json"}}}},
			BinaryAuthorizationPolicy: "default",
		},
		KmsKeyName: fieldsKMSKey,
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := fc.DeleteFunction(dctx, &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: in})
	if err != nil {
		t.Fatalf("CreateFunction with secrets, a worker pool, a KMS key and a binary authorization policy: %v", err)
	}
	if _, err := op.Wait(ctx); err == nil || !strings.Contains(err.Error(), "Build failed") {
		t.Fatalf("CreateFunction.Wait of a source that is not a zip = %v; want the build's error", err)
	}
	got, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetFunction: %v", err)
	}
	sc := got.GetServiceConfig()
	if got.GetBuildConfig().GetWorkerPool() != pool || got.GetKmsKeyName() != fieldsKMSKey ||
		sc.GetBinaryAuthorizationPolicy() != "default" ||
		len(sc.GetSecretEnvironmentVariables()) != 1 || sc.GetSecretEnvironmentVariables()[0].GetSecret() != "fn-api-key" ||
		sc.GetSecretEnvironmentVariables()[0].GetVersion() != "1" ||
		len(sc.GetSecretVolumes()) != 1 || sc.GetSecretVolumes()[0].GetMountPath() != "/etc/secrets" ||
		sc.GetSecretVolumes()[0].GetVersions()[0].GetPath() != "config.json" {
		t.Errorf("GetFunction = %v; want the worker pool, key, policy and secrets as set", got)
	}
	uop, err := fc.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{
		Function:   &functionspb.Function{Name: name, ServiceConfig: &functionspb.ServiceConfig{BinaryAuthorizationPolicy: "projects/" + h.Project() + "/platforms/cloudRun/policies/strict"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"service_config.binary_authorization_policy"}}})
	if err != nil {
		t.Fatalf("UpdateFunction of the policy: %v", err)
	}
	_, _ = uop.Wait(ctx) // its service was never built, so the rollout fails
	if g, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); err != nil ||
		!strings.HasSuffix(g.GetServiceConfig().GetBinaryAuthorizationPolicy(), "/policies/strict") || g.GetKmsKeyName() != fieldsKMSKey {
		t.Errorf("GetFunction after the update = %v, %v; want the new policy and the rest kept", g, err)
	}

	refused := []struct {
		what string
		fn   *functionspb.Function
		code codes.Code
		want string
	}{
		{"a missing worker pool", &functionspb.Function{BuildConfig: &functionspb.BuildConfig{Runtime: "go123", Source: src,
			WorkerPool: parent + "/workerPools/no-such-pool"}}, codes.NotFound, "not found"},
		{"a key that is not a CryptoKey's", &functionspb.Function{BuildConfig: build(), KmsKeyName: "fn-key"}, codes.InvalidArgument, "kms_key_name"},
		{"repo_source", &functionspb.Function{BuildConfig: &functionspb.BuildConfig{Runtime: "go123",
			Source: &functionspb.Source{Source: &functionspb.Source_RepoSource{RepoSource: &functionspb.RepoSource{RepoName: "r"}}}}},
			codes.Unimplemented, "Cloud Source Repositories"},
		{"a VPC connector", &functionspb.Function{BuildConfig: build(), ServiceConfig: &functionspb.ServiceConfig{
			VpcConnector: parent + "/connectors/c"}}, codes.Unimplemented, "Serverless VPC Access"},
		{"an audit log trigger", &functionspb.Function{BuildConfig: build(),
			EventTrigger: &functionspb.EventTrigger{EventType: "google.cloud.audit.log.v1.written"}}, codes.Unimplemented, "Cloud Audit Logs"},
		{"a Firestore trigger", &functionspb.Function{BuildConfig: build(),
			EventTrigger: &functionspb.EventTrigger{EventType: "google.cloud.firestore.document.v1.written"}}, codes.Unimplemented, "emulators"},
	}
	for _, r := range refused {
		_, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "sdk-fn-refused", Function: r.fn})
		if status.Code(err) != r.code || !strings.Contains(err.Error(), r.want) {
			t.Errorf("CreateFunction with %s = %v; want %v naming %q", r.what, err, r.code, r.want)
		}
	}

	bc := buildsClient(t, h)
	resp, err := bc.SubmitBuild(ctx, &runpb.SubmitBuildRequest{Parent: parent,
		Source:   &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: up.GetStorageSource().GetBucket(), Object: up.GetStorageSource().GetObject()}},
		ImageUri: "us-central1-docker.pkg.dev/" + h.Project() + "/fn/runtime-probe",
		BuildType: &runpb.SubmitBuildRequest_BuildpackBuild{BuildpackBuild: &runpb.SubmitBuildRequest_BuildpacksBuild{
			Runtime: "go123", FunctionTarget: "Hello"}}})
	if err != nil || resp.GetBuildOperation().GetName() == "" {
		t.Errorf("SubmitBuild with buildpack_build.runtime = %v, %v; want it accepted", resp, err)
	}
}

// covers: google.cloud.functions.v1.CloudFunctionsService/GenerateUploadUrl, google.cloud.functions.v1.CloudFunctionsService/CreateFunction, google.cloud.functions.v1.CloudFunctionsService/GetFunction, google.cloud.functions.v1.CloudFunctionsService/DeleteFunction
//
// TestFunctionsV1Fields (#1464): the official Cloud Functions v1 client,
// through 1st gen's fields that were refused: a function with
// secretEnvironmentVariables, secretVolumes, a buildWorkerPool and a
// kmsKeyName, whose source is not a zip, fails its build and reads every
// field back as set, and through v2 as its 2nd-gen fields; a
// sourceRepository, a vpcConnector and a Firestore event trigger are
// UNIMPLEMENTED with the reason.
func TestFunctionsV1Fields(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	c := functionsV1Client(t, h)
	fc := functionsClient(t, h)
	pool := fieldsPool(ctx, t, h)

	up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent, KmsKeyName: fieldsKMSKey})
	if err != nil {
		t.Fatalf("GenerateUploadUrl with kmsKeyName: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("this is not a zip archive"))
	name := parent + "/functions/sdk-v1-fields"
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := c.DeleteFunction(dctx, &fv1.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	fn := func(n string) *fv1.CloudFunction {
		return &fv1.CloudFunction{Name: n, Runtime: "go123", EntryPoint: "Secrets",
			SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
			Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}}
	}
	in := fn(name)
	in.KmsKeyName, in.BuildWorkerPool = fieldsKMSKey, pool
	in.SecretEnvironmentVariables = []*fv1.SecretEnvVar{{Key: "API_KEY", Secret: "fn-api-key", Version: "1"}}
	in.SecretVolumes = []*fv1.SecretVolume{{MountPath: "/etc/secrets", Secret: "fn-config"}}
	op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: in})
	if err != nil {
		t.Fatalf("CreateFunction with secrets, a worker pool and a KMS key: %v", err)
	}
	if _, err := op.Wait(ctx); err == nil || !strings.Contains(err.Error(), "Build failed") {
		t.Fatalf("CreateFunction.Wait of a source that is not a zip = %v; want the build's error", err)
	}
	got, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetFunction: %v", err)
	}
	if got.GetKmsKeyName() != fieldsKMSKey || got.GetBuildWorkerPool() != pool ||
		len(got.GetSecretEnvironmentVariables()) != 1 || got.GetSecretEnvironmentVariables()[0].GetKey() != "API_KEY" ||
		len(got.GetSecretVolumes()) != 1 || got.GetSecretVolumes()[0].GetMountPath() != "/etc/secrets" {
		t.Errorf("GetFunction = %v; want the key, pool and secrets as set", got)
	}
	if v2, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); err != nil ||
		v2.GetBuildConfig().GetWorkerPool() != pool || v2.GetKmsKeyName() != fieldsKMSKey ||
		v2.GetServiceConfig().GetSecretVolumes()[0].GetSecret() != "fn-config" {
		t.Errorf("the v2 GetFunction = %v, %v; want the same fields as 2nd gen's", v2, err)
	}

	repo := fn(parent + "/functions/sdk-v1-refused")
	repo.SourceCode = &fv1.CloudFunction_SourceRepository{SourceRepository: &fv1.SourceRepository{
		Url: "https://source.developers.google.com/projects/p/repos/r/moveable-aliases/main/paths/"}}
	vpc := fn(parent + "/functions/sdk-v1-refused")
	vpc.VpcConnector = parent + "/connectors/c"
	fs := fn(parent + "/functions/sdk-v1-refused")
	fs.Trigger = &fv1.CloudFunction_EventTrigger{EventTrigger: &fv1.EventTrigger{
		EventType: "providers/cloud.firestore/eventTypes/document.write", Resource: "projects/" + h.Project() + "/databases/(default)/documents/users/{id}"}}
	for what, r := range map[string]struct {
		fn   *fv1.CloudFunction
		want string
	}{"a sourceRepository": {repo, "Cloud Source Repositories"}, "a vpcConnector": {vpc, "Serverless VPC Access"},
		"a Firestore trigger": {fs, "Firestore emulator"}} {
		_, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: r.fn})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), r.want) {
			t.Errorf("CreateFunction with %s = %v; want UNIMPLEMENTED naming %q", what, err, r.want)
		}
	}
}

// covers: google.cloud.functions.v2.FunctionService/CreateFunction, google.cloud.functions.v2.FunctionService/DeleteFunction
//
// TestFunctionsSecretsBuildsAndMounts (#1464), gated as the Functions tests
// are: testdata/function's Secrets entry point, built with Google's builder
// on a Cloud Build worker pool, with a secret environment variable and a
// secret volume from the instance's Secret Manager, a KMS key and a binary
// authorization policy. It becomes ACTIVE, and answers the variable's value
// and the mounted file's, read through the ingress.
func TestFunctionsSecretsBuildsAndMounts(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSecrets)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	requireAmd64Functions(ctx, t)
	parent := runParent(h)
	fc := functionsClient(t, h)
	pool := fieldsPool(ctx, t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	envSecret, fileSecret := "fn-env-"+suffix, "fn-file-"+suffix
	runVolumeSecret(t, h, envSecret, "env-value-"+suffix)
	runVolumeSecret(t, h, fileSecret, "file-value-"+suffix)

	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent, KmsKeyName: fieldsKMSKey})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), zipFunction(t, filepath.Join(moduleRoot(t), "testdata", "function")))
	id := "sdk-fn-secrets"
	name := parent + "/functions/" + id
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := fc.DeleteFunction(dctx, &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Secrets", WorkerPool: pool,
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}},
		ServiceConfig: &functionspb.ServiceConfig{
			EnvironmentVariables:       map[string]string{"SECRET_ENV": "API_KEY", "SECRET_FILE": "/etc/secrets/value.txt"},
			SecretEnvironmentVariables: []*functionspb.SecretEnvVar{{Key: "API_KEY", Secret: envSecret, Version: "latest"}},
			SecretVolumes: []*functionspb.SecretVolume{{MountPath: "/etc/secrets", Secret: fileSecret,
				Versions: []*functionspb.SecretVolume_SecretVersion{{Version: "1", Path: "value.txt"}}}},
			BinaryAuthorizationPolicy: "default",
		},
		KmsKeyName: fieldsKMSKey,
	}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	fn, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFunction.Wait: %v", err)
	}
	if fn.GetState() != functionspb.Function_ACTIVE || fn.GetUrl() == "" {
		t.Fatalf("the created function = %v; want it ACTIVE with its url", fn)
	}
	code, body := functionRequest(t, ingress(t), hostOf(t, fn.GetUrl()), "", nil)
	var got map[string]string
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("GET / = %d %q", code, body)
	}
	if got["env"] != "env-value-"+suffix || got["file"] != "file-value-"+suffix {
		t.Errorf("the function read %v; want the secret variable's and the mounted file's values", got)
	}
}
