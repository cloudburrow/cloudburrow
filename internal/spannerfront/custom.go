package spannerfront

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints (#1895) on spanner.googleapis.com/Instance,
// InstanceConfig, Database and Backup, CREATE and UPDATE, over gRPC and
// REST (whose routes call the same methods) ("Add a custom organization
// policy", Spanner, read 2026-11-19). The condition reads the resource by
// its REST field names, the documented ones alone
// (internal/orgpolicy/fields_spanner.go), as the request would leave it;
// an unset scalar reads as its default (resource.enableDropProtection is
// false on a new database).
//
// A violation is FAILED_PRECONDITION "Operation denied by custom org
// policies: [...]", but CreateDatabase's: the page's example output is
// PERMISSION_DENIED with the message below, which names no constraint.

// databaseDenied is the page's output for a CreateDatabase a custom
// constraint refuses.
const databaseDenied = "Either caller is missing IAM permission spanner.databases.create on resource or the " +
	"CreateDatabaseRequest.create_statement field is malformed and the database name could not be identified " +
	"to verify Cloud IAM Conditions."

// asResource is m in its REST JSON, unset scalars at their defaults and
// unset messages absent.
func asResource(m proto.Message) map[string]any {
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

// customViolation is the violation of the custom constraints on
// resourceType by m (named by its name field), or nil.
func customViolation(ctx context.Context, name, resourceType, method string, m proto.Message) *orgpolicy.CustomViolation {
	project := orgpolicy.ProjectOf(name)
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	var v *orgpolicy.CustomViolation
	if errors.As(orgpolicy.CheckCustom(ctx, project, resourceType, method, asResource(m)), &v) {
		return v
	}
	return nil
}

// checkCustom refuses a violation FAILED_PRECONDITION.
func checkCustom(ctx context.Context, name, resourceType, method string, m proto.Message) error {
	if v := customViolation(ctx, name, resourceType, method, m); v != nil {
		return status.Error(codes.FailedPrecondition, v.Error())
	}
	return nil
}
