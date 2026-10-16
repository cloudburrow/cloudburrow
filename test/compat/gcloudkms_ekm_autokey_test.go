//go:build compat

package compat

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestGcloudKMSHSMEkmAndAutokey (#1452): gcloud kms, configured by
// `cloudburrow gcloud-setup` alone, creates an HSM key and sets its
// key access justifications policy (keys create --protection-level hsm,
// keys update --allowed-access-reasons); creates, lists, describes and
// updates an EKM connection and makes it its location's default
// (ekm-connections, ekm-config); and, with the project's AutokeyConfig set
// from a file (autokey-config update, describe, show-effective-config),
// creates a key handle, waiting on its operation, then describes and lists it
// (key-handles). The official gRPC client checks what each command did.
func TestGcloudKMSHSMEkmAndAutokey(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}

	// HSM, and key access justifications.
	ringID := "gc-hsm-" + h.Project()
	gc("kms", "keyrings", "create", ringID, "--location", "us-central1")
	gc("kms", "keys", "create", "k", "--keyring", ringID, "--location", "us-central1", "--purpose", "encryption", "--protection-level", "hsm")
	key := "projects/" + project + "/locations/us-central1/keyRings/" + ringID + "/cryptoKeys/k"
	if out := gc("kms", "keys", "describe", "k", "--keyring", ringID, "--location", "us-central1", "--format", "value(versionTemplate.protectionLevel)"); strings.TrimSpace(out) != "HSM" {
		t.Errorf("keys describe protectionLevel = %q, want HSM", out)
	}
	gc("kms", "keys", "update", "k", "--keyring", ringID, "--location", "us-central1", "--allowed-access-reasons", "customer-initiated-access,google-initiated-system-operation")
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil || k.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM || len(k.GetKeyAccessJustificationsPolicy().GetAllowedAccessReasons()) != 2 {
		t.Fatalf("GetCryptoKey = %v, %v", k, err)
	}

	// EKM connections.
	dir := t.TempDir()
	certFile := filepath.Join(dir, "ekm.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ekmCertificate(t)}), 0o600); err != nil {
		t.Fatal(err)
	}
	// The gcloud project is the instance's, shared by every run: the IDs are
	// this run's.
	ekmID, handleID := "gc-ekm-"+h.Project(), "gc-disk-"+h.Project()
	gc("kms", "ekm-connections", "create", ekmID, "--location", "europe-west1",
		"--service-directory-service", "projects/"+project+"/locations/europe-west1/namespaces/ekm/services/replica",
		"--hostname", "ekm.example.com", "--server-certificates-files", certFile)
	conn := "projects/" + project + "/locations/europe-west1/ekmConnections/" + ekmID
	if out := gc("kms", "ekm-connections", "list", "--location", "europe-west1", "--format", "value(name)"); !strings.Contains(out, conn) {
		t.Errorf("ekm-connections list = %q, want %s", out, conn)
	}
	if out := gc("kms", "ekm-connections", "describe", ekmID, "--location", "europe-west1", "--format", "value(serviceResolvers[0].hostname)"); strings.TrimSpace(out) != "ekm.example.com" {
		t.Errorf("ekm-connections describe hostname = %q", out)
	}
	gc("kms", "ekm-connections", "update", ekmID, "--location", "europe-west1", "--key-management-mode", "cloud-kms", "--crypto-space-path", "v0/longlived/gcloud")
	gc("kms", "ekm-config", "update", "--location", "europe-west1", "--default-ekm-connection", conn)
	if out := gc("kms", "ekm-config", "describe", "--location", "europe-west1", "--format", "value(defaultEkmConnection)"); strings.TrimSpace(out) != conn {
		t.Errorf("ekm-config describe = %q, want %s", out, conn)
	}

	// Autokey, on the project's own AutokeyConfig.
	cfgName := "projects/" + project + "/autokeyConfig"
	cfgFile := filepath.Join(dir, "autokey.yaml")
	if err := os.WriteFile(cfgFile, []byte("name: "+cfgName+"\nkeyProjectResolutionMode: RESOURCE_PROJECT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gc("kms", "autokey-config", "update", cfgFile)
	cleanupAdmin := kmsAdminClient(t, h)
	t.Cleanup(func() {
		// Leave the project with no Autokey, as other tests expect.
		_, _ = cleanupAdmin.UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{AutokeyConfig: &kmspb.AutokeyConfig{Name: cfgName},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"key_project", "key_project_resolution_mode"}}})
	})
	if out := gc("kms", "autokey-config", "describe", "--project", project, "--format", "value(keyProjectResolutionMode,state)"); !strings.Contains(out, "RESOURCE_PROJECT") || !strings.Contains(out, "ACTIVE") {
		t.Errorf("autokey-config describe = %q", out)
	}
	if out := gc("kms", "autokey-config", "show-effective-config", "--project", project, "--format", "value(keyProject)"); strings.TrimSpace(out) != "projects/"+project {
		t.Errorf("autokey-config show-effective-config = %q, want projects/%s", out, project)
	}
	gc("kms", "key-handles", "create", "--location", "us-central1", "--resource-type", "compute.googleapis.com/Disk", "--key-handle-id", handleID)
	kmsKey := strings.TrimSpace(gc("kms", "key-handles", "describe", handleID, "--location", "us-central1", "--format", "value(kmsKey)"))
	if !strings.HasPrefix(kmsKey, "projects/"+project+"/locations/us-central1/keyRings/autokey/cryptoKeys/") {
		t.Fatalf("key-handles describe kmsKey = %q", kmsKey)
	}
	if out := gc("kms", "key-handles", "list", "--location", "us-central1", "--resource-type", "compute.googleapis.com/Disk", "--format", "value(name)"); !strings.Contains(out, handleID) {
		t.Errorf("key-handles list = %q", out)
	}
	ak, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: kmsKey})
	if err != nil || ak.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Errorf("the Autokey key = %v, %v", ak, err)
	}
}

