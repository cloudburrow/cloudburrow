package resourcemanager

import (
	"context"
	"time"

	adminpb "cloud.google.com/go/iam/admin/apiv1/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// LintPolicy is the IAM API's iamPolicies.lintPolicy (#1751): it lints the
// request's condition with Google's eleven validation units
// (internal/iamcond's Lint), each result at level CONDITION on field
// condition.expression, named LintValidationUnits/UNIT as Google's
// linting page shows them, sorted by severity. As Google's reference says,
// full_resource_name is not read: the linter lints what the request
// carries. A request without a condition is INVALID_ARGUMENT (the
// lint_object is required).
func (s *IAMAdminServer) LintPolicy(_ context.Context, req *adminpb.LintPolicyRequest) (*adminpb.LintPolicyResponse, error) {
	return LintPolicy(req, time.Now())
}

// LintPolicy lints req at time now.
func LintPolicy(req *adminpb.LintPolicyRequest, now time.Time) (*adminpb.LintPolicyResponse, error) {
	cond := req.GetCondition()
	if cond == nil {
		return nil, status.Error(codes.InvalidArgument, "condition is required: it is the only lint object.")
	}
	out := &adminpb.LintPolicyResponse{}
	for _, r := range iamcond.Lint(cond.GetExpression(), now) {
		lr := &adminpb.LintResult{
			Level:              adminpb.LintResult_CONDITION,
			ValidationUnitName: "LintValidationUnits/" + r.Unit,
			Severity:           adminpb.LintResult_Severity(adminpb.LintResult_Severity_value[r.Severity]),
			FieldName:          "condition.expression",
			DebugMessage:       r.Message,
		}
		if r.Offset >= 0 {
			lr.LocationOffset = int32(r.Offset)
		}
		out.LintResults = append(out.LintResults, lr)
	}
	return out, nil
}
