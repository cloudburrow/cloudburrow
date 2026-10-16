package main

import (
	"context"
	"errors"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// A Cloud Build build's availableSecrets and secrets (#1407) read this
// instance's Secret Manager and Cloud KMS, in process, as AccessSecretVersion
// and Decrypt do. Both are looked up when a build runs: Cloud Run starts
// before them.

// buildSecretAccessor reads a secret version from the Secret Manager store.
type buildSecretAccessor struct{ svc *secretsService }

func (a buildSecretAccessor) AccessSecretVersion(_ context.Context, name string) ([]byte, error) {
	st := a.svc.Store()
	if st == nil {
		return nil, errors.New("Secret Manager has not started yet")
	}
	project, id, version, err := secrets.ParseVersionName(name)
	if err != nil {
		return nil, err
	}
	v, err := st.AccessVersion(project, id, version)
	if err != nil {
		return nil, err
	}
	return v.Payload, nil
}

// buildKMSDecrypter decrypts with the Cloud KMS server.
type buildKMSDecrypter struct{ svc *kmsService }

func (d buildKMSDecrypter) Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error) {
	api := d.svc.API()
	if api == nil {
		return nil, errors.New("Cloud KMS has not started yet")
	}
	r, err := api.Decrypt(ctx, &kmspb.DecryptRequest{Name: key, Ciphertext: ciphertext})
	if err != nil {
		return nil, err
	}
	return r.GetPlaintext(), nil
}

// useBuildSecrets sets where CreateBuild reads a build's secrets; a nil
// service leaves secrets of its kind refused.
func (r *runService) useBuildSecrets(sm *secretsService, kms *kmsService) {
	if r == nil {
		return
	}
	if sm != nil {
		r.buildSecretManager = buildSecretAccessor{svc: sm}
	}
	if kms != nil {
		r.buildKMS = buildKMSDecrypter{svc: kms}
	}
}

var (
	_ runadapter.SecretAccessor = buildSecretAccessor{}
	_ runadapter.KMSDecrypter   = buildKMSDecrypter{}
)
