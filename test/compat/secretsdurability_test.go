//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Secret Manager across stop and up (#483), the pattern of #418:
// TestSecretManagerRestartSetup runs alone just before `stop`, and
// TestSecretManagerAcrossRestart after each `up`.
const (
	envSecretsProbe  = "CLOUDBURROW_TEST_SECRETS_PROBE"
	envSecretsExpect = "CLOUDBURROW_TEST_SECRETS_EXPECT"
	secretsProbeData = "written-before-stop"
)

// TestSecretManagerRestartSetup leaves a secret with one version for
// TestSecretManagerAcrossRestart, and writes its name to envSecretsProbe.
func TestSecretManagerRestartSetup(t *testing.T) {
	path := os.Getenv(envSecretsProbe)
	if path == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envSecretsProbe)
	}
	h := New(t)
	c := secretsClient(t, h)
	s, err := c.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + h.Project(), SecretId: "restart-probe",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddSecretVersion(h.Context(), &secretmanagerpb.AddSecretVersionRequest{Parent: s.GetName(),
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(secretsProbeData)}}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"secret": s.GetName()})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSecretManagerAcrossRestart: after stop and up in persistent mode the
// secret's latest version reads back; in ephemeral mode the secret is gone.
//
// unverified: google.cloud.secretmanager.v1.SecretManagerService/GetSecret NOT_FOUND: a secret that did not survive an ephemeral restart
func TestSecretManagerAcrossRestart(t *testing.T) {
	expect := os.Getenv(envSecretsExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envSecretsExpect)
	}
	b, err := os.ReadFile(os.Getenv(envSecretsProbe))
	if err != nil {
		t.Fatalf("the probe TestSecretManagerRestartSetup left: %v", err)
	}
	var p struct{ Secret string }
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	h := New(t)
	c := secretsClient(t, h)
	switch expect {
	case "present":
		v, err := c.AccessSecretVersion(h.Context(), &secretmanagerpb.AccessSecretVersionRequest{Name: p.Secret + "/versions/latest"})
		if err != nil || string(v.GetPayload().GetData()) != secretsProbeData {
			t.Fatalf("persistent mode after stop/up: %q, %v; want the version written before stop", v.GetPayload().GetData(), err)
		}
	case "absent":
		if _, err := c.GetSecret(h.Context(), &secretmanagerpb.GetSecretRequest{Name: p.Secret}); status.Code(err) != codes.NotFound {
			t.Fatalf("ephemeral mode after stop/up: GetSecret = %v; want NotFound (#483)", err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envSecretsExpect, expect)
	}
}
