package secrets

import (
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Secret.secret_type (#1850): "Optional. Immutable." in resources.proto, the
// Secret type a console user picks on Create secret ("You can use secret
// types to categorize your secrets"; "You can only set the type when
// creating the secret",
// https://docs.cloud.google.com/secret-manager/docs/creating-and-accessing-secrets,
// read 2026-11-20). It is kept as the enum's name and returned as set; a
// secret created without one reads back SECRET_TYPE_UNSPECIFIED (absent).

// CheckSecretType reports whether t names a secret type: an enum value
// resources.proto defines, by name. Empty is none.
func CheckSecretType(t string) error {
	if t == "" {
		return nil
	}
	if _, ok := secretmanagerpb.Secret_SecretType_value[t]; !ok {
		return apierror.InvalidArgument("secret_type %q is not a secret type; it must be one of "+
			"CLOUD_SQL_DB_CREDENTIALS, ACCESS_KEY, CERTIFICATE, OTHER_DB_CREDENTIALS or OTHER", t)
	}
	return nil
}

// secretTypeOf is a request's secret_type as the store keeps it: the enum's
// name, empty for SECRET_TYPE_UNSPECIFIED. A number the enum does not
// define is INVALID_ARGUMENT.
func secretTypeOf(sec *secretmanagerpb.Secret) (string, error) {
	t := sec.GetSecretType()
	if t == secretmanagerpb.Secret_SECRET_TYPE_UNSPECIFIED {
		return "", nil
	}
	name, ok := secretmanagerpb.Secret_SecretType_name[int32(t)]
	if !ok {
		return "", apierror.InvalidArgument("secret_type %d is not a secret type", int32(t))
	}
	return name, nil
}

// SetSecretType records a new secret's type. It is part of creating the
// secret, so it neither changes the etag nor notifies the secret's topics;
// nothing else calls it, since the field is immutable.
func (s *Store) SetSecretType(project, id, t string) (Secret, error) {
	if err := CheckSecretType(t); err != nil {
		return Secret{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.getSecret(project, id)
	if err != nil {
		return Secret{}, err
	}
	if sec.SecretType == t {
		return sec, nil
	}
	sec.SecretType = t
	if err := s.put(secretKey(project, id), sec); err != nil {
		return Secret{}, err
	}
	return sec, nil
}
