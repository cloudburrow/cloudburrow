package workflows

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Customer-managed encryption keys, as
// https://docs.cloud.google.com/workflows/docs/use-cmek documents them:
//
//   - crypto_key_name is projects/P/locations/L/keyRings/R/cryptoKeys/K;
//     "The Cloud KMS key ring and the protected workflow must be in the
//     same region" (else "Key ring region does not match the resource to
//     be protected"), and may be in different projects.
//   - "This source file is encrypted using the key": the source is
//     encrypted with Cloud KMS Encrypt under the key at deployment, and the
//     revision records crypto_key_version, all_kms_keys and
//     all_kms_keys_versions. A key that does not exist fails the
//     deployment with "Permission 'cloudkms.cryptoKeyVersions.useToEncrypt'
//     denied", and a disabled one with "Key version is not enabled".
//     Changing the key makes a new revision, encrypted with the new key.
//   - "You can't create new executions ... [with] the disabled or
//     destroyed key": an execution's start decrypts its revision's source
//     with Cloud KMS Decrypt, and is refused (FAILED_PRECONDITION) when
//     that fails.
//   - "Any active executions will fail with a corresponding error
//     message": a running execution decrypts it again every
//     KeyCheckInterval and FAILS, with the error saying why, as soon as it
//     cannot.
//
// The key is used through the instance's own Cloud KMS; an instance
// without it refuses a workflow with a key (FAILED_PRECONDITION).

// KMS is the part of Cloud KMS a workflow's key is used through.
type KMS interface {
	Encrypt(context.Context, *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(context.Context, *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
}

// KeyCheckInterval is how often a running execution with a key checks it.
var KeyCheckInterval = time.Second

var keyNameRE = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/keyRings/[^/]+/cryptoKeys/[^/]+$`)

func checkKeyName(w *workflowspb.Workflow) error {
	key := w.GetCryptoKeyName()
	if key == "" {
		return nil
	}
	m := keyNameRE.FindStringSubmatch(key)
	if m == nil {
		return apierror.InvalidArgument("crypto_key_name %q must be projects/{project}/locations/{location}/keyRings/{keyRing}/cryptoKeys/{cryptoKey}", key)
	}
	if wm := workflowRE.FindStringSubmatch(w.GetName()); wm != nil {
		if loc := strings.Split(wm[1], "/")[3]; loc != m[1] {
			return apierror.InvalidArgument("Key ring region does not match the resource to be protected: the key %s is in %s and the workflow in %s", key, m[1], loc)
		}
	}
	return nil
}

func (s *Server) kms() KMS {
	if s.opts.KMS == nil {
		return nil
	}
	return s.opts.KMS()
}

// keyError is a Cloud KMS failure as Workflows reports it.
func keyError(key string, err error) error {
	st, _ := status.FromError(err)
	var ae *apierror.Error
	if errors.As(err, &ae) {
		st = status.New(ae.Code, ae.Message)
	}
	switch st.Code() {
	case codes.NotFound, codes.PermissionDenied:
		return &apierror.Error{Code: codes.PermissionDenied, Reason: "IAM_PERMISSION_DENIED",
			Message: fmt.Sprintf("Permission 'cloudkms.cryptoKeyVersions.useToEncrypt' denied on resource '%s' (or it may not exist): %s", key, st.Message())}
	case codes.FailedPrecondition:
		return apierror.FailedPrecondition("Key version is not enabled: the Cloud KMS key %s cannot be used: %s", key, st.Message())
	}
	return apierror.FailedPrecondition("the Cloud KMS key %s cannot be used: %v", key, err)
}

// seal encrypts w's source under its key and records the key version. The
// caller has set w.Name.
func (s *Server) seal(w *workflowspb.Workflow) ([]byte, error) {
	key := w.GetCryptoKeyName()
	w.CryptoKeyVersion, w.AllKmsKeys, w.AllKmsKeysVersions = "", nil, nil
	if key == "" {
		return nil, nil
	}
	if err := checkKeyName(w); err != nil {
		return nil, err
	}
	k := s.kms()
	if k == nil {
		return nil, apierror.FailedPrecondition("a workflow with crypto_key_name needs Cloud KMS, which this CloudBurrow instance does not serve: start it with --services including kms")
	}
	resp, err := k.Encrypt(context.Background(), &kmspb.EncryptRequest{Name: key, Plaintext: []byte(w.GetSourceContents()),
		AdditionalAuthenticatedData: []byte(w.GetName())})
	if err != nil {
		return nil, keyError(key, err)
	}
	w.CryptoKeyVersion = resp.GetName()
	w.AllKmsKeys = []string{key}
	w.AllKmsKeysVersions = []string{resp.GetName()}
	return resp.GetCiphertext(), nil
}

// unseal checks that an execution's key can still decrypt its source.
func (s *Server) unseal(workflow, key string, cipher []byte) error {
	if key == "" {
		return nil
	}
	k := s.kms()
	if k == nil {
		return apierror.FailedPrecondition("the workflow's key %s needs Cloud KMS, which this CloudBurrow instance does not serve", key)
	}
	if _, err := k.Decrypt(context.Background(), &kmspb.DecryptRequest{Name: key, Ciphertext: cipher,
		AdditionalAuthenticatedData: []byte(workflow)}); err != nil {
		return keyError(key, err)
	}
	return nil
}

// errKey is an execution stopped because its key became unusable.
type errKey struct{ err error }

func (e *errKey) Error() string { return e.err.Error() }

// watchKey fails a running execution as soon as its key cannot be used.
func (s *Server) watchKey(er *executionRecord, workflow string) {
	if er.key == "" {
		return
	}
	t := time.NewTicker(s.keyCheck)
	defer t.Stop()
	for {
		select {
		case <-er.done:
			return
		case <-t.C:
			if err := s.unseal(workflow, er.key, er.cipher); err != nil {
				s.mu.Lock()
				cancel := er.cancel
				s.mu.Unlock()
				if cancel != nil {
					cancel(&errKey{err: err})
				}
				return
			}
		}
	}
}
