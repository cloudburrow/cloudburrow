package resourcemanager

import (
	"context"
	"testing"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	exprpb "google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestLintPolicyServer (#1751): results carry Google's level, unit name
// and field, and a request without a condition is INVALID_ARGUMENT.
func TestLintPolicyServer(t *testing.T) {
	_, _, c := startIAMAdmin(t)
	ctx := context.Background()
	r, err := c.LintPolicy(ctx, &adminpb.LintPolicyRequest{LintObject: &adminpb.LintPolicyRequest_Condition{Condition: &exprpb.Expr{Expression: "true=false"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetLintResults()) != 11 {
		t.Fatalf("%d results, want 11", len(r.GetLintResults()))
	}
	g := r.GetLintResults()[0]
	if g.GetLevel() != adminpb.LintResult_CONDITION || g.GetValidationUnitName() != "LintValidationUnits/ConditionCompileCheck" ||
		g.GetSeverity() != adminpb.LintResult_ERROR || g.GetFieldName() != "condition.expression" || g.GetLocationOffset() != 4 {
		t.Errorf("compile result %v", g)
	}
	if n := r.GetLintResults()[1]; n.GetSeverity() != adminpb.LintResult_NOTICE || n.GetValidationUnitName() != "LintValidationUnits/ConditionComplexityCheck" {
		t.Errorf("skipped result %v", n)
	}
	if r, err := c.LintPolicy(ctx, &adminpb.LintPolicyRequest{LintObject: &adminpb.LintPolicyRequest_Condition{Condition: &exprpb.Expr{Expression: "1 < 2"}}}); err != nil || len(r.GetLintResults()) != 0 {
		t.Errorf("1 < 2 = %v, %v", r, err)
	}
	if _, err := c.LintPolicy(ctx, &adminpb.LintPolicyRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no condition = %v, want INVALID_ARGUMENT", err)
	}
}

// TestLegacySignAuthorizer (#1751): SignBlob asks the authorizer with the
// call's bearer, the account's email and signBlob's permission, before it
// finds the account missing or disabled.
func TestLegacySignAuthorizer(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "sign-project"}); err != nil {
		t.Fatal(err)
	}
	sa, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sign-project", AccountId: "signer"})
	if err != nil {
		t.Fatal(err)
	}
	type call struct{ bearer, account, perm string }
	var got []call
	reg.SetSignAuthorizer(func(bearer, account, perm string) error {
		got = append(got, call{bearer, account, perm})
		if bearer != "good" {
			return status.Error(codes.Unauthenticated, "no")
		}
		if account == "" {
			return status.Error(codes.PermissionDenied, "no")
		}
		return nil
	})
	as := func(tok string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}
	if _, err := c.SignBlob(as("bad"), &adminpb.SignBlobRequest{Name: sa.GetName(), BytesToSign: []byte("x")}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("bad bearer = %v", err)
	}
	if _, err := c.SignBlob(as("good"), &adminpb.SignBlobRequest{Name: sa.GetName(), BytesToSign: []byte("x")}); err != nil {
		t.Errorf("good bearer: %v", err)
	}
	if _, err := c.SignJwt(as("good"), &adminpb.SignJwtRequest{Name: "projects/sign-project/serviceAccounts/nobody@sign-project.iam.gserviceaccount.com", Payload: "{}"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("unknown account = %v, want PERMISSION_DENIED", err)
	}
	want := []call{{"bad", sa.GetEmail(), "iam.serviceAccounts.signBlob"}, {"good", sa.GetEmail(), "iam.serviceAccounts.signBlob"}, {"good", "", "iam.serviceAccounts.signJwt"}}
	if len(got) != len(want) {
		t.Fatalf("authorizer calls %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}
