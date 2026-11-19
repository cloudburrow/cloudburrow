package kms

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints (#1895) on Cloud KMS's AutokeyConfig, CryptoKey,
// CryptoKeyVersion, EkmConfig, EkmConnection, ImportJob and KeyHandle,
// CREATE and UPDATE, over gRPC and REST (which calls the same methods):
// the condition reads the resource as the request would leave it, by its
// REST field names and enum names (resource.protectionLevel == 'HSM',
// resource.rotationPeriod <= duration('7776000s'), the page's examples),
// limited to the fields the page lists ("Create custom organization
// policy constraints for Cloud KMS", read 2026-11-19). A violation is
// FAILED_PRECONDITION "Operation denied by custom org policies: [...]",
// the wording every custom constraint page shows.

// constraintParent is the project or folder a KMS resource name is under.
func constraintParent(name string) string {
	if p := orgpolicy.ProjectOf(name); p != "" {
		return p
	}
	if rest, ok := strings.CutPrefix(name, "folders/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		if id != "" {
			return "folders/" + id
		}
	}
	return ""
}

// customResource is m's REST JSON with its unset scalars and enums at
// their defaults, as a condition reads a proto3 resource
// (resource.importOnly is false on a key that did not set it); unset
// messages stay absent.
func customResource(m proto.Message) map[string]any {
	raw, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	dropNulls(out)
	return out
}

func dropNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
				continue
			}
			dropNulls(x)
		}
	case []any:
		for _, x := range t {
			dropNulls(x)
		}
	}
}

// checkCustom refuses method leaving m, a resource of resourceType named
// name, when a custom constraint enforced on its project or folder is
// violated.
func checkCustom(name, resourceType, method string, m proto.Message) error {
	under := constraintParent(name)
	if under == "" || !orgpolicy.SourceSet() {
		return nil
	}
	err := orgpolicy.CheckCustom(context.Background(), under, resourceType, method, customResource(m))
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return apierror.Wrap(apierror.FailedPrecondition("%s", v.Error()))
	}
	return nil
}
