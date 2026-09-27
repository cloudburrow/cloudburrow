//go:build compat

package compat

import (
	"fmt"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret
//
// TestTerraformSecretLabelChangeInPlace (#691): a label change on a
// google_secret_manager_secret is applied in place, which the provider does
// with UpdateSecret (a PATCH naming labels in its updateMask), not by
// replacing the secret. The official client reads the new labels back with
// the create time unchanged, and the plan is clean afterwards.
func TestTerraformSecretLabelChangeInPlace(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	sm := secretsClient(t, h)
	ctx := h.Context()
	project := h.Project()
	name := "projects/" + project + "/secrets/tf-labels"
	module := func(labels string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = "tf-labels"
  labels    = %s
  replication {
    auto {}
  }
}
`, project, labels)
	}
	read := func(when string, want map[string]string) *secretmanagerpb.Secret {
		t.Helper()
		s, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetSecret: %v", when, err)
		}
		// The provider adds its own attribution label; the module's must
		// be exactly as written.
		for k, v := range want {
			if s.GetLabels()[k] != v {
				t.Errorf("%s, labels = %v; want %s=%s", when, s.GetLabels(), k, v)
			}
		}
		if _, ok := s.GetLabels()["env"]; ok && want["env"] == "" {
			t.Errorf("%s, labels = %v; env should be gone", when, s.GetLabels())
		}
		return s
	}

	m.write(module(`{ env = "dev" }`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	first := read("after apply", map[string]string{"env": "dev"})
	m.planClean()

	m.write(module(`{ env = "prod", team = "a" }`))
	m.apply()
	changed := read("after the label change", map[string]string{"env": "prod", "team": "a"})
	if !changed.GetCreateTime().AsTime().Equal(first.GetCreateTime().AsTime()) {
		t.Errorf("the label change replaced the secret: create time %v, was %v", changed.GetCreateTime(), first.GetCreateTime())
	}
	m.planClean()

	m.write(module(`{ team = "a" }`))
	m.apply()
	read("after removing a label", map[string]string{"team": "a"})
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}