// TestGcloudKMSEkmIamPolicy (#1469): gcloud kms ekm-connections and
// ekm-config add-iam-policy-binding, get-iam-policy and
// remove-iam-policy-binding, configured by `cloudburrow gcloud-setup`
// alone, read and write the policies stored on an EKM connection and on its
// location's EkmConfig; the official client reads what each wrote. The
// policies are stored, never enforced.
func TestGcloudKMSEkmIamPolicy(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	ctx := h.Context()
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	certFile := filepath.Join(t.TempDir(), "ekm.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ekmCertificate(t)}), 0o600); err != nil {
		t.Fatal(err)
	}
	// The gcloud project is shared by every run: the location is this
	// test's alone, and the connection ID this run's.
	const location = "europe-west2"
	ekmID := "gc-ekm-iam-" + h.Project()
	gc("kms", "ekm-connections", "create", ekmID, "--location", location,
		"--service-directory-service", "projects/"+project+"/locations/"+location+"/namespaces/ekm/services/replica",
		"--hostname", "ekm.example.com", "--server-certificates-files", certFile)
	conn := "projects/" + project + "/locations/" + location + "/ekmConnections/" + ekmID
	member := "user:" + h.Project() + "@example.com"
	ekm, err := kms.NewEkmClient(ctx, kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	defer ekm.Close()
	members := func(res string) []string {
		t.Helper()
		p, err := ekm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil {
			t.Fatalf("GetIamPolicy(%s) = %v", res, err)
		}
		var out []string
		for _, b := range p.GetBindings() {
			if b.GetRole() == "roles/cloudkms.viewer" {
				out = append(out, b.GetMembers()...)
			}
		}
		return out
	}
	for _, c := range []struct {
		res  string
		args []string // the command group and its resource arguments
	}{
		{conn, []string{"ekm-connections", "%s", ekmID, "--location", location}},
		{"projects/" + project + "/locations/" + location + "/ekmConfig", []string{"ekm-config", "%s", "--location", location}},
	} {
		cmd := func(verb string, extra ...string) []string {
			args := []string{"kms"}
			for _, a := range c.args {
				if a == "%s" {
					a = verb
				}
				args = append(args, a)
			}
			return append(args, extra...)
		}
		gc(cmd("add-iam-policy-binding", "--member", member, "--role", "roles/cloudkms.viewer")...)
		if got := members(c.res); !slices.Contains(got, member) {
			t.Errorf("%s after add-iam-policy-binding: roles/cloudkms.viewer = %v, want %s", c.res, got, member)
		}
		if out := gc(cmd("get-iam-policy", "--format", "value(bindings.members)")...); !strings.Contains(out, member) {
			t.Errorf("%s get-iam-policy = %q, want %s", c.args[0], out, member)
		}
		gc(cmd("remove-iam-policy-binding", "--member", member, "--role", "roles/cloudkms.viewer")...)
		if got := members(c.res); slices.Contains(got, member) {
			t.Errorf("%s after remove-iam-policy-binding: roles/cloudkms.viewer = %v, want %s gone", c.res, got, member)
		}
	}
}
