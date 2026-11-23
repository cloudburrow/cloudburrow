package run

import (
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// A webhook's serviceAccountAuth over the JSON API (#1578). Secure Source
// Manager's discovery document publishes Hook.serviceAccountAuth ("Determines
// if the hook uses the Repository Service Account to generate an OIDC ID
// Token for webhook authentication"), and hashicorp/google's
// google_secure_source_manager_hook sends it on every update; the published
// securesourcemanagerpb has no such field, so the JSON API decoded no update
// of a hook Terraform made. The field is served through a rest.Overlay, as
// Cloud Run's worker pool scaling is (#1788), and is the same setting the
// console's "Enable service account authentication" sets (ssmExtra.HookAuth).

const (
	fieldHookServiceAccountAuth protoreflect.Name = "service_account_auth"
	// numHookServiceAccountAuth is the overlay's own number; the JSON name
	// is the contract (rest.Overlay).
	numHookServiceAccountAuth = 1000001
)

// ssmOverlay is google.cloud.securesourcemanager.v1 with
// Hook.service_account_auth.
var ssmOverlay = rest.MustOverlay("google.cloud.securesourcemanager.v1", addHookServiceAccountAuth)

// addHookServiceAccountAuth adds the field to Hook, unless the proto has it.
func addHookServiceAccountAuth(f *descriptorpb.FileDescriptorProto) {
	for _, m := range f.GetMessageType() {
		if m.GetName() != "Hook" {
			continue
		}
		for _, fd := range m.GetField() {
			if fd.GetName() == string(fieldHookServiceAccountAuth) {
				return
			}
		}
		m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{Name: proto.String(string(fieldHookServiceAccountAuth)),
			JsonName: proto.String("serviceAccountAuth"), Number: proto.Int32(numHookServiceAccountAuth),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()})
		return
	}
}

// hookAuthIn is the serviceAccountAuth a request's hook carries.
func hookAuthIn(h *ssmpb.Hook) bool {
	v, ok := ssmOverlay.Get(h, fieldHookServiceAccountAuth)
	return ok && v.Bool()
}

// setHookAuthLocked turns a stored hook's service account authentication on
// or off, as SetSourceManagerHookServiceAccountAuth does; ssmState.mu is held.
func setHookAuthLocked(hook string, on bool) error {
	if !on {
		delete(ssmExtra.HookAuth, hook)
		return nil
	}
	if ssmState.repos[ssmRepoOf(hook)].GetServiceAccount() == "" {
		return apierror.FailedPrecondition("service account authentication needs the repository's service account: the repository %s has none", lastSegmentOf(ssmRepoOf(hook)))
	}
	ssmExtra.HookAuth[hook] = true
	return nil
}

// withHookAuthLocked is out with its serviceAccountAuth set from the stored
// setting; ssmState.mu is held.
func withHookAuthLocked(out *ssmpb.Hook) *ssmpb.Hook {
	if ssmExtra.HookAuth[out.GetName()] {
		_ = ssmOverlay.Set(out, fieldHookServiceAccountAuth, protoreflect.ValueOfBool(true))
	}
	return out
}
