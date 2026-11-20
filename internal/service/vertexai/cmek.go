package vertexai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// Customer-managed encryption keys of Document AI and Speech-to-Text
// (#1950), through the shared helper internal/cmek: the key is a CryptoKey
// of the instance's Cloud KMS, checked usable (and, with IAM enforcement
// on, its use by the service agent) when it is set; what the service keeps
// is sealed with a data key the key wrapped; and a read of a resource
// encrypted with a version that is disabled or destroyed is refused.
//
// UNVERIFIED (docs re-read 2026-11-19, they name no error text:
// https://docs.cloud.google.com/document-ai/docs/cmek and
// https://docs.cloud.google.com/speech-to-text/v2/docs/encryption): the
// codes and texts of the refusals below. A key that cannot be used is
// FAILED_PRECONDITION (a missing key or one in the wrong location
// INVALID_ARGUMENT), a service agent without the role PERMISSION_DENIED.

// Service agents (Document AI's from its CMEK page; Speech-to-Text's is
// Google's service agent list's).
const (
	docAIAgentDomain  = "gcp-sa-prod-dai-core.iam.gserviceaccount.com"
	speechAgentDomain = "gcp-sa-speech.iam.gserviceaccount.com"
)

// UseCMEK gives Document AI and Speech-to-Text the instance's Cloud KMS:
// without it, a customer-managed key is refused, as Cloud KMS is not
// served.
func (s *Server) UseCMEK(k cmek.Keys) { s.keys.Store(&k) }

// keysOf is the configured keys.
func (s *Server) keysOf() cmek.Keys {
	if k := s.keys.Load(); k != nil {
		return *k
	}
	return cmek.Keys{}
}

// cmekError is a key refusal as the API answers it; what is the resource.
func cmekError(what string, err error) error {
	e := cmek.As(err)
	if e == nil {
		return err
	}
	switch e.Kind {
	case cmek.Invalid, cmek.NotFound:
		return apierror.InvalidArgument("The Cloud KMS key %s cannot be used for %s: %s", e.Key, what, e.Msg)
	case cmek.PermissionDenied:
		return &apierror.Error{Code: codes.PermissionDenied, Reason: "forbidden",
			Message: "Permission denied on Cloud KMS key " + e.Key + " for " + what + ": " + e.Msg}
	case cmek.Unavailable:
		return &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: "Cloud KMS did not answer for " + e.Key + ": " + e.Msg}
	case cmek.NoKMS:
		return apierror.FailedPrecondition("%s is encrypted with a customer-managed key, and %s", what, e.Msg)
	}
	return apierror.FailedPrecondition("The Cloud KMS key %s that encrypts %s cannot be used: %s", e.Key, what, e.Msg)
}

// readSealed reads a name → Sealed file, empty when there is none.
func readSealed(file string) map[string]cmek.Sealed {
	out := map[string]cmek.Sealed{}
	if file == "" {
		return out
	}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// writeSealed writes a name → Sealed file.
func writeSealed(file string, m map[string]cmek.Sealed) {
	if file == "" {
		return
	}
	b, _ := json.Marshal(m)
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	if os.WriteFile(file+".tmp", b, 0o600) == nil {
		_ = os.Rename(file+".tmp", file)
	}
}

// openSealed opens a resource's sealed part for agent of project.
func (s *Server) openSealed(ctx context.Context, project, agent, what, name string, sealed cmek.Sealed) ([]byte, error) {
	pt, err := s.keysOf().Open(ctx, project, agent, sealed, []byte(name))
	if err != nil {
		return nil, cmekError(what, err)
	}
	return pt, nil
}
