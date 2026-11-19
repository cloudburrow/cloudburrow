package pubsubfront

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

type schemaCustoms struct{ list []orgpolicy.CustomConstraint }

func (schemaCustoms) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (s schemaCustoms) Customs(_ context.Context, _, resourceType, method string) ([]orgpolicy.CustomConstraint, error) {
	if resourceType != orgpolicy.TypeSchema || method != orgpolicy.MethodUpdate {
		return nil, nil
	}
	return s.list, nil
}

// TestRollbackSchemaCustom (#1902): RollbackSchema "creates a new schema
// revision that is a copy of the provided revision_id", so it is an
// UPDATE of pubsub.googleapis.com/Schema read on that revision's
// definition: rolling back to a revision a DENY matches is refused
// FAILED_PRECONDITION, to one it does not match allowed, and to a
// revision that does not exist left to the call (NOT_FOUND).
func TestRollbackSchemaCustom(t *testing.T) {
	orgpolicy.Use(schemaCustoms{list: []orgpolicy.CustomConstraint{{Name: "custom.noLegacy",
		Condition: "resource.definition.contains('legacy')", ActionType: "DENY", Description: "No legacy fields.",
		Effective: orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
			{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})}}})
	defer orgpolicy.Use(nil)
	name := "projects/p/schemas/s"
	f := &Front{protoSchemas: map[string]*protoSchema{name: {revisions: []*pubsubpb.Schema{
		{Name: name, Type: pubsubpb.Schema_PROTOCOL_BUFFER, RevisionId: "r1", Definition: `syntax = "proto3"; message M { int32 legacy = 1; }`},
		{Name: name, Type: pubsubpb.Schema_PROTOCOL_BUFFER, RevisionId: "r2", Definition: `syntax = "proto3"; message M { int32 n = 1; }`},
	}}}}
	ctx := context.Background()
	err := f.customRollbackSchema(ctx, name, "r1")
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || !strings.Contains(st.Message(), "custom.noLegacy") {
		t.Errorf("a rollback to the legacy revision = %v, want FAILED_PRECONDITION naming custom.noLegacy", err)
	}
	if err := f.customRollbackSchema(ctx, name, "r2"); err != nil {
		t.Errorf("a rollback to an allowed revision = %v", err)
	}
	if err := f.customRollbackSchema(ctx, name, "r9"); err != nil {
		t.Errorf("a rollback to a missing revision = %v, want it left to RollbackSchema", err)
	}
}
