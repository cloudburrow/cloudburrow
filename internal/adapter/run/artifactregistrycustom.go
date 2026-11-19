package run

import (
	"context"
	"encoding/json"
	"errors"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints on artifactregistry.googleapis.com/Repository
// (#1895), CREATE and UPDATE, over gRPC and REST (which calls the same
// methods): the condition reads the repository as the request would
// leave it, by its REST field and enum names, on "all fields except
// labels" (resource.format == 'DOCKER',
// resource.platformLogsConfig.loggingState != 'DISABLED', the page's
// examples; "Use custom organization policies", Artifact Registry, read
// 2026-11-19). Unset scalars and enums read as their defaults, as a
// condition over a proto3 resource reads them. A violation is
// FAILED_PRECONDITION "Operation denied by custom org policies: [...]",
// the page's example output.
func checkRepositoryCustom(ctx context.Context, r *arpb.Repository, method string) error {
	project := orgpolicy.ProjectOf(r.GetName())
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	raw, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(r)
	if err != nil {
		return nil
	}
	var res map[string]any
	_ = json.Unmarshal(raw, &res)
	dropJSONNulls(res)
	delete(res, "labels")
	err = orgpolicy.CheckCustom(ctx, project, orgpolicy.TypeARRepository, method, res)
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return apierror.FailedPrecondition("%s", v.Error())
	}
	return nil
}

// dropJSONNulls removes the null members (unset messages) of v.
func dropJSONNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
				continue
			}
			dropJSONNulls(x)
		}
	case []any:
		for _, x := range t {
			dropJSONNulls(x)
		}
	}
}
