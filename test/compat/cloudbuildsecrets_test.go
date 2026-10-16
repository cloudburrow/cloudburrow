//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// buildSecret makes a Secret Manager secret holding value, through the
// official client, and returns its id.
func buildSecret(ctx context.Context, t *testing.T, h *Harness, value string) string {
	t.Helper()
	sm := secretsClient(t, h)
	id := "cb-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secretsParent(h) + "/secrets/" + id})
	})
	if _, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secretsParent(h) + "/secrets/" + id,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	return id
}

// buildLogText reads a build's log-{id}.txt from its logs bucket.
func buildLogText(ctx context.Context, t *testing.T, h *Harness, bucket, prefix, id string) string {
	t.Helper()
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(),
		option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	r, err := sc.Bucket(bucket).Object(prefix + "log-" + id + ".txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("the build's log: %v", err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestCloudBuildSecretsScriptAndAllowFailure (#1407), with the official
// Cloud Build client over gRPC on the Run port, on an instance started with
// --cloudbuild-host-steps:
//   - availableSecrets.secretManager reads a version from the instance's
//     Secret Manager at build time, and a step whose secretEnv names it gets
//     it as a variable: the step prints its SHA-256, which the log carries,
//     and the value itself, which the log masks; availableSecrets.inline
//     decrypts a ciphertext with the instance's Cloud KMS (when it runs);
//   - a script step runs through /bin/sh, and one with a #! line through
//     that interpreter; automapSubstitutions gives it the substitutions;
//   - a step with allowFailure that exits 3, and one whose allowExitCodes
//     lists its exit code, are FAILURE with exitCode 3 while the build is
//     SUCCESS; a script with args is INVALID_ARGUMENT.
func TestCloudBuildSecretsScriptAndAllowFailure(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(h.Context(), 6*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	value := "s3cr3t-" + uuid.NewString()[:8]
	secret := buildSecret(ctx, t, h, value)
	sum := sha256.Sum256([]byte(value))
	bucket := uploadSource(ctx, t, h, "unused.tar.gz", sourceArchive(t, map[string][]byte{"x": []byte("x")}))
	img := stepImage()

	avail := &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{
		{VersionName: "projects/$PROJECT_ID/secrets/" + secret + "/versions/latest", Env: "TOKEN"}}}
	steps := []*cbpb.BuildStep{
		{Id: "secret", Name: img, Entrypoint: "sh", SecretEnv: []string{"TOKEN"},
			Args: []string{"-c", `printf %s "$$TOKEN" | sha256sum; echo "raw:$$TOKEN"`}},
		{Id: "sh", Name: img, Script: "echo \"script $_WHO in $(pwd)\"", AutomapSubstitutions: ptr(true)},
		{Id: "shebang", Name: img, Script: "#!/bin/busybox ash\necho \"shebang ${0}\""},
		{Id: "allowed", Name: img, Entrypoint: "sh", Args: []string{"-c", "exit 3"}, AllowFailure: true},
		{Id: "codes", Name: img, Entrypoint: "sh", Args: []string{"-c", "exit 3"}, AllowExitCodes: []int32{1, 3}},
	}
	// availableSecrets.inline with the instance's Cloud KMS, when the
	// instance runs it.
	if addr := runShardEndpointIfSet(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS); addr != "" {
		kc, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kc.Close() })
		ring := "cb" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
		parent := "projects/" + h.Project() + "/locations/global"
		if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring}); err != nil {
			t.Fatalf("CreateKeyRing: %v", err)
		}
		key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/" + ring, CryptoKeyId: "k",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatalf("CreateCryptoKey: %v", err)
		}
		enc, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("inline-" + value)})
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		avail.Inline = []*cbpb.InlineSecret{{KmsKeyName: key.GetName(), EnvMap: map[string][]byte{"INLINE": enc.GetCiphertext()}}}
		steps[0].SecretEnv = append(steps[0].SecretEnv, "INLINE")
		steps[0].Args[1] += `; printf %s "$$INLINE" | sha256sum`
	}

	op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{
		LogsBucket: "gs://" + bucket + "/logs", Substitutions: map[string]string{"_WHO": "runs"},
		AvailableSecrets: avail, Steps: steps}})
	skipWithoutHostSteps(t, err)
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("the build = %v, %v", b, err)
	}
	for _, i := range []int{3, 4} {
		if st := b.GetSteps()[i]; st.GetStatus() != cbpb.Build_FAILURE || st.GetExitCode() != 3 {
			t.Errorf("allowed step %d = %v; want FAILURE with exitCode 3", i, st)
		}
	}
	for _, i := range []int{0, 1, 2} {
		if st := b.GetSteps()[i]; st.GetStatus() != cbpb.Build_SUCCESS {
			t.Errorf("step %d = %v", i, st)
		}
	}
	got, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Id: b.GetId()})
	if err != nil || got.GetSteps()[3].GetStatus() != cbpb.Build_FAILURE || got.GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("GetBuild = %v, %v", got, err)
	}
	logText := buildLogText(ctx, t, h, bucket, "logs/", b.GetId())
	want := []string{`Step #0 - "secret": ` + hex.EncodeToString(sum[:]), `Step #0 - "secret": raw:********`,
		`Step #1 - "sh": script runs in /workspace`, `Step #2 - "shebang": shebang /builder/script`, "DONE"}
	if avail.Inline != nil {
		isum := sha256.Sum256([]byte("inline-" + value))
		want = append(want, `Step #0 - "secret": `+hex.EncodeToString(isum[:]))
	}
	for _, w := range want {
		if !strings.Contains(logText, w) {
			t.Errorf("the log lacks %q:\n%s", w, logText)
		}
	}
	if strings.Contains(logText, value) {
		t.Errorf("the log holds the secret's value:\n%s", logText)
	}

	if _, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: img, Script: "echo hi", Args: []string{"x"}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("script with args = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: img, Args: []string{"true"}, SecretEnv: []string{"UNDEFINED"}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an undefined secretEnv = %v; want INVALID_ARGUMENT", err)
	}
}

