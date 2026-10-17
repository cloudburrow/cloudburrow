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
// handles itself. Google's reference documents only the request (the
// credentials of one Cloud SQL user), not what a rotation does to that
// user's password or to the secret's versions, and CloudBurrow's Cloud SQL
// Admin API (#1301) takes no password change from another service; adding
// a version alone would leave the secret and the database disagreeing. Both
// stay UNIMPLEMENTED, saying why (#1530).

const managedRotationReason = "managed rotation is not implemented: it changes a Cloud SQL user's password, " +
	"and Google documents only the request, not what a rotation does to the user or the secret's versions; " +
	"change the password with the Cloud SQL Admin API's users.update and add a version with AddSecretVersion instead"

func (g *GRPCServer) RotateSecret(context.Context, *secretmanagerpb.RotateSecretRequest) (*secretmanagerpb.SecretVersion, error) {
	return nil, apierror.Unimplemented("RotateSecret: %s", managedRotationReason)
}

func (g *GRPCServer) EnableManagedRotation(context.Context, *secretmanagerpb.EnableManagedRotationRequest) (*secretmanagerpb.SecretVersion, error) {
	return nil, apierror.Unimplemented("EnableManagedRotation: %s", managedRotationReason)
}
