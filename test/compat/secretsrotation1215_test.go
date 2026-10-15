//go:build compat

package compat

import (
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSecretsManagedRotationIsUnimplemented (#1215). Managed rotation
// changes a Cloud SQL user's password, which CloudBurrow cannot do, so both
// methods answer UNIMPLEMENTED through the official client, saying why.
// covers: google.cloud.secretmanager.v1.SecretManagerService/RotateSecret (unimplemented)
// covers: google.cloud.secretmanager.v1.SecretManagerService/EnableManagedRotation (unimplemented)
func TestSecretsManagedRotationIsUnimplemented(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	ctx := h.Context()
	name := secretsParent(h) + "/secrets/rotation-1215"
	_, err := c.RotateSecret(ctx, &secretmanagerpb.RotateSecretRequest{Parent: name})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "Cloud SQL") {
		t.Errorf("RotateSecret = %v, want Unimplemented naming Cloud SQL", err)
	}
	_, err = c.EnableManagedRotation(ctx, &secretmanagerpb.EnableManagedRotationRequest{Parent: name})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "Cloud SQL") {
		t.Errorf("EnableManagedRotation = %v, want Unimplemented naming Cloud SQL", err)
	}
}