// runShardEndpointIfSet is runShardEndpoint's address, or "" when neither
// variable is set.
func runShardEndpointIfSet(h *Harness, own, shared string) string {
	if strings.TrimSpace(os.Getenv(own)) == "" && strings.TrimSpace(os.Getenv(shared)) == "" {
		return ""
	}
	return runShardEndpoint(h, own, shared)
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestGcloudBuildsSecretsAndScript (#1407): `gcloud builds submit --config`
// of a cloudbuild.yaml with availableSecrets.secretManager, a step whose
// secretEnv names it, a script step and an allowFailure step, through the
// configuration `cloudburrow gcloud-setup` writes, egress blocked. The
// build is SUCCESS, the allowFailure step FAILURE, and `gcloud builds log`
// shows the secret's hash and the script's output, the value masked.
func TestGcloudBuildsSecretsAndScript(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(h.Context(), 6*time.Minute)
	t.Cleanup(cancel)
	g := newGcloudSession(t, h)
	value := "g-s3cr3t-" + uuid.NewString()[:8]
	secret := buildSecret(ctx, t, h, value)
	sum := sha256.Sum256([]byte(value))
	src := t.TempDir()
	config := "steps:\n" +
		"- name: " + stepImage() + "\n  entrypoint: sh\n  secretEnv: ['TOKEN']\n" +
		"  args: ['-c', 'printf %s \"$$TOKEN\" | sha256sum; echo \"raw:$$TOKEN\"']\n" +
		"- name: " + stepImage() + "\n  script: |\n    #!/bin/sh\n    echo \"script sees $_WHO\"\n  automapSubstitutions: true\n" +
		"- name: " + stepImage() + "\n  script: exit 4\n  allowFailure: true\n" +
		"substitutions:\n  _WHO: gcloud\n" +
		"availableSecrets:\n  secretManager:\n  - versionName: projects/$PROJECT_ID/secrets/" + secret + "/versions/latest\n    env: TOKEN\n"
	if err := os.WriteFile(filepath.Join(src, "cloudbuild.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := g.run(nil, "builds", "submit", src, "--config="+filepath.Join(src, "cloudbuild.yaml"), "--async", "--format=value(id)")
	t.Logf("gcloud builds submit --config:\n%s", out)
	if err != nil && strings.Contains(out, hostStepsFlag) {
		t.Skip("the instance runs arbitrary build steps only with " + hostStepsFlag + " (#1382, ADR-0004); start it with the flag")
	}
	if err != nil {
		t.Fatalf("gcloud builds submit: %v", err)
	}
	id := lastLine(out)
	var desc string
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		desc = g.must("builds", "describe", id, "--format=value(status,steps[0].status,steps[1].status,steps[2].status,steps[2].exitCode)")
		if f := strings.Fields(desc); len(f) > 0 && f[0] != "QUEUED" && f[0] != "WORKING" {
			break
		}
	}
	if f := strings.Fields(desc); len(f) != 5 || f[0] != "SUCCESS" || f[1] != "SUCCESS" || f[2] != "SUCCESS" || f[3] != "FAILURE" || f[4] != "4" {
		t.Fatalf("the build = %q; want SUCCESS with step 2 FAILURE, exit code 4", desc)
	}
	bucket := strings.TrimSpace(g.must("builds", "describe", id, "--format=value(logsBucket)"))
	logText := buildLogText(ctx, t, h, strings.TrimPrefix(bucket, "gs://"), "", id)
	for _, w := range []string{"Step #0: " + hex.EncodeToString(sum[:]), "Step #0: raw:********", "Step #1: script sees gcloud"} {
		if !strings.Contains(logText, w) {
			t.Errorf("the log lacks %q:\n%s", w, logText)
		}
	}
	if strings.Contains(logText, value) {
		t.Errorf("the log holds the secret's value:\n%s", logText)
	}
}
