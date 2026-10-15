package secrets

import (
	"context"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// RotateSecret and EnableManagedRotation are Secret Manager's managed
// rotation (#1215). As documented, EnableManagedRotation takes Cloud SQL
// single-user credentials, and each rotation adds a secret version and
// changes that user's password in Cloud SQL. It is not the Pub/Sub
// rotation notification of a secret's `rotation` field, which a caller
// handles itself. CloudBurrow serves no Cloud SQL Admin API, so there is no
// password to change, and adding a version alone would leave the secret
// and the database disagreeing. Both stay UNIMPLEMENTED, saying why.

const managedRotationReason = "managed rotation is not implemented: it changes a Cloud SQL user's password, " +
	"and CloudBurrow serves no Cloud SQL Admin API; add a version with AddSecretVersion instead"

func (g *GRPCServer) RotateSecret(context.Context, *secretmanagerpb.RotateSecretRequest) (*secretmanagerpb.SecretVersion, error) {
	return nil, apierror.Unimplemented("RotateSecret: %s", managedRotationReason)
}

func (g *GRPCServer) EnableManagedRotation(context.Context, *secretmanagerpb.EnableManagedRotationRequest) (*secretmanagerpb.SecretVersion, error) {
	return nil, apierror.Unimplemented("EnableManagedRotation: %s", managedRotationReason)
}
