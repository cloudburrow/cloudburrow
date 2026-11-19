package firestoreadmin

import (
	"context"
	"encoding/json"
	"errors"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// typeDatabase is Firestore's custom constraint resource type ("Manage
// Firestore resources using custom constraints", read 2026-11-19).
const typeDatabase = "firestore.googleapis.com/Database"

// checkDatabaseCustom checks db, as a create or update would leave it,
// against the custom constraints on firestore.googleapis.com/Database
// enforced on its project (#1891): resource.deleteProtectionState,
// .concurrencyMode, .appEngineIntegrationMode, .locationId, .type,
// .pointInTimeRecoveryEnablement, .name, by their REST names and enum
// values. A violation is FAILED_PRECONDITION "Operation denied by custom
// org policies: [...]".
func checkDatabaseCustom(ctx context.Context, db *adminpb.Database, method string) error {
	project := orgpolicy.ProjectOf(db.GetName())
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	raw, err := protojson.Marshal(db)
	if err != nil {
		return nil
	}
	var res map[string]any
	_ = json.Unmarshal(raw, &res)
	err = orgpolicy.CheckCustom(ctx, project, typeDatabase, method, res)
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return status.Error(codes.FailedPrecondition, v.Error())
	}
	return nil
}
