//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCloudRunRevisionReadsASecretManagerSecret is #84's acceptance criterion
// exercised end to end: a secret is created through the Secret Manager API,
// a Cloud Run service is deployed with a `secretKeyRef` environment variable,
// and the container reports the value it was actually given.
//
// Nothing templates the payload into the manifest. The revision reads it from
// the Kubernetes Secret the store wrote, which is the whole point of backing
// secrets with the cluster.
func TestCloudRunRevisionReadsASecretManagerSecret(t *testing.T) {
	h := New(t)
	sc := secretsClient(t, h)
	rc := runClient(t, h)
	ctx := h.Context()

	const secretID = "compat-env-secret"
	const payload = "value-from-secret-manager"
	secretName := secretsParent(h) + "/secrets/" + secretID

	if _, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   secretsParent(h),
		SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}},
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: secretName})
	})

	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  secretName,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(payload)},
	}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}

	// helloworld-go echoes TARGET, which makes the value observable over HTTP
	// without a fixture of our own.
	id := "compat-secret-env"
	svcName := runParent(h) + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{
				Image: "ghcr.io/knative/helloworld-go:latest",
				Env: []*runpb.EnvVar{{
					Name: "TARGET",
					Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
						SecretKeyRef: &runpb.SecretKeySelector{
							Secret: secretID, Version: "latest",
						},
					}},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateService with a secretKeyRef: %v", err)
	}
	t.Cleanup(func() {
		_, _ = rc.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: svcName})
	})

	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}

	base, host := ingress(t), hostOf(t, svc.GetUri())
	body, code := secretEnvGet(t, base, host)
	if code != http.StatusOK {
		t.Fatalf("GET / = %d: %s", code, body)
	}
	if !strings.Contains(body, payload) {
		t.Fatalf("the container did not receive the secret: %q", strings.TrimSpace(body))
	}
	t.Logf("revision read the secret from the cluster: %q", strings.TrimSpace(body))

	// The payload must not be in the deployed manifest: if it were, the
	// secret would be readable by anyone who can read a Knative Service,
	// which is a different and much wider audience than the Secret.
	manifest, err := kubectlGet(t, "ksvc", id)
	if err != nil {
		t.Fatalf("read the deployed service: %v", err)
	}
	if strings.Contains(manifest, payload) {
		t.Error("the payload was templated into the Knative Service instead of referenced")
	}
	if !strings.Contains(manifest, "secretKeyRef") {
		t.Errorf("the revision does not reference a secret:\n%s", manifest)
	}
}

// TestSecretKeyRefToAnInaccessibleVersionIsRefused proves the deployment
// fails with a useful message rather than producing a pod that cannot start.
func TestSecretKeyRefToAnInaccessibleVersionIsRefused(t *testing.T) {
	h := New(t)
	sc := secretsClient(t, h)
	rc := runClient(t, h)
	ctx := h.Context()

	const secretID = "compat-disabled-secret"
	secretName := secretsParent(h) + "/secrets/" + secretID

	if _, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   secretsParent(h),
		SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}},
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: secretName})
	})

	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  secretName,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")},
	}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	if _, err := sc.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{
		Name: secretName + "/versions/1",
	}); err != nil {
		t.Fatalf("DisableSecretVersion: %v", err)
	}

	id := "compat-secret-disabled"
	_, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent:    runParent(h),
		ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{
				Image: "ghcr.io/knative/helloworld-go:latest",
				Env: []*runpb.EnvVar{{
					Name: "TARGET",
					Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
						SecretKeyRef: &runpb.SecretKeySelector{Secret: secretID, Version: "1"},
					}},
				}},
			}},
		}},
	})
	t.Cleanup(func() {
		_, _ = rc.DeleteService(h.Context(),
			&runpb.DeleteServiceRequest{Name: runParent(h) + "/services/" + id})
	})

	if err == nil {
		t.Fatal("a reference to a disabled version was accepted")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "DISABLED") {
		t.Errorf("the error should name the state: %v", err)
	}
}

func secretEnvGet(t *testing.T, base, host string) (string, int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, body, err := doRequest(http.MethodGet, base+"/", host, "", 15*time.Second)
		if err == nil && code == http.StatusOK {
			return body, code
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err.Error(), 0
			}
			return body, code
		}
		time.Sleep(2 * time.Second)
	}
}

// kubectlGet reads a deployed object, so the test can assert on what was
// actually applied rather than on what the adapter says it applied.
func kubectlGet(t *testing.T, kind, name string) (string, error) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	raw, err := exec.Command("kubectl", "--kubeconfig", kubeconfig,
		"-n", "default", "get", kind, name, "-o", "yaml").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl get %s %s: %w\n%s", kind, name, err, raw)
	}
	return string(raw), nil
}
